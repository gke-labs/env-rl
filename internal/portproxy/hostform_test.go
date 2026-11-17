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

package portproxy_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/apiservice"
	"github.com/agent-substrate/env/internal/portproxy"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// hostSuffix is the -port-proxy-host-suffix of the tests.
const hostSuffix = "ports.example"

// seenRequest is one request as a daemon got it.
type seenRequest struct {
	uri, host, body string
	header          http.Header
}

// recorder is a daemon that records its requests and answers "daemon".
type recorder struct {
	mu   sync.Mutex
	reqs []seenRequest
}

func (d *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	d.mu.Lock()
	d.reqs = append(d.reqs, seenRequest{uri: r.RequestURI, host: r.Host, body: string(b), header: r.Header.Clone()})
	d.mu.Unlock()
	io.WriteString(w, "daemon")
}

// take returns the requests since the last call.
func (d *recorder) take() []seenRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.reqs
	d.reqs = nil
	return out
}

// hostFixture is a proxy behind Dispatch, in front of a stand-in for the API
// that counts its requests and answers "api".
type hostFixture struct {
	s    *setup
	p    *portproxy.Proxy
	addr string
	api  atomic.Int32
}

// hostSetup serves a proxy with what opts adds. suffix is its
// PortHostSuffix: hostSuffix in most tests, empty for the form turned off.
func hostSetup(t *testing.T, suffix string, opts portproxy.Options) *hostFixture {
	t.Helper()
	f := &hostFixture{s: newSetup(t)}
	opts.ConnectAddr = f.s.routerAddr
	opts.DefaultAtespace = "ate-env"
	opts.HandshakeTimeout = 5 * time.Second
	opts.PortHostSuffix = suffix
	if opts.WakeBudget == 0 {
		opts.WakeBudget = 2 * time.Second
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.api.Add(1)
		io.WriteString(w, "api")
	})
	f.p = mustNew(t, opts)
	srv := httptest.NewServer(portproxy.Dispatch(f.p, next))
	t.Cleanup(srv.Close)
	f.addr = strings.TrimPrefix(srv.URL, "http://")
	return f
}

// request writes one request on a fresh connection, with the Host line and
// the request-target exactly as given, and returns the whole answer.
func (f *hostFixture) request(t *testing.T, method, host, target string, headers ...string) answer {
	t.Helper()
	return f.requestBody(t, method, host, target, "", headers...)
}

