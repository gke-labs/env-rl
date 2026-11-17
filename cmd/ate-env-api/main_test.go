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

package main

import (
	"context"
	"net"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/agent-substrate/env/internal/apiservice"
	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/internaltest/fakecontrol"
	"github.com/agent-substrate/env/internal/portproxy"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
)

// TestAtespacesFlag checks the parse that -port-proxy-atespaces and
// -list-environments-atespaces share.
func TestAtespacesFlag(t *testing.T) {
	for in, want := range map[string][]string{
		"ate-env":            {"ate-env"},
		" ate-env , team-a ": {"ate-env", "team-a"},
		"*":                  nil,
		" * ":                nil,
	} {
		if got, err := atespacesFlag("port-proxy-atespaces", in); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("atespacesFlag(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// An empty value must not mean "any atespace", and "*" cannot be combined with names.
	// The error names the flag.
	for _, in := range []string{"", " ", ",", " , ", "ate-env,*", "*,ate-env"} {
		got, err := atespacesFlag("list-environments-atespaces", in)
		if err == nil || !strings.Contains(err.Error(), "-list-environments-atespaces") {
			t.Errorf("atespacesFlag(%q) = %q, %v; want an error that names the flag", in, got, err)
		}
	}
	// AE-11: without the flag, ListEnvironments reads the default atespace alone.
	if got, err := atespacesFlag("list-environments-atespaces", listEnvironmentsAtespacesDefault); err != nil || !reflect.DeepEqual(got, []string{ate.DefaultAtespace}) {
		t.Errorf("the default of -list-environments-atespaces parses to %q, %v; want the default atespace alone", got, err)
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:7777", true},
		{"localhost:7777", true},
		{"[::1]:7777", true},
		{"0.0.0.0:7777", false},
		{":7777", false},
		{"10.0.0.5:7777", false},
	}
	for _, tc := range cases {
		if got := isLoopbackAddr(tc.addr); got != tc.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

type stubEnvironments struct {
	ateenvv1alpha.UnimplementedEnvironmentServiceServer
}

func (stubEnvironments) GetEnvironment(context.Context, *ateenvv1alpha.GetEnvironmentRequest) (*ateenvv1alpha.GetEnvironmentResponse, error) {
	return &ateenvv1alpha.GetEnvironmentResponse{}, nil
}

// The API's interceptor chain authenticates before it checks for unknown
// fields, so an unauthenticated caller learns nothing from that check.
func TestGRPCServerOptionsAuthenticateBeforeUnknownFieldCheck(t *testing.T) {
	auth, err := apiservice.NewBearerAuth("tok")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		auth    *apiservice.BearerAuth
		token   string
		unknown bool
		want    codes.Code
	}{
		{"no token, unknown field", auth, "", true, codes.Unauthenticated},
		{"token, unknown field", auth, "tok", true, codes.InvalidArgument},
		{"token, clean request", auth, "tok", false, codes.OK},
		{"auth off, unknown field", nil, "", true, codes.InvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := grpc.NewServer(grpcServerOptions(tc.auth, nil, false)...)
			ateenvv1alpha.RegisterEnvironmentServiceServer(srv, stubEnvironments{})
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go srv.Serve(lis)
			defer srv.Stop()
			conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()

			req := &ateenvv1alpha.GetEnvironmentRequest{Id: "e1"}
			if tc.unknown {
				req.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1))
			}
			ctx := context.Background()
			if tc.token != "" {
				ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tc.token)
			}
			_, err = ateenvv1alpha.NewEnvironmentServiceClient(conn).GetEnvironment(ctx, req)
			if got := status.Code(err); got != tc.want {
				t.Errorf("code = %v (%v), want %v", got, err, tc.want)
			}
		})
	}
}

