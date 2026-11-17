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
  `ai-on-gke/sandboxenv-rl` because the reporter reuses `internal/ate` and the site reuses the
  schema package. The alternative is a separate repository; the maintainers decide at the first pull
  request.
- 2026-10-30, orchestrator, tentative: the site is Go with the standard library, server-rendered
  pages, inline SVG, no external assets, in the style of the explorer plan and `build_report.py`.
- 2026-10-30, open: the central project, the bucket name, the Google group, the cluster list,
  whether Cloud Trace is enabled in each cluster project, and whether every user sees every
  namespace.
- 2026-11-02, orchestrator: the plan is proposed to `ai-on-gke/sandboxenv-rl` under
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
