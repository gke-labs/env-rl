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

package fleetsite

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/agent-substrate/env/internal/inventory"
)

// scaledSnapshot is the golden snapshot grown to a large cluster: pools
// WorkerPools, workers workers spread over them, templates templates derived
// from base-0, base-1 and base-2 (the bases listed last, golden state none),
// tags tags, and maxRows+5 atespaces, actors and deployments. Every derived
// template is ready but derived-00000, which failed.
func scaledSnapshot(t *testing.T, cluster string, pools, workers, templates, tags int) *inventory.Snapshot {
	t.Helper()
	snap := golden(t, "proj-a", cluster, t0)
	pool, worker, tmpl, tag := snap.WorkerPools[0], snap.Workers[0], snap.Templates[0], snap.Tags[0]
	actor, space, dep := snap.Actors[0], snap.Atespaces[0], snap.Deployments[0]
	snap.WorkerPools, snap.Workers, snap.Templates, snap.Tags = nil, nil, nil, nil
	snap.Actors, snap.Atespaces, snap.Deployments = nil, nil, nil
	for i := range pools {
		p := pool
		p.Name = fmt.Sprintf("pool-%03d", i)
		snap.WorkerPools = append(snap.WorkerPools, p)
	}
	for i := range workers {
		w := worker
		w.Name, w.Pool = fmt.Sprintf("worker-%05d", i), fmt.Sprintf("pool-%03d", i%pools)
		snap.Workers = append(snap.Workers, w)
	}
	for i := range templates {
		d := tmpl
		d.Name, d.Base, d.GoldenState = fmt.Sprintf("derived-%05d", i), fmt.Sprintf("base-%d", i%3), "ready"
		if i == 0 {
			d.GoldenState = "failed"
		}
		snap.Templates = append(snap.Templates, d)
	}
	for i := range 3 {
		b := tmpl
		b.Name, b.GoldenState = fmt.Sprintf("base-%d", i), inventory.GoldenNone
		snap.Templates = append(snap.Templates, b)
	}
	for i := range tags {
		g := tag
		g.Name = fmt.Sprintf("tag-%05d", i)
		snap.Tags = append(snap.Tags, g)
	}
	for i := range maxRows + 5 {
		a := actor
		a.UID, a.Name, a.Template = fmt.Sprintf("u-%05d", i), fmt.Sprintf("env-%05d", i), "base-0"
		snap.Actors = append(snap.Actors, a)
		s := space
		s.Name = fmt.Sprintf("space-%03d", i)
		snap.Atespaces = append(snap.Atespaces, s)
		d := dep
		d.Name = fmt.Sprintf("api-%03d", i)
		snap.Deployments = append(snap.Deployments, d)
	}
	snap.Atespaces[maxRows+4].Name = actor.Atespace
	snap.Counts = inventory.ComputeCounts(snap.Actors)
	return snap
}

