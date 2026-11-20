# Decisions and deviations log

Append-only. Each entry: date, who, what, why.

- 2026-10-21, orchestrator: the target asked for as "substrateenv-rl" is `gke-labs/env-rl`; no
  repository of the other name exists. All plan documents name the real repository.
- 2026-10-21, orchestrator: plan documents live in a notes repository rather than in the target,
  because the target is a shared public project; only code, tests and user-facing docs go there.
- 2026-10-21, lead (seeded): W3C trace context travels in gRPC metadata rather than in object
  annotations, because every boundary in ate-env is gRPC and Substrate's `ateapi` already reads it
  with otelgrpc. The annotation pattern from agent-sandbox stays an open question for Substrate's
  asynchronous hops.
- 2026-10-21, lead (seeded): histograms in seconds with one bucket set; prefixes `ate_env_client_*`,
  `atefleet_*`, `ate_env_api_*`, `ate_env_guest_*`, next to Substrate's `ate_*`. agent-sandbox's SDK
  uses milliseconds; not copied.
- 2026-10-21, lead (seeded): configuration stays frozen dataclasses with the repository's
  unknown-field rejection; no pydantic dependency.
- 2026-10-21, lead (seeded): `prometheus-client` and the OpenTelemetry packages are extras
  (`metrics`, `tracing`); the base install stays `grpcio` and `protobuf`. agent-sandbox has
  `prometheus-client` in its base dependencies; not copied, because `ate_env` is imported by the
  NeMo Gym provider in processes that did not ask for a registry.
- 2026-10-21, orchestrator: kickoff verification of the acceptance cluster: Substrate in
  `ate-system` (ate-api-server, ate-controller, atenet-router, atenet-egress, ate-idp-server,
  postgres) and ate-env in `ate-env` (Deployment and Service `ate-env-api`, a WorkerPool). No
  telemetry namespace existed yet.
