# Decisions and deviations log

Append-only. Each entry: date, who, what, why.

- 2026-10-29, owner: the goal is one central website for all users of the clusters, always current
  on every ate-env deployment across all namespaces and all clusters. The per-run explorer plan
  (`plans/rl-perf-explorer/`) does not cover deployment, multi-cluster, auth or tenancy; its
  Sandboxes view and restore models carry over as a page of the new site.
- 2026-10-30, owner: read-only. No action from the site reaches a cluster.
- 2026-10-30, owner: the site runs on Cloud Run. Consequences: no long-running poller in the site;
  subscriptions and Eventarc replace polling; clusters push, the site reads.
- 2026-10-30, owner and orchestrator: GCS is the one store the project owns, for inventory
  snapshots, event archive, raw spans and run artifacts. Time series stay in Cloud Monitoring and
  traces in Cloud Trace, because re-aggregating scraped files into charts or indexing millions of
  span lines would re-implement Prometheus and a trace backend.
- 2026-10-30, orchestrator: events are queried from BigQuery, filled by a Pub/Sub BigQuery
  subscription, with a Cloud Storage subscription as the archive. GCS alone cannot append and
  batches files on a window of at least a minute, which misses the seconds target for the live feed.
  The owner can reverse this to GCS-only at the cost of a minute of event latency.
- 2026-10-30, orchestrator: inventory is not derived from traces. Tracing is opt-in and off by
  default, idle and suspended actors emit no spans, and WorkerPools, templates and deployments have
  none. Traces stay the drilldown.
- 2026-10-30, orchestrator, from research: the source of transitions is Substrate's actor event
  stream (`Actor state changed`, `Actor crashed`), one record per committed transition with the
  actor uid, the operation, the state and the trace id. Phase 1 reads it from pod stdout through a
  Cloud Logging sink, which needs no Substrate change; phase 2 switches `ate-otel-config` to
  `OTEL_LOGS_EXPORTER=otlp,console` and routes by `event.name` in the collector. The Substrate
  document's rules apply: never sample or filter the stream; take the last record per uid; reconcile
  with the control plane.
- 2026-10-30, orchestrator, from research: the reporter stays, for current state and for Kubernetes
  objects. `ateapi` has no watch RPC; `ListActors` across atespaces paged at 1000 every 5 s is the
  poll. It needs a global AccessPolicy with the `viewer` role, which exists in `ateapi`'s authz next
  to `editor` and `owner`.
- 2026-10-30, orchestrator, from research: the trace join through Substrate needs no new mechanism.
  The router ingress extracts `traceparent` and resumes under it; `ateapi` stamps actor identity on
  its server span and opens a span per workflow step; atelet and ateworker continue it. This closes
  the telemetry plan's open question.
- 2026-10-30, orchestrator: push reporters per cluster rather than a central poller, so the
  internet-facing service holds no cluster credentials.
- 2026-10-30, orchestrator: a thin custom Go service rather than Grafana. Grafana needs a database
  and does not fit Cloud Run; depth links go to Cloud Monitoring dashboards.
- 2026-10-30, orchestrator, tentative: reporter, schema, site, manifests and Terraform live in
  `gke-labs/env-rl` because the reporter reuses `internal/ate` and the site reuses the schema
  package. The alternative is a separate repository; the maintainers decide at the first pull
  request.
- 2026-10-30, orchestrator, tentative: the site is Go with the standard library, server-rendered
  pages, inline SVG, no external assets, in the style of the explorer plan and `build_report.py`.
- 2026-10-30, open: the central project, the bucket name, the Google group, the cluster list,
  whether Cloud Trace is enabled in each cluster project, and whether every user sees every
  namespace.
- 2026-11-02, orchestrator: the plan is proposed to `gke-labs/env-rl` under
  `docs/plans/sandbox-fleet-site/`, as the telemetry plan was in #189, so maintainers can object to
  the design before the first code pull request. The notes repository keeps the 2026-10-30 draft.
- 2026-11-03, orchestrator: the schema package is `internal/inventory`, not the planned
  `internal/fleet/v1`. "Fleet" already names the Python `SandboxFleet` and the trainer-side
  `ate_env_fleet_*` series; the snapshot is an inventory of a cluster. The package landed as pull
  request 1 of the execution plan.