// requestBody is request with a body.
func (f *hostFixture) requestBody(t *testing.T, method, host, target, body string, headers ...string) answer {
	t.Helper()
	c, err := net.Dial("tcp", f.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(20 * time.Second))
	req := method + " " + target + " HTTP/1.1\r\nHost: " + host + "\r\n"
	for _, h := range headers {
		req += h + "\r\n"
	}
	if body != "" {
		req += fmt.Sprintf("Content-Length: %d\r\n", len(body))
	}
	io.WriteString(c, req+"Connection: close\r\n\r\n"+body)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("%s %s (Host %s): %v", method, target, host, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	h := resp.Header.Clone()
	h.Del("Date")
	return answer{status: resp.StatusCode, header: h, body: string(b)}
}

// PP-4: the Host names the port, the id and, with one more label, the
// atespace. It is read without its port, without one trailing dot and
// without regard to ASCII case, and the port ends at the first '-'.
func TestHostFormForwardsToThePort(t *testing.T) {
	f := hostSetup(t, hostSuffix, portproxy.Options{})
	long := strings.Repeat("a", 58) // "8000-" + 58 is a label of 63 bytes
	daemons := map[string]*recorder{}
	for _, d := range []struct {
		id   string
		port int
	}{{"env1", 8000}, {"env1", 9000}, {"my-env-2", 8000}, {"9000-x", 8000}, {"xn--bcher-kva", 8000}, {long, 8000}, {"8000", 8000}} {
		r := &recorder{}
		daemons[fmt.Sprintf("%s:%d", d.id, d.port)] = r
		f.s.router.RegisterPort(d.id, d.port, r)
	}
	for _, c := range []struct {
		host   string
		ref    string // the actor reference the router gets
		daemon string
	}{
		{"8000-env1." + hostSuffix, "ate-env/env1", "env1:8000"},
		{"9000-env1." + hostSuffix, "ate-env/env1", "env1:9000"},
		{"8000-env1.team-a." + hostSuffix, "team-a/env1", "env1:8000"},
		{"8000-env1.ate-env." + hostSuffix, "ate-env/env1", "env1:8000"},
		// The port is the part before the first '-'; the rest is the id.
		{"8000-my-env-2." + hostSuffix, "ate-env/my-env-2", "my-env-2:8000"},
		{"8000-9000-x." + hostSuffix, "ate-env/9000-x", "9000-x:8000"},
		{"8000-8000." + hostSuffix, "ate-env/8000", "8000:8000"},
		// A Host is not case-sensitive.
		{"8000-ENV1.Team-A.PORTS.Example", "team-a/env1", "env1:8000"},
		// The port of the Host and one trailing dot are not part of the name.
		{"8000-env1." + hostSuffix + ":8443", "ate-env/env1", "env1:8000"},
		{"8000-env1." + hostSuffix + ".", "ate-env/env1", "env1:8000"},
		{"8000-env1.team-a." + hostSuffix + ".:8443", "team-a/env1", "env1:8000"},
		// An id or an atespace that reads as punycode is a label as any other.
		{"8000-xn--bcher-kva." + hostSuffix, "ate-env/xn--bcher-kva", "xn--bcher-kva:8000"},
		{"8000-env1.xn--bcher-kva." + hostSuffix, "xn--bcher-kva/env1", "env1:8000"},
		// The longest first label DNS can hold.
		{"8000-" + long + "." + hostSuffix, "ate-env/" + long, long + ":8000"},
	} {
		f.p.Close() // no idle tunnel: each request shows its CONNECT
		f.s.router.mu.Lock()
		f.s.router.connects = nil
		f.s.router.mu.Unlock()
		got := f.request(t, http.MethodGet, c.host, "/health?x=1")
		reqs := daemons[c.daemon].take()
		if got.status != http.StatusOK || got.body != "daemon" || len(reqs) != 1 {
			t.Errorf("Host %s: got %d %q and %d requests at the daemon, want its 200", c.host, got.status, got.body, len(reqs))
			continue
		}
		if reqs[0].uri != "/health?x=1" || !strings.HasPrefix(reqs[0].host, "127.0.0.1:") {
			t.Errorf("Host %s: the daemon got %q with Host %q, want /health?x=1 and a loopback Host", c.host, reqs[0].uri, reqs[0].host)
		}
		f.s.router.mu.Lock()
		connects := append([]string(nil), f.s.router.connects...)
		f.s.router.mu.Unlock()
		port := c.daemon[strings.LastIndex(c.daemon, ":"):]
		if len(connects) != 1 || !strings.HasPrefix(connects[0], c.ref+" ") || !strings.HasSuffix(connects[0], port) {
			t.Errorf("Host %s: the router got %q, want one CONNECT for %s port %s", c.host, connects, c.ref, port)
		}
	}
	if n := f.api.Load(); n != 0 {
		t.Errorf("the API got %d requests, want none", n)
	}
}

// PP-4: the whole path and the query reach the daemon as sent (PP-1, PP-2),
// with the answers the header form gives for the same request-target.
func TestHostFormKeepsPathAndQueryAsSent(t *testing.T) {
	f := hostSetup(t, hostSuffix, portproxy.Options{})
	d := &recorder{}
	f.s.router.RegisterPort("env1", 8000, d)
	for _, c := range []struct{ target, want string }{
		{"/", "/"},
		{"/a//b/../c?x=1", "/a//b/../c?x=1"},
		{"/a{b}", "/a{b}"},
		{"/caf\xc3\xa9", "/caf\xc3\xa9"},
		{"/a{b%2Fc", "/a{b%2Fc"},
		{"/files/report(1).txt", "/files/report(1).txt"},
		{"//a{b%2Fc", "//a%7Bb%2Fc"},
		{"//evil.example/x", "//evil.example/x"},
		{"/?", "/?"},
		{"/x?a=1;b=2", "/x?a=1;b=2"},
		{"/x?a=1&bad=%ZZ&b=2", "/x?a=1&bad=%ZZ&b=2"},
		{"/x?p=%2F&q={a}|b^c&u=caf\xc3\xa9#frag", "/x?p=%2F&q={a}|b^c&u=caf\xc3\xa9#frag"},
		{"//double-slash?;", "//double-slash?;"},
	} {
		got := f.request(t, http.MethodGet, "8000-env1."+hostSuffix, c.target)
		reqs := d.take()
		if got.status != http.StatusOK || len(reqs) != 1 || reqs[0].uri != c.want {
			t.Errorf("hostname form: sent %q, got %d, the daemon got %+v, want %q", c.target, got.status, reqs, c.want)
		}
		// The header form on a host of the API gives the daemon the same.
		got = f.request(t, http.MethodGet, f.addr, c.target, "X-Env-Id: env1", "X-Env-Port: 8000")
		reqs = d.take()
		if got.status != http.StatusOK || len(reqs) != 1 || reqs[0].uri != c.want {
			t.Errorf("header form: sent %q, got %d, the daemon got %+v, want %q", c.target, got.status, reqs, c.want)
		}
	}
}

// PP-5: without the suffix a Host of the hostname shape names no target.
// Each request gets the answer that the same request gets with a Host of the
// listener, which is the answer from before the flag existed: Dispatch looks
// at the headers and the path only.
func TestHostFormOffByDefault(t *testing.T) {
	f := hostSetup(t, "", portproxy.Options{})
	env1, env2 := &recorder{}, &recorder{}
	f.s.router.RegisterPort("env1", 8000, env1)
	f.s.router.RegisterPort("env2", 9000, env2)
	shaped := "8000-env1." + hostSuffix
	for _, c := range []struct {
		name, method, target string
		headers              []string
		status               int
		body                 string
		env1, env2, api      int
	}{
		{"no form", http.MethodGet, "/healthz", nil, http.StatusOK, "api", 0, 0, 1},
		{"header form", http.MethodGet, "/x", []string{"X-Env-Id: env2", "X-Env-Port: 9000"}, http.StatusOK, "daemon", 0, 1, 0},
		{"path form", http.MethodGet, "/v1alpha/envs/env2/ports/9000/x", nil, http.StatusOK, "daemon", 0, 1, 0},
		{"bad header form", http.MethodGet, "/x", []string{"X-Env-Port: 9000"}, http.StatusBadRequest, "", 0, 0, 0},
		{"gRPC", http.MethodPost, "/ateenv.v1alpha.EnvironmentService/Get", []string{"Content-Type: application/grpc", "Content-Length: 0"}, http.StatusOK, "api", 0, 0, 1},
	} {
		var answers []answer
		for _, host := range []string{f.addr, shaped, hostSuffix, "x.y.z." + hostSuffix} {
			f.api.Store(0)
			got := f.request(t, c.method, host, c.target, c.headers...)
			if got.status != c.status || (c.body != "" && got.body != c.body) {
				t.Errorf("%s with Host %s: got %d %q, want %d %q", c.name, host, got.status, got.body, c.status, c.body)
			}
			if a, b, n := len(env1.take()), len(env2.take()), int(f.api.Load()); a != c.env1 || b != c.env2 || n != c.api {
				t.Errorf("%s with Host %s: env1 got %d, env2 %d and the API %d requests; want %d, %d and %d", c.name, host, a, b, n, c.env1, c.env2, c.api)
			}
			answers = append(answers, got)
		}
		for i, a := range answers[1:] {
			if !a.equal(answers[0]) {
				t.Errorf("%s: answer %d is %+v, want the answer for the listener's Host, %+v", c.name, i+1, a, answers[0])
			}
		}
	}
	p := mustNew(t, portproxy.Options{ConnectAddr: f.s.routerAddr, DefaultAtespace: "ate-env"})
	if p.ServesHost(shaped) {
		t.Errorf("ServesHost(%q) without a suffix: true, want false", shaped)
	}
}

// badHosts are names under the suffix that name no environment port.
var badHosts = []string{
	// The suffix itself.
	hostSuffix,
	hostSuffix + ":7777",
	hostSuffix + ".",
	// More than two labels in front of the suffix.
	"8000-env1.a.b." + hostSuffix,
	"x.8000-env1.team-a." + hostSuffix,
	"8000-env1.8000-env1.team-a." + hostSuffix,
	// A first label without '-', such as a name the API could have had.
	"env1." + hostSuffix,
	"api." + hostSuffix,
	"8000." + hostSuffix,
	"env1.team-a." + hostSuffix,
	// A port that is not a canonical TCP port, or the guest's.
	"0-env1." + hostSuffix,
	"80-env1." + hostSuffix,
	"08000-env1." + hostSuffix,
	"65536-env1." + hostSuffix,
	"123456-env1." + hostSuffix,
	"http-env1." + hostSuffix,
	"-env1." + hostSuffix,
	"env1-8000." + hostSuffix,
	// A punycode first label: its "port" is "xn".
	"xn--8000-env1." + hostSuffix,
	// An id that is not a DNS label.
	"8000-." + hostSuffix,
	"8000--env1." + hostSuffix,
	"8000-env1-." + hostSuffix,
	"8000-env_1." + hostSuffix,
	"8000-*." + hostSuffix,
	// An atespace that is not a DNS label, the empty one included.
	"8000-env1.." + hostSuffix,
	"8000-env1.-a." + hostSuffix,
	"8000-env1.a_b." + hostSuffix,
	"8000-env1.*." + hostSuffix,
	// A first label of 64 bytes, one more than DNS can hold.
	"8000-" + strings.Repeat("a", 59) + "." + hostSuffix,
}

// PP-6: a Host that is the suffix or below it and names no environment port
// gets 400 that says what a name looks like. It reaches no daemon, no other
// form and no API route, whatever its headers and its path are.
func TestHostFormRefusesBadHosts(t *testing.T) {
	f := hostSetup(t, hostSuffix, portproxy.Options{})
	d := &recorder{}
	f.s.router.RegisterPort("env1", 8000, d)
	for _, host := range badHosts {
		for _, c := range []struct {
			name, target string
			headers      []string
		}{
			{"plain", "/healthz", nil},
			{"header form", "/x", []string{"X-Env-Id: env1", "X-Env-Port: 8000"}},
			{"path form", "/v1alpha/envs/env1/ports/8000/x", nil},
			{"gRPC", "/ateenv.v1alpha.EnvironmentService/Get", []string{"Content-Type: application/grpc"}},
		} {
			got := f.request(t, http.MethodGet, host, c.target, c.headers...)
			if got.status != http.StatusBadRequest || !strings.HasPrefix(got.body, "host ") || strings.Contains(got.body, "--allowed-hosts") {
				t.Errorf("Host %s, %s: got %d %q, want the port proxy's 400 about the host", host, c.name, got.status, got.body)
			}
			if n, api := len(d.take()), f.api.Swap(0); n != 0 || api != 0 {
				t.Errorf("Host %s, %s: the daemon got %d and the API %d requests, want none", host, c.name, n, api)
			}
		}
	}
	if n := f.s.router.connectCount(); n != 0 {
		t.Errorf("the router got %d CONNECTs, want none", n)
	}
	// A name that is not under the suffix is not this form's: the API serves
	// it here, and the Host check judges it in ate-env-api (see
	// TestHostFormListenerStack in internal/hostcheck).
	for _, host := range []string{"x" + hostSuffix, "8000-env1.x" + hostSuffix, "8000-env1." + hostSuffix + ".attacker.example", "8000-env1." + hostSuffix + ".."} {
		if got := f.request(t, http.MethodGet, host, "/healthz"); got.body != "api" {
			t.Errorf("Host %s: got %d %q, want the API", host, got.status, got.body)
		}
	}
}

// PP-6: a byte outside ASCII never passes, also a rune that Unicode case
// folding maps to an ASCII letter (U+212A KELVIN SIGN lowers to "k"). Go's
// HTTP/1.x server refuses such a Host before any handler, thus the test
// calls the handler directly, as a server that let it through would.
func TestHostFormRefusesNonASCIIHost(t *testing.T) {
	s := newSetup(t)
	d := &recorder{}
	s.router.RegisterPort("envk", 8000, d)
	s.router.RegisterPort("env1", 8000, d)
	api := 0
	p := mustNew(t, portproxy.Options{ConnectAddr: s.routerAddr, DefaultAtespace: "ate-env", PortHostSuffix: "ports.kiwi"})
	h := portproxy.Dispatch(p, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { api++ }))
	for _, host := range []string{
		"8000-env\u212a.ports.kiwi",      // KELVIN SIGN: the id would lower to "envk"
		"8000-env1.\u212a.ports.kiwi",    // the atespace would lower to "k"
		"8000-b\u00fccher.ports.kiwi",    // a U-label
		"8000\uff0denv1.ports.kiwi",      // a fullwidth hyphen
		"\uff18000-env1.ports.kiwi",      // a fullwidth digit
		"8000-env1\u3002team.ports.kiwi", // an ideographic full stop between labels
	} {
		if p.ServesHost(host) {
			t.Errorf("ServesHost(%q) = true, want false", host)
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = host
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("Host %q: got %d %q, want 400", host, rec.Code, rec.Body.String())
		}
	}
	// The same rune in the suffix part: the name is not under the suffix,
	// thus not this form's.
	for _, host := range []string{"8000-env1.ports.\u212aiwi", "8000-env1.ports.kiwi\u3002"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = host
		h.ServeHTTP(httptest.NewRecorder(), req)
		if p.ServesHost(host) || api != 1 {
			t.Errorf("Host %q: ServesHost %v and %d requests at the next handler, want false and 1", host, p.ServesHost(host), api)
		}
		api = 0
	}
	if n := len(d.take()); n != 0 || api != 0 {
		t.Errorf("the daemon got %d and the API %d requests, want none", n, api)
	}
}

