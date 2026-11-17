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

package envstore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

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

const testNamespace = "ate-env"

func newStore(t *testing.T, objects ...runtime.Object) (*Store, *fake.Clientset) {
	t.Helper()
	client := fake.NewSimpleClientset(objects...)
	stampCreates(client)
	s := New(client.CoreV1().ConfigMaps(testNamespace), testNamespace)
	if err := s.Run(t.Context(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	return s, client
}

// stampCreates gives each object that a test creates through the client what
// the API server gives it and the fake clientset does not: a uid, a resource
// version and a create time.
func stampCreates(client *fake.Clientset) {
	var n atomic.Int64
	client.PrependReactor("create", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		cm := action.(k8stesting.CreateAction).GetObject().(*corev1.ConfigMap)
		i := n.Add(1)
		cm.UID = types.UID(fmt.Sprintf("object-uid-%d", i))
		cm.ResourceVersion = strconv.FormatInt(i, 10)
		if cm.CreationTimestamp.IsZero() {
			cm.CreationTimestamp = metav1.Now()
		}
		return false, nil, nil
	})
	client.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		// An update gets a new resource version and keeps the create time.
		cm := action.(k8stesting.UpdateAction).GetObject().(*corev1.ConfigMap)
		cm.ResourceVersion = strconv.FormatInt(n.Add(1), 10)
		if cur, err := client.Tracker().Get(action.GetResource(), cm.Namespace, cm.Name); err == nil {
			cm.CreationTimestamp = cur.(*corev1.ConfigMap).CreationTimestamp
		}
		return false, nil, nil
	})
}

// put writes a record, over an object of the environment when it has one,
// and waits until the informer has that write.
func put(t *testing.T, s *Store, r Record) Record {
	t.Helper()
	err := s.Put(t.Context(), r)
	var exists *ExistsError
	if errors.As(err, &exists) {
		err = s.Replace(t.Context(), r, exists.Existing)
	}
	if err != nil {
		t.Fatal(err)
	}
	return waitFor(t, s, r.Atespace, r.ID, func(got Record, ok bool) bool { return ok && got.UID == r.UID })
}

func waitFor(t *testing.T, s *Store, atespace, id string, done func(Record, bool) bool) Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, ok, err := s.Get(atespace, id)
		if err != nil {
			t.Fatalf("Get(%s, %s): %v", atespace, id, err)
		}
		if done(got, ok) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("the informer did not reach the wanted state of %s/%s; last: %+v, %v", atespace, id, got, ok)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func actorsOf(uids map[string]map[string]string) ActorUIDs {
	return func(_ context.Context, atespace string) (map[string]string, error) {
		return uids[atespace], nil
	}
}

func TestPutThenGetReturnsTheRecordFromTheInformer(t *testing.T) {
	s, client := newStore(t)
	expire := time.Date(2026, 11, 15, 12, 0, 0, 500, time.UTC)
	want := Record{Atespace: "team", ID: "env-1", UID: "uid-1", Labels: map[string]string{"run": "a b/c", "": "x"}, ExpireTime: expire}
	got := put(t, s, want)
	if got.UID != "uid-1" || !got.ExpireTime.Equal(expire) || got.Labels["run"] != "a b/c" || len(got.Labels) != 2 {
		t.Errorf("Get = %+v, want the record that was put", got)
	}

	cm, err := client.CoreV1().ConfigMaps(testNamespace).Get(t.Context(), "ate-env.team.env-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the object is not named ate-env.<atespace>.<id>: %v", err)
	}
	if cm.Labels[LabelAtespace] != "team" || cm.Labels[LabelDeadline] != "true" {
		t.Errorf("object labels = %v, want the atespace and the deadline mark", cm.Labels)
	}

	// A record without a deadline has no deadline mark, and one of another
	// environment is not found.
	put(t, s, Record{Atespace: "team", ID: "env-2", UID: "uid-2"})
	cm, _ = client.CoreV1().ConfigMaps(testNamespace).Get(t.Context(), "ate-env.team.env-2", metav1.GetOptions{})
	if _, marked := cm.Labels[LabelDeadline]; marked {
		t.Errorf("object labels = %v, want no deadline mark", cm.Labels)
	}
	if _, ok, err := s.Get("other", "env-1"); ok || err != nil {
		t.Errorf("Get of another atespace = %v, %v, want no record", ok, err)
	}
}

