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
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
)

// syncBarrier releases its waiters once n have arrived, then resets.
type syncBarrier struct {
	mu      sync.Mutex
	n       int
	waiting int
	ch      chan struct{}
}

func newSyncBarrier(n int) *syncBarrier {
	return &syncBarrier{n: n, ch: make(chan struct{})}
}

func (b *syncBarrier) wait(ctx context.Context) error {
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

type coldStartBackend struct {
	mu                sync.Mutex
	created           map[string]int
	createInflight    atomic.Int32
	maxCreateInflight atomic.Int32
	coldInflight      atomic.Int32
	maxColdInflight   atomic.Int32
	awake             atomic.Int32
	maxAwake          atomic.Int32

	coldBarrier *syncBarrier
	workBarrier *syncBarrier
	wakeBarrier *syncBarrier
}

func newColdStartBackend() *coldStartBackend {
	return &coldStartBackend{created: map[string]int{}}
}

func bumpMax(cur, maxVal *atomic.Int32, delta int32) {
	n := cur.Add(delta)
	for {
		m := maxVal.Load()
		if n <= m || maxVal.CompareAndSwap(m, n) {
			return
		}
	}
}

func (b *coldStartBackend) Create(ctx context.Context, id, _ string) (Sandbox, error) {
	bumpMax(&b.createInflight, &b.maxCreateInflight, 1)
	defer b.createInflight.Add(-1)
	if b.coldBarrier != nil {
		if err := b.coldBarrier.wait(ctx); err != nil {
			return nil, err
		}
	}
	b.mu.Lock()
	b.created[id]++
	b.mu.Unlock()
	bumpMax(&b.awake, &b.maxAwake, 1)
	return &coldStartSandbox{b: b, id: id, fresh: true}, nil
}

func (b *coldStartBackend) Delete(_ context.Context, _ string) error { return nil }

type coldStartSandbox struct {
	b      *coldStartBackend
	id     string
	mu     sync.Mutex
	fresh  bool
	parked bool
}

func (s *coldStartSandbox) Run(ctx context.Context, command string, _ time.Duration) (Result, error) {
	s.mu.Lock()
	wasFresh := s.fresh
	wasParked := s.parked
	s.fresh = false
	s.parked = false
	s.mu.Unlock()

	if wasParked {
		bumpMax(&s.b.awake, &s.b.maxAwake, 1)
		if s.b.wakeBarrier != nil {
			if err := s.b.wakeBarrier.wait(ctx); err != nil {
				return Result{}, err
			}
		}
	}
	if wasFresh {
		bumpMax(&s.b.coldInflight, &s.b.maxColdInflight, 1)
		defer s.b.coldInflight.Add(-1)
		if s.b.coldBarrier != nil {
			if err := s.b.coldBarrier.wait(ctx); err != nil {
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

func (s *coldStartSandbox) WriteFile(context.Context, string, []byte, fs.FileMode) error { return nil }

func (s *coldStartSandbox) Suspend(context.Context) error {
	s.mu.Lock()
	if !s.parked {
		s.parked = true
		s.b.awake.Add(-1)
	}
	s.mu.Unlock()
	return nil
}

func (s *coldStartSandbox) Delete(context.Context) error {
	s.mu.Lock()
	if !s.parked {
		s.parked = true
		s.b.awake.Add(-1)
	}
	s.mu.Unlock()
	return nil
}

// SL-5 and SL-7 (docs/plans/swebench-loader/acceptance-criteria.md):
// -create-width caps concurrent creates and first-command cold boots on fresh
// steps, then releases the slot before Step.Think so all -width environments
// overlap during think and later commands.
func TestSL5CreateWidthCapsColdStartsBeforeThink(t *testing.T) {
	traj, err := ParseTrajectory([]byte(`
name: cold-then-think
steps:
  - name: probe
    run: "true"
  - name: work
    think: 20ms
    run: "work"
    reward: true
`))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name          string
		park          Park
		parkEach      bool
		imagesPerStep int
	}{
		{name: "park-each suspend", park: ParkSuspend, parkEach: true},
		{name: "barrier suspend", park: ParkSuspend, parkEach: false},
		{name: "park-each delete", park: ParkDelete, parkEach: true},
		{name: "window delete", park: ParkDelete, imagesPerStep: 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newColdStartBackend()
			b.coldBarrier = newSyncBarrier(2)
			b.workBarrier = newSyncBarrier(6)

			var out bytes.Buffer
			cfg := baseConfig(t, b, &out)
			cfg.Trajectory = traj
			cfg.Mode = ModeSteps
			cfg.Park = tc.park
			cfg.ParkEach = tc.parkEach
			cfg.ImagesPerStep = tc.imagesPerStep
			cfg.Instances = instances(6)
			cfg.Rollouts = 1
			cfg.Steps = 1
			cfg.Width = 6
			cfg.CreateWidth = 2

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			sum, err := Run(ctx, cfg)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if sum.Tasks != 6 || sum.Failed != 0 {
				t.Fatalf("summary = %+v, want 6 tasks and 0 failures", sum)
			}
			if got := b.maxCreateInflight.Load(); got != 2 {
				t.Errorf("maxCreateInflight = %d, want 2", got)
			}
			if got := b.maxColdInflight.Load(); got != 2 {
				t.Errorf("maxColdInflight = %d, want 2", got)
			}
			if got := b.maxAwake.Load(); got != 6 {
				t.Errorf("maxAwake = %d, want 6 (slot released before think)", got)
			}
			for _, ev := range parseEvents(t, out.String()) {
				if ev.Event == EventRun && ev.Labels["create_width"] != "2" {
					t.Errorf("run labels = %v, want create_width=2", ev.Labels)
				}
			}
		})
	}
}

// SL-5: wakes of a suspended fleet on later steps run at full -width rather
// than -create-width.
func TestSL5CreateWidthLeavesWakesAtWidth(t *testing.T) {
	traj, err := ParseTrajectory([]byte(`
name: wake-at-width
steps:
  - name: probe
    run: "true"
    reward: true
`))
	if err != nil {
		t.Fatal(err)
	}
	b := newColdStartBackend()
	b.coldBarrier = newSyncBarrier(2)
	b.wakeBarrier = newSyncBarrier(6)

	cfg := baseConfig(t, b, nil)
	cfg.Trajectory = traj
	cfg.Mode, cfg.Park, cfg.ParkEach = ModeSteps, ParkSuspend, true
	cfg.Instances, cfg.Rollouts, cfg.Steps = instances(6), 1, 2
	cfg.Width, cfg.CreateWidth = 6, 2

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sum, err := Run(ctx, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Tasks != 12 || sum.Failed != 0 {
		t.Fatalf("summary = %+v, want 12 tasks and 0 failures", sum)
	}
	if got := b.maxColdInflight.Load(); got != 2 {
		t.Errorf("step 1 cold inflight = %d, want 2", got)
	}
}

// SL-6: Config.validate refuses invalid -create-width combinations.
func TestSL6ValidateCreateWidth(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:    "negative",
			mutate:  func(c *Config) { c.CreateWidth = -1 },
			wantErr: ">= 0",
		},
		{
			name:    "above width",
			mutate:  func(c *Config) { c.CreateWidth = 5 },
			wantErr: "must not exceed width",
		},
		{
			name:    "eval mode",
			mutate:  func(c *Config) { c.Mode, c.TasksPerImage, c.CreateWidth = ModeEval, 1, 2 },
			wantErr: "steps mode",
		},
		{
			name:    "open-loop mode",
			mutate:  func(c *Config) { c.Mode, c.ArrivalRate, c.CreateWidth = ModeOpenLoop, 10, 2 },
			wantErr: "steps mode",
		},
		{
			name:    "with strategy",
			mutate:  func(c *Config) { c.Strategy, c.Steps, c.Park, c.CreateWidth = StrategyNaive, 1, "", 2 },
			wantErr: "step loop",
		},
		{
			name: "with schedule",
			mutate: func(c *Config) {
				c.CreateWidth = 2
				c.Schedule = &schema.Schedule{SchemaVersion: "v1", Jobs: []schema.ScheduledJob{{Job: "j", ParkPolicy: schema.ParkSuspendSync}}}
			},
			wantErr: "step loop",
		},
		{
			name:    "with tasks per step",
			mutate:  func(c *Config) { c.TasksPerStep, c.CreateWidth = 2, 2 },
			wantErr: "step loop",
		},
		{
			name:    "with group size",
			mutate:  func(c *Config) { c.GroupSize, c.CreateWidth = 2, 2 },
			wantErr: "step loop",
		},
		{
			name:    "with step pause",
			mutate:  func(c *Config) { c.StepPause, c.CreateWidth = time.Second, 2 },
			wantErr: "step loop",
		},
		{
			name:    "with prefetch",
			mutate:  func(c *Config) { c.Prefetch, c.CreateWidth = true, 2 },
			wantErr: "step loop",
		},
		{
			name:    "valid equal to width",
			mutate:  func(c *Config) { c.CreateWidth = 4 },
			wantErr: "",
		},
		{
			name:    "valid zero default",
			mutate:  func(c *Config) { c.CreateWidth = 0 },
			wantErr: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig(t, newFake(), nil)
			cfg.Mode, cfg.Park = ModeSteps, ParkSuspend
			cfg.Width, cfg.Rollouts, cfg.Steps = 4, 1, 1
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

// SL-7: create_width is declared in LoaderLabelKeys, omitted when 0, and
// rejected as an operator -label.
func TestSL7CreateWidthLabel(t *testing.T) {
	if !slices.Contains(LoaderLabelKeys, "create_width") {
		t.Fatal("LoaderLabelKeys lacks create_width")
	}
	cfg := baseConfig(t, newFake(), nil)
	cfg.Mode, cfg.Park, cfg.Width, cfg.Rollouts, cfg.Steps = ModeSteps, ParkSuspend, 4, 1, 1
	if _, ok := cfg.labels()["create_width"]; ok {
		t.Errorf("create_width=0 must omit label, got %v", cfg.labels())
	}
	cfg.CreateWidth = 2
	if got := cfg.labels()["create_width"]; got != "2" {
		t.Errorf("create_width = %q, want 2", got)
	}
	cfg.CreateWidth = 0
	cfg.Labels = map[string]string{"create_width": "2"}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "label create_width") {
		t.Errorf("validate() with -label create_width = %v, want refused", err)
	}
}
