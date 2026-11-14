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
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// domainFake is a fakeBackend with two placement domains: replay k of any
// image goes to domain k%2. It counts the environments alive per domain and
// holds every create until hold environments are alive across the domains,
// or until stuck closes.
type domainFake struct {
	*fakeBackend
	dmu     sync.Mutex
	live    map[string]int
	maxLive map[string]int
	created map[string]string
	hold    int
	full    chan struct{}
	stuck   <-chan struct{}
}

func (d *domainFake) DomainFor(image string, k int) string { return fmt.Sprintf("d%d", k%2) }

func (d *domainFake) CreateInDomain(ctx context.Context, id, image, domain string) (Sandbox, error) {
	sb, err := d.fakeBackend.Create(ctx, id, image)
	if err != nil {
		return nil, err
	}
	d.dmu.Lock()
	d.created[id] = domain
	d.live[domain]++
	d.maxLive[domain] = max(d.maxLive[domain], d.live[domain])
	if d.live["d0"]+d.live["d1"] == d.hold {
		select {
		case <-d.full:
		default:
			close(d.full)
		}
	}
	d.dmu.Unlock()
	select {
	case <-d.full:
	case <-d.stuck:
	}
	return &domainSandbox{Sandbox: sb, done: func() {
		d.dmu.Lock()
		d.live[domain]--
		d.dmu.Unlock()
	}}, nil
}

type domainSandbox struct {
	Sandbox
	done func()
}

func (s *domainSandbox) Delete(ctx context.Context) error {
	s.done()
	return s.Sandbox.Delete(ctx)
}

// runDomainFake runs eval on a domainFake that holds every create until hold
// environments are alive across the two domains, and returns the fake and
// the run's events. A run that never has hold alive waits 10 s and goes on.
func runDomainFake(t *testing.T, hold int, shape func(*Config)) (*domainFake, []Event) {
	t.Helper()
	stuck, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d := &domainFake{fakeBackend: newFake(), live: map[string]int{}, maxLive: map[string]int{}, created: map[string]string{},
		hold: hold, full: make(chan struct{}), stuck: stuck.Done()}
	var out bytes.Buffer
	cfg := baseConfig(t, d, &out)
	cfg.Mode = ModeEval
	cfg.Width = 1 // not used when DomainWidth is set
	shape(&cfg)
	sum, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if want := len(cfg.Instances) * cfg.TasksPerImage; sum.Tasks != want || sum.Failed != 0 || len(d.created) != want {
		t.Fatalf("summary %+v, %d created in a domain; want %d tasks, each created in a domain", sum, len(d.created), want)
	}
	return d, parseEvents(t, out.String())
}

func TestEvalDomainWidthIsAClosedLoopPerDomain(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		images, tasks, width int
		// alive is the most environments each domain holds at once. One loop
		// of width over the whole run never holds their sum, and the test
		// fails.
		alive map[string]int
	}{
		{"3 in each domain", 6, 8, 3, map[string]int{"d0": 3, "d1": 3}},
		{"1 in each domain", 2, 4, 1, map[string]int{"d0": 1, "d1": 1}},
		{"wider than the queues", 1, 3, 50, map[string]int{"d0": 2, "d1": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			total := tc.alive["d0"] + tc.alive["d1"]
			d, events := runDomainFake(t, total, func(c *Config) {
				c.Instances, c.TasksPerImage, c.DomainWidth = instances(tc.images), tc.tasks, tc.width
			})
			perDomain := map[string]int{}
			for id, dom := range d.created {
				perDomain[dom]++
				// The id ends in the replay index; replay k belongs to domain k%2.
				k, err := strconv.Atoi(id[strings.LastIndex(id, "-")+1:])
				if want := fmt.Sprintf("d%d", k%2); err != nil || want != dom {
					t.Errorf("%s created in %s, want %s (%v)", id, dom, want, err)
				}
			}
			if want := map[string]int{"d0": tc.images * ((tc.tasks + 1) / 2), "d1": tc.images * (tc.tasks / 2)}; !reflect.DeepEqual(perDomain, want) {
				t.Errorf("created per domain %v, want %v", perDomain, want)
			}
			select {
			case <-d.full:
			default:
				t.Errorf("the two domains never had %d environments alive at once: the run is not a loop per domain", total)
			}
			if !reflect.DeepEqual(d.maxLive, tc.alive) {
				t.Errorf("most environments alive at once per domain %v, want %v", d.maxLive, tc.alive)
			}
			// The run event reports what the two queues ran, and no width.
			run := events[len(events)-1]
			_, width := run.Labels["width"]
			if run.Event != EventRun || run.Labels["domain_width"] != strconv.Itoa(tc.width) || width || run.Tags["load_max_concurrent"] != strconv.Itoa(total) {
				t.Errorf("run event: labels %v, tags %v; want domain_width %d, no width and load_max_concurrent %d", run.Labels, run.Tags, tc.width, total)
			}
		})
	}
}