- 2026-11-05, orchestrator, from sunnylovestiramisu's review of #223: the reporter's grant is
  `roles/storage.objectUser` under an IAM condition for its own two prefixes, because an overwrite
  of `latest.json` needs `storage.objects.delete`, which `objectCreator` lacks. The reporter
  rewrites `latest.json` on every tick, so the object generation moves with `generated_at` and the
  site's revalidation holds; the metadata-only write is gone. The alternative, keeping
  `objectCreator` and writing an immutable object per tick that the site finds by listing, was
  rejected: at a 5 s interval that is 17,280 objects per cluster per day, a list per cluster on
  every request and a lifecycle rule under `v1/inventory/`, where the plan keeps one `latest.json`
  per cluster with the hourly snapshots as its history. The cost of `objectUser` is that the
  reporter can also delete its own cluster's snapshots. The condition limits that to its two
  prefixes, the event archive is outside them, and the Terraform can replace `objectUser` with a
  custom role of `storage.objects.create` and `storage.objects.delete`.
- 2026-11-05, orchestrator, same review: the BigQuery subscription uses no topic schema. `data` is a
  JSON column with the raw payload, and the typed columns are a view projected with `JSON_VALUE`,
  because a topic schema maps top-level fields only and cannot flatten a `LogEntry` or an OTLP
  record.
- 2026-11-05, orchestrator, same review: in phase 2 the lifecycle records leave `ate-api-server`
  through `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` set on that Deployment, pointing at the self-managed
  collector. The managed collector does not export logs to Pub/Sub, and the shared `ate-otel-config`
  cannot carry a differing per-signal endpoint without breaking atelet's relay.
- 2026-11-09, orchestrator: audit of `main` at `f10c68c` (research.md, "Audit of main"). The plan's
  assumptions hold; the status lines now carry a last-checked commit, the README has a drift-check
  procedure, and FS-13 records each run of it, following the convention the other plans under
  `docs/plans/` adopted.
- 2026-11-09, orchestrator: phase 0's question about the Substrate `viewer` role is answered by
  #235: `ate-env manifest access-policy --global` already grants `ate-env-api` the global `viewer`
  binding for `ListPlacementDomains`, and the README states the merge rule. The reporter's principal
  joins that binding; it never creates a second global policy.
