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
	"cmp"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/agent-substrate/env/internal/inventory"
)

// maxRows bounds each list of the Cluster page, and the options of each of its
// filter menus; the JSON API keeps every row.
const maxRows = 200

// clusterParams are the Cluster page's filters; a form keeps the other
// sections' filters as hidden fields in this order.
var clusterParams = [...]string{"atespace", "template", "state", "node", "pool", "base", "golden"}

// maxFleetPools bounds the WorkerPool rows of one cluster on the Fleet page.
const maxFleetPools = 10

// stateOrder is the order of actor states on the pages: live, moving,
// parked, broken.
var stateOrder = []string{
	inventory.ActorStateRunning, inventory.ActorStateResuming, inventory.ActorStateSuspending,
	inventory.ActorStateSuspended, inventory.ActorStateReverting, inventory.ActorStateDeleting,
	inventory.ActorStateCrashed, inventory.ActorStateUnspecified,
}

var templateFuncs = template.FuncMap{
	"age":       formatAge,
	"timestamp": func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	"deref": func(t *time.Time) time.Time {
		if t == nil {
			return time.Time{}
		}
		return *t
	},
	"cpu": func(m int64) string { return fmt.Sprintf("%.1f", float64(m)/1000) },
	"gib": func(b int64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) },
	"clusterURL": func(project, cluster string) string {
		return "/clusters/" + url.PathEscape(project) + "/" + url.PathEscape(cluster)
	},
	"apiClusterURL": func(project, cluster string) string {
		return "/api/v1/clusters/" + url.PathEscape(project) + "/" + url.PathEscape(cluster)
	},
	"actorURL": func(uid string) string { return "/actors/" + url.PathEscape(uid) },
	// A RunEntry's Path is escaped already.
	"runURL":     func(path string) string { return "/runs/" + path },
	"runAPIURL":  func(path string) string { return "/api/v1/runs/" + path },
	"runFileURL": func(path, name string) string { return "/runs/" + path + "/files/" + url.PathEscape(name) },
	"ms":         func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) },
	"sec":        func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) },
}

