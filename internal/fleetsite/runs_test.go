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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	runTrace  = "4bf92f3577b34da6a3ce929d0e0e4736"
	runSpanID = "00f067aa0ba902b7"
)

// otlpLine returns one OTLP/JSON export request with spans of one resource.
func otlpLine(t *testing.T, service, project string, spans ...map[string]any) string {
	t.Helper()
	attrs := []map[string]any{{"key": "service.name", "value": map[string]any{"stringValue": service}}}
	if project != "" {
		attrs = append(attrs, map[string]any{"key": "gcp.project_id", "value": map[string]any{"stringValue": project}})
	}
	b, err := json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{
		"resource":   map[string]any{"attributes": attrs},
		"scopeSpans": []any{map[string]any{"spans": spans}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func otlpSpanOf(trace, id, parent, name string, startMS, endMS int64, errored bool) map[string]any {
	s := map[string]any{
		"traceId": trace, "spanId": id, "parentSpanId": parent, "name": name,
		"startTimeUnixNano": fmt.Sprint(1_700_000_000_000_000_000 + startMS*1_000_000),
		"endTimeUnixNano":   fmt.Sprint(1_700_000_000_000_000_000 + endMS*1_000_000),
	}
	if errored {
		s["status"] = map[string]any{"code": 2}
	}
	return s
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const runReportJSON = `{"strategy": "sliding", "total_s": 24.87, "phases": {"teardown": {"count": 1, "total_s": 0.31, "max_s": 0.31}, "acquire": {"count": 200, "total_s": 41.2, "max_s": 1.9}, "zz_custom": {"count": 2, "total_s": 1, "max_s": 0.5}}, "claims": 200, "tasks_ok": 199, "tasks_err": 1, "warm_replicas_total": 0, "warm_replicas_peak": 0, "environment": {}}`

// runsDir writes a trainer run, a traced bundle, an untraced bundle and
// folders whose names are not runs.
func runsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := filepath.Join(dir, "v1", "runs", "grpo-e1")
	writeFile(t, filepath.Join(run, "sliding_200tasks_20261116-120000.json"), runReportJSON)
	writeFile(t, filepath.Join(run, "sliding_200tasks_20261116-120000.txt"), "── Run report ──")
	writeFile(t, filepath.Join(run, "run.json"), `{"title": "grpo-e1"}`)
	writeFile(t, filepath.Join(run, "report.html"), `<html><script>alert(document.domain)</script></html>`)
	writeFile(t, filepath.Join(run, "spans.jsonl"), strings.Join([]string{
		otlpLine(t, "ate-env-fleet", "proj-a",
			otlpSpanOf(runTrace, runSpanID, "", RunSpanName, 0, 1000, false),
			otlpSpanOf(runTrace, "1111111111111111", runSpanID, "ate_env_fleet.acquire", 10, 400, false)),
		otlpLine(t, "ate-env-api", "",
			otlpSpanOf(runTrace, "2222222222222222", "1111111111111111", "ateenv.v1alpha.EnvironmentService/CreateEnvironment", 20, 300, true),
			otlpSpanOf("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "3333333333333333", "", "other.trace", 0, 5, false)),
	}, "\n")+"\n")
	writeFile(t, filepath.Join(dir, "v1", "runs", "bad id", "x.json"), "{}")
	b := filepath.Join(dir, "bench-a")
	writeFile(t, filepath.Join(b, "2026-11-15_qwen3-8b_swe-bench_baseline", "report.json"), `{"mode": "steady"}`)
	writeFile(t, filepath.Join(b, "2026-11-15_qwen3-8b_swe-bench_baseline", "run_window.json"), `{"start": "<script>x</script>"}`)
	writeFile(t, filepath.Join(b, "2026-11-16_qwen3-8b_swe-bench_baseline_traced", "report.json"), `not json`)
	writeFile(t, filepath.Join(b, "not-a-bundle", "report.json"), "{}")
	writeFile(t, filepath.Join(b, "2026-13-40_a_b_c", "report.json"), "{}")
	return dir
}

// failingBucket fails every listing of one bucket.
type failingBucket struct {
	RunStore
	bucket string
}

func (f failingBucket) Folders(ctx context.Context, bucket, prefix string) ([]string, error) {
	if bucket == f.bucket {
		return nil, fmt.Errorf("403 on %s", bucket)
	}
	return f.RunStore.Folders(ctx, bucket, prefix)
}

// index returns r.Index on a live context, failing the test on an error.
func index(t *testing.T, r *Runs, force bool) RunIndex {
	t.Helper()
	idx, err := r.Index(context.Background(), force)
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func TestRunsIndex(t *testing.T) {
	r := NewRuns(failingBucket{DirRuns{Dir: runsDir(t)}, "bench-missing"}, []string{"bench-a", "bench-missing"}, (&fakeClock{t: t0}).now)
	idx := index(t, r, false)
	var got []string
	for _, e := range idx.Runs {
		got = append(got, fmt.Sprintf("%s:%s:%s:%v", e.Kind, e.Bucket, e.ID, e.Traced))
	}
	want := []string{
		"trainer::grpo-e1:false",
		"benchmark:bench-a:2026-11-16_qwen3-8b_swe-bench_baseline_traced:true",
		"benchmark:bench-a:2026-11-15_qwen3-8b_swe-bench_baseline:false",
	}
	if !equalStrings(got, want) {
		t.Errorf("index = %v, want %v", got, want)
	}
	// A bucket that cannot be listed is an error row; the rest still list.
	if len(idx.Errors) != 1 || !strings.Contains(idx.Errors[0], "bench-missing") {
		t.Errorf("errors = %v, want bench-missing", idx.Errors)
	}

	// Cached for RunsCacheTTL.
	writeFile(t, filepath.Join(r.store.(failingBucket).RunStore.(DirRuns).Dir, "v1", "runs", "grpo-e2", "x.json"), "{}")
	if idx := index(t, r, false); len(idx.Runs) != 3 {
		t.Errorf("within the TTL: %d runs, want the cached 3", len(idx.Runs))
	}
	if idx := index(t, r, true); len(idx.Runs) != 4 {
		t.Errorf("forced: %d runs, want 4", len(idx.Runs))
	}
}

// gatedStore holds each Folders call until gate is closed, then answers from
// the store, or with the call's own context error when that ends first.
type gatedStore struct {
	RunStore
	gate  chan struct{}
	mu    sync.Mutex
	calls int
}

func (g *gatedStore) Folders(ctx context.Context, bucket, prefix string) ([]string, error) {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	select {
	case <-g.gate:
		return g.RunStore.Folders(ctx, bucket, prefix)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (g *gatedStore) n() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// A request whose client leaves stops waiting, while the listing it started
// ends and is cached for everyone: the cancellation is not cached.
// Concurrent requests share the listing in flight; forced ones share one
// listing that starts after it.
func TestRunsIndexSurvivesALeavingClient(t *testing.T) {
	g := &gatedStore{RunStore: DirRuns{Dir: runsDir(t)}, gate: make(chan struct{})}
	r := NewRuns(g, []string{"bench-a"}, (&fakeClock{t: t0}).now)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := r.Index(ctx, true)
		errc <- err
	}()
	for g.n() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Index of a leaving client: %v, want context.Canceled", err)
	}

	var wg sync.WaitGroup
	results := make([]RunIndex, 10)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			idx, err := r.Index(context.Background(), i%2 == 1)
			if err != nil {
				t.Error(err)
			}
			results[i] = idx
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(g.gate)
	wg.Wait()
	for i, idx := range results {
		if len(idx.Runs) != 3 || len(idx.Errors) != 0 {
			t.Errorf("request %d: %d runs, errors %v; want the 3 runs of the listing the leaving client started", i, len(idx.Runs), idx.Errors)
		}
	}
	// Two sources: the leaving client's listing, and one listing for the
	// five forced requests that came during it.
	if got := g.n(); got != 4 {
		t.Errorf("%d Folders calls, want 4 (2 listings of 2 sources)", got)
	}
	if idx := index(t, r, false); len(idx.Runs) != 3 || g.n() != 4 {
		t.Errorf("within the TTL: %d runs after %d calls, want the cached 3", len(idx.Runs), g.n())
	}
}

// A listing whose own timeout expired is returned, not cached: the next
// request lists again.
func TestRunsIndexDoesNotCacheAnExpiredListing(t *testing.T) {
	g := &gatedStore{RunStore: DirRuns{Dir: runsDir(t)}, gate: make(chan struct{})}
	r := NewRuns(g, nil, (&fakeClock{t: t0}).now)
	r.timeout = 20 * time.Millisecond
	if idx := index(t, r, false); len(idx.Errors) != 1 {
		t.Fatalf("expired listing: errors %v, want the inventory bucket", idx.Errors)
	}
	close(g.gate)
	if idx := index(t, r, false); len(idx.Errors) != 0 || len(idx.Runs) != 1 || g.n() != 2 {
		t.Errorf("after an expired listing: %d runs, errors %v, %d calls; want a new listing", len(idx.Runs), idx.Errors, g.n())
	}
}

// Nothing outside -dir is listed or read: a benchmark bucket's directory
// that is a symbolic link out of -dir lists as empty and serves no file. A
// link that stays inside -dir is followed, and a bucket that is not one
// path element is refused.
func TestDirRunsStaysInsideDir(t *testing.T) {
	ctx := context.Background()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "2026-11-15_a_b_c", "report.json"), `{"secret": 1}`)
	dir := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "bench-x")); err != nil {
		t.Skip(err)
	}
	s := DirRuns{Dir: dir}
	if names, err := s.Folders(ctx, "bench-x", ""); err != nil || len(names) != 0 {
		t.Errorf("Folders of a bucket linked out of -dir = %v, %v; want none", names, err)
	}
	if files, err := s.Files(ctx, "bench-x", "2026-11-15_a_b_c"); err != nil || len(files) != 0 {
		t.Errorf("Files of a bucket linked out of -dir = %v, %v; want none", files, err)
	}
	if data, err := s.Read(ctx, "bench-x", "2026-11-15_a_b_c/report.json", 1<<20); !errors.Is(err, errOutside) {
		t.Errorf("Read of a bucket linked out of -dir = %q, %v; want errOutside", data, err)
	}
	if idx := index(t, NewRuns(s, []string{"bench-x"}, nil), false); len(idx.Runs) != 0 || len(idx.Errors) != 0 {
		t.Errorf("index with a bucket linked out of -dir = %+v, want empty", idx)
	}

	inside := filepath.Join(dir, "real")
	writeFile(t, filepath.Join(inside, "2026-11-15_a_b_c", "report.json"), "{}")
	if err := os.Symlink(inside, filepath.Join(dir, "bench-y")); err != nil {
		t.Fatal(err)
	}
	if names, err := s.Folders(ctx, "bench-y", ""); err != nil || !equalStrings(names, []string{"2026-11-15_a_b_c"}) {
		t.Errorf("Folders of a bucket linked inside -dir = %v, %v", names, err)
	}
	for _, b := range []string{"..", "../x", "a/b", "/abs"} {
		if _, err := s.Folders(ctx, b, ""); err == nil {
			t.Errorf("bucket %q accepted", b)
		}
	}
}

func TestRunsIndexTruncates(t *testing.T) {
	dir := t.TempDir()
	for i := range maxRunsListed + 3 {
		if err := os.MkdirAll(filepath.Join(dir, "v1", "runs", fmt.Sprintf("run-%04d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r := NewRuns(DirRuns{Dir: dir}, nil, nil)
	idx := index(t, r, false)
	if len(idx.Runs) != maxRunsListed || idx.Runs[0].ID != fmt.Sprintf("run-%04d", maxRunsListed+2) || len(idx.Truncated) != 1 {
		t.Errorf("%d runs, first %s, truncated %v; want the first %d by name", len(idx.Runs), idx.Runs[0].ID, idx.Truncated, maxRunsListed)
	}
	// A run past the cap is still found by its URL: Find reads the store.
	writeFile(t, filepath.Join(dir, "v1", "runs", "run-0000", "a.json"), "{}")
	if _, _, ok, err := r.Find(context.Background(), RunTrainer, "", "run-0000"); !ok || err != nil {
		t.Errorf("a run past the cap: found %v, %v", ok, err)
	}
}

func TestDirRunsRefusesNamesOutsideTheBucket(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "secret.txt"), "secret")
	s := DirRuns{Dir: filepath.Join(dir, "root")}
	for _, name := range []string{"../secret.txt", "/etc/passwd", "v1/runs/../../../secret.txt"} {
		if data, err := s.Read(context.Background(), "", name, 1<<10); err == nil {
			t.Errorf("Read(%q) = %q", name, data)
		}
	}
	if _, err := s.Files(context.Background(), "", "../"); err == nil {
		t.Error("Files outside the bucket")
	}
}

// FS-11: the run span's trace, its project and the waterfall come from the
// span file.
func TestParseRunTrace(t *testing.T) {
	dir := runsDir(t)
	data, err := os.ReadFile(filepath.Join(dir, "v1", "runs", "grpo-e1", "spans.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	tr, ok, err := ParseRunTrace(data)
	if err != nil || !ok {
		t.Fatalf("ok %v, %v", ok, err)
	}
	if tr.TraceID != runTrace || tr.Project != "proj-a" || tr.DurationMS != 1000 || len(tr.Spans) != 3 {
		t.Fatalf("trace = %+v", tr)
	}
	want := []WaterfallSpan{
		{Name: RunSpanName, Service: "ate-env-fleet", Depth: 0, OffsetMS: 0, DurMS: 1000},
		{Name: "ate_env_fleet.acquire", Service: "ate-env-fleet", Depth: 1, OffsetMS: 10, DurMS: 390},
		{Name: "ateenv.v1alpha.EnvironmentService/CreateEnvironment", Service: "ate-env-api", Depth: 2, OffsetMS: 20, DurMS: 280, Error: true},
	}
	for i, w := range want {
		if tr.Spans[i] != w {
			t.Errorf("span %d = %+v, want %+v", i, tr.Spans[i], w)
		}
	}

	// No run span; a cycle of parents; a bad line; many spans.
	if _, ok, err := ParseRunTrace([]byte(otlpLine(t, "x", "", otlpSpanOf(runTrace, "1111111111111111", "", "other", 0, 1, false)))); ok || err != nil {
		t.Errorf("no run span: ok %v, %v", ok, err)
	}
	cycle := otlpLine(t, "x", "",
		otlpSpanOf(runTrace, runSpanID, "", RunSpanName, 0, 10, false),
		otlpSpanOf(runTrace, "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "a", 1, 2, false),
		otlpSpanOf(runTrace, "bbbbbbbbbbbbbbbb", "aaaaaaaaaaaaaaaa", "b", 1, 2, false))
	if tr, ok, err := ParseRunTrace([]byte(cycle)); err != nil || !ok || len(tr.Spans) != 3 {
		t.Errorf("parent cycle: %+v, %v, %v", tr, ok, err)
	}
	if _, _, err := ParseRunTrace([]byte("not json\n")); err == nil {
		t.Error("a bad line is not an error")
	}
	var many []map[string]any
	many = append(many, otlpSpanOf(runTrace, runSpanID, "", RunSpanName, 0, 10, false))
	for i := range maxWaterfallSpans + 5 {
		many = append(many, otlpSpanOf(runTrace, fmt.Sprintf("%016x", i+1), runSpanID, "s", 1, 2, false))
	}
	if tr, _, _ := ParseRunTrace([]byte(otlpLine(t, "x", "Bad Project", many...))); len(tr.Spans) != maxWaterfallSpans || tr.Omitted != 6 || tr.Project != "" {
		t.Errorf("%d spans, %d omitted, project %q; want %d, 6, none", len(tr.Spans), tr.Omitted, tr.Project, maxWaterfallSpans)
	}
}

func TestRunsFindBySource(t *testing.T) {
	dir := runsDir(t)
	// The same bundle name in two buckets, and a trainer run of that name.
	name := "2026-11-15_qwen3-8b_swe-bench_baseline"
	writeFile(t, filepath.Join(dir, "bench-b", name, "report.json"), `{"bucket": "b"}`)
	writeFile(t, filepath.Join(dir, "v1", "runs", name, "x.txt"), "trainer")
	long := "2026-11-15_" + strings.Repeat("m", 60) + "_" + strings.Repeat("d", 30) + "_" + strings.Repeat("n", 30)
	writeFile(t, filepath.Join(dir, "bench-a", long, "report.json"), "{}")
	r := NewRuns(DirRuns{Dir: dir}, []string{"bench-a", "bench-b"}, nil)
	paths := map[string]bool{}
	for _, e := range index(t, r, false).Runs {
		if paths[e.Path] {
			t.Errorf("two runs share the path %s", e.Path)
		}
		paths[e.Path] = true
		if e.ID == long {
			t.Error("a bundle name longer than a run id is listed")
		}
	}
	for _, p := range []string{"trainer/" + name, "bench/bench-a/" + name, "bench/bench-b/" + name} {
		if !paths[p] {
			t.Errorf("%s is not listed", p)
		}
	}
	for _, tc := range []struct {
		kind, bucket, id string
		ok               bool
	}{
		{RunBenchmark, "bench-b", name, true},
		{RunTrainer, "", name, true},
		{RunBenchmark, "bench-c", name, false},               // not configured
		{RunBenchmark, "bench-a", "grpo-e1", false},          // not a bundle name
		{RunBenchmark, "bench-a", "2026-11-14_a_b_c", false}, // no folder
		{RunTrainer, "", "../bench-a", false},
	} {
		if _, _, ok, err := r.Find(context.Background(), tc.kind, tc.bucket, tc.id); ok != tc.ok || err != nil {
			t.Errorf("Find(%s, %s, %s) = %v, %v; want %v", tc.kind, tc.bucket, tc.id, ok, err, tc.ok)
		}
	}
}

// A deep chain costs one parent lookup per span, whatever the order the
// depths are asked in: depth is memoized. Counted, not timed, so -race and
// coverage do not make it flaky.
func TestDepthsDeepChainIsLinear(t *testing.T) {
	// Big enough that a walk per span (n*n/2 lookups) fails the count,
	// small enough that it fails fast instead of timing out.
	const n = 10_000
	parent := map[string]string{}
	ids := make([]string, n)
	prev := runSpanID
	for i := range n {
		ids[i] = fmt.Sprintf("%016x", i+1)
		parent[ids[i]] = prev
		prev = ids[i]
	}
	for _, tc := range []struct {
		name  string
		order func(i int) string
	}{
		{"shallow first", func(i int) string { return ids[i] }},
		{"deepest first", func(i int) string { return ids[n-1-i] }},
	} {
		d := newDepths(parent, runSpanID)
		for i := range n {
			id := tc.order(i)
			want := 0
			fmt.Sscanf(id, "%x", &want)
			if got := d.of(id); got != want {
				t.Fatalf("%s: depth of span %d = %d", tc.name, want, got)
			}
		}
		if d.steps > n {
			t.Errorf("%s: %d parent lookups for a chain of %d, want at most one per span", tc.name, d.steps, n)
		}
	}
}

// The waterfall of a chain longer than maxWaterfallSpans shows the first
// spans by start, each one hop deeper, and counts the rest as omitted.
func TestParseRunTraceDeepChain(t *testing.T) {
	const n = 2 * maxWaterfallSpans
	spans := []map[string]any{otlpSpanOf(runTrace, runSpanID, "", RunSpanName, 0, n, false)}
	prev := runSpanID
	for i := range n {
		id := fmt.Sprintf("%016x", i+1)
		spans = append(spans, otlpSpanOf(runTrace, id, prev, "s", int64(i), int64(i)+1, false))
		prev = id
	}
	var lines []string
	for i := 0; i < len(spans); i += 1000 {
		lines = append(lines, otlpLine(t, "x", "", spans[i:min(i+1000, len(spans))]...))
	}
	tr, ok, err := ParseRunTrace([]byte(strings.Join(lines, "\n")))
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if tr.Spans[maxWaterfallSpans-1].Depth != maxWaterfallSpans-1 || tr.Omitted != n+1-maxWaterfallSpans {
		t.Errorf("last bar depth %d, omitted %d", tr.Spans[maxWaterfallSpans-1].Depth, tr.Omitted)
	}
}

func TestBarOf(t *testing.T) {
	for _, tc := range []struct {
		sp   WaterfallSpan
		x, w float64
	}{
		{WaterfallSpan{OffsetMS: 100, DurMS: 200}, 96, 192},
		{WaterfallSpan{OffsetMS: -5000, DurMS: 4000}, 0, 1}, // all before the run
		{WaterfallSpan{OffsetMS: -100, DurMS: 300}, 0, 192}, // starts before the run
		{WaterfallSpan{OffsetMS: 900, DurMS: 500}, 864, 96}, // ends after the run
		{WaterfallSpan{OffsetMS: 2000, DurMS: 10}, 959, 1},  // all after the run
		{WaterfallSpan{OffsetMS: 500, DurMS: 0}, 480, 1},
	} {
		b := barOf(tc.sp, 1000, 0)
		if diff := b.X - tc.x; diff > 1e-9 || diff < -1e-9 || b.Width-tc.w > 1e-9 || tc.w-b.Width > 1e-9 {
			t.Errorf("%+v: x %v w %v, want %v %v", tc.sp, b.X, b.Width, tc.x, tc.w)
		}
	}
}

func TestDirRunsRefusesSymlinks(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside")
	writeFile(t, filepath.Join(outside, "secret.txt"), "secret")
	root := filepath.Join(dir, "root")
	run := filepath.Join(root, "v1", "runs", "r1")
	writeFile(t, filepath.Join(run, "ok.txt"), "ok")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(run, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "v1", "runs", "r2")); err != nil {
		t.Fatal(err)
	}
	s := DirRuns{Dir: root}
	files, err := s.Files(context.Background(), "", "v1/runs/r1")
	if err != nil || len(files) != 1 || files[0].Name != "ok.txt" {
		t.Errorf("files = %+v, %v; want ok.txt only", files, err)
	}
	for _, name := range []string{"v1/runs/r1/link.txt", "v1/runs/r2/secret.txt"} {
		if data, err := s.Read(context.Background(), "", name, 1<<10); err == nil {
			t.Errorf("Read(%q) = %q", name, data)
		}
	}
	if files, err := s.Files(context.Background(), "", "v1/runs/r2"); err == nil && len(files) > 0 {
		t.Errorf("a symlinked folder lists %+v", files)
	}
}

func newRunsSite(t *testing.T) *testSite {
	t.Helper()
	ts := newTestSite(t)
	ts.srv.cfg.Runs = NewRuns(DirRuns{Dir: runsDir(t)}, []string{"bench-a"}, ts.clock.now)
	return ts
}

// A request whose own deadline ends its wait for the listing answers 502,
// not an empty 200, on the API and the page. The listing is bounded below
// the request's deadline, so one that runs out still answers its request.
func TestRunsRoutesAnswerWhenTheRequestRunsOut(t *testing.T) {
	if refreshTimeout >= requestTimeout {
		t.Fatalf("refreshTimeout %s is not below requestTimeout %s", refreshTimeout, requestTimeout)
	}
	if r := NewRuns(DirRuns{}, nil, nil); r.timeout != refreshTimeout {
		t.Errorf("a listing is bounded by %s, want refreshTimeout %s", r.timeout, refreshTimeout)
	}
	ts := newTestSite(t)
	g := &gatedStore{RunStore: DirRuns{Dir: runsDir(t)}, gate: make(chan struct{})}
	t.Cleanup(func() { close(g.gate) })
	ts.srv.cfg.Runs = NewRuns(g, nil, ts.clock.now)
	for _, tc := range []struct{ path, want string }{
		{"/api/v1/runs", `"error":"the runs could not be listed"`},
		{"/runs", "The runs could not be listed."},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		req := httptest.NewRequest(http.MethodGet, tc.path, nil).WithContext(ctx)
		req.Header.Set("X-Test-User", "member@example.com")
		rec := httptest.NewRecorder()
		ts.srv.ServeHTTP(rec, req)
		cancel()
		if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s with the request's deadline past: %d %q, want 502 with %q", tc.path, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

func TestRunsPages(t *testing.T) {
	ts := newRunsSite(t)
	resp, body := ts.get(t, "/runs")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%d: %s", resp.StatusCode, body)
	}
	for _, want := range []string{`href="/runs/trainer/grpo-e1"`, `href="/runs/bench/bench-a/2026-11-16_qwen3-8b_swe-bench_baseline_traced"`, "traced", "bench-a"} {
		if !strings.Contains(body, want) {
			t.Errorf("Runs page lacks %q", want)
		}
	}
	if strings.Contains(body, "not-a-bundle") || strings.Contains(body, "bad id") {
		t.Error("the Runs page lists a folder that is not a run")
	}

	// FS-11, trainer: the report table in RunReport order, the HTML report,
	// the Cloud Trace link of the run span, the waterfall.
	resp, body = ts.get(t, "/runs/trainer/grpo-e1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%d: %s", resp.StatusCode, body)
	}
	for _, want := range []string{
		"strategy sliding", "total 24.87 s", "tasks 199 ok, 1 err",
		`href="/runs/trainer/grpo-e1/files/report.html"`,
		`href="https://console.cloud.google.com/traces/list?project=proj-a&amp;tid=` + runTrace + `"`,
		`class="span-error"`, "ateenv.v1alpha.EnvironmentService/CreateEnvironment",
		`href="/runs/trainer/grpo-e1/files/spans.jsonl"`, `viewBox="0 0 960 50" width="960" height="50"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("trainer run page lacks %q", want)
		}
	}
	if a, z := strings.Index(body, "<td>acquire</td>"), strings.Index(body, "<td>teardown</td>"); a < 0 || z < a || strings.Index(body, "<td>zz_custom</td>") < z {
		t.Error("phases are not in RunReport order with the others after")
	}

	// FS-11, benchmark: the three files, escaped; a bad file is a note; the
	// _traced folder is marked.
	_, body = ts.get(t, "/runs/bench/bench-a/2026-11-15_qwen3-8b_swe-bench_baseline")
	for _, want := range []string{"run_window.json", "&lt;script&gt;x&lt;/script&gt;", `&#34;mode&#34;: &#34;steady&#34;`, "system_description.json", "not in the bundle"} {
		if !strings.Contains(body, want) {
			t.Errorf("bundle page lacks %q", want)
		}
	}
	_, body = ts.get(t, "/runs/bench/bench-a/2026-11-16_qwen3-8b_swe-bench_baseline_traced")
	if !strings.Contains(body, "traced: latencies include trace overhead") || !strings.Contains(body, "not valid JSON") {
		t.Error("the traced bundle page lacks its badge or its note")
	}

	for path, code := range map[string]int{"/runs/trainer/missing": http.StatusNotFound, "/runs/trainer/bad%20id": http.StatusBadRequest, "/runs/bench/bench-a/not-a-bundle": http.StatusNotFound, "/runs/bench/bench-x/2026-11-15_qwen3-8b_swe-bench_baseline": http.StatusNotFound, "/runs/grpo-e1": http.StatusNotFound} {
		if resp, _ := ts.get(t, path); resp.StatusCode != code {
			t.Errorf("%s: %d, want %d", path, resp.StatusCode, code)
		}
	}
}

// Every run file is a download, report.html too: its script never runs on
// the site's origin.
func TestRunFiles(t *testing.T) {
	ts := newRunsSite(t)
	for name, ct := range map[string]string{
		"report.html": "application/octet-stream", "spans.jsonl": "text/plain; charset=utf-8",
		"run.json": "application/json", "sliding_200tasks_20261116-120000.txt": "text/plain; charset=utf-8",
	} {
		resp, _ := ts.get(t, "/runs/trainer/grpo-e1/files/"+name)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != ct || resp.Header.Get("Content-Disposition") != `attachment; filename="`+name+`"` {
			t.Errorf("%s: %d %q %q", name, resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Disposition"))
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "sandbox") {
			t.Errorf("%s: CSP %q, want the site's", name, csp)
		}
	}
	if resp, _ := ts.get(t, "/runs/bench/bench-a/2026-11-15_qwen3-8b_swe-bench_baseline/files/report.json"); resp.StatusCode != http.StatusOK {
		t.Errorf("bundle file: %d", resp.StatusCode)
	}
	for _, p := range []string{"/runs/trainer/grpo-e1/files/missing.json", "/runs/trainer/grpo-e1/files/..%2F..%2Fsecret", "/runs/trainer/grpo-e1/files/%2E%2E"} {
		if resp, _ := ts.get(t, p); resp.StatusCode == http.StatusOK {
			t.Errorf("%s: 200", p)
		}
	}
}

// countingRuns counts the reads of a RunStore.
type countingRuns struct {
	RunStore
	mu    sync.Mutex
	reads int
}

func (c *countingRuns) Read(ctx context.Context, bucket, name string, limit int) ([]byte, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	return c.RunStore.Read(ctx, bucket, name, limit)
}

// A run page reads its files once per change, also for concurrent views.
func TestRunDetailIsCached(t *testing.T) {
	ts := newTestSite(t)
	dir := runsDir(t)
	store := &countingRuns{RunStore: DirRuns{Dir: dir}}
	ts.srv.cfg.Runs = NewRuns(store, nil, ts.clock.now)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ts.do(t, http.MethodGet, "/runs/trainer/grpo-e1", "member@example.com")
		}()
	}
	wg.Wait()
	ts.get(t, "/runs/trainer/grpo-e1")
	if store.reads != 2 { // the RunReport and spans.jsonl
		t.Errorf("%d reads for 9 views, want 2", store.reads)
	}
	time.Sleep(10 * time.Millisecond)
	writeFile(t, filepath.Join(dir, "v1", "runs", "grpo-e1", "spans.jsonl"), "")
	ts.get(t, "/runs/trainer/grpo-e1")
	if store.reads != 4 {
		t.Errorf("%d reads after a file changed, want 4", store.reads)
	}
}

// blockingRuns blocks every read until its context ends.
type blockingRuns struct{ RunStore }

func (blockingRuns) Read(ctx context.Context, _, _ string, _ int) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A build that runs out of time serves its waiters but is not cached: the
// next view builds again and gets the files.
func TestRunDetailOutOfTimeIsNotCached(t *testing.T) {
	ts := newTestSite(t)
	dir := runsDir(t)
	ts.srv.cfg.Runs = NewRuns(blockingRuns{DirRuns{Dir: dir}}, nil, ts.clock.now)
	ts.srv.runDetails.timeout = 50 * time.Millisecond
	if _, body := ts.get(t, "/runs/trainer/grpo-e1"); !strings.Contains(body, "spans.jsonl could not be read") {
		t.Fatal("the timed-out build did not degrade as expected")
	}
	ts.srv.cfg.Runs = NewRuns(DirRuns{Dir: dir}, nil, ts.clock.now)
	if _, body := ts.get(t, "/runs/trainer/grpo-e1"); strings.Contains(body, "could not be read") || !strings.Contains(body, "span-error") {
		t.Error("the degraded build of a run was cached")
	}
}

// ctxRuns fails a read whose context has ended.
type ctxRuns struct{ RunStore }

func (c ctxRuns) Read(ctx context.Context, bucket, name string, limit int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.RunStore.Read(ctx, bucket, name, limit)
}

// A build queued behind the slots longer than its timeout still gets its
// whole time once it runs: the deadline starts with the slot.
func TestRunDetailDeadlineStartsWithTheSlot(t *testing.T) {
	ts := newTestSite(t)
	// Reads honor their context, as Cloud Storage's do.
	ts.srv.cfg.Runs = NewRuns(ctxRuns{DirRuns{Dir: runsDir(t)}}, []string{"bench-a"}, ts.clock.now)
	ts.srv.runDetails.timeout = 200 * time.Millisecond
	ts.get(t, "/runs/trainer/grpo-e1") // makes the slots
	d := &ts.srv.runDetails
	for range runDetailBuilds {
		d.sem <- struct{}{}
	}
	done := make(chan string, 1)
	go func() {
		_, body := ts.get(t, "/runs/bench/bench-a/2026-11-15_qwen3-8b_swe-bench_baseline")
		done <- body
	}()
	time.Sleep(400 * time.Millisecond)
	for range runDetailBuilds {
		<-d.sem
	}
	if body := <-done; !strings.Contains(body, "&#34;mode&#34;: &#34;steady&#34;") {
		t.Error("the queued build ran out of time before it ran")
	}
}

// The trace project falls back to -trace-project; a RunReport that is not
// JSON (NaN) is a note; counts render as integers.
func TestRunPageFallbacks(t *testing.T) {
	ts := newRunsSite(t)
	dir := ts.srv.cfg.Runs.store.(DirRuns).Dir
	run := filepath.Join(dir, "v1", "runs", "grpo-e2")
	writeFile(t, filepath.Join(run, "spans.jsonl"), otlpLine(t, "ate-env-fleet", "", otlpSpanOf(runTrace, runSpanID, "", RunSpanName, 0, 10, false))+"\n")
	writeFile(t, filepath.Join(run, "sliding_9tasks_20261116-120000.json"), `{"strategy": "sliding", "total_s": NaN}`)
	writeFile(t, filepath.Join(run, "sliding_9tasks_20261116-120001.json"), `{"strategy": "sliding", "total_s": 1, "claims": 1000000, "phases": {}}`)
	_, body := ts.get(t, "/runs/trainer/grpo-e2")
	if strings.Contains(body, "console.cloud.google.com") || !strings.Contains(body, "no Cloud Trace link") {
		t.Error("a span file without a project links to Cloud Trace")
	}
	if !strings.Contains(body, "sliding_9tasks_20261116-120000.json is not a RunReport") || !strings.Contains(body, "claims 1000000") {
		t.Error("the NaN note or the integer count is missing")
	}
	ts.srv.cfg.TraceProject = "proj-t"
	ts.clock.add(RunsCacheTTL)
	_, body = ts.get(t, "/runs/trainer/grpo-e2")
	if !strings.Contains(body, "project=proj-t&amp;tid="+runTrace) {
		t.Error("the trace project does not fall back to -trace-project")
	}
}

func TestRunsAPI(t *testing.T) {
	ts := newRunsSite(t)
	_, body := ts.get(t, "/api/v1/runs")
	var idx RunIndex
	if err := json.Unmarshal([]byte(body), &idx); err != nil || len(idx.Runs) != 3 {
		t.Fatalf("%v: %s", err, body)
	}
	_, body = ts.get(t, "/api/v1/runs/trainer/grpo-e1")
	var d RunDetail
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatal(err)
	}
	if d.Kind != RunTrainer || len(d.Reports) != 1 || d.Trace == nil || d.Trace.TraceID != runTrace || !d.HasHTML || len(d.Files) != 5 {
		t.Errorf("detail = %+v", d)
	}
	// A bundle by its bucket and id: the three shown files, one missing.
	_, body = ts.get(t, "/api/v1/runs/bench/bench-a/2026-11-15_qwen3-8b_swe-bench_baseline")
	var b RunDetail
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if b.Kind != RunBenchmark || b.Bucket != "bench-a" || len(b.Bundle) != len(bundleFiles) || len(b.Files) != 2 {
		t.Errorf("bundle detail = %+v", b)
	}
	for _, p := range []string{"/api/v1/runs/bench/bench-c/2026-11-15_qwen3-8b_swe-bench_baseline", "/api/v1/runs/bench/bench-a/not-a-bundle"} {
		if resp, _ := ts.get(t, p); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, resp.StatusCode)
		}
	}
	ts.srv.cfg.Runs = nil
	for _, p := range []string{"/api/v1/runs", "/runs"} {
		if resp, _ := ts.get(t, p); resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s without runs: %d", p, resp.StatusCode)
		}
	}
}
