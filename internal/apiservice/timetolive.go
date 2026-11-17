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

package apiservice

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/envstore"
	"github.com/agent-substrate/env/internal/telemetry"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The time to live of an environment: a deadline in the object of the
// environment (internal/envstore), and a sweep that deletes the environment
// after it. docs/plans/ate-env-api/environment-metadata.md is the design.
const (
	// deadlineDeleteTries is how many deletes by deadline Substrate can
	// refuse before the sweep stops for that environment (decision EM-I).
	deadlineDeleteTries = 5
	// deadlineTryInterval is the least time between two counted tries of
	// one environment, whatever the number of replicas: without it, N
	// replicas would use the tries in 1/N of the time, and a refusal that
	// lasts a moment would mark the environment.
	deadlineTryInterval = envstore.SweepInterval / 2
	// deadlineSweepWorkers is how many environments one sweep deletes at a
	// time. A fleet whose environments share a time to live reaches its
	// deadlines together, and one delete can take seconds.
	deadlineSweepWorkers = 16
	// storeWriteTimeout bounds a write to the store, or a rollback, that
	// must go on after the context of its caller ended.
	storeWriteTimeout = 10 * time.Second
	// storeWriteTries bounds how often a write starts again after another
	// writer changed the object.
	storeWriteTries = 3
)

// errStoreOff is the answer to a request that needs the environment store on
// a server that runs without it. The request changes nothing.
func errStoreOff(what string) error {
	return status.Errorf(codes.FailedPrecondition,
		"%s needs the environment store, and this server runs without it (ate-env-api --environment-store=configmaps; GetServerInfo lists %q when it is on)", what, FeatureTimeToLive)
}

// timeToLive reads a time_to_live field. Unset is zero.
func timeToLive(d *durationpb.Duration) (time.Duration, error) {
	if d == nil {
		return 0, nil
	}
	if err := d.CheckValid(); err != nil {
		return 0, status.Errorf(codes.InvalidArgument, "time_to_live: %v", err)
	}
	ttl := d.AsDuration()
	if ttl < 0 {
		return 0, status.Errorf(codes.InvalidArgument, "time_to_live %s is negative", ttl)
	}
	return ttl, nil
}

// errLateWrite is the error of writeObject when the actor of the write is no
// longer the one that exists: the environment was deleted, or deleted and
// created again, while the write waited.
var errLateWrite = errors.New("the environment was deleted before its object was written")

// writeObject writes the object of an environment whose informer copy shows
// none, for the actor with the uid of r. It follows EM-14: Put never
// deletes, and for a name that is taken the object decides. An object of the
// same actor is an earlier write of this environment, and gets the deadline
// of r. For an object of another actor, writeObject reads the actor again:
// when the actor of r still exists, the object is of an earlier environment
// with the same id, and Replace writes over it; else the write is late, and
// writeObject drops it with errLateWrite.
func (s *Server) writeObject(ctx context.Context, r envstore.Record) error {
	var err error
	for range storeWriteTries {
		err = s.Store.Put(ctx, r)
		var exists *envstore.ExistsError
		if !errors.As(err, &exists) {
			return err
		}
		if exists.Existing.UID == r.UID {
			next := exists.Existing
			next.ExpireTime, next.DeleteFailures, next.DeleteFailed, next.LastDeleteFailure = r.ExpireTime, 0, false, time.Time{}
			err = s.Store.Update(ctx, next, exists.Existing)
		} else {
			actor, getErr := s.client.Get(ctx, r.Atespace, r.ID)
			if errors.Is(getErr, ate.ErrNotFound) || (getErr == nil && actor.GetMetadata().GetUid() != r.UID) {
				return errLateWrite
			}
			if getErr != nil {
				return fmt.Errorf("reading the actor again before a write over the object of another: %w", getErr)
			}
			err = s.Store.Replace(ctx, r, exists.Existing)
		}
		// The object changed between the read and the write, or another
		// write came between the delete and the create of Replace: start
		// again from what the API server has now.
		if !errors.Is(err, envstore.ErrObjectChanged) && !errors.As(err, &exists) {
			return err
		}
	}
	return err
}

