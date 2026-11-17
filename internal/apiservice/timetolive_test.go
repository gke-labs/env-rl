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
	"fmt"
	"log"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/apiservice"
	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/envstore"
	"github.com/agent-substrate/env/internal/telemetry"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var configMaps = schema.GroupResource{Resource: "configmaps"}

// likeTheAPIServer makes the fake clientset do for ConfigMaps what the API
// server does and the fake does not: a create gives a uid, a resource
// version and a create time; an update is refused for another resource
// version or uid, and gives a new resource version; a delete is refused when
// its preconditions do not hold.
func likeTheAPIServer(kube *fake.Clientset) {
	var n atomic.Int64
	current := func(action k8stesting.Action, name string) *corev1.ConfigMap {
		obj, err := kube.Tracker().Get(action.GetResource(), action.GetNamespace(), name)
		if err != nil {
			return nil
		}
		return obj.(*corev1.ConfigMap)
	}
	kube.PrependReactor("create", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		cm := action.(k8stesting.CreateAction).GetObject().(*corev1.ConfigMap)
		i := n.Add(1)
		cm.UID = types.UID(fmt.Sprintf("object-uid-%d", i))
		cm.ResourceVersion = strconv.FormatInt(i, 10)
		cm.CreationTimestamp = metav1.Now()
		return false, nil, nil
	})
	kube.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		cm := action.(k8stesting.UpdateAction).GetObject().(*corev1.ConfigMap)
		cur := current(action, cm.Name)
		if cur == nil {
			return true, nil, apierrors.NewNotFound(configMaps, cm.Name)
		}
		if cm.ResourceVersion != cur.ResourceVersion || cm.UID != cur.UID {
			return true, nil, apierrors.NewConflict(configMaps, cm.Name, errors.New("the object has been modified"))
		}
		cm.ResourceVersion = strconv.FormatInt(n.Add(1), 10)
		cm.CreationTimestamp = cur.CreationTimestamp
		return false, nil, nil
	})
	kube.PrependReactor("delete", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		del := action.(k8stesting.DeleteActionImpl)
		cur, pre := current(action, del.Name), del.DeleteOptions.Preconditions
		if cur == nil || pre == nil {
			return false, nil, nil
		}
		if (pre.UID != nil && *pre.UID != cur.UID) || (pre.ResourceVersion != nil && *pre.ResourceVersion != cur.ResourceVersion) {
			return true, nil, apierrors.NewConflict(configMaps, del.Name, errors.New("precondition failed"))
		}
		return false, nil, nil
	})
}

// ttlEnv is a test server with the environment store on a fake Kubernetes
// that refuses what the API server refuses (likeTheAPIServer).
func ttlEnv(t *testing.T) (*testEnv, *envstore.Store, *fake.Clientset) {
	t.Helper()
	te := newFullTestEnv(t)
	kube := fake.NewSimpleClientset()
	likeTheAPIServer(kube)
	te.srv.Store = runStore(t, kube)
	return te, te.srv.Store, kube
}

func runStore(t *testing.T, kube *fake.Clientset) *envstore.Store {
	t.Helper()
	store := envstore.New(kube.CoreV1().ConfigMaps("ate-env"), "ate-env")
	if err := store.Run(t.Context(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	return store
}

// lateEnv is ttlEnv with an informer that gets only the objects the test
// feeds it: the informer of a replica that is behind the writes of another.
// feed gives the informer the object of an environment as Kubernetes has it.
func lateEnv(t *testing.T) (te *testEnv, kube *fake.Clientset, feed func(id string)) {
	t.Helper()
	te = newFullTestEnv(t)
	kube = fake.NewSimpleClientset()
	likeTheAPIServer(kube)
	events := watch.NewFakeWithChanSize(100, false)
	kube.PrependWatchReactor("configmaps", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, events, nil
	})
	te.srv.Store = runStore(t, kube)
	fed := map[string]bool{}
	feed = func(id string) {
		t.Helper()
		cm := object(t, kube, id)
		if cm == nil {
			t.Fatalf("no object of %s to feed the informer", id)
		}
		if fed[id] {
			events.Modify(cm)
		} else {
			events.Add(cm)
		}
		fed[id] = true
		waitForObject(t, te.srv.Store, id, func(r envstore.Record, ok bool) bool {
			if want := cm.Data["expire_time"]; want != "" {
				return ok && r.ExpireTime.UTC().Format(time.RFC3339Nano) == want
			}
			return ok && r.ExpireTime.IsZero()
		})
	}
	return te, kube, feed
}

// object returns the object of an environment as Kubernetes has it, or nil.
func object(t *testing.T, kube *fake.Clientset, id string) *corev1.ConfigMap {
	t.Helper()
	cm, err := kube.CoreV1().ConfigMaps("ate-env").Get(t.Context(), envstore.ObjectName("ate-env", id), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return cm
}

// createTTL creates an environment with a time to live and returns the
// environment of the answer. createTTLAndWait also waits until the informer
// has its object.
func createTTL(t *testing.T, te *testEnv, id string, ttl time.Duration) *ateenvv1alpha.Environment {
	t.Helper()
	resp, err := te.envClient.CreateEnvironment(t.Context(), &ateenvv1alpha.CreateEnvironmentRequest{Id: id, TimeToLive: durationpb.New(ttl)})
	if err != nil {
		t.Fatalf("CreateEnvironment(%s, time_to_live %s): %v", id, ttl, err)
	}
	return resp.GetEnvironment()
}

func createTTLAndWait(t *testing.T, te *testEnv, id string, ttl time.Duration) *ateenvv1alpha.Environment {
	t.Helper()
	env := createTTL(t, te, id, ttl)
	want := env.GetExpireTime().AsTime()
	waitForObject(t, te.srv.Store, id, func(r envstore.Record, ok bool) bool { return ok && r.ExpireTime.Equal(want) })
	return env
}

func exists(t *testing.T, te *testEnv, id string) bool {
	t.Helper()
	_, err := te.envClient.GetEnvironment(t.Context(), &ateenvv1alpha.GetEnvironmentRequest{Id: id})
	if err != nil && status.Code(err) != codes.NotFound {
		t.Fatalf("GetEnvironment(%s): %v", id, err)
	}
	return err == nil
}

func actorUID(t *testing.T, te *testEnv, id string) string {
	t.Helper()
	uids, err := te.srv.ActorUIDs(t.Context(), "ate-env")
	if err != nil {
		t.Fatal(err)
	}
	return uids[id]
}

// recreateBehind deletes the actor of an environment and creates the id
// again in the control plane, without ate-env-api: a new environment with
// the same id and another uid.
func recreateBehind(t *testing.T, te *testEnv, id string) {
	t.Helper()
	ctx := context.Background()
	ref := &ateapipb.ObjectRef{Atespace: "ate-env", Name: id}
	if _, err := te.control.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: ref}); err != nil {
		t.Errorf("deleting the actor %s behind the server: %v", id, err)
	}
	_, err := te.control.CreateActor(ctx, &ateapipb.CreateActorRequest{Actor: &ateapipb.Actor{
		Metadata:      &ateapipb.ResourceMetadata{Atespace: "ate-env", Name: id},
		ActorTemplate: &ateapipb.ObjectRef{Atespace: "ate-env", Name: apiservice.DefaultTemplate},
	}})
	if err != nil {
		t.Errorf("creating the actor %s again behind the server: %v", id, err)
	}
}