func formatAge(d time.Duration) string {
	switch {
	case d < -futureSkew:
		return formatAge(-d) + " ahead of the site clock"
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

type stateCount struct {
	State string
	Count int
}

// barSegment is one state's part of a cluster's actor bar, in percent.
type barSegment struct {
	State    string
	Count    int
	X, Width float64
}

// statesOf returns the non-zero counts in stateOrder, then any state this
// site does not know.
func statesOf(byState map[string]int) []stateCount {
	var out []stateCount
	known := map[string]bool{}
	for _, st := range stateOrder {
		known[st] = true
		if n := byState[st]; n > 0 {
			out = append(out, stateCount{st, n})
		}
	}
	var other []string
	for st, n := range byState {
		if !known[st] && n > 0 {
			other = append(other, st)
		}
	}
	sort.Strings(other)
	for _, st := range other {
		out = append(out, stateCount{st, byState[st]})
	}
	return out
}

func bar(states []stateCount, total int) []barSegment {
	if total == 0 {
		return nil
	}
	var out []barSegment
	x := 0.0
	for _, s := range states {
		w := 100 * float64(s.Count) / float64(total)
		out = append(out, barSegment{State: s.State, Count: s.Count, X: x, Width: w})
		x += w
	}
	return out
}

// poolSummary is a cluster's WorkerPools on the Fleet page: the totals of
// all of them and the first maxFleetPools rows, short pools first.
type poolSummary struct {
	Count, Short, More int
	Ready, Desired     int
	Rows               []inventory.WorkerPool
}

// summarizePools orders the pools by missing workers, most first, and keeps
// the snapshot's order among equals, so a short pool is never cut while a
// ready one shows.
func summarizePools(pools []inventory.WorkerPool) poolSummary {
	out := poolSummary{Count: len(pools)}
	for _, p := range pools {
		out.Ready += int(p.ReadyReplicas)
		out.Desired += int(p.Replicas)
		if p.ReadyReplicas < p.Replicas {
			out.Short++
		}
	}
	missing := func(p inventory.WorkerPool) int32 { return max(p.Replicas-p.ReadyReplicas, 0) }
	rows := slices.Clone(pools)
	slices.SortStableFunc(rows, func(a, b inventory.WorkerPool) int {
		return cmp.Compare(missing(b), missing(a))
	})
	if len(rows) > maxFleetPools {
		out.More = len(rows) - maxFleetPools
		rows = rows[:maxFleetPools]
	}
	out.Rows = rows
	return out
}

type fleetRow struct {
	ClusterSummary
	Age    time.Duration
	States []stateCount
	Total  int
	Bar    []barSegment
	Pools  poolSummary
}

func (s *Server) fleetPage(w http.ResponseWriter, r *http.Request) {
	clusters, err := s.cfg.Snapshots.Clusters(r.Context(), force(r))
	if err != nil {
		s.pageError(w, r, http.StatusBadGateway, "The inventory bucket could not be listed.", err)
		return
	}
	now := s.cfg.Now()
	var rows []fleetRow
	var actors, deployments, stale int
	for _, c := range clusters {
		row := fleetRow{ClusterSummary: summarize(c, now), Age: c.Age(now)}
		if c.Snapshot != nil {
			row.States = statesOf(c.Snapshot.Counts.ActorsByState)
			row.Total = len(c.Snapshot.Actors)
			row.Bar = bar(row.States, row.Total)
			row.Pools = summarizePools(c.Snapshot.WorkerPools)
			actors += row.Total
			deployments += len(c.Snapshot.Deployments)
		}
		if row.Stale {
			stale++
		}
		rows = append(rows, row)
	}
	s.render(w, r, http.StatusOK, "fleet.html", map[string]any{
		"Title":       "Fleet",
		"Rows":        rows,
		"Actors":      actors,
		"Deployments": deployments,
		"Stale":       stale,
	})
}

// poolCapacity sums a WorkerPool's workers.
type poolCapacity struct {
	inventory.WorkerPool
	Workers, Active     int
	Capacity, Allocated inventory.Resources
}

// listed is one bounded list of the Cluster page: the first maxRows of the
// Matched rows that pass its filter, of Total in the snapshot.
type listed[T any] struct {
	Rows           []T
	Matched, Total int
}

// Truncated reports whether the page leaves out matching rows.
func (l listed[T]) Truncated() bool { return l.Matched > len(l.Rows) }

// list keeps the first maxRows items that keep accepts.
func list[T any](items []T, keep func(T) bool) listed[T] {
	l := listed[T]{Total: len(items)}
	for _, it := range items {
		if !keep(it) {
			continue
		}
		l.Matched++
		if len(l.Rows) < maxRows {
			l.Rows = append(l.Rows, it)
		}
	}
	return l
}

func all[T any](T) bool { return true }

// filterField is one filter of a Cluster page form: a menu of its Options and
// the active value, or a text box when there are no options or more than
// maxRows of them.
type filterField struct {
	Name, Label, Value string
	Options            []string
}

// queryParam is one filter of another section, which a form keeps as a
// hidden field.
type queryParam struct{ Name, Value string }

type filterForm struct {
	Section string
	Fields  []filterField
	Keep    []queryParam
}

// clusterQuery is the Cluster page's filters.
type clusterQuery url.Values

func (q clusterQuery) get(name string) string { return url.Values(q).Get(name) }

// form returns a form of fields that keeps the filters of the other sections.
func (q clusterQuery) form(section string, fields ...filterField) filterForm {
	f := filterForm{Section: section}
	own := map[string]bool{}
	for _, fd := range fields {
		own[fd.Name] = true
		fd.Value = q.get(fd.Name)
		// An active filter whose value the snapshot no longer has (a
		// failure that cleared, a deleted WorkerPool) stays selected, so it
		// shows and can be cleared. It counts toward the menu bound.
		if fd.Value != "" && len(fd.Options) > 0 && !slices.Contains(fd.Options, fd.Value) {
			fd.Options = append(slices.Clone(fd.Options), fd.Value)
			sort.Strings(fd.Options)
		}
		if len(fd.Options) > maxRows {
			fd.Options = nil
		}
		f.Fields = append(f.Fields, fd)
	}
	for _, name := range clusterParams {
		if v := q.get(name); v != "" && !own[name] {
			f.Keep = append(f.Keep, queryParam{name, v})
		}
	}
	return f
}

// url returns the page's query with each name and value pair of kv set, or
// removed when the value is empty, and section as the fragment.
func (q clusterQuery) url(section string, kv ...string) string {
	v := url.Values{}
	for _, name := range clusterParams {
		if s := q.get(name); s != "" {
			v.Set(name, s)
		}
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			v.Del(kv[i])
		} else {
			v.Set(kv[i], kv[i+1])
		}
	}
	return "?" + v.Encode() + "#" + section
}

func (q clusterQuery) matchActor(a inventory.Actor) bool {
	return match(q.get("atespace"), a.Atespace) && match(q.get("template"), a.Template) &&
		match(q.get("state"), a.State) && match(q.get("node"), a.AssignedNode)
}

func match(filter, v string) bool { return filter == "" || filter == v }

// templateRoot is the base template a template derives from, or the template
// itself when it is a base.
func templateRoot(t inventory.Template) string {
	if t.Base != "" {
		return t.Base
	}
	return t.Name
}

// rootRow counts the templates of one base template by golden state.
type rootRow struct {
	Name   string
	URL    string
	Count  int
	Golden []goldenCount
}

type goldenCount struct {
	State, URL string
	Count      int
}

// templateRoots groups templates by templateRoot across atespaces, largest
// group first, and returns the first maxRows groups and every root's name.
func templateRoots(q clusterQuery, templates []inventory.Template) (listed[rootRow], []string) {
	index := map[string]int{}
	var rows []rootRow
	var golden []map[string]int
	for _, t := range templates {
		root := templateRoot(t)
		i, ok := index[root]
		if !ok {
			i = len(rows)
			index[root] = i
			rows = append(rows, rootRow{Name: root})
			golden = append(golden, map[string]int{})
		}
		rows[i].Count++
		if t.GoldenState != "" {
			golden[i][t.GoldenState]++
		}
	}
	names := make([]string, len(rows))
	for i := range rows {
		names[i] = rows[i].Name
		for _, st := range sortedKeys(golden[i]) {
			rows[i].Golden = append(rows[i].Golden, goldenCount{State: st, Count: golden[i][st]})
		}
	}
	sort.Strings(names)
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		return rows[i].Name < rows[j].Name
	})
	l := listed[rootRow]{Rows: rows[:min(len(rows), maxRows)], Matched: len(rows), Total: len(rows)}
	for i := range l.Rows {
		r := &l.Rows[i]
		r.URL = q.url("templates", "base", r.Name, "golden", "")
		for j := range r.Golden {
			r.Golden[j].URL = q.url("templates", "base", r.Name, "golden", r.Golden[j].State)
		}
	}
	return l, names
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// clusterPool is a Cluster page WorkerPool row, with the link that filters
// the workers to it.
type clusterPool struct {
	poolCapacity
	WorkersURL string
}