// TestPutNeverDeletes checks EM-14: a write for an environment whose id has
// an object leaves that object, and gives it to the caller. The case is the
// late write: replica A created the actor uid-1 and was slow to write its
// object; the caller deleted the environment and created the id again;
// replica B wrote the object of the actor uid-2; then the write of A comes.
func TestPutNeverDeletes(t *testing.T) {
	s, client := newStore(t)
	put(t, s, Record{Atespace: "team", ID: "env-1", UID: "uid-2", Labels: map[string]string{"run": "b"}})
	client.ClearActions()

	err := s.Put(t.Context(), Record{Atespace: "team", ID: "env-1", UID: "uid-1"})
	var exists *ExistsError
	if !errors.As(err, &exists) || exists.Existing.UID != "uid-2" || exists.Existing.Labels["run"] != "b" {
		t.Fatalf("the late Put = %v, want an ExistsError with the object of the actor uid-2", err)
	}
	var verbs []string
	for _, a := range client.Actions() {
		verbs = append(verbs, a.GetVerb())
	}
	if got := strings.Join(verbs, " "); got != "create list" {
		t.Errorf("the late Put made the calls %q, want a refused create and a read, and no delete", got)
	}
	cm, err := client.CoreV1().ConfigMaps(testNamespace).Get(t.Context(), ObjectName("team", "env-1"), metav1.GetOptions{})
	if err != nil || cm.Data[dataUID] != "uid-2" {
		t.Errorf("after the late Put the object holds the actor %q (%v), want uid-2", cm.Data[dataUID], err)
	}

	// An object of that name that holds no record, here one that is not of
	// the store, is not removed either, and comes back with no actor uid.
	foreign := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ObjectName("team", "env-9"), Namespace: testNamespace}}
	if _, err := client.CoreV1().ConfigMaps(testNamespace).Create(t.Context(), foreign, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	err = s.Put(t.Context(), Record{Atespace: "team", ID: "env-9", UID: "uid-9"})
	if !errors.As(err, &exists) || exists.Existing.UID != "" || exists.Existing.objectUID == "" {
		t.Fatalf("Put over an object with no record = %v, want an ExistsError with the identity of that object and no actor uid", err)
	}
}

// TestReplaceWritesOverTheObjectItWasGiven: the write over an object, which
// the caller of Put decides, deletes that object by its uid and resource
// version and creates a new one.
func TestReplaceWritesOverTheObjectItWasGiven(t *testing.T) {
	s, client := newStore(t)
	put(t, s, Record{Atespace: "team", ID: "env-1", UID: "uid-old", Labels: map[string]string{"run": "old"}, ExpireTime: time.Now()})
	r := Record{Atespace: "team", ID: "env-1", UID: "uid-new"}
	var exists *ExistsError
	if err := s.Put(t.Context(), r); !errors.As(err, &exists) {
		t.Fatalf("Put = %v, want an ExistsError", err)
	}
	if err := s.Replace(t.Context(), r, Record{Atespace: "team", ID: "env-1"}); err == nil {
		t.Error("Replace over a record that is not of an ExistsError = nil, want an error: its delete would have no precondition")
	}

	var options []metav1.DeleteOptions
	client.PrependReactor("delete", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		options = append(options, action.(k8stesting.DeleteActionImpl).DeleteOptions)
		return false, nil, nil
	})
	client.ClearActions()
	if err := s.Replace(t.Context(), r, exists.Existing); err != nil {
		t.Fatal(err)
	}
	got := waitFor(t, s, "team", "env-1", func(got Record, ok bool) bool { return ok && got.UID == "uid-new" })
	if len(got.Labels) != 0 || !got.ExpireTime.IsZero() {
		t.Errorf("after Replace, Get = %+v, want only the new record", got)
	}
	// A new object, not an update of the old one: the grace period of the
	// orphan sweep counts from the create time of the object.
	var verbs []string
	for _, a := range client.Actions() {
		verbs = append(verbs, a.GetVerb())
	}
	if got := strings.Join(verbs, " "); got != "delete create" {
		t.Errorf("Replace made the calls %q, want a delete and a create", got)
	}
	if len(options) != 1 || options[0].Preconditions == nil {
		t.Fatalf("delete options = %+v, want one delete with preconditions", options)
	}
	pre := options[0].Preconditions
	if pre.UID == nil || *pre.UID != exists.Existing.objectUID || pre.ResourceVersion == nil || *pre.ResourceVersion != exists.Existing.resourceVersion {
		t.Errorf("preconditions = %+v, want the uid and the resource version of the object that Put returned", pre)
	}
}

