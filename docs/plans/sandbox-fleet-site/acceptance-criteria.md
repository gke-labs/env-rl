# Acceptance criteria: sandbox fleet site

Status: accepted (#223); last checked 2026-11-26 against `9181b67`. Each criterion has a command and
an expected result. Evidence goes into `evidence/` in this directory, one file per acceptance pass,
with the commit of each component.

| ID    | Title                                    | Phase |
| ----- | ---------------------------------------- | ----- |
| FS-1  | Inventory freshness within one interval  | 3     |
| FS-2  | Transition visible within seconds        | 2, 7  |
| FS-3  | Read-only enforced by IAM                | 1, 5  |
| FS-4  | Two clusters, two projects, side by side | 8     |
| FS-5  | One trace from the trainer to the worker | 4     |
| FS-6  | Metrics at 5 s resolution                | 4     |
| FS-7  | IAP gate                                 | 1, 5  |
| FS-8  | No actor identity in metric labels       | 4     |
| FS-9  | Stale reporter is visible                | 3, 5  |
| FS-10 | Schema and secrets                       | 3     |
| FS-11 | Run artifacts resolve by run id          | 6     |
| FS-12 | Documentation                            | 8     |
| FS-13 | Plan drift check against main            | each  |
| FS-14 | Reporter against the Kind Substrate      | 3     |
| FS-15 | Reporter tick telemetry                  | 3, 4  |
| FS-16 | Target identity on ate-env-api spans     | 9     |
| FS-17 | Fleet page at scale                      | 5     |
| FS-18 | Cluster page at scale                    | 4     |
| FS-19 | Run trace is readable                    | 6     |

## FS-1: Inventory freshness

- Command: resize a WorkerPool or create one environment with the Python client; sample
  `gsutil stat gs://<bucket>/v1/inventory/<p>/<c>/latest.json` every second and
  `GET /api/v1/clusters/<p>/<c>`.
- Expected: the change is in `latest.json` within `interval + 2 s` and on the site within another 30
  s cache window or immediately with `?nocache=1`.
- Pass rule: ten trials, all within budget.

## FS-2: Transition latency

- Command: suspend and resume one environment with the client while recording the wall-clock time;
  query `fleet.actor_events` for the actor uid; read `/api/v1/actors/<uid>/events`.
- Expected: Logging path: median under 10 s, 99th percentile under 60 s over 100 transitions. OTLP
  path (phase 7): median under 5 s. The record's `state` and `operation` match the client's calls,
  and `trace_id` equals the client span's trace id when tracing is on. The events query of one
  request reports a `totalBytesProcessed` of the asked days' partitions, not the whole table (record
  it from the job in the evidence).
- Pass rule: the percentiles hold and no transition is missing against the client's log.

## FS-3: Read-only

- Command: `gcloud projects get-iam-policy` and `gsutil iam get` for the site's service account;
  `gcloud run services describe` for its identity; an attempt from the site's identity to write an
  object and to call `ateapi`.
- Expected: viewer roles only; the write returns 403; no `ateapi` endpoint or token is configured in
  the service.
- Pass rule: all three hold.

## FS-4: Multi-cluster

- Command: deploy the reporter and collector in a second cluster in a second project; open `/`.
- Expected: both clusters listed with distinct project and cluster labels, their own deployments and
  counts; `actor_events` rows carry the right project and cluster.
- Pass rule: no field from one cluster attributed to the other.

## FS-5: Trace join

- Command: run the mock-free quickstart with `ObservabilityConfig(enable_tracing=True)` against a
  cluster whose Substrate exports to Cloud Trace; open the trace by id in Cloud Trace; read the span
  file under `v1/traces/`.
- Expected: one trace id spans `ate_env_fleet.run`, `ate_env_client.create_environment`,
  `ateenv.v1alpha.EnvironmentService/CreateEnvironment`, `ateapi.Control/*`, `step.*` workflow
  spans, atelet `Restore` and ateworker spans with `ate.otlp.relay=relay`. The GCS file holds the
  client and `ate-env-api` spans of the same trace id.
- Pass rule: the chain is unbroken in Cloud Trace; the file has the ate-env half.

## FS-6: Metrics resolution

- Command: PodMonitoring with `interval: 5s`; after five minutes,
  `count_over_time(ate_env_api_requests_total[1m])` through the PromQL API.
