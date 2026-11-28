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

// Package ate implements the environment abstraction directly on top of
// Agent Substrate; it backs the ate-env-api service. A
// Environment wraps a Substrate actor: it can be created and deleted, and while running it
// accepts remote command execution and filesystem operations served by the
// ate-env-guest daemon inside the actor.
//
// Lifecycle operations go to the ateapi gRPC control plane; command and
// filesystem operations go through the atenet HTTP router, which routes
// requests by Host header to the actor's guest daemon.
//
// Clients outside this repository use the env package, which talks to
// the ate-env-api service instead.
package ate

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"
	"sync"

	"github.com/agent-substrate/env/internal/routertls"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TargetActorHeader identifies the actor selected for ingress routing as
// "<atespace>/<actor>". HTTP field names are case-insensitive; this uses its
// HTTP/2 wire form so dataplane configuration and metadata are native.
const TargetActorHeader = "ate-target-actor"

// PreviewHeader is the gRPC metadata key with which a client opts in to the
// preview features ate-api-server has enabled; kubectl-ate --preview sends
// it. CreateActorTemplate sends it for a template that names its own
// snapshotConfig.storageLocation, a PerTemplateSnapshotLocation preview
// feature: a derived template inherits the field from a base that was itself
// created with the opt-in, and a server without the gate rejects the field
// either way.
const PreviewHeader = "ate-preview-allowed"

// DefaultHostSuffix is the DNS suffix the atenet router uses to identify
// actors: requests with Host "<id>.<suffix>" are routed to actor <id>.
const DefaultHostSuffix = "actors.resources.substrate.ate.dev"

// DefaultControlAddr and DefaultRouterAddr are the in-cluster addresses of
// the Substrate control plane and router, as installed by Agent Substrate
// in the ate-system namespace. They are the defaults for a ate-env-api running
// inside the cluster.
const (
	DefaultControlAddr = "api.ate-system.svc.cluster.local:443"
	// DefaultRouterAddr and DefaultRouterConnectAddr are the router's plaintext
	// listeners, which only the fakes in tests serve: Substrate v0.6.0, the
	// first release this module pairs with, removed them.
	DefaultRouterAddr = "atenet-router.ate-system.svc.cluster.local:80"
	// DefaultRouterConnectAddr is the router's plaintext CONNECT listener, which
	// tunnels to a port on an actor other than 80.
	DefaultRouterConnectAddr = "atenet-router.ate-system.svc.cluster.local:8081"
	// DefaultRouterTLSAddr and DefaultRouterConnectTLSAddr are the router's TLS
	// listeners, the only ones Substrate v0.6.0 serves; they need a client
	// certificate (Options.RouterTLS).
	DefaultRouterTLSAddr        = "atenet-router.ate-system.svc.cluster.local:443"
	DefaultRouterConnectTLSAddr = "atenet-router.ate-system.svc.cluster.local:8444"
)

// DefaultAtespace is the Substrate atespace every environment actor lives in.
const DefaultAtespace = "ate-env"

// ErrNotFound is returned when an env, file, or directory does not exist.
var ErrNotFound = errors.New("not found")

// ErrAlreadyExists is returned when a resource with the same name exists.
var ErrAlreadyExists = errors.New("already exists")

// egressPolicyName is the name of an actor's egress policy. Substrate's
// EgressPolicy is a singleton subresource of its actor.
const egressPolicyName = "default"