// TestReplaceLeavesAnObjectThatChanged: when the object was written again
// since Put read it, Kubernetes refuses the delete, and Replace writes
// nothing. The fake clientset does not enforce a precondition, thus the test
// gives that answer itself.
func TestReplaceLeavesAnObjectThatChanged(t *testing.T) {
	s, client := newStore(t)
	put(t, s, Record{Atespace: "team", ID: "env-1", UID: "uid-2"})
	r := Record{Atespace: "team", ID: "env-1", UID: "uid-1"}
	var exists *ExistsError
	if err := s.Put(t.Context(), r); !errors.As(err, &exists) {
		t.Fatalf("Put = %v, want an ExistsError", err)
	}
	client.PrependReactor("delete", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "x", errors.New("precondition failed"))
	})
	client.ClearActions()
	if err := s.Replace(t.Context(), r, exists.Existing); !errors.Is(err, ErrObjectChanged) {
		t.Fatalf("Replace over an object that changed = %v, want ErrObjectChanged", err)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "create" {
			t.Error("Replace over an object that changed made a create")
		}
	}
	cm, _ := client.CoreV1().ConfigMaps(testNamespace).Get(t.Context(), ObjectName("team", "env-1"), metav1.GetOptions{})
	if cm.Data[dataUID] != "uid-2" {
		t.Errorf("the object holds the actor %q, want uid-2", cm.Data[dataUID])
	}
}

// TestTheStoreMakesOnlyTheCallsOfVerbs holds Verbs, which the Role of
// ate-env manifest --environment-metadata grants, to the calls the store
// makes: a call with another verb is one the Role refuses, and a verb with no
// call is a permission ate-env-api does not need.
func TestTheStoreMakesOnlyTheCallsOfVerbs(t *testing.T) {
	s, client := newStore(t)
	// Each path of the store: a first write, a write over an object, a
	// change in place, a read of the informer and of the API server, a
	// delete, and a sweep that deletes an orphan and an unreadable object.
	put(t, s, Record{Atespace: "team", ID: "env-1", UID: "uid-1"})
	first := put(t, s, Record{Atespace: "team", ID: "env-1", UID: "uid-1", Labels: map[string]string{"run": "a"}})
	next := first
	next.ExpireTime = time.Now().Add(time.Hour)
	if err := s.Update(t.Context(), next, first); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Read(t.Context(), "team", "env-1"); err != nil {
		t.Fatal(err)
	}
	s.Expired(time.Now())
	got := put(t, s, Record{Atespace: "team", ID: "env-2", UID: "uid-2"})
	if err := s.Delete(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	put(t, s, Record{Atespace: "team", ID: "orphan", UID: "uid-gone"})
	if n, err := s.SweepOrphans(t.Context(), actorsOf(map[string]map[string]string{"team": {"env-1": "uid-1"}}), 0, time.Now().Add(time.Hour)); n != 1 || err != nil {
		t.Fatalf("sweep = %d, %v, want the orphan deleted", n, err)
	}

	used := map[string]bool{}
	for _, a := range client.Actions() {
		if a.GetResource().Resource != "configmaps" {
			t.Errorf("the store called %s on %s, want only configmaps", a.GetVerb(), a.GetResource().Resource)
		}
		used[a.GetVerb()] = true
	}
	want := map[string]bool{}
	for _, v := range Verbs {
		want[v] = true
	}
	if !maps.Equal(used, want) {
		t.Errorf("the store used the verbs %v, and Verbs is %v: the Role must grant each call and no other", slices.Sorted(maps.Keys(used)), Verbs)
	}
}

// TestRunFailsWhenTheFirstListDoesNot: without the Role, or without the
// Kubernetes API, a start must end with an error and not wait.
func TestRunFailsWhenTheFirstListDoesNot(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("list", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "", errors.New("no Role"))
	})
	start := time.Now()
	err := New(client.CoreV1().ConfigMaps(testNamespace), testNamespace).Run(t.Context(), 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "--environment-metadata") {
		t.Fatalf("Run = %v, want an error that names the Role's source", err)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Errorf("Run waited %s, want about the sync timeout", waited)
	}
}