func kubeWrites(kube *fake.Clientset) []string {
	var out []string
	for _, a := range kube.Actions() {
		out = append(out, a.GetVerb())
	}
	return out
}

// TestTimeToLiveIsRefusedWithoutTheStore checks the time-to-live half of
// EM-1 and the server half of EM-16: with the store off, a create with a
// time to live and a SetEnvironmentTimeToLive are FailedPrecondition and
// create or change nothing, and GetServerInfo does not list the feature.
func TestTimeToLiveIsRefusedWithoutTheStore(t *testing.T) {
	te := newFullTestEnv(t)
	ctx := t.Context()

	_, err := te.envClient.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "env-1", TimeToLive: durationpb.New(time.Minute)})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("CreateEnvironment with a time to live = %v, want FailedPrecondition", err)
	}
	if n := te.control.Creates(); n != 0 || te.control.ActorCount() != 0 {
		t.Errorf("the refused create made %d CreateActor calls and left %d actors, want none", n, te.control.ActorCount())
	}

	// No time to live, or zero, is a create as before.
	if _, err := te.envClient.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "env-1", TimeToLive: durationpb.New(0)}); err != nil {
		t.Fatalf("CreateEnvironment with a zero time to live = %v, want success", err)
	}
	for _, ttl := range []time.Duration{time.Minute, 0} {
		_, err = te.envClient.SetEnvironmentTimeToLive(ctx, &ateenvv1alpha.SetEnvironmentTimeToLiveRequest{Id: "env-1", TimeToLive: durationpb.New(ttl)})
		if status.Code(err) != codes.FailedPrecondition {
			t.Errorf("SetEnvironmentTimeToLive(%s) = %v, want FailedPrecondition", ttl, err)
		}
	}
	got, err := te.envClient.GetEnvironment(ctx, &ateenvv1alpha.GetEnvironmentRequest{Id: "env-1"})
	if err != nil || got.GetEnvironment().GetExpireTime() != nil {
		t.Errorf("GetEnvironment = %v, %v, want the environment with no expire_time", got, err)
	}

	info, err := te.envClient.GetServerInfo(ctx, &ateenvv1alpha.GetServerInfoRequest{})
	if err != nil || slices.Contains(info.GetFeatures(), apiservice.FeatureTimeToLive) {
		t.Errorf("GetServerInfo = %v, %v, want no %q", info.GetFeatures(), err, apiservice.FeatureTimeToLive)
	}
}

// TestServerInfoReportsTimeToLive checks the server half of EM-16: with the
// store on, GetServerInfo lists "time_to_live".
func TestServerInfoReportsTimeToLive(t *testing.T) {
	te, _, _ := ttlEnv(t)
	info, err := te.envClient.GetServerInfo(t.Context(), &ateenvv1alpha.GetServerInfoRequest{})
	if err != nil || !slices.Contains(info.GetFeatures(), "time_to_live") {
		t.Errorf("GetServerInfo = %v, %v, want \"time_to_live\"", info.GetFeatures(), err)
	}
}

// TestCreateWithTimeToLiveReportsTheDeadline checks EM-9's expire_time and
// EM-D: Environment.expire_time is the create time plus the time to live,
// the answer of the create holds it, and GetEnvironment and
// ListEnvironments give it from the informer with no call to Kubernetes.
func TestCreateWithTimeToLiveReportsTheDeadline(t *testing.T) {
	te, _, kube := ttlEnv(t)
	ctx := t.Context()

	env := createTTLAndWait(t, te, "env-1", 90*time.Second)
	want := env.GetCreateTime().AsTime().Add(90 * time.Second)
	if env.GetCreateTime() == nil || !env.GetExpireTime().AsTime().Equal(want) {
		t.Fatalf("create: expire_time = %v, want the create time %v plus 90 s", env.GetExpireTime().AsTime(), env.GetCreateTime().AsTime())
	}
	if cm := object(t, kube, "env-1"); cm == nil || cm.Data["uid"] != actorUID(t, te, "env-1") || cm.Labels[envstore.LabelDeadline] != envstore.DeadlineSet {
		t.Errorf("object = %+v, want one with the uid of the actor and the deadline label", cm)
	}
	if _, err := te.envClient.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "plain"}); err != nil {
		t.Fatal(err)
	}

	kube.ClearActions()
	got, err := te.envClient.GetEnvironment(ctx, &ateenvv1alpha.GetEnvironmentRequest{Id: "env-1"})
	if err != nil || !got.GetEnvironment().GetExpireTime().AsTime().Equal(want) {
		t.Errorf("GetEnvironment: expire_time = %v, %v, want %v", got.GetEnvironment().GetExpireTime(), err, want)
	}
	list, err := te.envClient.ListEnvironments(ctx, &ateenvv1alpha.ListEnvironmentsRequest{})
	if err != nil || len(list.GetEnvironments()) != 2 {
		t.Fatalf("ListEnvironments = %v, %v, want the two environments", list, err)
	}
	for _, e := range list.GetEnvironments() {
		switch {
		case e.GetId() == "env-1" && !e.GetExpireTime().AsTime().Equal(want):
			t.Errorf("ListEnvironments: expire_time of env-1 = %v, want %v", e.GetExpireTime(), want)
		case e.GetId() == "plain" && e.GetExpireTime() != nil:
			t.Errorf("ListEnvironments: expire_time of an environment with no deadline = %v", e.GetExpireTime())
		}
	}
	if calls := kubeWrites(kube); len(calls) != 0 {
		t.Errorf("the reads made the Kubernetes calls %v, want none", calls)
	}

	// EM-9, EM-10: a negative value is refused before the actor.
	before := te.control.Creates()
	_, err = te.envClient.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "env-2", TimeToLive: durationpb.New(-time.Second)})
	if status.Code(err) != codes.InvalidArgument || te.control.Creates() != before {
		t.Errorf("create with a negative time to live = %v after %d CreateActor calls, want InvalidArgument and none", err, te.control.Creates()-before)
	}
}

