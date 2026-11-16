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
	"fmt"
	"slices"
	"testing"

	"github.com/agent-substrate/env/internal/apiservice"
	"github.com/agent-substrate/env/internal/internaltest/fakecontrol"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// seedActors creates n actors named <prefix>-0000.. in atespace on the fake
// control plane and returns their names in the fake's list order.
func seedActors(t *testing.T, control *fakecontrol.Server, atespace, prefix string, n int) []string {
	t.Helper()
	names := make([]string, 0, n)
	for i := range n {
		name := fmt.Sprintf("%s-%04d", prefix, i)
		if _, err := control.CreateActor(context.Background(), &ateapipb.CreateActorRequest{Actor: &ateapipb.Actor{
			Metadata:      &ateapipb.ResourceMetadata{Atespace: atespace, Name: name},
			ActorTemplate: &ateapipb.ObjectRef{Atespace: atespace, Name: "tmpl"},
		}}); err != nil {
			t.Fatalf("CreateActor(%s): %v", name, err)
		}
		names = append(names, name)
	}
	return names
}

func envIDs(envs []*ateenvv1alpha.Environment) []string {
	ids := make([]string, 0, len(envs))
	for _, e := range envs {
		ids = append(ids, e.GetId())
	}
	return ids
}

// TestListEnvironmentsReturnsOneAtespace checks AE-6: the list holds the
// environments of the request's atespace only, the default one when the
// request names none, each as GetEnvironment reports it.
func TestListEnvironmentsReturnsOneAtespace(t *testing.T) {
	client, control := newTestEnv(t)
	ctx := context.Background()

	for _, req := range []*ateenvv1alpha.CreateEnvironmentRequest{
		{Id: "a-1", WorkerSelector: map[string]string{"ate-env/pool": "p1"}},
		{Id: "a-2"},
		{Id: "b-1", Atespace: "other"},
	} {
		if _, err := client.CreateEnvironment(ctx, req); err != nil {
			t.Fatalf("CreateEnvironment(%s): %v", req.GetId(), err)
		}
	}
	control.SetState("a-2", ateapipb.ActorState_ACTOR_STATE_SUSPENDED)

	for _, tc := range []struct {
		atespace string
		want     []string
	}{
		{"", []string{"a-1", "a-2"}},
		{apiservice.DefaultAtespace, []string{"a-1", "a-2"}},
		{"other", []string{"b-1"}},
	} {
		resp, err := client.ListEnvironments(ctx, &ateenvv1alpha.ListEnvironmentsRequest{Atespace: tc.atespace})
		if err != nil {
			t.Fatalf("ListEnvironments(%q): %v", tc.atespace, err)
		}
		if got := envIDs(resp.GetEnvironments()); !slices.Equal(got, tc.want) {
			t.Errorf("ListEnvironments(%q) = %v, want %v", tc.atespace, got, tc.want)
		}
		if resp.GetNextPageToken() != "" {
			t.Errorf("ListEnvironments(%q) next_page_token = %q, want none", tc.atespace, resp.GetNextPageToken())
		}
		for _, listed := range resp.GetEnvironments() {
			got, err := client.GetEnvironment(ctx, &ateenvv1alpha.GetEnvironmentRequest{Id: listed.GetId(), Atespace: listed.GetAtespace()})
			if err != nil {
				t.Fatalf("GetEnvironment(%s): %v", listed.GetId(), err)
			}
			if !proto.Equal(listed, got.GetEnvironment()) {
				t.Errorf("listed %s = %v, GetEnvironment reports %v", listed.GetId(), listed, got.GetEnvironment())
			}
		}
	}

	if _, err := control.CreateAtespace(ctx, &ateapipb.CreateAtespaceRequest{
		Atespace: &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: "empty"}},
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := client.ListEnvironments(ctx, &ateenvv1alpha.ListEnvironmentsRequest{Atespace: "empty"})
	if err != nil {
		t.Fatalf("ListEnvironments(empty): %v", err)
	}
	if len(resp.GetEnvironments()) != 0 || resp.GetNextPageToken() != "" {
		t.Errorf("ListEnvironments(empty) = %v, token %q, want no environments and no token", envIDs(resp.GetEnvironments()), resp.GetNextPageToken())
	}

	// An atespace that nobody created is not an error for a list, as in
	// Substrate, whose scoped list does not check the atespace. A get in it
	// is NotFound.
	resp, err = client.ListEnvironments(ctx, &ateenvv1alpha.ListEnvironmentsRequest{Atespace: "never-created"})
	if err != nil {
		t.Fatalf("ListEnvironments(never-created): %v", err)
	}
	if len(resp.GetEnvironments()) != 0 || resp.GetNextPageToken() != "" {
		t.Errorf("ListEnvironments(never-created) = %v, token %q, want no environments and no token", envIDs(resp.GetEnvironments()), resp.GetNextPageToken())
	}
	if _, err := client.GetEnvironment(ctx, &ateenvv1alpha.GetEnvironmentRequest{Id: "a", Atespace: "never-created"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetEnvironment in never-created = %v, want NotFound", err)
	}
}

// TestListEnvironmentsPageSize checks AE-7: page_size 0 reads the default, a
// value above the cap reads the cap and is not refused, and a negative value
// is InvalidArgument.
func TestListEnvironmentsPageSize(t *testing.T) {
	client, control := newTestEnv(t)
	ctx := context.Background()
	seedActors(t, control, apiservice.DefaultAtespace, "env", apiservice.MaxListPageSize+1)
	// The fake control plane serves pages above the server's cap, thus a
	// page of the cap's size proves that the server lowered the value, and
	// not Substrate.
	control.MaxPageSize = 2 * apiservice.MaxListPageSize

	for _, tc := range []struct {
		name      string
		size      int32
		want      int
		wantToken bool
	}{
		{"unset reads the default", 0, apiservice.DefaultListPageSize, true},
		{"a size below the default", 7, 7, true},
		{"the cap", apiservice.MaxListPageSize, apiservice.MaxListPageSize, true},
		{"above the cap is lowered", apiservice.MaxListPageSize + 500, apiservice.MaxListPageSize, true},
	} {
		resp, err := client.ListEnvironments(ctx, &ateenvv1alpha.ListEnvironmentsRequest{PageSize: tc.size})
		if err != nil {
			t.Fatalf("%s: ListEnvironments(page_size=%d): %v", tc.name, tc.size, err)
		}
		if got := len(resp.GetEnvironments()); got != tc.want {
			t.Errorf("%s: page_size=%d returned %d environments, want %d", tc.name, tc.size, got, tc.want)
		}
		if (resp.GetNextPageToken() != "") != tc.wantToken {
			t.Errorf("%s: next_page_token = %q, want set: %v", tc.name, resp.GetNextPageToken(), tc.wantToken)
		}
	}

	calls := control.ListCalls()
	_, err := client.ListEnvironments(ctx, &ateenvv1alpha.ListEnvironmentsRequest{PageSize: -1})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("page_size=-1: got %v, want InvalidArgument", err)
	}
	if got := control.ListCalls(); got != calls {
		t.Errorf("page_size=-1 made %d ListActors calls, want none", got-calls)
	}
}