// TestTheInformerSelectsTheObjectsOfTheStore: the list and the watch carry
// the store's label, thus the informer holds no other ConfigMap of the
// namespace.
func TestTheInformerSelectsTheObjectsOfTheStore(t *testing.T) {
	_, client := newStore(t)
	seen := map[string]bool{}
	for _, a := range client.Actions() {
		switch a := a.(type) {
		case k8stesting.ListActionImpl:
			seen["list"] = a.GetListRestrictions().Labels.String() == LabelAtespace
		case k8stesting.WatchActionImpl:
			seen["watch"] = a.GetWatchRestrictions().Labels.String() == LabelAtespace
		}
	}
	if !seen["list"] || !seen["watch"] {
		t.Errorf("label selector on the informer's calls: %v, want %q on the list and the watch", seen, LabelAtespace)
	}
}

func TestPutNeedsTheActorUID(t *testing.T) {
	s, client := newStore(t)
	if err := s.Put(t.Context(), Record{Atespace: "team", ID: "env-1"}); err == nil {
		t.Error("Put without a uid: want an error")
	}
	if n := len(client.Actions()); n > 2 { // the informer's list and watch
		t.Errorf("Put without a uid made a write: %v", client.Actions()[2:])
	}
}

// TestDeleteRemovesOnlyTheObjectItWasGiven checks the delete of EM-6: the
// delete carries the identity of the record's object, and an object that is
// gone, or that was written again since, is not an error.
func TestDeleteRemovesOnlyTheObjectItWasGiven(t *testing.T) {
	s, client := newStore(t, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: ObjectName("team", "env-1"), Namespace: testNamespace,
			Labels: map[string]string{LabelAtespace: "team"},
			UID:    "object-uid-7", ResourceVersion: "41",
		},
		Data: map[string]string{dataUID: "uid-1"},
	})
	r := waitFor(t, s, "team", "env-1", func(_ Record, ok bool) bool { return ok })

	var options []metav1.DeleteOptions
	answer := error(nil)
	client.PrependReactor("delete", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		options = append(options, action.(k8stesting.DeleteActionImpl).DeleteOptions)
		return answer != nil, nil, answer
	})
	// As the API server answers for an object that changed, and for none.
	for _, answer = range []error{
		apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "x", errors.New("precondition failed")),
		apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "x"),
	} {
		if err := s.Delete(t.Context(), r); err != nil {
			t.Errorf("Delete with the answer %v = %v, want nil", answer, err)
		}
	}
	answer = nil
	if err := s.Delete(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	waitFor(t, s, "team", "env-1", func(_ Record, ok bool) bool { return !ok })
	for _, o := range options {
		pre := o.Preconditions
		if pre == nil || pre.UID == nil || *pre.UID != "object-uid-7" || pre.ResourceVersion == nil || *pre.ResourceVersion != "41" {
			t.Errorf("preconditions = %+v, want the uid and the resource version of the record's object", pre)
		}
	}
}

// TestSweepOrphans checks EM-7 (the foreign ConfigMap is one the sweep must
// never touch: it has no label of the store): an object whose actor is gone, or has another
// uid, is deleted after the grace period and not before; an object whose
// actor exists with the same uid is never deleted.
func TestSweepOrphans(t *testing.T) {
	foreign := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "someone-elses", Namespace: testNamespace}}
	s, client := newStore(t, foreign)
	for _, r := range []Record{
		{Atespace: "team", ID: "live", UID: "uid-live"},
		{Atespace: "team", ID: "gone", UID: "uid-gone"},
		{Atespace: "team", ID: "reused", UID: "uid-first"},
		{Atespace: "other", ID: "live", UID: "uid-other"},
	} {
		put(t, s, r)
	}
	actors := actorsOf(map[string]map[string]string{
		"team":  {"live": "uid-live", "reused": "uid-second"},
		"other": {"live": "uid-other"},
	})
	written := waitFor(t, s, "team", "gone", func(_ Record, ok bool) bool { return ok }).Written

	// Inside the grace period nothing is an orphan yet, and no actor is read.
	read := 0
	counting := func(ctx context.Context, atespace string) (map[string]string, error) {
		read++
		return actors(ctx, atespace)
	}
	if n, err := s.SweepOrphans(t.Context(), counting, time.Minute, written.Add(59*time.Second)); n != 0 || err != nil || read != 0 {
		t.Fatalf("sweep inside the grace period = %d, %v with %d actor reads, want nothing", n, err, read)
	}

	// The objects have the create times of their writes: the last one is the
	// youngest.
	last := waitFor(t, s, "other", "live", func(_ Record, ok bool) bool { return ok }).Written
	n, err := s.SweepOrphans(t.Context(), counting, time.Minute, last.Add(time.Minute))
	if n != 2 || err != nil {
		t.Fatalf("sweep after the grace period = %d, %v, want 2 deleted", n, err)
	}
	if read != 2 {
		t.Errorf("the sweep read the actors %d times, want once for each atespace", read)
	}
	left, _ := client.CoreV1().ConfigMaps(testNamespace).List(t.Context(), metav1.ListOptions{})
	var names []string
	for _, cm := range left.Items {
		names = append(names, cm.Name)
	}
	if got, want := strings.Join(names, " "), "ate-env.other.live ate-env.team.live someone-elses"; got != want {
		t.Errorf("objects after the sweep = %q, want %q", got, want)
	}
}