// FS-18: on a cluster with thousands of templates, tags and workers, every
// list of the Cluster page shows at most maxRows rows and says how many it
// leaves out, the templates are summarized by base template, and the page
// stays under 1 MiB. The JSON API keeps every row.
func TestClusterPageBoundsEveryList(t *testing.T) {
	ts := newTestSite(t)
	snap := scaledSnapshot(t, "big", 125, 1000, 3002, 5000)
	_, body := ts.getAfterWrite(t, snap, "/clusters/proj-a/big")
	if len(body) > 1<<20 {
		t.Errorf("the page is %d bytes, want at most 1 MiB", len(body))
	}
	for _, c := range []struct {
		name, row string
		total     int
	}{
		{"deployments", `<td>api-`, maxRows + 5},
		{"workers", `<td class="mono">worker-`, 1000},
		{"actors", `href="/actors/u-`, maxRows + 5},
		{"templates", `<td>derived-`, 3005},
		{"atespaces", `<td>space-`, maxRows + 5},
		{"tags", `<td>tag-`, 5000},
	} {
		if got := strings.Count(body, c.row); got != maxRows {
			t.Errorf("%d %s rows, want %d", got, c.name, maxRows)
		}
		if want := fmt.Sprintf("%d in the snapshot; the first %d are shown.", c.total, maxRows); !strings.Contains(body, want) {
			t.Errorf("the %s list does not say %q", c.name, want)
		}
	}
	if got := strings.Count(body, `<td><a href="?pool=ate-env%2Fpool-`); got != 125 {
		t.Errorf("%d WorkerPool rows, want 125", got)
	}
	// 125 pools fit a menu.
	if !strings.Contains(body, `<select name="pool"><option value="">any</option><option value="ate-env/pool-000">`) {
		t.Error("the pool filter of 125 pools is not a menu")
	}
	_, body = ts.get(t, "/clusters/proj-a/big?pool=ate-env/pool-003")
	if !strings.Contains(body, `<option value="ate-env/pool-003" selected>`) {
		t.Error("the pool menu does not keep the chosen pool")
	}
	// A filter and the bound together: base-1 has 1,001 derived templates.
	_, body = ts.get(t, "/clusters/proj-a/big?base=base-1")
	if !strings.Contains(body, fmt.Sprintf("3005 in the snapshot, 1002 match; the first %d are shown.", maxRows)) {
		t.Error("the filtered template list does not say how many match and how many are shown")
	}
	// base-0 has itself and 1,001 derived templates, one of them failed.
	for _, want := range []string{
		`<td><a href="?base=base-0#templates">base-0</a></td><td>1002</td>`,
		`<a href="?base=base-0&amp;golden=failed#templates">failed 1</a> · <a href="?base=base-0&amp;golden=none#templates">none 1</a> · <a href="?base=base-0&amp;golden=ready#templates">ready 1000</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the template summary lacks %q", want)
		}
	}

	_, api := ts.get(t, "/api/v1/clusters/proj-a/big")
	if got := strings.Count(api, `"name":"tag-`); got != 5000 {
		t.Errorf("the JSON API has %d tags, want 5000", got)
	}
}

