# Research: inventory at seconds resolution, and traces

Date: 2026-10-30. Code read: `gke-labs/env-rl` at `3b03b03` (2026-10-29) and
`agent-substrate/substrate` at `28520c0d` (2026-10-27). Cloud facts from the vendor documents linked
at the end.

## Question 1: how to know what exists, within seconds

### Substrate's actor event stream

`ateapi` writes one record per committed actor state transition, after the store commit, never
before (`cmd/ateapi/internal/controlapi/workflow.go`, `logActorStateChanged`, `logActorDeleted`;
`crash.go`, `logActorCrashed`; `internal/actorevent`). Facts from `docs/observability.md` in
Substrate:

- Records: `Actor state changed` (severity 9) and `Actor crashed` (severity 17). Attributes: the
  five identity keys `ate.atespace`, `ate.actor.name`, `ate.actor.uid`, `ate.template.atespace`,
  `ate.template.name`, plus `ate.actor.operation.name` (`create`, `resume`, `suspend`, `delete`,
  `revert`) and `ate.actor.state` (the `ateapi.ActorState` values lowercased, plus `deleted`).
  `trace_id`, `span_id` and `trace_flags` join the record to the RPC that drove it.
- Creation counts: a new actor is born `suspended` with operation `create`. `deleted` is the last
  record an actor gets.
- Consumer rules the document fixes: take the last record per `ate.actor.uid`; never sample or
  filter this stream; a gap looks like an actor that sat still, so reconcile with the control plane.
- Transport: with `OTEL_LOGS_EXPORTER` unset, which is the GKE default, the records go to pod stdout
  as JSON with the keys at the top level, so Cloud Logging indexes them as
  `jsonPayload."ate.actor.state"`. With `otlp` they go out as OTLP log events named
  `ate.actor.state_changed` and `ate.actor.crashed` in scope
  `go.ate.dev/substrate/internal/actorevent`. The document recommends `otlp,console` because `otlp`
  alone can lose a record on a full queue or an ungraceful exit. The GKE managed collector forwards
  only to Cloud Logging, Monitoring and Trace, so routing by `event.name` needs a self-managed
  collector.
- A third event, `ate.actor.usage_sampled`, carries per-actor CPU and memory from the ateworkers
  once per `--usage-sample-interval` (default 1 m, at least 50 s), the only per-actor resource
  signal.

### Control-plane reads

`api/ateapi/v1/ateapi.proto` has no streaming or watch RPC. What exists:

- `ListActors` with an empty `atespace` lists across all atespaces, paged with `ListOptions`
  (`page_size` coerced to at most 1000, `pagination.go`, `maxPageSize`). `Actor` carries
  `ResourceMetadata` (`uid`, `create_time`, `update_time`, `delete_time`), `actor_template`,
  `source_tag` and `ActorStatus` (`state`, `worker_assignment`, `assigned_node`, `crash`).
- `ListWorkers` returns each worker pod with `worker_namespace`, `worker_pool`, `worker_pod`,
  `node_name` and `WorkerStatus` (`state`, `capacity`, `allocated`). `ListWorkerActorAssignments`
  maps a worker to its actors. `ListAtespaces`, `ListActorTemplates`, `ListTags` complete the
  catalog.
- No rate limiter in `cmd/ateapi` (grep for `rate.Limiter` and `ratelimit` finds none); the list is
  a paged Postgres read. `ate-env-api` already pages `ListActors` per atespace for its
  `ate_env_api_environments` gauge (`internal/ate/client.go`).
- Roles in `cmd/ateapi/internal/authz`: `viewer`, `editor`, `owner`. The `ate-env manifest` grants
  `editor` only; a reporter needs a global AccessPolicy with `viewer`. `kubectl ate get actor -A` is
  the precedent for cross-atespace listing.
- Kubernetes side: WorkerPool `status.replicas`, `status.readyReplicas`, `status.selector`
  (`api/infra/k8s/ate/v1/workerpool_types.go`); atecontroller emits Kubernetes Events on WorkerPool
  conditions; informers deliver changes in under a second.

