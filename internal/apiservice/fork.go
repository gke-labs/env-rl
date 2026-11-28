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
	"strings"
	"sync"
	"time"

	"github.com/agent-substrate/env/internal/ate"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"github.com/google/uuid"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MaxForkCount is the most copies one ForkEnvironment call creates.
const MaxForkCount = 64

// maxEnvIDLen is the longest environment id: a DNS-1123 label.
const maxEnvIDLen = 63

// forkCleanupTimeout bounds the deletes that undo a failed fork. They run
// on a context of their own, because the caller's can be what ended the
// fork.
const forkCleanupTimeout = 30 * time.Second

// ForkEnvironment copies one environment count times. It suspends the
// source locally (Substrate's SuspendActor returns at once on a suspended or
// paused actor), tags its snapshot, and creates each copy from the tag under
// the source's template, in parallel. The copies are born suspended and wake
// on their first call. The tag stays: deleting it would make every copy that
// has not suspended since unrecoverable (Substrate's DeleteTag).
//
// All or nothing: when a copy fails, the copies created are deleted, then
// the tag, and the first error is returned.
//
// Errors: InvalidArgument for a missing id, a count outside 1 to
// MaxForkCount, ids that do not match count, are duplicated, or are set
// along with id_prefix, and an id longer than 63 characters. NotFound when
// the source does not exist. Other control plane failures (a source that
// cannot be suspended, such as a CRASHED one, or a copy whose id exists) are
// mapped by toGRPCError.
func (s *Server) ForkEnvironment(ctx context.Context, req *ateenvv1alpha.ForkEnvironmentRequest) (*ateenvv1alpha.ForkEnvironmentResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	atespace := req.GetAtespace()
	if atespace == "" {
		atespace = DefaultAtespace
	}
	setSpanEnvAttributes(ctx, atespace, req.GetId())
	ids, err := forkIDs(req)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	source, err := s.client.Get(ctx, atespace, req.GetId())
	if err != nil {
		return nil, toGRPCError(err)
	}
	setSpanActorAttributes(ctx, source)
	if err := s.client.SuspendLocal(ctx, atespace, req.GetId()); err != nil {
		return nil, toGRPCError(err)
	}
	if s.ReactiveWake && s.awakeEnvs != nil {
		s.awakeEnvs.Remove(atespace + "/" + req.GetId())
	}

	tagName := "fork-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	if _, err := s.client.CreateTag(ctx, atespace, req.GetId(), tagName); err != nil {
		if createOutcomeUnknown(err) {
			// The tag can have landed with its reply lost; no copy uses it.
			s.deleteForkTag(ctx, atespace, tagName)
		}
		return nil, toGRPCError(err)
	}

	selector := req.GetWorkerSelector()
	if len(selector) == 0 {
		selector = source.GetWorkerSelector().GetMatchLabels()
	}
	actors := make([]*ateapipb.Actor, len(ids))
	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Go(func() {
			actors[i], errs[i] = s.client.CreateActor(ctx, ate.CreateOptions{
				ID:             id,
				Template:       source.GetActorTemplate().GetName(),
				Atespace:       atespace,
				WorkerSelector: selector,
				SourceTag:      tagName,
			})
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			s.undoFork(ctx, atespace, tagName, ids, actors, errs)
			return nil, toGRPCError(err)
		}
	}

	resp := &ateenvv1alpha.ForkEnvironmentResponse{SourceTag: tagName}
	for _, actor := range actors {
		if s.ReactiveWake && s.awakeEnvs != nil {
			s.awakeEnvs.Remove(atespace + "/" + actor.GetMetadata().GetName())
		}
		resp.Environments = append(resp.Environments, ActorToEnvironment(actor))
	}
	return resp, nil
}