func distinct(values func(yield func(string))) []string {
	seen := map[string]bool{}
	var out []string
	values(func(v string) {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	})
	sort.Strings(out)
	return out
}

func (s *Server) clusterPage(w http.ResponseWriter, r *http.Request) {
	project, cluster, ok := pathCluster(r)
	if !ok {
		s.pageError(w, r, http.StatusBadRequest, "The project and the cluster must be one path segment each.", nil)
		return
	}
	c, found, err := s.cfg.Snapshots.Cluster(r.Context(), project, cluster, force(r))
	if err != nil {
		s.pageError(w, r, http.StatusBadGateway, "The inventory bucket could not be listed.", err)
		return
	}
	if !found {
		s.pageError(w, r, http.StatusNotFound, "The bucket has no snapshot for this cluster.", nil)
		return
	}
	now := s.cfg.Now()
	data := map[string]any{
		"Title":   project + "/" + cluster,
		"MaxRows": maxRows,
		"Summary": summarize(c, now),
		"Age":     c.Age(now),
	}
	snap := c.Snapshot
	if snap == nil {
		s.render(w, r, http.StatusOK, "cluster.html", data)
		return
	}
	q := clusterQuery(r.URL.Query())

	pools := make([]clusterPool, len(snap.WorkerPools))
	index := map[string]int{}
	for i, p := range snap.WorkerPools {
		pools[i] = clusterPool{poolCapacity: poolCapacity{WorkerPool: p}, WorkersURL: q.url("workers", "pool", p.Namespace+"/"+p.Name)}
		index[p.Namespace+"\x00"+p.Name] = i
	}
	for _, wk := range snap.Workers {
		i, ok := index[wk.Namespace+"\x00"+wk.Pool]
		if !ok {
			continue
		}
		p := &pools[i].poolCapacity
		p.Workers++
		if wk.State == inventory.WorkerStateActive {
			p.Active++
		}
		p.Capacity.CPUMillicores += wk.Capacity.CPUMillicores
		p.Capacity.MemoryBytes += wk.Capacity.MemoryBytes
		p.Allocated.CPUMillicores += wk.Allocated.CPUMillicores
		p.Allocated.MemoryBytes += wk.Allocated.MemoryBytes
	}

	data["Snapshot"] = snap
	data["States"] = statesOf(snap.Counts.ActorsByState)
	data["Deployments"] = list(snap.Deployments, all)
	data["Pools"] = list(pools, all)
	data["Workers"] = list(snap.Workers, func(wk inventory.Worker) bool {
		return match(q.get("pool"), wk.Namespace+"/"+wk.Pool)
	})
	data["WorkerForm"] = q.form("workers", filterField{Name: "pool", Label: "WorkerPool", Options: distinct(func(y func(string)) {
		for _, p := range snap.WorkerPools {
			y(p.Namespace + "/" + p.Name)
		}
		// Workers can outlive their WorkerPool object while they drain.
		for _, wk := range snap.Workers {
			if wk.Namespace != "" && wk.Pool != "" {
				y(wk.Namespace + "/" + wk.Pool)
			}
		}
	})})
	data["Actors"] = list(snap.Actors, q.matchActor)
	data["ActorForm"] = q.form("actors",
		filterField{Name: "atespace", Label: "Atespace", Options: distinct(func(y func(string)) {
			for _, a := range snap.Atespaces {
				y(a.Name)
			}
		})},
		filterField{Name: "template", Label: "Template", Options: distinct(func(y func(string)) {
			for _, a := range snap.Actors {
				y(a.Template)
			}
		})},
		filterField{Name: "state", Label: "State", Options: distinct(func(y func(string)) {
			for st := range snap.Counts.ActorsByState {
				y(st)
			}
		})},
		filterField{Name: "node", Label: "Node", Options: distinct(func(y func(string)) {
			for _, a := range snap.Actors {
				y(a.AssignedNode)
			}
		})},
	)
	roots, rootNames := templateRoots(q, snap.Templates)
	data["Roots"] = roots
	data["Templates"] = list(snap.Templates, func(t inventory.Template) bool {
		return match(q.get("base"), templateRoot(t)) && match(q.get("golden"), t.GoldenState)
	})
	data["TemplateForm"] = q.form("templates",
		filterField{Name: "base", Label: "Base template", Options: rootNames},
		filterField{Name: "golden", Label: "Golden", Options: distinct(func(y func(string)) {
			for _, t := range snap.Templates {
				y(t.GoldenState)
			}
		})},
	)
	data["Atespaces"] = list(snap.Atespaces, all)
	data["Tags"] = list(snap.Tags, all)
	s.render(w, r, http.StatusOK, "cluster.html", data)
}
