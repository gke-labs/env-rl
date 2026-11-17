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

package fleetsite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/inventory"
)

type fakeEvents struct {
	uid   string
	since time.Time
	out   []Event
	err   error
}

func (f *fakeEvents) ActorEvents(_ context.Context, uid string, since time.Time) ([]Event, error) {
	f.uid, f.since = uid, since
	return f.out, f.err
}

// headerAuth admits a request whose X-Test-User header names a user.
func headerAuth(r *http.Request) (string, error) {
	if u := r.Header.Get("X-Test-User"); u != "" {
		return u, nil
	}
	return "", errors.New("no user")
}

type testSite struct {
	srv    *Server
	dir    string
	clock  *fakeClock
	events *fakeEvents
}

func newTestSite(t *testing.T) *testSite {
	t.Helper()
	ts := &testSite{dir: t.TempDir(), clock: &fakeClock{t: t0}, events: &fakeEvents{}}
	srv, err := New(Config{
		Snapshots: NewSnapshots(DirStore{Dir: ts.dir}, ts.clock.now),
		Events:    ts.events,
		Auth:      headerAuth,
		Now:       ts.clock.now,
		Version:   "v-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	ts.srv = srv
	return ts
}

func (ts *testSite) get(t *testing.T, path string) (*http.Response, string) {
	t.Helper()
	return ts.do(t, http.MethodGet, path, "member@example.com")
}

func (ts *testSite) do(t *testing.T, method, path, user string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	rec := httptest.NewRecorder()
	ts.srv.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Result(), string(body)
}

// FS-7: without an identity every route but /healthz is denied.
func TestSiteDeniesWithoutIdentity(t *testing.T) {
	ts := newTestSite(t)
	for _, p := range []string{"/", "/clusters/p/c", "/api/v1/clusters", "/api/v1/clusters/p/c", "/api/v1/actors/u/events", "/static/site.css", "/nope"} {
		resp, body := ts.do(t, http.MethodGet, p, "")
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s without identity: %d %q, want 403", p, resp.StatusCode, body)
		}
	}
	if resp, _ := ts.do(t, http.MethodGet, "/healthz", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz: %d", resp.StatusCode)
	}
}

// The site serves its one stylesheet and no directory listing.
func TestSiteStatic(t *testing.T) {
	ts := newTestSite(t)
	resp, body := ts.get(t, "/static/site.css")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/css") || !strings.Contains(body, ".badge") {
		t.Errorf("/static/site.css: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, p := range []string{"/static/", "/static/other.css", "/static/../templates/layout.html"} {
		if resp, body := ts.get(t, p); resp.StatusCode == http.StatusOK {
			t.Errorf("%s: 200 %q", p, body)
		}
	}
}

// FS-3: the site serves GET and HEAD only.
func TestSiteRefusesWrites(t *testing.T) {
	ts := newTestSite(t)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, "TRACE"} {
		resp, _ := ts.do(t, m, "/api/v1/clusters", "member@example.com")
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d, want 405", m, resp.StatusCode)
		}
	}
	if resp, _ := ts.do(t, http.MethodHead, "/", "member@example.com"); resp.StatusCode != http.StatusOK {
		t.Errorf("HEAD /: %d", resp.StatusCode)
	}
}

func TestSiteSecurityHeaders(t *testing.T) {
	ts := newTestSite(t)
	resp, _ := ts.get(t, "/")
	for k, want := range map[string]string{
		"Content-Security-Policy": "default-src 'none'",
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Cache-Control":           "no-store",
		"Referrer-Policy":         "no-referrer",
	} {
		if got := resp.Header.Get(k); !strings.Contains(got, want) {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if csp := resp.Header.Get("Content-Security-Policy"); strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "script-src") {
		t.Errorf("CSP %q allows scripts", csp)
	}
}

// FS-1, FS-7, FS-9: the Fleet page lists each cluster, shows the user's
// email, and marks a cluster stale after three intervals.
func TestFleetPage(t *testing.T) {
	ts := newTestSite(t)
	writeSnapshot(t, ts.dir, golden(t, "proj-a", "fresh", t0.Add(-2*time.Second)))
	writeSnapshot(t, ts.dir, golden(t, "proj-b", "old", t0.Add(-time.Minute)))
	resp, body := ts.get(t, "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%d: %s", resp.StatusCode, body)
	}
	for _, want := range []string{"member@example.com", `href="/clusters/proj-a/fresh"`, `href="/clusters/proj-b/old"`, "stale 1m", "2s old", "1 stale", "ate-env-api", "default-template-workerpool", "v-test"} {
		if !strings.Contains(body, want) {
			t.Errorf("Fleet page lacks %q", want)
		}
	}
}