- Expected: at least 10 samples per minute per series; the `environments` named query
  (`GET /api/v1/metrics/environments?window=15m` on the site) returns per-cluster, per-atespace
  counts, which checks the `PodMonitoring` scrape path; and the `workers` named query
  (`GET /api/v1/metrics/workers?window=15m`) returns per-cluster, per-pool counts that are not
  empty, which checks the OTLP path through GKE Managed OpenTelemetry and the dotted names.
- Pass rule: holds for every `ate-env-api` in both clusters.

## FS-7: IAP

- Command: `curl -i https://<site>/` without credentials; the same with an identity token of a group
  member and of a non-member.
- Expected: redirect or 403 without credentials; 200 for the member; 403 for the non-member; the
  Fleet page shows the member's email from the IAP assertion.
- Pass rule: all four.

## FS-8: Cardinality

- Command: list label names of every `ate_env_*`, `ate_actor_*`, `ate_scheduler_*` and
  `ate_workerpool_*` series in Cloud Monitoring.
- Expected: no label named after an actor, a run id, an environment id or an image tag.
- Pass rule: holds.

## FS-9: Stale reporter

- Command: scale the reporter to zero; wait three intervals; open `/`.
- Expected: the cluster shows a stale badge with the age of `generated_at`; the API returns
  `"stale": true`; scaling back clears it within one interval plus the 30 s cache period, or at once
  with `?nocache=1`.
- Pass rule: holds.

## FS-10: Schema and secrets

- Command: `go test ./internal/inventory/...` and a grep over a day of snapshots and events for
  `token`, `password`, `authorization`, `ya29.`, `AIza`.
- Expected: the golden file matches; zero hits.
- Pass rule: holds.

## FS-11: Runs

- Command: three steps for the trainer source, because `examples/grpo/grpo_substrate_train.py` alone
  writes only the RunReport pair, `<strategy>_<n>tasks_<stamp>.txt` and `.json` (`RunReport.write`),
  and no span file. (1) Run the example with `REPORT_DIR=/tmp/fs11`, `ENABLE_TRACING=1` and
  `OTEL_EXPORTER_OTLP_ENDPOINT` at a collector whose traces pipeline has the `file` exporter of
  `tests/telemetry/otel-collector.yaml`, then copy its `/data/spans.jsonl` next to the reports, as
  the telemetry plan's acceptance pass does (the Python client exports over OTLP only and writes no
  local span file). (2)
  `python benchmarking/telemetry_report/build_report.py --report /tmp/fs11 --spans /tmp/fs11/spans.jsonl --title "$RUN_ID" --out /tmp/fs11`,
  which adds `run.json` and `report.html`. (3)
  `gcloud storage cp /tmp/fs11/* "gs://$BUCKET/v1/runs/$RUN_ID/"`, where `RUN_ID` is the example's
  run id. The fixture sets no `gcp.project_id` on the spans, so the site runs with
  `FLEET_TRACE_PROJECT` set to the project the fixture forwards the trace to. For the benchmark
  source, `bench upload` one validated bundle into a bucket named in `FLEET_BENCH_BUCKETS`. Open
  `/runs/trainer/$RUN_ID` and `/runs/bench/<bucket>/<bundle folder>`.
- Expected: the run report table shows, and the HTML report and the span file download; the Cloud
  Trace link opens the trace id of the `ate_env_fleet.run` span, which the site reads from
  `spans.jsonl` (neither the RunReport nor the example records a trace id), in the project of the
  run span's `gcp.project_id` resource attribute or `FLEET_TRACE_PROJECT`; the waterfall renders
  from the span file; the bundle page shows `run_window.json`, `report.json` and
  `system_description.json` and marks a `_traced` folder as traced.
- Pass rule: holds for both sources.

## FS-12: Documentation

- Expected: `docs/fleet-site.md` covers deployment per cluster and central, the IAM matrix, the
  bucket layout, the freshness budget and the tenancy rule; `CHANGELOG.md` lists the reporter, the
  manifests and the site.
- Pass rule: present and checked against the code in the final pull request.

## FS-13: Plan drift check

- Command: the seven steps of the README's "Drift check" section against the `main` the pull request
  targets.
- Expected: each step holds, or the plan is updated in the same pull request; the audit section of
  `research.md` records the commit and the date.
