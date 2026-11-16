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

package swebench

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// parkedRun is a steps run of two environments and two steps on f, so each
// environment is parked once, after step 1. The first environment's suspend
// is refused refusals times with err. It returns that environment's id and
// the config; shape sets the park.
func parkedRun(t *testing.T, f *fakeBackend, out *bytes.Buffer, refusals int, err error, shape func(*Config)) (string, Config) {
	t.Helper()
	cfg := baseConfig(t, f, out)
	cfg.Mode, cfg.Park = ModeSteps, ParkSuspend
	cfg.Instances, cfg.Rollouts, cfg.Steps = instances(2), 1, 2
	cfg.WakeBudget = time.Minute
	shape(&cfg)
	id := EnvID(cfg.RunID, cfg.Instances[0].InstanceID, 0)
	f.failSuspend[id], f.suspendErr = refusals, err
	return id, cfg
}

// suspendEvents returns the suspend events of a run by environment.
func suspendEvents(t *testing.T, out *bytes.Buffer) map[string]Event {
	t.Helper()
	byEnv := map[string]Event{}
	for _, e := range parseEvents(t, out.String()) {
		if e.Event == EventSuspend {
			if _, twice := byEnv[e.Env]; twice {
				t.Fatalf("two suspend events for %s", e.Env)
			}
			byEnv[e.Env] = e
		}
	}
	return byEnv
}

// SL-1 and SL-4 (docs/plans/swebench-loader/acceptance-criteria.md): the
// park sends a suspend again after each transient error, the environment is
// parked once, and the event and the report say that it took three attempts.
func TestSuspendRetriesTransientErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		code     codes.Code
		park     Park
		parkEach bool
		async    bool
	}{
		{"Unavailable, suspend", codes.Unavailable, ParkSuspend, false, false},
		{"DeadlineExceeded, suspend-sync", codes.DeadlineExceeded, ParkSuspendSync, false, false},
		{"Unknown, suspend-async", codes.Unknown, ParkSuspendAsync, false, true},
		{"Internal, fork", codes.Internal, ParkFork, false, true},
		{"Aborted, suspend with park-each", codes.Aborted, ParkSuspend, true, false},
		{"Unavailable, suspend-async with park-each", codes.Unavailable, ParkSuspendAsync, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			var out bytes.Buffer
			id, cfg := parkedRun(t, f, &out, 2, status.Error(tc.code, "refused"), func(c *Config) {
				c.Park, c.ParkEach = tc.park, tc.parkEach
			})
			sum, err := Run(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if sum.Tasks != 4 || sum.Failed != 0 {
				t.Fatalf("summary %+v, want 4 tasks and no failure", sum)
			}
			wantSync, wantAsync := 1, 0
			if tc.async {
				wantSync, wantAsync = 0, 1
			}
			if wantModes := []bool{tc.async, tc.async, tc.async}; f.suspendCalls[id] != 3 || !slices.Equal(f.suspendModes[id], wantModes) || f.suspended[id] != wantSync || f.suspendedAsync[id] != wantAsync {
				t.Errorf("suspend of %s: calls=%d modes=%v sync=%d async=%d; want 3, %v, %d, %d", id, f.suspendCalls[id], f.suspendModes[id], f.suspended[id], f.suspendedAsync[id], wantModes, wantSync, wantAsync)
			}
			events := suspendEvents(t, &out)
			if len(events) != 2 {
				t.Fatalf("suspend events for %d environments, want 2", len(events))
			}
			for env, e := range events {
				want := 1
				if env == id {
					want = 3
				}
				if !e.OK || e.Attempts != want {
					t.Errorf("suspend of %s: ok %v after %d attempts (%s), want ok after %d", env, e.OK, e.Attempts, e.Err, want)
				}
			}
			rep := Build(parseEvents(t, out.String()))
			if s := stat(rep, EventSuspend); s.Count != 2 || s.Failed != 0 || s.Retried != 1 {
				t.Errorf("report suspend row: count %d failed %d retried %d, want 2, 0, 1", s.Count, s.Failed, s.Retried)
			}
			if fc := rep.FaultClassification; fc == nil || fc.RecoveredRetries != 1 || len(fc.Categories) != 1 || fc.Categories[0].Category != schema.FaultCatSnapshotRestoreFailure || fc.Categories[0].Recovered != 1 {
				t.Errorf("fault classification %+v, want 1 recovered retry under snapshot_restore_failure", fc)
			}
		})
	}

	// A suspend event without an environment or instance key also counts in the
	// fault table when it succeeded after more than one attempt.
	rep := Build([]Event{
		{RunID: "r", Event: EventTask, Instance: "i", Task: 1, Step: 1, OK: true},
		{RunID: "r", Event: EventSuspend, Step: 1, OK: true, Attempts: 2},
	})
	if fc := rep.FaultClassification; fc == nil || fc.RecoveredRetries != 1 || len(fc.Categories) != 1 || fc.Categories[0].Category != schema.FaultCatSnapshotRestoreFailure || fc.Categories[0].Recovered != 1 {
		t.Errorf("unkeyed retried suspend fault classification %+v, want 1 recovered retry under snapshot_restore_failure", fc)
	}
}

