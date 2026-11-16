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
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/envstore"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

// TestCheckEnvironmentStoreFlags: a wrong value of a store flag is an error
// of the start, before the server waits for the control plane.
func TestCheckEnvironmentStoreFlags(t *testing.T) {
	for _, tc := range []struct {
		store string
		qps   float64
		burst int
		ok    bool
	}{
		{"", defaultEnvironmentStoreQPS, defaultEnvironmentStoreBurst, true},
		{"configmaps", defaultEnvironmentStoreQPS, defaultEnvironmentStoreBurst, true},
		{"configmaps", -1, 0, true},
		{"bogus", defaultEnvironmentStoreQPS, defaultEnvironmentStoreBurst, false},
		{"ConfigMaps", defaultEnvironmentStoreQPS, defaultEnvironmentStoreBurst, false},
		// client-go reads zero as its own default, 5 and 10.
		{"configmaps", 0, 100, false},
		{"configmaps", 50, 0, false},
		// With the store off the limits are not used.
		{"", 0, 0, true},
	} {
		err := checkEnvironmentStoreFlags(tc.store, tc.qps, tc.burst)
		if (err == nil) != tc.ok {
			t.Errorf("checkEnvironmentStoreFlags(%q, %v, %d) = %v, want ok = %v", tc.store, tc.qps, tc.burst, err, tc.ok)
		}
	}
}

func TestEnvironmentStoreClientConfig(t *testing.T) {
	in := &rest.Config{Host: "https://kubernetes.default.svc"}
	got := environmentStoreClientConfig(in, defaultEnvironmentStoreQPS, defaultEnvironmentStoreBurst)
	if got.QPS != 50 || got.Burst != 100 || got.Host != in.Host {
		t.Errorf("config = QPS %v, burst %d, host %q, want 50, 100 and the host of the input", got.QPS, got.Burst, got.Host)
	}
	if in.QPS != 0 || in.Burst != 0 {
		t.Errorf("the input config changed: QPS %v, burst %d", in.QPS, in.Burst)
	}
	// client-go reads a negative QPS as no limit in the client.
	if got := environmentStoreClientConfig(in, -1, 100); got.QPS >= 0 {
		t.Errorf("with a negative value: QPS %v, want a negative QPS", got.QPS)
	}
	if got := environmentStoreClientConfig(in, 7.5, 3); got.QPS != 7.5 || got.Burst != 3 {
		t.Errorf("config = QPS %v, burst %d, want the given 7.5 and 3", got.QPS, got.Burst)
	}
}

// TestEnvironmentStoreDeletesAreNotHeldByClientGoDefaultLimit: with
// client-go's default limit, 5 requests per second after a burst of 10, the
// 40 object deletes below take 6 s. DeleteEnvironment deletes one object
// inside the RPC, thus a fleet teardown would wait on the client, not on
// Kubernetes. The store's client has a limit of its own.
func TestEnvironmentStoreDeletesAreNotHeldByClientGoDefaultLimit(t *testing.T) {
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("request %s %s, want only deletes", r.Method, r.URL.Path)
		}
		served.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Success"}`))
	}))
	defer srv.Close()
	config := environmentStoreClientConfig(&rest.Config{Host: srv.URL}, defaultEnvironmentStoreQPS, defaultEnvironmentStoreBurst)
	client, err := corev1client.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	store := envstore.New(client.ConfigMaps("ate-env"), "ate-env")

	const deletes = 40
	var wg sync.WaitGroup
	var failed atomic.Int64
	start := time.Now()
	for range deletes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if err := store.Delete(ctx, envstore.Record{Atespace: "team", ID: "env-1"}); err != nil {
				failed.Add(1)
			}
		}()
	}
	wg.Wait()
	if took := time.Since(start); took > 3*time.Second || failed.Load() != 0 || served.Load() != deletes {
		t.Errorf("%d deletes took %s, %d failed, %d reached the server; want all served in less than the 6 s of client-go's default limit", deletes, took.Round(time.Millisecond), failed.Load(), served.Load())
	}
}