// Text from a snapshot is escaped: a crash message is the reporter's copy
// of whatever the control plane said.
func TestPagesEscapeSnapshotText(t *testing.T) {
	ts := newTestSite(t)
	snap := golden(t, "proj-a", "c-1", t0)
	for i := range snap.Actors {
		if snap.Actors[i].Crash != nil {
			snap.Actors[i].Crash.Message = `<script>alert(1)</script>`
		}
	}
	snap.Deployments[0].Version = `"><img src=x onerror=alert(1)>`
	writeSnapshot(t, ts.dir, snap)
	for _, p := range []string{"/", "/clusters/proj-a/c-1"} {
		_, body := ts.get(t, p)
		if strings.Contains(body, "<script>alert") || strings.Contains(body, "<img src=x") {
			t.Errorf("%s renders snapshot text as markup", p)
		}
	}
	_, body := ts.get(t, "/clusters/proj-a/c-1")
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("the crash message is not shown escaped")
	}
}

func TestClusterPage(t *testing.T) {
	ts := newTestSite(t)
	writeSnapshot(t, ts.dir, golden(t, "proj-a", "c-1", t0))
	resp, body := ts.get(t, "/clusters/proj-a/c-1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%d: %s", resp.StatusCode, body)
	}
	for _, want := range []string{"env-001", "env-002", "w-0a1b2c", "golden-default-template-1a2b3c4d5e6f", "8.0", "2.0 / 8.0", "snapshot download timed out", `href="/api/v1/actors/8f2a0001/events"`} {
		if !strings.Contains(body, want) {
			t.Errorf("Cluster page lacks %q", want)
		}
	}

	_, body = ts.get(t, "/clusters/proj-a/c-1?state=crashed")
	if !strings.Contains(body, "env-002") || strings.Contains(body, "<td>env-001</td>") {
		t.Error("the state filter does not select the crashed actor only")
	}
	if !strings.Contains(body, `value="crashed" selected`) {
		t.Error("the state filter is not kept in the form")
	}

	for path, code := range map[string]int{
		"/clusters/proj-a/missing": http.StatusNotFound,
		// ServeMux redirects a path with a literal ".." to its clean form.
		"/clusters/proj-a/..":     http.StatusTemporaryRedirect,
		"/clusters/proj-a/%2E%2E": http.StatusBadRequest,
		"/clusters/proj-a/c%5C1":  http.StatusBadRequest,
		"/clusters/proj-a/c%2F1":  http.StatusBadRequest,
	} {
		if resp, _ := ts.get(t, path); resp.StatusCode != code {
			t.Errorf("%s: %d, want %d", path, resp.StatusCode, code)
		}
	}
}

