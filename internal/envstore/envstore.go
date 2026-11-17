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

// Package envstore keeps what ate-env-api knows about an environment and
// Substrate has no field for: its labels and its deadline. A Substrate actor
// has a name, timestamps, a template, a worker selector and a status, and
// ate-env-api keeps no state of its own, thus the data goes to Kubernetes:
// one ConfigMap for each environment that has a label or a deadline, in the
// namespace of ate-env-api. docs/plans/ate-env-api/environment-metadata.md
// is the design.
//
// The actor is the truth: it decides that an environment exists, and an
// object only adds to it. An object holds the uid of its actor, which tells
// an environment from a later one with the same id. An object whose actor is
// gone, or has another uid, is an orphan, and SweepOrphans deletes it.
//
// A deadline is a time after which ate-env-api deletes the environment.
// Expired lists the objects whose deadline is past, and Read gives the copy
// of the Kubernetes API server, which the caller checks before it deletes.
//
// Reads come from an informer and never call the Kubernetes API. Thus a read
// on one replica follows a write on another after the delay of a watch event.
package envstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
)

const (
	// LabelAtespace is on each object of the store, with the atespace of its
	// environment. The informer selects the objects that have it.
	LabelAtespace = "ate-env.dev/atespace"
	// LabelDeadline is on an object that holds a deadline: DeadlineSet, or
	// DeadlineFailed once the sweep stopped trying to delete the environment
	// (Record.DeleteFailed). `kubectl get configmaps -l
	// ate-env.dev/deadline=failed` lists those environments.
	LabelDeadline  = "ate-env.dev/deadline"
	DeadlineSet    = "true"
	DeadlineFailed = "failed"

	dataLabels         = "labels"
	dataExpireTime     = "expire_time"
	dataUID            = "uid"
	dataDeleteFailures = "delete_failures"
	dataDeleteFailTime = "delete_failure_time"
)

// Verbs are the calls the store makes on ConfigMaps, and all of them: the
// informer lists and watches, a first write creates, a write over the object
// of an earlier environment deletes and creates, a change of the object of
// the same environment updates, and a delete deletes. Reads come from the
// informer, and a read of the API server (Read, and the object that refused
// a write) is a list of one name, thus the store never gets one object. The
// Role of ate-env manifest --environment-metadata grants these verbs and no
// other; a test of each side holds the two together.
var Verbs = []string{"list", "watch", "create", "update", "delete"}

// Record is what the store holds for one environment.
type Record struct {
	Atespace string
	ID       string
	// UID is the uid of the environment's actor in Substrate.
	UID string
	// Labels are the caller's labels. They are data of the object, not
	// Kubernetes labels: a label value is free text.
	Labels map[string]string
	// ExpireTime is the deadline; zero means none.
	ExpireTime time.Time
	// DeleteFailures counts the deletes by deadline that Substrate refused.
	// The count is in the object, thus the replicas share it.
	DeleteFailures int
	// LastDeleteFailure is when the last of DeleteFailures was counted.
	LastDeleteFailure time.Time
	// DeleteFailed marks an object whose environment the sweep stopped
	// trying to delete. Expired does not list it. It needs a deadline.
	DeleteFailed bool

	// Written is when Kubernetes created the object. Set on a read.
	Written time.Time
	// The identity of the object as the read saw it (the informer's, or the
	// API server's for Read and ExistsError), for a delete or an update that
	// must not remove a later write.
	objectUID       types.UID
	resourceVersion string
}

// Store reads and writes the objects of one namespace.
type Store struct {
	maps      corev1client.ConfigMapInterface
	namespace string
	informer  cache.SharedIndexInformer
}

// New returns a store on maps, the ConfigMaps of namespace. Call Run before
// a read.
//
// The store takes the ConfigMap client alone, and builds its informer on it,
// in place of client-go's clientset and informer factory: those link the
// client and the informer of each Kubernetes API group into ate-env-api.
func New(maps corev1client.ConfigMapInterface, namespace string) *Store {
	selected := func(o metav1.ListOptions) metav1.ListOptions {
		o.LabelSelector = LabelAtespace
		return o
	}
	lw := &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
			return maps.List(ctx, selected(o))
		},
		WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
			return maps.Watch(ctx, selected(o))
		},
	}
	return &Store{
		maps:      maps,
		namespace: namespace,
		informer:  cache.NewSharedIndexInformer(plainListWatch{lw}, &corev1.ConfigMap{}, 0, cache.Indexers{}),
	}
}