- Pass rule: the last-checked lines of the six files that carry one, `README.md`,
  `execution-plan.md`, `acceptance-criteria.md`, `interfaces.md`, `prd.md` and `quickstart.md`, name
  one commit, and that commit is the pull request's merge-base with `main` or the `main` head
  recorded in `research.md` on the audit date. A merge to `main` after the audit does not fail the
  check; it is the cue to run it again.

## FS-14: Reporter against the Kind Substrate

- Command: in `hack/ci/e2e.py`, after the manifests are applied, run
  `ate-env-reporter -out-dir /tmp/snap -project kind -cluster "$KIND_CLUSTER_NAME" -interval 5s -snapshot-interval 5s`
  for three ticks against the Kind `ateapi` with the reporter's member merged into the global
  `viewer` binding. Kind has no GKE metadata server, so `-project` and `-cluster` are explicit
  (`DecodeSnapshot` rejects a snapshot without them); `-out-dir` stands in for `-bucket`; the 5 s
  snapshot interval keeps one timestamped file per tick.
- Expected: `/tmp/snap/v1/inventory/kind/$KIND_CLUSTER_NAME/latest.json` and three files under
  `/tmp/snap/v1/snapshots/kind/$KIND_CLUSTER_NAME/` decode with `DecodeSnapshot`; the actor and
  worker counts equal `kubectl ate get actor -A` and `kubectl ate get worker`; `generated_at`
  advances from one snapshot file to the next.
- In the same case, before the run on the host: the Deployment of `ate-env manifest reporter`, with
  `--out-dir`, turns ready in the cluster. `/readyz` allows that only after a snapshot was written,
  so the ServiceAccount's token, the `viewer` binding and the ClusterRole hold. The run on the host
  uses a token of the same ServiceAccount and `TestE2ESnapshotDir` checks its files.
- Pass rule: holds in the `E2E Kind and Smoke Test` job.

## FS-15: Reporter tick telemetry

Added by pull request 2c.

- Command:
  `go test -race ./internal/reporter ./internal/telemetry ./cmd/ate-env-reporter ./cmd/ate-env` with
  `TestRunTracesAndReportsEachTick`, `TestRunReportsATimedOutTickButNotOneCutByShutdown`,
  `TestTickSpanParentsTheControlPlaneCalls`, `TestReporterMetrics`,
  `TestRunWritesSnapshotsOverGRPC`, `TestClientConfigWiresTelemetry`,
  `TestManifestReporterTelemetry` and `TestManifestReporterRejects`.
- Expected: with `-metrics-listen-addr`, `/metrics` serves `ate_env_reporter_poll_duration_seconds`
  with `status` `ok` or `error` and counts a tick that wrote its snapshot as `ok`; a tick that times
  out counts as `error`, and one that shutdown cuts short is not counted. With `-enable-tracing`,
  each tick is one root span `ate-env-reporter.tick`, with an error status when it fails, and the
  `ateapi` calls of the tick, over the client `ate.New` builds with `EnableTracing`, are its
  children. `ate-env manifest reporter --metrics-port <port> --pod-monitoring` renders
  `-metrics-listen-addr=:<port>`, a `metrics` container port and a `PodMonitoring` whose selector
  equals the reporter pods' labels and whose one endpoint scrapes `metrics` at `/metrics` every 5 s;
  `--pod-monitoring` without `--metrics-port`, `--metrics-port` on the health port, and
  `--otlp-endpoint` or `--otlp-insecure` without `--enable-tracing` are refused.
- Pass rule: the named tests pass in the Go CI job.

## FS-16: Target identity on ate-env-api spans

Added by pull request 7.

- Command:
  `go test -race -run 'TestServerSpansCarry|TestSetSpanActorAttributesKeepsKnownValues' ./internal/apiservice`,
  which runs `TestServerSpansCarryEnvironmentIdentity`, `TestServerSpansCarryGuestCallIdentity`,
  `TestServerSpansCarryTemplateIdentity`, `TestServerSpansCarryNoToken` and
  `TestSetSpanActorAttributesKeepsKnownValues`.
