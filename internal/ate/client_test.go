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

package ate_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/internaltest/fakecontrol"
	"github.com/agent-substrate/env/internal/internaltest/fakerouter"
	"github.com/agent-substrate/env/internal/internaltest/testcerts"
	"github.com/agent-substrate/env/internal/routertls"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fixture struct {
	control *fakecontrol.Server
	router  *fakerouter.Router
	client  *ate.Client
	guest   string // guest workdir
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	t.Chdir(t.TempDir())

	control := fakecontrol.New()
	controlAddr, stopControl, err := control.Serve()
	if err != nil {
		t.Fatalf("starting fake control plane: %v", err)
	}
	t.Cleanup(stopControl)

	router := fakerouter.New()
	router.Running = func(id string) bool {
		return control.Status(id) == ateapipb.ActorState_ACTOR_STATE_RUNNING
	}
	routerAddr, stopRouter := router.Serve()
	t.Cleanup(stopRouter)

	client, err := ate.New(ate.Options{
		ControlAddr: controlAddr,
		RouterAddr:  routerAddr,
		SkipVerify:  true,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	return &fixture{control: control, router: router, client: client, guest: t.TempDir()}
}

// create makes a env with a registered router route.
func (f *fixture) create(t *testing.T, id string) {
	t.Helper()
	f.router.Register(id, http.NotFoundHandler())
	req := ate.CreateOptions{ID: id, Template: "default-template"}
	if _, err := f.client.Create(t.Context(), req); err != nil {
		t.Fatalf("creating actor %q: %v", id, err)
	}
}

func TestCreateStartsEnv(t *testing.T) {
	f := newFixture(t)
	f.create(t, "sb-1")
}

func TestEnsureAtespace(t *testing.T) {
	f := newFixture(t)
	if err := f.client.EnsureAtespace(t.Context(), "custom-space"); err != nil {
		t.Fatalf("EnsureAtespace: %v", err)
	}
	// Ensuring again should be a no-op (AlreadyExists ignored)
	if err := f.client.EnsureAtespace(t.Context(), "custom-space"); err != nil {
		t.Fatalf("EnsureAtespace idempotent call: %v", err)
	}
}

func TestEnsureAtespaceWithoutCreatePermission(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	// An existing atespace is accepted through GetAtespace alone.
	if err := f.client.EnsureAtespace(ctx, "shared-space"); err != nil {
		t.Fatalf("EnsureAtespace (create): %v", err)
	}
	f.control.DenyCreateAtespace = true
	if err := f.client.EnsureAtespace(ctx, "shared-space"); err != nil {
		t.Fatalf("EnsureAtespace with create denied on an existing atespace: %v", err)
	}
	// A missing one still fails loudly rather than being silently skipped.
	if err := f.client.EnsureAtespace(ctx, "absent-space"); err == nil {
		t.Fatal("EnsureAtespace with create denied on a missing atespace: want error, got nil")
	}
}

func TestSuspend(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.create(t, "sb-susp")

	if err := f.client.Suspend(ctx, "", "sb-susp"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if st := f.control.Status("sb-susp"); st != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("status = %v, want SUSPENDED", st)
	}
	// Substrate's default is asynchronous since substrate#2805; ate-env asks
	// for a durable suspend.
	if got := f.control.SuspendModes; len(got) != 1 || got[0] != ateapipb.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS {
		t.Fatalf("suspend modes = %v, want one SYNCHRONOUS", got)
	}
}

func TestSuspendLocal(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.create(t, "sb-local")

	if err := f.client.SuspendLocal(ctx, "", "sb-local"); err != nil {
		t.Fatalf("SuspendLocal: %v", err)
	}
	if st := f.control.Status("sb-local"); st != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("status = %v, want SUSPENDED", st)
	}
	if got := f.control.SuspendModes; len(got) != 1 || got[0] != ateapipb.DurabilityMode_DURABILITY_MODE_ASYNCHRONOUS {
		t.Fatalf("suspend modes = %v, want one ASYNCHRONOUS", got)
	}
}

// A new actor is suspended and unplaced until it is resumed; Resume places
// it and returns it RUNNING on its node.
func TestResume(t *testing.T) {
	f := newFixture(t)
	f.control.BornSuspended = true
	f.control.AddWorker(fakecontrol.Worker("ate-system", "pool", "node-1", nil, 4))
	f.create(t, "sb-res")
	if st := f.control.Status("sb-res"); st != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("status after create = %v, want SUSPENDED", st)
	}
	actor, err := f.client.Resume(t.Context(), "", "sb-res")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if st := actor.GetStatus(); st.GetState() != ateapipb.ActorState_ACTOR_STATE_RUNNING || st.GetWorkerAssignment().GetNodeName() != "node-1" {
		t.Errorf("resumed actor status = %v, want RUNNING on node-1", st)
	}
	if st := f.control.Status("sb-res"); st != ateapipb.ActorState_ACTOR_STATE_RUNNING {
		t.Errorf("status after resume = %v, want RUNNING", st)
	}
	if _, err := f.client.Resume(t.Context(), "", ""); err == nil {
		t.Error("Resume without an id succeeded")
	}
}