// plainListWatch makes the informer start with a list and then watch. The
// other start, a watch that streams the list (client-go's default for a
// client that allows it), gains nothing for the few objects of the store,
// and a fake client cannot serve it.
type plainListWatch struct{ *cache.ListWatch }

// IsWatchListSemanticsUnSupported is what client-go's reflector asks.
func (plainListWatch) IsWatchListSemanticsUnSupported() bool { return true }

// Run starts the informer and returns when it holds the objects that exist,
// or with an error when it does not within syncTimeout: without the Role, or
// without the Kubernetes API, the first list never succeeds, and a server
// that waited for it would never listen. The informer stops when ctx ends.
func (s *Store) Run(ctx context.Context, syncTimeout time.Duration) error {
	go s.informer.Run(ctx.Done())
	wait, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	if !cache.WaitForCacheSync(wait.Done(), s.informer.HasSynced) {
		return fmt.Errorf("envstore: the first list of the ConfigMaps of namespace %q did not succeed within %s: is the Role of ate-env manifest --environment-metadata in place?", s.namespace, syncTimeout)
	}
	return nil
}

// ObjectName is the name of the object of an environment. An atespace and an
// id are DNS labels, thus the name is a valid object name, and two
// environments never share one.
func ObjectName(atespace, id string) string {
	return "ate-env." + atespace + "." + id
}

// ExistsError is what Put returns when an object with the name of the
// environment exists. Existing is that object as the API server has it, not
// as the informer does. Its UID is the uid of the actor the object was
// written for, and is empty for an object that holds no readable record.
//
// The caller decides what the object is, because only the actor tells: an
// object of the same actor is an earlier write of this environment; an
// object of another actor is of an earlier environment with the same id when
// the actor of the record is the one that exists now, and of a later one
// when it is not. In the last case the write is late and must be dropped:
// the environment it was for is gone. Replace writes over Existing.
type ExistsError struct {
	Existing Record
}

func (e *ExistsError) Error() string {
	return fmt.Sprintf("envstore: an object of %s/%s exists, written for the actor %q", e.Existing.Atespace, e.Existing.ID, e.Existing.UID)
}

// ErrObjectChanged is the error of Replace when the object it was to write
// over was written again since it was read.
var ErrObjectChanged = errors.New("envstore: the object was written again since it was read")

// Put writes the object of an environment that has none. The record needs
// the uid of the actor. When an object of that name exists, Put writes
// nothing and returns an *ExistsError with that object: Put never deletes.
// A write that removed whatever object has the name would let a late write,
// for an environment that was deleted and created again with the same id
// since, remove the object of the new environment.
func (s *Store) Put(ctx context.Context, r Record) error {
	cm, err := s.object(r)
	if err != nil {
		return err
	}
	// An object that goes between the refused create and the read of it is
	// gone for good (a delete of the environment or the sweep): try once more.
	for range 2 {
		_, err := s.maps.Create(ctx, cm, metav1.CreateOptions{})
		if !apierrors.IsAlreadyExists(err) {
			return wrap(err, "writing", r.Atespace, r.ID)
		}
		existing, found, err := s.read(ctx, cm.Name, r.Atespace, r.ID)
		if err != nil {
			return wrap(err, "reading the existing object while writing", r.Atespace, r.ID)
		}
		if found {
			return &ExistsError{Existing: existing}
		}
	}
	return fmt.Errorf("envstore: writing the object of %s/%s: an object of that name comes and goes", r.Atespace, r.ID)
}