// writeCreateDeadline is the second write of a create with a time to live
// (EM-E): the actor exists, and its object gets the deadline, the create
// time of the actor plus ttl. When the object is not written, it deletes the
// actor with its uid as the precondition, thus only the actor of this
// create, and fails the create: no environment stays without the deadline
// its caller asked for.
func (s *Server) writeCreateDeadline(ctx context.Context, atespace string, actor *ateapipb.Actor, ttl time.Duration) (time.Time, error) {
	id, uid := actor.GetMetadata().GetName(), actor.GetMetadata().GetUid()
	expire := actor.GetMetadata().GetCreateTime().AsTime().Add(ttl)
	var err error
	if actor.GetMetadata().GetCreateTime() == nil || uid == "" {
		err = errors.New("the control plane returned the actor with no uid or no create time")
	} else {
		err = s.writeObject(ctx, envstore.Record{Atespace: atespace, ID: id, UID: uid, ExpireTime: expire})
	}
	if err == nil {
		return expire, nil
	}
	if uid == "" {
		// Without the uid, a delete would have no precondition, and could
		// remove a later environment with the same id.
		log.Printf("apiservice: CreateEnvironment %s/%s: %v; the environment stays with no deadline", atespace, id, err)
		return time.Time{}, status.Errorf(codes.Internal, "the deadline of environment %q was not written (%v); the environment exists with no deadline: delete it", id, err)
	}
	// The context of the caller can be the reason the write failed.
	rollback, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeWriteTimeout)
	defer cancel()
	delErr := s.client.DeleteWithUID(rollback, atespace, id, uid)
	// An actor that is gone, or that is another one by now (Aborted), is not
	// the environment of this create: nothing is left to roll back.
	if delErr != nil && !errors.Is(delErr, ate.ErrNotFound) && status.Code(delErr) != codes.Aborted {
		log.Printf("apiservice: CreateEnvironment %s/%s: the object was not written (%v) and the delete of the actor failed (%v); the environment stays with no deadline", atespace, id, err, delErr)
		return time.Time{}, status.Errorf(codes.Internal, "the deadline of environment %q was not written (%v), and its delete failed (%v); the environment exists with no deadline: delete it", id, err, delErr)
	}
	return time.Time{}, status.Errorf(codes.Unavailable, "the deadline of environment %q was not written, and the environment was deleted: %v", id, err)
}

// withDeadline sets env.expire_time from the informer's copy of the object
// of the environment, when that object is of the actor with this uid (EM-D:
// a read makes no call to Kubernetes; EM-8: the object of an earlier
// environment with the same id gives nothing). An object that cannot be read
// gives nothing; the orphan sweep reports it.
func (s *Server) withDeadline(env *ateenvv1alpha.Environment, uid string) *ateenvv1alpha.Environment {
	if s.Store == nil || env == nil {
		return env
	}
	r, ok, err := s.Store.Get(env.GetAtespace(), env.GetId())
	if err != nil || !ok || r.UID != uid || r.ExpireTime.IsZero() {
		return env
	}
	env.ExpireTime = timestamppb.New(r.ExpireTime)
	return env
}