// PP-6: ServesHost, the question the Host check asks, is true exactly for
// the names that name a port.
func TestParseHost(t *testing.T) {
	p := mustNew(t, portproxy.Options{ConnectAddr: "127.0.0.1:1", DefaultAtespace: "ate-env", PortHostSuffix: "Ports.Example."})
	for _, host := range []string{
		"8000-env1." + hostSuffix,
		"8000-env1.team-a." + hostSuffix,
		"1-a." + hostSuffix,
		"65535-a.b." + hostSuffix,
		"8000-ENV1." + strings.ToUpper(hostSuffix) + ".:443",
		"8000-" + strings.Repeat("a", 58) + "." + hostSuffix,
	} {
		if !p.ServesHost(host) {
			t.Errorf("ServesHost(%q) = false, want true", host)
		}
	}
	for _, host := range append([]string{
		"", "localhost", "127.0.0.1:7777", "[::1]:7777", "attacker.example",
		"x" + hostSuffix, "8000-env1.x" + hostSuffix, "8000-env1." + hostSuffix + ".attacker.example",
		"8000-env1." + hostSuffix + "..", "8000-env1.ports", "8000-env1.example",
		"8000-env1." + hostSuffix + ":80:80", "[8000-env1." + hostSuffix + "]",
		// Brackets hold an IP literal; SplitHostPort would take them off.
		"[8000-env1." + hostSuffix + "]:7777", "[8000-env1.team-a." + hostSuffix + ".]:7777",
		"8000-env1." + hostSuffix + "/x", "user@8000-env1." + hostSuffix, " 8000-env1." + hostSuffix,
	}, badHosts...) {
		if p.ServesHost(host) {
			t.Errorf("ServesHost(%q) = true, want false", host)
		}
	}
}

