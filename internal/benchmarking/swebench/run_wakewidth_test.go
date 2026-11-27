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
	"io/fs"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
)

type wakeLimitBarrier struct {
	mu      sync.Mutex
	n       int
	waiting int
	ch      chan struct{}
}

func newWakeLimitBarrier(n int) *wakeLimitBarrier {
	return &wakeLimitBarrier{n: n, ch: make(chan struct{})}
}

func (b *wakeLimitBarrier) wait(ctx context.Context) error {
	b.mu.Lock()
	b.waiting++
	ch := b.ch
	if b.waiting == b.n {
		b.waiting = 0
		b.ch = make(chan struct{})
		close(ch)
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ch:
		return nil
	}
}

type wakeLimitBackend struct {
	coldInflight    atomic.Int32
	maxColdInflight atomic.Int32
	wakeInflight    atomic.Int32
	maxWakeInflight atomic.Int32
	wokenAwake      atomic.Int32
	maxWokenAwake   atomic.Int32

	coldBarrier *wakeLimitBarrier
	wakeBarrier *wakeLimitBarrier
	workBarrier *wakeLimitBarrier
	wakeDelay   time.Duration
}

func recordMax(cur, maxVal *atomic.Int32, delta int32) {
	n := cur.Add(delta)
	for {
		m := maxVal.Load()
		if n <= m || maxVal.CompareAndSwap(m, n) {
			return
		}
	}
}

func (b *wakeLimitBackend) Create(_ context.Context, id, _ string) (Sandbox, error) {
	return &wakeLimitSandbox{b: b, id: id, fresh: true}, nil
}

func (b *wakeLimitBackend) Delete(_ context.Context, _ string) error { return nil }

type wakeLimitSandbox struct {
	b      *wakeLimitBackend
	id     string
	mu     sync.Mutex
	fresh  bool
	parked bool
	woken  bool
}

func (s *wakeLimitSandbox) Run(ctx context.Context, command string, _ time.Duration) (Result, error) {
	s.mu.Lock()
	wasFresh := s.fresh
	wasParked := s.parked
	s.fresh = false
	s.parked = false
	if wasParked && !s.woken {
		s.woken = true
		recordMax(&s.b.wokenAwake, &s.b.maxWokenAwake, 1)
	}
	s.mu.Unlock()

	if wasFresh {
		recordMax(&s.b.coldInflight, &s.b.maxColdInflight, 1)
		defer s.b.coldInflight.Add(-1)
		if s.b.coldBarrier != nil {
			if err := s.b.coldBarrier.wait(ctx); err != nil {
				return Result{}, err
			}
		}
	}
	if wasParked {
		recordMax(&s.b.wakeInflight, &s.b.maxWakeInflight, 1)
		defer s.b.wakeInflight.Add(-1)
		if s.b.wakeDelay > 0 {
			select {
			case <-ctx.Done():
				return Result{}, ctx.Err()
			case <-time.After(s.b.wakeDelay):
			}
		}
		if s.b.wakeBarrier != nil {
			if err := s.b.wakeBarrier.wait(ctx); err != nil {
				return Result{}, err
			}
		}
	}
	if command == "work" && s.b.workBarrier != nil {
		if err := s.b.workBarrier.wait(ctx); err != nil {
			return Result{}, err
		}
	}
	return Result{ExitCode: 0}, nil
}

func (s *wakeLimitSandbox) WriteFile(context.Context, string, []byte, fs.FileMode) error {
	return nil
}

func (s *wakeLimitSandbox) Suspend(context.Context) error {
	s.mu.Lock()
	if !s.parked {
		s.parked = true
		if s.woken {
			s.woken = false
			s.b.wokenAwake.Add(-1)
		}
	}
	s.mu.Unlock()
	return nil
}

func (s *wakeLimitSandbox) Delete(context.Context) error {
	s.mu.Lock()
	if !s.parked {
		s.parked = true
		if s.woken {
			s.woken = false
			s.b.wokenAwake.Add(-1)
		}
	}
	s.mu.Unlock()
	return nil
}

