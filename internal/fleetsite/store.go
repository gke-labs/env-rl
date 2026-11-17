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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/agent-substrate/env/internal/inventory"
	"google.golang.org/api/iterator"
)

// inventoryPrefix and latestName place each cluster's snapshot:
// v1/inventory/<project>/<cluster>/latest.json, written by its reporter.
const (
	inventoryPrefix = "v1/inventory/"
	latestName      = "latest.json"
)

// maxSnapshotBytes bounds one snapshot read. A snapshot of 100,000 actors is
// about 50 MB; a larger object is refused rather than read into memory.
const maxSnapshotBytes = 128 << 20

// Object is one cluster's latest.json, the generation it was read at, and
// the time the store wrote that generation (the bucket's clock, not the
// reporter's).
type Object struct {
	Name       string
	Generation int64
	Updated    time.Time
}

// Store lists and reads the latest.json objects of the bucket layout. The
// site only reads: neither implementation writes.
type Store interface {
	// List returns every v1/inventory/<project>/<cluster>/latest.json.
	List(ctx context.Context) ([]Object, error)
	// Read returns the object's content at the generation List returned.
	Read(ctx context.Context, obj Object) ([]byte, error)
}

// splitLatest returns the project and cluster of a latest.json object name,
// or ok false when the name is not exactly
// v1/inventory/<project>/<cluster>/latest.json.
func splitLatest(name string) (project, cluster string, ok bool) {
	rest, found := strings.CutPrefix(name, inventoryPrefix)
	if !found {
		return "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[2] != latestName || parts[0] == "" || parts[1] == "" ||
		inventory.CheckPathSegment("project", parts[0]) != nil || inventory.CheckPathSegment("cluster", parts[1]) != nil {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// BucketStore reads a Cloud Storage bucket with the caller's credentials.
type BucketStore struct{ Bucket *storage.BucketHandle }

func (s BucketStore) List(ctx context.Context) ([]Object, error) {
	q := &storage.Query{Prefix: inventoryPrefix, MatchGlob: inventoryPrefix + "*/*/" + latestName}
	if err := q.SetAttrSelection([]string{"Name", "Generation", "Updated"}); err != nil {
		return nil, err
	}
	var out []Object
	it := s.Bucket.Objects(ctx, q)
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("listing %s: %w", inventoryPrefix, err)
		}
		if _, _, ok := splitLatest(attrs.Name); ok {
			out = append(out, Object{Name: attrs.Name, Generation: attrs.Generation, Updated: attrs.Updated})
		}
	}
}

func (s BucketStore) Read(ctx context.Context, obj Object) ([]byte, error) {
	r, err := s.Bucket.Object(obj.Name).Generation(obj.Generation).NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", obj.Name, err)
	}
	defer r.Close()
	return readLimited(r, obj.Name, maxSnapshotBytes)
}

// DirStore reads the bucket layout under a local directory, as
// ate-env-reporter -out-dir writes it. The generation is the file's
// modification time in nanoseconds; the reporter replaces the file by a
// rename on each tick. It leaves Updated empty: on one machine the
// reporter's clock is the site's, so generated_at serves.
type DirStore struct{ Dir string }

func (s DirStore) List(context.Context) ([]Object, error) {
	root := filepath.Join(s.Dir, filepath.FromSlash(inventoryPrefix))
	projects, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Object
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		clusters, err := os.ReadDir(filepath.Join(root, p.Name()))
		if err != nil {
			return nil, err
		}
		for _, c := range clusters {
			if !c.IsDir() {
				continue
			}
			info, err := os.Stat(filepath.Join(root, p.Name(), c.Name(), latestName))
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			out = append(out, Object{
				Name:       path.Join(inventoryPrefix, p.Name(), c.Name(), latestName),
				Generation: info.ModTime().UnixNano(),
			})
		}
	}
	return out, nil
}

func (s DirStore) Read(_ context.Context, obj Object) ([]byte, error) {
	if _, _, ok := splitLatest(obj.Name); !ok {
		return nil, fmt.Errorf("%q is not a latest.json object", obj.Name)
	}
	f, err := os.Open(filepath.Join(s.Dir, filepath.FromSlash(obj.Name)))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readLimited(f, obj.Name, maxSnapshotBytes)
}

func readLimited(r io.Reader, name string, limit int) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	if len(data) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, limit)
	}
	return data, nil
}