// TestSweepOrphansLeavesAnObjectWithNoCreateTime: the grace period counts
// from the create time of the object. An object without one has no age, and
// the sweep must not read the zero time as an age of centuries.
func TestSweepOrphansLeavesAnObjectWithNoCreateTime(t *testing.T) {
	orphan := func(id string, created metav1.Time) *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: ObjectName("team", id), Namespace: testNamespace,
				Labels: map[string]string{LabelAtespace: "team"}, CreationTimestamp: created,
				UID: types.UID("object-" + id), ResourceVersion: "1",
			},
			Data: map[string]string{dataUID: "uid-gone"},
		}
	}
	now := metav1.Now()
	s, client := newStore(t, orphan("stamped", now), orphan("no-time", metav1.Time{}),
		// One that holds no record: the same rule.
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: "ate-env.team.no-time-no-uid", Namespace: testNamespace, Labels: map[string]string{LabelAtespace: "team"},
		}})
	if n, err := s.SweepOrphans(t.Context(), actorsOf(nil), time.Minute, now.Add(30*time.Second)); n != 0 || err != nil {
		t.Fatalf("sweep inside the grace period = %d, %v, want nothing: an object with no create time is not old", n, err)
	}
	if n, err := s.SweepOrphans(t.Context(), actorsOf(nil), time.Minute, now.Add(time.Hour)); n != 1 || err != nil {
		t.Fatalf("sweep after the grace period = %d, %v, want only the object with a create time deleted", n, err)
	}
	left, _ := client.CoreV1().ConfigMaps(testNamespace).List(t.Context(), metav1.ListOptions{})
	var names []string
	for _, cm := range left.Items {
		names = append(names, cm.Name)
	}
	if got, want := strings.Join(names, " "), "ate-env.team.no-time ate-env.team.no-time-no-uid"; got != want {
		t.Errorf("objects after the sweep = %q, want %q", got, want)
	}
}

// TestSweepOrphansLeavesAnAtespaceItCannotRead checks a clause of EM-7: an
// object is an orphan only when the read of its atespace's actors says so.
func TestSweepOrphansLeavesAnAtespaceItCannotRead(t *testing.T) {
	s, _ := newStore(t)
	put(t, s, Record{Atespace: "down", ID: "env-1", UID: "uid-1"})
	put(t, s, Record{Atespace: "up", ID: "env-1", UID: "uid-1"})
	actors := func(_ context.Context, atespace string) (map[string]string, error) {
		if atespace == "down" {
			return nil, errors.New("control plane unavailable")
		}
		return nil, nil
	}
	n, err := s.SweepOrphans(t.Context(), actors, 0, time.Now().Add(time.Hour))
	if n != 1 || err == nil || !strings.Contains(err.Error(), `"down"`) {
		t.Fatalf("sweep = %d, %v, want the orphan of the readable atespace deleted and an error for the other", n, err)
	}
	waitFor(t, s, "down", "env-1", func(_ Record, ok bool) bool { return ok })
}