// TestNoTimeToLiveMakesNoKubernetesCall checks EM-3 and its cost side: with
// the store on, an environment created with no time to live has no object,
// and its create, get, list and delete make no call to the Kubernetes API.
func TestNoTimeToLiveMakesNoKubernetesCall(t *testing.T) {
	te, store, kube := ttlEnv(t)
	ctx := t.Context()
	kube.ClearActions()

	for _, req := range []*ateenvv1alpha.CreateEnvironmentRequest{{Id: "plain"}, {Id: "zero", TimeToLive: durationpb.New(0)}} {
		resp, err := te.envClient.CreateEnvironment(ctx, req)
		if err != nil || resp.GetEnvironment().GetExpireTime() != nil {
			t.Fatalf("CreateEnvironment(%s) = %v, %v, want an environment with no expire_time", req.GetId(), resp, err)
		}
	}
	if _, err := te.envClient.GetEnvironment(ctx, &ateenvv1alpha.GetEnvironmentRequest{Id: "plain"}); err != nil {
		t.Fatal(err)
	}
	if _, err := te.envClient.ListEnvironments(ctx, &ateenvv1alpha.ListEnvironmentsRequest{}); err != nil {
		t.Fatal(err)
	}
	if n, err := te.srv.SweepDeadlines(ctx, time.Now().Add(1000*time.Hour)); n != 0 || err != nil {
		t.Errorf("SweepDeadlines = %d, %v, want nothing: no environment has a deadline", n, err)
	}
	for _, id := range []string{"plain", "zero"} {
		if _, ok, _ := store.Get("ate-env", id); ok || object(t, kube, id) != nil {
			t.Errorf("%s has an object, want none", id)
		}
	}
	kube.ClearActions() // the reads of the test itself
	for _, id := range []string{"plain", "zero"} {
		if _, err := te.envClient.DeleteEnvironment(ctx, &ateenvv1alpha.DeleteEnvironmentRequest{Id: id}); err != nil {
			t.Fatal(err)
		}
	}
	if calls := kubeWrites(kube); len(calls) != 0 {
		t.Errorf("the deletes made the Kubernetes calls %v, want none", calls)
	}
}

// TestCreateRollsBackWhenTheObjectIsNotWritten checks the create half of
// EM-6: a create whose object is not written deletes the actor it created
// and fails; no object and no environment stay.
func TestCreateRollsBackWhenTheObjectIsNotWritten(t *testing.T) {
	te, _, kube := ttlEnv(t)
	kube.PrependReactor("create", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the Kubernetes API is down")
	})
	_, err := te.envClient.CreateEnvironment(t.Context(), &ateenvv1alpha.CreateEnvironmentRequest{Id: "env-1", TimeToLive: durationpb.New(time.Minute)})
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "the Kubernetes API is down") {
		t.Errorf("CreateEnvironment = %v, want Unavailable with the reason", err)
	}
	if te.control.Creates() != 1 || te.control.ActorCount() != 0 || exists(t, te, "env-1") {
		t.Errorf("after the failed create: %d CreateActor calls, %d actors; want the one actor created and deleted again", te.control.Creates(), te.control.ActorCount())
	}
	if object(t, kube, "env-1") != nil {
		t.Error("the failed create left an object")
	}
}

// TestCreateRollbackDeletesOnlyTheActorItCreated checks the precondition of
// EM-6's rollback: the delete carries the uid of the actor of this create.
// An environment that was deleted and created again with the same id while
// the write of the object failed is another actor, and stays.
func TestCreateRollbackDeletesOnlyTheActorItCreated(t *testing.T) {
	te, _, kube := ttlEnv(t)
	kube.PrependReactor("create", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		recreateBehind(t, te, "env-1")
		return true, nil, errors.New("the Kubernetes API is down")
	})
	_, err := te.envClient.CreateEnvironment(t.Context(), &ateenvv1alpha.CreateEnvironmentRequest{Id: "env-1", TimeToLive: durationpb.New(time.Minute)})
	if err == nil {
		t.Fatal("CreateEnvironment = nil, want the error of the write")
	}
	if !exists(t, te, "env-1") {
		t.Error("the rollback deleted the environment that was created again with the same id")
	}
}

// TestCreateWritesOverTheObjectOfAnEarlierEnvironment checks the create path
// of EM-14 and EM-8: the id has the object of an earlier environment, whose
// actor is gone. The create reads the actor again, writes over that object,
// and the new environment has its own deadline and not the old one.
func TestCreateWritesOverTheObjectOfAnEarlierEnvironment(t *testing.T) {
	te, store, kube := ttlEnv(t)
	ctx := t.Context()
	old := createTTLAndWait(t, te, "env-1", time.Minute)
	oldUID := actorUID(t, te, "env-1")
	// The environment goes without its object, as after a crash between
	// the two deletes.
	if _, err := te.control.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: &ateapipb.ObjectRef{Atespace: "ate-env", Name: "env-1"}}); err != nil {
		t.Fatal(err)
	}

	// EM-8: a new environment with no time to live does not get the
	// deadline of the old one.
	if _, err := te.envClient.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "env-1"}); err != nil {
		t.Fatal(err)
	}
	got, err := te.envClient.GetEnvironment(ctx, &ateenvv1alpha.GetEnvironmentRequest{Id: "env-1"})
	if err != nil || got.GetEnvironment().GetExpireTime() != nil {
		t.Errorf("the new environment has expire_time %v (%v), want none: the object is of the old one", got.GetEnvironment().GetExpireTime(), err)
	}
	if _, err := te.control.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: &ateapipb.ObjectRef{Atespace: "ate-env", Name: "env-1"}}); err != nil {
		t.Fatal(err)
	}
	if cm := object(t, kube, "env-1"); cm == nil || cm.Data["uid"] != oldUID {
		t.Fatalf("object = %+v, want the object of the old environment still in place", cm)
	}

	// EM-14: a new environment with a time to live writes over it.
	env := createTTL(t, te, "env-1", time.Hour)
	newUID := actorUID(t, te, "env-1")
	if newUID == "" || newUID == oldUID {
		t.Fatalf("uid of the new actor = %q, want a new one (old %q)", newUID, oldUID)
	}
	waitForObject(t, store, "env-1", func(r envstore.Record, ok bool) bool { return ok && r.UID == newUID })
	got, err = te.envClient.GetEnvironment(ctx, &ateenvv1alpha.GetEnvironmentRequest{Id: "env-1"})
	want := env.GetCreateTime().AsTime().Add(time.Hour)
	if err != nil || !got.GetEnvironment().GetExpireTime().AsTime().Equal(want) || want.Equal(old.GetExpireTime().AsTime()) {
		t.Errorf("expire_time = %v (%v), want the deadline of the new environment %v", got.GetEnvironment().GetExpireTime().AsTime(), err, want)
	}
}

