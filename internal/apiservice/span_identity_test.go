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

package apiservice_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/apiservice"
	"github.com/agent-substrate/env/internal/telemetry"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/durationpb"
	"k8s.io/client-go/kubernetes/fake"
)

// spanTracer records the server spans of a traced telemetryEnv, with the
// global tracer provider set as telemetry.SetupOTel sets it.
type spanTracer struct {
	t   *testing.T
	rec *tracetest.SpanRecorder
}

func newSpanTracer(t *testing.T) *spanTracer {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	otel.SetTextMapPropagator(telemetry.Propagator)
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
	return &spanTracer{t: t, rec: rec}
}

func (st *spanTracer) ended(method string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range st.rec.Ended() {
		if s.Name() == method {
			out = append(out, s)
		}
	}
	return out
}

// call runs fn, which makes one call of method, waits for the call's server
// span to end (the server ends it after the client has its answer) and
// returns the span's ate.* attributes. fn's error is ignored: a failed call
// is stamped too.
func (st *spanTracer) call(method string, fn func() error) map[string]attribute.Value {
	st.t.Helper()
	before := len(st.ended(method))
	_ = fn()
	var spans []sdktrace.ReadOnlySpan
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if spans = st.ended(method); len(spans) > before {
			break
		}
		if time.Now().After(deadline) {
			st.t.Fatalf("no new server span %s after 5 s", method)
		}
	}
	got := map[string]attribute.Value{}
	for _, kv := range spans[len(spans)-1].Attributes() {
		if strings.HasPrefix(string(kv.Key), "ate.") {
			got[string(kv.Key)] = kv.Value
		}
	}
	return got
}

// wantNames checks that got's ate.* attributes are exactly the strings of
// want.
func wantNames(t *testing.T, what string, got map[string]attribute.Value, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if got[k].Type() != attribute.STRING || got[k].AsString() != v {
			t.Errorf("%s: %s = %v, want the string %q", what, k, got[k].Emit(), v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%s: ate.* attributes %v, want exactly %v", what, got, want)
	}
}

// wantActor checks that got carries the actor's full identity: want's
// names, and the actor's uid and its version as an int64, as ateapi stamps
// them.
func wantActor(t *testing.T, what string, got map[string]attribute.Value, want map[string]string, actor *ateapipb.Actor) {
	t.Helper()
	uid, version := got["ate.actor.uid"], got["ate.actor.version"]
	if uid.AsString() == "" || uid.AsString() != actor.GetMetadata().GetUid() {
		t.Errorf("%s: ate.actor.uid = %q, want the actor's %q", what, uid.AsString(), actor.GetMetadata().GetUid())
	}
	if version.Type() != attribute.INT64 || version.AsInt64() != actor.GetMetadata().GetVersion() {
		t.Errorf("%s: ate.actor.version = %v (%v), want the int64 %d", what, version.Emit(), version.Type(), actor.GetMetadata().GetVersion())
	}
	names := map[string]attribute.Value{}
	for k, v := range got {
		if k != "ate.actor.uid" && k != "ate.actor.version" {
			names[k] = v
		}
	}
	wantNames(t, what, names, want)
}

const (
	envService = "ateenv.v1alpha.EnvironmentService/"
	space      = apiservice.DefaultAtespace
	tmpl       = apiservice.DefaultTemplate
)

// FS-16 (docs/plans/sandbox-fleet-site/acceptance-criteria.md): with tracing
// on, each ate-env-api server span of an environment call carries the
// environment's identity under the keys ateapi stamps on its actor RPCs'
// server spans, also when the call fails, and the full identity once the
// actor is read.
func TestServerSpansCarryEnvironmentIdentity(t *testing.T) {
	st := newSpanTracer(t)
	te := newTelemetryEnv(t, true)
	ctx := context.Background()
	ref := map[string]string{"ate.atespace": space, "ate.actor.name": "ident"}
	full := map[string]string{
		"ate.atespace": space, "ate.actor.name": "ident",
		"ate.template.name": tmpl, "ate.template.atespace": space,
	}
	actor := func() *ateapipb.Actor {
		a, err := te.control.GetActor(ctx, &ateapipb.GetActorRequest{Actor: &ateapipb.ObjectRef{Atespace: space, Name: "ident"}})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}

	got := st.call(envService+"CreateEnvironment", func() error {
		_, err := te.envClient.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "ident"})
		return err
	})
	wantActor(t, "CreateEnvironment", got, full, actor())
	got = st.call(envService+"GetEnvironment", func() error {
		_, err := te.envClient.GetEnvironment(ctx, &ateenvv1alpha.GetEnvironmentRequest{Id: "ident"})
		return err
	})
	wantActor(t, "GetEnvironment", got, full, actor())

	got = st.call(envService+"GetEnvironment", func() error {
		_, err := te.envClient.GetEnvironment(ctx, &ateenvv1alpha.GetEnvironmentRequest{Id: "missing", Atespace: "team-a"})
		return err
	})
	wantNames(t, "GetEnvironment (missing)", got, map[string]string{"ate.atespace": "team-a", "ate.actor.name": "missing"})

	// An id that is no resource name, here one over 63 characters, is not
	// stamped: Substrate accepts no such name, so no actor has it.
	got = st.call(envService+"GetEnvironment", func() error {
		_, err := te.envClient.GetEnvironment(ctx, &ateenvv1alpha.GetEnvironmentRequest{Id: strings.Repeat("a", 64)})
		return err
	})
	wantNames(t, "GetEnvironment (long id)", got, map[string]string{"ate.atespace": space})

	got = st.call(envService+"ListEnvironments", func() error {
		_, err := te.envClient.ListEnvironments(ctx, &ateenvv1alpha.ListEnvironmentsRequest{})
		return err
	})
	wantNames(t, "ListEnvironments", got, map[string]string{"ate.atespace": space})

	got = st.call(envService+"GetServerInfo", func() error {
		_, err := te.envClient.GetServerInfo(ctx, &ateenvv1alpha.GetServerInfoRequest{})
		return err
	})
	wantNames(t, "GetServerInfo", got, nil)

	got = st.call(envService+"SuspendEnvironment", func() error {
		_, err := te.envClient.SuspendEnvironment(ctx, &ateenvv1alpha.SuspendEnvironmentRequest{Id: "ident"})
		return err
	})
	wantNames(t, "SuspendEnvironment", got, ref)
	got = st.call(envService+"DeleteEnvironment", func() error {
		_, err := te.envClient.DeleteEnvironment(ctx, &ateenvv1alpha.DeleteEnvironmentRequest{Id: "ident"})
		return err
	})
	wantNames(t, "DeleteEnvironment", got, ref)
}

