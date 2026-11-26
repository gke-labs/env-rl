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

package fakecontrol

import (
	"context"
	"strconv"

	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// egressPolicyName is the name of an actor's egress policy: Substrate's
// EgressPolicy is a singleton subresource of its actor.
const egressPolicyName = "default"

// The egress policy RPCs mirror Substrate v1.0.0
// (cmd/ateapi/internal/controlapi/egress_policy.go): a create under a
// missing actor is FailedPrecondition and a second create AlreadyExists; a
// get, update or delete of a missing policy is NotFound; an update needs the
// policy's uid and version (InvalidArgument without them, Aborted when either
// is stale) and replaces the rules; the port defaults (80 for http, 443 for
// https) are applied on each write; a delete honors the uid and version of
// DeleteOptions and returns the policy with its delete_time set; DeleteActor
// deletes the actor's policy. Like Substrate's request validation, every call
// needs the actor's atespace and name, and the policy name "default"; a write
// needs metadata whose atespace is the actor's. The fake does not validate
// rules beyond that.

func (s *Server) CreateActorEgressPolicy(ctx context.Context, req *ateapipb.CreateActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref := req.GetActor()
	in := req.GetEgressPolicy()
	if err := checkEgressPolicyWrite(ref, in); err != nil {
		return nil, err
	}
	k := key(ref.GetAtespace(), ref.GetName())
	if _, ok := s.actors[k]; !ok {
		return nil, status.Error(codes.FailedPrecondition, "parent Actor does not exist")
	}
	if _, ok := s.egressPolicies[k]; ok {
		return nil, status.Error(codes.AlreadyExists, "EgressPolicy already exists")
	}
	s.egressSeq++
	now := timestamppb.Now()
	p := &ateapipb.EgressPolicy{
		Metadata: &ateapipb.ResourceMetadata{
			Atespace:   ref.GetAtespace(),
			Name:       egressPolicyName,
			Uid:        "egress-policy-" + strconv.Itoa(s.egressSeq),
			Version:    1,
			CreateTime: now,
			UpdateTime: now,
		},
		Rules: cloneRules(in.GetRules()),
	}
	applyEgressPolicyDefaults(p)
	if s.egressPolicies == nil {
		s.egressPolicies = make(map[string]*ateapipb.EgressPolicy)
	}
	s.egressPolicies[k] = p
	return proto.Clone(p).(*ateapipb.EgressPolicy), nil
}

func (s *Server) GetActorEgressPolicy(ctx context.Context, req *ateapipb.GetActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref := req.GetActor()
	if err := checkEgressPolicyRef(ref, req.GetName()); err != nil {
		return nil, err
	}
	p, ok := s.egressPolicies[key(ref.GetAtespace(), ref.GetName())]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "EgressPolicy for actor %s/%s not found", ref.GetAtespace(), ref.GetName())
	}
	return proto.Clone(p).(*ateapipb.EgressPolicy), nil
}

func (s *Server) UpdateActorEgressPolicy(ctx context.Context, req *ateapipb.UpdateActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref := req.GetActor()
	in := req.GetEgressPolicy()
	if err := checkEgressPolicyWrite(ref, in); err != nil {
		return nil, err
	}
	md := in.GetMetadata()
	if md.GetUid() == "" || md.GetVersion() == 0 {
		return nil, status.Error(codes.InvalidArgument, "EgressPolicy UID and version are required")
	}
	p, ok := s.egressPolicies[key(ref.GetAtespace(), ref.GetName())]
	if !ok {
		return nil, status.Error(codes.NotFound, "EgressPolicy not found")
	}
	if md.GetUid() != p.GetMetadata().GetUid() {
		return nil, status.Error(codes.Aborted, "EgressPolicy UID conflict")
	}
	if md.GetVersion() != p.GetMetadata().GetVersion() {
		return nil, status.Error(codes.Aborted, "EgressPolicy version conflict")
	}
	p.Rules = cloneRules(in.GetRules())
	applyEgressPolicyDefaults(p)
	p.Metadata.Version++
	p.Metadata.UpdateTime = timestamppb.Now()
	return proto.Clone(p).(*ateapipb.EgressPolicy), nil
}

func (s *Server) DeleteActorEgressPolicy(ctx context.Context, req *ateapipb.DeleteActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref := req.GetActor()
	if err := checkEgressPolicyRef(ref, req.GetName()); err != nil {
		return nil, err
	}
	k := key(ref.GetAtespace(), ref.GetName())
	p, ok := s.egressPolicies[k]
	if !ok {
		return nil, status.Error(codes.NotFound, "EgressPolicy not found")
	}
	if uid := req.GetOptions().GetUid(); uid != "" && uid != p.GetMetadata().GetUid() {
		return nil, status.Error(codes.Aborted, "EgressPolicy UID conflict")
	}
	if v := req.GetOptions().GetVersion(); v != 0 && v != p.GetMetadata().GetVersion() {
		return nil, status.Error(codes.Aborted, "EgressPolicy version conflict")
	}
	delete(s.egressPolicies, k)
	deleted := proto.Clone(p).(*ateapipb.EgressPolicy)
	deleted.Metadata.DeleteTime = timestamppb.Now()
	return deleted, nil
}

