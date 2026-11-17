// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package apiservice implements the EnvironmentService, ProcessService, and
// FileSystemService gRPC APIs for the ate-env API server, proxying in-actor
// execution and filesystem operations directly to the guest daemon via atenet.
package apiservice

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/envstore"
	"github.com/agent-substrate/env/internal/routertls"
	"github.com/agent-substrate/env/internal/telemetry"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"github.com/google/uuid"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// DefaultTemplate is the ActorTemplate name used when a create request
// does not specify one.
const DefaultTemplate = "default-template"

type targetActorCtxKey struct{}

// DefaultNamespace is the default Kubernetes namespace used when deploying
// ate-env manifests.
const DefaultNamespace = "ate-env"

// DefaultAtespace is the Substrate atespace used when a request does not
// specify one; the same one ate-env-api creates at startup.
const DefaultAtespace = ate.DefaultAtespace

// Server implements ateenvv1alpha.EnvironmentServiceServer, ateenvv1alpha.ProcessServiceServer,
// and ateenvv1alpha.FileSystemServiceServer.
type Server struct {
	ateenvv1alpha.UnimplementedEnvironmentServiceServer
	ateenvv1alpha.UnimplementedProcessServiceServer
	ateenvv1alpha.UnimplementedFileSystemServiceServer

	client     *ate.Client
	routerAddr string
	hostSuffix string

	// Store keeps the labels and the deadline of an environment
	// (internal/envstore). Nil, the default, means the store is off: the
	// server then makes no call to Kubernetes.
	Store *envstore.Store

	// WakeBudget bounds how long a guest call waits for a suspended
	// environment to wake; see ensureAwake. Zero means DefaultWakeBudget.
	WakeBudget time.Duration
	// WakeRunningBudget bounds the wait once the control plane reports the
	// environment RUNNING; see ensureAwake. Zero means DefaultWakeRunningBudget.
	WakeRunningBudget time.Duration
	// WakeNoCapacityGrace, when positive, fails a guest call with
	// ResourceExhausted once the router has said for this long that no
	// worker has room for the environment; see ensureAwake. Zero (the
	// default) keeps waiting for the whole WakeBudget.
	WakeNoCapacityGrace time.Duration
	// WarmMaxProbesPerNode caps the WarmImage probes in flight per node of
	// one worker selector, across calls: a round takes nodes x
	// probes_per_node slots on its selector before it creates its probes
	// and gives them back once they are deleted, so a pre-flight that fans
	// out one call per image puts at most this many first boots (image
	// unpacks) on a node at once. A request's probes_per_node above it is
	// refused. Zero means DefaultWarmMaxProbesPerNode.
	WarmMaxProbesPerNode int
	// WakeStateReadCap bounds one control-plane state read of a wake; see
	// ensureAwake. Zero or less means DefaultWakeStateReadCap; the
	// -wake-state-read-cap flag sets it.
	WakeStateReadCap time.Duration
	// Info is the build and configuration GetServerInfo reports.
	Info ServerInfo
	// RouterTLS, when set, is the client TLS configuration for the router's
	// listeners (see internal/routertls); nil dials them in plaintext.
	RouterTLS *tls.Config
	// RouterIdentity is the SPIFFE ID of the certificate RouterTLS presents,
	// named in the error when the router rejects it.
	RouterIdentity string

	// RouterPoolSize, when positive, reuses a striped pool of persistent
	// round_robin gRPC connections to the atenet router instead of opening and
	// closing a connection on every guest RPC (0 opens one connection per call).
	RouterPoolSize int
	// ReactiveWake, when true, skips the pre-flight ensureAwake probe for
	// environments cached as awake since creation or suspend. On a fast-path
	// Unavailable, the cache entry is evicted: idempotent calls and
	// StartProcess to a guest that advertised start-request-id wake and retry,
	// whereas SignalProcess and StartProcess to an older guest return
	// Unavailable without replaying.
	ReactiveWake bool

	// Metrics, when set, receives the create_to_ready and wake_wait
	// observations and tracks the atespaces ate_env_api_environments
	// reports (see EnvironmentCounts). Nil records nothing.
	Metrics *telemetry.Metrics

	routerPoolMu  sync.Mutex
	routerClosed  bool
	routerConns   []*grpc.ClientConn
	routerConnSeq atomic.Uint64
	awakeEnvs     *boundedAwakeCache

	readyMu   sync.Mutex
	pending   map[string]pendingCreate // atespace/id -> creation start, until the guest first answers
	atespaces map[string]bool          // atespaces environments were created in, for EnvironmentCounts

	// imageTemplates serializes the recreate of a failed derived template
	// with the creates that use it.
	imageTemplates imageTemplateLocks
	// warmSlots are the WarmImage probe slots per worker selector; see
	// WarmMaxProbesPerNode.
	warmSlots warmSlotRegistry
}

// New creates a new Server.
func New(client *ate.Client, routerAddr, hostSuffix string) *Server {
	if hostSuffix == "" {
		hostSuffix = ate.DefaultHostSuffix
	}
	routerAddr = strings.TrimPrefix(routerAddr, "http://")
	routerAddr = strings.TrimPrefix(routerAddr, "https://")
	return &Server{
		client:            client,
		routerAddr:        routerAddr,
		hostSuffix:        hostSuffix,
		WakeBudget:        DefaultWakeBudget,
		WakeRunningBudget: DefaultWakeRunningBudget,
		awakeEnvs:         newBoundedAwakeCache(defaultAwakeCapacity, defaultAwakeTTL),
	}
}