// TestListEnvironmentsFollowsSubstrateTokens checks AE-8: the page token is
// Substrate's in both directions, following it reads each environment once,
// and a token Substrate refuses is InvalidArgument.
func TestListEnvironmentsFollowsSubstrateTokens(t *testing.T) {
	client, control := newTestEnv(t)
	ctx := context.Background()
	want := seedActors(t, control, "paged", "env", 7)
	seedActors(t, control, "elsewhere", "x", 3)

	var got, tokens []string
	token := ""
	for {
		resp, err := client.ListEnvironments(ctx, &ateenvv1alpha.ListEnvironmentsRequest{Atespace: "paged", PageSize: 3, PageToken: token})
		if err != nil {
			t.Fatalf("ListEnvironments(token=%q): %v", token, err)
		}
		// The same request sent to Substrate returns the same token.
		direct, err := control.ListActors(ctx, &ateapipb.ListActorsRequest{
			Atespace: "paged",
			Options:  &ateapipb.ListOptions{PageSize: 3, PageToken: token},
		})
		if err != nil {
			t.Fatalf("ListActors(token=%q): %v", token, err)
		}
		if resp.GetNextPageToken() != direct.GetNextPageToken() {
			t.Errorf("next_page_token after %q = %q, Substrate's is %q", token, resp.GetNextPageToken(), direct.GetNextPageToken())
		}
		got = append(got, envIDs(resp.GetEnvironments())...)
		token = resp.GetNextPageToken()
		if token == "" {
			break
		}
		tokens = append(tokens, token)
	}
	if !slices.Equal(got, want) {
		t.Errorf("following the tokens read %v, want %v", got, want)
	}
	if len(tokens) != 2 {
		t.Errorf("7 environments in pages of 3 gave %d tokens, want 2", len(tokens))
	}

	_, err := client.ListEnvironments(ctx, &ateenvv1alpha.ListEnvironmentsRequest{Atespace: "paged", PageToken: "not-a-token"})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("a token Substrate refuses: got %v, want InvalidArgument", err)
	}
}