// TestSweepOrphansDeletesOnlyTheObjectItRead checks a clause of EM-7: the
// delete carries the identity of the informer's copy, and the answer of the
// API server for an object that was written again since is not an error.
// The fake clientset does not enforce a precondition, thus the test gives
// that answer itself: that Kubernetes refuses the delete is not tested here.
func TestSweepOrphansDeletesOnlyTheObjectItRead(t *testing.T) {
	s, client := newStore(t, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: ObjectName("team", "env-1"), Namespace: testNamespace,
			Labels: map[string]string{LabelAtespace: "team"},
			UID:    "object-uid-7", ResourceVersion: "41",
			CreationTimestamp: metav1.Now(),
		},
		Data: map[string]string{dataUID: "uid-1"},
	})
	seen := waitFor(t, s, "team", "env-1", func(_ Record, ok bool) bool { return ok })
	if seen.objectUID != "object-uid-7" || seen.resourceVersion != "41" {
		t.Fatalf("the informer's copy has uid %q and resource version %q, want those of the object", seen.objectUID, seen.resourceVersion)
	}

	var options []metav1.DeleteOptions
	client.PrependReactor("delete", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		options = append(options, action.(k8stesting.DeleteActionImpl).DeleteOptions)
		// As the API server answers when the object changed since.
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "ate-env.team.env-1", errors.New("precondition failed"))
	})
	n, err := s.SweepOrphans(t.Context(), actorsOf(nil), 0, time.Now().Add(time.Hour))
	if n != 0 || err != nil {
		t.Fatalf("sweep of an object that changed = %d, %v, want nothing deleted and no error", n, err)
	}
	if len(options) != 1 || options[0].Preconditions == nil {
		t.Fatalf("delete options = %+v, want one delete with preconditions", options)
	}
	pre := options[0].Preconditions
	if pre.UID == nil || *pre.UID != "object-uid-7" || pre.ResourceVersion == nil || *pre.ResourceVersion != "41" {
		t.Errorf("preconditions = %+v, want the uid and the resource version of the informer's copy", pre)
	}
}

// TestSweepOrphansDeletesAnObjectItCannotRead checks a clause of EM-7: an
// object with the store's label and no readable record belongs to no
// environment, and goes after the grace period, with a report.
func TestSweepOrphansDeletesAnObjectItCannotRead(t *testing.T) {
	broken := func(name string, data map[string]string) *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: testNamespace, Labels: map[string]string{LabelAtespace: "team"},
				CreationTimestamp: metav1.Now(),
			},
			Data: data,
		}
	}
	s, client := newStore(t,
		broken("ate-env.team.no-uid", nil),
		broken("ate-env.team.bad-labels", map[string]string{dataUID: "uid-1", dataLabels: "{"}),
		broken("not-an-environment-name", map[string]string{dataUID: "uid-1"}),
	)
	put(t, s, Record{Atespace: "team", ID: "live", UID: "uid-live"})
	actors := actorsOf(map[string]map[string]string{"team": {"live": "uid-live"}})

	if n, err := s.SweepOrphans(t.Context(), actors, time.Hour, time.Now()); n != 0 || err != nil {
		t.Fatalf("sweep inside the grace period = %d, %v, want nothing", n, err)
	}
	n, err := s.SweepOrphans(t.Context(), actors, 0, time.Now().Add(time.Hour))
	if n != 3 || err == nil || strings.Count(err.Error(), "the sweep deleted it") != 3 {
		t.Fatalf("sweep = %d, %v, want the three unreadable objects deleted and reported", n, err)
	}
	left, _ := client.CoreV1().ConfigMaps(testNamespace).List(t.Context(), metav1.ListOptions{})
	if len(left.Items) != 1 || left.Items[0].Name != "ate-env.team.live" {
		t.Errorf("objects after the sweep = %v, want only the live one", left.Items)
	}
}

// TestSweepOrphansReportsAnUnreadableObjectItDidNotDelete: when the object
// was written again since the informer read it, Kubernetes refuses the
// delete. The report must not say that the sweep deleted it.
func TestSweepOrphansReportsAnUnreadableObjectItDidNotDelete(t *testing.T) {
	s, client := newStore(t, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ate-env.team.no-uid", Namespace: testNamespace, Labels: map[string]string{LabelAtespace: "team"},
			CreationTimestamp: metav1.Now(), UID: "object-uid-3", ResourceVersion: "9",
		},
	})
	client.PrependReactor("delete", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "ate-env.team.no-uid", errors.New("precondition failed"))
	})
	n, err := s.SweepOrphans(t.Context(), actorsOf(nil), 0, time.Now().Add(time.Hour))
	if n != 0 || err == nil {
		t.Fatalf("sweep = %d, %v, want nothing deleted and the object reported", n, err)
	}
	if strings.Contains(err.Error(), "the sweep deleted it") || !strings.Contains(err.Error(), "written again") {
		t.Errorf("report = %q, want that the object was written again and stays, not that the sweep deleted it", err)
	}
}