// checkEgressPolicyRef applies Substrate's checks of the actor reference and
// the policy name of a get or a delete: the actor's atespace and name are
// required, and the name is "default".
func checkEgressPolicyRef(ref *ateapipb.ObjectRef, name string) error {
	if err := checkActorRef(ref); err != nil {
		return err
	}
	return checkEgressPolicyName("name", name)
}

// checkEgressPolicyWrite applies Substrate's checks of a create or an update:
// the actor's atespace and name are required, and the policy's metadata is
// required, with the name "default" and the actor's atespace.
func checkEgressPolicyWrite(ref *ateapipb.ObjectRef, p *ateapipb.EgressPolicy) error {
	if err := checkActorRef(ref); err != nil {
		return err
	}
	if p == nil {
		return status.Error(codes.InvalidArgument, "egress_policy is required")
	}
	md := p.GetMetadata()
	if md == nil {
		return status.Error(codes.InvalidArgument, "egress_policy.metadata is required")
	}
	if err := checkEgressPolicyName("egress_policy.metadata.name", md.GetName()); err != nil {
		return err
	}
	if md.GetAtespace() == "" {
		return status.Error(codes.InvalidArgument, "egress_policy.metadata.atespace is required")
	}
	if md.GetAtespace() != ref.GetAtespace() {
		return status.Errorf(codes.InvalidArgument, "egress_policy.metadata.atespace %q must match actor.atespace %q", md.GetAtespace(), ref.GetAtespace())
	}
	return nil
}

func checkActorRef(ref *ateapipb.ObjectRef) error {
	switch {
	case ref == nil:
		return status.Error(codes.InvalidArgument, "actor is required")
	case ref.GetAtespace() == "":
		return status.Error(codes.InvalidArgument, "actor.atespace is required")
	case ref.GetName() == "":
		return status.Error(codes.InvalidArgument, "actor.name is required")
	}
	return nil
}

func checkEgressPolicyName(field, name string) error {
	if name == "" {
		return status.Errorf(codes.InvalidArgument, "%s is required", field)
	}
	if name != egressPolicyName {
		return status.Errorf(codes.InvalidArgument, "%s %q must be %q", field, name, egressPolicyName)
	}
	return nil
}

func cloneRules(rules []*ateapipb.EgressRule) []*ateapipb.EgressRule {
	out := make([]*ateapipb.EgressRule, len(rules))
	for i, r := range rules {
		out[i] = proto.Clone(r).(*ateapipb.EgressRule)
	}
	return out
}

// applyEgressPolicyDefaults fills the port defaults Substrate applies
// (cmd/ateapi/internal/defaults/defaults.go): 80 for http, 443 for https.
func applyEgressPolicyDefaults(p *ateapipb.EgressPolicy) {
	for _, r := range p.GetRules() {
		switch {
		case r.GetHttp() != nil && r.Http.Ports == nil:
			r.Http.Ports = &ateapipb.Ports{Numbers: []int32{80}}
		case r.GetHttps() != nil && r.Https.Ports == nil:
			r.Https.Ports = &ateapipb.Ports{Numbers: []int32{443}}
		}
	}
}

func (c inProcessControlClient) CreateActorEgressPolicy(ctx context.Context, in *ateapipb.CreateActorEgressPolicyRequest, _ ...grpc.CallOption) (*ateapipb.EgressPolicy, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	return c.s.CreateActorEgressPolicy(ctx, in)
}

func (c inProcessControlClient) GetActorEgressPolicy(ctx context.Context, in *ateapipb.GetActorEgressPolicyRequest, _ ...grpc.CallOption) (*ateapipb.EgressPolicy, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	return c.s.GetActorEgressPolicy(ctx, in)
}

func (c inProcessControlClient) UpdateActorEgressPolicy(ctx context.Context, in *ateapipb.UpdateActorEgressPolicyRequest, _ ...grpc.CallOption) (*ateapipb.EgressPolicy, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	return c.s.UpdateActorEgressPolicy(ctx, in)
}

func (c inProcessControlClient) DeleteActorEgressPolicy(ctx context.Context, in *ateapipb.DeleteActorEgressPolicyRequest, _ ...grpc.CallOption) (*ateapipb.EgressPolicy, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	return c.s.DeleteActorEgressPolicy(ctx, in)
}
