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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/agent-substrate/env/internal/inventory"
	"google.golang.org/api/option"
)

// golden returns the inventory package's golden snapshot for project and
// cluster, generated at generatedAt.
func golden(t *testing.T, project, cluster string, generatedAt time.Time) *inventory.Snapshot {
	t.Helper()
	f, err := os.Open("../inventory/testdata/snapshot.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	snap, err := inventory.DecodeSnapshot(f)
	if err != nil {
		t.Fatal(err)
	}
	snap.Project, snap.Cluster, snap.GeneratedAt = project, cluster, generatedAt
	return snap
}

// writeSnapshot writes snap where its reporter would, under dir.
func writeSnapshot(t *testing.T, dir string, snap *inventory.Snapshot) {
	t.Helper()
	var buf bytes.Buffer
	if err := snap.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	writeRaw(t, dir, snap.Project, snap.Cluster, buf.Bytes())
}

func writeRaw(t *testing.T, dir, project, cluster string, data []byte) {
	t.Helper()
	p := filepath.Join(dir, "v1", "inventory", project, cluster, "latest.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p); err != nil {
		t.Fatal(err)
	}
}

// countingStore counts the reads of a Store.
type countingStore struct {
	Store
	mu    sync.Mutex
	reads int
	err   error
}

func (s *countingStore) Read(ctx context.Context, obj Object) ([]byte, error) {
	s.mu.Lock()
	s.reads++
	s.mu.Unlock()
	return s.Store.Read(ctx, obj)
}

func (s *countingStore) List(ctx context.Context) ([]Object, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.Store.List(ctx)
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var t0 = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func TestSnapshotsListsEveryCluster(t *testing.T) {
	dir := t.TempDir()
	writeSnapshot(t, dir, golden(t, "proj-b", "c-1", t0))
	writeSnapshot(t, dir, golden(t, "proj-a", "c-2", t0))
	writeSnapshot(t, dir, golden(t, "proj-a", "c-1", t0))
	// Not latest.json, or not at its depth: ignored.
	writeRaw(t, dir, "proj-a", "c-1/extra", []byte("{}"))
	if err := os.WriteFile(filepath.Join(dir, "v1", "inventory", "proj-a", "c-1", "other.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	clock := &fakeClock{t: t0}
	s := NewSnapshots(DirStore{Dir: dir}, clock.now)
	got, err := s.Clusters(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range got {
		if c.Err != "" {
			t.Errorf("%s/%s: %s", c.Project, c.Cluster, c.Err)
		}
		names = append(names, c.Project+"/"+c.Cluster)
	}
	if want := []string{"proj-a/c-1", "proj-a/c-2", "proj-b/c-1"}; !equalStrings(names, want) {
		t.Errorf("clusters = %v, want %v", names, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// FS-1: a listing serves CacheTTL; after it, or with force, the next
// request lists again and reads only the objects whose generation moved.
func TestSnapshotsRevalidateByGeneration(t *testing.T) {
	dir := t.TempDir()
	writeSnapshot(t, dir, golden(t, "proj-a", "c-1", t0))
	writeSnapshot(t, dir, golden(t, "proj-a", "c-2", t0))
	store := &countingStore{Store: DirStore{Dir: dir}}
	clock := &fakeClock{t: t0}
	s := NewSnapshots(store, clock.now)
	ctx := context.Background()

	if _, err := s.Clusters(ctx, false); err != nil {
		t.Fatal(err)
	}
	if store.reads != 2 {
		t.Fatalf("first listing read %d objects, want 2", store.reads)
	}

	// c-1's reporter writes a new snapshot.
	time.Sleep(10 * time.Millisecond) // a new modification time
	writeSnapshot(t, dir, golden(t, "proj-a", "c-1", t0.Add(5*time.Second)))

	clock.add(CacheTTL / 2)
	c, _, _ := s.Cluster(ctx, "proj-a", "c-1", false)
	if !c.Snapshot.GeneratedAt.Equal(t0) || store.reads != 2 {
		t.Errorf("within the TTL: generated_at %s after %d reads, want the cached %s after 2", c.Snapshot.GeneratedAt, store.reads, t0)
	}

	c, _, _ = s.Cluster(ctx, "proj-a", "c-1", true)
	if !c.Snapshot.GeneratedAt.Equal(t0.Add(5*time.Second)) || store.reads != 3 {
		t.Errorf("forced: generated_at %s after %d reads, want the new snapshot after 3", c.Snapshot.GeneratedAt, store.reads)
	}

	clock.add(CacheTTL)
	if _, err := s.Clusters(ctx, false); err != nil {
		t.Fatal(err)
	}
	if store.reads != 3 {
		t.Errorf("an unchanged listing read again: %d reads, want 3", store.reads)
	}
}

// FS-9: a cluster is stale after three of its reporter's intervals.
func TestClusterStale(t *testing.T) {
	snap := golden(t, "p", "c", t0) // interval_s 5
	c := Cluster{Snapshot: snap}
	for _, tc := range []struct {
		after time.Duration
		stale bool
	}{
		{0, false}, {5 * time.Second, false}, {15 * time.Second, false}, {15*time.Second + time.Millisecond, true}, {time.Hour, true},
	} {
		if got := c.Stale(t0.Add(tc.after)); got != tc.stale {
			t.Errorf("after %s: stale %v, want %v", tc.after, got, tc.stale)
		}
	}
	// A reporter whose clock runs ahead looks fresh after it stops unless
	// a generated_at in the future counts as stale.
	ahead := Cluster{Snapshot: golden(t, "p", "c", t0.Add(5*time.Hour))}
	if !ahead.Stale(t0) {
		t.Error("a snapshot 5 h in the future is not stale")
	}
	if (Cluster{Snapshot: golden(t, "p", "c", t0.Add(30*time.Second))}).Stale(t0) {
		t.Error("a snapshot 30 s ahead, within the skew, is stale")
	}
	if !(Cluster{Err: "unreadable"}).Stale(t0) {
		t.Error("a cluster without a snapshot is not stale")
	}
}

// One bad object is that cluster's error; the other clusters still show.
func TestSnapshotsReportABadObjectPerCluster(t *testing.T) {
	dir := t.TempDir()
	writeSnapshot(t, dir, golden(t, "proj-a", "good", t0))
	writeRaw(t, dir, "proj-a", "garbage", []byte("not json"))
	writeRaw(t, dir, "proj-a", "unknown-field", []byte(`{"schema_version":"v1","extra":1}`))
	// A snapshot under another cluster's path.
	var buf bytes.Buffer
	if err := golden(t, "proj-a", "elsewhere", t0).Encode(&buf); err != nil {
		t.Fatal(err)
	}
	writeRaw(t, dir, "proj-a", "moved", buf.Bytes())

	s := NewSnapshots(DirStore{Dir: dir}, (&fakeClock{t: t0}).now)
	got, err := s.Clusters(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	errs := map[string]string{}
	for _, c := range got {
		errs[c.Cluster] = c.Err
		if (c.Err == "") != (c.Snapshot != nil) {
			t.Errorf("%s: err %q with snapshot %v", c.Cluster, c.Err, c.Snapshot != nil)
		}
	}
	if errs["good"] != "" {
		t.Errorf("good: %s", errs["good"])
	}
	for _, bad := range []string{"garbage", "unknown-field", "moved"} {
		if errs[bad] == "" {
			t.Errorf("%s: no error", bad)
		}
	}
}

// flakyStore fails the first failReads reads.
type flakyStore struct {
	Store
	mu        sync.Mutex
	failReads int
	block     chan struct{} // when set, List waits for it
}

func (s *flakyStore) Read(ctx context.Context, obj Object) ([]byte, error) {
	s.mu.Lock()
	fail := s.failReads > 0
	s.failReads--
	s.mu.Unlock()
	if fail {
		return nil, errors.New("503 from storage")
	}
	return s.Store.Read(ctx, obj)
}

func (s *flakyStore) List(ctx context.Context) ([]Object, error) {
	if s.block != nil {
		<-s.block
	}
	return s.Store.List(ctx)
}

// FS-9: a read that failed is tried again on the next refresh, even when
// the reporter wrote no new generation.
func TestSnapshotsRetryAFailedRead(t *testing.T) {
	dir := t.TempDir()
	writeSnapshot(t, dir, golden(t, "proj-a", "c-1", t0))
	s := NewSnapshots(&flakyStore{Store: DirStore{Dir: dir}, failReads: 1}, (&fakeClock{t: t0}).now)
	c, _, err := s.Cluster(context.Background(), "proj-a", "c-1", false)
	if err != nil || c.Err == "" || c.Snapshot != nil {
		t.Fatalf("first read: %+v, %v; want the read error", c, err)
	}
	c, _, err = s.Cluster(context.Background(), "proj-a", "c-1", true)
	if err != nil || c.Err != "" || c.Snapshot == nil {
		t.Errorf("next refresh: %+v, %v; want the snapshot", c, err)
	}
}

// A refresh does not run on the context of the request that started it:
// a closed request leaves the cache good for the next one.
func TestSnapshotsRefreshOutlivesItsRequest(t *testing.T) {
	dir := t.TempDir()
	writeSnapshot(t, dir, golden(t, "proj-a", "c-1", t0))
	block := make(chan struct{})
	s := NewSnapshots(&flakyStore{Store: DirStore{Dir: dir}, block: block}, (&fakeClock{t: t0}).now)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := s.Clusters(ctx, false)
		errc <- err
	}()
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request: %v", err)
	}
	close(block)
	c, _, err := s.Cluster(context.Background(), "proj-a", "c-1", false)
	if err != nil || c.Snapshot == nil {
		t.Errorf("after the cancelled request: %+v, %v; want the snapshot", c, err)
	}
}

// While a refresh runs, a request gets the last listing without waiting.
func TestSnapshotsServeTheLastListingDuringARefresh(t *testing.T) {
	dir := t.TempDir()
	writeSnapshot(t, dir, golden(t, "proj-a", "c-1", t0))
	store := &flakyStore{Store: DirStore{Dir: dir}}
	clock := &fakeClock{t: t0}
	s := NewSnapshots(store, clock.now)
	if _, err := s.Clusters(context.Background(), false); err != nil {
		t.Fatal(err)
	}

	block := make(chan struct{})
	store.mu.Lock()
	store.block = block
	store.mu.Unlock()
	clock.add(CacheTTL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := s.Clusters(ctx, false)
	if err != nil || len(got) != 1 || got[0].Snapshot == nil {
		t.Errorf("during the refresh: %+v, %v; want the last listing at once", got, err)
	}
	close(block)
}

// After the site sat idle, the first request waits for a fresh listing
// instead of showing every cluster stale (FS-9).
func TestSnapshotsWaitAfterIdle(t *testing.T) {
	dir := t.TempDir()
	writeSnapshot(t, dir, golden(t, "proj-a", "c-1", t0))
	clock := &fakeClock{t: t0}
	s := NewSnapshots(DirStore{Dir: dir}, clock.now)
	if _, err := s.Clusters(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	clock.add(10 * time.Hour)
	time.Sleep(10 * time.Millisecond)
	writeSnapshot(t, dir, golden(t, "proj-a", "c-1", t0.Add(10*time.Hour)))
	c, _, err := s.Cluster(context.Background(), "proj-a", "c-1", false)
	if err != nil || c.Snapshot == nil || c.Stale(clock.now()) {
		t.Errorf("first request after 10 h idle: %+v, %v; want the new, fresh snapshot", c, err)
	}
}

// pinnedStore serves each object at the generation its listing saw, as
// GCS does for a read by generation, and its first List waits for release
// after it listed.
type pinnedStore struct {
	Store
	listed  chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	content map[Object][]byte
}

func (s *pinnedStore) List(ctx context.Context) ([]Object, error) {
	objs, err := s.Store.List(ctx)
	for _, obj := range objs {
		data, rerr := s.Store.Read(ctx, obj)
		if rerr != nil {
			return nil, rerr
		}
		s.mu.Lock()
		if s.content == nil {
			s.content = map[Object][]byte{}
		}
		s.content[obj] = data
		s.mu.Unlock()
	}
	s.once.Do(func() {
		close(s.listed)
		<-s.release
	})
	return objs, err
}

func (s *pinnedStore) Read(_ context.Context, obj Object) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.content[obj]
	if !ok {
		return nil, errors.New("no such generation")
	}
	return data, nil
}

// FS-1: ?nocache=1 sees a write made before it, even when a refresh that
// listed earlier is still running.
func TestSnapshotsForceWaitsForALaterRefresh(t *testing.T) {
	dir := t.TempDir()
	writeSnapshot(t, dir, golden(t, "proj-a", "c-1", t0))
	store := &pinnedStore{Store: DirStore{Dir: dir}, listed: make(chan struct{}), release: make(chan struct{})}
	s := NewSnapshots(store, (&fakeClock{t: t0}).now)

	first := make(chan error, 1)
	go func() {
		_, err := s.Clusters(context.Background(), false)
		first <- err
	}()
	<-store.listed // the first refresh has listed the old generation
	time.Sleep(10 * time.Millisecond)
	writeSnapshot(t, dir, golden(t, "proj-a", "c-1", t0.Add(5*time.Second)))

	forced := make(chan Cluster, 1)
	go func() {
		c, _, _ := s.Cluster(context.Background(), "proj-a", "c-1", true)
		forced <- c
	}()
	time.Sleep(10 * time.Millisecond)
	close(store.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if c := <-forced; c.Snapshot == nil || !c.Snapshot.GeneratedAt.Equal(t0.Add(5*time.Second)) {
		t.Errorf("forced request: %+v; want the snapshot written before it", c)
	}
}

func TestFormatAgeAhead(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-5 * time.Hour: "5h ahead of the site clock", -30 * time.Second: "0s", 4 * time.Second: "4s",
	} {
		if got := formatAge(d); got != want {
			t.Errorf("formatAge(%s) = %q, want %q", d, got, want)
		}
	}
}

// FS-9: a healthy reporter at the default 5 s interval stays fresh for the
// whole cache period; a dead one shows stale at the next listing, and stale
// anyway once the listing itself is old.
func TestClusterStaleAllowsForTheListingAge(t *testing.T) {
	listed := t0
	healthy := Cluster{Snapshot: golden(t, "p", "c", listed.Add(-2*time.Second)), ListedAt: listed}
	if healthy.Stale(listed.Add(CacheTTL - time.Second)) {
		t.Error("a snapshot 2 s old at its listing is stale before the next listing")
	}
	dead := Cluster{Snapshot: golden(t, "p", "c", listed.Add(-20*time.Second)), ListedAt: listed}
	if !dead.Stale(listed) {
		t.Error("a snapshot 20 s old at its listing (interval 5 s) is not stale")
	}
	if !healthy.Stale(listed.Add(15*time.Second + maxServedAge + 2*time.Second)) {
		t.Error("an old listing keeps a snapshot fresh forever")
	}
}

// A failed listing after the last one went old gives the last listing, its
// clusters stale, to a request without force; a forced one gets the error.
func TestSnapshotsKeepTheLastListingWhenListingFails(t *testing.T) {
	dir := t.TempDir()
	writeSnapshot(t, dir, golden(t, "proj-a", "c-1", t0))
	store := &countingStore{Store: DirStore{Dir: dir}}
	clock := &fakeClock{t: t0}
	s := NewSnapshots(store, clock.now)
	if _, err := s.Clusters(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	store.err = errors.New("503")
	clock.add(time.Hour)
	got, err := s.Clusters(context.Background(), false)
	if err != nil || len(got) != 1 || !got[0].Stale(clock.now()) {
		t.Errorf("outage, not forced: %+v, %v; want the last listing, stale", got, err)
	}
	if _, err := s.Clusters(context.Background(), true); err == nil {
		t.Error("outage, forced: no error")
	}

	// Within a cache period of the failure, a request does not wait on
	// another listing, which could hang as long.
	block := make(chan struct{})
	defer close(block)
	s.store = &flakyStore{Store: store, block: block}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if got, err := s.Clusters(ctx, false); err != nil || len(got) != 1 {
		t.Errorf("right after the failure: %+v, %v; want the last listing at once", got, err)
	}
}

// A field that the site does not know is ignored, so a reporter that
// writes a later v1 field does not turn its cluster into an error.
func TestSnapshotsAcceptAnUnknownField(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	if err := golden(t, "proj-a", "c-1", t0).Encode(&buf); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	doc["added_later"] = map[string]any{"x": 1}
	doc["workers"].([]any)[0].(map[string]any)["zone"] = "us-central1-a"
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	writeRaw(t, dir, "proj-a", "c-1", data)
	c, _, err := NewSnapshots(DirStore{Dir: dir}, (&fakeClock{t: t0}).now).Cluster(context.Background(), "proj-a", "c-1", false)
	if err != nil || c.Err != "" || c.Snapshot == nil {
		t.Errorf("snapshot with unknown fields: %+v, %v; want it read", c, err)
	}
	// What Validate checks still holds.
	doc["counts"] = map[string]any{"actors_by_state": map[string]any{}, "actors_by_atespace": map[string]any{}}
	data, _ = json.Marshal(doc)
	writeRaw(t, dir, "proj-a", "c-2", append([]byte(nil), bytes.ReplaceAll(data, []byte(`"cluster":"c-1"`), []byte(`"cluster":"c-2"`))...))
	c, _, _ = NewSnapshots(DirStore{Dir: dir}, (&fakeClock{t: t0}).now).Cluster(context.Background(), "proj-a", "c-2", false)
	if c.Err == "" {
		t.Error("a snapshot whose counts do not match its actors is read")
	}
}

// staticStore lists fixed objects and serves fixed content; it counts the
// reads in flight and remembers the List deadline.
type staticStore struct {
	objs     []Object
	data     map[string][]byte
	mu       sync.Mutex
	inFlight int
	peak     int
	deadline time.Duration
}

func (s *staticStore) List(ctx context.Context) ([]Object, error) {
	if d, ok := ctx.Deadline(); ok {
		s.mu.Lock()
		s.deadline = time.Until(d)
		s.mu.Unlock()
	}
	return s.objs, nil
}

func (s *staticStore) Read(_ context.Context, obj Object) ([]byte, error) {
	s.mu.Lock()
	s.inFlight++
	s.peak = max(s.peak, s.inFlight)
	s.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	s.mu.Lock()
	s.inFlight--
	s.mu.Unlock()
	return s.data[obj.Name], nil
}

// A refresh reads at most `readers` objects at once and runs under its own
// refreshTimeout.
func TestSnapshotsRefreshBounds(t *testing.T) {
	store := &staticStore{data: map[string][]byte{}}
	for i := range 40 {
		var buf bytes.Buffer
		snap := golden(t, "proj-a", fmt.Sprintf("c-%d", i), t0)
		if err := snap.Encode(&buf); err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("v1/inventory/proj-a/c-%d/latest.json", i)
		store.objs = append(store.objs, Object{Name: name, Generation: 1})
		store.data[name] = buf.Bytes()
	}
	got, err := NewSnapshots(store, (&fakeClock{t: t0}).now).Clusters(context.Background(), false)
	if err != nil || len(got) != 40 {
		t.Fatalf("%d clusters, %v", len(got), err)
	}
	if store.peak > readers || store.peak < 2 {
		t.Errorf("peak reads in flight = %d, want 2..%d", store.peak, readers)
	}
	if store.deadline <= refreshTimeout-5*time.Second || store.deadline > refreshTimeout {
		t.Errorf("List deadline in %s, want about %s", store.deadline, refreshTimeout)
	}
}

// FS-9: with the store's update time, a reporter whose clock is behind does
// not show stale, and one whose writes stopped does.
func TestClusterStaleUsesTheStoreTime(t *testing.T) {
	skewed := golden(t, "p", "c", t0.Add(-time.Hour)) // the reporter's clock is an hour behind
	c := Cluster{Snapshot: skewed, Updated: t0.Add(-2 * time.Second), ListedAt: t0}
	if c.Stale(t0) || c.Age(t0) != 2*time.Second {
		t.Errorf("written 2 s ago by the store's clock: stale %v, age %s", c.Stale(t0), c.Age(t0))
	}
	c.Updated = t0.Add(-time.Minute)
	if !c.Stale(t0) {
		t.Error("written a minute ago (interval 5 s): not stale")
	}
}

func TestReadLimited(t *testing.T) {
	if _, err := readLimited(strings.NewReader("12345"), "x", 4); err == nil {
		t.Error("5 bytes read under a limit of 4")
	}
	if data, err := readLimited(strings.NewReader("1234"), "x", 4); err != nil || string(data) != "1234" {
		t.Errorf("4 bytes under a limit of 4: %q, %v", data, err)
	}
	if maxSnapshotBytes != 128<<20 {
		t.Errorf("maxSnapshotBytes = %d, want 128 MiB", maxSnapshotBytes)
	}
}

// DirStore.Read refuses a name outside v1/inventory/<project>/<cluster>/
// even when the file exists.
func TestDirStoreReadRefusesAnExistingFileElsewhere(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"v1/x/latest.json", "secret/latest.json"} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"v1/inventory/../x/latest.json", "v1/inventory/../../secret/latest.json"} {
		if data, err := (DirStore{Dir: dir}).Read(context.Background(), Object{Name: name}); err == nil {
			t.Errorf("Read(%q) = %q", name, data)
		}
	}
}

func TestSnapshotsListError(t *testing.T) {
	store := &countingStore{Store: DirStore{Dir: t.TempDir()}, err: errors.New("403")}
	if _, err := NewSnapshots(store, nil).Clusters(context.Background(), false); err == nil {
		t.Error("a failed listing is not an error")
	}
}

func TestDirStoreReadRefusesOtherNames(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"../../etc/passwd", "v1/inventory/a/../../../x/latest.json", "v1/inventory/../x/latest.json", "v1/inventory/a/../latest.json", "v1/inventory/a/latest.json", "v1/snapshots/a/b/latest.json"} {
		if _, err := (DirStore{Dir: dir}).Read(context.Background(), Object{Name: name}); err == nil {
			t.Errorf("Read(%q) succeeded", name)
		}
	}
}

func TestDirStoreWithoutInventory(t *testing.T) {
	objs, err := DirStore{Dir: t.TempDir()}.List(context.Background())
	if err != nil || len(objs) != 0 {
		t.Errorf("List = %v, %v; want none", objs, err)
	}
}

// A field that only the schema requires is still checked: a worker without
// capacity is an error, not a zero.
func TestSnapshotsApplyTheSchema(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	if err := golden(t, "proj-a", "c-1", t0).Encode(&buf); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc["workers"].([]any)[0].(map[string]any), "capacity")
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inventory.DecodeSnapshot(bytes.NewReader(data)); err == nil {
		t.Fatal("inventory.DecodeSnapshot reads a worker without capacity")
	}
	writeRaw(t, dir, "proj-a", "c-1", data)
	c, _, err := NewSnapshots(DirStore{Dir: dir}, (&fakeClock{t: t0}).now).Cluster(context.Background(), "proj-a", "c-1", false)
	if err != nil || c.Err == "" || c.Snapshot != nil || !c.Stale(t0) {
		t.Errorf("worker without capacity: %+v, %v; want an error", c, err)
	}
}

// FS-9: the update time the store lists reaches the stale rule and the age,
// over generated_at.
func TestSnapshotsUseTheListedUpdateTime(t *testing.T) {
	store := &staticStore{data: map[string][]byte{}}
	for _, tc := range []struct {
		cluster          string
		generated, wrote time.Time
	}{
		{"behind", t0.Add(-time.Hour), t0.Add(-2 * time.Second)}, // the reporter's clock is an hour behind
		{"stopped", t0, t0.Add(-time.Minute)},                    // the reporter's clock is right but its writes stopped
	} {
		var buf bytes.Buffer
		if err := golden(t, "proj-a", tc.cluster, tc.generated).Encode(&buf); err != nil {
			t.Fatal(err)
		}
		name := "v1/inventory/proj-a/" + tc.cluster + "/latest.json"
		store.objs = append(store.objs, Object{Name: name, Generation: 1, Updated: tc.wrote})
		store.data[name] = buf.Bytes()
	}
	s := NewSnapshots(store, (&fakeClock{t: t0}).now)
	c, _, err := s.Cluster(context.Background(), "proj-a", "behind", false)
	if err != nil || c.Snapshot == nil || c.Stale(t0) || c.Age(t0) != 2*time.Second {
		t.Errorf("written 2 s ago by the store's clock: %v, stale %v, age %s", err, c.Stale(t0), c.Age(t0))
	}
	c, _, err = s.Cluster(context.Background(), "proj-a", "stopped", false)
	if err != nil || c.Snapshot == nil || !c.Stale(t0) || c.Age(t0) != time.Minute {
		t.Errorf("written a minute ago by the store's clock: %v, stale %v, age %s", err, c.Stale(t0), c.Age(t0))
	}
}

// BucketStore.List returns each object's update time from the bucket.
func TestBucketStoreListsTheUpdateTime(t *testing.T) {
	updated := t0.Add(-7 * time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/storage/v1/b/inv/o" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{
			"name": "v1/inventory/proj-a/c-1/latest.json", "generation": "5", "updated": updated.Format(time.RFC3339Nano),
		}}})
	}))
	defer srv.Close()
	client, err := storage.NewClient(context.Background(),
		option.WithEndpoint(srv.URL+"/storage/v1/"), option.WithoutAuthentication(), option.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	objs, err := BucketStore{Bucket: client.Bucket("inv")}.List(context.Background())
	if err != nil || len(objs) != 1 || objs[0].Generation != 5 || !objs[0].Updated.Equal(updated) {
		t.Errorf("List = %+v, %v; want generation 5 updated %s", objs, err, updated)
	}
}