- Expected: with tracing on, each `ate-env-api` server span carries the identity its call targets,
  under the `ateattr` keys `ateapi` stamps on its actor RPCs. `ate.atespace` and `ate.actor.name`
  (the environment id) go on the environment and the eight guest calls, also failed ones. Once
  `CreateEnvironment` or `GetEnvironment` has the actor, `ate.actor.uid`, `ate.actor.version` (an
  int64), `ate.template.name` and `ate.template.atespace` join them. `ListEnvironments` carries
  `ate.atespace`. The template calls carry `ate.atespace`, `ate.template.name` and
  `ate.template.atespace`, with the derived template once its name is known. `GetServerInfo` and a
  `ListPlacementDomains` that names no template carry no `ate.*` attribute. A name Substrate would
  refuse is not stamped. No span attribute, status or event carries the caller's bearer token.
- Pass rule: the named tests pass in the Go CI job, and in a Cloud Trace query on `ate.actor.name`
  an environment's `ate-env-api` and `ateapi` spans come up together.

## FS-17: Fleet page at scale

Added by pull request 4b.

- Command:
  `go test -race -run 'TestFleetPageSummarizesWorkerPools|TestSummarizePoolsBounds|TestSiteColumnsDoNotStretch' ./internal/fleetsite`;
  then serve a snapshot of a cluster with more than ten WorkerPools and three `ate-env-api`
  deployments with `ate-env-fleet-site -dir` and open `/` in a browser.
- Expected: each cluster's WorkerPools table names the number of pools, the sum of ready and desired
  workers and, when some pool has fewer ready than desired workers, how many are short. A cluster
  without pools shows only "WorkerPools". The table lists at most ten pools, the ones missing the
  most workers first and the snapshot's order among equals, and links the rest as "N more on the
  cluster page" to the Cluster page. Tables side by side keep their own height, so every deployment
  row shows right under the table's header.
- Pass rule: the named tests pass in the Go CI job, and in the browser the deployments table is no
  taller than its rows.

## FS-18: Cluster page at scale

Added by pull request 4c.

- Command: `go test -race -run '^TestClusterPage' ./internal/fleetsite`, which runs
  `TestClusterPage`, `TestClusterPageBoundsEveryList`, `TestClusterPageFilters`,
  `TestClusterPageBoundsTemplateSummary`, `TestClusterPageMenuBound`,
  `TestClusterPageEscapesFilterValues` and `TestClusterPageNodeFilterAndRowLimit`; then open
  `/clusters/<p>/<c>` for the largest cluster in headless Chrome.
- Expected: each list of the Cluster page (deployments, WorkerPools, workers, actors, templates,
  atespaces and tags) shows at most 200 rows and says how many the snapshot has, how many match its
  filter, and that only the first 200 are shown. Templates are summarized by base template, a
  derived template under its `base` and a base template under itself, in every atespace, with counts
  by golden state; the 200 base templates with the most templates are listed, and each links to the
  template list filtered by `base` and `golden`. `?pool=<namespace>/<name>` filters the workers, and
  each WorkerPool links to its workers. Each form and link keeps the filters of the other sections.
  A filter with more than 200 values is a text box instead of a menu. Snapshot text and query values
  are escaped in links and hidden fields. For a snapshot with thousands of templates, tags and
  workers, the page is under 1 MiB, and `GET /api/v1/clusters/<p>/<c>` still returns every row.
- Pass rule: the named tests pass in the Go CI job, and the page of the largest cluster loads in
  headless Chrome in under 1 s.

## FS-19: Run trace is readable

Added by pull request 5c.

- Command:
  `go test -race -run 'TestSpanLabel|TestBarLabel|TestColorByService|TestRunPageWaterfallLabels|TestRunPageLegendWithoutErrors|TestBarOf' ./internal/fleetsite`;
  then open `/runs/trainer/<id>` for a run with a few hundred spans.
- Expected: each bar of the waterfall carries its span name after the last slash (the method of an
  RPC name), at most 40 monospace cells and ending in an ellipsis when cut, with the full name, the
  service when the span has one, the duration, the offset and "error" in its title. The label sits
  inside the bar when it fits, else right of the bar when it fits there, else ending left of it.
  Bars are colored by service in the order the services first appear, from six colors, with a
  legend. An error bar is hatched red with a dark outline, and its label starts with a cross; its
  row in the span table says "(error)". A service with only error bars gets no color. Span names are
  escaped. The SVG scales without distortion between 640 and 960 px, so labels keep their fit, and
  scrolls sideways below 640 px. The bar caps of `ParseRunTrace` do not change.
- Pass rule: the named tests pass in the Go CI job, and in the page of the run every bar's label can
  be read without hovering.