// FS-1, FS-9: the API returns the snapshot with its age and staleness, and
// ?nocache=1 revalidates at once.
func TestAPIClusters(t *testing.T) {
	ts := newTestSite(t)
	writeSnapshot(t, ts.dir, golden(t, "proj-a", "c-1", t0.Add(-time.Minute)))

	_, body := ts.get(t, "/api/v1/clusters")
	var index struct {
		Clusters []ClusterSummary `json:"clusters"`
	}
	if err := json.Unmarshal([]byte(body), &index); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if len(index.Clusters) != 1 || !index.Clusters[0].Stale || index.Clusters[0].AgeSeconds != 60 || index.Clusters[0].Counts.ActorsByState["running"] != 1 {
		t.Errorf("index = %+v", index.Clusters)
	}

	// The reporter writes again; the cache still serves the old snapshot.
	time.Sleep(10 * time.Millisecond)
	writeSnapshot(t, ts.dir, golden(t, "proj-a", "c-1", t0))
	var detail ClusterDetail
	_, body = ts.get(t, "/api/v1/clusters/proj-a/c-1")
	if err := json.Unmarshal([]byte(body), &detail); err != nil {
		t.Fatal(err)
	}
	if !detail.Stale {
		t.Errorf("cached detail = stale %v, want the cached stale snapshot", detail.Stale)
	}
	_, body = ts.get(t, "/api/v1/clusters/proj-a/c-1?nocache=1")
	detail = ClusterDetail{}
	if err := json.Unmarshal([]byte(body), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Stale || detail.AgeSeconds != 0 || detail.Snapshot == nil || detail.Snapshot.Project != "proj-a" || detail.Generation == 0 {
		t.Errorf("nocache detail = %+v", detail)
	}

	if resp, _ := ts.get(t, "/api/v1/clusters/proj-a/nope"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing cluster: %d", resp.StatusCode)
	}
}

func TestAPIActorEvents(t *testing.T) {
	ts := newTestSite(t)
	ts.events.out = []Event{{ActorUID: "8f2a0001", State: "running"}}

	resp, body := ts.get(t, "/api/v1/actors/8f2a0001/events")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"state":"running"`) {
		t.Fatalf("%d: %s", resp.StatusCode, body)
	}
	if ts.events.uid != "8f2a0001" || !ts.events.since.Equal(t0.Add(-24*time.Hour)) {
		t.Errorf("queried uid %q since %s, want the last 24 h", ts.events.uid, ts.events.since)
	}

	ts.get(t, "/api/v1/actors/8f2a0001/events?since=2026-11-16T11:00:00Z")
	if !ts.events.since.Equal(t0.Add(-time.Hour)) {
		t.Errorf("since = %s", ts.events.since)
	}

	for _, p := range []string{
		"/api/v1/actors/bad%20uid/events",
		"/api/v1/actors/" + strings.Repeat("a", 129) + "/events",
		"/api/v1/actors/u/events?since=yesterday",
		"/api/v1/actors/u/events?since=2026-11-17T00:00:00Z",
		"/api/v1/actors/u/events?since=2026-01-01T00:00:00Z",
	} {
		if resp, _ := ts.get(t, p); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", p, resp.StatusCode)
		}
	}

	ts.events.err = errors.New("bigquery: secret detail")
	resp, body = ts.get(t, "/api/v1/actors/u/events")
	if resp.StatusCode != http.StatusBadGateway || strings.Contains(body, "secret detail") {
		t.Errorf("failed query: %d %s, want 502 without the detail", resp.StatusCode, body)
	}

	ts.srv.cfg.Events = nil
	if resp, _ := ts.get(t, "/api/v1/actors/u/events"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("no events table: %d, want 503", resp.StatusCode)
	}
}

// The events route, one BigQuery job per request, has its own lower limit;
// a full page of events is marked truncated.
func TestAPIActorEventsLimit(t *testing.T) {
	ts := newTestSite(t)
	ts.events.out = make([]Event, MaxEvents)
	resp, body := ts.get(t, "/api/v1/actors/u/events")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"truncated":true`) {
		t.Fatalf("%d, truncated missing", resp.StatusCode)
	}
	limited := 0
	for range eventsBurst + 2 {
		if resp, _ := ts.get(t, "/api/v1/actors/u/events"); resp.StatusCode == http.StatusTooManyRequests {
			limited++
			if got := resp.Header.Get("Retry-After"); got != "1" {
				t.Errorf("limited events request: Retry-After %q, want 1", got)
			}
		}
	}
	if limited != 3 {
		t.Errorf("%d events requests limited, want 3 (burst %d)", limited, eventsBurst)
	}
	// The limit is per user.
	if resp, _ := ts.do(t, http.MethodGet, "/api/v1/actors/u/events", "other@example.com"); resp.StatusCode != http.StatusOK {
		t.Errorf("another user's events request: %d", resp.StatusCode)
	}
	if resp, _ := ts.get(t, "/api/v1/clusters"); resp.StatusCode != http.StatusOK {
		t.Errorf("another route after the events limit: %d", resp.StatusCode)
	}
}

// A denied path is logged quoted, so a caller cannot forge a log line.
func TestSiteLogsQuotedPaths(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	ts := newTestSite(t)
	ts.do(t, http.MethodGet, "/clusters/x/y%0a2026/11/16%2000:00:00%20fleetsite:%20FORGED", "")
	if lines := strings.Count(strings.TrimRight(buf.String(), "\n"), "\n"); lines != 0 || !strings.Contains(buf.String(), `\n`) {
		t.Errorf("log = %q, want one line with the newline quoted", buf.String())
	}
}