// FS-18: the pool, base and golden filters select rows; each form and link
// keeps the filters of the other sections; a menu of more than maxRows
// options becomes a text box.
func TestClusterPageFilters(t *testing.T) {
	ts := newTestSite(t)
	snap := scaledSnapshot(t, "f", 250, 1000, 30, 0)
	_, body := ts.getAfterWrite(t, snap, "/clusters/proj-a/f?pool=ate-env/pool-007&state=running")
	workers := regexp.MustCompile(`<td class="mono">(worker-\d+)</td>`).FindAllStringSubmatch(body, -1)
	if len(workers) != 4 {
		t.Errorf("the pool filter shows %d workers, want 4 (7, 257, 507, 757)", len(workers))
	}
	for _, w := range workers {
		var n int
		fmt.Sscanf(w[1], "worker-%d", &n)
		if n%250 != 7 {
			t.Errorf("the pool filter shows %s of another pool", w[1])
		}
	}
	for _, want := range []string{
		"1000 in the snapshot, 4 match.",
		// 250 pools are more than a menu holds.
		`<input name="pool" value="ate-env/pool-007" placeholder="exact value">`,
		// The worker form keeps the actor filter, and the actor form the pool.
		`<form method="get" action="#workers" class="filters">
  <input type="hidden" name="state" value="running">`,
		`<form method="get" action="#actors" class="filters">
  <input type="hidden" name="pool" value="ate-env/pool-007">`,
		`<option value="running" selected>running</option>`,
		`href="?base=base-1&amp;pool=ate-env%2Fpool-007&amp;state=running#templates"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if strings.Contains(body, `<select name="pool"`) {
		t.Error("the pool filter is a menu of more than maxRows options")
	}
	if got := strings.Count(body, `<td><a href="?pool=ate-env%2Fpool-`); got != maxRows {
		t.Errorf("%d WorkerPool rows, want %d", got, maxRows)
	}
	if !strings.Contains(body, fmt.Sprintf("250 in the snapshot; the first %d are shown.", maxRows)) {
		t.Error("the WorkerPool list does not say it shows the first rows only")
	}

	_, body = ts.getAfterWrite(t, snap, "/clusters/proj-a/f?base=base-1")
	got := regexp.MustCompile(`<td>((?:base|derived)-\d+)</td><td>(base-\d)?</td>`).FindAllStringSubmatch(body, -1)
	if len(got) != 11 || got[10][1] != "base-1" {
		t.Errorf("base=base-1 shows %v, want its 10 derived templates and base-1", got)
	}
	for _, g := range got[:min(len(got), 10)] {
		if g[2] != "base-1" {
			t.Errorf("base=base-1 shows %s of %s", g[1], g[2])
		}
	}
	if !strings.Contains(body, `<option value="base-1" selected>base-1</option>`) {
		t.Error("the base filter is not kept in the form")
	}

	_, body = ts.getAfterWrite(t, snap, "/clusters/proj-a/f?golden=failed")
	if n := strings.Count(body, "<td>derived-"); n != 1 || !strings.Contains(body, "<td>derived-00000</td>") {
		t.Errorf("golden=failed shows %d templates, want derived-00000 only", n)
	}
	// A base template's link selects all its templates, whatever the golden
	// filter.
	if !strings.Contains(body, `<td><a href="?base=base-1#templates">base-1</a></td>`) {
		t.Error("the base template link keeps the golden filter")
	}

	// Every filter rides along in the other sections' forms and links.
	_, body = ts.get(t, "/clusters/proj-a/f?atespace=ate-env&base=base-1&golden=ready")
	for _, want := range []string{
		`<form method="get" action="#workers" class="filters">
  <input type="hidden" name="atespace" value="ate-env"><input type="hidden" name="base" value="base-1"><input type="hidden" name="golden" value="ready">`,
		`href="?atespace=ate-env&amp;base=base-1&amp;golden=ready&amp;pool=ate-env%2Fpool-000#workers"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
}

// FS-18: snapshot text in the filter links and forms is escaped; a pool name
// cannot close the attribute it is quoted in.
func TestClusterPageEscapesFilterValues(t *testing.T) {
	ts := newTestSite(t)
	snap := golden(t, "proj-a", "c-1", t0)
	evil := `p"><img src=x onerror=alert(1)>`
	snap.WorkerPools[0].Name = evil
	snap.Workers[0].Pool = evil
	snap.Templates[0].Name = evil
	snap.Templates[1].Base = evil
	_, body := ts.getAfterWrite(t, snap, "/clusters/proj-a/c-1?pool="+strings.ReplaceAll("ate-env/"+evil, " ", "+")+"&state=%22%3E%3Cimg")
	if strings.Contains(body, "<img") {
		t.Error("a snapshot or query value renders as markup")
	}
	for _, want := range []string{
		`href="?pool=ate-env%2Fp%22%3E%3Cimg&#43;src%3Dx&#43;onerror%3Dalert%281%29%3E&amp;state=%22%3E%3Cimg#workers"`,
		`<input type="hidden" name="state" value="&#34;&gt;&lt;img">`,
		`<td>ate-env/p&#34;&gt;&lt;img src=x onerror=alert(1)&gt;</td>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
}

// FS-18: the template summary lists the maxRows base templates with the most
// templates, and the base filter becomes a text box.
func TestClusterPageBoundsTemplateSummary(t *testing.T) {
	ts := newTestSite(t)
	snap := scaledSnapshot(t, "roots", 1, 1, 30, 0)
	for i := range maxRows + 5 {
		b := snap.Templates[0]
		b.Name, b.Base, b.GoldenState = fmt.Sprintf("solo-%03d", i), "", ""
		snap.Templates = append(snap.Templates, b)
	}
	_, body := ts.getAfterWrite(t, snap, "/clusters/proj-a/roots")
	if got := len(regexp.MustCompile(`<td><a href="\?base=[^"&]*#templates">`).FindAllString(body, -1)); got != maxRows {
		t.Errorf("%d base template rows, want %d", got, maxRows)
	}
	for _, want := range []string{
		fmt.Sprintf("%d base templates; the %d with the most templates are shown.", maxRows+8, maxRows),
		`<td><a href="?base=base-0#templates">base-0</a></td><td>11</td>`,
		`<input name="base" value="" placeholder="exact value">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	// The three bases with derived templates come first, then solo-000 to
	// solo-196 by name.
	if !strings.Contains(body, ">solo-196<") || strings.Contains(body, ">solo-197<") {
		t.Error("the summary does not keep the largest base templates, then the first by name")
	}
}

// FS-18: a filter of maxRows values is a menu, one of maxRows+1 a text box.
func TestClusterPageMenuBound(t *testing.T) {
	ts := newTestSite(t)
	for pools, menu := range map[int]bool{maxRows: true, maxRows + 1: false} {
		snap := scaledSnapshot(t, fmt.Sprintf("m%d", pools), pools, 1, 3, 0)
		_, body := ts.getAfterWrite(t, snap, fmt.Sprintf("/clusters/proj-a/m%d", pools))
		if got := strings.Contains(body, `<select name="pool">`); got != menu {
			t.Errorf("%d pools: menu %v, want %v", pools, got, menu)
		}
	}
}

// FS-18: an active filter whose value the snapshot does not have stays
// selected in its menu, so it shows and can be cleared; the pool menu also
// names the pools of workers whose WorkerPool object is gone.
func TestClusterPageKeepsActiveFilter(t *testing.T) {
	ts := newTestSite(t)
	snap := scaledSnapshot(t, "k", 3, 6, 6, 0)
	gone := snap.Workers[0]
	gone.Name, gone.Pool = "worker-gone", "gone-pool"
	snap.Workers = append(snap.Workers, gone)

	_, body := ts.getAfterWrite(t, snap, "/clusters/proj-a/k?golden=pending&state=crashed")
	for _, want := range []string{
		// In order among the snapshot's own values.
		`<option value="none">none</option><option value="pending" selected>pending</option><option value="ready">ready</option>`,
		`<option value="crashed" selected>crashed</option>`,
		"9 in the snapshot, 0 match.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}

	_, body = ts.get(t, "/clusters/proj-a/k?pool=ate-env/gone-pool")
	for _, want := range []string{
		`<option value="ate-env/gone-pool" selected>ate-env/gone-pool</option>`,
		`<td class="mono">worker-gone</td>`,
		"7 in the snapshot, 1 match.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if strings.Count(body, `<option value="ate-env/gone-pool"`) != 1 {
		t.Error("the deleted pool is not in the menu exactly once")
	}

	// Without a filter the menu still names the deleted pool.
	_, body = ts.get(t, "/clusters/proj-a/k")
	if !strings.Contains(body, `<option value="ate-env/gone-pool">ate-env/gone-pool</option>`) {
		t.Error("the pool menu lacks the pool of a worker whose WorkerPool is gone")
	}

	// A filter with no values in the snapshot stays a text box.
	for i := range snap.Actors {
		snap.Actors[i].Template = ""
	}
	_, body = ts.getAfterWrite(t, snap, "/clusters/proj-a/k?template=old")
	if !strings.Contains(body, `<input name="template" value="old" placeholder="exact value">`) {
		t.Error("a filter without values in the snapshot is not a text box")
	}
}

// FS-18: the active value counts toward the menu bound: maxRows-1 options and
// an absent value give a menu of maxRows, maxRows options and an absent value
// a text box that keeps the value.
func TestClusterPageActiveFilterMenuBound(t *testing.T) {
	ts := newTestSite(t)
	for pools, menu := range map[int]bool{maxRows - 1: true, maxRows: false} {
		snap := scaledSnapshot(t, fmt.Sprintf("a%d", pools), pools, 1, 3, 0)
		_, body := ts.getAfterWrite(t, snap, fmt.Sprintf("/clusters/proj-a/a%d?pool=ate-env/absent", pools))
		if got := strings.Contains(body, `<option value="ate-env/absent" selected>`); got != menu {
			t.Errorf("%d pools: menu with the absent pool %v, want %v", pools, got, menu)
		}
		if !menu && !strings.Contains(body, `<input name="pool" value="ate-env/absent" placeholder="exact value">`) {
			t.Errorf("%d pools: the text box does not keep the absent pool", pools)
		}
	}
}