// SetEnvironmentTimeToLive gives an environment a new deadline, the time of
// the call plus time_to_live, or removes its deadline with zero. It clears
// the count and the mark of the deletes by deadline that failed, thus the
// sweep tries again after the new deadline.
//
// The response is the environment, as GetEnvironment gives it, with the
// deadline the call wrote: the handler reads the actor for NotFound and for
// its uid, thus the environment costs no call more, and the caller needs no
// GetEnvironment, whose informer copy can be behind this write (EM-D).
//
// Errors: InvalidArgument for a missing id or a negative time_to_live.
// FailedPrecondition when the server runs without the store. NotFound when
// the environment does not exist. Aborted when other writers changed the
// object on each try.
func (s *Server) SetEnvironmentTimeToLive(ctx context.Context, req *ateenvv1alpha.SetEnvironmentTimeToLiveRequest) (*ateenvv1alpha.SetEnvironmentTimeToLiveResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	ttl, err := timeToLive(req.GetTimeToLive())
	if err != nil {
		return nil, err
	}
	if s.Store == nil {
		return nil, errStoreOff("SetEnvironmentTimeToLive")
	}
	atespace := req.GetAtespace()
	if atespace == "" {
		atespace = DefaultAtespace
	}
	var expire time.Time
	if ttl > 0 {
		expire = time.Now().Add(ttl)
	}

	for try := range storeWriteTries {
		actor, err := s.client.Get(ctx, atespace, req.GetId())
		if err != nil {
			return nil, toGRPCError(err)
		}
		// The first try trusts the informer only for an object of this actor
		// that the call then updates: the update names the copy it read, and
		// the API server refuses it when the informer was behind. A decision
		// that writes nothing has no such check, thus each other case (no
		// object, the object of another actor, no deadline to remove), and
		// each try after a refusal, reads the object from the API server.
		existing, found, err := s.Store.Get(atespace, req.GetId())
		trusted := err == nil && found && existing.UID == actor.GetMetadata().GetUid() && !(expire.IsZero() && existing.ExpireTime.IsZero())
		if try > 0 || !trusted {
			if existing, found, err = s.Store.Read(ctx, atespace, req.GetId()); err != nil {
				return nil, status.Errorf(codes.Unavailable, "%v", err)
			}
		}
		err = s.setDeadline(ctx, actor, existing, found, expire)
		switch {
		case err == nil:
			env := ActorToEnvironment(actor)
			if !expire.IsZero() {
				env.ExpireTime = timestamppb.New(expire)
			}
			return &ateenvv1alpha.SetEnvironmentTimeToLiveResponse{Environment: env}, nil
		case errors.Is(err, errLateWrite):
			return nil, status.Errorf(codes.NotFound, "environment %q: %v", req.GetId(), err)
		case !errors.Is(err, envstore.ErrObjectChanged):
			return nil, status.Errorf(codes.Unavailable, "the deadline of environment %q was not written: %v", req.GetId(), err)
		}
	}
	return nil, status.Errorf(codes.Aborted, "the object of environment %q changed on each of %d tries; try again", req.GetId(), storeWriteTries)
}

// setDeadline writes expire, or no deadline when it is zero, to the object
// of the actor's environment. existing is the object the caller read.
func (s *Server) setDeadline(ctx context.Context, actor *ateapipb.Actor, existing envstore.Record, found bool, expire time.Time) error {
	md := actor.GetMetadata()
	switch {
	case found && existing.UID == md.GetUid():
		if expire.IsZero() && existing.ExpireTime.IsZero() {
			return nil
		}
		next := existing
		next.ExpireTime, next.DeleteFailures, next.DeleteFailed, next.LastDeleteFailure = expire, 0, false, time.Time{}
		return s.Store.Update(ctx, next, existing)
	case expire.IsZero():
		// The environment has no object of its own, thus no deadline. An
		// object of an earlier environment with this id is an orphan.
		return nil
	default:
		return s.writeObject(ctx, envstore.Record{Atespace: md.GetAtespace(), ID: md.GetName(), UID: md.GetUid(), ExpireTime: expire})
	}
}

// deleteEnvironment is the delete of DeleteEnvironment and of the deadline
// sweep: the actor, then its object. uid, when not empty, is the
// precondition of the actor's delete: the sweep sends the uid of the object
// it read, and a caller's delete sends none. record is the object of the
// environment as the caller read it, when hasRecord is set.
func (s *Server) deleteEnvironment(ctx context.Context, atespace, id, uid string, record envstore.Record, hasRecord bool) error {
	if err := s.client.DeleteWithUID(ctx, atespace, id, uid); err != nil {
		return err
	}
	s.finishCreate(atespace, id, telemetry.ReadyStatusDeleted)
	if hasRecord {
		s.deleteObject(ctx, record)
	}
	return nil
}