- 2026-11-09, orchestrator: the Runs page indexes the benchmark bundles `bench upload` writes
  (#270), by their validated folder name, next to the trainers' `REPORT_DIR` output. The site reads
  them from the buckets in `FLEET_BENCH_BUCKETS` with a viewer role only.
- 2026-11-09, orchestrator: schema v1 gains `workers[].labels` and `actors[].worker_selector` in
  pull request 2, additive, with the Go `Worker` and `Actor` structs in
  `internal/inventory/inventory.go`, `internal/inventory/v1/snapshot.schema.json` and the golden
  fixture updated together, because `DecodeSnapshot` disallows unknown fields before it validates
  against the schema. Placement domains are derived from worker labels as `ListPlacementDomains`
  derives them, so the site needs no new RPC.
- 2026-11-09, orchestrator: the reporter's acceptance runs in two places: unit tests against
  `fakecontrol`, which pull request 2 extends with the three list calls it lacks, and one case in
  the Kind end-to-end job against the real `ateapi` with `-out-dir` (FS-14).
- 2026-11-10, orchestrator: pull request 2 is two pull requests. 2 is the binary:
  `internal/reporter`, `cmd/ate-env-reporter`, the schema additions and the `fakecontrol` list
  calls. 2b is `ate-env manifest reporter` and the Kind end-to-end case (FS-14). The binary is
  reviewable without its deployment, and the repo asks for one change per pull request.
- 2026-11-10, orchestrator: the reporter's control-plane flags are `ate-env-api`'s own: `-ateapi`,
  `-ateapi-token-file` and `-skip-verify`. The planned `-ateapi-endpoint` and `-ateapi-ca-file` do
  not exist on `ate-env-api`, and the reporter was to match it. Kept snapshots are named
  `<YYYYMMDD>T<HHMMSS>Z.json`, a name without colons. `/readyz` answers 200 while the last tick
  wrote its snapshot.
- 2026-11-10, orchestrator: three planned parts of the reporter are left out of pull request 2. The
  diff on `uid` and `update_time`: each tick lists every actor and rewrites the whole object, so a
  diff saves only the conversion of the listed actors, and no measurement asks for it.
  `-enable-tracing` and `-metrics-listen-addr`: `docs/metrics.md` is generated from `ate-env-api`'s
  definitions, a reporter family needs its own section there, and nothing scrapes the reporter
  before the telemetry manifests of pull request 3, which is where they go. The ClusterRole rules
  for `sandboxconfigs.ate.dev`, `services` and `pods`: the reporter reads WorkerPools and
  Deployments only.
- 2026-11-10, orchestrator, from the code: `actors[].labels` has no source, because Substrate's
  `ResourceMetadata` carries no labels at the version `go.mod` requires; the field stays in the
  schema and stays empty. The rule "labels pass through except keys under `ate.`" is replaced by the
  schema's own rule: a label or selector key that names a credential is refused by both validators,
  and the reporter leaves the key out and logs it once, so one such key does not stop a cluster's
  snapshots.
- 2026-11-10, orchestrator: drift check against `main` at `3dd8bab2` (research.md, "Audit of main,
  2026-11-10"). The seven steps hold. Step 4 named a test, `TestFS13DriftCheckStep4`, that no
  package has; the README no longer cites it.
- 2026-11-10, orchestrator, from the review of pull request 2: the reporter lists WorkerPools and
  Deployments on each tick and uses no informer. An informer whose watch dies keeps serving its last
  cache without an error, so a snapshot would carry old Kubernetes state under a new `generated_at`.
  A list that fails fails the tick, which `/readyz` and the stale badge show. The cost is two lists
  per tick from the API server's cache, and the ClusterRole needs `list` only.
- 2026-11-10, orchestrator: FS-14 runs the reporter twice in the Kind job. In the cluster, from
  `ate-env manifest reporter`, where readiness proves its identity end to end; the image has no
  shell, so its files are not read. On the host, with `-out-dir` and a token of the same
  ServiceAccount, where `TestE2ESnapshotDir` decodes the files and compares the counts. The
  manifest's Deployment uses the `Recreate` strategy: `latest.json` has one writer.
- 2026-11-16, orchestrator: drift check against `main` at `66b0e526` (research.md, "Audit of main,
  2026-11-16"). The seven steps hold; open #392 (`ListEnvironments`) does not change the reporter.
- 2026-11-16, orchestrator, pull request 3: the collector is `ate-env manifest collector`, not a
  static `manifests/telemetry/collector.yaml`. The bucket and the project are inputs, the reporter's
  manifest is rendered the same way, a test checks the rendered pipeline, and a new top-level
  directory would need an entry in `hack/verify/layers.py`. The namespace and the names are
  `ate-env-collector`, not the fixture's `ate-env-telemetry` and `otel-collector`: an apply over the
  fixture would replace its ConfigMap and Deployment and drop its `files` container. The endpoint is
  `http://ate-env-collector.ate-env-collector:4317`.
- 2026-11-16, orchestrator, pull request 3: the collector renders two traces pipelines from one OTLP
  receiver, one per exporter, because the bucket's batch waits up to 30 s to make fewer objects and
  Cloud Trace keeps the default batch. Both set `gcp.project_id` with `upsert`, so a sender cannot
  direct spans into another cluster's project with the collector's identity, which holds
  `roles/cloudtrace.agent` in every cluster project. The collector renders traces only. The metrics
  pipeline is left out, because `--pod-monitoring` covers `ate-env-api`, and the logs pipeline comes
  with phase 7, when Substrate sends its records over OTLP. The image is
  `otel/opentelemetry-collector-contrib` 0.165.0 pinned by digest: the `google_cloud_storage`
  exporter takes traces and a partition from 0.145.0, and 0.165.0 renames the components the
  configuration uses (`google_client_auth`, `otlp_http`, `google_cloud_storage`).
- 2026-11-16, orchestrator, pull request 3: the collector's Google service account needs
  `storage.buckets.get` on the bucket next to `roles/storage.objectCreator`, because the exporter
  with `reuse_if_exists: true` reads the bucket's attributes at start and fails without them. The
  quickstart grants `roles/storage.legacyBucketReader`, which adds `storage.objects.list`; the
  Terraform of pull request 6 can use a custom role with `storage.buckets.get` alone. The span
  objects are `v1/traces/year=YYYY/month=MM/day=DD/spans_<uuid>.gz`, one export request each.
  `telemetry.googleapis.com` must be enabled in the cluster project and in the project of the
  collector's Google service account; the quickstart and `docs/observability.md` say so.
- 2026-11-16, orchestrator, pull request 3: the reporter's `-enable-tracing` and
  `-metrics-listen-addr` (2026-11-10) are not in pull request 3, which stays the manifests. They are
  a pull request of their own, with the reporter's section in `docs/metrics.md` and a
  `PodMonitoring` for the reporter.
- 2026-11-16, orchestrator, from the review of #415: the collector's pod template carries the
  SHA-256 of its configuration as `ate-env.dev/config-sha256`, so `kubectl apply` of a new
  `--project` or `--bucket` restarts it. The check that loads the rendered configuration in the real
  collector (`TestManifestCollectorConfigValidates`) is manual: CI has no `otelcol-contrib` binary.
  Drift check after the rebase onto `9fed77fc`: #392 adds `ListEnvironments`, which does not change
  the reporter (research.md).
- 2026-11-16, orchestrator, pull request 4: the site is `cmd/ate-env-fleet-site` over
  `internal/fleetsite`. It verifies the IAP assertion with `google.golang.org/api/idtoken` and
  queries BigQuery with `google.golang.org/api/bigquery/v2`; both are in `google.golang.org/api`,
  already in the module graph, so no module is added. It pins `ES256` before `idtoken` sees the
  token, because `idtoken` also accepts `RS256` ID tokens that any Google service account can mint
  for any audience, and checks the issuer, which `idtoken` does not.
- 2026-11-16, orchestrator, pull request 4: `GET /api/v1/clusters/{project}/{cluster}` returns the
  snapshot inside an envelope with `generation`, `age_s` and `stale`, not the bare snapshot. FS-9
  asks the API for `"stale": true`, and a field added to the snapshot itself would fail
  `DecodeSnapshot`, which refuses unknown fields.
- 2026-11-16, orchestrator, pull request 4: `FLEET_EVENTS_TABLE` names the view `actor_events_v`,
  which the site queries, and `FLEET_IAP_AUDIENCE` is a new variable for the assertion's audience.
  `-dir` reads a reporter's `-out-dir` output and `-insecure-no-iap` serves it without IAP on a
  loopback address only, so the pages can be checked without a deployment.
- 2026-11-16, orchestrator, pull request 4: the pages have no script, so the policy is
  `default-src 'none'` with the stylesheet from the site; the Live page of pull request 5 decides
  whether it needs a script and its nonce. The site serves `GET` and `HEAD` only and limits each
  user to 10 requests a second per instance, because each events request is a BigQuery job.
- 2026-11-16, orchestrator, from the review of pull request 4: `fleet.actor_events` is partitioned
  by `publish_time` (day). The view takes every other column from the JSON `data` column, so only
  `publish_time` can prune; without the partition each events request would bill the whole table and
  fail once the table passed the 10 GiB cap. FS-2 records the bytes one query processed.
- 2026-11-16, orchestrator, same review: the site's Cloud Run service gets `--memory 2Gi` and
  `--concurrency 20`. A 100,000-actor snapshot measured about 600 MB resident once decoded and 1.5
  GB at peak with four concurrent downloads; the API writes compact JSON. A refresh runs on its own
  context with a 20 s timeout, requests get the last listing while it runs, and a snapshot whose
  read failed is read again, so one storage error or a closed tab does not stick to a generation. A
  `generated_at` more than a minute ahead of the site's clock counts as stale.
- 2026-11-16, orchestrator, from the second review of pull request 4: a request gets the last
  listing during a refresh only while it is younger than two cache periods, so the first view after
  an idle night waits for a fresh one instead of showing every cluster stale; `?nocache=1` waits for
  a refresh that started after it. The deployment sets `GOMEMLIMIT=1600MiB` under `--memory 2Gi`: 20
  concurrent downloads of a 100,000-actor snapshot reached 2.1 GB without it and 1.6 GB with 1500
  MiB.
- 2026-11-16, orchestrator, from the third review of pull request 4: staleness is judged at the
  listing that read the snapshot, not at the request, because a listing serves up to 30 s and the
  reporter's default limit is 15 s: a healthy cluster showed stale in 18 of 25 samples. A snapshot
  is also stale when it is older than the limit plus 60 s now, which covers a listing that went old
  in a bucket outage; such an outage serves the last listing to requests without `?nocache=1`. FS-9
  clears within one interval plus the cache period.
- 2026-11-16, orchestrator, from the review of #416: the site decodes a snapshot without refusing
  unknown fields and then runs `Snapshot.Validate`, so a field added to `v1` later does not turn a
  cluster with a newer reporter into an error before the site is redeployed; the reporter and the
  tests keep the strict `inventory.DecodeSnapshot`. Staleness and `age_s` use the object's update
  time from the bucket, not the reporter's clock, when the store has one. The events route has its
  own limit, 1 request a second per user, burst 5, because each request is a BigQuery job; the
  reader follows the result's page tokens up to 1000 rows.
- 2026-11-16, orchestrator: pull requests 3 and 4 merged as #415 and #416. Drift check against
  `main` at `a12f48a0` (research.md, "Audit of main, 2026-11-16, after pull requests 3 and 4
  merged"). The seven steps hold. Step 1 of the README's drift check no longer says that no RPC
  enumerates environments, because `ListEnvironments` (#392) does, one atespace page per call; the
  step now watches for a call that lists across atespaces or a watch call.
- 2026-11-16, orchestrator: the reporter's tracing and metrics, which the entry of 2026-11-16 on
  pull request 3 made a pull request of their own, are row 2c of the execution plan's table.
- 2026-11-16, orchestrator: the environment labels of
  `docs/plans/ate-env-api/environment-metadata.md` (#391, store in #407) are recorded in
  `interfaces.md` as a possible source for `actors[].labels`. No pull request of this plan reads the
  store; the field stays empty.
- 2026-11-17, orchestrator: pull request 5 is two. 5a is the Actor and Live pages, `/api/v1/live`
  and the metrics API; 5b is the Runs page, which reads other objects (`v1/runs/`, the benchmark
  buckets) and renders a waterfall. Each is reviewable alone, and the repo asks for one change per
  pull request.
- 2026-11-17, orchestrator, pull request 5a: the Live page reloads itself with
  `<meta http-equiv="refresh" content="10">`, so the pages still have no script and the CSP keeps
  `default-src 'none'` with no nonce. The Live query (last hour, 200 rows, newest first) is cached
  10 s for every viewer of an instance; the Actor page shares the events route's per-user limit.
- 2026-11-17, orchestrator, pull request 5a: the metrics API runs the six named queries of
  `interfaces.md` as range queries over 15m, 1h, 6h or 24h, with `cluster` and namespace filters
  checked as Kubernetes or GKE names before they become label matchers, and a `project` filter;
  every query groups by `project_id` and `location` next to `cluster`, so same-named clusters in two
  projects stay apart, and `crashes_rate` filters the namespace on `ate.workerpool.namespace`.
  `FLEET_METRICS_PROJECT` empty turns the API off (503). The metric names are not yet confirmed in a
  metrics explorer; FS-6 does that on the first deployment.
- 2026-11-17, review of #434: the `workers` and `crashes_rate` queries select Substrate's series by
  their dotted OpenTelemetry names (`{"ate.workerpool.workers"}`, `{"ate.actor.crashes"}`), which
  Cloud Monitoring keeps for an OTLP push, and copy each dotted key with `label_replace` before they
  group by it. `workers` averages across the `ateapi` replicas before it adds, and leaves out
  unschedulable workers and the unknown sandbox class, so it counts capacity once. Every rate range
  is 5 m, no shorter than the 24h window's step. A cached Live or PromQL entry counts its 10 s from
  the end of the upstream call, not from its start, so a call that takes 10 s or times out is cached
  as well. Project filters and `-metrics-project` take domain-scoped project ids, as the collector
  and the reporter do.
- 2026-11-17, orchestrator, pull request 5a: a trace link is the Cloud Trace console URL
  `traces/list?project=<project>&tid=<trace_id>` of the event's cluster project, made only from a
  well-formed project and a 32-hex trace id. The usage samples of the Actor page stay a follow-up
  (phase 9).
- 2026-11-17, orchestrator, from the review of pull request 5a: the Live query runs one at a time
  apart from the requests, and its error is cached for the TTL like its result, so a failing
  BigQuery gets one job per 10 s, not one per viewer; the page reloads itself after an error too.
  The metrics API shares one upstream call among concurrent requests for a query and has its own
  per-user limit (2 a second, burst 10), because each new request is a Cloud Monitoring query on the
  project's shared quota. A Google API error keeps its message in the log.
- 2026-11-17, orchestrator, pull request 5b: every run file is a download
  (`Content-Disposition: attachment`), a trainer's `report.html` too, and a file is served only when
  the run's listing has it. `build_report.py` writes a page with inline script; under a sandbox
  policy it still navigated its own tab to an outside address with data in the query (the review's
  browser probe), so the site does not render a run's HTML at all.
- 2026-11-17, orchestrator, pull request 5b: the bundle folder check moves from
  `cmd/benchmarking/bench` to `schema.CheckFolderName` in `internal/benchmarking/schema`, because a
  package under `internal/` cannot import `cmd/`; `bench`'s `ValidateGCSFolderName` calls it, so the
  two keep one rule.
- 2026-11-17, orchestrator, pull request 5b: a trainer run's Cloud Trace link needs a project, which
  neither the RunReport nor `run.json` records. The page takes the `gcp.project_id` resource
  attribute of the run span, which the collector of #415 sets, else `cloud.account.id`, else
  `FLEET_TRACE_PROJECT`; without any it shows the trace id as text. The acceptance fixture sets no
  project, so FS-11 sets `FLEET_TRACE_PROJECT` to the project the trace was exported to.
- 2026-11-17, orchestrator, pull request 5b: with `-dir`, a benchmark bucket `b` is the directory
  `<dir>/b`, so the Runs page can be checked on a workstation. From the review of #437: `b` must be
  one path element, and every path, the bucket's own directory too, must resolve inside `<dir>`; a
  bucket directory that is a symbolic link out of `<dir>` lists as empty and serves no file.
- 2026-11-17, orchestrator, from the review of pull request 5b: a run's URL names its source
  (`trainer/<id>`, `bench/<bucket>/<id>`) and a run page reads its folder from the store, so a run
  past the 500 of the index or a name in two sources stays reachable; the index sorts by name and
  says so, because the SDK's default run id is random. A source that cannot be listed is an error
  row. What a run page shows is cached per change of its file listing and built at most two at a
  time, and the span parser keeps only the run trace's spans with memoized depths: a 59 MB span file
  made 8 views take 2.16 GB before. RunReport candidates are the files named as `RunReport.write`
  names them, at most 20 of 1 MiB.
- 2026-11-17, orchestrator, pull request 2c: the reporter's tick is the unit of both signals. With
  `-enable-tracing` each tick is a root span `ate-env-reporter.tick` (scope and `service.name`
  `ate-env-reporter`) with an error status when the tick fails, and the `ateapi` calls of the tick
  are its children through `internal/ate`'s otelgrpc client handler; the Kubernetes lists and the
  bucket writes have no spans of their own. `ate_env_reporter_poll_duration_seconds{status}` uses
  `telemetry.SecondsBuckets`, the bucket set of every `internal/telemetry` histogram, 50 ms to 10
  min, which spans a 5 s interval and the 60 s tick timeout. `status` is `ok` or `error`, the values
  `create_to_ready` uses for success and failure; a tick that shutdown cuts short is not observed,
  so a rollout adds no error sample. `internal/reporter` takes a `Tracer` and an `OnTick` callback
  in its `Config`, the way the guest takes `OnProcessExit`, and links no Prometheus code. The family
  is defined in `telemetry.ReporterDefinitions`, which `hack/metrics-docs` renders into
  `docs/metrics.md`. Unlike the environments gauge's reads, which `telemetry.Untraced` keeps out of
  traces, each tick is a sampled root trace: about 17,000 a day per cluster at 5 s, and `ateapi`
  records its server spans under them when it traces. Tracing is opt-in, and the SDK honors
  `OTEL_TRACES_SAMPLER` and `OTEL_TRACES_SAMPLER_ARG` (for example `traceidratio` with `0.1`) on the
  reporter's container to thin them.
- 2026-11-17, orchestrator, pull request 2c: `ate-env manifest reporter` takes the five telemetry
  flags of `ate-env manifest` with the same names and meaning. `--metrics-port` refuses the health
  port, 9091, because both are listeners of one pod. The `PodMonitoring` reuses `buildPodMonitoring`
  with the reporter's name and namespace, and the pod keeps the `prometheus.io` annotations, as the
  `ate-env-api` pod does. Drift check against `main` at `7e03d2b9` (research.md, "Audit of main,
  2026-11-18, for pull request 2c"); the seven steps hold, and step 5 also names the reporter's
  telemetry flags.
- 2026-11-17, review of pull request 2c: FS-15, "Reporter tick telemetry", is added, because no
  criterion covered the reporter's own traces and metrics; its tests name it.
  `ate-env manifest reporter` refuses `--otlp-endpoint` and `--otlp-insecure` without
  `--enable-tracing`, because without it they render environment variables nothing reads. Its flags
  are new, so the check costs no compatibility. `ate-env manifest` still accepts the combination;
  refusing it there too is a follow-up, a behavior change of its own.
- 2026-11-18, orchestrator, pull request 7: `ate-env-api` stamps the target of each call on its
  otelgrpc server span with the keys of Substrate's `internal/ateattr` that `ateapi` stamps
  (`controlapi/span_identity.go`): `ate.atespace` and `ate.actor.name` before the actor is read, so
  a failed call carries them; the full set of `ateattr.ActorAttributes` (adding `ate.actor.uid`,
  `ate.actor.version`, `ate.template.name`, `ate.template.atespace`) once `CreateEnvironment` or
  `GetEnvironment` has the actor, leaving out an empty value so a missing template reference does
  not blank the one the request named. Guest calls are stamped where their `x-env-id` and
  `x-env-atespace` metadata is read (`envFromContext`); template calls carry the base template and
  then the derived one. The keys are copied, because `internal/ateattr` is internal to Substrate.
  `ateapi` validates a request before it stamps it, and `ate-env-api` stamps before its call to
  `ateapi`, so a name is stamped only when it is one Substrate accepts (a lowercase DNS label of at
  most 63 characters, `shortName`). A caller's id of megabytes thus never reaches an `ate.*`
  attribute, where it could push an OTLP batch over its size limit and drop other callers' spans.
- 2026-11-18, orchestrator, pull request 7: no caller identity goes on a span. `ate-env-api` has no
  principal to stamp: callers share one bearer token (`BearerAuth`, known gap 1 of the threat
  model), and `ateapi` stamps no caller principal on its spans either. No token goes on a span, and
  of the metadata only `x-env-id` and `x-env-atespace` do, as the environment's id and atespace. No
  attribute carries a command or a file path. The span status is another matter: otelgrpc v0.72.0
  puts the gRPC message of an `Unknown`, `DeadlineExceeded`, `Unimplemented`, `Internal`,
  `Unavailable` or `DataLoss` error in it (`serverStatus`), and that message can carry
  caller-supplied text, such as the `x-env-id` that the router's "no actor" answer echoes, or a file
  path the guest could not read. A `StartProcess` with a 10,000-character `x-env-id` left a
  1,171-character status message. When callers get identities of their own, whether a span carries
  one is that change's decision. The threat model's Server telemetry section gains a "Data in server
  spans" row. FS-16 covers the attributes.
- 2026-11-18, orchestrator, pull request 7: drift check against `main` at `a0c6024f` (research.md,
  "Audit of main, 2026-11-18, for pull request 7"). The seven steps hold. Step 6 now names
  `internal/benchmarking/schema.CheckFolderName`, where #437 moved the folder form.
- 2026-11-26, orchestrator, pull request 5c: the Run page's waterfall had no labels; a span's name
  showed only on hover or in the table below, so a trace of 224 spans from a dev cluster could not
  be read. Each bar now carries its span name after the last slash, which for an RPC is its method.
  The SVG drew in percent with `preserveAspectRatio="none"`, which stretches text, so it now draws
  in 960 units, its widest size on the page, and scales without distortion. With a monospace label
  font a label's width is known on the server: it goes inside the bar, else right of it, else ending
  left of it. A label is at most 40 monospace cells (a Hangul Jamo, CJK or emoji rune counts two,
  from fixed ranges, not the full East Asian Width table) plus the error cross, under half the
  width, so one side always has room. Below 640 px the waterfall scrolls sideways instead of
  shrinking the labels further. No script is needed, which the site's CSP forbids. Bars are colored
  by service from a fixed palette of six, in the order the services first appear, with a legend. Red
  stays the error meaning: an error bar is hatched red with a dark outline, and its label starts
  with a cross, so the error does not rest on color alone; the span table says "(error)" for screen
  readers. A service with only error bars gets no color or legend swatch. The hatch is an SVG
  pattern the bar names with a `fill` attribute, because a fragment-only `url(#id)` in an external
  stylesheet does not resolve to the page in every browser. Rows grow from 14 to 16 units to fit the
  10-unit font. A gutter of names or an indent by depth was left out: the labels name each bar, and
  the table below keeps the depth. `ParseRunTrace` and its 300-bar cap do not change. FS-19 covers
  it.
- 2026-11-26, orchestrator, pull request 5c: drift check against `main` at `7df41c1` (research.md,
  "Audit of main, 2026-11-26, for pull request 5c"). The seven steps hold. The repository moved to
  `gke-labs/env-rl` with its history, so pull request numbers from #1 on are the new repository's.
- 2026-11-25, orchestrator, pull request 4b: the Fleet page summarizes a cluster's WorkerPools. On a
  dev GKE cluster it listed all 125, and one cluster took several screens. The table's caption names
  the number of pools, the ready and desired workers summed, and how many pools are short. The table
  lists at most ten pools, the ones missing the most workers first and the snapshot's order among
  equals, so a short pool is never hidden behind a ready one, and "N more on the cluster page" links
  the rest. A cluster of ten pools or fewer still shows all of them. Ten rows keep a cluster within
  one screen (363 px of tables on a dev GKE cluster, from 3,394 px); the Cluster page keeps every
  pool with its capacity. During a scale-down a pool can have more ready workers than desired. It
  counts as not short, and the sum shows the ready workers as they are. The JSON API keeps every
  WorkerPool, since a script wants them all. FS-17 covers the page.
- 2026-11-25, orchestrator, pull request 4b: `.cols` sets `align-items: flex-start`. A flex item
  stretches to the line's tallest item by default, and Chrome spreads a stretched table's extra
  height over its rows: beside the 125 WorkerPool rows, the header and the three rows of the
  deployments table were 826 to 842 px tall each, so one deployment row showed per screen under a
  grey header block. With the fix the table is 134 px. The Cluster page's atespace and tag tables
  share the class and the fix.
- 2026-11-25, pull request 4c, found on a dev GKE cluster: its snapshot has 37,255 templates, 49,703
  tags, 2,923 workers and 125 WorkerPools. The Cluster page listed every row: 16.1 MB of HTML and
  about 90,000 table rows, which headless Chrome took 14 s to load (22 s in a desktop Chrome), while
  the site built the page in 0.5 s. Each list of the page now shows its first 200 rows (`maxRows`)
  in snapshot order and says how many the snapshot has and how many match. The actor list drops from
  1,000 rows to the same 200. Templates are summarized by base template with counts by golden state,
  because all but five of them are derived templates, and the summary has six rows there. The
  `pool`, `base` and `golden` filters reach any worker or template, and each form keeps the other
  sections' filters as hidden fields. A filter menu with more than 200 values becomes a text box,
  because the template and node menus grow with the actors. Deployments, WorkerPools, atespaces and
  tags get no filter: no question on the page names one tag, a cluster has few of the others, and
  the JSON has them all. The summary groups base templates by name across atespaces, because a
  cluster runs one ate-env atespace in practice; an atespace column waits for a cluster with two.
  Paging with an offset is left out: a filter answers which rows, and the 201st row by snapshot
  order answers nothing the JSON does not. The JSON API stays complete (27 MB in 0.9 s on that
  cluster): its readers are scripts, interfaces.md says it returns the snapshot as the reporter
  wrote it, and the 128 MiB read cap bounds it. After the change the page is 264 KB and loads in 0.2
  s. FS-18 covers it, because no criterion covered the Cluster page.
- 2026-11-26, orchestrator, pull request 4b: drift check against `main` at `7c04afa` (research.md,
  "Audit of main, 2026-11-26, for pull request 4b"). The seven steps hold.
- 2026-11-26, pull request 4c: drift check against `main` at `9181b67` (research.md, "Audit of main,
  2026-11-26, for pull request 4c"). The seven steps hold.
