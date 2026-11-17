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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	bigquery "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/option"
)

// fakeBigQuery answers jobs.query, and jobs.getQueryResults when the query
// is not complete at once.
type fakeBigQuery struct {
	mu       sync.Mutex
	queries  []bigquery.QueryRequest
	paths    []string
	complete bool
	rows     []*bigquery.TableRow
	// pages, when set, are the rows of the pages after the first; the query
	// returns rows with a page token.
	pages [][]*bigquery.TableRow
}

func (f *fakeBigQuery) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/projects/central-proj/queries"):
		var q bigquery.QueryRequest
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.queries = append(f.queries, q)
		resp := bigquery.QueryResponse{JobComplete: f.complete, JobReference: &bigquery.JobReference{ProjectId: "central-proj", JobId: "job-1", Location: "US"}}
		if f.complete {
			resp.Rows = f.rows
			if len(f.pages) > 0 {
				resp.PageToken = "p1"
			}
		}
		_ = json.NewEncoder(w).Encode(resp)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/central-proj/queries/job-1"):
		if r.URL.Query().Get("location") != "US" {
			http.Error(w, "no location", http.StatusBadRequest)
			return
		}
		if tok := r.URL.Query().Get("pageToken"); tok != "" {
			var i int
			fmt.Sscanf(tok, "p%d", &i)
			res := bigquery.GetQueryResultsResponse{JobComplete: true, Rows: f.pages[i-1]}
			if i < len(f.pages) {
				res.PageToken = fmt.Sprintf("p%d", i+1)
			}
			_ = json.NewEncoder(w).Encode(res)
			return
		}
		_ = json.NewEncoder(w).Encode(bigquery.GetQueryResultsResponse{JobComplete: true, Rows: f.rows})
	default:
		http.NotFound(w, r)
	}
}

func row(cells ...any) *bigquery.TableRow {
	r := &bigquery.TableRow{}
	for _, c := range cells {
		r.F = append(r.F, &bigquery.TableCell{V: c})
	}
	return r
}

func newTestEvents(t *testing.T, f *fakeBigQuery) *BigQueryEvents {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	b, err := NewBigQueryEvents(context.Background(), "central-proj.fleet.actor_events_v",
		option.WithEndpoint(srv.URL+"/bigquery/v2/"), option.WithoutAuthentication(), option.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// FS-2: the events of one actor come from the view, by a parameterized query.
func TestBigQueryEventsActorEvents(t *testing.T) {
	// The query returns the newest first; the reader returns them oldest
	// first.
	f := &fakeBigQuery{complete: true, rows: []*bigquery.TableRow{
		row("2026-11-16T12:00:09Z", "2026-11-16T12:00:10Z", "proj", "c-1", "ate-env", "env-1", "8f2a0001", "ate-env", "tmpl", "SuspendActor", "suspended", nil, nil),
		row("2026-11-16T12:00:01.5Z", "2026-11-16T12:00:02Z", "proj", "c-1", "ate-env", "env-1", "8f2a0001", "ate-env", "tmpl", "ResumeActor", "running", "0af7651916cd43dd8448eb211c80319c", "b7ad6b7169203331"),
	}}
	b := newTestEvents(t, f)
	since := time.Date(2026, 11, 16, 11, 0, 0, 0, time.FixedZone("x", 3600))
	events, err := b.ActorEvents(context.Background(), "8f2a0001", since)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].State != "running" || events[0].TraceID != "0af7651916cd43dd8448eb211c80319c" || events[1].Operation != "SuspendActor" || events[1].TraceID != "" {
		t.Errorf("events = %+v", events)
	}

	q := f.queries[0]
	if !strings.Contains(q.Query, "FROM `central-proj.fleet.actor_events_v`") || !strings.Contains(q.Query, "actor_uid = @uid") || !strings.Contains(q.Query, fmt.Sprintf("LIMIT %d", MaxEvents)) || !strings.Contains(q.Query, "ORDER BY time DESC") || strings.Contains(q.Query, "8f2a0001") {
		t.Errorf("query = %s", q.Query)
	}
	if q.UseLegacySql == nil || *q.UseLegacySql || q.ParameterMode != "NAMED" || q.MaximumBytesBilled != maxBytesBilled {
		t.Errorf("query options = legacy %v, mode %q, max bytes %d", q.UseLegacySql, q.ParameterMode, q.MaximumBytesBilled)
	}
	params := map[string]string{}
	for _, p := range q.QueryParameters {
		params[p.Name] = p.ParameterType.Type + " " + p.ParameterValue.Value
	}
	if params["uid"] != "STRING 8f2a0001" || params["since"] != "TIMESTAMP 2026-11-16 10:00:00.000000+00:00" {
		t.Errorf("parameters = %v", params)
	}
}

// A result on several pages is read to the end.
func TestBigQueryEventsReadsEveryPage(t *testing.T) {
	ev := func(ts string) *bigquery.TableRow {
		return row(ts, ts, "proj", "c-1", "ate-env", "env-1", "u", "ate-env", "tmpl", "op", "running", nil, nil)
	}
	f := &fakeBigQuery{complete: true,
		rows:  []*bigquery.TableRow{ev("2026-11-16T12:00:03Z")},
		pages: [][]*bigquery.TableRow{{ev("2026-11-16T12:00:02Z")}, {ev("2026-11-16T12:00:01Z")}},
	}
	events, err := newTestEvents(t, f).ActorEvents(context.Background(), "u", t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Time != "2026-11-16T12:00:01Z" || events[2].Time != "2026-11-16T12:00:03Z" {
		t.Errorf("events = %+v, want the three pages, oldest first", events)
	}
}

func TestBigQueryEventsWaitsForTheJob(t *testing.T) {
	f := &fakeBigQuery{complete: false, rows: []*bigquery.TableRow{
		row("2026-11-16T12:00:01Z", "2026-11-16T12:00:02Z", "proj", "c-1", "ate-env", "env-1", "u", "ate-env", "tmpl", "CreateActor", "running", nil, nil),
	}}
	events, err := newTestEvents(t, f).ActorEvents(context.Background(), "u", t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || len(f.paths) != 2 {
		t.Errorf("events %+v after requests %v, want one event after a query and a wait", events, f.paths)
	}
}

func TestBigQueryEventsRejectsABadRow(t *testing.T) {
	for _, r := range []*bigquery.TableRow{row("only", "three", "cells"), row(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13)} {
		f := &fakeBigQuery{complete: true, rows: []*bigquery.TableRow{r}}
		if _, err := newTestEvents(t, f).ActorEvents(context.Background(), "u", t0); err == nil {
			t.Errorf("row %v accepted", r)
		}
	}
}

func TestNewBigQueryEventsChecksTheTable(t *testing.T) {
	for _, table := range []string{"", "fleet.actor_events_v", "p.fleet.actor_events_v", "central-proj.fleet.actor_events_v`; DROP TABLE x; --", "central-proj.fleet.a-b", "central-proj.fleet.actor_events_v.x"} {
		if _, err := NewBigQueryEvents(context.Background(), table, option.WithoutAuthentication()); err == nil {
			t.Errorf("table %q accepted", table)
		}
	}
}
