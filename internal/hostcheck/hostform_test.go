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

package hostcheck

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/portproxy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// PP-8: WrapFor lets a Host pass that the route says it serves, and no
// other: an allowed Host passes without the question, and a Host that is
// neither gets the 403 that Wrap gives.
func TestWrapFor(t *testing.T) {
	c, err := New([]string{"lb.internal.example"})
	if err != nil {
		t.Fatal(err)
	}
	var asked []string
	serves := func(hostport string) bool {
		asked = append(asked, hostport)
		return hostport == "8000-env1.ports.example:7777"
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "served") })
	answer := func(h http.Handler, host string) (int, string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Host = host
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	route, plain := c.WrapFor(serves)(next), c.Wrap(next)

	if code, body := answer(route, "8000-env1.ports.example:7777"); code != http.StatusOK || body != "served" {
		t.Errorf("a Host the route serves: got %d %q, want it served", code, body)
	}
	if code, body := answer(plain, "8000-env1.ports.example:7777"); code != http.StatusForbidden || !strings.Contains(body, "--allowed-hosts") {
		t.Errorf("the same Host behind Wrap: got %d %q, want the 403", code, body)
	}
	for _, host := range []string{"attacker.example:7777", "9000-env1.ports.example:7777", "ports.example"} {
		code, body := answer(route, host)
		wantCode, wantBody := answer(plain, host)
		if code != http.StatusForbidden || code != wantCode || body != wantBody {
			t.Errorf("Host %s: WrapFor answers %d %q, Wrap %d %q; want the same 403", host, code, body, wantCode, wantBody)
		}
	}
	asked = nil
	for _, host := range []string{"lb.internal.example", "127.0.0.1:7777", "localhost:7777", ""} {
		if code, _ := answer(route, host); code != http.StatusOK {
			t.Errorf("allowed Host %q: got %d, want it served", host, code)
		}
	}
	if len(asked) != 0 {
		t.Errorf("the route was asked about allowed hosts %q, want no question", asked)
	}
	if code, _ := answer(c.WrapFor(nil)(next), "8000-env1.ports.example:7777"); code != http.StatusForbidden {
		t.Errorf("WrapFor(nil): got %d, want the 403 of Wrap", code)
	}
}

// listenerStack serves the composition of cmd/ate-env-api on one listener
// with HTTP/1.1 and h2c: the port proxy's Dispatch, inside
// WrapFor(ServesHost), in front of a mux with /healthz, the MCP route and
// gRPC. suffix is the proxy's host suffix; empty turns the hostname form off.
func listenerStack(t *testing.T, suffix string) string {
	t.Helper()
	grpcServer := grpc.NewServer()
	healthpb.RegisterHealthServer(grpcServer, health.NewServer())
	t.Cleanup(grpcServer.Stop)

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	connectAddr := closed.Addr().String()
	closed.Close() // nothing listens: the proxy fails fast instead of reaching a router
	pp, err := portproxy.New(portproxy.Options{
		ConnectAddr: connectAddr, DefaultAtespace: "ate-env", WakeBudget: 50 * time.Millisecond,
		HandshakeTimeout: time.Second, PortHostSuffix: suffix,
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := New([]string{"ate-env-api.ate-env"})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", c.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })))
	mux.Handle("/v1alpha/envs/{id}/mcp", c.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "mcp\n") })))
	mux.Handle("/", grpcServer)

	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: portproxy.Dispatch(pp, mux, c.WrapFor(pp.ServesHost)), Protocols: &protocols}
	go srv.Serve(lis)
	t.Cleanup(func() { srv.Close() })
	return lis.Addr().String()
}