// TestListEnvironmentsStatusFilter checks AE-9: the filter keeps the
// environments of the page read whose status is in the set and leaves the
// token alone, thus a page can be short or empty with a token; PAUSED
// selects what GetEnvironment reports as paused; a value outside the enum is
// InvalidArgument before any read.
func TestListEnvironmentsStatusFilter(t *testing.T) {
	client, control := newTestEnv(t)
	ctx := context.Background()
	const (
		running   = ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_RUNNING
		suspended = ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_SUSPENDED
		paused    = ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_PAUSED
		crashed   = ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_CRASHED
	)
	// In list order: two running, then suspended, paused, running, crashed.
	for _, id := range []string{"e-0", "e-1", "e-2", "e-3", "e-4", "e-5"} {
		if _, err := client.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: id}); err != nil {
			t.Fatalf("CreateEnvironment(%s): %v", id, err)
		}
	}
	if _, err := client.SuspendEnvironment(ctx, &ateenvv1alpha.SuspendEnvironmentRequest{Id: "e-2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SuspendEnvironment(ctx, &ateenvv1alpha.SuspendEnvironmentRequest{Id: "e-3", Mode: ateenvv1alpha.SuspendMode_SUSPEND_MODE_LOCAL}); err != nil {
		t.Fatal(err)
	}
	control.Crash("e-5", "boom")
	got, err := client.GetEnvironment(ctx, &ateenvv1alpha.GetEnvironmentRequest{Id: "e-3"})
	if err != nil || got.GetEnvironment().GetStatus() != paused {
		t.Fatalf("GetEnvironment(e-3) = %v, %v, want PAUSED", got.GetEnvironment().GetStatus(), err)
	}

	list := func(size int32, token string, statuses ...ateenvv1alpha.EnvironmentStatus) *ateenvv1alpha.ListEnvironmentsResponse {
		t.Helper()
		resp, err := client.ListEnvironments(ctx, &ateenvv1alpha.ListEnvironmentsRequest{PageSize: size, PageToken: token, Statuses: statuses})
		if err != nil {
			t.Fatalf("ListEnvironments(size=%d, token=%q, %v): %v", size, token, statuses, err)
		}
		return resp
	}

	for _, tc := range []struct {
		statuses []ateenvv1alpha.EnvironmentStatus
		want     []string
	}{
		{nil, []string{"e-0", "e-1", "e-2", "e-3", "e-4", "e-5"}},
		{[]ateenvv1alpha.EnvironmentStatus{running}, []string{"e-0", "e-1", "e-4"}},
		{[]ateenvv1alpha.EnvironmentStatus{suspended}, []string{"e-2"}},
		{[]ateenvv1alpha.EnvironmentStatus{paused}, []string{"e-3"}},
		{[]ateenvv1alpha.EnvironmentStatus{suspended, paused, crashed}, []string{"e-2", "e-3", "e-5"}},
		{[]ateenvv1alpha.EnvironmentStatus{ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_DELETING}, nil},
		// UNSPECIFIED is a status like the others, not "all".
		{[]ateenvv1alpha.EnvironmentStatus{ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_UNSPECIFIED}, nil},
	} {
		resp := list(0, "", tc.statuses...)
		if ids := envIDs(resp.GetEnvironments()); !slices.Equal(ids, tc.want) {
			t.Errorf("statuses %v: got %v, want %v", tc.statuses, ids, tc.want)
		}
	}

	// Pages of two actors, filtered to SUSPENDED: the first page is empty
	// with a token, the second holds one environment with a token, and the
	// tokens are the unfiltered list's.
	unfiltered := list(2, "")
	first := list(2, "", suspended)
	if len(first.GetEnvironments()) != 0 || first.GetNextPageToken() == "" {
		t.Fatalf("first filtered page = %v, token %q, want no environments and a token", envIDs(first.GetEnvironments()), first.GetNextPageToken())
	}
	if first.GetNextPageToken() != unfiltered.GetNextPageToken() {
		t.Errorf("filtered token %q differs from the unfiltered token %q", first.GetNextPageToken(), unfiltered.GetNextPageToken())
	}
	second := list(2, first.GetNextPageToken(), suspended)
	if ids := envIDs(second.GetEnvironments()); !slices.Equal(ids, []string{"e-2"}) || second.GetNextPageToken() == "" {
		t.Errorf("second filtered page = %v, token %q, want [e-2] and a token", ids, second.GetNextPageToken())
	}
	third := list(2, second.GetNextPageToken(), suspended)
	if len(third.GetEnvironments()) != 0 || third.GetNextPageToken() != "" {
		t.Errorf("last filtered page = %v, token %q, want no environments and no token", envIDs(third.GetEnvironments()), third.GetNextPageToken())
	}

	calls := control.ListCalls()
	_, err = client.ListEnvironments(ctx, &ateenvv1alpha.ListEnvironmentsRequest{
		Statuses: []ateenvv1alpha.EnvironmentStatus{running, ateenvv1alpha.EnvironmentStatus(99)},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("status 99: got %v, want InvalidArgument", err)
	}
	if n := control.ListCalls(); n != calls {
		t.Errorf("status 99 made %d ListActors calls, want none", n-calls)
	}

	// An actor in a state the server cannot name is UNSPECIFIED. The
	// UNSPECIFIED filter keeps it and nothing else, and another filter does
	// not keep it.
	if _, err := client.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "e-6"}); err != nil {
		t.Fatalf("CreateEnvironment(e-6): %v", err)
	}
	control.SetState("e-6", ateapipb.ActorState(99))
	unspecified := ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_UNSPECIFIED
	if ids := envIDs(list(0, "", unspecified).GetEnvironments()); !slices.Equal(ids, []string{"e-6"}) {
		t.Errorf("statuses [UNSPECIFIED] with an unnamed state: got %v, want [e-6]", ids)
	}
	if ids := envIDs(list(0, "", running).GetEnvironments()); !slices.Equal(ids, []string{"e-0", "e-1", "e-4"}) {
		t.Errorf("statuses [RUNNING] with an unnamed state: got %v, want [e-0 e-1 e-4]", ids)
	}
}