// SL-13 and SL-15 (docs/plans/swebench-loader/acceptance-criteria.md):
// -wake-width caps concurrent first-command wakes of a suspended fleet,
// stamps wake duration after acquiring the slot, and releases the slot before
// Step.Think so all -width environments overlap during think and later work.
func TestSL13WakeWidthCapsWakesBeforeThink(t *testing.T) {
	traj, err := ParseTrajectory([]byte(`
name: wake-then-think
steps:
  - name: probe
    run: "true"
  - name: work
    think: 1s
    run: "work"
    reward: true
`))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		park     Park
		parkEach bool
	}{
		{name: "park-each suspend", park: ParkSuspend, parkEach: true},
		{name: "barrier suspend", park: ParkSuspend, parkEach: false},
		{name: "park-each suspend-sync", park: ParkSuspendSync, parkEach: true},
		{name: "park-each suspend-async", park: ParkSuspendAsync, parkEach: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b := &wakeLimitBackend{
					wakeBarrier: newWakeLimitBarrier(2),
					workBarrier: newWakeLimitBarrier(6),
					wakeDelay:   100 * time.Millisecond,
				}

				var out bytes.Buffer
				cfg := baseConfig(t, b, &out)
				cfg.Trajectory = traj
				cfg.Mode = ModeSteps
				cfg.Park = tc.park
				cfg.ParkEach = tc.parkEach
				cfg.Instances = instances(6)
				cfg.Rollouts = 1
				cfg.Steps = 2
				cfg.Width = 6
				cfg.WakeWidth = 2

				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				sum, err := Run(ctx, cfg)
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				if sum.Tasks != 12 || sum.Failed != 0 {
					t.Fatalf("summary = %+v, want 12 tasks and 0 failures", sum)
				}
				if got := b.maxWakeInflight.Load(); got != 2 {
					t.Errorf("maxWakeInflight = %d, want 2", got)
				}
				if got := b.maxWokenAwake.Load(); got != 6 {
					t.Errorf("maxWokenAwake = %d, want 6", got)
				}
				wakes := 0
				var step2BarrierMS float64
				for _, ev := range parseEvents(t, out.String()) {
					if ev.Event == EventRun && ev.Labels["wake_width"] != "2" {
						t.Errorf("run labels = %v, want wake_width=2", ev.Labels)
					}
					if ev.Event == EventWake {
						wakes++
						// Three batches of 2 wake at 100ms each. Stamping wake
						// after acquiring wakeSem records 100ms for every wake
						// rather than 200ms or 300ms on queued batches.
						if ev.DurMS != 100 {
							t.Errorf("wake event DurMS = %.1fms, want 100ms", ev.DurMS)
						}
					}
					if ev.Event == EventBarrier && ev.Step == 2 {
						step2BarrierMS = ev.DurMS
					}
				}
				if wakes != 6 {
					t.Errorf("wake events = %d, want 6", wakes)
				}
				// Releasing wakeSem before Step.Think overlaps the three 1s
				// think windows so Step 2 finishes in 3*100ms + 1000ms = 1300ms
				// instead of 3*(100ms + 1000ms) = 3300ms.
				if step2BarrierMS != 1300 {
					t.Errorf("step 2 barrier DurMS = %.1fms, want 1300ms (overlapped think)", step2BarrierMS)
				}
			})
		})
	}
}

// SL-13: fresh starts on step 1 run at -width, and -wake-width 0 wakes at -width.
func TestSL13WakeWidthLeavesColdStartsAtWidth(t *testing.T) {
	traj, err := ParseTrajectory([]byte(`
name: cold-at-width
steps:
  - name: probe
    run: "true"
    reward: true
`))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name         string
		wakeWidth    int
		wantMaxWakes int32
	}{
		{name: "capped wake width", wakeWidth: 2, wantMaxWakes: 2},
		{name: "zero default uses width", wakeWidth: 0, wantMaxWakes: 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &wakeLimitBackend{
				coldBarrier: newWakeLimitBarrier(6),
				wakeBarrier: newWakeLimitBarrier(int(tc.wantMaxWakes)),
			}

			cfg := baseConfig(t, b, nil)
			cfg.Trajectory = traj
			cfg.Mode, cfg.Park, cfg.ParkEach = ModeSteps, ParkSuspend, true
			cfg.Instances, cfg.Rollouts, cfg.Steps = instances(6), 1, 2
			cfg.Width, cfg.WakeWidth = 6, tc.wakeWidth

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			sum, err := Run(ctx, cfg)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if sum.Tasks != 12 || sum.Failed != 0 {
				t.Fatalf("summary = %+v, want 12 tasks and 0 failures", sum)
			}
			if got := b.maxColdInflight.Load(); got != 6 {
				t.Errorf("step 1 cold inflight = %d, want 6", got)
			}
			if got := b.maxWakeInflight.Load(); got != tc.wantMaxWakes {
				t.Errorf("step 2 wake inflight = %d, want %d", got, tc.wantMaxWakes)
			}
		})
	}
}

