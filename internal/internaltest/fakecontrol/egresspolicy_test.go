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

package fakecontrol_test

import (
	"testing"

	"github.com/agent-substrate/env/internal/internaltest/fakecontrol"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestEgressPolicyRequestChecks checks that the fake refuses, with
// InvalidArgument, each request Substrate v1.0.0's request validation
// refuses: a missing actor atespace or name, missing policy metadata, an
// empty or other policy name, and an empty or other policy atespace.
func TestEgressPolicyRequestChecks(t *testing.T) {
	s := fakecontrol.New()
	c := s.Client()
	ctx := t.Context()
	ref := &ateapipb.ObjectRef{Atespace: "ate-env", Name: "a"}
	if _, err := c.CreateActor(ctx, &ateapipb.CreateActorRequest{Actor: &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "ate-env", Name: "a"},
	}}); err != nil {
		t.Fatalf("CreateActor: %v", err)
	}
	valid := &ateapipb.ResourceMetadata{Atespace: "ate-env", Name: "default"}

	for name, req := range map[string]*ateapipb.CreateActorEgressPolicyRequest{
		"no actor":             {EgressPolicy: &ateapipb.EgressPolicy{Metadata: valid}},
		"empty actor atespace": {Actor: &ateapipb.ObjectRef{Name: "a"}, EgressPolicy: &ateapipb.EgressPolicy{Metadata: valid}},
		"empty actor name":     {Actor: &ateapipb.ObjectRef{Atespace: "ate-env"}, EgressPolicy: &ateapipb.EgressPolicy{Metadata: valid}},
		"no policy":            {Actor: ref},
		"no metadata":          {Actor: ref, EgressPolicy: &ateapipb.EgressPolicy{}},
		"empty name":           {Actor: ref, EgressPolicy: &ateapipb.EgressPolicy{Metadata: &ateapipb.ResourceMetadata{Atespace: "ate-env"}}},
		"other name":           {Actor: ref, EgressPolicy: &ateapipb.EgressPolicy{Metadata: &ateapipb.ResourceMetadata{Atespace: "ate-env", Name: "custom"}}},
		"empty atespace":       {Actor: ref, EgressPolicy: &ateapipb.EgressPolicy{Metadata: &ateapipb.ResourceMetadata{Name: "default"}}},
		"other atespace":       {Actor: ref, EgressPolicy: &ateapipb.EgressPolicy{Metadata: &ateapipb.ResourceMetadata{Atespace: "elsewhere", Name: "default"}}},
	} {
		if _, err := c.CreateActorEgressPolicy(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("create with %s: err = %v, want InvalidArgument", name, err)
		}
	}
	if _, err := c.UpdateActorEgressPolicy(ctx, &ateapipb.UpdateActorEgressPolicyRequest{Actor: ref, EgressPolicy: &ateapipb.EgressPolicy{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "ate-env", Uid: "u", Version: 1},
	}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("update with an empty name: err = %v, want InvalidArgument", err)
	}

	for name, get := range map[string]*ateapipb.GetActorEgressPolicyRequest{
		"empty name":           {Actor: ref},
		"other name":           {Actor: ref, Name: "custom"},
		"empty actor atespace": {Actor: &ateapipb.ObjectRef{Name: "a"}, Name: "default"},
		"empty actor name":     {Actor: &ateapipb.ObjectRef{Atespace: "ate-env"}, Name: "default"},
	} {
		if _, err := c.GetActorEgressPolicy(ctx, get); status.Code(err) != codes.InvalidArgument {
			t.Errorf("get with %s: err = %v, want InvalidArgument", name, err)
		}
		del := &ateapipb.DeleteActorEgressPolicyRequest{Actor: get.GetActor(), Name: get.GetName()}
		if _, err := c.DeleteActorEgressPolicy(ctx, del); status.Code(err) != codes.InvalidArgument {
			t.Errorf("delete with %s: err = %v, want InvalidArgument", name, err)
		}
	}
}

// TestEgressPolicyDelete covers DeleteActorEgressPolicy, which ate.Client
// does not call: its uid and version preconditions, the delete_time of the
// policy it returns, and NotFound once the policy is gone.
func TestEgressPolicyDelete(t *testing.T) {
	s := fakecontrol.New()
	c := s.Client()
	ctx := t.Context()
	ref := &ateapipb.ObjectRef{Atespace: "ate-env", Name: "a"}
	if _, err := c.CreateActor(ctx, &ateapipb.CreateActorRequest{Actor: &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "ate-env", Name: "a"},
	}}); err != nil {
		t.Fatalf("CreateActor: %v", err)
	}
	p, err := c.CreateActorEgressPolicy(ctx, &ateapipb.CreateActorEgressPolicyRequest{Actor: ref, EgressPolicy: &ateapipb.EgressPolicy{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "ate-env", Name: "default"},
	}})
	if err != nil {
		t.Fatalf("CreateActorEgressPolicy: %v", err)
	}
	uid, version := p.GetMetadata().GetUid(), p.GetMetadata().GetVersion()
	del := func(opts *ateapipb.DeleteOptions) (*ateapipb.EgressPolicy, error) {
		return c.DeleteActorEgressPolicy(ctx, &ateapipb.DeleteActorEgressPolicyRequest{Actor: ref, Name: "default", Options: opts})
	}

	if _, err := del(&ateapipb.DeleteOptions{Uid: "other"}); status.Code(err) != codes.Aborted {
		t.Errorf("delete with another uid: err = %v, want Aborted", err)
	}
	if _, err := del(&ateapipb.DeleteOptions{Uid: uid, Version: version + 1}); status.Code(err) != codes.Aborted {
		t.Errorf("delete with a stale version: err = %v, want Aborted", err)
	}
	deleted, err := del(&ateapipb.DeleteOptions{Uid: uid, Version: version})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted.GetMetadata().GetDeleteTime() == nil {
		t.Error("deleted policy has no delete_time")
	}
	if _, err := c.GetActorEgressPolicy(ctx, &ateapipb.GetActorEgressPolicyRequest{Actor: ref, Name: "default"}); status.Code(err) != codes.NotFound {
		t.Errorf("get after delete: err = %v, want NotFound", err)
	}
	if _, err := del(nil); status.Code(err) != codes.NotFound {
		t.Errorf("second delete: err = %v, want NotFound", err)
	}
}