// FS-16: SetEnvironmentTimeToLive stamps the environment's identity before
// the store check, thus also when the store is off, and the actor's full
// identity once it reads the actor.
func TestServerSpansCarrySetTimeToLiveIdentity(t *testing.T) {
	st := newSpanTracer(t)
	te := newTelemetryEnv(t, true)
	ctx := context.Background()
	if _, err := te.envClient.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "ident"}); err != nil {
		t.Fatal(err)
	}
	set := func(req *ateenvv1alpha.SetEnvironmentTimeToLiveRequest) map[string]attribute.Value {
		return st.call(envService+"SetEnvironmentTimeToLive", func() error {
			_, err := te.envClient.SetEnvironmentTimeToLive(ctx, req)
			return err
		})
	}
	ref := map[string]string{"ate.atespace": space, "ate.actor.name": "ident"}
	wantNames(t, "SetEnvironmentTimeToLive (store off)", set(&ateenvv1alpha.SetEnvironmentTimeToLiveRequest{Id: "ident", TimeToLive: durationpb.New(time.Minute)}), ref)

	te.srv.Store = runStore(t, fake.NewSimpleClientset())
	got := set(&ateenvv1alpha.SetEnvironmentTimeToLiveRequest{Id: "ident", TimeToLive: durationpb.New(time.Minute)})
	a, err := te.control.GetActor(ctx, &ateapipb.GetActorRequest{Actor: &ateapipb.ObjectRef{Atespace: space, Name: "ident"}})
	if err != nil {
		t.Fatal(err)
	}
	wantActor(t, "SetEnvironmentTimeToLive", got, map[string]string{
		"ate.atespace": space, "ate.actor.name": "ident",
		"ate.template.name": tmpl, "ate.template.atespace": space,
	}, a)
	wantNames(t, "SetEnvironmentTimeToLive (missing)",
		set(&ateenvv1alpha.SetEnvironmentTimeToLiveRequest{Id: "missing", Atespace: "team-a", TimeToLive: durationpb.New(time.Minute)}),
		map[string]string{"ate.atespace": "team-a", "ate.actor.name": "missing"})
}