// PP-12: the suffix is a domain name. New fails for another value, thus
// ate-env-api does not start with it.
func TestNormalizeHostSuffix(t *testing.T) {
	for in, want := range map[string]string{
		"ports.example":         "ports.example",
		" Ports.Example. ":      "ports.example",
		"ports.localhost":       "ports.localhost",
		"example.com":           "example.com",
		"co.uk":                 "co.uk", // no public-suffix list: only a suffix of one label is refused
		"a-b.c1.example":        "a-b.c1.example",
		"xn--bcher-kva.example": "xn--bcher-kva.example",
	} {
		if got, err := portproxy.NormalizeHostSuffix(in); err != nil || got != want {
			t.Errorf("NormalizeHostSuffix(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"", " ", ".", "*.ports.example", ".ports.example", "ports..example", "ports.example..",
		"http://ports.example", "ports.example:443", "ports.example/x", "ports_x.example",
		"-ports.example", "ports-.example", "10.0.0.1", "::1", "b\u00fccher.example",
		"ports.e\u212aample", strings.Repeat("a", 64) + ".example",
		strings.Repeat("a.", 127) + "example",
		// One label: each <port>-<id>.com would pass the Host check.
		"com", "COM.", "sandbox", "localhost", "xn--p1ai",
	} {
		if got, err := portproxy.NormalizeHostSuffix(bad); err == nil {
			t.Errorf("NormalizeHostSuffix(%q) = %q, want an error", bad, got)
		}
		if bad = strings.TrimSpace(bad); bad == "" {
			continue // an empty Options.PortHostSuffix is the form turned off
		}
		if _, err := portproxy.New(portproxy.Options{ConnectAddr: "127.0.0.1:1", PortHostSuffix: bad}); err == nil {
			t.Errorf("New with the host suffix %q: want an error", bad)
		}
	}
}

// PP-13: a suffix that is a name of the API, or above one, is refused: the
// API would answer 400 there. A suffix next to the names, or below a
// wildcard of the Host check, passes.
func TestCheckHostSuffix(t *testing.T) {
	apiHosts := []string{
		"localhost", "ate-env-api", "ate-env-api.ate-env", "ate-env-api.ate-env.svc",
		"ate-env-api.ate-env.svc.cluster.local", "LB.Internal.Example.", "*.corp.example",
	}
	for in, want := range map[string]string{
		"ports.example":          "ports.example",
		"Ports.Localhost.":       "ports.localhost",
		"ports.internal.example": "ports.internal.example", // next to the load balancer's name
		"ports.corp.example":     "ports.corp.example",     // below the wildcard
		"env.svc.cluster.local":  "env.svc.cluster.local",
	} {
		if got, err := portproxy.CheckHostSuffix(in, apiHosts); err != nil || got != want {
			t.Errorf("CheckHostSuffix(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"localhost", "ate-env-api", "ate-env", "svc", "ate-env.svc", "cluster.local", "svc.cluster.local",
		"ate-env-api.ate-env.svc.cluster.local", "local", "Cluster.Local.",
		"lb.internal.example", "internal.example", "example", "corp.example",
		"https://ports.example", "", "api",
	} {
		if got, err := portproxy.CheckHostSuffix(bad, apiHosts); err == nil {
			t.Errorf("CheckHostSuffix(%q) = %q, want an error", bad, got)
		}
	}
	if got, err := portproxy.CheckHostSuffix("cluster.local", nil); err != nil || got != "cluster.local" {
		t.Errorf("CheckHostSuffix without API names = %q, %v; want the suffix", got, err)
	}
}

// PP-13: the names of the API that a suffix must not cover are localhost,
// each in-cluster name of the Service and each name of --allowed-hosts.
// ate-env-api and ate-env manifest both take the list from APIHosts.
func TestAPIHosts(t *testing.T) {
	service := []string{"ate-env-api", "ate-env-api.ate-env", "ate-env-api.ate-env.svc", "ate-env-api.ate-env.svc.cluster.local"}
	allowed := []string{"lb.internal.example", "*.corp.example"}
	got := portproxy.APIHosts(service, allowed)
	want := append(append([]string{"localhost"}, service...), allowed...)
	if !slices.Equal(got, want) {
		t.Fatalf("APIHosts = %q, want %q", got, want)
	}
	if got := portproxy.APIHosts(nil, nil); !slices.Equal(got, []string{"localhost"}) {
		t.Errorf("APIHosts without names = %q, want localhost alone", got)
	}
	// Each kind of name refuses the suffix that covers it.
	for _, suffix := range []string{"ports.localhost", "ate-env.svc", "svc.cluster.local", "internal.example", "corp.example"} {
		_, err := portproxy.CheckHostSuffix(suffix, portproxy.APIHosts(service, allowed))
		if suffix == "ports.localhost" {
			if err != nil {
				t.Errorf("CheckHostSuffix(%q): %v; a name below localhost is not a name of the API", suffix, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("CheckHostSuffix(%q) with the API names: want an error", suffix)
		}
	}
}

// PP-7: under the suffix the Host alone names the target. A routing header
// of the header form is refused, a path of the path form's shape is the
// daemon's path, and no request reaches the API, gRPC included.
func TestHostFormTakesExactlyOneForm(t *testing.T) {
	f := hostSetup(t, hostSuffix, portproxy.Options{})
	env1, env2 := &recorder{}, &recorder{}
	f.s.router.RegisterPort("env1", 8000, env1)
	f.s.router.RegisterPort("env2", 9000, env2)
	host := "8000-env1." + hostSuffix

	for _, headers := range [][]string{
		{"X-Env-Id: env2", "X-Env-Port: 9000"},
		{"X-Env-Id: env1", "X-Env-Port: 8000"}, // the same target is refused too
		{"X-Env-Id: env2"},
		{"X-Env-Atespace: team-a"},
		{"X-Env-Port: 9000"},
		{"X-Env-Port: 9000", "Content-Type: application/grpc"},
	} {
		got := f.request(t, http.MethodGet, host, "/x", headers...)
		if got.status != http.StatusBadRequest || !strings.Contains(got.body, "X-Env-") {
			t.Errorf("%q next to the Host: got %d %q, want 400 that names the header", headers, got.status, got.body)
		}
		if a, b := len(env1.take()), len(env2.take()); a != 0 || b != 0 {
			t.Errorf("%q next to the Host: env1 got %d and env2 %d requests, want none", headers, a, b)
		}
	}

	for _, c := range []struct {
		method, target string
		headers        []string
	}{
		// The path form's shape, for another environment: not read.
		{http.MethodGet, "/v1alpha/envs/env2/ports/9000/x?y=1", nil},
		{http.MethodGet, "/v1alpha/atespaces/team-a/envs/env2/ports/9000", nil},
		// A malformed path form is not read either, thus not refused.
		{http.MethodGet, "/v1alpha/envs/Env2/ports/0/x", nil},
		// The API's own routes.
		{http.MethodGet, "/healthz", nil},
		{http.MethodPost, "/v1alpha/envs/env2/mcp", []string{"Content-Type: application/json", "Content-Length: 0"}},
		{http.MethodPost, "/ateenv.v1alpha.EnvironmentService/Get", []string{"Content-Type: application/grpc", "Content-Length: 0"}},
		// These two headers do not name a target.
		{http.MethodGet, "/x", []string{"X-Env-Authorization: Bearer any", "X-Env-Wake-Wait: 0"}},
	} {
		got := f.request(t, c.method, host, c.target, c.headers...)
		reqs := env1.take()
		if got.status != http.StatusOK || got.body != "daemon" || len(reqs) != 1 || reqs[0].uri != c.target {
			t.Errorf("%s %s: got %d %q, env1 got %+v; want it to get the request-target whole", c.method, c.target, got.status, got.body, reqs)
		}
		if n := len(env2.take()); n != 0 {
			t.Errorf("%s %s: env2 got %d requests, want none", c.method, c.target, n)
		}
	}
	if n := f.api.Load(); n != 0 {
		t.Errorf("the API got %d requests, want none", n)
	}
}

// PP-9: the caller check of the other forms runs before anything is
// forwarded, and the daemon gets neither the API credential nor an X-Env-*
// header.
func TestHostFormAuthenticates(t *testing.T) {
	auth, err := apiservice.NewBearerAuth("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	f := hostSetup(t, hostSuffix, portproxy.Options{Authenticate: auth.Check, AuthenticateCapability: auth.CheckCapability})
	d := &recorder{}
	f.s.router.RegisterPort("env1", 8000, d)
	host := "8000-env1." + hostSuffix

	for name, headers := range map[string][]string{
		"no credential":               nil,
		"a wrong token":               {"Authorization: Bearer wrong"},
		"a wrong X-Env-Authorization": {"X-Env-Authorization: Bearer wrong", "Authorization: Bearer s3cret"},
		"Basic with the token":        {"Authorization: Basic " + "czNjcmV0"},
		"the token in the query":      nil,
	} {
		target := "/run"
		if name == "the token in the query" {
			target = "/run?token=s3cret&access_token=s3cret"
		}
		got := f.request(t, http.MethodPost, host, target, append(headers, "Content-Length: 0")...)
		if got.status != http.StatusUnauthorized || got.header.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s: got %d (WWW-Authenticate %q), want 401 Bearer", name, got.status, got.header.Get("WWW-Authenticate"))
		}
	}
	if n, connects := len(d.take()), f.s.router.connectCount(); n != 0 || connects != 0 {
		t.Fatalf("without a valid credential the daemon got %d requests and the router %d CONNECTs, want none", n, connects)
	}

	got := f.request(t, http.MethodGet, host, "/x", "Authorization: Bearer s3cret", "X-Env-Wake-Wait: 0")
	reqs := d.take()
	if got.status != http.StatusOK || len(reqs) != 1 {
		t.Fatalf("the token in Authorization: got %d and %d requests at the daemon, want its 200", got.status, len(reqs))
	}
	if v := reqs[0].header.Get("Authorization"); v != "" {
		t.Errorf("the daemon got Authorization %q, want the API's token removed", v)
	}

	got = f.request(t, http.MethodGet, host, "/x", "X-Env-Authorization: Bearer s3cret", "Authorization: Bearer daemon-key", "X-Env-Wake-Wait: 0")
	reqs = d.take()
	if got.status != http.StatusOK || len(reqs) != 1 {
		t.Fatalf("the token in X-Env-Authorization: got %d and %d requests at the daemon, want its 200", got.status, len(reqs))
	}
	if v := reqs[0].header.Get("Authorization"); v != "Bearer daemon-key" {
		t.Errorf("the daemon got Authorization %q, want its own credential", v)
	}
	for k := range reqs[0].header {
		if strings.HasPrefix(k, "X-Env-") {
			t.Errorf("the daemon got %s, want no X-Env-* header", k)
		}
	}
	if v := reqs[0].header.Get("X-Forwarded-Host"); v != host {
		t.Errorf("the daemon got X-Forwarded-Host %q, want %q", v, host)
	}
}

// PP-9: a port capability opens only a WebSocket handshake, and only for the
// atespace, the id and the port that the Host names.
func TestHostFormCapability(t *testing.T) {
	auth, err := apiservice.NewBearerAuth("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	f := hostSetup(t, hostSuffix, portproxy.Options{Authenticate: auth.Check, AuthenticateCapability: auth.CheckCapability})
	d := &recorder{}
	for _, id := range []string{"env1", "env2"} {
		for _, port := range []int{8000, 9000} {
			f.s.router.RegisterPort(id, port, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Upgrade") == "" {
					d.ServeHTTP(w, r)
					return
				}
				d.mu.Lock()
				d.reqs = append(d.reqs, seenRequest{uri: r.RequestURI, header: r.Header.Clone()})
				d.mu.Unlock()
				w.Header().Set("Connection", "Upgrade")
				w.Header().Set("Upgrade", "websocket")
				w.WriteHeader(http.StatusSwitchingProtocols)
			}))
		}
	}
	capability := basic(portproxy.Capability("s3cret", "ate-env", "env1", 8000, time.Now().Add(time.Hour)))
	upgrade := []string{"Connection: Upgrade", "Upgrade: websocket", "Authorization: " + capability}

	for _, host := range []string{"8000-env1." + hostSuffix, "8000-env1.ate-env." + hostSuffix} {
		got := f.request(t, http.MethodGet, host, "/sockets/events", upgrade...)
		reqs := d.take()
		if got.status != http.StatusSwitchingProtocols || len(reqs) != 1 {
			t.Fatalf("Host %s: a handshake with the capability got %d and %d requests at the daemon, want 101", host, got.status, len(reqs))
		}
		if v := reqs[0].header.Get("Authorization"); v != "" {
			t.Errorf("Host %s: the daemon got Authorization %q, want the capability removed", host, v)
		}
	}
	for name, c := range map[string]struct {
		host    string
		headers []string
	}{
		"a plain GET":        {"8000-env1." + hostSuffix, []string{"Authorization: " + capability}},
		"another port":       {"9000-env1." + hostSuffix, upgrade},
		"another id":         {"8000-env2." + hostSuffix, upgrade},
		"another atespace":   {"8000-env1.team-a." + hostSuffix, upgrade},
		"an Upgrade to h2c":  {"8000-env1." + hostSuffix, []string{"Connection: Upgrade", "Upgrade: h2c", "Authorization: " + capability}},
		"a forged signature": {"8000-env1." + hostSuffix, []string{"Connection: Upgrade", "Upgrade: websocket", "Authorization: " + basic("v1.9999999999.00")}},
	} {
		if got := f.request(t, http.MethodGet, c.host, "/sockets/events", c.headers...); got.status != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", name, got.status)
		}
		if n := len(d.take()); n != 0 {
			t.Errorf("%s: the daemon got %d requests, want none", name, n)
		}
	}
	// PP-10: a valid capability does not pass the origin check. A browser
	// that kept one from a URL sends it on a handshake that a page of another
	// environment port, or of another site, starts.
	for _, origin := range []string{"https://8000-env2." + hostSuffix, "https://evil.example"} {
		got := f.request(t, http.MethodGet, "8000-env1."+hostSuffix, "/sockets/events", append([]string{"Origin: " + origin}, upgrade...)...)
		if n := len(d.take()); got.status != http.StatusForbidden || n != 0 {
			t.Errorf("a handshake with the capability from %s: got %d and %d requests at the daemon, want 403 and none", origin, got.status, n)
		}
	}
	got := f.request(t, http.MethodGet, "8000-env1."+hostSuffix, "/sockets/events", append([]string{"Origin: https://8000-env1." + hostSuffix}, upgrade...)...)
	if n := len(d.take()); got.status != http.StatusSwitchingProtocols || n != 1 {
		t.Errorf("a handshake with the capability from the port's own origin: got %d and %d requests at the daemon, want 101 and one", got.status, n)
	}
}

// PP-9: -port-proxy-atespaces limits the hostname form as it limits the
// others, the default atespace of the short name included, after the caller
// check.
func TestHostFormAtespacesLimit(t *testing.T) {
	auth, err := apiservice.NewBearerAuth("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	f := hostSetup(t, hostSuffix, portproxy.Options{Authenticate: auth.Check, Atespaces: []string{"team-a"}})
	d := &recorder{}
	f.s.router.RegisterPort("env1", 8000, d)
	token := "Authorization: Bearer s3cret"
	for _, c := range []struct {
		host    string
		headers []string
		status  int
		served  int
	}{
		{"8000-env1.team-a." + hostSuffix, []string{token}, http.StatusOK, 1},
		{"8000-env1." + hostSuffix, []string{token}, http.StatusForbidden, 0}, // ate-env is not listed
		{"8000-env1.ate-env." + hostSuffix, []string{token}, http.StatusForbidden, 0},
		{"8000-env1.team-b." + hostSuffix, []string{token}, http.StatusForbidden, 0},
		// The caller check comes first: no token, no word on the atespaces.
		{"8000-env1.team-b." + hostSuffix, nil, http.StatusUnauthorized, 0},
	} {
		got := f.request(t, http.MethodGet, c.host, "/", c.headers...)
		if n := len(d.take()); got.status != c.status || n != c.served {
			t.Errorf("Host %s with %q: got %d and %d requests at the daemon, want %d and %d", c.host, c.headers, got.status, n, c.status, c.served)
		}
	}
}

// PP-10: a browser page of another origin cannot drive a daemon through the
// hostname form: no state-changing request, no WebSocket handshake. Another
// environment port under the same suffix is another origin, although the
// same site, and so is the API's own host.
func TestHostFormRefusesCrossOrigin(t *testing.T) {
	f := hostSetup(t, hostSuffix, portproxy.Options{TrustedOrigins: []string{"https://ui.example"}})
	f.s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "" {
			w.Header().Set("Connection", "Upgrade")
			w.Header().Set("Upgrade", "echo")
			w.WriteHeader(http.StatusSwitchingProtocols)
			return
		}
		io.WriteString(w, "daemon")
	}))
	host := "8000-env1." + hostSuffix
	own := "Origin: https://" + host
	sibling := "Origin: https://8000-env2." + hostSuffix
	otherPort := "Origin: https://9000-env1." + hostSuffix
	otherAtespace := "Origin: https://8000-env1.team-a." + hostSuffix
	upgrade := []string{"Connection: Upgrade", "Upgrade: echo"}
	for _, c := range []struct {
		name    string
		method  string
		host    string
		headers []string
		want    int
	}{
		{"cross-site POST", http.MethodPost, host, []string{"Sec-Fetch-Site: cross-site", "Origin: https://evil.example"}, http.StatusForbidden},
		{"foreign Origin POST", http.MethodPost, host, []string{"Origin: https://evil.example"}, http.StatusForbidden},
		{"another environment, same-site POST", http.MethodPost, host, []string{"Sec-Fetch-Site: same-site", sibling}, http.StatusForbidden},
		{"another environment, Origin only", http.MethodPost, host, []string{sibling}, http.StatusForbidden},
		{"another environment, DELETE", http.MethodDelete, host, []string{"Sec-Fetch-Site: same-site", sibling}, http.StatusForbidden},
		{"another port of the environment", http.MethodPost, host, []string{"Sec-Fetch-Site: same-site", otherPort}, http.StatusForbidden},
		{"the id in another atespace", http.MethodPost, host, []string{otherAtespace}, http.StatusForbidden},
		{"the suffix itself", http.MethodPost, host, []string{"Origin: https://" + hostSuffix}, http.StatusForbidden},
		{"the API's host", http.MethodPost, host, []string{"Origin: http://" + f.addr}, http.StatusForbidden},
		{"another environment, upgrade", http.MethodGet, host, append([]string{"Sec-Fetch-Site: same-site", sibling}, upgrade...), http.StatusForbidden},
		{"another environment, upgrade with Origin only", http.MethodGet, host, append([]string{sibling}, upgrade...), http.StatusForbidden},
		{"cross-site upgrade", http.MethodGet, host, append([]string{"Sec-Fetch-Site: cross-site"}, upgrade...), http.StatusForbidden},
		// Reads pass the check, as in the path form; the caller check still
		// applies to them, and a page cannot read the answer of another origin.
		{"another environment, GET", http.MethodGet, host, []string{"Sec-Fetch-Site: same-site", sibling}, http.StatusOK},
		{"no browser headers", http.MethodPost, host, nil, http.StatusOK},
		{"own origin POST", http.MethodPost, host, []string{own}, http.StatusOK},
		{"own origin with a port", http.MethodPost, host + ":8443", []string{own + ":8443"}, http.StatusOK},
		{"same-origin POST", http.MethodPost, host, []string{"Sec-Fetch-Site: same-origin", own}, http.StatusOK},
		{"same-origin upgrade", http.MethodGet, host, append([]string{"Sec-Fetch-Site: same-origin"}, upgrade...), http.StatusSwitchingProtocols},
		{"trusted origin POST", http.MethodPost, host, []string{"Sec-Fetch-Site: cross-site", "Origin: https://ui.example"}, http.StatusOK},
	} {
		if got := f.request(t, c.method, c.host, "/run", append(c.headers, "Content-Length: 0")...); got.status != c.want {
			t.Errorf("%s: got %d %q, want %d", c.name, got.status, got.body, c.want)
		}
	}
}

// PP-11: X-Env-Wake-Wait holds a hostname-form request while its environment
// wakes and does not reach the daemon (PP-3).
func TestHostFormWakeWait(t *testing.T) {
	var awake atomic.Bool
	k := &wake{}
	f := hostSetup(t, hostSuffix, portproxy.Options{
		ActorState: fixedState(ateapipb.ActorState_ACTOR_STATE_RESUMING),
		Wake:       k.Wake, WakeWaitCap: time.Minute, WakeBudget: 30 * time.Second,
	})
	k.fn = func(ctx context.Context, atespace, id string) error {
		if atespace != "team-a" || id != "waking" {
			t.Errorf("wake wait for %s/%s, want team-a/waking", atespace, id)
		}
		if n := f.s.router.connectCount(); n != 0 {
			t.Errorf("%d CONNECTs before the wake wait returned, want 0", n)
		}
		time.Sleep(300 * time.Millisecond)
		awake.Store(true)
		return nil
	}
	f.s.router.Running = func(string) bool { return awake.Load() }
	d := &recorder{}
	f.s.router.RegisterPort("waking", 8000, d)
	host := "8000-waking.team-a." + hostSuffix

	start := time.Now()
	got := f.requestBody(t, http.MethodPost, host, "/run", "the whole body", "X-Env-Wake-Wait: 30")
	reqs := d.take()
	if got.status != http.StatusOK || got.body != "daemon" || len(reqs) != 1 {
		t.Fatalf("got %d %q and %d requests at the daemon, want its 200 once", got.status, got.body, len(reqs))
	}
	if took := time.Since(start); took < 300*time.Millisecond {
		t.Errorf("answered after %s, before the wake", took)
	}
	if reqs[0].body != "the whole body" {
		t.Errorf("the daemon got the body %q, want it whole", reqs[0].body)
	}
	if v, ok := reqs[0].header[portproxy.EnvWakeWaitHeader]; ok {
		t.Errorf("the daemon got %s %q, want it consumed", portproxy.EnvWakeWaitHeader, v)
	}
	if n := k.calls.Load(); n != 1 {
		t.Errorf("the wake wait ran %d times, want 1", n)
	}

	// A hold that ends while the environment still wakes gets the 503 of a
	// request without the header; an invalid value gets 400.
	awake.Store(false)
	k.fn = nil
	f.s.router.EnvoyTimeout = 200 * time.Millisecond
	want := stillWaking
	want.body = "environment team-a/waking port 8000 is still waking; retry\n"
	want.header = stillWaking.header.Clone()
	want.header.Set("Content-Length", fmt.Sprint(len(want.body)))
	for _, headers := range [][]string{nil, {"X-Env-Wake-Wait: 1"}} {
		if got := f.request(t, http.MethodGet, host, "/", headers...); !got.equal(want) {
			t.Errorf("%q while the environment wakes: got %+v, want %+v", headers, got, want)
		}
	}
	if got := f.request(t, http.MethodGet, host, "/", "X-Env-Wake-Wait: soon"); got.status != http.StatusBadRequest {
		t.Errorf("an invalid %s: got %d, want 400", portproxy.EnvWakeWaitHeader, got.status)
	}
	if n := len(d.take()); n != 0 {
		t.Errorf("the daemon got %d requests while its environment did not run, want none", n)
	}
}

// PP-11: an environment that is crashed, deleting, gone or waking gets, in
// the hostname form, the answer that the header form gives for it, header
// for header and byte for byte.
func TestHostFormAnswersForAnEnvironmentThatDoesNotServe(t *testing.T) {
	for _, tc := range []struct {
		name     string
		denyCode int
		denyBody string
		envoy    time.Duration
		state    ateapipb.ActorState
		err      error
		status   int
		header   string
	}{
		{"crashed", 503, routerBody("broken", "CRASHED"), 0, ateapipb.ActorState_ACTOR_STATE_CRASHED, nil, http.StatusGone, "CRASHED"},
		{"deleting", 503, routerBody("broken", "DELETING"), 0, ateapipb.ActorState_ACTOR_STATE_DELETING, nil, http.StatusNotFound, "DELETING"},
		{"deleted after the 503", 503, routerBody("broken", "DELETING"), 0, 0, status.Error(codes.NotFound, "environment not found"), http.StatusNotFound, ""},
		{"deleted", 404, "actor ate-env/broken not found", 0, 0, status.Error(codes.NotFound, "environment not found"), http.StatusNotFound, ""},
		{"waking", 0, "", 200 * time.Millisecond, ateapipb.ActorState_ACTOR_STATE_RESUMING, nil, http.StatusServiceUnavailable, "RESUMING"},
	} {
		f := hostSetup(t, hostSuffix, portproxy.Options{
			WakeBudget: 30 * time.Second,
			ActorState: func(context.Context, string, string) (ateapipb.ActorState, error) { return tc.state, tc.err },
		})
		d := &recorder{}
		f.s.router.RegisterPort("broken", 8000, d)
		if tc.denyCode != 0 {
			f.s.router.Deny = func(string) (int, string) { return tc.denyCode, tc.denyBody }
		} else {
			f.s.router.Running = func(string) bool { return false }
			f.s.router.EnvoyTimeout = tc.envoy
		}
		byHost := f.request(t, http.MethodGet, "8000-broken."+hostSuffix, "/")
		byHeader := f.request(t, http.MethodGet, f.addr, "/", "X-Env-Id: broken", "X-Env-Port: 8000")
		if byHost.status != tc.status || byHost.header.Get(portproxy.EnvStateHeader) != tc.header {
			t.Errorf("%s: got %d with %s %q, want %d with %q", tc.name, byHost.status, portproxy.EnvStateHeader, byHost.header.Get(portproxy.EnvStateHeader), tc.status, tc.header)
		}
		if !byHost.equal(byHeader) {
			t.Errorf("%s: the hostname form got %+v, the header form %+v; want the same answer", tc.name, byHost, byHeader)
		}
		if n := len(d.take()); n != 0 {
			t.Errorf("%s: the daemon got %d requests, want none", tc.name, n)
		}
	}
}

// PP-11: an HTTP upgrade (what a WebSocket handshake is) passes, after which
// bytes flow both ways, and a streamed response reaches the caller as the
// daemon writes it.
func TestHostFormUpgradeAndStreaming(t *testing.T) {
	f := hostSetup(t, hostSuffix, portproxy.Options{})
	release := make(chan struct{})
	f.s.router.RegisterPort("env1", 8000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/events" {
			io.WriteString(w, "first\n")
			w.(http.Flusher).Flush()
			<-release
			io.WriteString(w, "second\n")
			return
		}
		if r.Header.Get("Upgrade") != "echo" {
			http.Error(w, "want Upgrade: echo", http.StatusBadRequest)
			return
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		rw.Flush()
		buf := make([]byte, 4)
		if _, err := io.ReadFull(rw, buf); err == nil {
			rw.Write(append([]byte("re:"), buf...))
			rw.Flush()
		}
	}))
	host := "8000-env1." + hostSuffix

	conn, err := net.Dial("tcp", f.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprint(conn, "GET /sockets/events/c1 HTTP/1.1\r\nHost: "+host+"\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade answered %v, %v", resp, err)
	}
	conn.Write([]byte("ping"))
	got := make([]byte, 7)
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "re:ping" {
		t.Errorf("after the upgrade got %q, %v; want re:ping", got, err)
	}

	req, _ := http.NewRequest(http.MethodGet, "http://"+f.addr+"/events", nil)
	req.Host = host
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	buf := make([]byte, len("first\n"))
	if _, err := io.ReadFull(stream.Body, buf); err != nil || string(buf) != "first\n" {
		t.Fatalf("first chunk %q, %v: the proxy must flush before the daemon finishes", buf, err)
	}
	close(release)
	if rest, _ := io.ReadAll(stream.Body); string(rest) != "second\n" {
		t.Errorf("rest %q", rest)
	}
}
