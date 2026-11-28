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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/benchmarking/schema"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type adoptableFake struct {
	*fakeBackend
	existing map[string]bool
	adoptErr map[string]error
}

func newAdoptableFake() *adoptableFake {
	return &adoptableFake{
		fakeBackend: newFake(),
		existing:    map[string]bool{},
		adoptErr:    map[string]error{},
	}
}

func (a *adoptableFake) Adopt(_ context.Context, id string) (Sandbox, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.adoptErr[id]; err != nil {
		return nil, err
	}
	if !a.existing[id] {
		return nil, status.Error(codes.NotFound, "no such environment")
	}
	sb := &fakeSandbox{b: a.fakeBackend, id: id}
	sb.parked.Store(true)
	return sb, nil
}

// SL-8 and SL-10 (docs/plans/swebench-loader/acceptance-criteria.md): -adopt
// reuses parked environments on step 1 (running Reset and recording wake
// without creating them), creates missing ones, and does not delete existing
// environments if Adopt returns a non-NotFound error.
func TestSL8AdoptStartsOnExistingFleet(t *testing.T) {
	traj, err := ParseTrajectory([]byte(`
name: adopt-reset
setup:
  - name: init
    run: "setup-cmd"
reset:
  - name: rst
    run: "reset-cmd"
steps:
  - name: work
    run: "work-cmd"
    reward: true
`))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		existing int
		parkEach bool
	}{
		{name: "whole fleet exists, park-each", existing: 4, parkEach: true},
		{name: "half fleet exists, park-each", existing: 2, parkEach: true},
		{name: "half fleet exists, barrier", existing: 2, parkEach: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdoptableFake()
			var out bytes.Buffer
			cfg := baseConfig(t, f, &out)
			cfg.Trajectory = traj
			cfg.Mode, cfg.Park, cfg.ParkEach = ModeSteps, ParkSuspend, tc.parkEach
			cfg.Adopt, cfg.Keep = true, true
			cfg.Instances, cfg.Rollouts, cfg.Steps = instances(4), 1, 2
			cfg.Width = 4

			adoptedIDs := map[string]bool{}
			for _, in := range cfg.Instances[:tc.existing] {
				id := EnvID(cfg.RunID, in.InstanceID, 0)
				f.existing[id] = true
				adoptedIDs[id] = true
			}

			sum, err := Run(context.Background(), cfg)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if sum.Tasks != 8 || sum.Failed != 0 {
				t.Fatalf("summary = %+v, want 8 tasks and 0 failures", sum)
			}
			if got, want := len(f.created), 4-tc.existing; got != want {
				t.Errorf("created %d environments, want %d", got, want)
			}

			events := parseEvents(t, out.String())
			step1Creates, step1Wakes, step1TTFI := 0, 0, 0
			for _, ev := range events {
				switch {
				case ev.Event == EventCreate:
					step1Creates++
					if adoptedIDs[ev.Env] {
						t.Errorf("adopted env %s recorded a create event", ev.Env)
					}
				case ev.Event == EventWake && ev.Step == 1:
					step1Wakes++
					if !adoptedIDs[ev.Env] {
						t.Errorf("fresh env %s recorded wake on step 1", ev.Env)
					}
				case ev.Event == EventTTFI && ev.Step == 1:
					step1TTFI++
					if adoptedIDs[ev.Env] {
						t.Errorf("adopted env %s recorded ttfi on step 1", ev.Env)
					}
				case ev.Event == EventStep && ev.Step == 1 && ev.Name == "init" && adoptedIDs[ev.Env]:
					t.Errorf("adopted env %s ran Setup instead of Reset on step 1", ev.Env)
				case ev.Event == EventStep && ev.Step == 1 && ev.Name == "rst" && !adoptedIDs[ev.Env]:
					t.Errorf("fresh env %s ran Reset instead of Setup on step 1", ev.Env)
				case ev.Event == EventRun && ev.Labels["adopt"] != "true":
					t.Errorf("run labels = %v, want adopt=true", ev.Labels)
				}
			}
			if step1Creates != 4-tc.existing || step1Wakes != tc.existing || step1TTFI != 4-tc.existing {
				t.Errorf("step 1 creates=%d wakes=%d ttfi=%d, want %d %d %d",
					step1Creates, step1Wakes, step1TTFI, 4-tc.existing, tc.existing, 4-tc.existing)
			}
		})
	}

	// Non-NotFound Adopt errors fail the task without calling Create or Delete
	// on the existing environment.
	t.Run("non-NotFound adopt error preserves existing env", func(t *testing.T) {
		f := newAdoptableFake()
		var out bytes.Buffer
		cfg := baseConfig(t, f, &out)
		cfg.Trajectory = traj
		cfg.Mode, cfg.Park, cfg.Adopt, cfg.Keep = ModeSteps, ParkSuspend, true, true
		cfg.Instances, cfg.Rollouts, cfg.Steps = instances(2), 1, 1
		id0 := EnvID(cfg.RunID, cfg.Instances[0].InstanceID, 0)
		f.existing[id0] = true
		f.adoptErr[id0] = status.Error(codes.Unavailable, "api unreachable")

		sum, err := Run(context.Background(), cfg)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if sum.Tasks != 2 || sum.Failed != 1 {
			t.Fatalf("summary = %+v, want 2 tasks and 1 failure", sum)
		}
		if f.created[id0] != 0 || f.deletedByID[id0] != 0 {
			t.Errorf("existing env %s: created=%d deletedByID=%d, want 0 and 0", id0, f.created[id0], f.deletedByID[id0])
		}
	})
}