// FS-16: ForkEnvironment stamps the source's identity, also when the source
// does not exist, and the source actor's full identity once it reads it.
func TestServerSpansCarryForkIdentity(t *testing.T) {
	st := newSpanTracer(t)
	te := newTelemetryEnv(t, true)
	ctx := context.Background()
	if _, err := te.envClient.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "ident"}); err != nil {
		t.Fatal(err)
	}
	a, err := te.control.GetActor(ctx, &ateapipb.GetActorRequest{Actor: &ateapipb.ObjectRef{Atespace: space, Name: "ident"}})
	if err != nil {
		t.Fatal(err)
	}
	fork := func(req *ateenvv1alpha.ForkEnvironmentRequest) map[string]attribute.Value {
		return st.call(envService+"ForkEnvironment", func() error {
			_, err := te.envClient.ForkEnvironment(ctx, req)
			return err
		})
	}
	wantActor(t, "ForkEnvironment", fork(&ateenvv1alpha.ForkEnvironmentRequest{Id: "ident", Count: 1}), map[string]string{
		"ate.atespace": space, "ate.actor.name": "ident",
		"ate.template.name": tmpl, "ate.template.atespace": space,
	}, a)
	wantNames(t, "ForkEnvironment (missing)",
		fork(&ateenvv1alpha.ForkEnvironmentRequest{Id: "missing", Atespace: "team-a", Count: 1}),
		map[string]string{"ate.atespace": "team-a", "ate.actor.name": "missing"})
}

// FS-16: each of the eight guest calls carries the environment its x-env-id
// and x-env-atespace metadata name, also when the environment does not
// exist.
func TestServerSpansCarryGuestCallIdentity(t *testing.T) {
	st := newSpanTracer(t)
	te := newTelemetryEnv(t, true)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "x-env-id", "nowhere", "x-env-atespace", "team-b")
	want := map[string]string{"ate.atespace": "team-b", "ate.actor.name": "nowhere"}
	drain := func(recv func() error) error {
		for {
			if err := recv(); err != nil {
				return err
			}
		}
	}
	for method, fn := range map[string]func() error{
		"ateenv.v1alpha.ProcessService/StartProcess": func() error {
			_, err := te.procClient.StartProcess(ctx, &ateenvv1alpha.StartProcessRequest{Command: []string{"true"}})
			return err
		},
		"ateenv.v1alpha.ProcessService/GetProcess": func() error {
			_, err := te.procClient.GetProcess(ctx, &ateenvv1alpha.GetProcessRequest{ProcessId: "p1"})
			return err
		},
		"ateenv.v1alpha.ProcessService/SignalProcess": func() error {
			_, err := te.procClient.SignalProcess(ctx, &ateenvv1alpha.SignalProcessRequest{ProcessId: "p1"})
			return err
		},
		"ateenv.v1alpha.ProcessService/ResizeProcess": func() error {
			_, err := te.procClient.ResizeProcess(ctx, &ateenvv1alpha.ResizeProcessRequest{ProcessId: "p1"})
			return err
		},
		"ateenv.v1alpha.ProcessService/StreamProcessOutput": func() error {
			s, err := te.procClient.StreamProcessOutput(ctx, &ateenvv1alpha.StreamProcessOutputRequest{ProcessId: "p1"})
			if err != nil {
				return err
			}
			return drain(func() error { _, err := s.Recv(); return err })
		},
		"ateenv.v1alpha.ProcessService/WriteProcessInput": func() error {
			s, err := te.procClient.WriteProcessInput(ctx)
			if err != nil {
				return err
			}
			_, err = s.CloseAndRecv()
			return err
		},
		"ateenv.v1alpha.FileSystemService/ReadFile": func() error {
			s, err := te.fsClient.ReadFile(ctx, &ateenvv1alpha.ReadFileRequest{Path: "/f"})
			if err != nil {
				return err
			}
			return drain(func() error { _, err := s.Recv(); return err })
		},
		"ateenv.v1alpha.FileSystemService/WriteFile": func() error {
			s, err := te.fsClient.WriteFile(ctx)
			if err != nil {
				return err
			}
			_, err = s.CloseAndRecv()
			return err
		},
	} {
		wantNames(t, method, st.call(method, fn), want)
	}

	// A WriteProcessInput whose first Recv fails, here because the client
	// cancels the stream before it sends anything, still carries the
	// environment: the identity goes on the span before that Recv. (Above,
	// CloseAndRecv ends the stream with io.EOF, which is not an error.)
	got := st.call("ateenv.v1alpha.ProcessService/WriteProcessInput", func() error {
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		s, err := te.procClient.WriteProcessInput(cctx)
		if err != nil {
			return err
		}
		cancel()
		_, err = s.CloseAndRecv()
		return err
	})
	wantNames(t, "WriteProcessInput (first Recv fails)", got, want)
}