// undoFork deletes the copies a failed fork created, then its tag once no
// copy is left that it seeds. A copy whose create failed is deleted too when
// the failure leaves it unknown whether the create landed (its reply lost),
// and only when the actor of that id was seeded from this fork's tag: an
// environment that held the id before the fork stays. One refused outright,
// such as AlreadyExists, is not touched. Each delete names the uid of the
// copy. A delete that fails is logged and leaves the actor, and then the
// tag, in place.
func (s *Server) undoFork(ctx context.Context, atespace, tagName string, ids []string, actors []*ateapipb.Actor, errs []error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), forkCleanupTimeout)
	defer cancel()
	left := false
	for i, id := range ids {
		actor := actors[i]
		if actor == nil {
			if !createOutcomeUnknown(errs[i]) {
				continue
			}
			got, err := s.client.Get(ctx, atespace, id)
			if errors.Is(err, ate.ErrNotFound) {
				continue
			}
			if err != nil {
				log.Printf("apiservice: ForkEnvironment: reading copy %s/%s: %v", atespace, id, err)
				left = true
				continue
			}
			if got.GetSourceTag().GetName() != tagName {
				continue
			}
			actor = got
		}
		err := s.client.DeleteWithUID(ctx, atespace, id, actor.GetMetadata().GetUid())
		if err != nil && !errors.Is(err, ate.ErrNotFound) {
			log.Printf("apiservice: ForkEnvironment: deleting copy %s/%s: %v", atespace, id, err)
			left = true
		}
	}
	if left {
		log.Printf("apiservice: ForkEnvironment: keeping tag %s/%s, which a copy left in place uses", atespace, tagName)
		return
	}
	s.deleteForkTag(ctx, atespace, tagName)
}

// deleteForkTag deletes the tag of a failed fork on a context of its own,
// bounded by forkCleanupTimeout, and logs a failure other than NotFound.
func (s *Server) deleteForkTag(ctx context.Context, atespace, tagName string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), forkCleanupTimeout)
	defer cancel()
	if err := s.client.DeleteTag(ctx, atespace, tagName); err != nil && !errors.Is(err, ate.ErrNotFound) {
		log.Printf("apiservice: ForkEnvironment: deleting tag %s/%s: %v", atespace, tagName, err)
	}
}

// createOutcomeUnknown reports whether a CreateActor error leaves it unknown
// whether the actor was stored: the call was cut off rather than refused.
func createOutcomeUnknown(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.Unknown, codes.Internal, codes.Aborted:
		return true
	}
	return false
}

// forkIDs returns the ids of a fork's copies: req.ids, or "<prefix>-<i>"
// with req.id_prefix, else the source id cut to 50 characters and 6 random
// hex digits as the prefix.
func forkIDs(req *ateenvv1alpha.ForkEnvironmentRequest) ([]string, error) {
	count := int(req.GetCount())
	if count == 0 && len(req.GetIds()) > 0 {
		count = len(req.GetIds())
	}
	if count < 1 || count > MaxForkCount {
		return nil, fmt.Errorf("count %d is outside 1 to %d", req.GetCount(), MaxForkCount)
	}
	if len(req.GetIds()) > 0 {
		if req.GetIdPrefix() != "" {
			return nil, fmt.Errorf("ids and id_prefix are exclusive")
		}
		if len(req.GetIds()) != count {
			return nil, fmt.Errorf("%d ids for count %d", len(req.GetIds()), count)
		}
		seen := make(map[string]bool, count)
		for _, id := range req.GetIds() {
			if id == "" || len(id) > maxEnvIDLen {
				return nil, fmt.Errorf("id %q is empty or longer than %d characters", id, maxEnvIDLen)
			}
			if seen[id] {
				return nil, fmt.Errorf("id %q is repeated", id)
			}
			seen[id] = true
		}
		return req.GetIds(), nil
	}
	prefix := req.GetIdPrefix()
	if prefix == "" {
		base := req.GetId()
		if len(base) > 50 {
			base = strings.TrimRight(base[:50], "-")
		}
		prefix = base + "-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:6]
	}
	ids := make([]string, count)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s-%d", prefix, i)
		if len(ids[i]) > maxEnvIDLen {
			return nil, fmt.Errorf("id %q is longer than %d characters", ids[i], maxEnvIDLen)
		}
	}
	return ids, nil
}