// Replace writes the object of an environment in place of existing, the
// object of an ExistsError, and of that object only: the delete carries its
// uid and resource version. It deletes and creates, and does not update: the
// new object then has its own create time, which the grace period of
// SweepOrphans counts from. When the object was written again since,
// Replace writes nothing and returns ErrObjectChanged; when another write
// comes between the delete and the create, it returns that write as an
// *ExistsError.
func (s *Store) Replace(ctx context.Context, r Record, existing Record) error {
	cm, err := s.object(r)
	if err != nil {
		return err
	}
	if existing.objectUID == "" || existing.resourceVersion == "" {
		return errors.New("envstore: Replace needs the record of an ExistsError")
	}
	err = s.maps.Delete(ctx, cm.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &existing.objectUID, ResourceVersion: &existing.resourceVersion},
	})
	switch {
	case err == nil, apierrors.IsNotFound(err):
	case apierrors.IsConflict(err):
		return fmt.Errorf("%w: %s/%s", ErrObjectChanged, r.Atespace, r.ID)
	default:
		return wrap(err, "replacing", r.Atespace, r.ID)
	}
	return s.Put(ctx, r)
}

// object returns the ConfigMap of a record.
func (s *Store) object(r Record) (*corev1.ConfigMap, error) {
	if r.Atespace == "" || r.ID == "" || r.UID == "" {
		return nil, errors.New("envstore: a record needs an atespace, an id and the uid of the actor")
	}
	labels, err := json.Marshal(r.Labels)
	if err != nil {
		return nil, fmt.Errorf("envstore: labels of %s/%s: %w", r.Atespace, r.ID, err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ObjectName(r.Atespace, r.ID),
			Namespace: s.namespace,
			Labels:    map[string]string{LabelAtespace: r.Atespace},
		},
		Data: map[string]string{dataUID: r.UID, dataLabels: string(labels)},
	}
	if !r.ExpireTime.IsZero() {
		cm.Labels[LabelDeadline] = DeadlineSet
		if r.DeleteFailed {
			cm.Labels[LabelDeadline] = DeadlineFailed
		}
		cm.Data[dataExpireTime] = r.ExpireTime.UTC().Format(time.RFC3339Nano)
		if r.DeleteFailures > 0 {
			cm.Data[dataDeleteFailures] = strconv.Itoa(r.DeleteFailures)
			if !r.LastDeleteFailure.IsZero() {
				cm.Data[dataDeleteFailTime] = r.LastDeleteFailure.UTC().Format(time.RFC3339Nano)
			}
		}
	}
	return cm, nil
}

// Update changes the object of an environment in place: existing, a record
// that Get, Read or an ExistsError returned, becomes r. It is for a change
// of the object of the same actor, such as a new deadline: the object keeps
// its create time, and no moment has the environment without its object,
// which a delete and a create would have. The update carries the uid and the
// resource version of existing. When the object is gone or was written again
// since, Update writes nothing and returns ErrObjectChanged.
func (s *Store) Update(ctx context.Context, r Record, existing Record) error {
	cm, err := s.object(r)
	if err != nil {
		return err
	}
	if existing.objectUID == "" || existing.resourceVersion == "" {
		return errors.New("envstore: Update needs a record that the store read")
	}
	if r.UID != existing.UID || r.Atespace != existing.Atespace || r.ID != existing.ID {
		return errors.New("envstore: Update changes the object of the same actor only; Replace writes over the object of another")
	}
	cm.UID = existing.objectUID
	cm.ResourceVersion = existing.resourceVersion
	_, err = s.maps.Update(ctx, cm, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: %s/%s", ErrObjectChanged, r.Atespace, r.ID)
	}
	return wrap(err, "updating", r.Atespace, r.ID)
}

// Read returns the record of an environment from the Kubernetes API server,
// and false when it has no object. It is the read before a delete by
// deadline: the informer of this replica can be behind a write of another.
// An object that holds no readable record comes back with no actor uid.
func (s *Store) Read(ctx context.Context, atespace, id string) (Record, bool, error) {
	r, found, err := s.read(ctx, ObjectName(atespace, id), atespace, id)
	return r, found, wrap(err, "reading", atespace, id)
}