// FS-16: each template call carries the template it targets, the base
// template first and the derived template once its name is known, and a
// ListPlacementDomains that names no template carries none.
func TestServerSpansCarryTemplateIdentity(t *testing.T) {
	st := newSpanTracer(t)
	te := newTelemetryEnv(t, true)
	ctx := context.Background()
	seedGuestTemplate(te.control, space, tmpl)
	base := map[string]string{"ate.atespace": space, "ate.template.name": tmpl, "ate.template.atespace": space}
	derived := map[string]string{"ate.atespace": space, "ate.template.name": tmpl + "-0123456789ab", "ate.template.atespace": space}

	got := st.call(envService+"GetImageStatus", func() error {
		_, err := te.envClient.GetImageStatus(ctx, &ateenvv1alpha.GetImageStatusRequest{RootfsImage: testRootfsImage})
		return err
	})
	wantNames(t, "GetImageStatus", got, derived)
	// An image that is not pinned has no derived template: the base stays.
	got = st.call(envService+"GetImageStatus", func() error {
		_, err := te.envClient.GetImageStatus(ctx, &ateenvv1alpha.GetImageStatusRequest{RootfsImage: "python:3"})
		return err
	})
	wantNames(t, "GetImageStatus (unpinned)", got, base)
	got = st.call(envService+"EnsureImageTemplate", func() error {
		_, err := te.envClient.EnsureImageTemplate(ctx, &ateenvv1alpha.EnsureImageTemplateRequest{RootfsImage: testRootfsImage})
		return err
	})
	wantNames(t, "EnsureImageTemplate", got, derived)
	got = st.call(envService+"EnsureImageTemplate", func() error {
		_, err := te.envClient.EnsureImageTemplate(ctx, &ateenvv1alpha.EnsureImageTemplateRequest{RootfsImage: "python:3"})
		return err
	})
	wantNames(t, "EnsureImageTemplate (unpinned)", got, base)
	got = st.call(envService+"WarmImage", func() error {
		s, err := te.envClient.WarmImage(ctx, &ateenvv1alpha.WarmImageRequest{RootfsImage: testRootfsImage})
		if err != nil {
			return err
		}
		for {
			if _, err := s.Recv(); err != nil {
				return err
			}
		}
	})
	wantNames(t, "WarmImage", got, derived)
	got = st.call(envService+"ListPlacementDomains", func() error {
		_, err := te.envClient.ListPlacementDomains(ctx, &ateenvv1alpha.ListPlacementDomainsRequest{Template: &ateenvv1alpha.Template{Name: tmpl}})
		return err
	})
	wantNames(t, "ListPlacementDomains (template)", got, base)
	got = st.call(envService+"ListPlacementDomains", func() error {
		_, err := te.envClient.ListPlacementDomains(ctx, &ateenvv1alpha.ListPlacementDomainsRequest{Atespace: space})
		return err
	})
	wantNames(t, "ListPlacementDomains (atespace)", got, base)
	got = st.call(envService+"ListPlacementDomains", func() error {
		_, err := te.envClient.ListPlacementDomains(ctx, &ateenvv1alpha.ListPlacementDomainsRequest{})
		return err
	})
	wantNames(t, "ListPlacementDomains (all)", got, nil)

	// A create whose image template cannot be derived keeps the base.
	got = st.call(envService+"CreateEnvironment", func() error {
		_, err := te.envClient.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "bad-image", RootfsImage: "python:3"})
		return err
	})
	wantNames(t, "CreateEnvironment (unpinned)", got, map[string]string{
		"ate.atespace": space, "ate.actor.name": "bad-image", "ate.template.name": tmpl, "ate.template.atespace": space,
	})

	// A create that fails after the derivation, here on an id that exists,
	// carries the derived template.
	if _, err := te.envClient.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "dup", RootfsImage: testRootfsImage}); err != nil {
		t.Fatal(err)
	}
	got = st.call(envService+"CreateEnvironment", func() error {
		_, err := te.envClient.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "dup", RootfsImage: testRootfsImage})
		if err == nil {
			t.Error("a second create of dup succeeded")
		}
		return err
	})
	wantNames(t, "CreateEnvironment (duplicate)", got, map[string]string{
		"ate.atespace": space, "ate.actor.name": "dup", "ate.template.name": tmpl + "-0123456789ab", "ate.template.atespace": space,
	})

	// A call whose image template fails after its name is derived, here
	// because the base template is not registered, carries the derived
	// template, as GetImageStatus does.
	nobase := &ateenvv1alpha.Template{Name: "nobase"}
	missing := map[string]string{"ate.atespace": space, "ate.template.name": "nobase-0123456789ab", "ate.template.atespace": space}
	failed := func(rpc string) func(error) error {
		return func(err error) error {
			if err == nil {
				t.Errorf("%s on an unregistered base template succeeded", rpc)
			}
			return err
		}
	}
	got = st.call(envService+"EnsureImageTemplate", func() error {
		_, err := te.envClient.EnsureImageTemplate(ctx, &ateenvv1alpha.EnsureImageTemplateRequest{Template: nobase, RootfsImage: testRootfsImage})
		return failed("EnsureImageTemplate")(err)
	})
	wantNames(t, "EnsureImageTemplate (no base)", got, missing)
	got = st.call(envService+"WarmImage", func() error {
		s, err := te.envClient.WarmImage(ctx, &ateenvv1alpha.WarmImageRequest{Template: nobase, RootfsImage: testRootfsImage})
		if err != nil {
			return failed("WarmImage")(err)
		}
		for {
			if _, err := s.Recv(); err != nil {
				if err == io.EOF {
					err = nil
				}
				return failed("WarmImage")(err)
			}
		}
	})
	wantNames(t, "WarmImage (no base)", got, missing)
	got = st.call(envService+"CreateEnvironment", func() error {
		_, err := te.envClient.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "nobase-env", Template: nobase, RootfsImage: testRootfsImage})
		return failed("CreateEnvironment")(err)
	})
	wantNames(t, "CreateEnvironment (no base)", got, map[string]string{
		"ate.atespace": space, "ate.actor.name": "nobase-env", "ate.template.name": "nobase-0123456789ab", "ate.template.atespace": space,
	})
}

