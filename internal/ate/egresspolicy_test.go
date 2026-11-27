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
	"errors"
	"testing"

	"github.com/agent-substrate/env/internal/ate"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func rulesEqual(a, b []*ateapipb.EgressRule) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !proto.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

func httpsRule(hosts ...string) *ateapipb.EgressRule {
	return &ateapipb.EgressRule{Https: &ateapipb.HttpsRule{Hostnames: hosts}}
}

func passthroughRule(port int32, hosts ...string) *ateapipb.EgressRule {
	return &ateapipb.EgressRule{TlsPassthrough: &ateapipb.TlsPassthroughRule{Hostnames: hosts, Ports: &ateapipb.Ports{Numbers: []int32{port}}}}
}

// TestEgressPolicyCreateGetUpdate checks the round trip the egress policy
// plan builds on: a create stores the rules with Substrate's port defaults,
// a get returns them, and an update with the read uid and version replaces
// them and bumps the version.
func TestEgressPolicyCreateGetUpdate(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.create(t, "eg-1")

	created, err := f.client.CreateEgressPolicy(ctx, "", "eg-1", []*ateapipb.EgressRule{httpsRule("pypi.org")})
	if err != nil {
		t.Fatalf("CreateEgressPolicy: %v", err)
	}
	want := []*ateapipb.EgressRule{{Https: &ateapipb.HttpsRule{Hostnames: []string{"pypi.org"}, Ports: &ateapipb.Ports{Numbers: []int32{443}}}}}
	if !rulesEqual(created.GetRules(), want) {
		t.Errorf("created rules = %v, want %v", created.GetRules(), want)
	}
	if md := created.GetMetadata(); md.GetAtespace() != ate.DefaultAtespace || md.GetName() != "default" || md.GetUid() == "" || md.GetVersion() == 0 {
		t.Errorf("created metadata = %v, want atespace %q, name \"default\", a uid and a version", md, ate.DefaultAtespace)
	}

	got, err := f.client.GetEgressPolicy(ctx, "", "eg-1")
	if err != nil {
		t.Fatalf("GetEgressPolicy: %v", err)
	}
	if !proto.Equal(got, created) {
		t.Errorf("GetEgressPolicy = %v, want %v", got, created)
	}

	next := []*ateapipb.EgressRule{passthroughRule(443, "github.com")}
	updated, err := f.client.UpdateEgressPolicy(ctx, "", "eg-1", got.GetMetadata().GetUid(), got.GetMetadata().GetVersion(), next)
	if err != nil {
		t.Fatalf("UpdateEgressPolicy: %v", err)
	}
	if !rulesEqual(updated.GetRules(), next) {
		t.Errorf("updated rules = %v, want %v", updated.GetRules(), next)
	}
	if updated.GetMetadata().GetVersion() <= got.GetMetadata().GetVersion() {
		t.Errorf("updated version = %d, want more than %d", updated.GetMetadata().GetVersion(), got.GetMetadata().GetVersion())
	}
}

// TestEgressPolicyNoRules checks that a policy with no rules is stored as
// such: the deny-all shape of the egress policy plan.
func TestEgressPolicyNoRules(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.create(t, "eg-deny")

	if _, err := f.client.CreateEgressPolicy(ctx, "", "eg-deny", nil); err != nil {
		t.Fatalf("CreateEgressPolicy with no rules: %v", err)
	}
	got, err := f.client.GetEgressPolicy(ctx, "", "eg-deny")
	if err != nil {
		t.Fatalf("GetEgressPolicy: %v", err)
	}
	if n := len(got.GetRules()); n != 0 {
		t.Errorf("rules = %d, want 0", n)
	}
}

// TestEgressPolicyErrors checks the error of each failure the egress policy
// plan relies on, with Substrate's codes.
func TestEgressPolicyErrors(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.create(t, "eg-err")

	if _, err := f.client.GetEgressPolicy(ctx, "", "eg-err"); !errors.Is(err, ate.ErrNotFound) {
		t.Errorf("GetEgressPolicy with no policy: err = %v, want ErrNotFound", err)
	}
	if _, err := f.client.UpdateEgressPolicy(ctx, "", "eg-err", "uid", 1, nil); !errors.Is(err, ate.ErrNotFound) {
		t.Errorf("UpdateEgressPolicy with no policy: err = %v, want ErrNotFound", err)
	}
	if _, err := f.client.CreateEgressPolicy(ctx, "", "absent", nil); status.Code(errors.Unwrap(err)) != codes.FailedPrecondition {
		t.Errorf("CreateEgressPolicy under a missing actor: err = %v, want FailedPrecondition", err)
	}

	p, err := f.client.CreateEgressPolicy(ctx, "", "eg-err", nil)
	if err != nil {
		t.Fatalf("CreateEgressPolicy: %v", err)
	}
	if _, err := f.client.CreateEgressPolicy(ctx, "", "eg-err", nil); !errors.Is(err, ate.ErrAlreadyExists) {
		t.Errorf("second CreateEgressPolicy: err = %v, want ErrAlreadyExists", err)
	}
	uid, version := p.GetMetadata().GetUid(), p.GetMetadata().GetVersion()
	if _, err := f.client.UpdateEgressPolicy(ctx, "", "eg-err", uid, version+1, nil); status.Code(errors.Unwrap(err)) != codes.Aborted {
		t.Errorf("UpdateEgressPolicy with a stale version: err = %v, want Aborted", err)
	}
	if _, err := f.client.UpdateEgressPolicy(ctx, "", "eg-err", "other", version, nil); status.Code(errors.Unwrap(err)) != codes.Aborted {
		t.Errorf("UpdateEgressPolicy with another uid: err = %v, want Aborted", err)
	}
	if _, err := f.client.UpdateEgressPolicy(ctx, "", "eg-err", "", 0, nil); err == nil {
		t.Error("UpdateEgressPolicy without uid and version: want error, got nil")
	}
}

// TestDeleteDeletesEgressPolicy checks that a policy goes with its actor, as
// in Substrate, so an actor created again under the same id has none.
func TestDeleteDeletesEgressPolicy(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.create(t, "eg-del")
	if _, err := f.client.CreateEgressPolicy(ctx, "", "eg-del", nil); err != nil {
		t.Fatalf("CreateEgressPolicy: %v", err)
	}
	if err := f.client.Delete(ctx, "", "eg-del"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	f.create(t, "eg-del")
	if _, err := f.client.GetEgressPolicy(ctx, "", "eg-del"); !errors.Is(err, ate.ErrNotFound) {
		t.Errorf("GetEgressPolicy after delete and create: err = %v, want ErrNotFound", err)
	}
}