// TestUpdateChangesTheObjectItWasGiven: an update, the write of a new
// deadline (EM-10) and of the count of failed deletes (EM-15), changes the
// object in place and names it by uid and resource version, thus the API
// server refuses it for an object that was written again since. The test
// checks that the update carries them and what the store does with the
// refusal, not that Kubernetes refuses.
func TestUpdateChangesTheObjectItWasGiven(t *testing.T) {
	s, client := newStore(t)
	first := put(t, s, Record{Atespace: "team", ID: "env-1", UID: "uid-1", Labels: map[string]string{"run": "a"}})

	var sent []*corev1.ConfigMap
	answer := error(nil)
	client.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		sent = append(sent, action.(k8stesting.UpdateAction).GetObject().(*corev1.ConfigMap).DeepCopy())
		return answer != nil, nil, answer
	})

	expire := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	next := first
	next.ExpireTime, next.DeleteFailures, next.LastDeleteFailure = expire, 3, expire.Add(time.Minute)
	if err := s.Update(t.Context(), next, first); err != nil {
		t.Fatal(err)
	}
	got := waitFor(t, s, "team", "env-1", func(r Record, ok bool) bool { return ok && !r.ExpireTime.IsZero() })
	if !got.ExpireTime.Equal(expire) || got.DeleteFailures != 3 || !got.LastDeleteFailure.Equal(expire.Add(time.Minute)) || got.DeleteFailed || got.Labels["run"] != "a" || !got.Written.Equal(first.Written) {
		t.Errorf("after the update: %+v, want the deadline, 3 failures, the labels and the create time of the first write", got)
	}
	if len(sent) != 1 || sent[0].UID != first.objectUID || sent[0].ResourceVersion != first.resourceVersion {
		t.Fatalf("the update sent %+v, want the uid and the resource version of the object it read", sent)
	}

	// The mark of a delete that failed for good is a label value, and a
	// record with no deadline has neither the mark nor the count.
	marked := got
	marked.DeleteFailures, marked.DeleteFailed = 5, true
	if err := s.Update(t.Context(), marked, got); err != nil {
		t.Fatal(err)
	}
	got = waitFor(t, s, "team", "env-1", func(r Record, ok bool) bool { return ok && r.DeleteFailed })
	cm, _ := client.CoreV1().ConfigMaps(testNamespace).Get(t.Context(), ObjectName("team", "env-1"), metav1.GetOptions{})
	if got.DeleteFailures != 5 || cm.Labels[LabelDeadline] != DeadlineFailed {
		t.Errorf("marked: %+v, labels %v, want 5 failures and the label value %q", got, cm.Labels, DeadlineFailed)
	}
	cleared := got
	cleared.ExpireTime = time.Time{}
	if err := s.Update(t.Context(), cleared, got); err != nil {
		t.Fatal(err)
	}
	got = waitFor(t, s, "team", "env-1", func(r Record, ok bool) bool { return ok && r.ExpireTime.IsZero() })
	if got.DeleteFailed || got.DeleteFailures != 0 || !got.LastDeleteFailure.IsZero() {
		t.Errorf("with no deadline: %+v, want no mark and no count", got)
	}

	// As the API server answers for an object that changed, and for none.
	for _, answer = range []error{
		apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "x", errors.New("the object has been modified")),
		apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "x"),
	} {
		if err := s.Update(t.Context(), next, got); !errors.Is(err, ErrObjectChanged) {
			t.Errorf("Update with the answer %v = %v, want ErrObjectChanged", answer, err)
		}
	}
	answer = nil

	// An update is for the object of the same actor. Over the object of
	// another actor, the new object needs its own create time: Replace.
	other := got
	other.UID = "uid-2"
	before := len(sent)
	if err := s.Update(t.Context(), other, got); err == nil || len(sent) != before {
		t.Errorf("Update to another actor = %v after %d calls, want an error and no call", err, len(sent)-before)
	}
	if err := s.Update(t.Context(), next, Record{Atespace: "team", ID: "env-1", UID: "uid-1"}); err == nil {
		t.Error("Update over a record that the store did not read = nil, want an error")
	}
}