// Expired returns, from the informer, the records whose deadline is at or
// before now and that are not marked DeleteFailed. An object that holds no
// readable record is not in the list; SweepOrphans deletes and reports it.
func (s *Store) Expired(now time.Time) []Record {
	var out []Record
	for _, obj := range s.informer.GetStore().List() {
		cm := obj.(*corev1.ConfigMap)
		if cm.Labels[LabelDeadline] != DeadlineSet {
			continue
		}
		r, err := decode(cm, cm.Labels[LabelAtespace], "")
		if err != nil || r.ExpireTime.IsZero() || r.ExpireTime.After(now) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// read returns the object of that name from the API server, with a list of
// that one name: the Role has no get. An object that holds no readable
// record comes back with its identity and no actor uid.
func (s *Store) read(ctx context.Context, name, atespace, id string) (Record, bool, error) {
	list, err := s.maps.List(ctx, metav1.ListOptions{FieldSelector: "metadata.name=" + name})
	if err != nil {
		return Record{}, false, err
	}
	for i := range list.Items {
		cm := &list.Items[i]
		if cm.Name != name {
			continue
		}
		r, err := decode(cm, atespace, id)
		if err != nil {
			r = Record{Atespace: atespace, ID: id, Written: cm.CreationTimestamp.Time, objectUID: cm.UID, resourceVersion: cm.ResourceVersion}
		}
		return r, true, nil
	}
	return Record{}, false, nil
}

// Get returns the record of an environment from the informer, and false when
// it has none.
func (s *Store) Get(atespace, id string) (Record, bool, error) {
	obj, ok, err := s.informer.GetStore().GetByKey(s.namespace + "/" + ObjectName(atespace, id))
	if err != nil || !ok {
		return Record{}, false, err
	}
	r, err := decode(obj.(*corev1.ConfigMap), atespace, id)
	return r, err == nil, err
}

// Delete removes the object that Get returned as r, and only that one: the
// delete carries the identity of the informer's copy. An object that is gone,
// or that was written again since, is not an error and stays: it is then the
// object of a later environment with the same id.
func (s *Store) Delete(ctx context.Context, r Record) error {
	_, err := s.deleteSeen(ctx, ObjectName(r.Atespace, r.ID), r.objectUID, r.resourceVersion)
	return err
}

// deleteSeen deletes the object of that name when it still is the one with
// that uid and resource version, and reports whether it did.
func (s *Store) deleteSeen(ctx context.Context, name string, uid types.UID, resourceVersion string) (bool, error) {
	err := s.maps.Delete(ctx, name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &resourceVersion},
	})
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err), apierrors.IsConflict(err):
		// Gone, or written again since the informer's copy.
		return false, nil
	default:
		return false, fmt.Errorf("envstore: deleting the object %s/%s: %w", s.namespace, name, err)
	}
}

// ActorUIDs returns the uid of each actor of an atespace, by id.
type ActorUIDs func(ctx context.Context, atespace string) (map[string]string, error)

// SweepOrphans deletes each object whose actor is gone, or has another uid,
// once the object is older than grace. It reads the objects from the
// informer and the actors of each atespace that has such an object once. It
// returns the number of objects it deleted, and the errors joined: an
// atespace whose actors cannot be read is left for the next sweep.
//
// An object with the store's label that cannot be read as a record (no actor
// uid, labels that are not JSON) belongs to no environment. The sweep deletes
// it too after the grace period, and reports it.
//
// A delete carries the identity of the object as the informer saw it. Thus
// the sweep never removes an object that another replica wrote after that:
// Kubernetes refuses the delete, and the next sweep reads the new object.
//
// The grace period compares the create time that the Kubernetes API server
// gave the object with now, the clock of this process. It must be longer than
// the difference of the two clocks. The sweep leaves an object with no create
// time.
func (s *Store) SweepOrphans(ctx context.Context, actors ActorUIDs, grace time.Duration, now time.Time) (int, error) {
	old := map[string][]Record{}
	var errs []error
	deleted := 0
	for _, obj := range s.informer.GetStore().List() {
		cm := obj.(*corev1.ConfigMap)
		atespace := cm.Labels[LabelAtespace]
		if atespace == "" || cm.CreationTimestamp.IsZero() || now.Sub(cm.CreationTimestamp.Time) < grace {
			// Not an object of the store (the informer selects by the
			// label; this is the second guard before a delete), or young.
			// An object with no create time has no age: the zero time
			// would read as centuries, past each grace period. The API
			// server gives each object a create time.
			continue
		}
		r, err := decode(cm, atespace, "")
		if err != nil {
			// It names no environment, thus it goes by its own name. The
			// report says what happened to it, after the delete.
			done, delErr := s.deleteSeen(ctx, cm.Name, cm.UID, cm.ResourceVersion)
			switch {
			case delErr != nil:
				errs = append(errs, fmt.Errorf("%w; the sweep did not delete it: %w", err, delErr))
			case done:
				errs = append(errs, fmt.Errorf("%w; the sweep deleted it", err))
				deleted++
			default:
				errs = append(errs, fmt.Errorf("%w; it is gone or was written again since, and the sweep left it", err))
			}
			continue
		}
		old[atespace] = append(old[atespace], r)
	}
	for atespace, records := range old {
		uids, err := actors(ctx, atespace)
		if err != nil {
			errs = append(errs, fmt.Errorf("envstore: reading the actors of %q: %w", atespace, err))
			continue
		}
		for _, r := range records {
			if uids[r.ID] == r.UID {
				continue
			}
			done, err := s.deleteSeen(ctx, ObjectName(r.Atespace, r.ID), r.objectUID, r.resourceVersion)
			if err != nil {
				errs = append(errs, err)
			} else if done {
				deleted++
			}
		}
	}
	return deleted, errors.Join(errs...)
}