func TestSuspendMissing(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	if err := f.client.Suspend(ctx, "", "nonexistent"); !errors.Is(err, ate.ErrNotFound) {
		t.Fatalf("Suspend nonexistent: err = %v, want ErrNotFound", err)
	}
}

func TestDelete(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.create(t, "sb-life")
	conn, err := f.client.DialGuest("", "sb-life")
	if err != nil {
		t.Fatalf("DialGuest: %v", err)
	}

	if err := f.client.Delete(ctx, "", "sb-life"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Substrate v0.6.0 deletes a running actor: no suspend first.
	if got := f.control.SuspendModes; len(got) != 0 {
		t.Fatalf("suspend calls = %v, want none", got)
	}
	// The pooled guest connection goes with the actor, so a create-and-delete
	// workload does not keep one per environment it ever had.
	if conn.GetState() != connectivity.Shutdown {
		t.Fatalf("pooled guest connection after Delete: %s, want Shutdown", conn.GetState())
	}
	if again, err := f.client.DialGuest("", "sb-life"); err != nil || again == conn {
		t.Fatalf("DialGuest after Delete = %p, %v; want a new connection", again, err)
	}
	// A missing actor is ErrNotFound, which callers count as deleted.
	if err := f.client.Delete(ctx, "", "sb-life"); !errors.Is(err, ate.ErrNotFound) {
		t.Fatalf("Delete of a missing actor: err = %v, want ErrNotFound", err)
	}
}

// Delete removes an actor in each state with DeleteActor alone, also a
// CRASHED one, whose suspend Substrate rejects.
func TestDeleteInAnyState(t *testing.T) {
	for _, state := range []ateapipb.ActorState{
		ateapipb.ActorState_ACTOR_STATE_RUNNING,
		ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
		ateapipb.ActorState_ACTOR_STATE_CRASHED,
		ateapipb.ActorState_ACTOR_STATE_RESUMING,
		ateapipb.ActorState_ACTOR_STATE_REVERTING,
		ateapipb.ActorState_ACTOR_STATE_DELETING,
	} {
		t.Run(state.String(), func(t *testing.T) {
			f := newFixture(t)
			f.create(t, "sb-del")
			f.control.SetState("sb-del", state)

			if err := f.client.Delete(t.Context(), "", "sb-del"); err != nil {
				t.Fatalf("Delete of a %s actor: %v", state, err)
			}
			if _, err := f.client.Get(t.Context(), "", "sb-del"); !errors.Is(err, ate.ErrNotFound) {
				t.Fatalf("Get after Delete: err = %v, want ErrNotFound", err)
			}
			if got := f.control.SuspendModes; len(got) != 0 {
				t.Fatalf("suspend calls = %v, want none", got)
			}
		})
	}
}

func TestCreateRequiresTemplate(t *testing.T) {
	control := fakecontrol.New()
	addr, stop, err := control.Serve()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)

	client, err := ate.New(ate.Options{ControlAddr: addr, SkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })

	if _, err := client.Create(context.Background(), ate.CreateOptions{ID: "sb-x"}); err == nil {
		t.Fatal("Create without template succeeded, want error")
	}
}