// Options configures a Client.
type Options struct {
	// ControlAddr is the ateapi gRPC endpoint, e.g. "localhost:8080"
	// (typically a port-forward of svc/api in ate-system). Required.
	ControlAddr string

	// RouterAddr is the atenet HTTP router endpoint, e.g. "localhost:8000"
	// (typically a port-forward of svc/atenet-router in ate-system).
	// Required for Cmd and filesystem operations.
	RouterAddr string

	// HostSuffix overrides the router host suffix. Defaults to
	// DefaultHostSuffix.
	HostSuffix string

	// RouterTLS, when set, is the TLS client configuration for the router:
	// the pod identity certificate and the trust bundle that verifies the
	// router (see internal/routertls). Nil dials the router in plaintext,
	// which only a Substrate before v0.6.0, or the fake router in tests,
	// serves.
	RouterTLS *tls.Config

	// SkipVerify disables TLS certificate verification on the control
	// plane connection. Set this when talking to a port-forwarded ateapi,
	// which serves with pod certificates not signed by a public CA (the
	// Substrate demos and kubectl-ate connect the same way).
	SkipVerify bool

	// TLSConfig overrides the TLS configuration for the control plane
	// connection.
	TLSConfig *tls.Config

	// BearerTokenFile, if set, is a file whose contents are sent as a
	// bearer token on every control-plane RPC. Point it at a projected
	// ServiceAccount token with the ateapi audience when the control
	// plane requires authentication. The file is re-read on each RPC so
	// rotated tokens are picked up.
	BearerTokenFile string

	// EnableTracing adds the OpenTelemetry gRPC client handler to the
	// control-plane and guest connections, so the W3C trace context of each
	// call's ctx travels to ateapi and the guest and the calls show as
	// client spans. It uses the global tracer provider and propagator (see
	// telemetry.SetupOTel). Off, the connections are as before.
	EnableTracing bool
}

// Client manages environments on a Substrate cluster.
type Client struct {
	opts    Options
	conn    *grpc.ClientConn
	control ateapipb.ControlClient

	mu        sync.Mutex
	guestPool map[string]*grpc.ClientConn
}

// maxControlRecvBytes is the largest control-plane response the client
// reads. The lists ask for Substrate's largest page, 1,000 objects, and a
// crashed actor alone carries a message of up to 4,096 bytes, so a full page
// is over gRPC's default of 4 MiB.
const maxControlRecvBytes = 32 << 20

// New creates a Client. The control-plane connection is established
// lazily on first use.
func New(opts Options) (*Client, error) {
	if opts.ControlAddr == "" {
		return nil, errors.New("ate: Options.ControlAddr is required")
	}
	if opts.HostSuffix == "" {
		opts.HostSuffix = DefaultHostSuffix
	}

	var creds credentials.TransportCredentials
	if opts.TLSConfig != nil {
		creds = credentials.NewTLS(opts.TLSConfig)
	} else {
		creds = credentials.NewTLS(&tls.Config{InsecureSkipVerify: opts.SkipVerify})
	}

	// svc/api in ate-system is headless, so the DNS resolver sees every
	// ate-api-server pod. The default pick_first policy would still pin
	// this client to one of them.
	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxControlRecvBytes)),
	}
	if opts.BearerTokenFile != "" {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(fileTokenCreds{path: opts.BearerTokenFile}))
	}
	dialOpts = append(dialOpts, opts.TracingDialOptions()...)
	conn, err := grpc.NewClient(opts.ControlAddr, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("ate: dialing control plane: %w", err)
	}

	return &Client{
		opts:    opts,
		conn:    conn,
		control: ateapipb.NewControlClient(conn),
	}, nil
}

// NewControlClient returns a Client backed directly by control, without
// opening a gRPC transport connection. Callers in unit tests use it to drive
// control-plane logic inside testing/synctest bubbles without OS sockets.
func NewControlClient(control ateapipb.ControlClient) *Client {
	return &Client{
		opts:    Options{HostSuffix: DefaultHostSuffix},
		control: control,
	}
}