// FS-16: no attribute of a server span carries the bearer token a caller
// sends. Nothing stamps metadata today but x-env-id and x-env-atespace;
// this guards against a stamp of the authorization header.
func TestServerSpansCarryNoToken(t *testing.T) {
	st := newSpanTracer(t)
	te := newTelemetryEnv(t, true)
	const token = "s3cr3t-token-value"
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
	st.call(envService+"CreateEnvironment", func() error {
		_, err := te.envClient.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "tok"})
		return err
	})
	guestCtx := metadata.AppendToOutgoingContext(serveGuest(t, te, "tok", nil), "authorization", "Bearer "+token)
	st.call("ateenv.v1alpha.ProcessService/StartProcess", func() error {
		_, err := te.procClient.StartProcess(guestCtx, &ateenvv1alpha.StartProcessRequest{Command: []string{"true"}})
		return err
	})
	for _, s := range st.rec.Ended() {
		for _, kv := range s.Attributes() {
			if strings.Contains(kv.Value.Emit(), token) {
				t.Errorf("span %s attribute %s carries the bearer token", s.Name(), kv.Key)
			}
		}
		if strings.Contains(s.Status().Description, token) {
			t.Errorf("span %s status carries the bearer token", s.Name())
		}
		for _, e := range s.Events() {
			for _, kv := range e.Attributes {
				if strings.Contains(kv.Value.Emit(), token) {
					t.Errorf("span %s event %s carries the bearer token", s.Name(), e.Name)
				}
			}
		}
	}
}
