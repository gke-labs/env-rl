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
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestRoundOrderInterleavesReplays(t *testing.T) {
	const instances, fanout = 3, 2
	var envs []*environment
	for i := 0; i < instances; i++ {
		for k := 0; k < fanout; k++ {
			envs = append(envs, &environment{inst: Instance{InstanceID: string(rune('a' + i))}, task: k})
		}
	}
	got := roundOrder(envs, instances, fanout)
	want := []string{"a0", "b0", "c0", "a1", "b1", "c1"}
	if len(got) != len(want) {
		t.Fatalf("roundOrder returned %d environments, want %d", len(got), len(want))
	}
	for i, e := range got {
		if id := e.inst.InstanceID + string(rune('0'+e.task)); id != want[i] {
			t.Errorf("position %d: got %s, want %s", i, id, want[i])
		}
	}
	if same := roundOrder(envs, instances, 1); len(same) != len(envs) {
		t.Errorf("fanout 1 must return the input unchanged")
	}
}

// taskOrderConfigs returns one valid Config per mode, so a validate() error in
// these tests comes from the task order and from nothing before it.
func taskOrderConfigs(t *testing.T) map[Mode]Config {
	t.Helper()
	eval := baseConfig(t, newFake(), nil)
	eval.Mode, eval.TasksPerImage = ModeEval, 1
	steps := baseConfig(t, newFake(), nil)
	steps.Mode, steps.Rollouts, steps.Steps, steps.Park = ModeSteps, 1, 1, ParkDelete
	open := baseConfig(t, newFake(), nil)
	open.Mode, open.ArrivalRate = ModeOpenLoop, 1
	return map[Mode]Config{ModeEval: eval, ModeSteps: steps, ModeOpenLoop: open}
}

func TestValidateTaskOrder(t *testing.T) {
	for mode, cfg := range taskOrderConfigs(t) {
		for _, order := range []string{"", TaskOrderImage} {
			c := cfg
			c.TaskOrder = order
			if err := c.validate(); err != nil {
				t.Errorf("%s with task order %q: %v", mode, order, err)
			}
		}
		c := cfg
		c.TaskOrder = "sideways"
		if err := c.validate(); err == nil || !strings.Contains(err.Error(), `task order must be "image" or "round"`) {
			t.Errorf("%s with an unknown task order = %v, want it refused", mode, err)
		}
	}
}

func TestRoundOrderRefusedOutsideEval(t *testing.T) {
	for mode, cfg := range taskOrderConfigs(t) {
		cfg.TaskOrder = TaskOrderRound
		err := cfg.validate()
		if mode == ModeEval {
			if err != nil {
				t.Errorf("eval with task order round: %v", err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "task order round applies to eval mode only") {
			t.Errorf("%s with task order round = %v, want it refused", mode, err)
		}
	}
}

// A strategy run plans its own order and does not use plan(), so round would
// keep the image order while the run records task_order=round.
func TestRoundOrderRefusedWithStrategy(t *testing.T) {
	eval := taskOrderConfigs(t)[ModeEval]
	for _, strategy := range []Strategy{StrategyNone, StrategyNaive, StrategySliding, StrategyPipelined} {
		for _, order := range []string{"", TaskOrderImage} {
			c := eval
			c.Strategy, c.TaskOrder = strategy, order
			if err := c.validate(); err != nil {
				t.Errorf("strategy %s with task order %q: %v", strategy, order, err)
			}
		}
		c := eval
		c.Strategy, c.TaskOrder = strategy, TaskOrderRound
		if err := c.validate(); err == nil || !strings.Contains(err.Error(), "task order round belongs to eval's loop; it cannot be combined with a strategy") {
			t.Errorf("strategy %s with task order round = %v, want it refused", strategy, err)
		}
	}
}

// At width 1 an eval run creates its environments in plan order, so the
// creates show the order plan() gives each task order.
func TestEvalRunFollowsTaskOrder(t *testing.T) {
	for _, tc := range []struct {
		order, label string
		want         []string
	}{
		{TaskOrderImage, "", []string{"a-0", "a-1", "b-0", "b-1", "c-0", "c-1"}},
		{TaskOrderRound, TaskOrderRound, []string{"a-0", "b-0", "c-0", "a-1", "b-1", "c-1"}},
	} {
		f := newFake()
		var created []string
		f.onCreate = func(id string) { created = append(created, strings.TrimPrefix(id, "t-repo--repo-")) }
		var out bytes.Buffer
		cfg := baseConfig(t, f, &out)
		cfg.Mode, cfg.TasksPerImage, cfg.Width, cfg.TaskOrder = ModeEval, 2, 1, tc.order
		if _, err := Run(context.Background(), cfg); err != nil {
			t.Fatalf("task order %s: %v", tc.order, err)
		}
		if !slices.Equal(created, tc.want) {
			t.Errorf("task order %s created %v, want %v", tc.order, created, tc.want)
		}
		if got := Build(parseEvents(t, out.String())).Labels["task_order"]; got != tc.label {
			t.Errorf("task order %s: run event has task_order=%q, want %q", tc.order, got, tc.label)
		}
	}
}

// With a domain width each domain drains its own queue, and the queue keeps
// the task order. Two domains take replay k%2: 4 replays put two replays of
// every image in each domain, and round runs them a pass apart. 2 replays
// put one in each domain, so round has nothing to spread and the two orders
// are equal; the round run still records its label.
func TestEvalDomainWidthFollowsTaskOrder(t *testing.T) {
	for _, tc := range []struct {
		order string
		tasks int
		want  map[string][]string
	}{
		{TaskOrderImage, 4, map[string][]string{"d0": {"a-0", "a-2", "b-0", "b-2", "c-0", "c-2"}, "d1": {"a-1", "a-3", "b-1", "b-3", "c-1", "c-3"}}},
		{TaskOrderRound, 4, map[string][]string{"d0": {"a-0", "b-0", "c-0", "a-2", "b-2", "c-2"}, "d1": {"a-1", "b-1", "c-1", "a-3", "b-3", "c-3"}}},
		{TaskOrderImage, 2, map[string][]string{"d0": {"a-0", "b-0", "c-0"}, "d1": {"a-1", "b-1", "c-1"}}},
		{TaskOrderRound, 2, map[string][]string{"d0": {"a-0", "b-0", "c-0"}, "d1": {"a-1", "b-1", "c-1"}}},
	} {
		// The creates of one domain are in sequence at domain width 1, and
		// onCreate runs under the fake's lock.
		var created []string
		d, events := runDomainFake(t, 2, func(c *Config) {
			c.Backend.(*domainFake).onCreate = func(id string) { created = append(created, id) }
			c.TasksPerImage, c.DomainWidth, c.TaskOrder = tc.tasks, 1, tc.order
		})
		got := map[string][]string{}
		for _, id := range created {
			got[d.created[id]] = append(got[d.created[id]], strings.TrimPrefix(id, "t-repo--repo-"))
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("task order %s, %d replays: created per domain %v, want %v", tc.order, tc.tasks, got, tc.want)
		}
		want := ""
		if tc.order == TaskOrderRound {
			want = TaskOrderRound
		}
		if run := events[len(events)-1]; run.Event != EventRun || run.Labels["task_order"] != want || run.Labels["domain_width"] != "1" {
			t.Errorf("task order %s, %d replays: run event labels %v, want task_order=%q and domain_width=1", tc.order, tc.tasks, run.Labels, want)
		}
	}
}