// The guest's gRPC goes to the router's HTTPS listener since Substrate
// v0.6.0: the client presents the pod identity certificate over h2.
func TestDialGuestOverTLS(t *testing.T) {
	control := fakecontrol.New()
	controlAddr, stopControl, err := control.Serve()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stopControl)
	ca := testcerts.NewAuthority(t, "test podcert")
	router := fakerouter.New()
	router.Register("env1", grpc.NewServer()) // no services: any RPC answers Unimplemented
	addr, stop := router.ServeTLS(&tls.Config{
		Certificates: []tls.Certificate{ca.Server(t, "atenet-router.ate-system.svc")},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca.Pool(),
	})
	t.Cleanup(stop)
	dir := t.TempDir()
	cfg, err := routertls.Options{
		CredentialBundleFile: testcerts.WriteFile(t, dir, "bundle.pem", ca.ClientBundle(t, routertls.SPIFFEID("ate-env", "ate-env-api"))),
		TrustBundleFile:      testcerts.WriteFile(t, dir, "trust.pem", ca.PEM),
		ServerName:           "atenet-router.ate-system.svc",
	}.Config()
	if err != nil {
		t.Fatal(err)
	}
	client, err := ate.New(ate.Options{ControlAddr: controlAddr, RouterAddr: addr, SkipVerify: true, RouterTLS: cfg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	conn, err := client.DialGuest("ate-env", "env1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = conn.Invoke(ctx, "/ateenv.test.Nothing/Ping", &emptypb.Empty{}, &emptypb.Empty{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("RPC through the TLS router: %v, want Unimplemented from the guest's gRPC server", err)
	}

	// Without TLS the client speaks h2c to a TLS listener; the call fails
	// with a transport error, no handshake takes place.
	plain, err := ate.New(ate.Options{ControlAddr: controlAddr, RouterAddr: addr, SkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { plain.Close() })
	conn2, err := plain.DialGuest("ate-env", "env1")
	if err != nil {
		t.Fatal(err)
	}
	if err := conn2.Invoke(ctx, "/ateenv.test.Nothing/Ping", &emptypb.Empty{}, &emptypb.Empty{}); err == nil || status.Code(err) == codes.Unimplemented {
		t.Fatalf("plaintext to a TLS router: %v, want a transport error", err)
	}
}

func TestNewControlClientInSynctest(t *testing.T) {
	synctest.Test(t, func(bt *testing.T) {
		ctrl := fakecontrol.New()
		raw := ctrl.Client()
		client := ate.NewControlClient(raw)

		ctx := context.Background()
		if err := client.EnsureAtespace(ctx, "bubble-space"); err != nil {
			bt.Fatalf("EnsureAtespace: %v", err)
		}
		if _, err := raw.GetAtespace(ctx, &ateapipb.GetAtespaceRequest{
			Atespace: &ateapipb.ObjectRef{Name: "bubble-space"},
		}); err != nil {
			bt.Fatalf("GetAtespace: %v", err)
		}
		if _, err := raw.CreateActorTemplate(ctx, &ateapipb.CreateActorTemplateRequest{
			ActorTemplate: &ateapipb.ActorTemplate{
				Metadata: &ateapipb.ResourceMetadata{Atespace: "bubble-space", Name: "tpl"},
			},
		}); err != nil {
			bt.Fatalf("CreateActorTemplate: %v", err)
		}
		if _, err := raw.GetActorTemplate(ctx, &ateapipb.GetActorTemplateRequest{
			ActorTemplate: &ateapipb.ObjectRef{Atespace: "bubble-space", Name: "tpl"},
		}); err != nil {
			bt.Fatalf("GetActorTemplate: %v", err)
		}
		if _, err := client.Create(ctx, ate.CreateOptions{Atespace: "bubble-space", ID: "a1", Template: "tpl"}); err != nil {
			bt.Fatalf("Create: %v", err)
		}
		if _, err := client.Get(ctx, "bubble-space", "a1"); err != nil {
			bt.Fatalf("Get: %v", err)
		}
		if err := client.Suspend(ctx, "bubble-space", "a1"); err != nil {
			bt.Fatalf("Suspend: %v", err)
		}
		if _, err := raw.ResumeActor(ctx, &ateapipb.ResumeActorRequest{
			Actor: &ateapipb.ObjectRef{Atespace: "bubble-space", Name: "a1"},
		}); err != nil {
			bt.Fatalf("ResumeActor: %v", err)
		}
		if _, err := raw.ListActors(ctx, &ateapipb.ListActorsRequest{Atespace: "bubble-space"}); err != nil {
			bt.Fatalf("ListActors: %v", err)
		}
		ctrl.AddWorker(fakecontrol.Worker("ate-system", "pool", "node-1", nil, 4))
		if workers, err := client.ListWorkers(ctx); err != nil || len(workers) != 1 {
			bt.Fatalf("ListWorkers: %v, %d workers, want 1", err, len(workers))
		}
		if err := client.Delete(ctx, "bubble-space", "a1"); err != nil {
			bt.Fatalf("Delete: %v", err)
		}
		if _, err := raw.DeleteActorTemplate(ctx, &ateapipb.DeleteActorTemplateRequest{
			ActorTemplate: &ateapipb.ObjectRef{Atespace: "bubble-space", Name: "tpl"},
		}); err != nil {
			bt.Fatalf("DeleteActorTemplate: %v", err)
		}

		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		for name, call := range map[string]func() error{
			"GetActor":       func() error { _, err := raw.GetActor(cancelled, &ateapipb.GetActorRequest{}); return err },
			"CreateActor":    func() error { _, err := raw.CreateActor(cancelled, &ateapipb.CreateActorRequest{}); return err },
			"ResumeActor":    func() error { _, err := raw.ResumeActor(cancelled, &ateapipb.ResumeActorRequest{}); return err },
			"SuspendActor":   func() error { _, err := raw.SuspendActor(cancelled, &ateapipb.SuspendActorRequest{}); return err },
			"DeleteActor":    func() error { _, err := raw.DeleteActor(cancelled, &ateapipb.DeleteActorRequest{}); return err },
			"ListActors":     func() error { _, err := raw.ListActors(cancelled, &ateapipb.ListActorsRequest{}); return err },
			"ListWorkers":    func() error { _, err := raw.ListWorkers(cancelled, &ateapipb.ListWorkersRequest{}); return err },
			"CreateAtespace": func() error { _, err := raw.CreateAtespace(cancelled, &ateapipb.CreateAtespaceRequest{}); return err },
			"GetAtespace":    func() error { _, err := raw.GetAtespace(cancelled, &ateapipb.GetAtespaceRequest{}); return err },
			"GetActorTemplate": func() error {
				_, err := raw.GetActorTemplate(cancelled, &ateapipb.GetActorTemplateRequest{})
				return err
			},
			"CreateActorTemplate": func() error {
				_, err := raw.CreateActorTemplate(cancelled, &ateapipb.CreateActorTemplateRequest{})
				return err
			},
			"DeleteActorTemplate": func() error {
				_, err := raw.DeleteActorTemplate(cancelled, &ateapipb.DeleteActorTemplateRequest{})
				return err
			},
		} {
			if err := call(); status.Code(err) != codes.Canceled {
				bt.Fatalf("%s on cancelled ctx: code=%v (%v), want Canceled", name, status.Code(err), err)
			}
		}

		if err := client.Close(); err != nil {
			bt.Fatalf("Close: %v", err)
		}
	})
}

// A template that names its own snapshotConfig.storageLocation uses the
// PerTemplateSnapshotLocation preview feature, so its create carries the
// preview opt-in header; one without the field does not.
func TestCreateActorTemplatePreviewOptIn(t *testing.T) {
	control := fakecontrol.New()
	addr, stop, err := control.Serve()
	if err != nil {
		t.Fatalf("starting fake control plane: %v", err)
	}
	t.Cleanup(stop)
	client, err := ate.New(ate.Options{ControlAddr: addr, SkipVerify: true})
	if err != nil {
		t.Fatalf("ate.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	plain := &ateapipb.ActorTemplate{
		Metadata:   &ateapipb.ResourceMetadata{Name: "plain", Atespace: "ate-env"},
		Containers: []*ateapipb.Container{{Name: "guest", Image: "example.com/guest@sha256:aaaa"}},
	}
	if _, err := client.CreateActorTemplate(t.Context(), plain); err != nil {
		t.Fatalf("CreateActorTemplate(plain): %v", err)
	}
	if got := control.LastTemplateCreateMetadata().Get(ate.PreviewHeader); len(got) != 0 {
		t.Errorf("template without a storageLocation sent %s=%q, want no header", ate.PreviewHeader, got)
	}

	located := &ateapipb.ActorTemplate{
		Metadata:       &ateapipb.ResourceMetadata{Name: "located", Atespace: "ate-env"},
		Containers:     []*ateapipb.Container{{Name: "guest", Image: "example.com/guest@sha256:aaaa"}},
		SnapshotConfig: &ateapipb.SnapshotConfig{StorageLocation: "gs://bucket/prefix/"},
	}
	if _, err := client.CreateActorTemplate(t.Context(), located); err != nil {
		t.Fatalf("CreateActorTemplate(located): %v", err)
	}
	if got := control.LastTemplateCreateMetadata().Get(ate.PreviewHeader); len(got) != 1 || got[0] != "true" {
		t.Errorf("template with a storageLocation sent %s=%q, want [true]", ate.PreviewHeader, got)
	}
}

// TestListAllFitsAFullPageOfCrashedActors: Substrate's largest page is 1,000
// actors, and a crashed actor carries a message of up to 4,096 bytes, so a
// full page is over gRPC's default 4 MiB receive limit. The client must
// still read it.
func TestListAllFitsAFullPageOfCrashedActors(t *testing.T) {
	control := fakecontrol.New()
	control.AddTemplate(&ateapipb.ActorTemplate{Metadata: &ateapipb.ResourceMetadata{Atespace: "ate-env", Name: "tmpl"}})
	message := strings.Repeat("x", 4096)
	// The fake stamps a new actor with the wall clock, and the encoded size
	// of a timestamp follows its nanoseconds: early in a second the page was
	// under the limit. Every actor gets the same times, on a whole second
	// (the shortest encoding), so the page has the same size on every run.
	at := &timestamppb.Timestamp{Seconds: 1794441600} // 2026-11-12T00:00:00Z
	const n = 1000
	for i := range n {
		name := fmt.Sprintf("env-%04d", i)
		if _, err := control.CreateActor(t.Context(), &ateapipb.CreateActorRequest{Actor: &ateapipb.Actor{
			Metadata:      &ateapipb.ResourceMetadata{Atespace: "ate-env", Name: name},
			ActorTemplate: &ateapipb.ObjectRef{Atespace: "ate-env", Name: "tmpl"},
		}}); err != nil {
			t.Fatalf("CreateActor(%s): %v", name, err)
		}
		control.MutateActor(name, func(a *ateapipb.Actor) {
			a.Metadata.CreateTime, a.Metadata.UpdateTime = at, at
			a.WorkerSelector = &ateapipb.Selector{MatchLabels: map[string]string{"ate-env/pool": "default"}}
			a.Status = &ateapipb.ActorStatus{
				State: ateapipb.ActorState_ACTOR_STATE_CRASHED,
				Crash: &ateapipb.ActorCrash{Message: message, CrashTime: at},
			}
		})
	}
	addr, stop, err := control.Serve()
	if err != nil {
		t.Fatalf("starting fake control plane: %v", err)
	}
	t.Cleanup(stop)
	client, err := ate.New(ate.Options{ControlAddr: addr, SkipVerify: true})
	if err != nil {
		t.Fatalf("ate.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	actors, err := client.ListAll(t.Context())
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(actors) != n {
		t.Fatalf("ListAll returned %d actors, want %d", len(actors), n)
	}
	if size := proto.Size(&ateapipb.ListActorsResponse{Actors: actors}); size <= 4<<20 {
		t.Fatalf("the page is %d bytes, not over the 4 MiB default: the test does not test the limit", size)
	}
}

// TestListsFollowThePagination: every list reads all pages. The fake serves
// pages of two, over gRPC, and each list must return every object once.
func TestListsFollowThePagination(t *testing.T) {
	control := fakecontrol.New()
	control.MaxPageSize = 2
	const n = 5
	var wantSpaces, wantNames []string
	for i := range n {
		space, name := fmt.Sprintf("space-%d", i), fmt.Sprintf("obj-%d", i)
		wantSpaces, wantNames = append(wantSpaces, space), append(wantNames, name)
		if _, err := control.CreateAtespace(t.Context(), &ateapipb.CreateAtespaceRequest{
			Atespace: &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: space}},
		}); err != nil {
			t.Fatal(err)
		}
		control.AddTemplate(&ateapipb.ActorTemplate{Metadata: &ateapipb.ResourceMetadata{Atespace: "ate-env", Name: name}})
		control.AddTag(&ateapipb.Tag{Metadata: &ateapipb.ResourceMetadata{Atespace: "ate-env", Name: name}})
		control.AddWorker(fakecontrol.Worker("ate-env", "pool", name, nil, 8))
		// Actors in two atespaces: List reads one, ListAll both.
		for _, atespace := range []string{"ate-env", "other"} {
			if _, err := control.CreateActor(t.Context(), &ateapipb.CreateActorRequest{Actor: &ateapipb.Actor{
				Metadata:      &ateapipb.ResourceMetadata{Atespace: atespace, Name: name + "-" + atespace},
				ActorTemplate: &ateapipb.ObjectRef{Atespace: "ate-env", Name: name},
			}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	addr, stop, err := control.Serve()
	if err != nil {
		t.Fatalf("starting fake control plane: %v", err)
	}
	t.Cleanup(stop)
	client, err := ate.New(ate.Options{ControlAddr: addr, SkipVerify: true})
	if err != nil {
		t.Fatalf("ate.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	ctx := t.Context()

	check := func(list string, got, want []string) {
		t.Helper()
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", list, got, want)
		}
	}
	actorNames := func(actors []*ateapipb.Actor) (names []string) {
		for _, a := range actors {
			names = append(names, a.GetMetadata().GetName())
		}
		return names
	}
	suffixed := func(suffixes ...string) (names []string) {
		for _, name := range wantNames {
			for _, s := range suffixes {
				names = append(names, name+s)
			}
		}
		return names
	}

	all, err := client.ListAll(ctx)
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	check("ListAll", actorNames(all), suffixed("-ate-env", "-other"))

	one, err := client.List(ctx, "other")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	check("List", actorNames(one), suffixed("-other"))

	atespaces, err := client.ListAtespaces(ctx)
	if err != nil {
		t.Fatalf("ListAtespaces: %v", err)
	}
	var got []string
	for _, a := range atespaces {
		got = append(got, a.GetMetadata().GetName())
	}
	check("ListAtespaces", got, wantSpaces)

	templates, err := client.ListAllActorTemplates(ctx)
	if err != nil {
		t.Fatalf("ListAllActorTemplates: %v", err)
	}
	got = nil
	for _, tmpl := range templates {
		got = append(got, tmpl.GetMetadata().GetName())
	}
	check("ListAllActorTemplates", got, wantNames)

	tags, err := client.ListAllTags(ctx)
	if err != nil {
		t.Fatalf("ListAllTags: %v", err)
	}
	got = nil
	for _, tag := range tags {
		got = append(got, tag.GetMetadata().GetName())
	}
	check("ListAllTags", got, wantNames)

	workers, err := client.ListWorkers(ctx)
	if err != nil {
		t.Fatalf("ListWorkers: %v", err)
	}
	if len(workers) != n {
		t.Errorf("ListWorkers returned %d workers, want %d", len(workers), n)
	}

	// An unknown token is the caller's error, as on Substrate.
	if _, err := control.ListActors(ctx, &ateapipb.ListActorsRequest{Options: &ateapipb.ListOptions{PageToken: "nope"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("ListActors with an unknown page token = %v, want InvalidArgument", err)
	}
}