// Close releases the control-plane connection and any cached guest connections.
func (c *Client) Close() error {
	c.mu.Lock()
	for _, conn := range c.guestPool {
		_ = conn.Close()
	}
	c.guestPool = nil
	c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// TracingEnabled reports whether the client was created with EnableTracing.
func (c *Client) TracingEnabled() bool { return c.opts.EnableTracing }

// TracingDialOptions returns the dial options that add the OpenTelemetry
// client handler when EnableTracing is set, and none otherwise. Other
// connections to Substrate (the API server's own guest connections) use it
// to trace the same way.
func (o Options) TracingDialOptions() []grpc.DialOption {
	if !o.EnableTracing {
		return nil
	}
	return []grpc.DialOption{grpc.WithStatsHandler(otelgrpc.NewClientHandler())}
}

// DialGuest dials the guest daemon inside actor id in atespace via the atenet router.
func (c *Client) DialGuest(atespace, id string) (*grpc.ClientConn, error) {
	if c.opts.RouterAddr == "" {
		return nil, errors.New("ate: Options.RouterAddr is required for guest operations")
	}
	if id == "" {
		return nil, errors.New("ate: id is required")
	}
	if atespace == "" {
		atespace = DefaultAtespace
	}
	key := atespace + "/" + id

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.guestPool == nil {
		c.guestPool = make(map[string]*grpc.ClientConn)
	}
	if conn, ok := c.guestPool[key]; ok {
		return conn, nil
	}

	targetActor := atespace + "/" + id
	routerCreds := insecure.NewCredentials()
	if c.opts.RouterTLS != nil {
		routerCreds = routertls.TransportCredentials(c.opts.RouterTLS)
	}
	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(routerCreds),
		grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			ctx = metadata.AppendToOutgoingContext(ctx, TargetActorHeader, targetActor)
			return invoker(ctx, method, req, reply, cc, opts...)
		}),
		grpc.WithChainStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			ctx = metadata.AppendToOutgoingContext(ctx, TargetActorHeader, targetActor)
			return streamer(ctx, desc, cc, method, opts...)
		}),
	}
	dialOpts = append(dialOpts, c.opts.TracingDialOptions()...)
	conn, err := grpc.NewClient(c.opts.RouterAddr, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("ate: dialing guest %s: %w", key, err)
	}
	c.guestPool[key] = conn
	return conn, nil
}

// fileTokenCreds sends the contents of a file as a bearer token on each RPC.
type fileTokenCreds struct{ path string }

func (c fileTokenCreds) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	b, err := os.ReadFile(c.path)
	if err != nil {
		return nil, fmt.Errorf("ate: reading bearer token file: %w", err)
	}
	return map[string]string{"authorization": "Bearer " + strings.TrimSpace(string(b))}, nil
}

func (c fileTokenCreds) RequireTransportSecurity() bool { return true }

// EnsureAtespace creates the atespace with name if it does not already exist.
// If name is empty, it defaults to DefaultAtespace.
func (c *Client) EnsureAtespace(ctx context.Context, name string) error {
	if name == "" {
		name = DefaultAtespace
	}
	ref := &ateapipb.ObjectRef{Name: name}
	if _, err := c.control.GetAtespace(ctx, &ateapipb.GetAtespaceRequest{Atespace: ref}); err == nil {
		return nil
	} else if status.Code(err) != codes.NotFound {
		return wrapGRPCError(err)
	}
	// Create only when it is known to be missing: an identity allowed to use
	// an atespace but not to create one must still start.
	_, err := c.control.CreateAtespace(ctx, &ateapipb.CreateAtespaceRequest{
		Atespace: &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: name}},
	})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return wrapGRPCError(err)
	}
	return nil
}

// CreateOptions configures the creation of a new actor.
type CreateOptions struct {
	ID       string
	Template string
	Atespace string
	// WorkerSelector, when non-empty, is the actor's own worker selector:
	// labels the worker pool must carry, matched by exact equality and ANDed
	// by Substrate with the template's selector.
	WorkerSelector map[string]string
	// SourceTag, when set, is the Tag in Atespace the actor is seeded from
	// (Actor.source_tag): it resumes from the tag's snapshot instead of
	// booting. The tag must have been taken under Template.
	SourceTag string
}

// check validates opts and returns the atespace with the default applied.
func (o CreateOptions) check() (string, error) {
	if o.ID == "" {
		return "", errors.New("ate: CreateOptions.ID is required")
	}
	if o.Template == "" {
		return "", errors.New("ate: CreateOptions.Template is required")
	}
	if o.Atespace == "" {
		return DefaultAtespace, nil
	}
	return o.Atespace, nil
}

