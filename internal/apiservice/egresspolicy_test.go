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
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

func egHTTP(ports *ateenvv1alpha.Ports, hosts ...string) *ateenvv1alpha.EgressRule {
	return &ateenvv1alpha.EgressRule{Protocol: &ateenvv1alpha.EgressRule_Http{Http: &ateenvv1alpha.HttpRule{Hostnames: hosts, Ports: ports}}}
}

func egHTTPS(ports *ateenvv1alpha.Ports, hosts ...string) *ateenvv1alpha.EgressRule {
	return &ateenvv1alpha.EgressRule{Protocol: &ateenvv1alpha.EgressRule_Https{Https: &ateenvv1alpha.HttpsRule{Hostnames: hosts, Ports: ports}}}
}

func egTLS(ports *ateenvv1alpha.Ports, hosts ...string) *ateenvv1alpha.EgressRule {
	return &ateenvv1alpha.EgressRule{Protocol: &ateenvv1alpha.EgressRule_TlsPassthrough{TlsPassthrough: &ateenvv1alpha.TlsPassthroughRule{Hostnames: hosts, Ports: ports}}}
}

func egPorts(n ...int32) *ateenvv1alpha.Ports { return &ateenvv1alpha.Ports{Numbers: n} }

func egAllPorts() *ateenvv1alpha.Ports { return &ateenvv1alpha.Ports{All: &ateenvv1alpha.AllPorts{}} }

func egPolicy(rules ...*ateenvv1alpha.EgressRule) *ateenvv1alpha.EgressPolicy {
	return &ateenvv1alpha.EgressPolicy{Rules: rules}
}

// storedPolicy reads the actor's Substrate egress policy from the fake
// control plane, or returns its error.
func storedPolicy(t *testing.T, te *testEnv, id string) (*ateapipb.EgressPolicy, error) {
	t.Helper()
	return te.control.GetActorEgressPolicy(t.Context(), &ateapipb.GetActorEgressPolicyRequest{
		Actor: &ateapipb.ObjectRef{Atespace: "ate-env", Name: id},
		Name:  "default",
	})
}

// TestEG1CreateEnvironmentWritesEgressPolicyBeforeWake checks EG-1: a create
// with egress_policy stores the actor's Substrate policy with the same rules
// before it responds, and the actor is still suspended then. The fake
// control plane leaves a new actor suspended until a resume, as Substrate
// does, and nothing resumes it here.
func TestEG1CreateEnvironmentWritesEgressPolicyBeforeWake(t *testing.T) {
	te := newFullTestEnv(t)
	te.control.BornSuspended = true
	_, err := te.envClient.CreateEnvironment(t.Context(), &ateenvv1alpha.CreateEnvironmentRequest{
		Id: "eg-1",
		EgressPolicy: egPolicy(
			egHTTPS(nil, "pypi.org", "files.pythonhosted.org"),
			egTLS(egPorts(443), "github.com"),
			egHTTP(egAllPorts(), "*.example.com"),
		),
	})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if st := te.control.Status("eg-1"); st != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Errorf("actor state = %v, want SUSPENDED: the policy must be written before a wake", st)
	}
	got, err := storedPolicy(t, te, "eg-1")
	if err != nil {
		t.Fatalf("stored policy: %v", err)
	}
	want := []*ateapipb.EgressRule{
		{Https: &ateapipb.HttpsRule{Hostnames: []string{"pypi.org", "files.pythonhosted.org"}, Ports: &ateapipb.Ports{Numbers: []int32{443}}}},
		{TlsPassthrough: &ateapipb.TlsPassthroughRule{Hostnames: []string{"github.com"}, Ports: &ateapipb.Ports{Numbers: []int32{443}}}},
		{Http: &ateapipb.HttpRule{Hostnames: []string{"*.example.com"}, Ports: &ateapipb.Ports{All: &ateapipb.AllPorts{}}}},
	}
	if len(got.GetRules()) != len(want) {
		t.Fatalf("stored rules = %v, want %v", got.GetRules(), want)
	}
	for i := range want {
		if !proto.Equal(got.GetRules()[i], want[i]) {
			t.Errorf("stored rules[%d] = %v, want %v", i, got.GetRules()[i], want[i])
		}
	}
}

// TestEG2CreateEnvironmentEmptyRulesStoresDenyAll checks EG-2: a policy with
// no rules is stored as a Substrate policy with no rules.
func TestEG2CreateEnvironmentEmptyRulesStoresDenyAll(t *testing.T) {
	te := newFullTestEnv(t)
	if _, err := te.envClient.CreateEnvironment(t.Context(), &ateenvv1alpha.CreateEnvironmentRequest{
		Id: "eg-2", EgressPolicy: &ateenvv1alpha.EgressPolicy{},
	}); err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	got, err := storedPolicy(t, te, "eg-2")
	if err != nil {
		t.Fatalf("stored policy: %v (want a policy with no rules)", err)
	}
	if n := len(got.GetRules()); n != 0 {
		t.Errorf("stored rules = %d, want 0", n)
	}
}

