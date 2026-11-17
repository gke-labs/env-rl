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
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/agent-substrate/env/internal/inventory"
)

// maxActorRows bounds the actor rows of one Cluster page.
const maxActorRows = 1000

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
	"eventsURL": func(uid string) string { return "/api/v1/actors/" + url.PathEscape(uid) + "/events" },
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

type fleetRow struct {
	ClusterSummary
	Age    time.Duration
	States []stateCount
	Total  int
	Bar    []barSegment
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

type actorFilter struct {
	Atespace, Template, State, Node string
}

func (f actorFilter) match(a inventory.Actor) bool {
	return (f.Atespace == "" || a.Atespace == f.Atespace) &&
		(f.Template == "" || a.Template == f.Template) &&
		(f.State == "" || a.State == f.State) &&
		(f.Node == "" || a.AssignedNode == f.Node)
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
		"Summary": summarize(c, now),
		"Age":     c.Age(now),
	}
	snap := c.Snapshot
	if snap == nil {
		s.render(w, r, http.StatusOK, "cluster.html", data)
		return
	}

	pools := make([]poolCapacity, len(snap.WorkerPools))
	index := map[string]int{}
	for i, p := range snap.WorkerPools {
		pools[i] = poolCapacity{WorkerPool: p}
		index[p.Namespace+"\x00"+p.Name] = i
	}
	for _, wk := range snap.Workers {
		i, ok := index[wk.Namespace+"\x00"+wk.Pool]
		if !ok {
			continue
		}
		p := &pools[i]
		p.Workers++
		if wk.State == inventory.WorkerStateActive {
			p.Active++
		}
		p.Capacity.CPUMillicores += wk.Capacity.CPUMillicores
		p.Capacity.MemoryBytes += wk.Capacity.MemoryBytes
		p.Allocated.CPUMillicores += wk.Allocated.CPUMillicores
		p.Allocated.MemoryBytes += wk.Allocated.MemoryBytes
	}

	q := r.URL.Query()
	filter := actorFilter{Atespace: q.Get("atespace"), Template: q.Get("template"), State: q.Get("state"), Node: q.Get("node")}
	var actors []inventory.Actor
	matched := 0
	for _, a := range snap.Actors {
		if !filter.match(a) {
			continue
		}
		matched++
		if len(actors) < maxActorRows {
			actors = append(actors, a)
		}
	}

	data["Snapshot"] = snap
	data["States"] = statesOf(snap.Counts.ActorsByState)
	data["Pools"] = pools
	data["Filter"] = filter
	data["Actors"] = actors
	data["Matched"] = matched
	data["Truncated"] = matched > len(actors)
	data["AtespaceOptions"] = distinct(func(y func(string)) {
		for _, a := range snap.Atespaces {
			y(a.Name)
		}
	})
	data["TemplateOptions"] = distinct(func(y func(string)) {
		for _, a := range snap.Actors {
			y(a.Template)
		}
	})
	data["StateOptions"] = distinct(func(y func(string)) {
		for st := range snap.Counts.ActorsByState {
			y(st)
		}
	})
	data["NodeOptions"] = distinct(func(y func(string)) {
		for _, a := range snap.Actors {
			y(a.AssignedNode)
		}
	})
	s.render(w, r, http.StatusOK, "cluster.html", data)
}
