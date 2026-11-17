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
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	bigquery "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/option"
)

// MaxEvents bounds the rows of one actor's events.
const MaxEvents = 1000

// maxBytesBilled bounds what one events query may scan. A query that would
// scan more fails instead of billing it.
const maxBytesBilled = 10 << 30

// eventsTable matches <project>.<dataset>.<table or view>. BigQuery allows
// 1024 characters in a dataset or table name; RE2 caps a repeat at 1000.
var eventsTable = regexp.MustCompile(`^([a-z][a-z0-9-]{4,28}[a-z0-9])\.([A-Za-z0-9_]{1,1000})\.([A-Za-z0-9_]{1,1000})$`)

// eventTime is the column format of time and publish_time: RFC 3339 in UTC
// with the fraction BigQuery holds.
const eventTime = `'%Y-%m-%dT%H:%M:%E*SZ'`

// Event is one actor lifecycle record from the actor_events_v view
// (docs/plans/sandbox-fleet-site/interfaces.md, "Event stream"). A column
// that is NULL is empty.
type Event struct {
	Time             string `json:"time"`
	PublishTime      string `json:"publish_time"`
	Project          string `json:"project"`
	Cluster          string `json:"cluster"`
	Atespace         string `json:"atespace"`
	ActorName        string `json:"actor_name"`
	ActorUID         string `json:"actor_uid"`
	TemplateAtespace string `json:"template_atespace"`
	TemplateName     string `json:"template_name"`
	Operation        string `json:"operation"`
	State            string `json:"state"`
	TraceID          string `json:"trace_id"`
	SpanID           string `json:"span_id"`
}

// eventColumns are the view's columns, in Event's field order.
const eventColumns = "FORMAT_TIMESTAMP(" + eventTime + ", time, 'UTC'), FORMAT_TIMESTAMP(" + eventTime + ", publish_time, 'UTC'), " +
	"project, cluster, atespace, actor_name, actor_uid, template_atespace, template_name, operation, state, trace_id, span_id"

// BigQueryEvents reads actor events from a BigQuery table or view with the
// caller's credentials. It runs read-only queries in the table's project.
type BigQueryEvents struct {
	svc     *bigquery.Service
	project string
	table   string
}

// NewBigQueryEvents returns a reader of table, <project>.<dataset>.<view>.
func NewBigQueryEvents(ctx context.Context, table string, opts ...option.ClientOption) (*BigQueryEvents, error) {
	m := eventsTable.FindStringSubmatch(table)
	if m == nil {
		return nil, fmt.Errorf("events table %q is not <project>.<dataset>.<table>", table)
	}
	svc, err := bigquery.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating the BigQuery client: %w", err)
	}
	return &BigQueryEvents{svc: svc, project: m[1], table: table}, nil
}

// ActorEvents returns the events of the actor with uid published at or
// after since, oldest first: the newest MaxEvents when there are more.
func (b *BigQueryEvents) ActorEvents(ctx context.Context, uid string, since time.Time) ([]Event, error) {
	// The table name is checked by eventsTable; the values are parameters.
	query := "SELECT " + eventColumns + " FROM `" + b.table + "`" +
		" WHERE actor_uid = @uid AND publish_time >= @since ORDER BY time DESC, publish_time DESC" +
		fmt.Sprintf(" LIMIT %d", MaxEvents)
	useLegacy := false
	req := &bigquery.QueryRequest{
		Query:              query,
		UseLegacySql:       &useLegacy,
		ParameterMode:      "NAMED",
		MaximumBytesBilled: maxBytesBilled,
		MaxResults:         MaxEvents,
		TimeoutMs:          10_000,
		QueryParameters: []*bigquery.QueryParameter{
			{Name: "uid", ParameterType: &bigquery.QueryParameterType{Type: "STRING"}, ParameterValue: &bigquery.QueryParameterValue{Value: uid}},
			{Name: "since", ParameterType: &bigquery.QueryParameterType{Type: "TIMESTAMP"}, ParameterValue: &bigquery.QueryParameterValue{Value: since.UTC().Format("2006-01-02 15:04:05.000000+00:00")}},
		},
	}
	resp, err := b.svc.Jobs.Query(b.project, req).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("querying %s: %w", b.table, err)
	}
	// Wait for the job, then read every page up to MaxEvents rows.
	rows, complete, token := resp.Rows, resp.JobComplete, resp.PageToken
	for !complete || (token != "" && len(rows) < MaxEvents) {
		if resp.JobReference == nil {
			return nil, errors.New("the events query did not complete and returned no job")
		}
		call := b.svc.Jobs.GetQueryResults(resp.JobReference.ProjectId, resp.JobReference.JobId).
			Location(resp.JobReference.Location).MaxResults(int64(MaxEvents - len(rows))).TimeoutMs(10_000)
		if complete {
			call = call.PageToken(token)
		}
		res, err := call.Context(ctx).Do()
		if err != nil {
			return nil, fmt.Errorf("reading the events query: %w", err)
		}
		if !complete {
			rows = nil
		}
		rows, complete, token = append(rows, res.Rows...), res.JobComplete, res.PageToken
	}
	out := make([]Event, 0, len(rows))
	for _, row := range rows {
		e, err := eventFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	slices.Reverse(out)
	return out, nil
}

func eventFromRow(row *bigquery.TableRow) (Event, error) {
	var e Event
	fields := []*string{
		&e.Time, &e.PublishTime, &e.Project, &e.Cluster, &e.Atespace, &e.ActorName, &e.ActorUID,
		&e.TemplateAtespace, &e.TemplateName, &e.Operation, &e.State, &e.TraceID, &e.SpanID,
	}
	if len(row.F) != len(fields) {
		return Event{}, fmt.Errorf("an events row has %d columns, want %d", len(row.F), len(fields))
	}
	for i, cell := range row.F {
		switch v := cell.V.(type) {
		case nil:
		case string:
			*fields[i] = v
		default:
			return Event{}, fmt.Errorf("events column %d is %T, want a string", i, cell.V)
		}
	}
	return e, nil
}
