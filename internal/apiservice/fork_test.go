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
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/agent-substrate/env/internal/apiservice"
	"github.com/agent-substrate/env/internal/internaltest/fakecontrol"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestForkEnvironment checks AE-11: the copies are created from one tag of
// the source's snapshot, under the source's template and selector, after a
// local suspend of a running source; the tag stays.
func TestForkEnvironment(t *testing.T) {
	client, control := newTestEnv(t)
	control.RequireSourceTag = true
	ctx := context.Background()
	selector := map[string]string{"pool": "a"}
	if _, err := client.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{
		Id: "src", Atespace: "fork-space", Template: &ateenvv1alpha.Template{Name: "tmpl"}, WorkerSelector: selector,
	}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}

	resp, err := client.ForkEnvironment(ctx, &ateenvv1alpha.ForkEnvironmentRequest{
		Id: "src", Atespace: "fork-space", Count: 3, IdPrefix: "branch",
	})
	if err != nil {
		t.Fatalf("ForkEnvironment: %v", err)
	}
	var got []string
	for _, e := range resp.GetEnvironments() {
		got = append(got, e.GetId())
		if e.GetAtespace() != "fork-space" || e.GetTemplate().GetName() != "tmpl" || !maps.Equal(e.GetWorkerSelector(), selector) {
			t.Errorf("copy %s = %v, want atespace fork-space, template tmpl, selector %v", e.GetId(), e, selector)
		}
	}
	if want := []string{"branch-0", "branch-1", "branch-2"}; !slices.Equal(got, want) {
		t.Errorf("copies = %v, want %v", got, want)
	}
	if got := control.SuspendModes; len(got) != 1 || got[0] != ateapipb.DurabilityMode_DURABILITY_MODE_ASYNCHRONOUS {
		t.Errorf("suspend modes = %v, want one ASYNCHRONOUS (local)", got)
	}
	if got := control.TagNames(); len(got) != 1 || got[0] != resp.GetSourceTag() {
		t.Errorf("tags = %v, want only %q", got, resp.GetSourceTag())
	}
	for _, a := range control.Created() {
		if !strings.HasPrefix(a.GetMetadata().GetName(), "branch-") {
			continue
		}
		if ref := a.GetSourceTag(); ref.GetName() != resp.GetSourceTag() || ref.GetAtespace() != "fork-space" {
			t.Errorf("copy %s source_tag = %v, want fork-space/%s", a.GetMetadata().GetName(), ref, resp.GetSourceTag())
		}
	}

	// A second fork of the now suspended source suspends nothing new and
	// gets its own tag and default ids.
	resp2, err := client.ForkEnvironment(ctx, &ateenvv1alpha.ForkEnvironmentRequest{
		Id: "src", Atespace: "fork-space", Count: 2, WorkerSelector: map[string]string{"pool": "b"},
	})
	if err != nil {
		t.Fatalf("second ForkEnvironment: %v", err)
	}
	if resp2.GetSourceTag() == resp.GetSourceTag() {
		t.Errorf("second fork reused tag %q", resp.GetSourceTag())
	}
	for _, e := range resp2.GetEnvironments() {
		if !strings.HasPrefix(e.GetId(), "src-") || e.GetWorkerSelector()["pool"] != "b" {
			t.Errorf("copy %s selector %v, want id src-<hex>-<i> and pool b", e.GetId(), e.GetWorkerSelector())
		}
	}
}

// TestForkEnvironmentPartialFailure checks AE-12: one copy that fails
// deletes the copies created and the tag, and returns the error.
func TestForkEnvironmentPartialFailure(t *testing.T) {
	client, control := newTestEnv(t)
	ctx := context.Background()
	if _, err := client.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "src"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	control.FailCreates(1, codes.ResourceExhausted, "quota")

	_, err := client.ForkEnvironment(ctx, &ateenvv1alpha.ForkEnvironmentRequest{Id: "src", Count: 4, IdPrefix: "b"})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("ForkEnvironment = %v, want ResourceExhausted", err)
	}
	if got := control.ActorNames(); !slices.Equal(got, []string{"src"}) {
		t.Errorf("actors after a failed fork = %v, want only src", got)
	}
	if got := control.Deletes(); got != 3 {
		t.Errorf("deletes = %d, want the 3 copies created", got)
	}
	if got := control.TagNames(); len(got) != 0 || control.TagDeletes() != 1 {
		t.Errorf("tags = %v after %d deletes, want none after 1", got, control.TagDeletes())
	}
}