// deleteObject removes the object of an environment whose actor is deleted.
// The actor is the truth, thus a failure is logged and not returned: an
// object that stays is an orphan, and the sweep removes it. The delete
// removes only the object the caller read; one written since is of a new
// environment with this id. The context of the caller can have ended with
// the actor's delete, thus this call has a time of its own.
func (s *Server) deleteObject(ctx context.Context, record envstore.Record) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeWriteTimeout)
	defer cancel()
	if err := s.Store.Delete(ctx, record); err != nil {
		log.Printf("apiservice: the delete of %s/%s: %v; the orphan sweep removes the object", record.Atespace, record.ID, err)
	}
}

// Sweep is one sweep of the store, what envstore.RunSweep calls on its
// timer: the environments past their deadline first, then the orphan
// objects, which include what a delete by deadline left behind. The two
// share the timer and nothing else: the deadline sweep reads no actor (the
// delete itself tells), and the orphan sweep lists the actors of each
// atespace that has an old object. Each has one interval of its own: a
// backlog of deadlines, or a control plane that does not answer, must not
// take the time of the orphan sweep, or stop the sweeps after it.
func (s *Server) Sweep(ctx context.Context, now time.Time) {
	if s.Store == nil {
		return
	}
	deadlines, cancel := context.WithTimeout(ctx, envstore.SweepInterval)
	deleted, err := s.SweepDeadlines(deadlines, now)
	cancel()
	if deleted > 0 || err != nil {
		log.Printf("environment store: the deadline sweep deleted %d environments, error: %v", deleted, err)
	}
	orphans, cancel := context.WithTimeout(ctx, envstore.SweepInterval)
	deleted, err = s.Store.SweepOrphans(orphans, s.ActorUIDs, envstore.OrphanGrace, now)
	cancel()
	if deleted > 0 || err != nil {
		log.Printf("environment store: the orphan sweep deleted %d objects, error: %v", deleted, err)
	}
}

// SweepDeadlines deletes each environment whose deadline is at or before
// now, in any status, and returns how many it deleted and the errors,
// joined. Without the store it does nothing (EM-12).
//
// It takes the expired objects from the informer, and for each one reads the
// object from the Kubernetes API server and goes on only when that copy is
// past its deadline too (EM-F): the informer can be behind a
// SetEnvironmentTimeToLive on another replica. The delete is the one of
// DeleteEnvironment, with the uid of the object as the precondition
// (EM-13): an environment that was created again with the same id stays.
//
// Each replica sweeps. An environment that another replica deleted is not an
// error (EM-11). A delete that Substrate refuses (FailedPrecondition) is
// counted in the object; after deadlineDeleteTries the sweep marks the
// object, reports the environment once, in a log line and in
// ate_env_api_deadline_delete_failed_total, and leaves it (EM-15). Each
// other failure is tried again on each sweep, without a count.
//
// One sweep deletes deadlineSweepWorkers environments at a time until ctx
// ends; what is left waits for the next sweep.
func (s *Server) SweepDeadlines(ctx context.Context, now time.Time) (int, error) {
	if s.Store == nil {
		return 0, nil
	}
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		deleted int
		errs    []error
		slots   = make(chan struct{}, deadlineSweepWorkers)
	)
	for _, r := range s.Store.Expired(now) {
		if ctx.Err() != nil {
			// The rest waits for the next sweep.
			mu.Lock()
			errs = append(errs, fmt.Errorf("the sweep ran out of time; the environments it did not reach wait for the next one: %w", ctx.Err()))
			mu.Unlock()
			break
		}
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-slots; wg.Done() }()
			done, err := s.deleteExpired(ctx, r, now)
			mu.Lock()
			defer mu.Unlock()
			if done {
				deleted++
			}
			if err != nil {
				errs = append(errs, err)
			}
		}()
	}
	wg.Wait()
	return deleted, errors.Join(errs...)
}