// TestEG3CreateEnvironmentInvalidEgressRule checks EG-3: each rule that
// Substrate v1.0.0 refuses is InvalidArgument, and no actor is created.
func TestEG3CreateEnvironmentInvalidEgressRule(t *testing.T) {
	manyHosts := make([]string, 257)
	for i := range manyHosts {
		manyHosts[i] = fmt.Sprintf("h%d.example.com", i)
	}
	manyRules := make([]*ateenvv1alpha.EgressRule, 257)
	for i := range manyRules {
		manyRules[i] = egHTTPS(nil, fmt.Sprintf("h%d.example.com", i))
	}
	manyPorts := make([]int32, 17)
	for i := range manyPorts {
		manyPorts[i] = int32(1000 + i)
	}
	cases := map[string]*ateenvv1alpha.EgressPolicy{
		"no protocol":                   egPolicy(&ateenvv1alpha.EgressRule{}),
		"no hostnames":                  egPolicy(egHTTPS(nil)),
		"more than 256 hostnames":       egPolicy(egHTTPS(nil, manyHosts...)),
		"repeated hostname":             egPolicy(egHTTPS(nil, "a.example.com", "a.example.com")),
		"uppercase":                     egPolicy(egHTTPS(nil, "PyPI.org")),
		"trailing dot":                  egPolicy(egHTTPS(nil, "pypi.org.")),
		"empty label":                   egPolicy(egHTTPS(nil, "pypi..org")),
		"empty hostname":                egPolicy(egHTTPS(nil, "")),
		"wildcard inside a label":       egPolicy(egHTTPS(nil, "a*.example.com")),
		"wildcard not leftmost":         egPolicy(egHTTPS(nil, "api.*.example.com")),
		"two wildcard labels":           egPolicy(egHTTPS(nil, "*.*.example.com")),
		"wildcard with no name":         egPolicy(egHTTPS(nil, "*.")),
		"IPv4 address":                  egPolicy(egHTTPS(nil, "10.0.0.1")),
		"IPv6 address":                  egPolicy(egHTTPS(nil, "::1")),
		"URL":                           egPolicy(egHTTPS(nil, "https://pypi.org")),
		"hostname with a port":          egPolicy(egHTTPS(nil, "pypi.org:443")),
		"all-digit last label":          egPolicy(egHTTPS(nil, "foo.123")),
		"name over 253 characters":      egPolicy(egHTTPS(nil, strings.Repeat("a.", 127)+"a")),
		"ports with all and numbers":    egPolicy(egHTTPS(&ateenvv1alpha.Ports{All: &ateenvv1alpha.AllPorts{}, Numbers: []int32{443}}, "pypi.org")),
		"ports with neither":            egPolicy(egHTTPS(&ateenvv1alpha.Ports{}, "pypi.org")),
		"more than 16 port numbers":     egPolicy(egHTTPS(egPorts(manyPorts...), "pypi.org")),
		"repeated port number":          egPolicy(egHTTPS(egPorts(443, 443), "pypi.org")),
		"port 0":                        egPolicy(egHTTPS(egPorts(0), "pypi.org")),
		"port 65536":                    egPolicy(egHTTPS(egPorts(65536), "pypi.org")),
		"negative port":                 egPolicy(egHTTPS(egPorts(-1), "pypi.org")),
		"tls_passthrough without ports": egPolicy(egTLS(nil, "github.com")),
		"more than 256 rules":           egPolicy(manyRules...),
		"tie across protocols":          egPolicy(egHTTPS(nil, "pypi.org"), egTLS(egPorts(443), "pypi.org")),
		"tie after the http default":    egPolicy(egHTTP(nil, "pypi.org"), egTLS(egPorts(80), "pypi.org")),
		"tie on all ports":              egPolicy(egHTTP(egAllPorts(), "pypi.org"), egTLS(egAllPorts(), "pypi.org")),
	}
	te := newFullTestEnv(t)
	for name, p := range cases {
		_, err := te.envClient.CreateEnvironment(t.Context(), &ateenvv1alpha.CreateEnvironmentRequest{Id: "eg-3", EgressPolicy: p})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: CreateEnvironment = %v, want InvalidArgument", name, err)
		}
	}
	if n := te.control.Creates(); n != 0 {
		t.Errorf("CreateActor calls = %d, want 0", n)
	}
}