- 2026-10-29, orchestrator: the entries below record, after the fact, where the merged pull requests
  (#93, #109, #113, #114, #115, #150, #164, #168) depart from the 2026-10-21 contracts. The
  developer made these calls in the pull requests; the reviews accepted them.
- 2026-10-29, orchestrator: the plan documents move into the target repository under
  `docs/plans/telemetry/` at the owner's request, which reverses the 2026-10-21 entry. The notes
  repository keeps the 2026-10-21 snapshot. Cluster names, project ids and local paths were removed
  from the documents for the public repository.
- 2026-10-29, recorded from #109: metric prefix `ate_env_fleet_*` and span prefix
  `ate_env_fleet.<phase>` instead of `atefleet_*` and `atefleet.*`, so every family and span in the
  stack shares the `ate_env` prefix next to `ate_env_client_*`, `ate_env_api_*` and
  `ate_env_guest_*`.
- 2026-10-29, recorded from #93 and #150: span attribute keys are Substrate's `ateattr` keys for
  identities (`ate.actor.name`, `ate.atespace`, `ate.template.name`, `ate.template.atespace`) and
  `ate_env.*` for client-only fields, instead of the planned `env.*`, so spans join Substrate's logs
  and traces without a mapping and the Go and Python clients share one set of constants.
- 2026-10-29, recorded from #164: Python `Client` spans carry otelgrpc 0.72's `rpc.system.name` and
  the full `rpc.method`, with `OTEL_SEMCONV_STABILITY_OPT_IN` (`rpc/old`, `rpc/dup`) selecting the
  older keys, so a trace backend sees one vocabulary from Python, Go and `ate-env-api`.
- 2026-10-29, recorded from #93: `start_span` is a context manager; the planned `trace_span` and
  `async_trace_span` decorators were not ported. OpenTelemetry loads lazily through `_load_otel` and
  a module `__getattr__`; the planned `OPENTELEMETRY_AVAILABLE` flag and `Mock*` classes were not
  ported.
- 2026-10-29, recorded from #93: provider ownership. The client never replaces an application's
  `TracerProvider`; a `TracerManager` resolves the tracer at span time so a provider installed
  before or after the client wins. The client builds its own OTLP provider only when
  `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is set and no provider is
  installed. With neither, no exporter exists. The plan had one global provider owned by the client.
- 2026-10-29, recorded from #93: context crosses `LoopRunner` because the runner executes each
  coroutine in a copy of the caller's `contextvars` context, which carries the OpenTelemetry
  context; `process_parallel` copies the context into its pool threads. No `run_with_context` helper
  was added.
- 2026-10-29, recorded from #93: the `tracing` extra pins `opentelemetry-api`, `opentelemetry-sdk`
  and `opentelemetry-exporter-otlp-proto-grpc` at `>=1.43` with no upper bound. The planned
  `>=1.39,<1.42` cannot work: 1.43 is the first release whose `opentelemetry-proto` accepts protobuf
  7, which the client requires, and a cap conflicts with applications on a newer SDK.
- 2026-10-29, recorded from #109: `ObservabilityConfig.enable_metrics` defaults to `None`, meaning
  on when the `metrics` extra is installed and one INFO message otherwise; `True` warns when the
  extra is missing. The plan had `True`.
- 2026-10-29, recorded from #109 and #113: `RunReport.write(report_dir)` writes the `.txt` and
  `.json` pair and never overwrites; `reap` is in `RunReport.ORDER`; negative durations clamp to 0;
  collector caches are keyed by registry through weak references; a repo named `other` or `unknown`
  is labelled `other_repo` or `unknown_repo`; `MAX_FAMILIES` (64) bounds `family` with an `other`
  overflow.
- 2026-10-29, recorded from #115: `ate_env_fleet_release_failures_total{backend}` and
  `RunReport.release_errors` record failed releases instead of swallowing them. Not in the plan.
- 2026-10-29, recorded from #109: client histograms are
  `ate_env_client_{acquire,exec,suspend,warm_claim,release}_seconds{status}` plus the claim
  sub-phases `ate_env_client_{slot_wait,wire_claim,ready_to_first_command}_seconds{status,source}`.
  The planned `ate_env_client_wake_seconds` was not added; wake is the warm claim's
  `ate_env_client.get_environment` span and a GRPO phase.
- 2026-10-29, recorded from #114: `internal/telemetry` has no `Instrumenter` interface. otelgrpc's
  server and client handlers cover every boundary, so `SetupOTel(ctx, serviceName)` returns a
  shutdown func only and `telemetry.Untraced(ctx)` keeps the scrape-time `ListActors` out of traces.
- 2026-10-29, recorded from #114: `-metrics-listen-addr` defaults to empty (metrics off) instead of
  `:9090`, because the listener has no authentication and no Host check and must be opt-in;
  `ate-env manifest --metrics-port` turns it on and adds the container and Service port.
  `-metrics-environments-ttl` (60 s) caches the control-plane read behind
  `ate_env_api_environments`.
- 2026-10-29, recorded from #114: `atespace` is a label on `ate_env_api_create_to_ready_seconds` and
  `ate_env_api_environments`; the open question is closed as bounded in practice.
- 2026-10-29, recorded from #114 and #168: the acceptance collector manifest lives in the repository
  as `tests/telemetry/otel-collector.yaml`, with OTLP/JSON file output, the `debug` exporter and an
  optional forward to a managed collector, and the observability guide documents the Cloud Trace
  routes. The plan left the location open.
- 2026-10-29, recorded from #150: the guest shipped. The dependencies add 2.6 MiB to the stripped
  binary (13.82 MB to 16.56 MB), under the 5 MB gate. Metrics have their own flag,
  `-enable-metrics`, separate from `-enable-tracing`, because the guest's `/metrics` is served
  without authentication on the guest listener and must be a choice of its own.
- 2026-10-29, recorded from #150: `Client.Get` was added to the Go client to back `get_environment`.
  With a caller-supplied `GRPCConn`, the client puts the trace carrier in each call's metadata
  instead of a stats handler on the connection.
- 2026-10-29, recorded from #168: the swebench `Recorder` takes an OpenTelemetry `trace.Tracer`
  (`WithTracer`, `SetTracer`) instead of an `Instrumenter`, and records each span after the fact
  from `ts - dur_ms` to `ts`, because the loader writes an event when its operation finishes and has
  no start/done pair. `swebench-loader run -enable-tracing` opens one `swebench-loader.run` span per
  run.
- 2026-10-29, recorded from #168: the metrics-docs freshness check is a step in the Go CI job and
  the Python extras run is a second step in the Python job, rather than new jobs or a matrix,
  because the `main` ruleset requires the jobs named `Go build, vet, test and license headers` and
  `Python client and integration tests`. #168's first revision renamed them and was blocked.
- 2026-10-29, recorded from #168: `benchmarking/telemetry_report/build_report.py` (an HTML report of
  one run's report, spans and scrapes) was added. Not in the plan.
- 2026-10-29, orchestrator: process. The series ran as reviewed pull requests from one developer
  branch per PR, with the repository's maintainers as reviewers. No `tests/RESULTS.md` or
  `review/findings.md` were produced; the evidence is each pull request's Tests section and CI. #113
  was the one pass driven directly by the criteria in this directory.
- 2026-10-29, orchestrator: `ate_env.telemetry.trace_recorder` (#173, benchmark series 4/8) writes
  the client spans as v1 `TraceEvent` JSONL under `ENV_BENCH_MODE=trace`. It consumes this plan's
  spans and is specified in `internal/benchmarking/PLAN.md`; it is outside this plan's scope.
- 2026-10-29, orchestrator: #186 (telemetry in the VeRL SWE-bench example) is open as the sixth and
  last pull request of the series. Its end-to-end run on GKE: 622 spans in 6 traces, 0 orphan roots,
  every `verl.iteration` trace joining `verl-trainer`, `ate-env-fleet` and `ate-env-api`.
- 2026-10-29, orchestrator: still open. A trace-context label on actors so Substrate's scheduler and
  atelet restore join the client trace; `docs/glossary.md` has no telemetry section (the phase
  glossary is in `docs/observability.md`); no overhead measurement of telemetry off against on is
  recorded.
- 2026-10-30, orchestrator: the Substrate trace join is closed, from reading Substrate at
  `28520c0d`. The atenet router's ingress extracts `traceparent` from the request metadata and calls
  `ResumeActor` under it; `ateapi` stamps actor identity on its server span, opens a `step.<name>`
  span per workflow step and calls atelet `Restore` with otelgrpc; ateworker's spans follow through
  the atelet relay. Every component samples parent-based, so a trace the client samples is complete
  through to the worker once Substrate exports to the same backend. No trace-context label on actors
  is needed. Substrate's `Actor state changed` records carry the trace id as well.
- 2026-11-02, orchestrator: #186 merged as `9c30be1`; the series is complete. #202 (`5fc65b1`,
  2026-11-02) raised the default wake budget from two minutes to five; no document in this directory
  cites the figure.
- 2026-11-02, orchestrator: the entries below copy, after the fact, decisions from the plan's
  working log that the 2026-10-29 revision left out. Each carries the date it was made. They are
  reworded to match `main` at `9b2148a`.
- 2026-10-22, recorded from #93: the run id is `ate_env.run_id`, on every step-level span (`exec`,
  `read_file`, `write_file`) as well as the driver's `acquire` and `reap` spans, so a run's steps
  are selectable without walking parents. As shipped on `main`, the driver's `warm_pool` span has it
  too. `Client.create_environment` has no `ate_env.run_id` attribute: inside a fleet task the trace
  recorder takes the run id from `task_context` for its JSONL event, not for the OTel span.
- 2026-10-22, recorded from #93: the first exec started on each acquired sandbox carries
  `ate_env.first_command=true`. It holds the wake and readiness wait and ends the time to first
  command window, which `ate_env_client_ready_to_first_command_seconds` also records.
- 2026-10-23, spec author: AC-9. A Dafny model of `repo_family` (not in the repository) failed the
  property "`other` only on overflow" with a repo literally named `other`: it got the overflow
  label, so dashboards could not tell the two apart, and it used a registry slot although reserved
  names do not count toward the cap. Reproduced against the #109 code. This is why a reserved name
  is labelled `<name>_repo` (the #109 and #113 entry above). `main` now escapes every `other` or
  `unknown` followed by any number of `_repo`, so the mapping stays one-to-one.
- 2026-10-23, spec author: AC-7. The same model proved `0 <= max_s <= total_s` only with the
  unstated precondition `dur >= 0`; `add_phase("acquire", 2.0)` then `add_phase("acquire", -5.0)`
  gave `total_s=-3.0, max_s=2.0`. Every caller uses `time.monotonic()`, so this was latent. This is
  why `add_phase` and `task_done` clamp negative durations to 0.
- 2026-10-23, recorded from #109: `status` has a third value, `cancelled`, from `metrics.status_of`:
  `asyncio.CancelledError` and `KeyboardInterrupt` give `cancelled`, other exceptions `error`. It
  applies to phases, tasks, claims and client timings, so a trainer that cancels stragglers does not
  inflate the error count.
- 2026-10-23, recorded from #109: `ate_env_client_warm_claim_seconds` replaces the planned
  `ate_env_client_wake_seconds`. It times only the `get()` check on a claimed warm actor. The resume
  happens on the first guest request and lands in the first `ate_env_client_exec_seconds`
  observation, so "wake" misnamed it.
- 2026-10-23, recorded from #109: AC-14 exception. #109 replaced three #93 tests in
  `test_telemetry_tracing.py`, which asserted that the caller's span is the direct parent of
  `ate_env_client.exec` and `.release`, with `test_nests_*` tests. The `ate_env_fleet.*` spans now
  sit between them by design. `test_nests_user_span_parents_the_run` keeps "the caller's span
  parents the run", and `test_nests_context_manager_fleet_deletes_every_environment` restores the
  `delete_environment` count and the `with SandboxFleet(...)` path.
- 2026-10-23, recorded from #109: two files outside the deliverables list changed.
  `backend/mock.py`: `MockBackendDriver` sets `backend_name = "mock"`, calls the observer's `claim`
  and `warm_add` / `warm_remove`, so a mock run fills the report and the series like a Substrate
  run. `tests/test_substrate_driver.py` gained tests only: warm-pool accounting
  (`test_recycle_counts_the_actor_back_into_the_warm_pool`,
  `test_failed_warm_check_counts_the_put_back`) and `test_cancelled_claim_has_status_cancelled`.
- 2026-10-24, spec author: AC-15 proposed. A Dafny model of `SubstrateBackendDriver`'s warm-pool
  bookkeeping (not in the repository), with each stretch between two `await`s as one atomic step,
  found two interleaving bugs, both reproduced against the driver: a recycle paused in `suspend()`
  while `reap()` runs leaves the VM alive and back in the pool after teardown, with the gauge at 0
  while the pool holds 1 (P-10, P-9); two overlapping `warm_pool(replicas=3)` calls create 6 actors
  (P-11). Proposed fix: a teardown counter re-checked after the await, and an in-flight reservation
  per pool key. The fix is not on `main`; AC-15 is open.
- 2026-10-27, spec author: the acceptance criteria were refined into numbered requirements (AC-n.m)
  with `For any …` properties P-1 to P-20, each tagged with the requirements it validates and meant
  to run as Hypothesis or `rapid` tests. Fixed-artifact and cluster requirements stay example or
  integration checks. A stateful Hypothesis test of AC-15 found the P-10 and P-11 bugs on the driver
  and shrank them to 3- and 4-step counter-examples. Neither the tests nor the tooling are in the
  repository.
- 2026-10-27, spec author (property run): running the properties against the series' code found four
  places where the spec was behind the code, now matched: `RunReport.release_errors` and the
  `release_errors=N` summary line (#115); `ate_env_fleet_release_failures_total{backend}` (#115);
  one swebench span per event, back-dated by `dur_ms` (#168); and the `family` bound
  `MAX_FAMILIES + 2`, since `unknown` sits outside the cap as well as `other`. It found one code
  race: P-14, with concurrent execs on one handle the span marked `ate_env.first_command=true` is
  not always the first started, because the flag is set before the span starts. P-14 stays as
  written.
- 2026-10-28, recorded from #150: the Go client's `Attr*` constants are the Python keys for every
  field the Go client has, including `ate.template.atespace`. Not in Go: `ate_env.run_id` and
  `ate_env.first_command`, which describe the Python fleet's run and acquired sandbox, and
  `ate_env.timed_out`, which the Go client cannot know because the guest API returns an exit status
  and no timeout field. Adding it needs a guest API field first.
- 2026-10-28, recorded from #150: the Go client matches the Python client on span kind (CLIENT with
  `rpc.*` and `server.*` only for a single-RPC span on a caller-supplied connection, INTERNAL
  otherwise), empty values (not recorded), `ate_env.rootfs_image` (tag dropped) and
  `get_server_info` (traced). The Python handle's exec and file spans record `ate.atespace` when the
  runtime has one, as the Go client does. Left open: the span-name prefix, fixed `ate_env_client.`
  in Python and `<TraceServiceName>.` in Go.
- 2026-11-02, orchestrator: on `main` at `9b2148a`, P-9, P-10 and P-11 (AC-15) and P-14 still fail.
  `_release_internal` re-pools after awaiting `suspend()` with no teardown counter,
  `_warm_pool_internal` sizes the pool with no in-flight reservation, and `SandboxHandle.exec`
  claims the first-command flag before the span starts. A scripted reproduction against `main`
  printed the leaked VM and the 6-actor pool for `replicas=3`. Added
  [properties.md](./properties.md), AC-15 (open) and property links in
  [acceptance-criteria.md](./acceptance-criteria.md), the proposed warm-pool contract in
  [interfaces.md](./interfaces.md), and test-plan section 8. No property tests, Dafny models or
  result files were added; `hypothesis` is not a dependency.
- 2026-11-03, orchestrator: #221 fixes AC-15 and merges before this directory's update. `reap` bumps
  `_teardowns` when it starts and ends; a recycle, `warm_pool` or failed warm claim that awaited
  across it deletes its actor instead of pooling it; `_warm_inflight` reserves the actors
  `warm_pool` calls are creating, per pool key and `_teardowns` value, and `reap` clears it.
  `hypothesis` joins the `dev` extra for `test_warm_pool_accounting.py`. An adversarial review of
  #221's first version found two more bugs: (A) a reservation outlived a teardown, so a `warm_pool`
  started after it returned with an empty pool; (B) a failed warm claim put its actor back after a
  teardown. The Hypothesis machine missed both (it starts no `warm_pool` after a teardown, and its
  fake `get()` never fails). `warm_pool.dfy`'s invariant already required a reap to drop
  reservations, which is A's fix, but its permanent teardown could not express A's symptom, and its
  one-step acquire could not express B. `warm_pool_generations.dfy`, written after the review,
  states both and fails on them. #221 fixed both and added a plain test for each. The Dafny models
  are now in `verify/`; CI does not run them. Still open: a warm claim whose `get()` succeeds after
  an overlapping reap deleted the actor returns a deleted environment (`ClaimOkReturnsLive`), a
  `warm_pool` cancelled during its `create()` calls can leak the actors already created, and P-14.
- 2026-11-03, recorded from #221: a second review of #221 found two more gaps, fixed in the same
  pull request. (C) Work that starts and finishes while `reap` waits on its deletes saw an unchanged
  `_teardowns` and pooled its actors, which `warm_reset` then hid; `_reaping` now counts reaps in
  progress and blocks pooling. (D) A cancelled delete after a teardown (an `acquire_async` timeout,
  say) dropped an actor that `_owned_actors` no longer held; the actor now stays owned until the
  delete succeeds, and a recycle cancelled in `suspend()` keeps its actor owned. The Hypothesis
  machine now pauses `delete()` and runs recycles and `warm_pool` calls inside a teardown; with the
  `_reaping` check removed it fails in 6 of 6 runs. Neither Dafny model represents the awaits inside
  `reap` or cancellation, so neither covers C or D.
- 2026-11-03, recorded from #221: a third review round, merged with #221 (`efca088`). (E) A delete
  that failed after the run's reap already ran left the actor tagged with that run, which no later
  reap would match; the driver now re-tags it with `WARM_POOL_RUN_ID` (a copy, so the caller's
  handle keeps its run id) and every reap deletes those. (F) A direct `reap`, not through a fleet,
  cleared the pool without moving the warm-replica gauge; `_reap_actors` now moves it (P-9). (G)
  `unwarm_pool` and a shrinking `resize_pool` did not stop `warm_pool` calls in flight, which then
  refilled the pool; a per-pool generation (`_pool_gens`, also in the `_warm_inflight` key) makes
  them delete what they create. The plain tests grew to 18 and the stateful machine gained
  `start_teardown` / `finish_teardown` and `unwarm`. The Dafny models cover none of E, F or G
  ([verify/README.md](./verify/README.md)).
- 2026-11-03, orchestrator: `clients/python` renamed its distribution package from `ate-env-client`
  to `ate-env-rl` and its import module from `ate_env` to `ate_env_rl`. Span prefixes
  (`ate_env_client.*`, `ate_env_fleet.*`), attribute keys (`ate_env.*`), default OTel resource
  `service.name` (`ate-env-client`), and Prometheus metric families (`ate_env_*`) stay unchanged so
  dashboards, collectors, and cross-language Go/Python queries do not break.
- 2026-11-03, orchestrator: added `verify/warm_pool_drain.dfy` for E and G (F, a counter update, is
  left to its plain test). The fixed actions verify and the code before the third round fails, as
  expected. Stating E's property (ORPHAN: no owned actor is left under a run that was already
  reaped) found two more paths that break it: a cleanup delete cancelled after the run's reap
  (`_delete_unpooled` re-tags on `Exception` only), and a `release` whose delete fails or is
  cancelled while a reap runs (`missed_by_reap` covers only a recycle that saw the teardown before
  its delete). A scratch test against `c0c41ac` reproduced both: `reap` of a later run left the
  actors alive. Recorded as open criterion AC-15.9, not fixed here; the model verifies one fix for
  the release path (re-tag whenever `_teardowns` changed since the pop).
- 2026-11-04, orchestrator: AC-20. The models fail on purpose, so the CI check compares results with
  `verify/README.md` (counts from its Run block, failing lemmas from its table) instead of requiring
  `dafny verify` to exit 0; the README stays the one list. Dafny comes from the pinned nixpkgs
  (4.11.0, the version the models were checked with) in a `.#dafny` devShell and a job of its own,
  not the default devShell: it brings the .NET SDK and Z3, which no other check needs. A download of
  the GitHub release zip would have been a second, unpinned source.
- 2026-11-04, orchestrator: #238 fixes AC-15.9 as the model proposed. `_delete_unpooled` re-tags on
  a cancellation as well as a failure (re-tagging an actor whose run is not reaped yet is harmless:
  every reap deletes `WARM_POOL_RUN_ID`, the run's own included). `_release_internal` re-tags when a
  reap ran since the pop, not only when a recycle saw the teardown; a failed release with no reap
  keeps its run id, which a test pins. The model's gap lemmas stay as the code before the fix.
  Review of #238 found three more paths and one race, fixed in the same pull request: a recycle
  cancelled in `suspend()` after a reap; a `warm_pool` call cancelled after a teardown before its
  deletes started (its actors were owned under the reaped run id until a delete failed); and a
  release that checked `_may_pool` and updated `_owned_actors` in two lock holds, so a reap on
  another thread could start between them. `_return_popped` now does both in one hold for every
  release path, and torn-down or drained `warm_pool` actors are owned under `WARM_POOL_RUN_ID` from
  the start. `missed_by_reap` is gone: a reap that the recycle saw also fails `_may_pool`.
- 2026-11-04, orchestrator: #239 fixes AC-15.7. A warm claim whose `get()` succeeds after a reap ran
  since the pop moves on (another pooled actor, else a cold create) instead of raising: `reap(run)`
  clears the whole pool for every run, so a claim for another run must not fail because of it.
  Review of #239: the popped actor is not always the reap's. A reap of another run skips an actor
  recycled under this run, and a cancelled reap may never delete it, so the claim deletes the actor
  if it is still owned; it also checks `_may_pool` and takes ownership in one lock hold, so a reap
  on another thread cannot run in between. In the model `ClaimOk` deletes a stale claim's actor, as
  `ClaimFailFixed` does. Not changed: an `acquire` that cold-creates while its own run's reap is
  running can still end owned under the reaped run id; that is the acquire-during-teardown case, not
  a warm claim. It is tracked as open criterion AC-15.10, which a scratch test reproduced. A stale
  claim also records one `wire_claim` sample with status `error`, as a failed claim does.
- 2026-11-04, orchestrator: #240 fixes AC-15.8. A `warm_pool` call cancelled during its creates owns
  every id it sent a `create()` for under `WARM_POOL_RUN_ID`, instead of awaiting deletes in the
  cancelled task, which would delay the cancellation. Any reap deletes them (one whose create never
  landed is NotFound, counted as gone). The run-id model gained TRACKED (every live actor is owned
  or being released), which made it drop `owned.Keys <= live` and `releasing.Keys <= live`: an owned
  id may never have been created, and `ReleaseStart` can move one to releasing. Review of #240: a
  suspend that fails after a successful create leaked the actor the same way; `_warm_one` now
  separates the two calls and owns and deletes such an actor at once. Second review of #240: a reap
  that snapshots before the cancel handler runs misses the actors, and a fleet reaps its run once,
  so the handler now also starts `_delete_unpooled` in background tasks when `_may_pool(teardowns)`
  is false (a reap ran or is running), as the `wait=False` suspends do; a delete that fails or never
  runs leaves them owned under `WARM_POOL_RUN_ID`.
- 2026-11-05, orchestrator: #242 fixes AC-15.10. A cold `acquire` whose create overlaps its own
  run's reap owns the actor under `WARM_POOL_RUN_ID`, so any later reap finds it. The driver keeps,
  per run id, the reaps in progress and the `_teardowns` value when the last one ended
  (`_run_reaped_since`), because fleets are reused: `run()` reaps its run at the end of each pass,
  so "this run was ever reaped" would tag every acquire of a later pass. A reap of another run does
  not change the owner, so a reap from another fleet on the same driver never deletes an environment
  in use. Not re-tagging when the create started after the run's reap ended (the fleet is closed):
  that is a caller error, as for a release, and the model leaves it out.
- 2026-11-07, orchestrator: #276 fixes the AC-15.2 leak the acceptance-criteria audit found (#273).
  A cold `acquire` cancelled while its `create()` is in flight, for example by its own `timeout_s`,
  dropped the actor id, though the create may have landed. It now does what a cancelled `warm_pool`
  call does (AC-15.8): owns the id under `WARM_POOL_RUN_ID` and, if a reap ran since it read
  `_teardowns`, deletes it in a background task. A create that never landed is NotFound for the
  reap, which counts as gone. The Dafny model has no cancelled-acquire action, so no model claim
  changes. A reap's own failed deletes stay out of scope: the reap drops them and raises
  `InfrastructureError` with their ids, as `warm_pool_drain.dfy` records. Server-side assumption: a
  reap's delete can reach Substrate before a timed-out create lands there; the delete is NotFound,
  the driver drops the id, and a create that lands later has no owner. This case is out of scope for
  the driver: it relies on Substrate registering the actor name before a delete can miss it, or
  cleaning up such an actor itself. The AC-15.8 path (a cancelled `warm_pool` call) makes the same
  assumption.
- 2026-11-08, recorded from #279: AC-7. The bound `0 <= max_s <= total_s <= count * max_s` now holds
  for the rounded `to_dict()` values, not only for `phases`: `to_dict()` rounded `total_s` and
  `max_s` to 0.01 separately, so 3 phases of 0.004 s reported `total_s` 0.01 and `max_s` 0.0 (#273).
  `max_s` rounds up to the next 0.01 and `total_s` is kept in `[max_s, count * max_s]`, which can
  leave it one float step off the 0.01 grid; `summary()` prints the same values, so the `.txt` and
  `.json` of one `write()` agree. P-8 is no longer proposed: `test_phase_stats_consistent` checks
  it, and the AC-7 command runs it.
- 2026-11-09, orchestrator: #320 closes two windows the #276 review left in AC-15.10 and AC-15.2.
  `_acquire_internal` re-read `_teardowns` on each pass of its loop, so a warm claim paused in
  `get()` across its own run's reap compared against the value read after the reap: its cold
  fallback, and a retried claim on an actor another run pooled after the reap, stayed under the
  reaped run id, and a cancel in that fallback create started no delete. It now reads `_teardowns`
  once on entry (`entry_teardowns`). The background deletes a cancelled acquire or `warm_pool` call
  starts are joined: `reap` waits for them before it returns, and `close()` before it closes the
  clients and stops the loop, each up to `background_delete_timeout_s` (10 s). The join runs while
  `_reaping > 0`, so on a shared driver a delete that hangs extends the teardown window by up to
  that bound, during which other runs' recycles delete instead of pooling and their `warm_pool`
  calls delete what they create. Accepted: a delete that hangs for 10 s is already an incident, and
  pooling into a teardown is the leak AC-15 forbids. A delete still running at the deadline is not
  cancelled, and its actor stays owned under `WARM_POOL_RUN_ID` for a later reap; the reap adds the
  actor id to `failed_actors`, so `teardown()` raises `InfrastructureError` naming it, as for a
  failed reap delete, and the caller can sweep it. No new metric: the error carries the ids.
  `close()` logs such an id and does not raise: it has no result to carry it, and the reap before it
  has raised. A delete that outlives both deadlines is waited for twice. `LoopRunner.stop` cancels
  the loop's tasks once and then waits, in bounded rounds, for the tasks the cancel handlers
  started, so a delete started when `close()`'s stop cancels an acquire is awaited rather than
  abandoned by `loop.stop()`. The Dafny models are unchanged: they have no cancelled-acquire action
  and no notion of the join. Review of #320 added two rules: a joined delete that failed, or was
  cancelled, since the last join counts as a failed reap delete too (a fast `Unavailable` left the
  task done before the join looked, and the reap returned with the actor live), with `NotFound`
  still gone and `close()` logging it; and `background_delete_timeout_s` is validated as
  `connections` is (a bool, NaN, infinity or a negative value is a `ValueError`), with `0` meaning
  no wait: the join still lets the cancel handlers start their deletes, then names them all. The
  second review pass of #320 added four more: `close()` cancels the other tasks on its own
  `LoopRunner` and waits for them before the join, because a channel closed first cancelled a
  `create()` whose handler then dialed a new client for its delete, which `stop()` cancelled;
  `AsyncSandboxFleet.__aexit__` awaits the new `close_async()`, because the sync `close()` blocked
  the loop that had to serve the deletes; the join skips a delete whose actor a reap's own delete
  removed, and one on another loop; and a delete task cancelled before its first step is recorded in
  its done callback, because its `finally` never runs. The state machine gates the reap's own
  deletes apart from the deletes other work starts during the teardown, so a reap that does not join
  fails it (verified with the no-join mutant).
- 2026-11-10, Go client parity (PR 6, D6 of `docs/plans/go-client-parity/README.md`): the Go client
  now emits `start_process` on `Env.StartProcess` (un-nested under `Run`), sets
  `ate_env.first_command=true` on the first `exec` or `start_process` of an `Env`, sets
  `ate_env.timed_out` on `exec` (pairing `Env.Run` with `SandboxHandle.exec`; Python's direct
  `Env.shell` still emits `ate_env_client.shell` without `ate_env.timed_out`), records
  `ate_env.file.size` only when a file read reaches `io.EOF` or a write finishes without error
  (matching `ate_env_rl.Env`), and registers `ate_env_client_exec_seconds`,
  `ate_env_client_suspend_seconds` and `ate_env_client_ready_to_first_command_seconds`
  (`source="direct"`) when `ClientOptions.MetricsRegisterer` is set. Remaining AC-17.2 differences:
  `ate_env.run_id` is Python fleet-only, `Env.shell` vs `Env.Run` (`ate_env.timed_out` and span
  name), and Go backfilling server-resolved `ate.template.*` on `Create`, `ImageStatus` and
  `EnsureImageTemplate` (to be reconciled in PR 7's parity vectors).
- 2026-11-10, #330: the guest reports `Process.timed_out` (`optional bool`), so the 2026-10-28
  entry's "the Go client cannot know" no longer holds. Go `Env.Run` and Python `SandboxHandle.exec`
  record `ate_env.timed_out` from the guest's field and fall back to the exit-137 heuristic only
  when it is absent. The PR 6 split stays: Python's direct `Env.shell` returns
  `ShellResult.timed_out` but its span records no `ate_env.timed_out`. New AC-17.2 difference: Go's
  `exec` span records `ate_env.exit_code` 137 with `ate_env.timed_out=true`, while Python's records
  `ate_env.timed_out=true` without `ate_env.exit_code` (`ExecResult.exit_code` is None for a
  timeout); to be reconciled in PR 7's parity vectors.
- 2026-11-10, #329 (Go client parity PR 7): the shared vectors in `proto/testdata/client-parity`
  cover 17.3 (`semconv_mode.json`, `rpc_attrs.json`) and 17.4 (`executable.json`), plus image refs,
  the error mapping with `retryable`, the metric families and RPC coverage. PR 7 adds no vector for
  the differences the 2026-11-10 PR 6 entry left "to be reconciled in PR 7's parity vectors":
  `ate_env.run_id` (Python fleet-only), `Env.shell` vs `Env.Run` (span name and `ate_env.timed_out`)
  and Go's backfill of server-resolved `ate.template.*` stay recorded differences, as does the #330
  entry's `exec` span difference (Go records `ate_env.exit_code` 137 on a timeout, Python none), and
  17.1 and 17.2 stay with per-client tests.
- 2026-11-10, third review pass of #320 (AC-15.2, AC-15.8): `close()` and `close_async()` count a
  teardown (`_count_teardown` bumps `_teardowns`) before they cancel a call or close a channel. The
  cancel handler of an `acquire` or `warm_pool` call in `create()` starts its delete only when a
  teardown ran since the call read `_teardowns` (`_may_pool`), so an acquire that started after the
  run's reap (a worker thread's `acquire()` between `reap()` and `close()`), or with no reap at all,
  owned its environment under `WARM_POOL_RUN_ID` and left it live after `close()`; now the delete
  starts and the shutdown joins it. A recycle or `warm_pool` that finishes during the shutdown
  deletes instead of pooling, as during a reap. And `_warm_pool_internal` raises the first
  `CancelledError` (any non-`Exception`) that its `gather(return_exceptions=True)` returns: when
  `close_async()` on the caller's loop closed the channel, one actor's `create()` or `suspend()` was
  cancelled with the call itself not cancelled, the gather returned the cancel as a result, and the
  call neither owned nor deleted the actors whose create was in flight; its cancel path now owns and
  deletes every actor it started. Tests on the in-process `grpc.aio` server: `close()` and
  `close_async()` after a reap, and with none, during an acquire held in `CreateEnvironment`, and
  during a two-actor `warm_pool` with one create held. The state machine's `AcquireGate` gates
  `get()` and `create()` apart, so a warm claim let through pauses again in the cold create it falls
  back to, where a later step finishes or cancels it. The Dafny models are unchanged: they have no
  `close()`.
- 2026-11-11, fourth review pass of #320 (AC-15.9): a `release`, or a delete of an unpooled actor,
  that `close()` or `close_async()` cancels starts a background delete, which the shutdown joins.
  `_return_popped` and `_delete_unpooled` only re-tagged the actor under `WARM_POOL_RUN_ID` for the
  next reap, and no reap follows a close: a `release` in `suspend()` or `delete()`, whose actor the
  run's reap could not see (the release popped it), and a `warm_pool` call that finished after the
  reap and was deleting its actors, left the environment live. `_count_teardown` sets `_closing`,
  and both handlers start the delete while it is set. This entry first recorded that a cancel
  outside a close kept the AC-15.9 behaviour (tracked for the next reap, no delete started behind
  the caller's cancel). Decided by the owner the same day: `_return_popped` starts the background
  delete after any teardown since the release popped its actor (a reap ran or runs, or a close), as
  `_own_abandoned_create` does for a cancelled `acquire`. A `release` cancelled during or after a
  reap only re-tagged the actor, the reap could not see it, and a fleet reaps its run once, so the
  environment stayed live while a cancelled `acquire` in the same window deleted at once. A reap
  still running joins the delete and names it if it fails. `_return_popped` also serves a `release`
  whose delete failed, so that path now gets one background retry after a teardown; a retry that
  fails leaves the actor under `WARM_POOL_RUN_ID` for the next reap of any run, which is what
  AC-15.5 and AC-15.9 stated before. With no teardown since the pop the actor keeps its run id and
  no delete starts. A cancelled `_delete_unpooled` outside a close still only re-tags. The state
  machine gained a `cancel_recycle` rule; with the delete limited to a close it fails on each of
  five seeds (`alive after teardown`). Its check after the reap now allows a pending background
  delete whose actor is no longer owned, which is the join's own rule: a caller that releases again
  after a cancelled release pops the actor, and that release finishes it. A cancelled background
  delete does not start another one. Decided by the owner the same day, after this entry first
  recorded it as not changed: a `reap` that `close()` cancels on its own runner (another thread's
  `teardown()` still in flight) no longer leaves its actors tracked and not deleted. While
  `_closing` is set, `_reap_one` starts a background delete for its actor when it is cancelled, in
  `delete()` or waiting for the reap's semaphore, and `_reap_actors` does it for a `_reap_one`
  cancelled before its first step (`_delete_unreaped_in_close`); the shutdown joins those deletes.
  Reproduced before the fix on the in-process `grpc.aio` server with 1 and with 18 owned actors (2
  of the 18 wait on the semaphore): no `DeleteEnvironment` reached the server after `close()`.
  `close_async()` on the caller's loop had the same leak by another path: the channel close raises
  `CancelledError` in the reap's deletes, `gather(return_exceptions=True)` returns them as results,
  and the reap returned with the actors live. A reap cancelled outside a close is not changed: its
  actors stay owned as they were, for the next reap.
- 2026-11-12, fifth review pass of #320 (AC-15.8, AC-15.9): `close_async()` on the caller's loop
  left environments live when a `reap` or a `warm_pool` call had more than 16 actors. It cancels no
  task; the channel close cut off the 16 calls in flight, and the calls that waited on the semaphore
  woke uncancelled, dialed a second owned client and sent a foreground `delete()` or `create()` on
  it. `_shutdown_async` joined the background deletes once and closed that client once. Reproduced
  on the in-process `grpc.aio` server: with 18 owned actors and the 2 queued deletes slower (30 ms)
  than the others (2 ms), `close_async()` returned with 2 environments live, 2 background deletes
  not joined and a third client open; with `warm_pool_async("tmpl", 18)` the queued call created an
  18th environment after the close began. Two changes. A `_reap_one` or `_warm_one` that gets its
  slot while `_closing` is set raises `CancelledError` (`_stop_if_closing`; `_warm_one` also before
  `suspend()`), so its actor goes to a background delete as the cut-off ones do and no create is
  sent. `_shutdown_async` repeats join-then-close while an owned client exists, bounded by
  `LoopRunner`'s `_STOP_ROUNDS` (4), for a caller's task that was cut off and went on to another
  call. Not chosen: cancelling the caller's tasks from `close_async()`; they are not the driver's.
  The sync `close()` already cancels every task on its own runner, the queued ones included. Same
  pass, the other callers that delete unpooled actors through a `gather`. `unwarm_pool_async` and a
  shrinking `resize_pool_async` popped actors from the pool and left their run id as it was; only a
  `_delete_unpooled` that ran and was cancelled, or failed, re-tagged one. A drain of 18 cancelled
  with 16 deletes in flight left 2 actors, and a drain of 2 cancelled before its delete tasks ran
  left both, under the run id `warm_pool` gave them (reproduced: `reap_async("run-2")` left them
  live). In the pool, any run's reap deleted them. The pop now re-tags them under `WARM_POOL_RUN_ID`
  in the same lock hold (`_own_drained_for_any_reap`). In a close the same actors got no delete at
  all, so `_delete_drained` has the reap's two handlers and the `_stop_if_closing` check, and the
  delete `gather` at the end of `_warm_pool_internal` has the handler around it: a `close()` that
  lands between that `gather` and the first step of its tasks left the actors live (reproduced by
  starting the shutdown at each loop iteration of the call; the 11th fails before the fix). A drain
  cancelled outside a close still starts no delete: the next reap of any run deletes the actors.
- 2026-11-11, fourth review pass of #320 (AC-15.7): a warm claim cancelled in `get()` (the acquire's
  `timeout_s`, the caller, or a close) no longer leaves the popped actor live and unpooled.
  `_acquire_internal` caught `NotFoundError` and `Exception` around `get()`, not a cancel. With no
  teardown since the pop the actor goes back to the pool and the gauge, as after a failed `get()`.
  After one, it is owned under `WARM_POOL_RUN_ID` and deleted in a background task, which `reap` and
  `close()` join: an actor that `run-0` recycled and `run-1` claimed kept `run-0`, and neither
  `reap("run-1")` nor a later run's reap matched it. Accepted: the put-back can take the pool above
  what `warm_pool` was asked for, when a `warm_pool` call filled the pool while the claim held the
  actor; the failed-`get()` put-back already could, and a recycle can.
- 2026-11-11, fourth review pass of #320 (the AC-15 state machine): `WarmPoolMachine` passed with
  the cancelled-claim leak above, because it checked nothing before the teardown, every acquire was
  `run-1`'s (so every leaked actor matched the teardown's reap or `WARM_POOL_RUN_ID`), and few runs
  reached the cancel. It now draws each acquire's run from `run-0` and `run-1` (the teardown is
  `run-1`'s; what `run-0` still holds may outlive it and goes with a last `reap("run-0")`), tracks
  who holds each actor, and has the invariant `no_warm_actor_is_lost_before_teardown`: before the
  teardown starts, a live `warm-*` actor is pooled, held, in a recycle, or in the `get()` of the
  acquire that claims it. `start_acquire` can pool one actor first, `idle` runs only with work in
  flight or after the teardown, and an example has 16 steps, not 12. Measured over 1,500 examples
  (two seeds each): runs that cancel a claim in `get()` went from 1.5 to 2.7% to 5.2 to 5.7%, and
  runs that cancel the cold fallback of a claim from 0 to 0.14% to 0.27 to 0.5%; the plain test
  `test_warm_claim_paused_across_own_runs_reap_then_cancelled_in_cold_create_deletes_actor` stays
  the deterministic cover for the second. Without the cancelled-claim fix the machine fails on each
  of three seeds, on the new invariant and on `nothing_survives_teardown`.
- 2026-11-12, #361: option (a) chosen for plan question 3 (Python Env.shell attributes vs Go
  Env.Run). Added `ate_env.timed_out` to `ate_env_client.shell` to close the 17.2 attribute
  difference without a breaking span rename.
- 2026-11-12, #362: `proto/testdata/client-parity/spans.json` moves AC-17.1 and 17.2 onto shared
  vectors read by `TestSpansParity` (`clients/go/parity_test.go`) and `test_spans_parity`
  (`clients/python/tests/test_parity_vectors.py`), with `go_only` and `python_only` entries for the
  recorded differences. New AC-17.2 difference recorded from the default-atespace vectors: Python's
  `Client.get`, `Client.suspend` and `Client.delete` default `atespace` to `"ate-env"` and record
  `ate.atespace="ate-env"`, whereas Go's `Client.Get`, `Client.Suspend` and `Client.Delete` take a
  positional `atespace` string and omit `ate.atespace` when it is `""` (the server resolves the
  default).
- 2026-11-13, #378 (from #251): the client histograms (Python `SEC_BUCKETS`, Go `secondsBuckets`,
  pinned for both by `proto/testdata/client-parity/metric_families.json`) add `0.01` and `0.025` s
  buckets, so the sub-second client operations (`read_file`, `write_file`, short `exec`) that
  `bench report` now reads from the histograms are not all in the first bucket. The server's
  `internal/telemetry.SecondsBuckets` keeps its 50 ms floor; the 2026-10-21 "one bucket set" now
  holds per side (client, server).