// SL-8 on EnvBackend: GetEnvironment adopts existing environments and falls
// back to CreateEnvironment when GetEnvironment answers NotFound.
func TestSL8AdoptOnEnvBackend(t *testing.T) {
	traj, err := ParseTrajectory([]byte(`
name: env-adopt
reset:
  - name: rst
    run: "true"
steps:
  - name: work
    run: "true"
    reward: true
`))
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{
		nodes:    map[string]string{},
		existing: map[string]bool{},
	}
	var out bytes.Buffer
	cfg := baseConfig(t, newFakeBackend(t, api), &out)
	cfg.Trajectory = traj
	cfg.Mode, cfg.Park, cfg.Adopt, cfg.Keep = ModeSteps, ParkSuspend, true, true
	cfg.Instances, cfg.Rollouts, cfg.Steps = instances(2), 1, 2
	id0 := EnvID(cfg.RunID, cfg.Instances[0].InstanceID, 0)
	id1 := EnvID(cfg.RunID, cfg.Instances[1].InstanceID, 0)
	api.nodes[id0] = "node-1"
	api.existing[id0] = true

	sum, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Tasks != 4 || sum.Failed != 0 {
		t.Fatalf("summary = %+v, want 4 tasks and 0 failures", sum)
	}
	if !slices.Equal(api.created, []string{id1}) {
		t.Errorf("api.created = %v, want only missing [%s]", api.created, id1)
	}
}

// SL-9: Config.validate refuses invalid -adopt combinations.
func TestSL9ValidateAdopt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:    "eval mode",
			mutate:  func(c *Config) { c.Mode, c.TasksPerImage, c.Adopt = ModeEval, 1, true },
			wantErr: "steps mode",
		},
		{
			name:    "park delete",
			mutate:  func(c *Config) { c.Park, c.Adopt = ParkDelete, true },
			wantErr: "suspend",
		},
		{
			name:    "park none",
			mutate:  func(c *Config) { c.Park, c.Adopt = ParkNone, true },
			wantErr: "suspend",
		},
		{
			name:    "park fork",
			mutate:  func(c *Config) { c.Park, c.Adopt = ParkFork, true },
			wantErr: "suspend",
		},
		{
			name:    "images per step",
			mutate:  func(c *Config) { c.Park, c.ImagesPerStep, c.Adopt = ParkDelete, 1, true },
			wantErr: "images per step",
		},
		{
			name:    "with strategy",
			mutate:  func(c *Config) { c.Strategy, c.Steps, c.Park, c.Adopt = StrategyNaive, 1, "", true },
			wantErr: "step loop",
		},
		{
			name: "with schedule",
			mutate: func(c *Config) {
				c.Adopt = true
				c.Schedule = &schema.Schedule{SchemaVersion: "v1", Jobs: []schema.ScheduledJob{{Job: "j", ParkPolicy: schema.ParkSuspendSync}}}
			},
			wantErr: "step loop",
		},
		{
			name:    "with step pause",
			mutate:  func(c *Config) { c.StepPause, c.Adopt = time.Second, true },
			wantErr: "step loop",
		},
		{
			name:    "with group size",
			mutate:  func(c *Config) { c.GroupSize, c.Adopt = 2, true },
			wantErr: "step loop",
		},
		{
			name:    "backend lacks Adopter",
			mutate:  func(c *Config) { c.Backend, c.Adopt = newFake(), true },
			wantErr: "Adopter",
		},
		{
			name:    "valid suspend",
			mutate:  func(c *Config) { c.Park, c.Adopt = ParkSuspend, true },
			wantErr: "",
		},
		{
			name:    "valid suspend-sync",
			mutate:  func(c *Config) { c.Park, c.Adopt = ParkSuspendSync, true },
			wantErr: "",
		},
		{
			name:    "valid suspend-async",
			mutate:  func(c *Config) { c.Park, c.Adopt = ParkSuspendAsync, true },
			wantErr: "",
		},
		{
			name:    "valid park-each",
			mutate:  func(c *Config) { c.Park, c.ParkEach, c.Adopt = ParkSuspend, true, true },
			wantErr: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig(t, newAdoptableFake(), nil)
			cfg.Mode, cfg.Park = ModeSteps, ParkSuspend
			cfg.Width, cfg.Rollouts, cfg.Steps = 2, 1, 2
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

// SL-9: Run refuses Adopt when Backend does not implement Adopter without
// creating environments or recording a run event.
func TestSL9RunRejectsBackendWithoutAdopter(t *testing.T) {
	f := newFake()
	var out bytes.Buffer
	cfg := baseConfig(t, f, &out)
	cfg.Mode, cfg.Park, cfg.Adopt = ModeSteps, ParkSuspend, true
	cfg.Instances, cfg.Rollouts, cfg.Steps, cfg.Width = instances(2), 1, 2, 2

	_, err := Run(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "Adopter") {
		t.Fatalf("Run() = %v, want Adopter error", err)
	}
	if len(f.created) != 0 {
		t.Errorf("created %d environments on rejected Run, want 0", len(f.created))
	}
	if out.Len() != 0 {
		t.Errorf("recorded events on rejected Run: %s", out.String())
	}
}

// SL-10: adopt is declared in LoaderLabelKeys, omitted when false, and
// rejected as an operator -label.
func TestSL10AdoptLabel(t *testing.T) {
	if !slices.Contains(LoaderLabelKeys, "adopt") {
		t.Fatal("LoaderLabelKeys lacks adopt")
	}
	cfg := baseConfig(t, newAdoptableFake(), nil)
	cfg.Mode, cfg.Park, cfg.Width, cfg.Rollouts, cfg.Steps = ModeSteps, ParkSuspend, 2, 1, 2
	if _, ok := cfg.labels()["adopt"]; ok {
		t.Errorf("Adopt=false must omit label, got %v", cfg.labels())
	}
	cfg.Adopt = true
	if got := cfg.labels()["adopt"]; got != "true" {
		t.Errorf("adopt = %q, want true", got)
	}
	cfg.Adopt = false
	cfg.Labels = map[string]string{"adopt": "true"}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "label adopt") {
		t.Errorf("validate() with -label adopt = %v, want refused", err)
	}
}