// TestForkEnvironmentCleansUpLostReply checks AE-12 for a create that
// landed while its reply was lost: the cleanup deletes that copy too, and
// leaves an id another environment holds alone.
func TestForkEnvironmentCleansUpLostReply(t *testing.T) {
	client, control := newTestEnv(t)
	ctx := context.Background()
	for _, id := range []string{"src", "taken"} {
		if _, err := client.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: id}); err != nil {
			t.Fatalf("CreateEnvironment %s: %v", id, err)
		}
	}
	control.PlanCreates("lost", fakecontrol.CreateOutcome{Code: codes.Unavailable, Msg: "reply lost", Stored: true})

	_, err := client.ForkEnvironment(ctx, &ateenvv1alpha.ForkEnvironmentRequest{Id: "src", Ids: []string{"ok", "lost", "taken"}})
	if err == nil {
		t.Fatal("ForkEnvironment succeeded, want an error")
	}
	got := control.ActorNames()
	slices.Sort(got)
	if want := []string{"src", "taken"}; !slices.Equal(got, want) {
		t.Errorf("actors after a failed fork = %v, want %v", got, want)
	}
	if got := control.TagNames(); len(got) != 0 {
		t.Errorf("tags = %v, want none", got)
	}
}

// TestForkEnvironmentKeepsHolderOfID checks AE-12 for a create that was cut
// off on an id another environment holds: the cleanup leaves that
// environment, which the fork's tag did not seed.
func TestForkEnvironmentKeepsHolderOfID(t *testing.T) {
	client, control := newTestEnv(t)
	ctx := context.Background()
	for _, id := range []string{"src", "taken"} {
		if _, err := client.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: id}); err != nil {
			t.Fatalf("CreateEnvironment %s: %v", id, err)
		}
	}
	control.PlanCreates("taken", fakecontrol.CreateOutcome{Code: codes.Unavailable, Msg: "cut off"})

	_, err := client.ForkEnvironment(ctx, &ateenvv1alpha.ForkEnvironmentRequest{Id: "src", Ids: []string{"ok", "taken"}})
	if err == nil {
		t.Fatal("ForkEnvironment succeeded, want an error")
	}
	got := control.ActorNames()
	slices.Sort(got)
	if want := []string{"src", "taken"}; !slices.Equal(got, want) {
		t.Errorf("actors after a failed fork = %v, want %v", got, want)
	}
	if got := control.TagNames(); len(got) != 0 {
		t.Errorf("tags = %v, want none", got)
	}
}

// TestForkEnvironmentRejects checks AE-11's refusals: bad counts and ids
// are InvalidArgument before any call, a missing source is NotFound, and a
// source with no snapshot to tag fails without a copy.
func TestForkEnvironmentRejects(t *testing.T) {
	client, control := newTestEnv(t)
	ctx := context.Background()
	if _, err := client.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "src"}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	for _, tc := range []struct {
		name string
		req  *ateenvv1alpha.ForkEnvironmentRequest
		want codes.Code
	}{
		{"no id", &ateenvv1alpha.ForkEnvironmentRequest{Count: 1}, codes.InvalidArgument},
		{"zero count", &ateenvv1alpha.ForkEnvironmentRequest{Id: "src"}, codes.InvalidArgument},
		{"count above max", &ateenvv1alpha.ForkEnvironmentRequest{Id: "src", Count: apiservice.MaxForkCount + 1}, codes.InvalidArgument},
		{"ids do not match count", &ateenvv1alpha.ForkEnvironmentRequest{Id: "src", Count: 2, Ids: []string{"a"}}, codes.InvalidArgument},
		{"repeated id", &ateenvv1alpha.ForkEnvironmentRequest{Id: "src", Ids: []string{"a", "a"}}, codes.InvalidArgument},
		{"ids and prefix", &ateenvv1alpha.ForkEnvironmentRequest{Id: "src", Ids: []string{"a"}, IdPrefix: "p"}, codes.InvalidArgument},
		{"prefix too long", &ateenvv1alpha.ForkEnvironmentRequest{Id: "src", Count: 1, IdPrefix: strings.Repeat("p", 62)}, codes.InvalidArgument},
		{"missing source", &ateenvv1alpha.ForkEnvironmentRequest{Id: "nope", Count: 1}, codes.NotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := client.ForkEnvironment(ctx, tc.req); status.Code(err) != tc.want {
				t.Errorf("ForkEnvironment = %v, want %v", err, tc.want)
			}
		})
	}
	if control.Creates() != 1 || len(control.SuspendModes) != 0 {
		t.Errorf("creates = %d, suspends = %v, want only the source's create", control.Creates(), control.SuspendModes)
	}

	// A crashed source cannot be suspended: no tag, no copy.
	control.SetState("src", ateapipb.ActorState_ACTOR_STATE_CRASHED)
	if _, err := client.ForkEnvironment(ctx, &ateenvv1alpha.ForkEnvironmentRequest{Id: "src", Count: 1}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("fork of a crashed source = %v, want FailedPrecondition", err)
	}
	// A tag that fails leaves no copy.
	control.SetState("src", ateapipb.ActorState_ACTOR_STATE_RUNNING)
	control.FailCreateTags(1, codes.Unavailable)
	if _, err := client.ForkEnvironment(ctx, &ateenvv1alpha.ForkEnvironmentRequest{Id: "src", Ids: []string{"x"}}); status.Code(err) != codes.Unavailable {
		t.Errorf("fork with a failing tag = %v, want Unavailable", err)
	}
	if control.Creates() != 1 || len(control.TagNames()) != 0 {
		t.Errorf("creates = %d, tags = %v, want no copy and no tag", control.Creates(), control.TagNames())
	}
}