// Width is not used with a domain width, so report -arm takes two runs that
// differ only in it as reps of one workload.
func TestDomainWidthRunsPoolWhateverTheirWidth(t *testing.T) {
	var runs [][]Event
	for i, width := range []int{1, 50} {
		_, events := runDomainFake(t, 2, func(c *Config) {
			c.RunID, c.Width = fmt.Sprintf("r%d", i+1), width
			c.Instances, c.TasksPerImage, c.DomainWidth = instances(1), 2, 1
		})
		runs = append(runs, events)
	}
	if rep, err := Pool(runs); err != nil || rep.Reps != 2 {
		t.Errorf("Pool of two domain-width runs with -width 1 and 50: %v", err)
	}
}

func TestEvalDomainWidthNeedsPlacementDomains(t *testing.T) {
	cfg := baseConfig(t, newFake(), nil)
	cfg.Mode = ModeEval
	cfg.TasksPerImage = 1
	cfg.DomainWidth = 2
	if _, err := Run(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "placement domains") {
		t.Fatalf("a backend without placement domains: err = %v, want a refusal", err)
	}
}

// On the EnvBackend the domains are the placement plan's.
func TestEvalDomainWidthOnTheEnvBackend(t *testing.T) {
	// Without a plan no environment gets a domain, and nothing is created.
	api := &fakeAPI{}
	cfg := baseConfig(t, newFakeBackend(t, api), nil)
	cfg.Mode, cfg.TasksPerImage, cfg.DomainWidth = ModeEval, 2, 2
	if _, err := Run(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "put none of them in a domain") {
		t.Fatalf("an EnvBackend without a plan: err = %v, want a refusal", err)
	}
	if len(api.created) != 0 {
		t.Fatalf("created %v before the refusal", api.created)
	}

	// Instance a has the image the plan puts in d0 and d1; the plan does not
	// place the image of instance b. That is three queues.
	api = &fakeAPI{}
	b := newFakeBackend(t, api)
	WithPlacement(twoDomainPlan())(b)
	var out bytes.Buffer
	cfg = baseConfig(t, b, &out)
	cfg.Mode, cfg.TasksPerImage, cfg.DomainWidth = ModeEval, 2, 2
	cfg.Instances = instances(2)
	cfg.Instances[0].Image = pfImgB
	// fakeAPI serves no WriteFile, so each task fails at its write step,
	// after the create that is checked here.
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	a, unplaced := cfg.Instances[0].InstanceID, cfg.Instances[1].InstanceID
	for id, want := range map[string]string{EnvID("t", a, 0): "d0", EnvID("t", a, 1): "d1", EnvID("t", unplaced, 0): "", EnvID("t", unplaced, 1): ""} {
		sel, created := api.selectors[id]
		if !created || sel["ate-env/domain"] != want {
			t.Errorf("%s: created %v with selector %v, want domain %q", id, created, sel, want)
		}
	}
	events := parseEvents(t, out.String())
	if run := events[len(events)-1]; run.Event != EventRun || run.Tags["load_max_concurrent"] != "4" {
		t.Errorf("run event tags %v, want load_max_concurrent 4: 1 in d0, 1 in d1 and 2 unplaced", run.Tags)
	}
}

func TestValidateDomainWidth(t *testing.T) {
	steps := func(c *Config) { c.Mode, c.Rollouts, c.Steps, c.Park = ModeSteps, 1, 1, ParkDelete }
	for _, tc := range []struct {
		name  string
		shape func(*Config)
		width int
		err   string
	}{
		{"negative in eval", func(*Config) {}, -1, "domain width must be >= 0"},
		{"steps", steps, 2, "domain width applies to eval mode"},
		{"negative in steps", steps, -1, "domain width applies to eval mode"},
		{"open-loop", func(c *Config) { c.Mode, c.ArrivalRate = ModeOpenLoop, 1 }, 2, "domain width applies to eval mode"},
		{"strategy", func(c *Config) { c.Strategy = StrategyNone }, 2, "domain width belongs to eval's loop per domain"},
	} {
		cfg := baseConfig(t, newFake(), nil)
		cfg.Mode, cfg.TasksPerImage = ModeEval, 1
		tc.shape(&cfg)
		if err := cfg.validate(); err != nil {
			t.Fatalf("%s, no domain width: %v", tc.name, err)
		}
		cfg.DomainWidth = tc.width
		if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s, domain width %d: err = %v, want %q", tc.name, tc.width, err, tc.err)
		}
	}
}