// An admitted request whose error is logged has its path quoted too, on the
// API (apiError) and on a page (pageError).
func TestSiteLogsQuotedPathsOnErrors(t *testing.T) {
	for _, prefix := range []string{"/api/v1/clusters", "/clusters"} {
		t.Run(prefix, func(t *testing.T) {
			var buf bytes.Buffer
			log.SetOutput(&buf)
			defer log.SetOutput(os.Stderr)
			ts := newTestSite(t)
			ts.srv.cfg.Snapshots = NewSnapshots(&countingStore{Store: DirStore{}, err: errors.New("403 from storage")}, ts.clock.now)
			if resp, _ := ts.get(t, prefix+"/x/y%0a2026-11-16%2000:00:00%20fleetsite:%20FORGED"); resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("%d, want 502", resp.StatusCode)
			}
			// The refresh logs the store's error on a line of its own.
			for _, line := range strings.Split(buf.String(), "\n") {
				if strings.HasPrefix(line, "2026-11-16 00:00:00 fleetsite: FORGED") {
					t.Errorf("log = %q, has a forged line", buf.String())
				}
			}
			if !strings.Contains(buf.String(), `y\n2026-11-16`) {
				t.Errorf("log = %q, want the newline quoted", buf.String())
			}
		})
	}
}

// The node filter selects the actors on one node; the page shows at most
// maxActorRows of them.
func TestClusterPageNodeFilterAndRowLimit(t *testing.T) {
	ts := newTestSite(t)
	snap := golden(t, "proj-a", "c-1", t0)
	_, body := ts.getAfterWrite(t, snap, "/clusters/proj-a/c-1?node=gke-bench-1-pool-1-abcd")
	if !strings.Contains(body, "<td>env-001</td>") || strings.Contains(body, "<td>env-002</td>") {
		t.Error("the node filter does not select env-001 only")
	}

	big := golden(t, "proj-a", "c-2", t0)
	base := big.Actors[0]
	big.Actors = nil
	for i := range maxActorRows + 5 {
		a := base
		a.UID, a.Name = fmt.Sprintf("u-%05d", i), fmt.Sprintf("env-%05d", i)
		big.Actors = append(big.Actors, a)
	}
	big.Counts = inventory.ComputeCounts(big.Actors)
	_, body = ts.getAfterWrite(t, big, "/clusters/proj-a/c-2")
	if got := strings.Count(body, `href="/api/v1/actors/u-`); got != maxActorRows {
		t.Errorf("%d actor rows, want %d", got, maxActorRows)
	}
	if !strings.Contains(body, fmt.Sprintf("%d match; the first %d are shown", maxActorRows+5, maxActorRows)) {
		t.Error("the page does not say it shows the first rows only")
	}
}

func (ts *testSite) getAfterWrite(t *testing.T, snap *inventory.Snapshot, path string) (*http.Response, string) {
	t.Helper()
	writeSnapshot(t, ts.dir, snap)
	return ts.get(t, path+map[bool]string{true: "&", false: "?"}[strings.Contains(path, "?")]+"nocache=1")
}

func TestSiteRateLimitsEachUser(t *testing.T) {
	ts := newTestSite(t)
	limited := 0
	for range userBurst + 5 {
		if resp, _ := ts.do(t, http.MethodGet, "/api/v1/clusters", "busy@example.com"); resp.StatusCode == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited != 5 {
		t.Errorf("%d requests limited, want 5", limited)
	}
	if resp, _ := ts.do(t, http.MethodGet, "/api/v1/clusters", "other@example.com"); resp.StatusCode != http.StatusOK {
		t.Errorf("another user: %d", resp.StatusCode)
	}
	ts.clock.add(time.Second)
	if resp, _ := ts.do(t, http.MethodGet, "/api/v1/clusters", "busy@example.com"); resp.StatusCode != http.StatusOK {
		t.Errorf("after a second: %d", resp.StatusCode)
	}
}

func TestSiteBucketError(t *testing.T) {
	ts := newTestSite(t)
	ts.srv.cfg.Snapshots = NewSnapshots(&countingStore{Store: DirStore{}, err: errors.New("403 from storage")}, ts.clock.now)
	for _, p := range []string{"/", "/api/v1/clusters"} {
		resp, body := ts.get(t, p)
		if resp.StatusCode != http.StatusBadGateway || strings.Contains(body, "403 from storage") {
			t.Errorf("%s: %d %q, want 502 without the detail", p, resp.StatusCode, body)
		}
	}
}