// TestEG3CreateEnvironmentValidEgressRules checks the other side of EG-3:
// the shapes Substrate accepts pass the local check, including the limits
// and ports that overlap without a tie.
func TestEG3CreateEnvironmentValidEgressRules(t *testing.T) {
	hosts := make([]string, 256)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("h%d.example.com", i)
	}
	ports := make([]int32, 16)
	for i := range ports {
		ports[i] = int32(1000 + i)
	}
	manyRules256 := make([]*ateenvv1alpha.EgressRule, 256)
	for i := range manyRules256 {
		manyRules256[i] = egHTTPS(nil, fmt.Sprintf("r%d.example.com", i))
	}
	cases := map[string]*ateenvv1alpha.EgressPolicy{
		"wildcard and any":               egPolicy(egHTTPS(nil, "*.example.com", "*")),
		"256 hostnames and 16 ports":     egPolicy(egTLS(egPorts(ports...), hosts...)),
		"same hostname on other ports":   egPolicy(egHTTP(nil, "pypi.org"), egHTTPS(nil, "pypi.org"), egTLS(egPorts(8443), "pypi.org")),
		"all ports next to a number":     egPolicy(egHTTP(egAllPorts(), "pypi.org"), egTLS(egPorts(443), "pypi.org")),
		"digits in a non-final label":    egPolicy(egHTTPS(nil, "1.example.com")),
		"ports 1 and 65535":              egPolicy(egTLS(egPorts(1, 65535), "github.com")),
		"hyphen inside a label":          egPolicy(egHTTPS(nil, "files-1.pythonhosted.org")),
		"single-label name":              egPolicy(egHTTPS(nil, "localhost")),
		"wildcard over a 2-label suffix": egPolicy(egHTTPS(nil, "*.co.uk")),
		"256 rules":                      egPolicy(manyRules256...),
	}
	te := newFullTestEnv(t)
	i := 0
	for name, p := range cases {
		i++
		id := fmt.Sprintf("eg-3-ok-%d", i)
		if _, err := te.envClient.CreateEnvironment(t.Context(), &ateenvv1alpha.CreateEnvironmentRequest{Id: id, EgressPolicy: p}); err != nil {
			t.Errorf("%s: CreateEnvironment = %v, want success", name, err)
		}
	}
}

// TestEG4CreateEnvironmentPolicyFailureDeletesActor checks EG-4 for the
// policy write: a failed write deletes the actor, and the create returns the
// write's error code.
func TestEG4CreateEnvironmentPolicyFailureDeletesActor(t *testing.T) {
	te := newFullTestEnv(t)
	te.control.OnCreateEgressPolicy = func(context.Context) error {
		return status.Error(codes.ResourceExhausted, "policy store is full")
	}
	_, err := te.envClient.CreateEnvironment(t.Context(), &ateenvv1alpha.CreateEnvironmentRequest{Id: "eg-4", EgressPolicy: egPolicy(egHTTPS(nil, "pypi.org"))})
	if status.Code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), "policy store is full") {
		t.Errorf("CreateEnvironment = %v, want ResourceExhausted with the reason", err)
	}
	if te.control.Creates() != 1 || te.control.ActorCount() != 0 {
		t.Errorf("after the failed create: %d CreateActor calls, %d actors; want the one actor created and deleted again", te.control.Creates(), te.control.ActorCount())
	}
}

// TestEG4CreateEnvironmentStoreFailureDeletesActorAndPolicy checks EG-4 for
// the environment store's object of a create with time_to_live: its failed
// write deletes the actor, and the policy written before goes with it.
func TestEG4CreateEnvironmentStoreFailureDeletesActorAndPolicy(t *testing.T) {
	te, _, kube := ttlEnv(t)
	kube.PrependReactor("create", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the Kubernetes API is down")
	})
	_, err := te.envClient.CreateEnvironment(t.Context(), &ateenvv1alpha.CreateEnvironmentRequest{
		Id:           "eg-4s",
		TimeToLive:   durationpb.New(time.Minute),
		EgressPolicy: egPolicy(egHTTPS(nil, "pypi.org")),
	})
	if status.Code(err) != codes.Unavailable {
		t.Errorf("CreateEnvironment = %v, want Unavailable, the code of EM-6's rollback", err)
	}
	if te.control.ActorCount() != 0 {
		t.Errorf("actors = %d, want 0", te.control.ActorCount())
	}
	if _, err := storedPolicy(t, te, "eg-4s"); status.Code(err) != codes.NotFound {
		t.Errorf("stored policy after the rollback: err = %v, want NotFound", err)
	}
}