// Create registers a new actor from opts and starts it, creating its
// atespace first when it is missing. ID and Template are required. Atespace
// is the Substrate atespace the actor and template live in. It returns the
// actor as the control plane stored it.
func (c *Client) Create(ctx context.Context, opts CreateOptions) (*ateapipb.Actor, error) {
	atespace, err := opts.check()
	if err != nil {
		return nil, err
	}
	if err := c.EnsureAtespace(ctx, atespace); err != nil {
		return nil, fmt.Errorf("ate: creating %q: %w", opts.ID, err)
	}
	return c.CreateActor(ctx, opts)
}

// CreateActor registers a new actor from opts with one CreateActor call, in
// an atespace the caller knows exists (EnsureAtespace): an error is that
// call's, so a caller that retries can tell a refusal from a call cut off.
// ID and Template are required. It returns the actor as the control plane
// stored it.
func (c *Client) CreateActor(ctx context.Context, opts CreateOptions) (*ateapipb.Actor, error) {
	atespace, err := opts.check()
	if err != nil {
		return nil, err
	}
	actor := &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{
			Atespace: atespace,
			Name:     opts.ID,
		},
		ActorTemplate: &ateapipb.ObjectRef{
			Atespace: atespace,
			Name:     opts.Template,
		},
	}
	if len(opts.WorkerSelector) > 0 {
		actor.WorkerSelector = &ateapipb.Selector{MatchLabels: maps.Clone(opts.WorkerSelector)}
	}
	if opts.SourceTag != "" {
		actor.SourceTag = &ateapipb.ObjectRef{Atespace: atespace, Name: opts.SourceTag}
	}
	created, err := c.control.CreateActor(ctx, &ateapipb.CreateActorRequest{Actor: actor})
	if err != nil {
		return nil, fmt.Errorf("ate: creating %q: %w", opts.ID, wrapGRPCError(err))
	}
	return created, nil
}

// Get retrieves the actor with ID in atespace from the control plane.
// If atespace is empty, DefaultAtespace is used.
func (c *Client) Get(ctx context.Context, atespace, id string) (*ateapipb.Actor, error) {
	if id == "" {
		return nil, errors.New("ate: id is required")
	}
	actor, err := c.control.GetActor(ctx, &ateapipb.GetActorRequest{Actor: c.ref(atespace, id)})
	if err != nil {
		return nil, fmt.Errorf("ate: getting %q: %w", id, wrapGRPCError(err))
	}
	return actor, nil
}

// List returns every actor in atespace, following the control plane's
// pagination. If atespace is empty, DefaultAtespace is used.
func (c *Client) List(ctx context.Context, atespace string) ([]*ateapipb.Actor, error) {
	if atespace == "" {
		atespace = DefaultAtespace
	}
	return c.listActors(ctx, atespace)
}

// ListAll returns every actor of every atespace, following the control
// plane's pagination. It takes the global viewer role, as ListWorkers does.
func (c *Client) ListAll(ctx context.Context) ([]*ateapipb.Actor, error) {
	return c.listActors(ctx, "")
}

// ListPage returns one page of the actors in atespace and the control
// plane's token for the next page, empty on the last one. pageToken is the
// token of the previous page, empty for the first. If atespace is empty,
// DefaultAtespace is used.
func (c *Client) ListPage(ctx context.Context, atespace string, pageSize int32, pageToken string) ([]*ateapipb.Actor, string, error) {
	if atespace == "" {
		atespace = DefaultAtespace
	}
	return c.listActorsPage(ctx, atespace, pageSize, pageToken)
}

// listActors lists the actors of atespace, or of every atespace when it is
// empty.
func (c *Client) listActors(ctx context.Context, atespace string) ([]*ateapipb.Actor, error) {
	var actors []*ateapipb.Actor
	token := ""
	for {
		page, next, err := c.listActorsPage(ctx, atespace, 1000, token)
		if err != nil {
			return nil, err
		}
		actors = append(actors, page...)
		token = next
		if token == "" {
			return actors, nil
		}
	}
}