// Close releases any pooled router connections held by the server and rejects
// subsequent guest sessions with Unavailable.
func (s *Server) Close() {
	s.routerPoolMu.Lock()
	s.routerClosed = true
	conns := s.routerConns
	s.routerConns = nil
	s.routerPoolMu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (s *Server) isRouterClosed() bool {
	s.routerPoolMu.Lock()
	closed := s.routerClosed
	s.routerPoolMu.Unlock()
	return closed
}

// ============================================================================
// --- ENVIRONMENT SERVICE ---
// ============================================================================

// CreateEnvironment registers and starts a new environment.
//
// Defaults: id is required. The template name defaults to DefaultTemplate.
// The atespace is the request's atespace, else template.atespace, else
// DefaultAtespace, and that one atespace is used both to resolve the template
// and to create the actor; a template.atespace that differs from a non-empty
// request atespace is ignored.
//
// When rootfs_image is set, the template acts as the base and the environment is
// created from a template derived from it, named
// "<base>-<first 12 hex digits of the image digest>" in the same atespace.
// The derived template keeps the base's settings, runs the rootfs image as
// the container image and mounts the base's guest as a read-only image volume,
// so the image runs unmodified (see ensureImageTemplate and
// DeriveImageTemplate). It is created on first use and reused by later
// creates on the same image (created again when its golden snapshot build
// failed long enough ago and no actor uses it; see
// recreateFailedImageTemplate), and the response reports it in
// Environment.template. When rootfs_image is empty the template is used as is.
//
// With time_to_live, the environment gets a deadline: the create time of its
// actor plus the value. The actor is created first, then its object in the
// store, with the uid of the actor and the deadline (decision EM-E). When
// that write fails, the actor is deleted, with its uid as the precondition,
// and the create fails. A create with no time_to_live writes no object and
// makes no call to Kubernetes.
//
// Errors: InvalidArgument for a missing id, a negative time_to_live, a rootfs
// image not pinned by digest, a
// derived name that is too long or not a valid resource name, or a base that
// cannot be derived from (no containers, unpinned guest image, or a command
// that cannot be re-rooted under the guest volume). FailedPrecondition when
// the base template does not exist in the atespace, when the derived name
// is already held by a template running a different image, or for a
// time_to_live on a server that runs without the store (nothing is created).
// Unavailable when the deadline was not written and the environment was
// deleted again. DeadlineExceeded
// or Canceled when the request ends while a recreate of its derived template
// runs. Other control plane failures are mapped by toGRPCError.
func (s *Server) CreateEnvironment(ctx context.Context, req *ateenvv1alpha.CreateEnvironmentRequest) (resp *ateenvv1alpha.CreateEnvironmentResponse, retErr error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	ttl, err := timeToLive(req.GetTimeToLive())
	if err != nil {
		return nil, err
	}
	if ttl > 0 && s.Store == nil {
		// Never accept and drop: the caller would wait for a delete that
		// does not come.
		return nil, errStoreOff("time_to_live")
	}
	var start time.Time
	if s.Metrics != nil {
		start = requestedAt(ctx, time.Now())
	}
	templateName, atespace := templateDefaults(req.GetTemplate(), req.GetAtespace())
	setSpanEnvAttributes(ctx, atespace, req.GetId())
	setSpanTemplateAttributes(ctx, atespace, templateName)
	if s.Metrics != nil {
		defer func() {
			if retErr != nil {
				s.Metrics.ObserveCreateToReady(atespace, telemetry.ReadyStatusError, time.Since(start))
				return
			}
			s.trackCreate(atespace, req.GetId(), start)
		}()
	}

	if image := req.GetRootfsImage(); image != "" {
		name, _, release, err := s.ensureImageTemplate(ctx, atespace, templateName, image)
		if err != nil {
			return nil, err
		}
		defer release()
		templateName = name
	}

	opts := ate.CreateOptions{
		ID:             req.GetId(),
		Template:       templateName,
		Atespace:       atespace,
		WorkerSelector: req.GetWorkerSelector(),
	}
	actor, err := s.client.Create(ctx, opts)
	if err != nil {
		return nil, toGRPCError(err)
	}
	setSpanActorAttributes(ctx, actor)
	if s.ReactiveWake && s.awakeEnvs != nil {
		s.awakeEnvs.Remove(atespace + "/" + req.GetId())
	}

	env := &ateenvv1alpha.Environment{
		Id:       req.GetId(),
		Atespace: atespace,
		Template: &ateenvv1alpha.Template{
			Name:     templateName,
			Atespace: atespace,
		},
		Status:         ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_UNSPECIFIED,
		WorkerSelector: req.GetWorkerSelector(),
		CreateTime:     actor.GetMetadata().GetCreateTime(),
	}
	if ttl > 0 {
		expire, err := s.writeCreateDeadline(ctx, atespace, actor, ttl)
		if err != nil {
			return nil, err
		}
		// What this call wrote: the informer can be behind it (EM-D).
		env.ExpireTime = timestamppb.New(expire)
	}
	return &ateenvv1alpha.CreateEnvironmentResponse{Environment: env}, nil
}

// templateDefaults resolves a request's base template name and atespace the
// way CreateEnvironment does: the template name defaults to DefaultTemplate,
// the atespace to the template's, then to DefaultAtespace.
func templateDefaults(tmpl *ateenvv1alpha.Template, atespace string) (name, space string) {
	name = tmpl.GetName()
	if name == "" {
		name = DefaultTemplate
	}
	space = atespace
	if space == "" {
		space = tmpl.GetAtespace()
	}
	if space == "" {
		space = DefaultAtespace
	}
	return name, space
}

// GetImageStatus reports the template derived for a rootfs image and the
// state of its golden snapshot, without creating anything.
func (s *Server) GetImageStatus(ctx context.Context, req *ateenvv1alpha.GetImageStatusRequest) (*ateenvv1alpha.ImageStatus, error) {
	base, atespace := templateDefaults(req.GetTemplate(), req.GetAtespace())
	setSpanTemplateAttributes(ctx, atespace, base)
	name, err := ImageTemplateName(base, req.GetRootfsImage())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	setSpanTemplateAttributes(ctx, atespace, name)
	tmpl, err := s.client.GetActorTemplate(ctx, atespace, name)
	if err != nil && !errors.Is(err, ate.ErrNotFound) {
		return nil, toGRPCError(err)
	}
	// The same conflict a create reports: a template under the derived name
	// that runs another image is not this image's template.
	if got := firstContainerImage(tmpl); tmpl != nil && !sameImageDigest(got, req.GetRootfsImage()) {
		return nil, status.Errorf(codes.FailedPrecondition,
			"actor template %q exists but runs %q, not %q; delete it or use another base template", name, got, req.GetRootfsImage())
	}
	return imageStatus(req.GetRootfsImage(), atespace, name, tmpl), nil
}

// EnsureImageTemplate creates the template derived for a rootfs image when
// it is missing, which starts its golden snapshot bake, and reports it. It
// takes the same path a create does, so a template whose bake failed is
// recreated under the same rules, but no actor is created.
func (s *Server) EnsureImageTemplate(ctx context.Context, req *ateenvv1alpha.EnsureImageTemplateRequest) (*ateenvv1alpha.ImageStatus, error) {
	base, atespace := templateDefaults(req.GetTemplate(), req.GetAtespace())
	setSpanTemplateAttributes(ctx, atespace, base)
	name, tmpl, release, err := s.ensureImageTemplate(ctx, atespace, base, req.GetRootfsImage())
	if err != nil {
		return nil, err
	}
	release()
	return imageStatus(req.GetRootfsImage(), atespace, name, tmpl), nil
}

// imageStatus is the ImageStatus for the derived template tmpl, nil when it
// does not exist. The golden state follows Substrate's
// golden_snapshot_status: a tag means READY, an error message FAILED, a
// template with neither is still baking, and a template whose golden
// snapshots are disabled reports NONE, because none will come.
func imageStatus(image, atespace, name string, tmpl *ateapipb.ActorTemplate) *ateenvv1alpha.ImageStatus {
	out := &ateenvv1alpha.ImageStatus{
		RootfsImage: image,
		Template:    &ateenvv1alpha.Template{Name: name, Atespace: atespace},
		Golden:      ateenvv1alpha.GoldenSnapshotState_GOLDEN_SNAPSHOT_STATE_NONE,
	}
	if tmpl == nil {
		return out
	}
	out.TemplateExists = true
	if tmpl.GetSnapshotConfig().GetGoldenSnapshotConfig().GetMode() == ateapipb.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED {
		return out
	}
	golden := tmpl.GetStatus().GetGoldenSnapshotStatus()
	switch {
	case golden.GetErrorMessage() != "":
		out.Golden = ateenvv1alpha.GoldenSnapshotState_GOLDEN_SNAPSHOT_STATE_FAILED
		out.GoldenError = golden.GetErrorMessage()
	case golden.GetGoldenTag().GetName() != "":
		out.Golden = ateenvv1alpha.GoldenSnapshotState_GOLDEN_SNAPSHOT_STATE_READY
		out.GoldenTag = golden.GetGoldenTag().GetName()
	default:
		out.Golden = ateenvv1alpha.GoldenSnapshotState_GOLDEN_SNAPSHOT_STATE_PENDING
	}
	return out
}

// ensureImageTemplate returns the name of the ActorTemplate that runs image
// on top of base, creating it from base on first use. The derived template
// lives in the environment's atespace, next to base. The caller must call
// release once its actor is created: until then, this process does not
// delete the template (see recreateFailedImageTemplate).
func (s *Server) ensureImageTemplate(ctx context.Context, atespace, base, image string) (name string, tmpl *ateapipb.ActorTemplate, release func(), err error) {
	name, err = ImageTemplateName(base, image)
	if err != nil {
		return "", nil, nil, status.Error(codes.InvalidArgument, err.Error())
	}
	// The derived template is the RPC's template from here on, also when a
	// step below fails on it (FS-16): CreateEnvironment, EnsureImageTemplate
	// and WarmImage stamp the base before the call, and GetImageStatus stamps
	// the derived name at the same point.
	setSpanTemplateAttributes(ctx, atespace, name)
	lock := s.imageTemplates.get(atespace, name)
	if err := lock.rlock(ctx); err != nil {
		return "", nil, nil, err
	}
	tmpl, err = s.getOrCreateImageTemplate(ctx, atespace, base, name, image)
	if err != nil {
		lock.mu.RUnlock()
		return "", nil, nil, err
	}
	if !lock.recreateDue(tmpl, time.Now()) {
		return name, tmpl, lock.mu.RUnlock, nil
	}
	lock.mu.RUnlock()

	lock.mu.Lock()
	err = s.recreateFailedImageTemplate(ctx, lock, atespace, base, name, image)
	lock.mu.Unlock()
	if err != nil {
		return "", nil, nil, err
	}
	if err := lock.rlock(ctx); err != nil {
		return "", nil, nil, err
	}
	if tmpl, err = s.getOrCreateImageTemplate(ctx, atespace, base, name, image); err != nil {
		lock.mu.RUnlock()
		return "", nil, nil, err
	}
	return name, tmpl, lock.mu.RUnlock, nil
}

// getOrCreateImageTemplate returns the derived template name, creating it
// from base when it does not exist. A template that runs the same digest
// under another repository path is reused: the digest pins the bytes, and
// datasets list one image under many paths.
func (s *Server) getOrCreateImageTemplate(ctx context.Context, atespace, base, name, image string) (*ateapipb.ActorTemplate, error) {
	existing, err := s.client.GetActorTemplate(ctx, atespace, name)
	switch {
	case err == nil:
		if got := firstContainerImage(existing); !sameImageDigest(got, image) {
			return nil, status.Errorf(codes.FailedPrecondition,
				"actor template %q exists but runs %q, not %q; delete it or use another base template", name, got, image)
		}
		return existing, nil
	case !errors.Is(err, ate.ErrNotFound):
		return nil, toGRPCError(err)
	}
	baseTmpl, err := s.client.GetActorTemplate(ctx, atespace, base)
	if err != nil {
		if errors.Is(err, ate.ErrNotFound) {
			return nil, status.Errorf(codes.FailedPrecondition,
				"base actor template %q not found in atespace %q; register it first (ate-env manifest template)", base, atespace)
		}
		return nil, toGRPCError(err)
	}
	derived, err := DeriveImageTemplate(baseTmpl, name, image)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	created, err := s.client.CreateActorTemplate(ctx, derived)
	if err == nil {
		return created, nil
	}
	if !errors.Is(err, ate.ErrAlreadyExists) {
		return nil, toGRPCError(err)
	}
	// A concurrent create won the race. It may have built the template for
	// another digest that shares the first 12 hex digits (the derived name
	// keeps only those), so check the image as for a template that already
	// existed.
	if existing, err = s.client.GetActorTemplate(ctx, atespace, name); err != nil {
		return nil, toGRPCError(err)
	}
	if got := firstContainerImage(existing); !sameImageDigest(got, image) {
		return nil, status.Errorf(codes.FailedPrecondition,
			"actor template %q exists but runs %q, not %q; delete it or use another base template", name, got, image)
	}
	return existing, nil
}

func firstContainerImage(t *ateapipb.ActorTemplate) string {
	if len(t.GetContainers()) == 0 {
		return ""
	}
	return t.GetContainers()[0].GetImage()
}

// sameImageDigest reports whether two digest-pinned references name the same
// image, whatever their repository paths. An unpinned reference matches
// nothing.
func sameImageDigest(a, b string) bool {
	da, err := ImageDigest(a)
	if err != nil {
		return false
	}
	db, err := ImageDigest(b)
	if err != nil {
		return false
	}
	return da == db
}

// GetEnvironment retrieves the status and configuration of an existing
// environment. Its expire_time comes from the informer of the store: the
// read makes no call to Kubernetes (decision EM-D).
func (s *Server) GetEnvironment(ctx context.Context, req *ateenvv1alpha.GetEnvironmentRequest) (*ateenvv1alpha.GetEnvironmentResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	atespace := req.GetAtespace()
	if atespace == "" {
		atespace = DefaultAtespace
	}

	setSpanEnvAttributes(ctx, atespace, req.GetId())

	actor, err := s.client.Get(ctx, atespace, req.GetId())
	if err != nil {
		return nil, toGRPCError(err)
	}
	setSpanActorAttributes(ctx, actor)

	return &ateenvv1alpha.GetEnvironmentResponse{
		Environment: s.withDeadline(ActorToEnvironment(actor), actor.GetMetadata().GetUid()),
	}, nil
}

// Page sizes of ListEnvironments: the actors one call reads from Substrate.
const (
	// DefaultListPageSize is what a request with page_size 0 reads.
	DefaultListPageSize = 100
	// MaxListPageSize is Substrate's largest page. A larger page_size reads
	// this many.
	MaxListPageSize = 1000
)

// ListEnvironments returns one page of the environments of one atespace: one
// page of Substrate's actors, each converted as GetEnvironment does, with
// Substrate's token for the next page.
//
// The statuses filter applies to the page after the read. A page can thus
// hold fewer environments than page_size, or none, while next_page_token is
// set.
//
// Errors: InvalidArgument for a negative page_size, a status outside
// EnvironmentStatus, or a page_token Substrate refuses. Other control plane
// failures are mapped by toGRPCError.
func (s *Server) ListEnvironments(ctx context.Context, req *ateenvv1alpha.ListEnvironmentsRequest) (*ateenvv1alpha.ListEnvironmentsResponse, error) {
	size := req.GetPageSize()
	switch {
	case size < 0:
		return nil, status.Errorf(codes.InvalidArgument, "page_size %d is negative", size)
	case size == 0:
		size = DefaultListPageSize
	case size > MaxListPageSize:
		size = MaxListPageSize
	}
	keep := make(map[ateenvv1alpha.EnvironmentStatus]bool, len(req.GetStatuses()))
	for _, st := range req.GetStatuses() {
		if _, ok := ateenvv1alpha.EnvironmentStatus_name[int32(st)]; !ok {
			return nil, status.Errorf(codes.InvalidArgument, "status %d is not an EnvironmentStatus", st)
		}
		keep[st] = true
	}
	atespace := req.GetAtespace()
	if atespace == "" {
		atespace = DefaultAtespace
	}

	setSpanAtespaceAttribute(ctx, atespace)
	actors, next, err := s.client.ListPage(ctx, atespace, size, req.GetPageToken())
	if err != nil {
		return nil, toGRPCError(err)
	}
	resp := &ateenvv1alpha.ListEnvironmentsResponse{NextPageToken: next}
	for _, actor := range actors {
		env := s.withDeadline(ActorToEnvironment(actor), actor.GetMetadata().GetUid())
		if len(keep) > 0 && !keep[env.GetStatus()] {
			continue
		}
		resp.Environments = append(resp.Environments, env)
	}
	return resp, nil
}

// SuspendEnvironment checkpoints and stops an active environment.
func (s *Server) SuspendEnvironment(ctx context.Context, req *ateenvv1alpha.SuspendEnvironmentRequest) (*ateenvv1alpha.SuspendEnvironmentResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	atespace := req.GetAtespace()
	if atespace == "" {
		atespace = DefaultAtespace
	}

	setSpanEnvAttributes(ctx, atespace, req.GetId())

	var err error
	switch req.GetMode() {
	case ateenvv1alpha.SuspendMode_SUSPEND_MODE_UNSPECIFIED, ateenvv1alpha.SuspendMode_SUSPEND_MODE_DURABLE:
		err = s.client.Suspend(ctx, atespace, req.GetId())
	case ateenvv1alpha.SuspendMode_SUSPEND_MODE_LOCAL:
		err = s.client.SuspendLocal(ctx, atespace, req.GetId())
	default:
		return nil, status.Errorf(codes.InvalidArgument, "mode %d is not a SuspendMode", req.GetMode())
	}
	if err != nil {
		return nil, toGRPCError(err)
	}
	if s.ReactiveWake && s.awakeEnvs != nil {
		s.awakeEnvs.Remove(atespace + "/" + req.GetId())
	}

	return &ateenvv1alpha.SuspendEnvironmentResponse{}, nil
}

// DeleteEnvironment permanently removes an environment and its resources.
func (s *Server) DeleteEnvironment(ctx context.Context, req *ateenvv1alpha.DeleteEnvironmentRequest) (*ateenvv1alpha.DeleteEnvironmentResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	atespace := req.GetAtespace()
	if atespace == "" {
		atespace = DefaultAtespace
	}

	setSpanEnvAttributes(ctx, atespace, req.GetId())

	// The object of the environment, as the store's informer has it before
	// the delete. An environment with no label and no deadline has none, and
	// its delete then makes no call to Kubernetes.
	var record envstore.Record
	var hasRecord bool
	if s.Store != nil {
		var err error
		record, hasRecord, err = s.Store.Get(atespace, req.GetId())
		if err != nil {
			// An object that holds no readable record is not an environment
			// with no object. The delete goes on, because the actor is the
			// truth, and the object stays for the sweep, which deletes and
			// reports it after the grace period.
			log.Printf("apiservice: DeleteEnvironment %s/%s: %v; the orphan sweep removes the object", atespace, req.GetId(), err)
		}
	}

	// A caller's delete names the environment by its id and sends no uid
	// precondition; the deadline sweep, on the same path, sends one.
	if err := s.deleteEnvironment(ctx, atespace, req.GetId(), "", record, hasRecord); err != nil {
		return nil, toGRPCError(err)
	}

	return &ateenvv1alpha.DeleteEnvironmentResponse{}, nil
}

// ============================================================================
// --- PROCESS SERVICE ---
// ============================================================================

// evictOnGuestUnavailable reports whether err is a router or guest Unavailable
// not caused by caller context cancellation or deadline expiry. Under
// ReactiveWake it also removes targetActor from awakeEnvs so neither a cold
// call nor a post-wake retry that hits Unavailable leaves the environment
// cached as awake.
func (s *Server) evictOnGuestUnavailable(ctx context.Context, targetActor string, err error) bool {
	if err == nil || status.Code(err) != codes.Unavailable || ctx.Err() != nil {
		return false
	}
	if s.ReactiveWake && s.awakeEnvs != nil {
		s.awakeEnvs.Remove(targetActor)
	}
	return true
}

func (s *Server) callProcessUnary(ctx context.Context, isIdempotent bool, fn func(ateenvv1alpha.ProcessServiceClient, context.Context, ...grpc.CallOption) (*ateenvv1alpha.Process, error)) (*ateenvv1alpha.Process, error) {
	envID, atespace, err := envFromContext(ctx)
	if err != nil {
		return nil, err
	}
	targetActor := atespace + "/" + envID
	conn, outCtx, closeFn, fastPath, err := s.guestSession(ctx, atespace, envID, false)
	if err != nil {
		return nil, err
	}
	resp, err := fn(ateenvv1alpha.NewProcessServiceClient(conn), outCtx)
	closeFn()
	if s.evictOnGuestUnavailable(ctx, targetActor, err) && fastPath && isIdempotent {
		conn2, outCtx2, closeFn2, _, werr := s.guestSession(ctx, atespace, envID, true)
		if werr != nil {
			return nil, werr
		}
		defer closeFn2()
		resp, err = fn(ateenvv1alpha.NewProcessServiceClient(conn2), outCtx2)
		s.evictOnGuestUnavailable(ctx, targetActor, err)
		return resp, err
	}
	return resp, err
}

// StartProcess launches a process inside the target environment container.
func (s *Server) StartProcess(ctx context.Context, req *ateenvv1alpha.StartProcessRequest) (*ateenvv1alpha.Process, error) {
	envID, atespace, err := envFromContext(ctx)
	if err != nil {
		return nil, err
	}
	targetActor := atespace + "/" + envID

	conn, outCtx, closeFn, fastPath, err := s.guestSession(ctx, atespace, envID, false)
	if err != nil {
		return nil, err
	}
	if !s.ReactiveWake {
		defer closeFn()
		return ateenvv1alpha.NewProcessServiceClient(conn).StartProcess(outCtx, req)
	}

	reqSend := proto.Clone(req).(*ateenvv1alpha.StartProcessRequest)
	isIdempotent := false
	if s.hasGuestCap(targetActor, guestCapStartRequestID) {
		if reqSend.GetRequestId() == "" {
			reqSend.RequestId = uuid.NewString()
		}
		isIdempotent = true
	} else {
		// Without observed guest support, clear any client-supplied request_id
		// and never treat the call as idempotent, preventing double execution on
		// older guests.
		reqSend.RequestId = ""
	}

	resp, err := ateenvv1alpha.NewProcessServiceClient(conn).StartProcess(outCtx, reqSend)
	closeFn()

	if s.evictOnGuestUnavailable(ctx, targetActor, err) && fastPath && isIdempotent {
		conn2, outCtx2, closeFn2, _, werr := s.guestSession(ctx, atespace, envID, true)
		if werr != nil {
			return nil, werr
		}
		defer closeFn2()
		resp, err = ateenvv1alpha.NewProcessServiceClient(conn2).StartProcess(outCtx2, reqSend)
		s.evictOnGuestUnavailable(ctx, targetActor, err)
		return resp, err
	}
	return resp, err
}

// GetProcess retrieves the state of a process inside the environment.
func (s *Server) GetProcess(ctx context.Context, req *ateenvv1alpha.GetProcessRequest) (*ateenvv1alpha.Process, error) {
	return s.callProcessUnary(ctx, true, func(c ateenvv1alpha.ProcessServiceClient, outCtx context.Context, opts ...grpc.CallOption) (*ateenvv1alpha.Process, error) {
		return c.GetProcess(outCtx, req, opts...)
	})
}

// SignalProcess delivers a signal to a process inside the environment.
func (s *Server) SignalProcess(ctx context.Context, req *ateenvv1alpha.SignalProcessRequest) (*ateenvv1alpha.Process, error) {
	// SignalProcess is not idempotent and carries no request_id, so a fast-path
	// Unavailable evicts the awake cache without replaying.
	return s.callProcessUnary(ctx, false, func(c ateenvv1alpha.ProcessServiceClient, outCtx context.Context, opts ...grpc.CallOption) (*ateenvv1alpha.Process, error) {
		return c.SignalProcess(outCtx, req, opts...)
	})
}

// ResizeProcess updates the window size of a process's pseudo-terminal inside the environment.
func (s *Server) ResizeProcess(ctx context.Context, req *ateenvv1alpha.ResizeProcessRequest) (*ateenvv1alpha.ResizeProcessResponse, error) {
	envID, atespace, err := envFromContext(ctx)
	if err != nil {
		return nil, err
	}
	targetActor := atespace + "/" + envID
	conn, outCtx, closeFn, fastPath, err := s.guestSession(ctx, atespace, envID, false)
	if err != nil {
		return nil, err
	}
	resp, err := ateenvv1alpha.NewProcessServiceClient(conn).ResizeProcess(outCtx, req)
	closeFn()
	if s.evictOnGuestUnavailable(ctx, targetActor, err) && fastPath {
		conn2, outCtx2, closeFn2, _, werr := s.guestSession(ctx, atespace, envID, true)
		if werr != nil {
			return nil, werr
		}
		defer closeFn2()
		resp, err = ateenvv1alpha.NewProcessServiceClient(conn2).ResizeProcess(outCtx2, req)
		s.evictOnGuestUnavailable(ctx, targetActor, err)
		return resp, err
	}
	return resp, err
}

// StreamProcessOutput streams stdout, stderr and the exit message from a process.
func (s *Server) StreamProcessOutput(req *ateenvv1alpha.StreamProcessOutputRequest, stream grpc.ServerStreamingServer[ateenvv1alpha.ProcessOutput]) error {
	ctx := stream.Context()
	envID, atespace, err := envFromContext(ctx)
	if err != nil {
		return err
	}
	targetActor := atespace + "/" + envID
	conn, outCtx, closeFn, fastPath, err := s.guestSession(ctx, atespace, envID, false)
	if err != nil {
		return err
	}
	defer func() { closeFn() }()

	clientStream, err := ateenvv1alpha.NewProcessServiceClient(conn).StreamProcessOutput(outCtx, req)
	if s.evictOnGuestUnavailable(ctx, targetActor, err) && fastPath {
		closeFn()
		closeFn = noopClose
		conn2, outCtx2, closeFn2, fastPath2, werr := s.guestSession(ctx, atespace, envID, true)
		if werr != nil {
			return werr
		}
		conn, outCtx, closeFn, fastPath = conn2, outCtx2, closeFn2, fastPath2
		clientStream, err = ateenvv1alpha.NewProcessServiceClient(conn).StreamProcessOutput(outCtx, req)
		s.evictOnGuestUnavailable(ctx, targetActor, err)
	}
	if err != nil {
		return err
	}
	first := true
	for {
		msg, err := clientStream.Recv()
		if s.evictOnGuestUnavailable(ctx, targetActor, err) && first && fastPath {
			closeFn()
			closeFn = noopClose
			conn2, outCtx2, closeFn2, fastPath2, werr := s.guestSession(ctx, atespace, envID, true)
			if werr != nil {
				return werr
			}
			conn, outCtx, closeFn, fastPath = conn2, outCtx2, closeFn2, fastPath2
			clientStream, err = ateenvv1alpha.NewProcessServiceClient(conn).StreamProcessOutput(outCtx, req)
			if err != nil {
				s.evictOnGuestUnavailable(ctx, targetActor, err)
				return err
			}
			first = false
			continue
		}
		first = false
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(msg); err != nil {
			return err
		}
	}
}

// WriteProcessInput forwards stdin chunks to a process inside the environment.
func (s *Server) WriteProcessInput(stream grpc.ClientStreamingServer[ateenvv1alpha.WriteProcessInputRequest, ateenvv1alpha.WriteProcessInputResponse]) error {
	// The environment's identity goes on the server span first, so a stream
	// whose first message fails still carries it (FS-16), as WriteFile does.
	ctx := stream.Context()
	envID, atespace, err := envFromContext(ctx)
	if err != nil {
		return err
	}
	targetActor := atespace + "/" + envID
	// Receive the first message before waking the guest, so a request the API
	// rejects (one with unknown fields) does not resume a suspended environment.
	// A client that opens a stream and sends nothing therefore holds no guest
	// connection until it closes.
	req, err := nextMessage(stream)
	if err != nil {
		return err
	}
	conn, outCtx, closeFn, _, err := s.guestSession(ctx, atespace, envID, true)
	if err != nil {
		return err
	}
	defer closeFn()

	clientStream, err := ateenvv1alpha.NewProcessServiceClient(conn).WriteProcessInput(outCtx)
	if err != nil {
		s.evictOnGuestUnavailable(ctx, targetActor, err)
		return err
	}
	for req != nil {
		if err := clientStream.Send(req); err != nil {
			// The guest rejected the stream; surface its status instead of io.EOF.
			if errors.Is(err, io.EOF) {
				_, err = clientStream.CloseAndRecv()
			}
			s.evictOnGuestUnavailable(ctx, targetActor, err)
			return err
		}
		if req, err = nextMessage(stream); err != nil {
			return err
		}
	}

	resp, err := clientStream.CloseAndRecv()
	if err != nil {
		s.evictOnGuestUnavailable(ctx, targetActor, err)
		return err
	}
	return stream.SendAndClose(resp)
}

// ============================================================================
// --- FILESYSTEM SERVICE ---
// ============================================================================

// ReadFile streams file contents from the target environment.
func (s *Server) ReadFile(req *ateenvv1alpha.ReadFileRequest, stream grpc.ServerStreamingServer[ateenvv1alpha.ReadFileResponse]) error {
	ctx := stream.Context()
	envID, atespace, err := envFromContext(ctx)
	if err != nil {
		return err
	}
	targetActor := atespace + "/" + envID
	conn, outCtx, closeFn, fastPath, err := s.guestSession(ctx, atespace, envID, false)
	if err != nil {
		return err
	}
	defer func() { closeFn() }()

	clientStream, err := ateenvv1alpha.NewFileSystemServiceClient(conn).ReadFile(outCtx, req)
	if s.evictOnGuestUnavailable(ctx, targetActor, err) && fastPath {
		closeFn()
		closeFn = noopClose
		conn2, outCtx2, closeFn2, fastPath2, werr := s.guestSession(ctx, atespace, envID, true)
		if werr != nil {
			return werr
		}
		conn, outCtx, closeFn, fastPath = conn2, outCtx2, closeFn2, fastPath2
		clientStream, err = ateenvv1alpha.NewFileSystemServiceClient(conn).ReadFile(outCtx, req)
		s.evictOnGuestUnavailable(ctx, targetActor, err)
	}
	if err != nil {
		return err
	}

	first := true
	for {
		chunk, err := clientStream.Recv()
		if s.evictOnGuestUnavailable(ctx, targetActor, err) && first && fastPath {
			closeFn()
			closeFn = noopClose
			conn2, outCtx2, closeFn2, fastPath2, werr := s.guestSession(ctx, atespace, envID, true)
			if werr != nil {
				return werr
			}
			conn, outCtx, closeFn, fastPath = conn2, outCtx2, closeFn2, fastPath2
			clientStream, err = ateenvv1alpha.NewFileSystemServiceClient(conn).ReadFile(outCtx, req)
			if err != nil {
				s.evictOnGuestUnavailable(ctx, targetActor, err)
				return err
			}
			first = false
			continue
		}
		first = false
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(chunk); err != nil {
			return err
		}
	}
}

// WriteFile streams file contents to the target environment.
func (s *Server) WriteFile(stream grpc.ClientStreamingServer[ateenvv1alpha.WriteFileRequest, ateenvv1alpha.WriteFileResponse]) error {
	ctx := stream.Context()
	envID, atespace, err := envFromContext(ctx)
	if err != nil {
		return err
	}
	targetActor := atespace + "/" + envID
	// Receive the first message before waking the guest, as WriteProcessInput
	// does.
	req, err := nextMessage(stream)
	if err != nil {
		return err
	}
	conn, outCtx, closeFn, _, err := s.guestSession(ctx, atespace, envID, true)
	if err != nil {
		return err
	}
	defer closeFn()

	clientStream, err := ateenvv1alpha.NewFileSystemServiceClient(conn).WriteFile(outCtx)
	if err != nil {
		s.evictOnGuestUnavailable(ctx, targetActor, err)
		return err
	}

	for req != nil {
		if err := clientStream.Send(req); err != nil {
			// The guest rejected the stream; surface its status instead of io.EOF.
			if errors.Is(err, io.EOF) {
				_, err = clientStream.CloseAndRecv()
			}
			s.evictOnGuestUnavailable(ctx, targetActor, err)
			return err
		}
		if req, err = nextMessage(stream); err != nil {
			return err
		}
	}

	resp, err := clientStream.CloseAndRecv()
	if err != nil {
		s.evictOnGuestUnavailable(ctx, targetActor, err)
		return err
	}
	return stream.SendAndClose(resp)
}

// ============================================================================
// --- HELPERS ---
// ============================================================================

// envFromContext returns the environment id and atespace of a guest call,
// from its x-env-id and x-env-atespace metadata, and stamps them on the
// call's server span.
func envFromContext(ctx context.Context) (string, string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", "", status.Error(codes.InvalidArgument, "missing environment metadata in request context")
	}
	ids := md.Get("x-env-id")
	if len(ids) == 0 || ids[0] == "" {
		return "", "", status.Error(codes.InvalidArgument, "x-env-id header is required")
	}
	atespaces := md.Get("x-env-atespace")
	atespace := DefaultAtespace
	if len(atespaces) > 0 && atespaces[0] != "" {
		atespace = atespaces[0]
	}
	setSpanEnvAttributes(ctx, atespace, ids[0])
	return ids[0], atespace, nil
}