`ate-env-api` cannot serve a fleet inventory. When this was first written `ateenv.v1alpha` had only
`CreateEnvironment`, `GetEnvironment`, `SuspendEnvironment`, `DeleteEnvironment` and
`GetServerInfo`; by 2026-11-09 it also has `GetImageStatus`, `EnsureImageTemplate`,
`ListPlacementDomains` and `WarmImage` (#209, #235), and `Environment` carries `node` and
`worker_selector` (#207). None of them enumerates environments: `ListPlacementDomains` reads
Substrate's workers. The gauge is unchanged: `ate-env-api`'s `ate_env_api_environments` gauge covers
only the atespaces that deployment has created environments in (`apiservice.Server.atespaces`, at
most `maxAtespaces`), cached for `DefaultEnvironmentsCacheTTL` (60 s, flag
`-metrics-environments-ttl`).

### Metrics

- Managed Service for Prometheus scrapes at a 5 s minimum (the operator clamps lower intervals;
  Cloud Monitoring rejects points closer than 2.5 s). Full granularity is kept one week, then
  1-minute points for five weeks, then 10-minute points.
- `ateapi` serves `ate.actor.lifecycle.operation.duration` (operation, template, pool, class,
  snapshot kind and fidelity, `error.type`), `ate.scheduler.assignment.duration` (outcome, pool),
  `ate.actor.crashes`, `ate.workerpool.workers` (every replica reports the whole fleet: `max` by
  pool, state and class, then `sum`). atelet serves the restore, checkpoint, upload and image-cache
  families; atecontroller serves `ate.workerpool.desired_workers` and `ready_workers`. On GKE these
  reach Cloud Monitoring by OTLP push through the managed collector; `OTEL_METRICS_EXPORTER`
  defaults to `otlp,prometheus`. The push interval is the SDK default of 60 s unless
  `OTEL_METRIC_EXPORT_INTERVAL` is set.
- No Substrate metric carries actor identity, by the registry's cardinality rules
  (`docs/metrics/registry/metrics.yaml`).
- `ate-env manifest --metrics-port` emits `prometheus.io` annotations, which managed collection
  ignores; a `PodMonitoring` is missing.

### Conclusion for inventory

Poll `ListActors` across atespaces plus `ListWorkers` every 5 s for the truth, and consume the actor
event stream for exact transition times and the live feed. Freshness of the poll is the interval
plus one object write. Freshness of events is seconds on the OTLP path and seconds typical, 99%
under 60 s, through Cloud Logging. Neither needs a change to Substrate; the OTLP path needs one
environment variable in `ate-otel-config` and a rollout restart.

## Question 2: traces, end to end

### The join exists today

- Python and Go client spans carry `ate.actor.name`, `ate.atespace`, `ate.template.name`,
  `ate.template.atespace`, `ate_env.rootfs_image` and `ate_env.run_id`
  (`clients/python/src/ate_env_rl/client.py`, `clients/go/tracing.go`).
- `ate-env-api` continues the context with otelgrpc on its server and on the `ateapi`, guest and
  wake-probe connections (#114). Its own spans carry `rpc.*` only.
- The atenet router's ingress extracts `traceparent` from the request metadata and calls
  `ResumeActor` under that context (`cmd/atenet/internal/router/ingress/ingress.go`, `resumer.go`).
  Parentless requests root at 1% there.
- `ateapi` stamps actor attributes on its server span (`controlapi/span_identity.go`), opens a
  `step.<name>` span per workflow step with `step.skipped` and `step.skip_reason` (`workflow.go`,
  `stepSpan`, `markSkipped`), traces every Postgres statement, and calls atelet `Run`, `Restore` and
  `Checkpoint` with otelgrpc (`workflow_resume.go`, `workflow_suspend.go`).
- ateworker's otelgrpc spans travel through the atelet OTLP relay on a unix socket
  (`internal/otlprelay`), stamped `ate.otlp.relay=relay`. Every component samples parent-based
  (`internal/serverboot/sampling.go`, `ControlPlaneTraceRatio = 0.1`), so a trace the trainer
  samples is complete through to the worker.
- The lifecycle log records carry the trace id, so an inventory event links to its trace.

This closes the telemetry plan's open question: no trace-context label on actors is needed. The join
needs only that Substrate's components and the ate-env collector export to the same backend.

### Edges

- `ate-env-api` spans lack identity attributes; `ateapi` shows the pattern to copy.
- `ate-env-guest` sets `service.name` only; its spans carry no actor identity on the resource.
- Telemetry an SDK emits inside an actor needs a per-actor EgressPolicy `http` rule to the collector
  on ports 4317 and 4318, and the collector sees the egress gateway as the source. A template-level
  default is Substrate #1430; actor identity injection is #853 and #761.

### Sinks and latency

- Both SDKs batch for 5 s by default (`BatchSpanProcessor`, `sdktrace.WithBatcher`).
- Cloud Trace accepts OTLP at `telemetry.googleapis.com` (GA September 2025) with higher limits than
  the older exporter, 30-day retention and SQL over spans. No freshness number is published; the
  documents say near real-time, and the first trace in a project waits for database creation.
- The collector-contrib `google_cloud_storage` exporter writes OTLP objects with time partitioning,
  gzip and routing by a resource attribute, traces included since v0.145.0. The `googlecloudpubsub`
  exporter publishes OTLP as CloudEvents to a topic.
- The repo's fixture already fans out to files, the `debug` exporter and a forward endpoint; the
  change is configuration.
- Volume: #186's end-to-end run produced 622 spans for 12 tasks, about 50 spans per task.

## Question 3: Cloud delivery paths

| Path                                                 | Documented or measured latency                                                                   |
| ---------------------------------------------------- | ------------------------------------------------------------------------------------------------ |
| Cloud Logging sink to Pub/Sub                        | seconds typical; 99% of entries under 60 s from arrival at Logging; occasional minute-long tails |
| Direct Cloud Storage trigger to Cloud Run (Eventarc) | near-immediate, Pub/Sub under the hood; no published figure                                      |
| Pub/Sub BigQuery subscription                        | streaming, seconds; no code                                                                      |
| Pub/Sub Cloud Storage subscription                   | batched files, window of at least one minute                                                     |
| Managed Prometheus                                   | 5 s scrape floor; the troubleshooting guide recommends `offset` for the newest window            |

## Audit of main, 2026-11-09

Checked against `gke-labs/env-rl` at `f10c68c` and the Substrate `go.mod` requires, `274605f1f2a8`
(Substrate `main` was at `94e0a26a`). What moved since the first read, and what it changes in the
plan:

- `ate-env-api` gained `GetImageStatus`, `EnsureImageTemplate`, `ListPlacementDomains` and
  `WarmImage`, and `Environment` gained `node` and `worker_selector` (#207, #209, #235). Still no
  call enumerates environments, so the reporter stays. The snapshot schema gains two additive fields
  in pull request 2: `workers[].labels`, because placement domains are the distinct label sets of a
  pool's workers, which is how `ListPlacementDomains` computes them, and `actors[].worker_selector`.
- The global `viewer` role is in production use: `ate-env manifest access-policy --global` renders
  the binding `ate-env-api` needs for `ListPlacementDomains`, and the README states the merge rule
  (one global policy object; add a member, never a second object). Phase 0's "verify the role on a
  test atespace" is answered; the reporter's member joins the same binding.
- Substrate at `274605f1`: `ListActors` across atespaces, the 1000-item page, the three actor event
  names and the `viewer`, `editor`, `owner` roles are unchanged since `28520c0d`. The two API
  changes in the range, denying streaming RPCs without an authorization rule (#3262) and removing
  `MintActorCertificate` from `ateapi.proto` (#3427), do not touch the calls the reporter makes.
- Kind end-to-end CI exists (#122, #214, #215, #254): the CI target in `hack/ci/targets.py` installs
  Substrate with `ate-setup --kind`, and `hack/ci/e2e.py` applies the atespace, the access policy,
  the token Secret, the WorkerPool and `ate-env-api`, and runs the Python end-to-end tests. The
  reporter gets one case there against the real `ateapi`, writing to a directory instead of a bucket
  (FS-14).
- `internal/internaltest/fakecontrol` now has workers (`AddWorker`, `ListWorkers`, `GetWorker`) and
  `ListActors`, `GetAtespace`, `AddTemplate`, `GetActorTemplate`, `SetState`. It lacks
  `ListAtespaces`, `ListActorTemplates` and `ListTags`; pull request 2 adds them.
- Benchmark bundles go to GCS (#270): `bench validate` and `bench upload` write one folder per run,
  `<YYYY-MM-DD>_<model-family>-<size>_<dataset>_<nickname>[_traced]`, checked by
  `ValidateGCSFolderName`, with the six files `bundle.go` requires: `system_description.json`,
  `measurements.json`, `run_window.json`, `report.md`, `report.json`, and `events.jsonl` or
  `events/*.jsonl`. The Runs page indexes those folders next to the trainers' `REPORT_DIR` output.
- Metric families are unchanged: the six `ate_env_api_*` families and
  `ate_env_guest_process_duration_seconds` in `docs/metrics.md`, the `ate_env_fleet_*` and
  `ate_env_client_*` series in the Python client, and, on the Substrate side,
  `ate.workerpool.workers` (an UpDownCounter) and `ate.actor.crashes` (a counter) in
  `docs/metrics/registry/metrics.yaml` at `274605f1`, which Cloud Monitoring names
  `ate_workerpool_workers` and `ate_actor_crashes_total`. The named queries stand; step 4 of the
  drift check reads the two registries separately.
- The Python package is `ate-env-rl`, import `ate_env_rl` (#232); the plan already uses that name.
- `docs/plans/` has a convention now: unique criterion prefixes per plan (`AC-`, `AE-`, `PP-`,
  `NG-`, `WF-`, `FS-`), a "last checked" line, and a drift-check procedure (the NeMo Gym plan's
  section 4). This plan adopts the last-checked line and the drift check in the README; the
  telemetry plan's "properties" are not adopted here yet.
- Not affecting the plan: `ate-env-api` round-robins its control-plane calls across `ate-api-server`
  pods (#236); environments that cannot start fail fast with a reason (#222), which the Actor page
  can show later from the event stream.

## Audit of main, 2026-11-10

Checked against `gke-labs/env-rl` at `3dd8bab2`, with pull request 2 on top; the Substrate `go.mod`
requires is unchanged, `274605f1f2a8`. The seven steps of the drift check:

1. `env.proto` has the same nine RPCs; the change since `f10c68c` is comments on `WarmImage` (#304).
   No call enumerates environments.
2. Substrate is the same version: `ListActors` with an empty atespace, the 1000-item page, no
   `Batch*` or stream RPC, the three event names and the three roles are as audited on 2026-11-09.
3. The JSON example in `interfaces.md` is the golden file of `internal/inventory`, with
   `workers[].labels` and `actors[].worker_selector`; `go test ./internal/inventory/` passes.
4. The four `ate_env_api_*` families of the named queries are in `docs/metrics.md`;
   `ate.workerpool.workers` and `ate.actor.crashes` are in Substrate's registry. Checked by hand:
   the test the README named for this step does not exist.
5. `ate-env manifest` has `access-policy --global`, `--metrics-port`, `--enable-tracing`,
   `--otlp-endpoint` and `--otlp-insecure`.
6. `cmd/benchmarking/bench/bundle.go` is unchanged since `f10c68c`.
7. `FS-` is used by this plan only.

What the reporter's code found, recorded in [decisions.md](./decisions.md): Substrate's actors carry
no labels, so `actors[].labels` has no source; `ate-env-api` has no `-ateapi-endpoint` or
`-ateapi-ca-file` flag; and `fakecontrol` gave actors no uid or timestamps, which it now does, as
the control plane does.

## Audit of main, 2026-11-16

Checked against `gke-labs/env-rl` at `66b0e526`, with pull request 3 on top; the Substrate `go.mod`
requires is unchanged, `274605f1f2a8`. The seven steps of the drift check:

1. `env.proto` has the same nine RPCs. No call enumerates environments on `main`. Open pull request
   #392 adds `ListEnvironments`, one atespace page per call; when it lands, the reporter still lists
   across atespaces through `ateapi`, so the plan holds, and this step records it.
2. Substrate is the same version as on 2026-11-10; nothing to re-read.
3. The JSON example in `interfaces.md` equals `internal/inventory/testdata/snapshot.golden.json`;
   `go test ./internal/inventory/` passes.
4. The six `ate_env_api_*` families in `docs/metrics.md` are unchanged, and hold the four the named
   queries use. Substrate's registry is the same version.
5. `ate-env manifest` has `access-policy --global`, `--metrics-port`, `--enable-tracing`,
   `--otlp-endpoint`, `--otlp-insecure` and `reporter` (#349); pull request 3 adds `collector` and
   `--pod-monitoring`.
6. `cmd/benchmarking/bench/bundle.go` is unchanged since `f10c68c`.
7. `FS-` is used by this plan only.

What pull request 3 found, recorded in [decisions.md](./decisions.md): the contrib collector's
`google_cloud_storage` exporter takes traces and a partition from 0.145.0, and since 0.165.0 the
component names are `google_client_auth`, `otlp_http` and `google_cloud_storage`; with
`reuse_if_exists` it reads the bucket's attributes at start, which takes `storage.buckets.get`; and
an object name is `<prefix>/<partition>/<file_prefix>_<uuid>.gz`.

## Audit of main, 2026-11-16, after the rebase onto 9fed77fc

Pull request 3 is rebased onto `9fed77fc`. What moved since `66b0e526`:

1. #392 merged (`469f18a0`): `env.proto` has ten RPCs, the tenth `ListEnvironments`, one page of one
   atespace per call through `ate-env-api`. The reporter still lists every atespace at once through
   `ateapi`, so the plan holds.
2. The Substrate `go.mod` requires is unchanged, `274605f1f2a8`.
3. `internal/inventory` is unchanged; the golden file equals the example of `interfaces.md`.
4. `docs/metrics.md` is unchanged.
5. `ate-env manifest` gained `--environment-metadata` (#407), which adds a Role and a RoleBinding to
   the manifest; the flags of step 5 are all there.
6. `cmd/benchmarking/bench/bundle.go` is unchanged.
7. `FS-` is used by this plan only.

## Audit of main, 2026-11-16, after pull requests 3 and 4 merged

Checked against `gke-labs/env-rl` at `a12f48a0`. Pull requests 3 and 4 are on `main` as #415
(`f548c13b`) and #416 (`b4d69141`). What moved since `9fed77fc`:

1. `env.proto` is unchanged: ten RPCs, and `ListEnvironments` reads one page of one atespace per
   call. The README's step 1 now states that, in place of "no RPC enumerates environments".
2. The Substrate `go.mod` requires is unchanged, `274605f1f2a8`. `google.golang.org/api` moved from
   an indirect to a direct requirement at the same version, because the site imports it (#416).
3. `internal/inventory` is unchanged; the golden file equals the example of `interfaces.md`, and
   `go test ./internal/inventory/` passes.
4. `docs/metrics.md` is unchanged.
5. `ate-env manifest` has `collector` and `--pod-monitoring` (#415) next to the flags of step 5.
6. `cmd/benchmarking/bench/bundle.go` is unchanged.
7. `FS-` is used by this plan only.

The environment-metadata plan (#391) is on `main` with its store (#407). It gives environments
labels once its create field lands; `interfaces.md` names that as a possible source for
`actors[].labels`. `deploy/fleet-site/` and `docs/fleet-site.md` do not exist yet; they are pull
request 6.

## Audit of main, 2026-11-17

Checked against `gke-labs/env-rl` at `8d110751`, for pull request 5a. Since `a12f48a0`: Substrate
moved to `v1.0.0` (`78068ee8`), whose runtime packages equal `274605f1`; #417, #423, #424 and #433
change nothing the plan reads. The seven steps:

1. `env.proto` has the same ten RPCs.
2. Substrate `v1.0.0`: the three actor event names in `internal/actorevent` are unchanged, and
   `ateapi.proto` has no `Batch*` or stream RPC; `ListActors` and the roles are those of `274605f1`.
3. `internal/inventory` is unchanged; the golden file equals the example of `interfaces.md`.
4. `docs/metrics.md` is unchanged; `ate.workerpool.workers` and `ate.actor.crashes` are in
   Substrate's `docs/metrics/registry/metrics.yaml` at `v1.0.0`. Corrected in review of #434: the
   audit of `28520c0d` said that Cloud Monitoring names them `ate_workerpool_workers` and
   `ate_actor_crashes_total`. It does not: Substrate pushes them over OTLP, and Cloud Monitoring
   keeps the dotted names, without `_total`, and the dotted attribute keys
   (`docs/dev/best-practices/metrics.md`; `docs/examples/gmp-alerting-rules.yaml`, which copies the
   keys with `label_replace` before `by (...)`). The registry also says, since that audit, to
   average `ate.workerpool.workers` across the `ateapi` replicas before adding, and that a worker of
   the `unknown` sandbox class is not capacity. The `workers` and `crashes_rate` queries follow.
5. `ate-env manifest` has every flag and subcommand of step 5.
6. `cmd/benchmarking/bench/bundle.go` is unchanged.
7. `FS-` is used by this plan only.

## Audit of main, 2026-11-17, after pull request 5a merged

Checked against `gke-labs/env-rl` at `36e07232`, for pull request 5b. Since `8d110751`: #434 is pull
request 5a; #404 changes the SWE-bench loader's pre-flight, and in `internal/benchmarking/schema`
only `preflight_coverage.go`, next to which pull request 5b adds `folder.go`; #420 adds `--domains`
and `--node-selector` to `ate-env manifest` and a `workerpool` subcommand, which also has
`--domain-offset`. The seven steps:

1. `env.proto` is unchanged.
2. `go.mod` and `go.sum` are unchanged, so Substrate is still `v1.0.0`; `internal/actorevent` is
   unchanged.
3. `internal/inventory` is unchanged and its tests pass; the example of `interfaces.md` still parses
   equal to `snapshot.golden.json`.
4. `docs/metrics.md` is unchanged, and so is Substrate's `docs/metrics/registry/metrics.yaml`, since
   the Substrate version is.
5. `ate-env manifest` keeps every flag and subcommand of step 5; #420 only adds flags.
6. `cmd/benchmarking/bench/bundle.go` is unchanged on main; pull request 5b moves its folder-name
   check to `internal/benchmarking/schema` so the site can use it.
7. `FS-` is used by this plan only.

## Audit of main, 2026-11-18, for pull request 2c

Checked against `gke-labs/env-rl` at `7e03d2b9`, for pull request 2c. Since `36e07232`, the last
audit: #437 (`7e03d2b9`) is pull request 5b; #436 and #439 change the SWE-bench loader's pre-flight
knobs; #438 changes the Harbor provider; `220e9586` pools `ate-env-api`'s router connections, adds
`-reactive-wake` and a `request_id` to the guest's `StartProcessRequest`; #440 changes one
`internal/apiservice` test. The seven steps:

1. `env.proto` is unchanged: ten RPCs. `220e9586` changes `guest.proto` only.
2. `go.mod` and `go.sum` are unchanged, so Substrate is still `v1.0.0`, with the same actor event
   names, `ListActors` paging, roles and no `Batch*` or stream RPC.
3. `internal/inventory` is unchanged and `go test ./internal/inventory/` passes; the example of
   `interfaces.md` parses equal to `snapshot.golden.json`.
4. `docs/metrics.md` is unchanged on `main` and holds the four `ate_env_api_*` families the named
   queries use; pull request 2c adds the `ate-env-reporter` section, which no named query reads.
   Substrate's registry is the same version.
5. `ate-env manifest` keeps every flag and subcommand of step 5. Pull request 2c gives `reporter`
   the five telemetry flags, and step 5 now names them.
6. `cmd/benchmarking/bench/bundle.go` changed in #437 only, which moved its folder-name check to
   `schema.CheckFolderName`; the folder form is the same.
7. `FS-` is used by this plan only. Pull request 2c adds FS-15.

## Audit of main, 2026-11-18, for pull request 7

Checked against `gke-labs/env-rl` at `a0c6024f`, for pull request 7. Since `7e03d2b9`, the last
audit: #442 (`ba056deb`) is pull request 2c; #441 changes `.github/CODEOWNERS` only. The seven
steps:

1. `env.proto` is unchanged: ten RPCs.
2. `go.mod` and `go.sum` are unchanged, so Substrate is still `v1.0.0`. Its
   `cmd/ateapi/internal/controlapi/span_identity.go` and `internal/ateattr` give the keys pull
   request 7 copies.
3. `internal/inventory` is unchanged and `go test ./internal/inventory/` passes; the example of
   `interfaces.md` parses equal to `snapshot.golden.json`.
4. `docs/metrics.md` gained the `ate-env-reporter` section (#442); the four `ate_env_api_*` families
   the named queries use are unchanged. Substrate's registry is the same version.
5. `ate-env manifest` keeps every flag and subcommand of step 5, and `reporter` has the five
   telemetry flags (#442).
6. `cmd/benchmarking/bench/bundle.go` and `internal/benchmarking/schema` are unchanged. Step 6 now
   names `schema.CheckFolderName`, where #437 moved the folder form.
7. `FS-` is used by this plan only. #442 added FS-15; pull request 7 adds FS-16.

The edge "`ate-env-api` spans lack identity attributes" above is closed by pull request 7.

## Audit of main, 2026-11-26, for pull request 5c

Checked against `gke-labs/env-rl` at `7df41c1`, for pull request 5c. The repository moved from
`ai-on-gke/sandboxenv-rl` to `gke-labs/env-rl`: `b420e32` starts the new repository and #1
(`d80e332`) merges the imported history (`5770309`) with its commit ids kept, so `a0c6024f` is an
ancestor of `7df41c1`. Pull request numbers from #1 on are `gke-labs/env-rl`'s; the numbers this
plan cites up to #443 are `ai-on-gke/sandboxenv-rl`'s. Since `a0c6024f`, the last audit: #443
(`441aa2c`) is pull request 7. #41 points references at `gke-labs/env-rl`: in this plan the audit
lines above, and the `$id` of `internal/inventory/v1/snapshot.schema.json`, now under
`https://gke-labs.github.io/env-rl/schema/inventory/v1/`; the reporter and the site validate against
the embedded file and do not fetch the `$id`. #36 adds `SetEnvironmentTimeToLive` and
`Environment.expire_time`, kept in ConfigMaps of `ate-env-api`'s namespace, which the snapshot does
not carry. #51 lowers the defaults of `ate-env manifest template`. #34, #45, #50 and #57 are plans
of other directories, #42 changes the guest, and #3, #4, #5, #40, #55 and #56 are CI and dependency
bumps. The seven steps:

1. `env.proto` has eleven RPCs: #36 adds `SetEnvironmentTimeToLive`, which changes one environment's
   deadline. No call lists across atespaces or watches, so the reporter is unchanged.
2. Substrate is still `v1.0.0`: #3 bumps `google.golang.org/api` and its indirect modules only, and
   `go.ate.dev/substrate` is unchanged in `go.mod` and `go.sum`.
3. `internal/inventory` changes the schema `$id` only (#41); `go test ./internal/inventory/` passes,
   and the example of `interfaces.md` parses equal to `snapshot.golden.json`.
4. `docs/metrics.md` gains `ate_env_api_deadline_delete_failed_total` (#36); the four
   `ate_env_api_*` families the named queries use are unchanged. Substrate's registry is the same
   version.
5. `ate-env manifest` keeps every flag and subcommand of step 5. #36 adds `update` to the Role of
   `--environment-metadata`, and #51 changes the `--cpu` and `--memory` defaults of `template`.
6. `cmd/benchmarking/bench/bundle.go` is unchanged. `internal/benchmarking/schema` changes its
   schema `$id` base only (#41); `CheckFolderName` is unchanged.
7. `FS-` is used by this plan only. Pull request 5c adds FS-19; FS-17 and FS-18 are taken by the
   Fleet and Cluster page changes in review (#59, #60).

## Audit of main, 2026-11-26, for pull request 4b

Checked against `gke-labs/env-rl` at `7c04afa`, for pull request 4b. Since `7df41c1`, the last
audit: #62 (`7c04afa`) is pull request 5c, and it changes `internal/fleetsite`, this plan and
`CHANGELOG.md` only. The seven steps:

1. `env.proto` is unchanged: eleven RPCs.
2. `go.mod` and `go.sum` are unchanged, so Substrate is still `v1.0.0`.
3. `internal/inventory` is unchanged and `go test ./internal/inventory/` passes; the example of
   `interfaces.md` parses equal to `snapshot.golden.json`.
4. `docs/metrics.md` is unchanged, and so is Substrate's registry, since the Substrate version is.
5. `ate-env manifest` is unchanged and keeps every flag and subcommand of step 5.
6. `cmd/benchmarking/bench/bundle.go` and `internal/benchmarking/schema` are unchanged.
7. `FS-` is used by this plan only. #62 added FS-19; pull request 4b adds FS-17, and FS-18 is pull
   request 4c's (#60).

## Sources

- Substrate: `docs/observability.md`, `internal/actorevent/actorevent.go`,
  `cmd/ateapi/internal/controlapi/{workflow.go,span_identity.go,metrics.go,pagination.go}`,
  `cmd/atenet/internal/router/ingress/{ingress.go,resumer.go}`, `internal/serverboot/sampling.go`,
  `api/ateapi/v1/ateapi.proto`.
- sandboxenv-rl: `internal/ate/client.go`, `internal/apiservice/server.go`,
  `internal/telemetry/{otel.go,metrics.go}`, `tests/telemetry/otel-collector.yaml`,
  `cmd/ate-env/manifest.go`, `docs/observability.md`.
- [googlecloudstorage exporter](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/exporter/googlecloudstorageexporter),
  [googlecloudpubsub exporter](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/exporter/googlecloudpubsubexporter),
  [Managed Service for Prometheus](https://cloud.google.com/stackdriver/docs/managed-prometheus),
  [prometheus-engine PR #2328](https://github.com/GoogleCloudPlatform/prometheus-engine/pull/2328),
  [Logs routed to Pub/Sub](https://docs.cloud.google.com/logging/docs/export/pubsub),
  [Eventarc Cloud Storage trigger](https://cloud.google.com/blog/topics/developers-practitioners/introducing-new-cloud-storage-trigger-eventarc),
  [OpenTelemetry in Google Cloud Observability](https://cloud.google.com/blog/products/management-tools/opentelemetry-now-in-google-cloud-observability),
  [Telemetry API v1.traces](https://docs.cloud.google.com/stackdriver/docs/reference/telemetry/v1.traces).