// PP-5 and PP-8, on the listener as cmd/ate-env-api composes it. With the
// suffix, a name that names an environment port reaches the port proxy on
// each path and nothing else; a name under the suffix that names none gets
// the proxy's 400; each other Host gets what it got before. Without the
// suffix, the names get the 403 of an unknown host.
func TestHostFormListenerStack(t *testing.T) {
	const suffix = "ports.example"
	paths := []string{"/healthz", "/v1alpha/envs/dev1/mcp", "/v1alpha/envs/dev1/ports/8000/", "/any"}
	valid := []string{"8000-env1." + suffix, "8000-env1.team-a." + suffix + ":7777", "8000-ENV1." + suffix + "."}
	invalid := []string{suffix, "api." + suffix, "80-env1." + suffix, "8000-env1.a.b." + suffix, "8000-env1.." + suffix}
	foreign := []string{
		"attacker.example:7777", "8000-env1." + suffix + ".attacker.example", "8000-env1.x" + suffix,
		"8000-env1." + suffix + "..", "8000-env1.ports", "8000-env1.example",
	}

	var h2c http.Protocols
	h2c.SetUnencryptedHTTP2(true)
	clients := map[string]*http.Client{"HTTP/1.1": http.DefaultClient, "h2c": {Transport: &http.Transport{Protocols: &h2c}}}
	get := func(client *http.Client, addr, host, path string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
		req.Host = host
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s %s: %v", host, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	refused := func(code int, body string) bool {
		return code == http.StatusForbidden && strings.Contains(body, "--allowed-hosts")
	}

	on, off := listenerStack(t, suffix), listenerStack(t, "")
	for proto, client := range clients {
		for _, path := range paths {
			for _, host := range valid {
				// The proxy is reached (and fails with 502, nothing listens).
				if code, body := get(client, on, host, path); code != http.StatusBadGateway || !strings.Contains(body, "environment ") {
					t.Errorf("%s, suffix set, Host %s on %s: got %d %q, want the port proxy's 502", proto, host, path, code, body)
				}
			}
			for _, host := range invalid {
				if code, body := get(client, on, host, path); code != http.StatusBadRequest || !strings.HasPrefix(body, "host ") {
					t.Errorf("%s, suffix set, Host %s on %s: got %d %q, want the port proxy's 400", proto, host, path, code, body)
				}
			}
			// The last path is no HTTP route: it is the gRPC handler's, which
			// the Host check does not wrap, as before.
			if path == "/any" {
				continue
			}
			for _, host := range foreign {
				if code, body := get(client, on, host, path); !refused(code, body) {
					t.Errorf("%s, suffix set, Host %s on %s: got %d %q, want the hostcheck 403", proto, host, path, code, body)
				}
			}
			// Without the suffix each of these names is an unknown host.
			for _, host := range append(append(append([]string{}, valid...), invalid...), foreign...) {
				if code, body := get(client, off, host, path); !refused(code, body) {
					t.Errorf("%s, no suffix, Host %s on %s: got %d %q, want the hostcheck 403", proto, host, path, code, body)
				}
			}
		}
	}
	// A Host of the listener still reaches the API's routes with the suffix set.
	for _, host := range []string{on, "localhost:7777", "ate-env-api.ate-env:7777"} {
		if code, body := get(http.DefaultClient, on, host, "/healthz"); code != http.StatusOK || body != "ok\n" {
			t.Errorf("suffix set, Host %s on /healthz: got %d %q, want 200", host, code, body)
		}
		if code, body := get(http.DefaultClient, on, host, "/v1alpha/envs/dev1/mcp"); code != http.StatusOK || body != "mcp\n" {
			t.Errorf("suffix set, Host %s on the MCP route: got %d %q, want 200", host, code, body)
		}
	}

	// gRPC is not checked for its authority, with one exception that the
	// suffix adds: an authority under the suffix names an environment port,
	// thus the call never reaches the gRPC server.
	check := func(addr, authority string) error {
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithAuthority(authority))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err = healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
		return err
	}
	for _, authority := range []string{"attacker.example:7777", "8000-env1.x" + suffix} {
		if err := check(on, authority); err != nil {
			t.Errorf("suffix set, gRPC with the authority %s: %v, want it served", authority, err)
		}
	}
	for _, authority := range []string{"8000-env1." + suffix, "api." + suffix, suffix} {
		if err := check(on, authority); err == nil {
			t.Errorf("suffix set, gRPC with the authority %s: served, want it kept from the gRPC server", authority)
		}
		if err := check(off, authority); err != nil {
			t.Errorf("no suffix, gRPC with the authority %s: %v, want it served", authority, err)
		}
	}
}