// TestReadReturnsTheCopyOfTheAPIServer checks the read of EM-F: Read gives
// the object as the API server has it, also when the informer is behind.
// The informer of the test gets only the events the test sends it.
func TestReadReturnsTheCopyOfTheAPIServer(t *testing.T) {
	client := fake.NewSimpleClientset()
	stampCreates(client)
	events := watch.NewFakeWithChanSize(10, false)
	client.PrependWatchReactor("configmaps", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, events, nil
	})
	s := New(client.CoreV1().ConfigMaps(testNamespace), testNamespace)
	if err := s.Run(t.Context(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.Read(t.Context(), "team", "env-1"); found || err != nil {
		t.Fatalf("Read with no object = %v, %v, want not found", found, err)
	}

	early := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	if err := s.Put(t.Context(), Record{Atespace: "team", ID: "env-1", UID: "uid-1", ExpireTime: early}); err != nil {
		t.Fatal(err)
	}
	cm, err := client.CoreV1().ConfigMaps(testNamespace).Get(t.Context(), ObjectName("team", "env-1"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	events.Add(cm)
	seen := waitFor(t, s, "team", "env-1", func(_ Record, ok bool) bool { return ok })

	// A write whose event the informer does not get: a write of another
	// replica that this one has not seen yet.
	next := seen
	next.ExpireTime = early.Add(time.Hour)
	if err := s.Update(t.Context(), next, seen); err != nil {
		t.Fatal(err)
	}
	fresh, found, err := s.Read(t.Context(), "team", "env-1")
	if err != nil || !found || !fresh.ExpireTime.Equal(early.Add(time.Hour)) || fresh.UID != "uid-1" {
		t.Errorf("Read = %+v, %v, %v, want the deadline of the write", fresh, found, err)
	}
	if stale, _, _ := s.Get("team", "env-1"); !stale.ExpireTime.Equal(early) {
		t.Errorf("the informer has the deadline %s, want the one before the write: the test does not check a late informer", stale.ExpireTime)
	}
	// The record of Read names the object for a later write.
	again := fresh
	again.DeleteFailures = 1
	if err := s.Update(t.Context(), again, fresh); err != nil {
		t.Errorf("Update over the record of Read = %v", err)
	}

	client.PrependReactor("list", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the Kubernetes API is down")
	})
	if _, _, err := s.Read(t.Context(), "team", "env-1"); err == nil {
		t.Error("Read with the API down = nil, want the error: a sweep must not read it as no object")
	}
}

// TestExpiredListsTheObjectsPastTheirDeadline: the list of the deadline
// sweep (EM-9) holds the objects whose deadline is at or before now, and not
// one with no deadline, with a later one, or with the mark of EM-15.
func TestExpiredListsTheObjectsPastTheirDeadline(t *testing.T) {
	s, _ := newStore(t)
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	put(t, s, Record{Atespace: "team", ID: "past", UID: "uid-1", ExpireTime: now.Add(-time.Second), DeleteFailures: 2})
	put(t, s, Record{Atespace: "team", ID: "now", UID: "uid-2", ExpireTime: now})
	put(t, s, Record{Atespace: "other", ID: "later", UID: "uid-3", ExpireTime: now.Add(time.Second)})
	put(t, s, Record{Atespace: "team", ID: "none", UID: "uid-4", Labels: map[string]string{"run": "a"}})
	put(t, s, Record{Atespace: "team", ID: "failed", UID: "uid-5", ExpireTime: now.Add(-time.Hour), DeleteFailures: 5, DeleteFailed: true})

	var got []string
	for _, r := range s.Expired(now) {
		got = append(got, fmt.Sprintf("%s/%s/%s/%d", r.Atespace, r.ID, r.UID, r.DeleteFailures))
	}
	slices.Sort(got)
	if want := []string{"team/now/uid-2/0", "team/past/uid-1/2"}; !slices.Equal(got, want) {
		t.Errorf("Expired = %v, want %v", got, want)
	}
	if got := s.Expired(now.Add(time.Second)); len(got) != 3 {
		t.Errorf("Expired one second later = %d records, want 3", len(got))
	}
}

// TestRunSweepSweepsOnEachInterval checks the timer of EM-9: RunSweep calls
// the sweep once for each interval, with the time of the tick, and stops
// when its context ends. The clock is synctest's.
func TestRunSweepSweepsOnEachInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		start := time.Now()
		var at []time.Duration
		done := make(chan struct{})
		go func() {
			defer close(done)
			RunSweep(ctx, func(_ context.Context, now time.Time) {
				at = append(at, now.Sub(start))
			})
		}()
		time.Sleep(3*SweepInterval + time.Second)
		synctest.Wait()
		cancel()
		<-done
		if want := []time.Duration{SweepInterval, 2 * SweepInterval, 3 * SweepInterval}; !slices.Equal(at, want) {
			t.Errorf("sweeps at %v after the start, want %v", at, want)
		}
	})
}
