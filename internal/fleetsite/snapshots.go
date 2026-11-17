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
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/agent-substrate/env/internal/inventory"
	"github.com/google/jsonschema-go/jsonschema"
)

// CacheTTL is how long a listing of the bucket serves requests before the
// next request lists it again. An object whose generation did not change is
// not read again.
const CacheTTL = 30 * time.Second

// StaleIntervals is how many reporter intervals a snapshot may age before
// the site marks its cluster stale.
const StaleIntervals = 3

// readers bounds the snapshots read at once in one refresh.
const readers = 8

// refreshTimeout bounds one listing and its reads. A refresh runs apart
// from the request that started it, so a closed tab does not cancel it.
const refreshTimeout = 20 * time.Second

// futureSkew is how far ahead of the site's clock a generated_at may be
// before the cluster counts as stale: a reporter with a fast clock would
// otherwise look fresh after it stops.
const futureSkew = time.Minute

// Cluster is one cluster's latest snapshot as the site last read it.
// Snapshot is nil when Err is set.
type Cluster struct {
	Project    string
	Cluster    string
	Generation int64
	Snapshot   *inventory.Snapshot
	// Err says why the object could not be used. It names no credential;
	// the detail goes to the log.
	Err string
	// ListedAt is when the listing that holds this entry was made.
	ListedAt time.Time
	// Updated is when the store wrote this generation, by the store's
	// clock; empty when the store does not say (DirStore).
	Updated time.Time
	// retry is set when the read failed, not the object: the next refresh
	// reads the same generation again.
	retry bool
}

// written is when the snapshot was written: the store's update time when it
// has one, so a reporter whose clock is off does not look stale or fresh,
// else generated_at.
func (c Cluster) written() time.Time {
	if !c.Updated.IsZero() {
		return c.Updated
	}
	return c.Snapshot.GeneratedAt
}

// Age returns how old the snapshot is at now; it is negative when the
// write time is ahead of now.
func (c Cluster) Age(now time.Time) time.Duration {
	if c.Snapshot == nil {
		return 0
	}
	return now.Sub(c.written())
}

// Stale reports whether the reporter missed StaleIntervals of its interval
// as of the listing that holds the snapshot, or the snapshot is older than
// that plus maxServedAge now (the listing itself is old, as when the bucket
// cannot be listed), or its write time is more than futureSkew ahead, or the
// snapshot could not be read. The listing's own age does not count against
// the reporter: a listing serves up to CacheTTL, longer than three default
// intervals.
func (c Cluster) Stale(now time.Time) bool {
	if c.Snapshot == nil {
		return true
	}
	limit := StaleIntervals * time.Duration(c.Snapshot.Reporter.IntervalSeconds) * time.Second
	at := c.ListedAt
	if at.IsZero() {
		at = now
	}
	atListing := at.Sub(c.written())
	return atListing > limit || atListing < -futureSkew || c.Age(now) > limit+maxServedAge
}

// maxServedAge is the oldest listing a request gets while a refresh runs.
// An older one, after the site sat idle, would show every cluster stale.
const maxServedAge = 2 * CacheTTL

// Snapshots caches the clusters' latest snapshots per instance. One
// refresh runs at a time; while it runs, requests get the last listing if
// it is recent.
type Snapshots struct {
	store Store
	now   func() time.Time

	mu       sync.Mutex
	listedAt time.Time
	clusters map[string]Cluster // by object name
	// started and completed count the refreshes; running is closed when the
	// refresh in flight ends, and nil when none is.
	started, completed int
	running            chan struct{}
	lastErr            error
	failedAt           time.Time // when lastErr was set
}

// NewSnapshots returns a cache over store. now is the clock; nil means
// time.Now.
func NewSnapshots(store Store, now func() time.Time) *Snapshots {
	if now == nil {
		now = time.Now
	}
	return &Snapshots{store: store, now: now, clusters: map[string]Cluster{}}
}

// Clusters returns every cluster, ordered by project and cluster. It lists
// the bucket when the last listing is older than CacheTTL or force is set,
// and reads only the objects whose generation changed. While a refresh
// runs, a request without force gets the last listing if it is younger
// than maxServedAge. A forced request waits for a refresh that started
// after it, and others wait for the next refresh to end; each waits at most
// until its own ctx ends.
func (s *Snapshots) Clusters(ctx context.Context, force bool) ([]Cluster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	age := now.Sub(s.listedAt)
	if !force && !s.listedAt.IsZero() && age < CacheTTL {
		return s.sorted(), nil
	}
	// After a failed refresh, serve the last listing for a cache period
	// instead of waiting on another listing that can hang as long.
	if !force && !s.listedAt.IsZero() && s.lastErr != nil && now.Sub(s.failedAt) < CacheTTL {
		return s.sorted(), nil
	}
	// The refresh this request needs: one that starts after it when forced,
	// else the next one to end.
	target := s.completed + 1
	if force {
		target = s.started + 1
	}
	if s.running == nil {
		s.start()
	}
	if !force && !s.listedAt.IsZero() && age < maxServedAge {
		return s.sorted(), nil
	}
	haveListing := !s.listedAt.IsZero()
	for s.completed < target {
		if s.running == nil {
			s.start()
		}
		running := s.running
		s.mu.Unlock()
		select {
		case <-running:
		case <-ctx.Done():
			s.mu.Lock()
			return nil, ctx.Err()
		}
		s.mu.Lock()
	}
	// The latest refresh failed: a forced request gets the error; another
	// gets the last listing, whose clusters then show stale.
	if s.lastErr != nil && (force || !haveListing) {
		return nil, s.lastErr
	}
	return s.sorted(), nil
}