// SweepInterval is how often RunSweep calls its sweep, and OrphanGrace how old an
// object is before the sweep can call it an orphan: longer than the delay
// between a write of Substrate and a list that shows it, and than the
// difference between the clocks of the API server and of ate-env-api.
// SyncTimeout is how long a start waits for the first list of the objects.
const (
	SweepInterval = 30 * time.Second
	OrphanGrace   = time.Minute
	SyncTimeout   = 30 * time.Second
)

// RunSweep calls sweep on a timer until ctx ends, with ctx and the time of
// the tick. The sweep bounds its own parts; a sweep that takes longer than
// one interval skips the ticks it missed.
func RunSweep(ctx context.Context, sweep func(ctx context.Context, now time.Time)) {
	ticker := time.NewTicker(SweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			sweep(ctx, now)
		}
	}
}

// decode reads a record from an object. id is the id of the environment, or
// empty to take it from the name of the object.
func decode(cm *corev1.ConfigMap, atespace, id string) (Record, error) {
	if id == "" {
		prefix := ObjectName(atespace, "")
		if atespace == "" || len(cm.Name) <= len(prefix) || cm.Name[:len(prefix)] != prefix {
			return Record{}, fmt.Errorf("envstore: object %s/%s is not named for an environment of atespace %q", cm.Namespace, cm.Name, atespace)
		}
		id = cm.Name[len(prefix):]
	}
	r := Record{
		Atespace:        atespace,
		ID:              id,
		UID:             cm.Data[dataUID],
		Written:         cm.CreationTimestamp.Time,
		objectUID:       cm.UID,
		resourceVersion: cm.ResourceVersion,
	}
	if r.UID == "" {
		return Record{}, fmt.Errorf("envstore: object %s/%s has no actor uid", cm.Namespace, cm.Name)
	}
	if raw := cm.Data[dataLabels]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &r.Labels); err != nil {
			return Record{}, fmt.Errorf("envstore: labels of object %s/%s: %w", cm.Namespace, cm.Name, err)
		}
	}
	if raw := cm.Data[dataExpireTime]; raw != "" {
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return Record{}, fmt.Errorf("envstore: expire_time of object %s/%s: %w", cm.Namespace, cm.Name, err)
		}
		r.ExpireTime = t
		r.DeleteFailed = cm.Labels[LabelDeadline] == DeadlineFailed
		if raw := cm.Data[dataDeleteFailures]; raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 {
				return Record{}, fmt.Errorf("envstore: delete_failures of object %s/%s: %q is not a count", cm.Namespace, cm.Name, raw)
			}
			r.DeleteFailures = n
		}
		if raw := cm.Data[dataDeleteFailTime]; raw != "" {
			t, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				return Record{}, fmt.Errorf("envstore: delete_failure_time of object %s/%s: %w", cm.Namespace, cm.Name, err)
			}
			r.LastDeleteFailure = t
		}
	}
	return r, nil
}

func wrap(err error, doing, atespace, id string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("envstore: %s the object of %s/%s: %w", doing, atespace, id, err)
}