// TestCreateDropsALateWrite checks the create path of EM-14: the write of an
// object waits, the environment is deleted and created again with the same
// id and an object of its own, and the late write then finds that object.
// It reads the actor again, sees another actor, and drops the write: the
// object of the new environment stays, and so does the new environment.
func TestCreateDropsALateWrite(t *testing.T) {
	te, _, kube := ttlEnv(t)
	ctx := t.Context()
	var once sync.Once
	var newUID string
	kube.PrependReactor("create", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		late := false
		once.Do(func() {
			late = true
			// While the write of the first create waits: the id is deleted
			// and created again, and the new environment has its object.
			recreateBehind(t, te, "env-1")
			newUID = actorUID(t, te, "env-1")
			cm := action.(k8stesting.CreateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
			cm.Data["uid"] = newUID
			cm.UID, cm.ResourceVersion, cm.CreationTimestamp = "object-of-the-new", "1000", metav1.Now()
			if err := kube.Tracker().Add(cm); err != nil {
				t.Error(err)
			}
		})
		if late {
			return true, nil, apierrors.NewAlreadyExists(configMaps, "x")
		}
		return false, nil, nil
	})

	_, err := te.envClient.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "env-1", TimeToLive: durationpb.New(time.Minute)})
	if err == nil {
		t.Fatal("the late create = nil, want an error: its environment is gone")
	}
	if cm := object(t, kube, "env-1"); cm == nil || cm.Data["uid"] != newUID || cm.UID != "object-of-the-new" {
		t.Errorf("object = %+v, want the object of the new environment (actor %s) untouched", cm, newUID)
	}
	if got := actorUID(t, te, "env-1"); got != newUID {
		t.Errorf("actor uid = %q, want the new environment %q in place", got, newUID)
	}
}

// TestSetEnvironmentTimeToLive checks EM-10: the deadline is the time of the
// call plus the value, for an environment with or without one; zero removes
// the deadline, and the environment is then not deleted; a negative value is
// InvalidArgument and an unknown environment NotFound.
func TestSetEnvironmentTimeToLive(t *testing.T) {
	te, store, kube := ttlEnv(t)
	ctx := t.Context()
	set := func(id string, ttl time.Duration) (*ateenvv1alpha.Environment, time.Time, time.Time, error) {
		before := time.Now()
		resp, err := te.envClient.SetEnvironmentTimeToLive(ctx, &ateenvv1alpha.SetEnvironmentTimeToLiveRequest{Id: id, TimeToLive: durationpb.New(ttl)})
		return resp.GetEnvironment(), before, time.Now(), err
	}
	within := func(env *ateenvv1alpha.Environment, before, after time.Time, ttl time.Duration) bool {
		got := env.GetExpireTime().AsTime()
		return env.GetExpireTime() != nil && !got.Before(before.Add(ttl)) && !got.After(after.Add(ttl))
	}

	// An environment with no deadline gets one, and its object.
	if _, err := te.envClient.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "plain"}); err != nil {
		t.Fatal(err)
	}
	env, before, after, err := set("plain", time.Hour)
	if err != nil || !within(env, before, after, time.Hour) || env.GetId() != "plain" || env.GetCreateTime() == nil {
		t.Fatalf("SetEnvironmentTimeToLive(plain, 1h) = %v, %v, want the environment with a deadline one hour after the call", env, err)
	}
	want := env.GetExpireTime().AsTime()
	waitForObject(t, store, "plain", func(r envstore.Record, ok bool) bool { return ok && r.ExpireTime.Equal(want) })
	if cm := object(t, kube, "plain"); cm.Data["uid"] != actorUID(t, te, "plain") {
		t.Errorf("object uid = %q, want the uid of the actor", cm.Data["uid"])
	}

	// An environment with a deadline gets a new one, counted from the call
	// and not from its create time.
	created := createTTLAndWait(t, te, "env-1", time.Minute)
	env, before, after, err = set("env-1", 2*time.Hour)
	if err != nil || !within(env, before, after, 2*time.Hour) {
		t.Fatalf("SetEnvironmentTimeToLive(env-1, 2h) = %v, %v, want a deadline two hours after the call", env, err)
	}
	want = env.GetExpireTime().AsTime()
	waitForObject(t, store, "env-1", func(r envstore.Record, ok bool) bool { return ok && r.ExpireTime.Equal(want) })
	got, err := te.envClient.GetEnvironment(ctx, &ateenvv1alpha.GetEnvironmentRequest{Id: "env-1"})
	if err != nil || !got.GetEnvironment().GetExpireTime().AsTime().Equal(want) {
		t.Errorf("GetEnvironment: expire_time = %v, %v, want %v", got.GetEnvironment().GetExpireTime(), err, want)
	}
	// The first deadline is past, the new one is not.
	if n, err := te.srv.SweepDeadlines(ctx, created.GetExpireTime().AsTime().Add(time.Minute)); n != 0 || err != nil || !exists(t, te, "env-1") {
		t.Errorf("SweepDeadlines after the first deadline = %d, %v, want the environment in place", n, err)
	}

	// Zero removes the deadline: no sweep deletes the environment.
	env, _, _, err = set("env-1", 0)
	if err != nil || env.GetExpireTime() != nil {
		t.Fatalf("SetEnvironmentTimeToLive(env-1, 0) = %v, %v, want no expire_time", env, err)
	}
	waitForObject(t, store, "env-1", func(r envstore.Record, ok bool) bool { return ok && r.ExpireTime.IsZero() })
	if n, err := te.srv.SweepDeadlines(ctx, time.Now().Add(1000*time.Hour)); n != 1 || err != nil {
		t.Errorf("SweepDeadlines far in the future = %d, %v, want only the environment that kept its deadline deleted", n, err)
	}
	if !exists(t, te, "env-1") || exists(t, te, "plain") {
		t.Error("want env-1, with no deadline, in place, and plain, with one, deleted")
	}
	got, err = te.envClient.GetEnvironment(ctx, &ateenvv1alpha.GetEnvironmentRequest{Id: "env-1"})
	if err != nil || got.GetEnvironment().GetExpireTime() != nil {
		t.Errorf("GetEnvironment after the deadline was removed: expire_time = %v, %v", got.GetEnvironment().GetExpireTime(), err)
	}
	// Zero on an environment with no object writes nothing. It reads the
	// object from the API server: the informer can be behind a deadline.
	if _, err := te.envClient.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "none"}); err != nil {
		t.Fatal(err)
	}
	kube.ClearActions()
	if env, _, _, err = set("none", 0); err != nil || env.GetExpireTime() != nil || !slices.Equal(kubeWrites(kube), []string{"list"}) {
		t.Errorf("SetEnvironmentTimeToLive(none, 0) = %v, %v, Kubernetes calls %v; want no deadline, one read and no write", env, err, kubeWrites(kube))
	}

	if _, _, _, err = set("env-1", -time.Second); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a negative value = %v, want InvalidArgument", err)
	}
	if _, _, _, err = set("unknown", time.Hour); status.Code(err) != codes.NotFound {
		t.Errorf("an unknown environment = %v, want NotFound", err)
	}
	if object(t, kube, "unknown") != nil {
		t.Error("SetEnvironmentTimeToLive wrote an object for an environment that does not exist")
	}
	if _, err := te.envClient.SetEnvironmentTimeToLive(ctx, &ateenvv1alpha.SetEnvironmentTimeToLiveRequest{TimeToLive: durationpb.New(time.Hour)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("no id = %v, want InvalidArgument", err)
	}
}