// listActorsPage reads one page of the actors of atespace, or of every
// atespace when it is empty.
func (c *Client) listActorsPage(ctx context.Context, atespace string, pageSize int32, pageToken string) ([]*ateapipb.Actor, string, error) {
	resp, err := c.control.ListActors(ctx, &ateapipb.ListActorsRequest{
		Atespace: atespace,
		Options:  &ateapipb.ListOptions{PageToken: pageToken, PageSize: pageSize},
	})
	if err != nil {
		return nil, "", fmt.Errorf("ate: listing actors in %q: %w", atespace, wrapGRPCError(err))
	}
	return resp.GetActors(), resp.GetNextPageToken(), nil
}

// ListAtespaces returns every atespace, following the control plane's
// pagination.
func (c *Client) ListAtespaces(ctx context.Context) ([]*ateapipb.Atespace, error) {
	var atespaces []*ateapipb.Atespace
	token := ""
	for {
		resp, err := c.control.ListAtespaces(ctx, &ateapipb.ListAtespacesRequest{
			Options: &ateapipb.ListOptions{PageToken: token, PageSize: 1000},
		})
		if err != nil {
			return nil, fmt.Errorf("ate: listing atespaces: %w", wrapGRPCError(err))
		}
		atespaces = append(atespaces, resp.GetAtespaces()...)
		token = resp.GetNextPageToken()
		if token == "" {
			return atespaces, nil
		}
	}
}

// ListAllActorTemplates returns every ActorTemplate of every atespace,
// following the control plane's pagination.
func (c *Client) ListAllActorTemplates(ctx context.Context) ([]*ateapipb.ActorTemplate, error) {
	var templates []*ateapipb.ActorTemplate
	token := ""
	for {
		resp, err := c.control.ListActorTemplates(ctx, &ateapipb.ListActorTemplatesRequest{
			Options: &ateapipb.ListOptions{PageToken: token, PageSize: 1000},
		})
		if err != nil {
			return nil, fmt.Errorf("ate: listing templates: %w", wrapGRPCError(err))
		}
		templates = append(templates, resp.GetActorTemplates()...)
		token = resp.GetNextPageToken()
		if token == "" {
			return templates, nil
		}
	}
}

// ListAllTags returns every Tag of every atespace, following the control
// plane's pagination.
func (c *Client) ListAllTags(ctx context.Context) ([]*ateapipb.Tag, error) {
	var tags []*ateapipb.Tag
	token := ""
	for {
		resp, err := c.control.ListTags(ctx, &ateapipb.ListTagsRequest{
			Options: &ateapipb.ListOptions{PageToken: token, PageSize: 1000},
		})
		if err != nil {
			return nil, fmt.Errorf("ate: listing tags: %w", wrapGRPCError(err))
		}
		tags = append(tags, resp.GetTags()...)
		token = resp.GetNextPageToken()
		if token == "" {
			return tags, nil
		}
	}
}

// ListWorkers returns every worker the control plane knows, following its
// pagination. Workers are global: listing them takes the global viewer
// role, and a caller without it gets PermissionDenied.
func (c *Client) ListWorkers(ctx context.Context) ([]*ateapipb.Worker, error) {
	var workers []*ateapipb.Worker
	token := ""
	for {
		resp, err := c.control.ListWorkers(ctx, &ateapipb.ListWorkersRequest{
			Options: &ateapipb.ListOptions{PageToken: token},
		})
		if err != nil {
			return nil, fmt.Errorf("ate: listing workers: %w", wrapGRPCError(err))
		}
		workers = append(workers, resp.GetWorkers()...)
		token = resp.GetNextPageToken()
		if token == "" {
			return workers, nil
		}
	}
}

// Suspend checkpoints and stops the actor with ID in atespace.
// If atespace is empty, DefaultAtespace is used.
//
// The suspend is durable: it returns once the snapshot is in object storage,
// thus a suspended environment survives the loss of its node. Substrate's
// default became asynchronous in substrate#2805 (the snapshot stays on the
// node and uploads in the background); ate-env keeps the durable behavior it
// always had here and offers the other as SuspendLocal.
func (c *Client) Suspend(ctx context.Context, atespace, id string) error {
	return c.suspend(ctx, atespace, id, ateapipb.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS)
}