func (s *Server) dialEphemeralRouterConn(targetActor string) (*grpc.ClientConn, error) {
	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(s.routerCredentials()),
		grpc.WithChainUnaryInterceptor(s.routerUnaryInterceptor(targetActor)),
		grpc.WithChainStreamInterceptor(s.routerStreamInterceptor(targetActor)),
	}
	if s.client != nil && s.client.TracingEnabled() {
		dialOpts = append(dialOpts, ate.Options{EnableTracing: true}.TracingDialOptions()...)
	}
	return grpc.NewClient(s.routerAddr, dialOpts...)
}

func noopClose() {}

func (s *Server) pickPooledRouterConn() (*grpc.ClientConn, error) {
	s.routerPoolMu.Lock()
	if s.routerClosed {
		s.routerPoolMu.Unlock()
		return nil, status.Error(codes.Unavailable, "server is closed")
	}
	if len(s.routerConns) == 0 {
		dialOpts := []grpc.DialOption{
			grpc.WithTransportCredentials(s.routerCredentials()),
			grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`),
			grpc.WithChainUnaryInterceptor(s.routerUnaryInterceptor("")),
			grpc.WithChainStreamInterceptor(s.routerStreamInterceptor("")),
		}
		if s.client != nil && s.client.TracingEnabled() {
			dialOpts = append(dialOpts, ate.Options{EnableTracing: true}.TracingDialOptions()...)
		}
		target := s.routerAddr
		if !strings.Contains(target, "://") {
			target = "dns:///" + target
		}
		conns := make([]*grpc.ClientConn, 0, s.RouterPoolSize)
		for range s.RouterPoolSize {
			c, err := grpc.NewClient(target, dialOpts...)
			if err != nil {
				for _, prev := range conns {
					_ = prev.Close()
				}
				s.routerPoolMu.Unlock()
				return nil, status.Errorf(codes.Internal, "dialing pooled guest router %s: %v", target, err)
			}
			conns = append(conns, c)
		}
		s.routerConns = conns
	}
	conns := s.routerConns
	s.routerPoolMu.Unlock()
	idx := (s.routerConnSeq.Add(1) - 1) % uint64(len(conns))
	return conns[idx], nil
}

// guestSession prepares a router gRPC connection and outgoing context for an
// environment's guest, and waits within WakeBudget for the guest to answer
// unless ReactiveWake is enabled, forceWake is false, and the environment is
// cached as awake (fastPath == true). The returned closeFn is always non-nil
// (a no-op on error or when pooled).
//
// Calls on the returned connection take ctx for its deadline, cancellation, and
// trace span (plus targetActorCtxKey when pooled): routerUnaryInterceptor and
// routerStreamInterceptor are the only writers of ate-target-actor, and no
// incoming caller metadata is forwarded. Forwarding caller metadata once sent
// ate-target-actor twice, which the Substrate router rejects
// (agent-substrate/substrate#2308 answers duplicate ate-target-actor headers
// with "invalid actor reference"), and it would let a client pass routing
// headers through the proxy. Nothing in the guest reads caller metadata.
func (s *Server) guestSession(ctx context.Context, atespace, id string, forceWake bool) (*grpc.ClientConn, context.Context, func(), bool, error) {
	if s.routerAddr == "" {
		return nil, nil, noopClose, false, status.Error(codes.FailedPrecondition, "router address not configured for guest operations")
	}
	if atespace == "" {
		atespace = DefaultAtespace
	}
	targetActor := atespace + "/" + id

	var (
		conn    *grpc.ClientConn
		outCtx  context.Context
		closeFn func()
		err     error
	)
	if s.RouterPoolSize > 0 {
		conn, err = s.pickPooledRouterConn()
		if err != nil {
			return nil, nil, noopClose, false, err
		}
		outCtx = context.WithValue(ctx, targetActorCtxKey{}, targetActor)
		closeFn = noopClose
	} else {
		if s.isRouterClosed() {
			return nil, nil, noopClose, false, status.Error(codes.Unavailable, "server is closed")
		}
		conn, err = s.dialEphemeralRouterConn(targetActor)
		if err != nil {
			return nil, nil, noopClose, false, status.Errorf(codes.Internal, "dialing guest router for %s: %v", targetActor, err)
		}
		outCtx = ctx
		closeFn = func() { _ = conn.Close() }
	}

	var wakeGen uint64
	if s.ReactiveWake && s.awakeEnvs != nil {
		if !forceWake && s.awakeEnvs.Contains(targetActor) {
			return conn, outCtx, closeFn, true, nil
		}
		wakeGen = s.awakeEnvs.Generation(targetActor)
	}
	if err := s.ensureAwake(outCtx, conn, atespace, id); err != nil {
		if s.ReactiveWake && s.awakeEnvs != nil {
			s.awakeEnvs.Remove(targetActor)
		}
		closeFn()
		return nil, nil, noopClose, false, err
	}
	if s.ReactiveWake && s.awakeEnvs != nil {
		s.awakeEnvs.AddIfGen(targetActor, wakeGen)
	}
	return conn, outCtx, closeFn, false, nil
}

// routerCredentials are the transport credentials for the router: TLS with
// the pod identity certificate when RouterTLS is set, else plaintext.
func (s *Server) routerCredentials() credentials.TransportCredentials {
	if s.RouterTLS != nil {
		return routertls.TransportCredentials(s.RouterTLS)
	}
	return insecure.NewCredentials()
}

// ActorToEnvironment converts an ateapipb Actor to an ateenvv1alpha Environment.
func ActorToEnvironment(actor *ateapipb.Actor) *ateenvv1alpha.Environment {
	if actor == nil {
		return nil
	}
	templateName := ""
	templateAtespace := ""
	if tmpl := actor.GetActorTemplate(); tmpl != nil {
		templateName = tmpl.GetName()
		templateAtespace = tmpl.GetAtespace()
	}
	return &ateenvv1alpha.Environment{
		Id:       actor.GetMetadata().GetName(),
		Atespace: actor.GetMetadata().GetAtespace(),
		Template: &ateenvv1alpha.Template{
			Name:     templateName,
			Atespace: templateAtespace,
		},
		Status:         ActorStatusToEnvStatus(actor.GetStatus()),
		Node:           actorNode(actor.GetStatus()),
		WorkerSelector: actor.GetWorkerSelector().GetMatchLabels(),
		CreateTime:     actor.GetMetadata().GetCreateTime(),
	}
}

// actorNode is the node an actor is on: the one hosting its worker, or its
// local snapshot while it is suspended (assigned_node), which the worker
// assignment confirms while it runs. Empty before the first placement and
// after a detach.
func actorNode(st *ateapipb.ActorStatus) string {
	if node := st.GetAssignedNode(); node != "" {
		return node
	}
	return st.GetWorkerAssignment().GetNodeName()
}

// ActorStatusToEnvStatus maps an ateapipb ActorStatus to an ateenvv1alpha EnvironmentStatus.
func ActorStatusToEnvStatus(st *ateapipb.ActorStatus) ateenvv1alpha.EnvironmentStatus {
	if st == nil {
		return ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_UNSPECIFIED
	}
	switch st.GetState() {
	case ateapipb.ActorState_ACTOR_STATE_RESUMING:
		return ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_RESUMING
	case ateapipb.ActorState_ACTOR_STATE_RUNNING:
		return ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_RUNNING
	case ateapipb.ActorState_ACTOR_STATE_SUSPENDING:
		return ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_SUSPENDING
	case ateapipb.ActorState_ACTOR_STATE_SUSPENDED:
		// Since substrate#2620 a paused actor is SUSPENDED too; Substrate has
		// no PAUSED or PAUSING state. The latest snapshot tells the two
		// apart: a local suspend (SUSPEND_MODE_LOCAL) leaves it on a node
		// with its upload pending, and the actor resumes on that node only.
		// Once the upload lands the environment is SUSPENDED like any other.
		// ENVIRONMENT_STATUS_PAUSING is never produced: Substrate reports
		// SUSPENDING for both modes.
		if localOnly(latestSnapshot(st)) {
			return ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_PAUSED
		}
		return ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_SUSPENDED
	case ateapipb.ActorState_ACTOR_STATE_CRASHED:
		return ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_CRASHED
	case ateapipb.ActorState_ACTOR_STATE_DELETING:
		return ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_DELETING
	default:
		return ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_UNSPECIFIED
	}
}

// latestSnapshot returns the actor's snapshot with the highest generation, or
// nil when it has none.
func latestSnapshot(st *ateapipb.ActorStatus) *ateapipb.Snapshot {
	var latest *ateapipb.Snapshot
	for _, snap := range st.GetSnapshots() {
		if latest == nil || snap.GetGeneration() > latest.GetGeneration() {
			latest = snap
		}
	}
	return latest
}

// localOnly reports whether snap exists on a node and not yet in object
// storage: what a local suspend leaves until its background upload lands.
func localOnly(snap *ateapipb.Snapshot) bool {
	return snap.GetLocality() != "" &&
		snap.GetDurableSnapshot().GetState() != ateapipb.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED
}

func toGRPCError(err error) error {
	if err == nil {
		return nil
	}
	if s, ok := status.FromError(err); ok && s.Code() != codes.Unknown {
		return err
	}
	switch {
	case errors.Is(err, ate.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	default:
		return status.Errorf(codes.Internal, "%v", err)
	}
}

// nextMessage receives the next message of a client stream: nil when the
// client closed the stream.
func nextMessage[Req any](stream interface{ Recv() (*Req, error) }) (*Req, error) {
	req, err := stream.Recv()
	if errors.Is(err, io.EOF) {
		return nil, nil
	}
	return req, err
}

// ============================================================================
// --- TELEMETRY ---
// ============================================================================

// pendingCreate is an environment created through this replica whose guest
// has not answered yet.
type pendingCreate struct {
	atespace string
	start    time.Time
}

// Bounds on the create_to_ready bookkeeping. An environment that is created
// and never used, deleted or crashed through this replica (another replica
// served its calls, or the client went away) would otherwise stay pending
// forever. Entries older than pendingTTL are dropped unobserved when the map
// is full; maxAtespaces caps the atespace label of ate_env_api_environments.
const (
	maxPending   = 10000
	pendingTTL   = time.Hour
	maxAtespaces = 64
)

// environmentListConcurrency bounds the ListActors calls EnvironmentCounts
// has in flight, so a scrape over up to maxAtespaces atespaces finishes
// within the collector's timeout without flooding the control plane.
const environmentListConcurrency = 8

// requestedAt returns the start of create_to_ready for a CreateEnvironment
// call received at now: the client's x-ate-env-requested-at when it is a
// valid RFC 3339 time no later than now, else now. A time in the future
// (client clock ahead) falls back to now rather than producing a negative
// or shortened duration.
func requestedAt(ctx context.Context, now time.Time) time.Time {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return now
	}
	vals := md.Get(telemetry.RequestedAtHeader)
	if len(vals) == 0 {
		return now
	}
	t, err := time.Parse(time.RFC3339Nano, vals[0])
	if err != nil || t.After(now) {
		return now
	}
	return t
}

// trackCreate starts the create_to_ready clock of a created environment.
func (s *Server) trackCreate(atespace, id string, start time.Time) {
	if s.Metrics == nil {
		return
	}
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	if s.pending == nil {
		s.pending = make(map[string]pendingCreate)
	}
	if s.atespaces == nil {
		s.atespaces = map[string]bool{DefaultAtespace: true}
	}
	if !s.atespaces[atespace] && len(s.atespaces) < maxAtespaces {
		s.atespaces[atespace] = true
		s.Metrics.InitAtespace(atespace)
	}
	if len(s.pending) >= maxPending {
		cutoff := time.Now().Add(-pendingTTL)
		for k, p := range s.pending {
			if p.start.Before(cutoff) {
				delete(s.pending, k)
			}
		}
		if len(s.pending) >= maxPending {
			return
		}
	}
	s.pending[atespace+"/"+id] = pendingCreate{atespace: atespace, start: start}
}

// ObserveFirstContact ends create_to_ready (status ok) for an environment
// whose port the port proxy reached: like a guest RPC through ensureAwake,
// a CONNECT the router accepts means the environment answers. It does
// nothing for an environment that is not pending, or without Metrics.
func (s *Server) ObserveFirstContact(atespace, id string) {
	s.finishCreate(atespace, id, telemetry.ReadyStatusOK)
}

// ObserveCrashedContact ends create_to_ready (status crashed) for an
// environment that the port proxy found CRASHED behind a router answer, as
// ensureAwake does for a guest call. It does nothing for an environment that
// is not pending, or without Metrics.
func (s *Server) ObserveCrashedContact(atespace, id string) {
	s.finishCreate(atespace, id, telemetry.ReadyStatusCrashed)
}

// finishCreate observes create_to_ready with readyStatus for a pending
// environment, once; later calls for the same environment do nothing.
func (s *Server) finishCreate(atespace, id, readyStatus string) {
	if s.Metrics == nil {
		return
	}
	key := atespace + "/" + id
	s.readyMu.Lock()
	p, ok := s.pending[key]
	if ok {
		delete(s.pending, key)
	}
	s.readyMu.Unlock()
	if ok {
		s.Metrics.ObserveCreateToReady(p.atespace, readyStatus, time.Since(p.start))
	}
}

// environmentStates are the state label values of ate_env_api_environments,
// reported as zero when no environment is in them so the series stay put.
var environmentStates = []string{"unspecified", "resuming", "running", "suspending", "suspended", "crashed", "deleting"}

func environmentState(st *ateapipb.ActorStatus) string {
	return strings.ToLower(strings.TrimPrefix(ActorStatusToEnvStatus(st).String(), "ENVIRONMENT_STATUS_"))
}

// EnvironmentCounts lists the actors of every atespace this replica has
// created environments in, DefaultAtespace always included, and counts them
// by environment state. It is the source of ate_env_api_environments: the
// control plane is the truth, so the gauge survives restarts and agrees
// across replicas, at the cost of one ListActors per atespace per scrape
// (cached by the collector for --metrics-environments-ttl, default 60 s). The atespaces are listed concurrently,
// at most environmentListConcurrency at a time.
func (s *Server) EnvironmentCounts(ctx context.Context) ([]telemetry.EnvironmentCount, error) {
	s.readyMu.Lock()
	spaces := []string{DefaultAtespace}
	for a := range s.atespaces {
		if a != DefaultAtespace {
			spaces = append(spaces, a)
		}
	}
	s.readyMu.Unlock()
	return countEnvironments(ctx, spaces, environmentListConcurrency, s.client.List)
}

// countEnvironments lists the actors of each atespace in spaces with list,
// running at most limit calls at once, and counts them by environment
// state. The first error cancels the calls still running and is returned.
func countEnvironments(ctx context.Context, spaces []string, limit int,
	list func(ctx context.Context, atespace string) ([]*ateapipb.Actor, error)) ([]telemetry.EnvironmentCount, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make([][]*ateapipb.Actor, len(spaces))
	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	sem := make(chan struct{}, max(limit, 1))
	for i, a := range spaces {
		sem <- struct{}{}
		if ctx.Err() != nil {
			<-sem
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			actors, err := list(ctx, a)
			if err != nil {
				errOnce.Do(func() {
					firstErr = err
					cancel()
				})
				return
			}
			results[i] = actors
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	out := make([]telemetry.EnvironmentCount, 0, len(spaces)*len(environmentStates))
	for i, a := range spaces {
		counts := make(map[string]int, len(environmentStates))
		for _, st := range environmentStates {
			counts[st] = 0
		}
		for _, actor := range results[i] {
			counts[environmentState(actor.GetStatus())]++
		}
		for st, n := range counts {
			out = append(out, telemetry.EnvironmentCount{Atespace: a, State: st, Count: n})
		}
	}
	return out, nil
}

// ActorUIDs returns the uid of each actor of an atespace, by id: what the
// store's orphan sweep compares its objects with (envstore.ActorUIDs).
func (s *Server) ActorUIDs(ctx context.Context, atespace string) (map[string]string, error) {
	actors, err := s.client.List(ctx, atespace)
	if err != nil {
		return nil, err
	}
	uids := make(map[string]string, len(actors))
	for _, a := range actors {
		uids[a.GetMetadata().GetName()] = a.GetMetadata().GetUid()
	}
	return uids, nil
}