// start begins a refresh. s.mu is held.
func (s *Snapshots) start() {
	s.started++
	s.running = make(chan struct{})
	go s.refresh(s.running)
}

// sorted returns the clusters by project and cluster. s.mu is held.
func (s *Snapshots) sorted() []Cluster {
	out := make([]Cluster, 0, len(s.clusters))
	for _, c := range s.clusters {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		return out[i].Cluster < out[j].Cluster
	})
	return out
}

// Cluster returns one cluster, with ok false when the bucket has no
// snapshot for it.
func (s *Snapshots) Cluster(ctx context.Context, project, cluster string, force bool) (Cluster, bool, error) {
	all, err := s.Clusters(ctx, force)
	if err != nil {
		return Cluster{}, false, err
	}
	for _, c := range all {
		if c.Project == project && c.Cluster == cluster {
			return c, true, nil
		}
	}
	return Cluster{}, false, nil
}

// refresh lists the objects, reads the changed ones and the ones whose last
// read failed, and closes done. A failed listing keeps the last clusters
// and is the error of the requests that waited for it.
func (s *Snapshots) refresh(done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
	defer cancel()
	s.mu.Lock()
	prev := s.clusters
	s.mu.Unlock()

	// The listing's time is when it started: a slow read does not add to
	// the snapshots' age at listing.
	listedAt := s.now()
	next, err := s.list(ctx, prev)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastErr = err
	if err == nil {
		s.listedAt = listedAt
		for k, c := range next {
			c.ListedAt = s.listedAt
			next[k] = c
		}
		s.clusters = next
	} else {
		s.failedAt = s.now()
		log.Printf("fleetsite: %v", err)
	}
	s.completed++
	s.running = nil
	close(done)
}

func (s *Snapshots) list(ctx context.Context, prev map[string]Cluster) (map[string]Cluster, error) {
	objs, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	next := make(map[string]Cluster, len(objs))
	var changed []Object
	for _, obj := range objs {
		if old, ok := prev[obj.Name]; ok && old.Generation == obj.Generation && !old.retry {
			next[obj.Name] = old
			continue
		}
		changed = append(changed, obj)
	}

	read := make([]Cluster, len(changed))
	sem := make(chan struct{}, readers)
	var wg sync.WaitGroup
	for i, obj := range changed {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			read[i] = s.load(ctx, obj)
		}()
	}
	wg.Wait()
	for i, obj := range changed {
		next[obj.Name] = read[i]
	}
	return next, nil
}

// load reads and decodes one object. A failure is the cluster's Err, so one
// bad object does not hide the other clusters.
func (s *Snapshots) load(ctx context.Context, obj Object) Cluster {
	project, cluster, ok := splitLatest(obj.Name)
	c := Cluster{Project: project, Cluster: cluster, Generation: obj.Generation, Updated: obj.Updated}
	if !ok {
		c.Err = "the object name is not v1/inventory/<project>/<cluster>/latest.json"
		return c
	}
	data, err := s.store.Read(ctx, obj)
	if err != nil {
		log.Printf("fleetsite: %v", err)
		c.Err = "the snapshot could not be read"
		c.retry = true
		return c
	}
	snap, err := decodeSnapshot(data)
	if err != nil {
		log.Printf("fleetsite: decoding %s: %v", obj.Name, err)
		c.Err = "the snapshot does not pass the schema"
		return c
	}
	// The path is the reporter's IAM prefix; a snapshot that names another
	// cluster would be shown under the wrong one.
	if snap.Project != project || snap.Cluster != cluster {
		c.Err = fmt.Sprintf("the snapshot names %s/%s, not the cluster of its path", snap.Project, snap.Cluster)
		return c
	}
	c.Snapshot = snap
	return c
}

// decodeSnapshot reads a snapshot as the site needs it: a field it does not
// know is ignored, so a reporter that writes a field added to v1 later does
// not turn its cluster into an error before the site is deployed again.
// Everything else that the schema and Snapshot.Validate check still holds.
// The reporter's side keeps inventory.DecodeSnapshot, which refuses unknown
// fields.
func decodeSnapshot(data []byte) (*inventory.Snapshot, error) {
	schema, err := openSchema()
	if err != nil {
		return nil, err
	}
	var instance any
	if err := json.Unmarshal(data, &instance); err != nil {
		return nil, err
	}
	if err := schema.Validate(instance); err != nil {
		return nil, fmt.Errorf("%s validation failed: %w", inventory.SnapshotSchema, err)
	}
	var snap inventory.Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, err
	}
	if err := snap.Validate(); err != nil {
		return nil, err
	}
	return &snap, nil
}

// openSchema resolves the snapshot schema without its
// "additionalProperties": false, so it checks what the schema requires and
// permits a field it does not name.
var openSchema = sync.OnceValues(func() (*jsonschema.Resolved, error) {
	raw, err := inventory.SchemaFS.ReadFile("v1/" + inventory.SnapshotSchema)
	if err != nil {
		return nil, err
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	permitUnknown(doc)
	if raw, err = json.Marshal(doc); err != nil {
		return nil, err
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return s.Resolve(nil)
})

// permitUnknown removes every "additionalProperties": false under v.
func permitUnknown(v any) {
	switch v := v.(type) {
	case map[string]any:
		if v["additionalProperties"] == false {
			delete(v, "additionalProperties")
		}
		for _, e := range v {
			permitUnknown(e)
		}
	case []any:
		for _, e := range v {
			permitUnknown(e)
		}
	}
}