type adoptCreateWidthBackend struct {
	*coldStartBackend
	existing map[string]bool
}

func (a *adoptCreateWidthBackend) Adopt(_ context.Context, id string) (Sandbox, error) {
	if !a.existing[id] {
		return nil, status.Error(codes.NotFound, "no such environment")
	}
	return &coldStartSandbox{b: a.coldStartBackend, id: id, fresh: false}, nil
}

// SL-5 and SL-8: under -adopt with -create-width, environments that Adopt finds
// wake at -width without acquiring -create-width, while NotFound fallback
// Backend.Create environments hold -create-width through their first command.
func TestSL8AdoptWithCreateWidth(t *testing.T) {
	traj, err := ParseTrajectory([]byte(`
name: adopt-cw
setup:
  - name: init
    run: "cold-cmd"
reset:
  - name: rst
    run: "wake-cmd"
steps:
  - name: work
    think: 20ms
    run: "work-cmd"
    reward: true
`))
	if err != nil {
		t.Fatal(err)
	}

	for _, parkEach := range []bool{false, true} {
		name := "barrier"
		if parkEach {
			name = "park-each"
		}
		t.Run(name, func(t *testing.T) {
			b := &adoptCreateWidthBackend{
				coldStartBackend: newColdStartBackend(),
				existing:         map[string]bool{},
			}
			// 4 adopted environments must all reach wakeBarrier(4) concurrently
			// even though CreateWidth is 2, while 4 missing environments must
			// never exceed 2 concurrent cold starts.
			b.wakeBarrier = newSyncBarrier(4)
			b.coldBarrier = newSyncBarrier(2)

			var out bytes.Buffer
			cfg := baseConfig(t, b, &out)
			cfg.Trajectory = traj
			cfg.Mode, cfg.Park, cfg.ParkEach = ModeSteps, ParkSuspend, parkEach
			cfg.Adopt, cfg.Keep = true, true
			cfg.Instances, cfg.Rollouts, cfg.Steps = instances(8), 1, 1
			cfg.Width, cfg.CreateWidth = 8, 2

			for _, in := range cfg.Instances[:4] {
				b.existing[EnvID(cfg.RunID, in.InstanceID, 0)] = true
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			sum, err := Run(ctx, cfg)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if sum.Tasks != 8 || sum.Failed != 0 {
				t.Fatalf("summary = %+v, want 8 tasks and 0 failures", sum)
			}
			if got := b.maxColdInflight.Load(); got != 2 {
				t.Fatalf("max concurrent fallback cold starts = %d, want 2", got)
			}
			if got := b.maxCreateInflight.Load(); got > 2 {
				t.Fatalf("max concurrent fallback Backend.Create calls = %d, want <= 2", got)
			}
		})
	}
}