// TestEnvironmentCreateTime checks AE-10: create_time is the actor's
// metadata.create_time on CreateEnvironment, GetEnvironment and
// ListEnvironments.
func TestEnvironmentCreateTime(t *testing.T) {
	client, control := newTestEnv(t)
	ctx := context.Background()

	created, err := client.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "timed"})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	var stored *timestamppb.Timestamp
	control.MutateActor("timed", func(a *ateapipb.Actor) {
		stored = proto.Clone(a.GetMetadata().GetCreateTime()).(*timestamppb.Timestamp)
	})
	if stored == nil {
		t.Fatal("the fake control plane stored no create_time")
	}
	if got := created.GetEnvironment().GetCreateTime(); !proto.Equal(got, stored) {
		t.Errorf("CreateEnvironment create_time = %v, the actor's is %v", got, stored)
	}

	// A time the wall clock cannot produce shows the value is read from the
	// actor on each call.
	at := &timestamppb.Timestamp{Seconds: 1794441600, Nanos: 5} // 2026-11-12T00:00:00.000000005Z
	control.MutateActor("timed", func(a *ateapipb.Actor) { a.Metadata.CreateTime = at })
	got, err := client.GetEnvironment(ctx, &ateenvv1alpha.GetEnvironmentRequest{Id: "timed"})
	if err != nil {
		t.Fatalf("GetEnvironment: %v", err)
	}
	if ct := got.GetEnvironment().GetCreateTime(); !proto.Equal(ct, at) {
		t.Errorf("GetEnvironment create_time = %v, want %v", ct, at)
	}
	listed, err := client.ListEnvironments(ctx, &ateenvv1alpha.ListEnvironmentsRequest{})
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if envs := listed.GetEnvironments(); len(envs) != 1 || !proto.Equal(envs[0].GetCreateTime(), at) {
		t.Errorf("ListEnvironments = %v, want one environment with create_time %v", envs, at)
	}
}