// SuspendLocal checkpoints and stops the actor with ID in atespace and returns
// once the snapshot is on the worker's node; the upload to object storage goes
// on in the background. Until it lands the actor resumes on that node only,
// and a node lost before then takes the snapshot with it. One
// checkpoint-and-resume cycle is cheaper than with Suspend, which is what a
// caller that parks an environment between every command wants.
// If atespace is empty, DefaultAtespace is used.
func (c *Client) SuspendLocal(ctx context.Context, atespace, id string) error {
	return c.suspend(ctx, atespace, id, ateapipb.DurabilityMode_DURABILITY_MODE_ASYNCHRONOUS)
}

// Resume wakes an actor through the control plane and returns it placed:
// Substrate's ResumeActor returns once the actor is RUNNING and its template's
// wakeup probe answered, or fails (the actor is CRASHED then). A new actor is
// born suspended and unplaced; nothing but a resume, the router's on a guest
// call or this one, puts it on a node. The caller's role must allow it
// (editor of the atespace).
func (c *Client) Resume(ctx context.Context, atespace, id string) (*ateapipb.Actor, error) {
	if id == "" {
		return nil, errors.New("ate: id is required")
	}
	resp, err := c.control.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: c.ref(atespace, id)})
	if err != nil {
		return nil, fmt.Errorf("ate: resuming %q: %w", id, wrapGRPCError(err))
	}
	return resp.GetActor(), nil
}

func (c *Client) suspend(ctx context.Context, atespace, id string, mode ateapipb.DurabilityMode) error {
	req := &ateapipb.SuspendActorRequest{
		Actor:   c.ref(atespace, id),
		Options: &ateapipb.SuspendOptions{Mode: mode},
	}
	if _, err := c.control.SuspendActor(ctx, req); err != nil {
		return fmt.Errorf("ate: suspending %q: %w", id, wrapGRPCError(err))
	}
	return nil
}

// Delete removes the actor with ID in atespace permanently, in any state.
// If atespace is empty, DefaultAtespace is used.
func (c *Client) Delete(ctx context.Context, atespace, id string) error {
	return c.DeleteWithUID(ctx, atespace, id, "")
}

// DeleteWithUID deletes the actor with ID in atespace as Delete does, and
// only when it still has that uid: for another actor of the name, created
// since the caller read the uid, Substrate refuses the delete with Aborted
// and the actor stays. An empty uid sends no precondition.
func (c *Client) DeleteWithUID(ctx context.Context, atespace, id, uid string) error {
	// Delete calls DeleteActor directly; ate-env requires Substrate v0.6.0 or
	// later. There, DeleteActor takes an actor in any state: it marks the
	// actor DELETING, terminates the workload, releases the worker and
	// deletes the actor's snapshots. Older Substrate deleted only suspended
	// actors, thus Delete suspended first. That suspend gave the workload no
	// graceful stop, its snapshot was deleted right away, and Substrate
	// rejects it for a CRASHED actor, which then could not be deleted.
	req := &ateapipb.DeleteActorRequest{Actor: c.ref(atespace, id)}
	if uid != "" {
		req.Options = &ateapipb.DeleteOptions{Uid: uid}
	}
	_, err := c.control.DeleteActor(ctx, req)
	if err != nil {
		return fmt.Errorf("actor: deleting %q: %w", id, wrapGRPCError(err))
	}
	c.evictGuest(atespace, id)
	return nil
}