func TestServiceHostNames(t *testing.T) {
	files := func(m map[string]string) func(string) ([]byte, error) {
		return func(p string) ([]byte, error) {
			if v, ok := m[p]; ok {
				return []byte(v), nil
			}
			return nil, os.ErrNotExist
		}
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	kubelet := "search team-env.svc.corp.k8s svc.corp.k8s corp.k8s\nnameserver 10.0.0.10\n"

	for _, tc := range []struct {
		name    string
		service string
		files   map[string]string
		env     map[string]string
		want    []string
	}{
		{"resolv.conf gives namespace and domain", "ate-env-api", map[string]string{resolvConfPath: kubelet}, map[string]string{"POD_NAMESPACE": "other"},
			[]string{"ate-env-api", "ate-env-api.team-env", "ate-env-api.team-env.svc", "ate-env-api.team-env.svc.corp.k8s"}},
		{"POD_NAMESPACE without a cluster search list", "ate-env-api", map[string]string{resolvConfPath: "nameserver 1.1.1.1\n"}, map[string]string{"POD_NAMESPACE": "team-env"},
			[]string{"ate-env-api", "ate-env-api.team-env", "ate-env-api.team-env.svc"}},
		{"ServiceAccount namespace file", "ate-env-api", map[string]string{podNamespacePath: "team-env\n"}, nil,
			[]string{"ate-env-api", "ate-env-api.team-env", "ate-env-api.team-env.svc"}},
		{"outside a cluster", "ate-env-api", nil, nil, []string{"ate-env-api"}},
		{"disabled", "", map[string]string{resolvConfPath: kubelet}, nil, nil},
	} {
		got := serviceHostNames(tc.service, files(tc.files), env(tc.env))
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The port proxy reads actor states through the API server, so a crashed or
// deleted environment fails at once, and checks callers with the API's
// token.
func TestPortProxyOptionsWiresStateReadAndAuth(t *testing.T) {
	control := fakecontrol.New()
	controlAddr, stop, err := control.Serve()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	client, err := ate.New(ate.Options{ControlAddr: controlAddr, RouterAddr: "127.0.0.1:1", SkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	auth, err := apiservice.NewBearerAuth("s3cret")
	if err != nil {
		t.Fatal(err)
	}

	opts := portProxyOptions(portproxy.Options{ConnectAddr: "router:8444"}, apiservice.New(client, "127.0.0.1:1", ""), auth)
	if opts.ConnectAddr != "router:8444" {
		t.Errorf("ConnectAddr = %q, want the base options kept", opts.ConnectAddr)
	}
	if opts.ActorState == nil {
		t.Fatal("ActorState is not set: a crashed environment's port-proxy request would wait out the wake budget")
	}
	if _, err := opts.ActorState(context.Background(), "ate-env", "missing"); status.Code(err) != codes.NotFound {
		t.Errorf("ActorState of a missing environment: %v, want NotFound from the control plane", err)
	}
	if opts.Wake == nil {
		t.Fatal("Wake is not set: a request with X-Env-Wake-Wait would not be held")
	}
	if opts.Authenticate == nil || opts.Authenticate("Bearer s3cret") != nil || opts.Authenticate("Bearer wrong") == nil {
		t.Error("Authenticate does not check the API's token")
	}
	if noAuth := portProxyOptions(portproxy.Options{}, apiservice.New(client, "127.0.0.1:1", ""), nil); noAuth.Authenticate != nil {
		t.Error("Authenticate set without -auth-token-file")
	}
}

// TestCheckPortProxyHostSuffix: a host suffix that cannot work is an error of
// the start (PP-13). The suffix must not cover a name of the API: localhost,
// a Service name or a name of --allowed-hosts. A suffix with the port proxy
// off (--atenet-connect empty) would start and then answer the Host check's
// 403 for each name under it.
func TestCheckPortProxyHostSuffix(t *testing.T) {
	service := []string{"ate-env-api", "ate-env-api.ate-env", "ate-env-api.ate-env.svc", "ate-env-api.ate-env.svc.cluster.local"}
	allowed := []string{"lb.internal.example", "*.corp.example"}
	const connect = "atenet-router.ate-system.svc.cluster.local:8444"
	for _, tc := range []struct {
		suffix, connect string
		service         []string
		allowed         []string
		wantErr         string
	}{
		{"", connect, service, allowed, ""},
		// The form is off: the port proxy can be off too.
		{"", "", service, allowed, ""},
		{"ports.example", connect, service, allowed, ""},
		{"ports.internal.example", connect, service, allowed, ""},
		{"ports.example", connect, nil, nil, ""},
		// The port proxy is off: nothing serves the names under the suffix.
		{"ports.example", "", service, allowed, "--atenet-connect"},
		{"ports.example", "", nil, nil, "--atenet-connect"},
		// A name of the API, or a name above one.
		{"ate-env.svc", connect, service, allowed, "ate-env-api.ate-env.svc"},
		{"svc.cluster.local", connect, service, allowed, "ate-env-api.ate-env.svc.cluster.local"},
		{"internal.example", connect, service, allowed, "lb.internal.example"},
		{"corp.example", connect, service, allowed, "*.corp.example"},
		{"ate-env.svc", connect, nil, allowed, ""}, // no Service names known: nothing to cover
		// Not a domain name of two labels or more.
		{"localhost", connect, service, allowed, "one label"},
		{"com", connect, nil, nil, "one label"},
		{"https://ports.example", connect, service, allowed, "not a domain name"},
		// A wrong suffix is named also when the port proxy is off.
		{"com", "", nil, nil, "one label"},
	} {
		err := checkPortProxyHostSuffix(tc.suffix, tc.connect, tc.service, tc.allowed)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("checkPortProxyHostSuffix(%q, %q) = %v, want no error", tc.suffix, tc.connect, err)
		case tc.wantErr != "" && err == nil:
			t.Errorf("checkPortProxyHostSuffix(%q, %q) = nil, want an error with %q", tc.suffix, tc.connect, tc.wantErr)
		case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
			t.Errorf("checkPortProxyHostSuffix(%q, %q) = %v, want an error with %q", tc.suffix, tc.connect, err, tc.wantErr)
		case err != nil && !strings.Contains(err.Error(), "--port-proxy-host-suffix"):
			t.Errorf("checkPortProxyHostSuffix(%q, %q) = %v, want the flag name in the error", tc.suffix, tc.connect, err)
		}
	}
}