// TestEG4RollbackSurvivesCallerCancel checks the bounded context of EG-4:
// the caller cancels while the policy is written, and the delete of the
// actor still runs.
func TestEG4RollbackSurvivesCallerCancel(t *testing.T) {
	te := newFullTestEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	te.control.OnCreateEgressPolicy = func(callCtx context.Context) error {
		cancel()
		// The write ends only once the cancel reached the API server's
		// call, thus its rollback starts with the caller's context done.
		<-callCtx.Done()
		return status.FromContextError(callCtx.Err()).Err()
	}
	_, err := te.envClient.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "eg-4c", EgressPolicy: egPolicy(egHTTPS(nil, "pypi.org"))})
	if status.Code(err) != codes.Canceled {
		t.Errorf("CreateEnvironment = %v, want Canceled", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for te.control.ActorCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := te.control.ActorCount(); n != 0 {
		t.Errorf("actors = %d, want 0: the rollback must outlive the caller's cancel", n)
	}
}

// TestEG4RollbackDeletesOnlyTheActorItCreated checks the uid precondition
// of EG-4's rollback: an environment deleted and created again with the same
// id while the policy write failed is another actor, and stays.
func TestEG4RollbackDeletesOnlyTheActorItCreated(t *testing.T) {
	te := newFullTestEnv(t)
	te.control.OnCreateEgressPolicy = func(context.Context) error {
		recreateBehind(t, te, "eg-4u")
		return status.Error(codes.Unavailable, "policy store is down")
	}
	_, err := te.envClient.CreateEnvironment(t.Context(), &ateenvv1alpha.CreateEnvironmentRequest{Id: "eg-4u", EgressPolicy: egPolicy(egHTTPS(nil, "pypi.org"))})
	if status.Code(err) != codes.Unavailable {
		t.Errorf("CreateEnvironment = %v, want Unavailable", err)
	}
	if !exists(t, te, "eg-4u") {
		t.Error("the rollback deleted the environment that was created again with the same id")
	}
}

// TestEG4PolicyAlreadyExistsKeepsTheCode checks that an AlreadyExists from
// the policy write keeps its code, which ate.Client wraps in its own error.
func TestEG4PolicyAlreadyExistsKeepsTheCode(t *testing.T) {
	te := newFullTestEnv(t)
	te.control.OnCreateEgressPolicy = func(context.Context) error {
		return status.Error(codes.AlreadyExists, "EgressPolicy already exists")
	}
	_, err := te.envClient.CreateEnvironment(t.Context(), &ateenvv1alpha.CreateEnvironmentRequest{Id: "eg-4a", EgressPolicy: egPolicy(egHTTPS(nil, "pypi.org"))})
	if status.Code(err) != codes.AlreadyExists {
		t.Errorf("CreateEnvironment = %v, want AlreadyExists", err)
	}
	if te.control.ActorCount() != 0 {
		t.Errorf("actors = %d, want 0", te.control.ActorCount())
	}
}

// TestEG5CreateEnvironmentPolicyAndDeleteFailure checks EG-5: when the
// policy write and the delete both fail, the error names both.
func TestEG5CreateEnvironmentPolicyAndDeleteFailure(t *testing.T) {
	te := newFullTestEnv(t)
	te.control.OnCreateEgressPolicy = func(context.Context) error {
		return status.Error(codes.ResourceExhausted, "policy store is full")
	}
	te.control.FailDeletes(1, codes.Internal)
	_, err := te.envClient.CreateEnvironment(t.Context(), &ateenvv1alpha.CreateEnvironmentRequest{Id: "eg-5", EgressPolicy: egPolicy(egHTTPS(nil, "pypi.org"))})
	if err == nil {
		t.Fatal("CreateEnvironment = nil, want an error")
	}
	if status.Code(err) != codes.Internal {
		t.Errorf("CreateEnvironment code = %v, want Internal", status.Code(err))
	}
	msg := status.Convert(err).Message()
	if !strings.Contains(msg, "policy store is full") || !strings.Contains(msg, "delete failed") {
		t.Errorf("CreateEnvironment error %q, want it to name the policy write and the delete failure", msg)
	}
	if te.control.ActorCount() != 1 {
		t.Errorf("actors = %d, want 1: the delete failed", te.control.ActorCount())
	}
}

// TestEG6CreateEnvironmentWithoutEgressPolicy checks EG-6: a create without
// egress_policy makes no egress policy call and succeeds as before.
func TestEG6CreateEnvironmentWithoutEgressPolicy(t *testing.T) {
	te := newFullTestEnv(t)
	resp, err := te.envClient.CreateEnvironment(t.Context(), &ateenvv1alpha.CreateEnvironmentRequest{Id: "eg-6"})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if resp.GetEnvironment().GetId() != "eg-6" {
		t.Errorf("environment id = %q, want eg-6", resp.GetEnvironment().GetId())
	}
	if n := te.control.EgressPolicyCalls(); n != 0 {
		t.Errorf("egress policy calls = %d, want 0", n)
	}
}