// evictGuest closes and forgets the pooled guest connection of a deleted
// actor, so a workload that creates and deletes thousands of environments
// does not keep a connection for each.
func (c *Client) evictGuest(atespace, id string) {
	if atespace == "" {
		atespace = DefaultAtespace
	}
	c.mu.Lock()
	conn := c.guestPool[atespace+"/"+id]
	delete(c.guestPool, atespace+"/"+id)
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// ref returns the ObjectRef identifying the actor backing actor id in atespace.
// If atespace is empty, DefaultAtespace is used.
// GetActorTemplate reads an ActorTemplate. A missing template is ErrNotFound.
func (c *Client) GetActorTemplate(ctx context.Context, atespace, name string) (*ateapipb.ActorTemplate, error) {
	if name == "" {
		return nil, errors.New("ate: template name is required")
	}
	tmpl, err := c.control.GetActorTemplate(ctx, &ateapipb.GetActorTemplateRequest{ActorTemplate: c.ref(atespace, name)})
	if err != nil {
		return nil, fmt.Errorf("ate: getting template %q: %w", name, wrapGRPCError(err))
	}
	return tmpl, nil
}

// CreateActorTemplate creates an ActorTemplate in its metadata's atespace. A
// name collision is ErrAlreadyExists, so callers racing to create the same
// derived template can treat it as success.
func (c *Client) CreateActorTemplate(ctx context.Context, tmpl *ateapipb.ActorTemplate) (*ateapipb.ActorTemplate, error) {
	name := tmpl.GetMetadata().GetName()
	if name == "" {
		return nil, errors.New("ate: template metadata.name is required")
	}
	if tmpl.GetMetadata().GetAtespace() == "" {
		tmpl.Metadata.Atespace = DefaultAtespace
	}
	if tmpl.GetSnapshotConfig().GetStorageLocation() != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, PreviewHeader, "true")
	}
	created, err := c.control.CreateActorTemplate(ctx, &ateapipb.CreateActorTemplateRequest{ActorTemplate: tmpl})
	if err != nil {
		if status.Code(err) == codes.AlreadyExists {
			return nil, fmt.Errorf("ate: creating template %q: %w: %s", name, ErrAlreadyExists, status.Convert(err).Message())
		}
		return nil, fmt.Errorf("ate: creating template %q: %w", name, wrapGRPCError(err))
	}
	return created, nil
}

// DeleteActorTemplate deletes the ActorTemplate name in atespace, but only
// while it still has uid: a template that a concurrent caller created
// under the same name since is kept, and Substrate answers Aborted. A
// missing template is ErrNotFound.
func (c *Client) DeleteActorTemplate(ctx context.Context, atespace, name, uid string) error {
	req := &ateapipb.DeleteActorTemplateRequest{
		ActorTemplate: c.ref(atespace, name),
		Options:       &ateapipb.DeleteOptions{Uid: uid},
	}
	if _, err := c.control.DeleteActorTemplate(ctx, req); err != nil {
		return fmt.Errorf("ate: deleting template %q: %w", name, wrapGRPCError(err))
	}
	return nil
}

// CreateEgressPolicy creates the egress policy of the actor with ID in
// atespace with rules, and returns it as stored, with Substrate's port
// defaults applied. No rules is a policy that denies all egress. A second
// create is ErrAlreadyExists. A create under a missing actor is Substrate's
// FailedPrecondition. If atespace is empty, DefaultAtespace is used.
func (c *Client) CreateEgressPolicy(ctx context.Context, atespace, id string, rules []*ateapipb.EgressRule) (*ateapipb.EgressPolicy, error) {
	if id == "" {
		return nil, errors.New("ate: id is required")
	}
	ref := c.ref(atespace, id)
	created, err := c.control.CreateActorEgressPolicy(ctx, &ateapipb.CreateActorEgressPolicyRequest{
		Actor: ref,
		EgressPolicy: &ateapipb.EgressPolicy{
			Metadata: &ateapipb.ResourceMetadata{Atespace: ref.GetAtespace(), Name: egressPolicyName},
			Rules:    rules,
		},
	})
	if err != nil {
		if status.Code(err) == codes.AlreadyExists {
			return nil, fmt.Errorf("ate: creating egress policy of %q: %w: %s", id, ErrAlreadyExists, status.Convert(err).Message())
		}
		return nil, fmt.Errorf("ate: creating egress policy of %q: %w", id, wrapGRPCError(err))
	}
	return created, nil
}

