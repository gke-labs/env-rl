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

// TestEgressPolicyDeleteAndMetadataChecks covers the parts of the fake's
// egress policy RPCs that ate.Client does not call: DeleteActorEgressPolicy
// with its uid precondition, and Substrate's checks of the policy metadata.
func TestEgressPolicyDeleteAndMetadataChecks(t *testing.T) {
	s := fakecontrol.New()
	c := s.Client()
	ctx := t.Context()
	ref := &ateapipb.ObjectRef{Atespace: "ate-env", Name: "a"}
	if _, err := c.CreateActor(ctx, &ateapipb.CreateActorRequest{Actor: &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "ate-env", Name: "a"},
	}}); err != nil {
		t.Fatalf("CreateActor: %v", err)
	}

	for name, md := range map[string]*ateapipb.ResourceMetadata{
		"no metadata":    nil,
		"other name":     {Atespace: "ate-env", Name: "custom"},
		"other atespace": {Atespace: "elsewhere"},
	} {
		_, err := c.CreateActorEgressPolicy(ctx, &ateapipb.CreateActorEgressPolicyRequest{Actor: ref, EgressPolicy: &ateapipb.EgressPolicy{Metadata: md}})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("create with %s: err = %v, want InvalidArgument", name, err)
		}
	}

	p, err := c.CreateActorEgressPolicy(ctx, &ateapipb.CreateActorEgressPolicyRequest{Actor: ref, EgressPolicy: &ateapipb.EgressPolicy{Metadata: &ateapipb.ResourceMetadata{}}})
	if err != nil {
		t.Fatalf("CreateActorEgressPolicy: %v", err)
	}
	if _, err := c.DeleteActorEgressPolicy(ctx, &ateapipb.DeleteActorEgressPolicyRequest{Actor: ref, Options: &ateapipb.DeleteOptions{Uid: "other"}}); status.Code(err) != codes.Aborted {
		t.Errorf("delete with another uid: err = %v, want Aborted", err)
	}
	if _, err := c.DeleteActorEgressPolicy(ctx, &ateapipb.DeleteActorEgressPolicyRequest{Actor: ref, Options: &ateapipb.DeleteOptions{Uid: p.GetMetadata().GetUid()}}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := c.GetActorEgressPolicy(ctx, &ateapipb.GetActorEgressPolicyRequest{Actor: ref}); status.Code(err) != codes.NotFound {
		t.Errorf("get after delete: err = %v, want NotFound", err)
	}
	if _, err := c.DeleteActorEgressPolicy(ctx, &ateapipb.DeleteActorEgressPolicyRequest{Actor: ref}); status.Code(err) != codes.NotFound {
		t.Errorf("second delete: err = %v, want NotFound", err)
	}
}