// deleteExpired deletes the environment of one object that the informer
// shows past its deadline, and reports whether it did.
func (s *Server) deleteExpired(ctx context.Context, seen envstore.Record, now time.Time) (bool, error) {
	// EM-F: the copy of the API server decides, not the informer's.
	r, found, err := s.Store.Read(ctx, seen.Atespace, seen.ID)
	if err != nil {
		return false, err
	}
	if !found || r.UID == "" || r.ExpireTime.IsZero() || r.ExpireTime.After(now) || r.DeleteFailed {
		// Gone, not a record, with no deadline or a later one by now, or
		// marked by another replica.
		return false, nil
	}

	err = s.deleteEnvironment(ctx, r.Atespace, r.ID, r.UID, r, true)
	switch code := status.Code(err); {
	case err == nil:
		return true, nil
	case errors.Is(err, ate.ErrNotFound):
		// Another replica or a caller deleted the environment. The object
		// has no actor: remove it now and do not wait for the orphan sweep.
		s.deleteObject(ctx, r)
		return false, nil
	case code == codes.Aborted:
		// Substrate's answer to the uid precondition: the actor is a new
		// environment with this id, it stays, and the object is an orphan
		// for the orphan sweep. Aborted is also Substrate's answer to a
		// write that raced another one; the next sweep tries that again.
		return false, nil
	case ctx.Err() != nil, code != codes.FailedPrecondition:
		// Not a refusal of this delete: a sweep that ran out of time, a
		// control plane that is down, busy or cannot reach its database
		// (Substrate answers a failure of its own with Internal), or a
		// credential that it does not take. These do not count, and the
		// next sweep tries again, without limit: an outage must not mark
		// each environment that reached its deadline in it.
		return false, fmt.Errorf("the delete by deadline of %s/%s failed, and the next sweep tries again: %w", r.Atespace, r.ID, err)
	case now.Sub(r.LastDeleteFailure) < deadlineTryInterval:
		// Refused, and another replica counted a refusal a moment ago: the
		// two are one try.
		return false, nil
	}

	// Substrate refused the delete: FailedPrecondition, its answer for an
	// actor in a state that it does not delete. Count the try in the
	// object, thus the replicas share the count. An update that another
	// writer wins (the other replica counted its own try, or a caller moved
	// the deadline) leaves the count to that writer.
	next := r
	next.DeleteFailures++
	next.LastDeleteFailure = now
	next.DeleteFailed = next.DeleteFailures >= deadlineDeleteTries
	count, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeWriteTimeout)
	defer cancel()
	if countErr := s.Store.Update(count, next, r); countErr != nil {
		if errors.Is(countErr, envstore.ErrObjectChanged) {
			return false, nil
		}
		return false, fmt.Errorf("the delete by deadline of %s/%s failed (%w), and the try was not counted: %w", r.Atespace, r.ID, err, countErr)
	}
	if next.DeleteFailed {
		// Once: only the replica whose update wrote the mark is here.
		log.Printf("environment store: the deadline sweep stops for %s/%s (actor %s, deadline %s) after %d deletes that Substrate refused, the last with: %v. The environment is not deleted and its object stays, with the label ate-env.dev/deadline=failed. A DeleteEnvironment that succeeds, or a SetEnvironmentTimeToLive, clears the mark",
			r.Atespace, r.ID, r.UID, r.ExpireTime.Format(time.RFC3339), next.DeleteFailures, err)
		s.Metrics.CountDeadlineDeleteFailed(r.Atespace)
		return false, nil
	}
	return false, fmt.Errorf("Substrate refused the delete by deadline of %s/%s, try %d of %d: %w", r.Atespace, r.ID, next.DeleteFailures, deadlineDeleteTries, err)
}