// SL-3: an error that is not transient ends the suspend at once.
func TestSuspendDoesNotRetryOtherErrors(t *testing.T) {
	for _, refused := range []error{
		status.Error(codes.NotFound, "no such environment"),
		status.Error(codes.FailedPrecondition, "refused"),
		status.Error(codes.InvalidArgument, "refused"),
		status.Error(codes.ResourceExhausted, "refused"),
		status.Error(codes.PermissionDenied, "refused"),
		errors.New("agent-sandbox POST /suspend: status 503"),
	} {
		f := newFake()
		var out bytes.Buffer
		id, cfg := parkedRun(t, f, &out, 2, refused, func(*Config) {})
		if _, err := Run(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		if e := suspendEvents(t, &out)[id]; e.OK || e.Attempts != 1 || f.suspendCalls[id] != 1 || !strings.Contains(e.Err, refused.Error()) {
			t.Errorf("%v: suspend ok %v after %d attempts and %d calls (%s); want one failed attempt", refused, e.OK, e.Attempts, f.suspendCalls[id], e.Err)
		}
	}
}

// SL-2 and SL-4: with the default budget of 3 minutes and pauses of 2, 4, 8
// and then 10 seconds, a suspend that keeps failing is sent 20 times: the
// 19 pauses take 174 seconds, and one more would end after the budget. A
// cancelled run ends the retries during the pause.
func TestSuspendRetriesStopAtTheWakeBudget(t *testing.T) {
	always := func(c *Config) { c.WakeBudget, c.WakeBackoff = 3*time.Minute, DefaultWakeBackoff }
	unavailable := status.Error(codes.Unavailable, "while calling atelet Checkpoint: handshake timed out")

	t.Run("budget", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newFake()
			var out bytes.Buffer
			id, cfg := parkedRun(t, f, &out, 1000, unavailable, always)
			if _, err := Run(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			e := suspendEvents(t, &out)[id]
			if e.OK || e.Attempts != 20 || f.suspendCalls[id] != 20 || !strings.Contains(e.Err, "handshake timed out") {
				t.Errorf("suspend ok %v after %d attempts and %d calls (%s); want the 20th attempt's failure", e.OK, e.Attempts, f.suspendCalls[id], e.Err)
			}
			if e.DurMS != 174_000 {
				t.Errorf("suspend dur_ms = %v, want 174000: the event covers the pauses", e.DurMS)
			}
		})
	})

	t.Run("cancel", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newFake()
			var out bytes.Buffer
			id, cfg := parkedRun(t, f, &out, 1000, unavailable, always)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error)
			go func() {
				_, err := Run(ctx, cfg)
				done <- err
			}()
			// After one second step 1 is done, the first attempt has failed
			// and the run is in its pause of two.
			time.Sleep(time.Second)
			synctest.Wait()
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("Run = %v, want the cancellation", err)
			}
			if e := suspendEvents(t, &out)[id]; e.OK || e.Attempts != 1 || f.suspendCalls[id] != 1 || e.DurMS >= 2000 || !strings.Contains(e.Err, "handshake timed out") {
				t.Errorf("suspend ok %v after %d attempts, %d calls and %v ms (%s); want the first attempt's error, ended inside the pause", e.OK, e.Attempts, f.suspendCalls[id], e.DurMS, e.Err)
			}
		})
	})
}

// SL-1 on the EnvBackend: an Unavailable from ate-env-api reaches the park
// as a transient error through the Go client.
func TestSuspendRetriesOnTheEnvBackend(t *testing.T) {
	tr, err := ParseTrajectory([]byte("name: t\nsteps:\n  - {name: a, run: \"true\", reward: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{suspendUnavailable: 2}
	var out bytes.Buffer
	cfg := baseConfig(t, newFakeBackend(t, api), &out)
	cfg.Trajectory = tr
	cfg.Mode, cfg.Park = ModeSteps, ParkSuspend
	cfg.Instances, cfg.Rollouts, cfg.Steps = instances(1), 1, 2
	sum, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Tasks != 2 || sum.Failed != 0 {
		t.Fatalf("summary %+v, want 2 tasks and no failure", sum)
	}
	if len(api.suspendModes) != 3 || len(api.suspended) != 1 {
		t.Errorf("%d SuspendEnvironment calls, %d served; want 3 and 1", len(api.suspendModes), len(api.suspended))
	}
	if e := suspendEvents(t, &out)[EnvID(cfg.RunID, cfg.Instances[0].InstanceID, 0)]; !e.OK || e.Attempts != 3 {
		t.Errorf("suspend ok %v after %d attempts (%s), want ok after 3", e.OK, e.Attempts, e.Err)
	}
}