// GetEgressPolicy reads the egress policy of the actor with ID in atespace.
// An actor without a policy, or a missing actor, is ErrNotFound. If atespace
// is empty, DefaultAtespace is used.
func (c *Client) GetEgressPolicy(ctx context.Context, atespace, id string) (*ateapipb.EgressPolicy, error) {
	if id == "" {
		return nil, errors.New("ate: id is required")
	}
	policy, err := c.control.GetActorEgressPolicy(ctx, &ateapipb.GetActorEgressPolicyRequest{Actor: c.ref(atespace, id), Name: egressPolicyName})
	if err != nil {
		return nil, fmt.Errorf("ate: getting egress policy of %q: %w", id, wrapGRPCError(err))
	}
	return policy, nil
}

// UpdateEgressPolicy replaces the rules of the egress policy of the actor
// with ID in atespace, and returns it as stored. It writes only while the
// policy still has uid and version, as read by GetEgressPolicy: a policy
// written or recreated since is Substrate's Aborted. A missing policy is
// ErrNotFound. If atespace is empty, DefaultAtespace is used.
func (c *Client) UpdateEgressPolicy(ctx context.Context, atespace, id, uid string, version int64, rules []*ateapipb.EgressRule) (*ateapipb.EgressPolicy, error) {
	if id == "" {
		return nil, errors.New("ate: id is required")
	}
	if uid == "" || version == 0 {
		return nil, errors.New("ate: egress policy uid and version are required")
	}
	ref := c.ref(atespace, id)
	updated, err := c.control.UpdateActorEgressPolicy(ctx, &ateapipb.UpdateActorEgressPolicyRequest{
		Actor: ref,
		EgressPolicy: &ateapipb.EgressPolicy{
			Metadata: &ateapipb.ResourceMetadata{Atespace: ref.GetAtespace(), Name: egressPolicyName, Uid: uid, Version: version},
			Rules:    rules,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("ate: updating egress policy of %q: %w", id, wrapGRPCError(err))
	}
	return updated, nil
}

func (c *Client) ref(atespace, id string) *ateapipb.ObjectRef {
	if atespace == "" {
		atespace = DefaultAtespace
	}
	return &ateapipb.ObjectRef{Atespace: atespace, Name: id}
}

// CreateTag tags the snapshot of the suspended actor id in atespace as name,
// scoped to that atespace. Substrate copies the snapshot into the tag, after
// uploading it first when a local suspend left it on the node, thus the call
// can take as long as that upload. If atespace is empty, DefaultAtespace is
// used.
func (c *Client) CreateTag(ctx context.Context, atespace, id, name string) (*ateapipb.Tag, error) {
	ref := c.ref(atespace, id)
	tag, err := c.control.CreateTag(ctx, &ateapipb.CreateTagRequest{Tag: &ateapipb.Tag{
		Metadata:    &ateapipb.ResourceMetadata{Atespace: ref.GetAtespace(), Name: name},
		Scope:       ateapipb.TagScope_TAG_SCOPE_ATESPACE,
		SourceActor: ref,
	}})
	if err != nil {
		return nil, fmt.Errorf("ate: tagging %q as %q: %w", id, name, wrapGRPCError(err))
	}
	return tag, nil
}

// DeleteTag deletes the Tag name in atespace and the snapshot it owns. An
// actor seeded from the tag that has not suspended since becomes
// unrecoverable, so callers delete a tag only once no such actor exists. If
// atespace is empty, DefaultAtespace is used.
func (c *Client) DeleteTag(ctx context.Context, atespace, name string) error {
	if _, err := c.control.DeleteTag(ctx, &ateapipb.DeleteTagRequest{Tag: c.ref(atespace, name)}); err != nil {
		return fmt.Errorf("ate: deleting tag %q: %w", name, wrapGRPCError(err))
	}
	return nil
}

func wrapGRPCError(err error) error {
	if status.Code(err) == codes.NotFound {
		return fmt.Errorf("%w: %s", ErrNotFound, status.Convert(err).Message())
	}
	return err
}
