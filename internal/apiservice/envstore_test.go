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
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/envstore"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// storeEnv is a test server with the environment store on a fake Kubernetes.
func storeEnv(t *testing.T) (*testEnv, *envstore.Store, *fake.Clientset) {
	t.Helper()
	te := newFullTestEnv(t)
	kube := fake.NewSimpleClientset()
	// What the API server gives each object and the fake clientset does not:
	// the orphan sweep leaves an object with no create time.
	kube.PrependReactor("create", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		cm := action.(k8stesting.CreateAction).GetObject().(*corev1.ConfigMap)
		if cm.CreationTimestamp.IsZero() {
			cm.CreationTimestamp = metav1.Now()
		}
		return false, nil, nil
	})
	store := envstore.New(kube.CoreV1().ConfigMaps("ate-env"), "ate-env")
	if err := store.Run(t.Context(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	te.srv.Store = store
	return te, store, kube
}

// createWithObject creates an environment and writes an object for it, as a
// create with a label does: the actor first, then the object with its uid.
func createWithObject(t *testing.T, te *testEnv, store *envstore.Store, id string) string {
	t.Helper()
	ctx := t.Context()
	if _, err := te.envClient.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: id}); err != nil {
		t.Fatalf("CreateEnvironment(%s): %v", id, err)
	}
	uids, err := te.srv.ActorUIDs(ctx, "ate-env")
	if err != nil || uids[id] == "" {
		t.Fatalf("ActorUIDs = %v, %v, want a uid for %s", uids, err, id)
	}
	if err := store.Put(ctx, envstore.Record{Atespace: "ate-env", ID: id, UID: uids[id], Labels: map[string]string{"run": id}}); err != nil {
		t.Fatal(err)
	}
	waitForObject(t, store, id, func(r envstore.Record, ok bool) bool { return ok && r.UID == uids[id] })
	return uids[id]
}

func waitForObject(t *testing.T, store *envstore.Store, id string, done func(envstore.Record, bool) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, ok, err := store.Get("ate-env", id)
		if err != nil {
			t.Fatal(err)
		}
		if done(r, ok) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the store did not reach the wanted state of %s; last: %+v, %v", id, r, ok)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestDeleteEnvironmentRemovesTheObject checks the delete half of EM-6: a
// delete of an environment removes its object.
func TestDeleteEnvironmentRemovesTheObject(t *testing.T) {
	te, store, _ := storeEnv(t)
	createWithObject(t, te, store, "env-1")
	if _, err := te.envClient.DeleteEnvironment(t.Context(), &ateenvv1alpha.DeleteEnvironmentRequest{Id: "env-1"}); err != nil {
		t.Fatal(err)
	}
	waitForObject(t, store, "env-1", func(_ envstore.Record, ok bool) bool { return !ok })
}

// TestDeleteEnvironmentSucceedsWhenTheObjectStays checks EM-6: the actor is
// the truth. A delete whose object cannot be removed has deleted the
// environment, and the orphan sweep then removes the object.
func TestDeleteEnvironmentSucceedsWhenTheObjectStays(t *testing.T) {
	te, store, kube := storeEnv(t)
	createWithObject(t, te, store, "env-1")

	refuse := true
	kube.PrependReactor("delete", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if refuse {
			return true, nil, errors.New("the Kubernetes API is down")
		}
		return false, nil, nil
	})
	if _, err := te.envClient.DeleteEnvironment(t.Context(), &ateenvv1alpha.DeleteEnvironmentRequest{Id: "env-1"}); err != nil {
		t.Fatalf("DeleteEnvironment = %v, want success: the actor is deleted", err)
	}
	if _, err := te.envClient.GetEnvironment(t.Context(), &ateenvv1alpha.GetEnvironmentRequest{Id: "env-1"}); err == nil {
		t.Fatal("the environment still exists after its delete")
	}
	if _, ok, _ := store.Get("ate-env", "env-1"); !ok {
		t.Fatal("the object is gone although its delete was refused")
	}

	refuse = false
	n, err := store.SweepOrphans(t.Context(), te.srv.ActorUIDs, 0, time.Now().Add(time.Hour))
	if n != 1 || err != nil {
		t.Fatalf("SweepOrphans = %d, %v, want the one orphan deleted", n, err)
	}
}

// TestDeleteEnvironmentWithoutAnObjectCallsNoKubernetes checks EM-3's cost
// side: an environment with no label and no deadline has no object, and its
// delete makes no call to the Kubernetes API with the store on.
func TestDeleteEnvironmentWithoutAnObjectCallsNoKubernetes(t *testing.T) {
	te, _, kube := storeEnv(t)
	ctx := t.Context()
	if _, err := te.envClient.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "plain"}); err != nil {
		t.Fatal(err)
	}
	kube.ClearActions()
	if _, err := te.envClient.DeleteEnvironment(ctx, &ateenvv1alpha.DeleteEnvironmentRequest{Id: "plain"}); err != nil {
		t.Fatal(err)
	}
	if actions := kube.Actions(); len(actions) != 0 {
		t.Errorf("the delete of an environment with no object made %d Kubernetes calls: %v", len(actions), actions)
	}
}