// TestSetEnvironmentTimeToLiveOverALateInformer checks EM-10 across
// replicas: the informer of the replica that serves the call is behind a
// write of another. The update that the API server refuses is tried again
// on the copy of the API server, and the call writes its deadline.
func TestSetEnvironmentTimeToLiveOverALateInformer(t *testing.T) {
	te, kube, feed := lateEnv(t)
	ctx := t.Context()
	createTTL(t, te, "env-1", time.Minute)
	feed("env-1")
	// The write of another replica, which this informer does not get.
	first, err := te.envClient.SetEnvironmentTimeToLive(ctx, &ateenvv1alpha.SetEnvironmentTimeToLiveRequest{Id: "env-1", TimeToLive: durationpb.New(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	second, err := te.envClient.SetEnvironmentTimeToLive(ctx, &ateenvv1alpha.SetEnvironmentTimeToLiveRequest{Id: "env-1", TimeToLive: durationpb.New(3 * time.Hour)})
	if err != nil {
		t.Fatalf("SetEnvironmentTimeToLive on a late informer = %v, want success after a read of the API server", err)
	}
	want := second.GetEnvironment().GetExpireTime().AsTime()
	if cm := object(t, kube, "env-1"); cm.Data["expire_time"] != want.UTC().Format(time.RFC3339Nano) || !want.After(first.GetEnvironment().GetExpireTime().AsTime()) {
		t.Errorf("object deadline = %s, want the one of the second call %s", cm.Data["expire_time"], want)
	}

	// Zero on a late informer (the reviewer's case): the informer of this
	// replica never got the object that another replica wrote with a
	// deadline. The call must remove that deadline, and not answer success
	// from an informer that shows none.
	env3 := createTTL(t, te, "env-3", time.Minute)
	if _, ok, _ := te.srv.Store.Get("ate-env", "env-3"); ok {
		t.Fatal("the informer has the object: the test does not check a late informer")
	}
	cleared, err := te.envClient.SetEnvironmentTimeToLive(ctx, &ateenvv1alpha.SetEnvironmentTimeToLiveRequest{Id: "env-3"})
	if err != nil || cleared.GetEnvironment().GetExpireTime() != nil {
		t.Fatalf("SetEnvironmentTimeToLive(env-3, 0) = %v, %v, want no expire_time", cleared, err)
	}
	if cm := object(t, kube, "env-3"); cm.Data["expire_time"] != "" {
		t.Errorf("the object keeps the deadline %s after the call removed it", cm.Data["expire_time"])
	}
	// The same with an informer that has the object with no deadline and
	// is behind a new one.
	feed("env-3")
	if _, err := te.envClient.SetEnvironmentTimeToLive(ctx, &ateenvv1alpha.SetEnvironmentTimeToLiveRequest{Id: "env-3", TimeToLive: durationpb.New(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := te.envClient.SetEnvironmentTimeToLive(ctx, &ateenvv1alpha.SetEnvironmentTimeToLiveRequest{Id: "env-3"}); err != nil {
		t.Fatal(err)
	}
	if cm := object(t, kube, "env-3"); cm.Data["expire_time"] != "" {
		t.Errorf("the object keeps the deadline %s after the second call removed it", cm.Data["expire_time"])
	}
	second2 := secondReplica(t, te, kube)
	if n, err := second2.SweepDeadlines(ctx, env3.GetExpireTime().AsTime().Add(time.Hour)); n != 0 || err != nil || !exists(t, te, "env-3") {
		t.Errorf("SweepDeadlines of another replica = %d, %v, want env-3 in place: its deadline was removed", n, err)
	}

	// An environment whose object the informer never got: the write finds
	// the object of the same actor and changes it.
	createTTL(t, te, "env-2", time.Minute)
	resp, err := te.envClient.SetEnvironmentTimeToLive(ctx, &ateenvv1alpha.SetEnvironmentTimeToLiveRequest{Id: "env-2", TimeToLive: durationpb.New(time.Hour)})
	if err != nil {
		t.Fatalf("SetEnvironmentTimeToLive with an object the informer does not have = %v", err)
	}
	if cm := object(t, kube, "env-2"); cm.Data["expire_time"] != resp.GetEnvironment().GetExpireTime().AsTime().UTC().Format(time.RFC3339Nano) {
		t.Errorf("object deadline = %s, want the one of the call", cm.Data["expire_time"])
	}
}

// TestDeadlineSweepDeletesInEachStatus checks EM-9: an environment with a
// time to live is deleted by the sweep after its deadline, and not before,
// in the status RUNNING, SUSPENDED or PAUSED, and its object goes with it.
func TestDeadlineSweepDeletesInEachStatus(t *testing.T) {
	te, store, kube := ttlEnv(t)
	ctx := t.Context()
	wants := map[string]ateenvv1alpha.EnvironmentStatus{
		"running":   ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_RUNNING,
		"suspended": ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_SUSPENDED,
		"paused":    ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_PAUSED,
	}
	var last time.Time
	for id := range wants {
		env := createTTLAndWait(t, te, id, time.Hour)
		if env.GetExpireTime().AsTime().After(last) {
			last = env.GetExpireTime().AsTime()
		}
	}
	if _, err := te.envClient.CreateEnvironment(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "plain"}); err != nil {
		t.Fatal(err)
	}
	if _, err := te.envClient.SuspendEnvironment(ctx, &ateenvv1alpha.SuspendEnvironmentRequest{Id: "suspended"}); err != nil {
		t.Fatal(err)
	}
	if _, err := te.envClient.SuspendEnvironment(ctx, &ateenvv1alpha.SuspendEnvironmentRequest{Id: "paused", Mode: ateenvv1alpha.SuspendMode_SUSPEND_MODE_LOCAL}); err != nil {
		t.Fatal(err)
	}
	for id, want := range wants {
		got, err := te.envClient.GetEnvironment(ctx, &ateenvv1alpha.GetEnvironmentRequest{Id: id})
		if err != nil || got.GetEnvironment().GetStatus() != want {
			t.Fatalf("status of %s = %v, %v, want %v", id, got.GetEnvironment().GetStatus(), err, want)
		}
	}

	if n, err := te.srv.SweepDeadlines(ctx, last.Add(-time.Minute)); n != 0 || err != nil || te.control.ActorCount() != 4 {
		t.Fatalf("SweepDeadlines before the deadline = %d, %v with %d actors, want nothing deleted", n, err, te.control.ActorCount())
	}
	if n, err := te.srv.SweepDeadlines(ctx, last); n != 3 || err != nil {
		t.Fatalf("SweepDeadlines at the deadline = %d, %v, want the three environments deleted", n, err)
	}
	for id := range wants {
		if exists(t, te, id) || object(t, kube, id) != nil {
			t.Errorf("%s: the environment or its object stays after its deadline", id)
		}
		waitForObject(t, store, id, func(_ envstore.Record, ok bool) bool { return !ok })
	}
	if !exists(t, te, "plain") {
		t.Error("the sweep deleted an environment with no deadline")
	}
}

// TestDeadlineSweepReadsTheObjectFromTheAPIServer checks EM-F: the informer
// of the sweeping replica is behind a SetEnvironmentTimeToLive of another
// and shows the earlier deadline as past. The sweep reads the object from
// the API server, sees the later deadline, and does not delete.
func TestDeadlineSweepReadsTheObjectFromTheAPIServer(t *testing.T) {
	te, _, feed := lateEnv(t)
	ctx := t.Context()
	env := createTTL(t, te, "env-1", time.Minute)
	feed("env-1")
	// The informer does not get this write.
	moved, err := te.envClient.SetEnvironmentTimeToLive(ctx, &ateenvv1alpha.SetEnvironmentTimeToLiveRequest{Id: "env-1", TimeToLive: durationpb.New(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	past := env.GetExpireTime().AsTime().Add(time.Second)
	if got := te.srv.Store.Expired(past); len(got) != 1 {
		t.Fatalf("the informer shows %d expired objects, want the one with the earlier deadline: the test does not check a late informer", len(got))
	}
	if n, err := te.srv.SweepDeadlines(ctx, past); n != 0 || err != nil || !exists(t, te, "env-1") {
		t.Fatalf("SweepDeadlines on a late informer = %d, %v, want the environment in place: its deadline moved", n, err)
	}
	// Past the deadline that the API server has, the sweep deletes.
	if n, err := te.srv.SweepDeadlines(ctx, moved.GetEnvironment().GetExpireTime().AsTime()); n != 1 || err != nil || exists(t, te, "env-1") {
		t.Errorf("SweepDeadlines past the new deadline = %d, %v, want the environment deleted", n, err)
	}

	// The same with a deadline that was removed.
	env = createTTL(t, te, "env-2", time.Minute)
	feed("env-2")
	if _, err := te.envClient.SetEnvironmentTimeToLive(ctx, &ateenvv1alpha.SetEnvironmentTimeToLiveRequest{Id: "env-2"}); err != nil {
		t.Fatal(err)
	}
	if n, err := te.srv.SweepDeadlines(ctx, env.GetExpireTime().AsTime().Add(time.Hour)); n != 0 || err != nil || !exists(t, te, "env-2") {
		t.Errorf("SweepDeadlines after the deadline was removed = %d, %v, want the environment in place", n, err)
	}
}

// TestDeadlineSweepFailsWhenItCannotReadTheObject: a read of the API server
// that fails is not "the object is past its deadline" (EM-F): the sweep
// deletes nothing and reports the error.
func TestDeadlineSweepFailsWhenItCannotReadTheObject(t *testing.T) {
	te, _, kube := ttlEnv(t)
	env := createTTLAndWait(t, te, "env-1", time.Minute)
	kube.PrependReactor("list", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the Kubernetes API is down")
	})
	n, err := te.srv.SweepDeadlines(t.Context(), env.GetExpireTime().AsTime().Add(time.Hour))
	if n != 0 || err == nil || !exists(t, te, "env-1") {
		t.Errorf("SweepDeadlines = %d, %v, want an error and the environment in place", n, err)
	}
}

// TestDeadlineSweepLeavesAnEnvironmentCreatedAgain checks EM-13: the sweep
// deletes by deadline only with the uid of the object as the precondition.
// An environment that was created again with the same id, with or without
// an object of its own, is not deleted by the deadline of the old one.
func TestDeadlineSweepLeavesAnEnvironmentCreatedAgain(t *testing.T) {
	te, store, kube := ttlEnv(t)
	ctx := t.Context()

	// Without an object of its own: the object of the old environment
	// stays, past its deadline, next to the new actor.
	old := createTTLAndWait(t, te, "bare", time.Minute)
	oldUID := actorUID(t, te, "bare")
	recreateBehind(t, te, "bare")
	newUID := actorUID(t, te, "bare")
	if newUID == "" || newUID == oldUID {
		t.Fatalf("uid of the new actor = %q, want a new one (old %q)", newUID, oldUID)
	}
	past := old.GetExpireTime().AsTime().Add(time.Hour)
	deletes := te.control.Deletes()
	if n, err := te.srv.SweepDeadlines(ctx, past); n != 0 || err != nil {
		t.Errorf("SweepDeadlines = %d, %v, want nothing deleted and no error", n, err)
	}
	if got := actorUID(t, te, "bare"); got != newUID {
		t.Fatalf("actor uid after the sweep = %q, want the new environment %q in place: the deadline of the old one deleted it", got, newUID)
	}
	if te.control.Deletes() == deletes {
		t.Error("the sweep sent no delete: the test does not check the precondition")
	}
	// The object is an orphan, and the orphan sweep removes it.
	if n, err := store.SweepOrphans(ctx, te.srv.ActorUIDs, 0, past); n != 1 || err != nil || object(t, kube, "bare") != nil {
		t.Errorf("SweepOrphans = %d, %v, want the object of the old environment deleted", n, err)
	}

	// With an object of its own: a delete and a create with a new time to
	// live through the server.
	old = createTTLAndWait(t, te, "own", time.Minute)
	if _, err := te.control.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: &ateapipb.ObjectRef{Atespace: "ate-env", Name: "own"}}); err != nil {
		t.Fatal(err)
	}
	createTTLAndWait(t, te, "own", 10*time.Hour)
	newUID = actorUID(t, te, "own")
	if n, err := te.srv.SweepDeadlines(ctx, old.GetExpireTime().AsTime().Add(time.Hour)); n != 0 || err != nil || actorUID(t, te, "own") != newUID {
		t.Errorf("SweepDeadlines past the deadline of the old environment = %d, %v, want the new one in place", n, err)
	}
}

// secondReplica is another ate-env-api on the same control plane and the
// same Kubernetes, with its own informer.
func secondReplica(t *testing.T, te *testEnv, kube *fake.Clientset) *apiservice.Server {
	t.Helper()
	srv := apiservice.New(ate.NewControlClient(te.control.Client()), "", "")
	srv.Store = runStore(t, kube)
	return srv
}

// TestTwoReplicasSweepTheSameEnvironment checks EM-11: two replicas that
// sweep the same expired environments both end without an error, and each
// environment is deleted once.
func TestTwoReplicasSweepTheSameEnvironment(t *testing.T) {
	te, _, kube := ttlEnv(t)
	ctx := t.Context()
	other := secondReplica(t, te, kube)
	const envs = 20
	var last time.Time
	for i := range envs {
		last = createTTLAndWait(t, te, fmt.Sprintf("env-%d", i), time.Minute).GetExpireTime().AsTime()
	}
	for i := range envs {
		waitForObject(t, other.Store, fmt.Sprintf("env-%d", i), func(_ envstore.Record, ok bool) bool { return ok })
	}

	var wg sync.WaitGroup
	counts, errs := make([]int, 2), make([]error, 2)
	for i, srv := range []*apiservice.Server{te.srv, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			counts[i], errs[i] = srv.SweepDeadlines(ctx, last)
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Errorf("the sweeps ended with %v and %v, want no error", errs[0], errs[1])
	}
	if counts[0]+counts[1] != envs || te.control.ActorCount() != 0 {
		t.Errorf("the sweeps deleted %d + %d environments and %d actors stay, want each of the %d deleted once", counts[0], counts[1], te.control.ActorCount(), envs)
	}
	for i := range envs {
		if object(t, kube, fmt.Sprintf("env-%d", i)) != nil {
			t.Errorf("the object of env-%d stays", i)
		}
	}

	// The replica that comes second, with the object still in place: the
	// environment is gone, which is not an error, and the object goes.
	env := createTTLAndWait(t, te, "late", time.Minute)
	if _, err := te.control.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: &ateapipb.ObjectRef{Atespace: "ate-env", Name: "late"}}); err != nil {
		t.Fatal(err)
	}
	if n, err := te.srv.SweepDeadlines(ctx, env.GetExpireTime().AsTime()); n != 0 || err != nil || object(t, kube, "late") != nil {
		t.Errorf("SweepDeadlines of an environment that is gone = %d, %v, want no error and the object removed", n, err)
	}
}

// TestNoDeadlineIsEnforcedWithTheStoreOff checks EM-12: with the store
// turned off after use, no environment is deleted by a deadline, and the
// objects stay.
func TestNoDeadlineIsEnforcedWithTheStoreOff(t *testing.T) {
	te, _, kube := ttlEnv(t)
	env := createTTLAndWait(t, te, "env-1", time.Minute)
	te.srv.Store = nil // a restart without --environment-store

	kube.ClearActions()
	past := env.GetExpireTime().AsTime().Add(time.Hour)
	if n, err := te.srv.SweepDeadlines(t.Context(), past); n != 0 || err != nil {
		t.Errorf("SweepDeadlines with the store off = %d, %v, want nothing", n, err)
	}
	te.srv.Sweep(t.Context(), past)
	if calls := kubeWrites(kube); len(calls) != 0 {
		t.Errorf("the sweep with the store off made the Kubernetes calls %v", calls)
	}
	if !exists(t, te, "env-1") || object(t, kube, "env-1") == nil {
		t.Error("want the environment and its object in place with the store off")
	}
	got, err := te.envClient.GetEnvironment(t.Context(), &ateenvv1alpha.GetEnvironmentRequest{Id: "env-1"})
	if err != nil || got.GetEnvironment().GetExpireTime() != nil {
		t.Errorf("expire_time with the store off = %v, %v, want none: no deadline is enforced", got.GetEnvironment().GetExpireTime(), err)
	}
}

// failedTotal is the value of ate_env_api_deadline_delete_failed_total for
// the atespace of the tests.
func failedTotal(t *testing.T, m *telemetry.Metrics) float64 {
	t.Helper()
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, mf := range mfs {
		if mf.GetName() != telemetry.MetricDeadlineDeleteFailed {
			continue
		}
		for _, s := range mf.GetMetric() {
			total += s.GetCounter().GetValue()
		}
	}
	return total
}

// TestDeadlineSweepStopsAfterFiveRefusals checks EM-15 (decision EM-I): a
// delete by deadline that Substrate refuses is tried again on each sweep,
// the object counts the tries, and the replicas share the count. After 5
// failed tries the sweep stops for that environment: the object stays,
// marked, the environment stays, and one log line and one metric sample
// report it. A SetEnvironmentTimeToLive clears the mark, and so does a
// caller's delete.
func TestDeadlineSweepStopsAfterFiveRefusals(t *testing.T) {
	te, store, kube := ttlEnv(t)
	ctx := t.Context()
	metrics := telemetry.NewMetrics()
	te.srv.Metrics = metrics
	other := secondReplica(t, te, kube)
	other.Metrics = metrics

	env := createTTLAndWait(t, te, "env-1", time.Minute)
	past := env.GetExpireTime().AsTime().Add(time.Hour)
	// The case WarmImage records: CRASHED after a resume that failed, which
	// Substrate refuses to delete.
	te.control.KeepCrashed = true
	te.control.Crash("env-1", "restore failed")

	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	failures := func() string { return object(t, kube, "env-1").Data["delete_failures"] }

	// The time of try n: the tries of one environment that count are at
	// least DeadlineTryInterval apart.
	at := func(n int) time.Time { return past.Add(time.Duration(n) * apiservice.DeadlineTryInterval) }

	// A failure that is not a refusal of this delete does not count: a
	// control plane that is down or busy, a failure of its own (Internal), a
	// credential it does not take, a call that ran out of time.
	for _, code := range []codes.Code{codes.Unavailable, codes.Internal, codes.Unknown, codes.DeadlineExceeded, codes.ResourceExhausted, codes.Unauthenticated, codes.PermissionDenied} {
		te.control.FailDeletes(1, code)
		if n, err := te.srv.SweepDeadlines(ctx, past); n != 0 || err == nil || failures() != "" {
			t.Fatalf("SweepDeadlines with the answer %s = %d, %v, failures %q; want an error and no count", code, n, err, failures())
		}
	}
	ended, cancel := context.WithCancel(ctx)
	cancel()
	if n, err := te.srv.SweepDeadlines(ended, past); n != 0 || err == nil || failures() != "" {
		t.Fatalf("SweepDeadlines with an ended context = %d, %v, failures %q; want an error and no count", n, err, failures())
	}

	// Try 1 by the two replicas in the same moment: one try, counted once.
	var both sync.WaitGroup
	for _, srv := range []*apiservice.Server{te.srv, other} {
		both.Add(1)
		go func() {
			defer both.Done()
			_, _ = srv.SweepDeadlines(ctx, at(1))
		}()
	}
	both.Wait()
	if got := failures(); got != "1" {
		t.Fatalf("two replicas that failed in the same moment count %q failures, want 1", got)
	}
	// A refusal a moment later is the same try.
	if n, err := other.SweepDeadlines(ctx, at(1).Add(time.Second)); n != 0 || err != nil || failures() != "1" {
		t.Fatalf("a refusal one second after a counted one = %d, %v, failures %q; want no new count", n, err, failures())
	}

	// Tries 2 to 4, by the two replicas in turn: each fails and counts.
	for try := 2; try <= apiservice.DeadlineDeleteTries-1; try++ {
		srv := []*apiservice.Server{te.srv, other}[try%2]
		n, err := srv.SweepDeadlines(ctx, at(try))
		if n != 0 || err == nil || !strings.Contains(err.Error(), "DeleteVolumes prerequisite not met") {
			t.Fatalf("try %d: SweepDeadlines = %d, %v, want the refusal of Substrate", try, n, err)
		}
		if got := failures(); got != strconv.Itoa(try) {
			t.Fatalf("try %d: the object counts %q failures", try, got)
		}
	}
	if failedTotal(t, metrics) != 0 || strings.Contains(logged.String(), "stops for") {
		t.Fatalf("before the last try: metric %v, log %q; want no report yet", failedTotal(t, metrics), logged.String())
	}

	// Try 5 marks the object and reports once.
	deletes := te.control.Deletes()
	if n, err := te.srv.SweepDeadlines(ctx, at(5)); n != 0 || err != nil {
		t.Fatalf("try 5: SweepDeadlines = %d, %v, want no error: the environment is reported, once", n, err)
	}
	if te.control.Deletes() != deletes+1 {
		t.Fatalf("try 5 sent %d deletes, want 1", te.control.Deletes()-deletes)
	}
	cm := object(t, kube, "env-1")
	if cm.Labels[envstore.LabelDeadline] != envstore.DeadlineFailed || cm.Data["delete_failures"] != "5" || cm.Data["expire_time"] == "" {
		t.Errorf("object after try 5: labels %v, data %v; want the failed mark, 5 failures and the past deadline", cm.Labels, cm.Data)
	}

	// More sweeps, of each replica, with or without the mark in its
	// informer: no delete, no error, no second report.
	deletes = te.control.Deletes()
	for range 3 {
		for _, srv := range []*apiservice.Server{te.srv, other} {
			if n, err := srv.SweepDeadlines(ctx, at(9)); n != 0 || err != nil {
				t.Errorf("a sweep after the mark = %d, %v, want nothing", n, err)
			}
		}
	}
	waitForObject(t, store, "env-1", func(r envstore.Record, ok bool) bool { return ok && r.DeleteFailed })
	if n, err := te.srv.SweepDeadlines(ctx, at(10)); n != 0 || err != nil || te.control.Deletes() != deletes {
		t.Errorf("sweeps after the mark = %d, %v, with %d deletes sent; want none", n, err, te.control.Deletes()-deletes)
	}
	if got, lines := failedTotal(t, metrics), strings.Count(logged.String(), "the deadline sweep stops for ate-env/env-1"); got != 1 || lines != 1 {
		t.Errorf("reports: metric %v, %d log lines; want one of each\nlog: %s", got, lines, logged.String())
	}
	got, err := te.envClient.GetEnvironment(ctx, &ateenvv1alpha.GetEnvironmentRequest{Id: "env-1"})
	if err != nil || !got.GetEnvironment().GetExpireTime().AsTime().Equal(env.GetExpireTime().AsTime()) {
		t.Errorf("the environment after the mark = %v, %v, want it in place with its past expire_time", got, err)
	}
	// The orphan sweep leaves the marked object: its actor exists.
	if n, err := store.SweepOrphans(ctx, te.srv.ActorUIDs, 0, at(10)); n != 0 || err != nil {
		t.Errorf("SweepOrphans = %d, %v, want the marked object in place", n, err)
	}

	// A SetEnvironmentTimeToLive clears the mark and the count, and the
	// sweep tries again after the new deadline.
	moved, err := te.envClient.SetEnvironmentTimeToLive(ctx, &ateenvv1alpha.SetEnvironmentTimeToLiveRequest{Id: "env-1", TimeToLive: durationpb.New(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	cm = object(t, kube, "env-1")
	if cm.Labels[envstore.LabelDeadline] != envstore.DeadlineSet || cm.Data["delete_failures"] != "" {
		t.Errorf("object after SetEnvironmentTimeToLive: labels %v, data %v; want no mark and no count", cm.Labels, cm.Data)
	}
	waitForObject(t, store, "env-1", func(r envstore.Record, ok bool) bool { return ok && !r.DeleteFailed })
	if n, err := te.srv.SweepDeadlines(ctx, moved.GetEnvironment().GetExpireTime().AsTime()); n != 0 || err == nil || failures() != "1" {
		t.Errorf("the sweep after the new deadline = %d, %v, failures %q; want a new first try", n, err, failures())
	}

	// A caller's delete that succeeds removes the object, and the mark.
	again := moved.GetEnvironment().GetExpireTime().AsTime()
	for try := 2; try <= apiservice.DeadlineDeleteTries; try++ {
		_, _ = te.srv.SweepDeadlines(ctx, again.Add(time.Duration(try)*apiservice.DeadlineTryInterval))
	}
	if cm = object(t, kube, "env-1"); cm.Labels[envstore.LabelDeadline] != envstore.DeadlineFailed {
		t.Fatalf("object labels = %v, want the mark again", cm.Labels)
	}
	waitForObject(t, store, "env-1", func(r envstore.Record, ok bool) bool { return ok && r.DeleteFailed })
	te.control.KeepCrashed = false
	if _, err := te.envClient.DeleteEnvironment(ctx, &ateenvv1alpha.DeleteEnvironmentRequest{Id: "env-1"}); err != nil {
		t.Fatal(err)
	}
	if exists(t, te, "env-1") || object(t, kube, "env-1") != nil {
		t.Error("after the caller's delete: the environment or its marked object stays")
	}
	if got := failedTotal(t, metrics); got != 2 {
		t.Errorf("metric = %v, want 2: the environment was given up twice", got)
	}
}

// TestDeadlineSweepThatRunsOutOfTime: a sweep whose context ends with
// environments left deletes what it reached, reports the rest as an error,
// and counts no failure (EM-15: no answer is not a refusal). Run with -race:
// the sweep's workers and its loop share the result.
func TestDeadlineSweepThatRunsOutOfTime(t *testing.T) {
	te, _, kube := ttlEnv(t)
	const envs = 60
	var last time.Time
	for i := range envs {
		last = createTTLAndWait(t, te, fmt.Sprintf("env-%d", i), time.Minute).GetExpireTime().AsTime()
	}
	ctx, cancel := context.WithCancel(t.Context())
	var reads atomic.Int64
	kube.PrependReactor("list", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if reads.Add(1) == 8 {
			cancel()
		}
		return false, nil, nil
	})
	n, err := te.srv.SweepDeadlines(ctx, last)
	if err == nil || n == envs {
		t.Fatalf("SweepDeadlines with a context that ended = %d, %v, want an error and environments left", n, err)
	}
	if left := te.control.ActorCount(); left != envs-n {
		t.Errorf("%d actors stay after %d deletes of %d", left, n, envs)
	}
	for i := range envs {
		if cm := object(t, kube, fmt.Sprintf("env-%d", i)); cm != nil && cm.Data["delete_failures"] != "" {
			t.Errorf("env-%d counts %s failures after a sweep that ran out of time", i, cm.Data["delete_failures"])
		}
	}
	// The next sweep deletes the rest.
	if m, err := te.srv.SweepDeadlines(t.Context(), last); m != envs-n || err != nil || te.control.ActorCount() != 0 {
		t.Errorf("the next sweep = %d, %v, with %d actors left; want the other %d deleted", m, err, te.control.ActorCount(), envs-n)
	}
}