// SL-14: Config.validate refuses invalid -wake-width combinations.
func TestSL14ValidateWakeWidth(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:    "negative",
			mutate:  func(c *Config) { c.WakeWidth = -1 },
			wantErr: ">= 0",
		},
		{
			name:    "above width",
			mutate:  func(c *Config) { c.WakeWidth = 5 },
			wantErr: "must not exceed width",
		},
		{
			name:    "eval mode",
			mutate:  func(c *Config) { c.Mode, c.TasksPerImage, c.WakeWidth = ModeEval, 1, 2 },
			wantErr: "steps mode",
		},
		{
			name:    "open-loop mode",
			mutate:  func(c *Config) { c.Mode, c.ArrivalRate, c.WakeWidth = ModeOpenLoop, 10, 2 },
			wantErr: "steps mode",
		},
		{
			name:    "park none",
			mutate:  func(c *Config) { c.Park, c.WakeWidth = ParkNone, 2 },
			wantErr: "park mode suspend",
		},
		{
			name:    "park delete",
			mutate:  func(c *Config) { c.Park, c.WakeWidth = ParkDelete, 2 },
			wantErr: "park mode suspend",
		},
		{
			name:    "park fork",
			mutate:  func(c *Config) { c.Park, c.WakeWidth = ParkFork, 2 },
			wantErr: "park mode suspend",
		},
		{
			name:    "with images per step",
			mutate:  func(c *Config) { c.Park, c.ImagesPerStep, c.WakeWidth = ParkDelete, 2, 2 },
			wantErr: "images per step",
		},
		{
			name:    "with strategy",
			mutate:  func(c *Config) { c.Strategy, c.Steps, c.Park, c.WakeWidth = StrategyNaive, 1, "", 2 },
			wantErr: "step loop",
		},
		{
			name: "with schedule",
			mutate: func(c *Config) {
				c.WakeWidth = 2
				c.Schedule = &schema.Schedule{SchemaVersion: "v1", Jobs: []schema.ScheduledJob{{Job: "j", ParkPolicy: schema.ParkSuspendSync}}}
			},
			wantErr: "step loop",
		},
		{
			name:    "with tasks per step",
			mutate:  func(c *Config) { c.TasksPerStep, c.WakeWidth = 2, 2 },
			wantErr: "step loop",
		},
		{
			name:    "with group size",
			mutate:  func(c *Config) { c.GroupSize, c.WakeWidth = 2, 2 },
			wantErr: "step loop",
		},
		{
			name:    "with step pause",
			mutate:  func(c *Config) { c.StepPause, c.WakeWidth = time.Second, 2 },
			wantErr: "step loop",
		},
		{
			name:    "with prefetch",
			mutate:  func(c *Config) { c.Prefetch, c.WakeWidth = true, 2 },
			wantErr: "step loop",
		},
		{
			name:    "valid suspend-sync",
			mutate:  func(c *Config) { c.Park, c.WakeWidth = ParkSuspendSync, 2 },
			wantErr: "",
		},
		{
			name:    "valid suspend-async",
			mutate:  func(c *Config) { c.Park, c.WakeWidth = ParkSuspendAsync, 2 },
			wantErr: "",
		},
		{
			name:    "valid equal to width",
			mutate:  func(c *Config) { c.WakeWidth = 4 },
			wantErr: "",
		},
		{
			name:    "valid zero default",
			mutate:  func(c *Config) { c.WakeWidth = 0 },
			wantErr: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig(t, newFake(), nil)
			cfg.Mode, cfg.Park = ModeSteps, ParkSuspend
			cfg.Width, cfg.Rollouts, cfg.Steps = 4, 1, 2
			tc.mutate(&cfg)
			err := cfg.validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("validate() = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// SL-15: LoaderLabelKeys includes wake_width, Config.labels omits it at 0,
// report -arm separates runs with different wake widths, and Config.validate
// refuses -label wake_width.
func TestSL15WakeWidthLabel(t *testing.T) {
	if !slices.Contains(LoaderLabelKeys, "wake_width") {
		t.Fatal("LoaderLabelKeys lacks wake_width")
	}
	cfg := baseConfig(t, newFake(), nil)
	cfg.Mode, cfg.Park, cfg.Width, cfg.Rollouts, cfg.Steps = ModeSteps, ParkSuspend, 4, 1, 2
	l0 := cfg.labels()
	if _, ok := l0["wake_width"]; ok {
		t.Errorf("wake_width=0 must omit label, got %v", l0)
	}
	cfg.WakeWidth = 2
	l2 := cfg.labels()
	if got := l2["wake_width"]; got != "2" {
		t.Errorf("wake_width = %q, want 2", got)
	}
	runEvents := func(id string, lbl map[string]string) []Event {
		return []Event{
			{RunID: id, Event: EventRun, Labels: lbl, OK: true},
			{RunID: id, Event: EventTask, DurMS: 1, OK: true},
		}
	}
	if _, err := Pool([][]Event{runEvents("r1", l0), runEvents("r2", l2)}); err == nil || !strings.Contains(err.Error(), "wake_width") {
		t.Errorf("Pool(wake_width=0, wake_width=2) = %v, want wake_width mismatch", err)
	}
	cfg.WakeWidth = 0
	cfg.Labels = map[string]string{"wake_width": "2"}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "label wake_width") {
		t.Errorf("validate() with -label wake_width = %v, want refused", err)
	}
}