// TestDeleteEnvironmentLogsAnObjectItCannotRead: an object of the
// environment that holds no readable record is not deleted with it (the
// sweep removes it), and the delete says so in the log; it is not the same
// as an environment with no object.
func TestDeleteEnvironmentLogsAnObjectItCannotRead(t *testing.T) {
	te, store, kube := storeEnv(t)
	if _, err := te.envClient.CreateEnvironment(t.Context(), &ateenvv1alpha.CreateEnvironmentRequest{Id: "env-1"}); err != nil {
		t.Fatal(err)
	}
	broken := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: envstore.ObjectName("ate-env", "env-1"), Namespace: "ate-env",
			Labels: map[string]string{envstore.LabelAtespace: "ate-env"},
		},
		Data: map[string]string{"uid": "uid-1", "labels": "{"},
	}
	if _, err := kube.CoreV1().ConfigMaps("ate-env").Create(t.Context(), broken, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// Until the informer has the object, a read gives no record and no error.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, _, err := store.Get("ate-env", "env-1"); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the informer did not get the object")
		}
		time.Sleep(5 * time.Millisecond)
	}

	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	kube.ClearActions()
	if _, err := te.envClient.DeleteEnvironment(t.Context(), &ateenvv1alpha.DeleteEnvironmentRequest{Id: "env-1"}); err != nil {
		t.Fatalf("DeleteEnvironment = %v, want success: the actor is deleted", err)
	}
	if got := logged.String(); !strings.Contains(got, "ate-env/env-1") || !strings.Contains(got, "labels of object") {
		t.Errorf("log = %q, want a line that names the environment and why its object cannot be read", got)
	}
	for _, a := range kube.Actions() {
		if a.GetVerb() == "delete" {
			t.Error("DeleteEnvironment deleted an object it could not read; that is the sweep's")
		}
	}
}

// TestOrphanSweepReadsTheActorsOfTheControlPlane checks EM-7 with the
// server's own read of the actors: an object stays while its actor exists
// with the same uid, and goes when the actor is gone or is a new one with
// the same id.
func TestOrphanSweepReadsTheActorsOfTheControlPlane(t *testing.T) {
	te, store, _ := storeEnv(t)
	ctx := t.Context()
	createWithObject(t, te, store, "stays")
	createWithObject(t, te, store, "gone")
	first := createWithObject(t, te, store, "reused")
	later := time.Now().Add(time.Hour)

	if n, err := store.SweepOrphans(ctx, te.srv.ActorUIDs, 0, later); n != 0 || err != nil {
		t.Fatalf("SweepOrphans with each actor in place = %d, %v, want nothing deleted", n, err)
	}

	// The actors go without ate-env-api, as after a crash between the two
	// deletes. One id is then used again.
	for _, id := range []string{"gone", "reused"} {
		ref := &ateapipb.ObjectRef{Atespace: "ate-env", Name: id}
		if _, err := te.control.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: ref}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := te.envClient.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "reused"}); err != nil {
		t.Fatal(err)
	}
	uids, err := te.srv.ActorUIDs(ctx, "ate-env")
	if err != nil || uids["reused"] == "" || uids["reused"] == first {
		t.Fatalf("ActorUIDs = %v, %v, want a new uid for the reused id (was %s)", uids, err, first)
	}

	if n, err := store.SweepOrphans(ctx, te.srv.ActorUIDs, 0, later); n != 2 || err != nil {
		t.Fatalf("SweepOrphans = %d, %v, want the two orphans deleted", n, err)
	}
	waitForObject(t, store, "gone", func(_ envstore.Record, ok bool) bool { return !ok })
	waitForObject(t, store, "reused", func(_ envstore.Record, ok bool) bool { return !ok })
	if _, ok, _ := store.Get("ate-env", "stays"); !ok {
		t.Error("the sweep deleted the object of a live environment")
	}
}

// TestActorUIDsFailsWhenTheControlPlaneDoes: the sweep must not read "no
// actors" from a failed list.
func TestActorUIDsFailsWhenTheControlPlaneDoes(t *testing.T) {
	te := newFullTestEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if uids, err := te.srv.ActorUIDs(ctx, "ate-env"); err == nil {
		t.Errorf("ActorUIDs on a cancelled call = %v, nil, want an error", uids)
	}
}
