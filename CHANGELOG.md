# Changelog

User-visible changes. GitHub release notes are generated from the merged pull
requests (see [docs/release.md](docs/release.md)); this file collects what a
user needs to know across them.

## Unreleased

### Changed
- Go client: the install steps in `clients/go/README.md` set
  `GOPRIVATE=github.com/ai-on-gke/*`, because the repository is private.
  Without it, `go mod tidy` takes `@main` from `proxy.golang.org`, which
  still serves the commit from before the repository became private.
- Python client: the `Env.shell` span (`ate_env_client.shell`) records
  `ate_env.timed_out`, as Go's `Env.Run` span does. The span keeps its name.
- `bench prewarm` ensures each image's derived template through
  `EnsureImageTemplate`, which starts its golden snapshot bake, and waits for
  the goldens to settle (`-golden-wait`, default on; `-golden-poll`) instead
  of creating and deleting one environment per image. `events/prewarm.jsonl`
  keeps one `prewarm` event per image, now with the template, the golden
  state and its tag as labels; a failed golden fails the image. The time
  and `dur_ms` of an event are those of its own image, up to the moment the
  image settled. `-images` counts distinct images, not task rows, and a
  count of zero or less is refused. The `-timeout` default is `30m`, up from
  `5m`, because it now bounds each ensure and each golden wait; a command
  that cannot reach the endpoint fails after 30 minutes unless `-timeout`
  is set lower. The layers
  of an image reach a node only when an environment boots there: that is the
  pre-flight's job (`swebench-loader preflight`), not the prewarm's.

### Added
- `ListEnvironments` on `EnvironmentService`: the environments of one
  atespace, one page per call. `page_size` defaults to 100, and the server
  lowers a value above 1000 to 1000. `page_token` is Substrate's token. An
  optional `statuses` filter applies to each page after the server reads it,
  thus a page can hold fewer environments than `page_size`, or none, while
  `next_page_token` is set: follow the token until it is empty. Go client:
  `Client.List`. Python client: `Client.list_environments()`, which returns
  an `EnvironmentPage`. Labels and a label filter are not part of it (#387).
- `Environment.create_time`: when Substrate created the environment's actor,
  on `CreateEnvironment`, `GetEnvironment` and `ListEnvironments`. Python:
  `EnvironmentInfo.create_time`, a UTC `datetime`, or `None` from a server
  that predates the field.
- `integrations/e2b`: the package `ate-env-e2b` (e2b 2.63 to below 3, Python
  3.11 or newer). `ate_env_e2b.Sandbox` and `ate_env_e2b.AsyncSandbox` are
  subclasses of `e2b.Sandbox` and `e2b.AsyncSandbox`: a program changes its
  import line and its sandboxes are environments on the E2B base template.
  `create`, `connect`, `pause`, `kill`, `get_info` and `is_running` go to
  `ate-env-api`, each with a deadline; `commands`, `files` and `pty` are the
  SDK's own code on envd. The classes override public SDK names only. Each
  other public method, and each argument with no meaning on ate-env, raises
  `SandboxException`, also one that a newer SDK adds. `create` refuses an
  image that does not run as root. envd has no access token, and nothing ends
  an environment after a time. The Kind E2E job runs the classes against real
  envd (`integrations/e2b/tests/test_sandbox_e2e.py`).
- `integrations/e2b`: a runtime layer with envd 0.10.2, the daemon the E2B
  SDK talks to inside a sandbox, from upstream's release binary with a
  checked sha256, and `hack/base-template.sh`, which prints the base template
  that runs it as a sidecar on port 49983 with the capabilities it needs. The
  task image must run as root (its `USER` is root or not set). The Kind E2E
  job checks the layer in a glibc and a musl image, over a suspend, with a
  file written for another user and without the API token, and records what
  envd does in an image with a non-root `USER`.
- `ate-env manifest template --add-capability NAME` (repeatable): adds a
  Linux capability, named without the `CAP_` prefix, to the container of the
  template's actors, on top of Substrate's default set. A runtime layer whose
  daemon starts processes as another user needs `SETUID` and `SETGID`. The
  flag accepts the names that Substrate accepts at the pin in `go.mod`
  (`ate-env manifest template --help` lists them) and fails for another name,
  such as `SYS_ADMIN` or `ALL`. A repeated name is in the template once. The
  capabilities are effective only for a container that runs as root. A derived
  template keeps the capabilities.
- ate-env-api: a port-proxy caller can ask the proxy to wait while a
  suspended environment wakes, with `X-Env-Wake-Wait: <seconds>` (a whole
  number in decimal) on the request, in the header form and the path form.
  The proxy wakes the environment before it forwards anything, with the wait
  of guest gRPC calls, and forwards the request once when the guest answers.
  The wait lasts at most the header's value or the new
  `-port-proxy-wake-wait-cap` (default `1m`; `0` turns it off). A longer
  wake gets the 503 with `Retry-After: 1` as before, a crashed environment
  410 and a deleting one 404 at once, and an invalid value 400. The proxy
  strips the header. Without the header nothing changes (#382).
- `ate-env-reporter` (`cmd/ate-env-reporter`): a per-cluster daemon that writes
  the fleet inventory snapshot of `internal/inventory` every `-interval`
  (default 5 s) to `gs://<-bucket>/v1/inventory/<project>/<cluster>/latest.json`,
  or under `-out-dir`, and keeps one copy per `-snapshot-interval` (default 1 h)
  under `v1/snapshots/`. It lists actors, workers, atespaces, templates and tags
  from the Substrate control plane (global `viewer` role) and lists WorkerPools
  and `ate-env-api` Deployments from the Kubernetes API (`list`).
  `/healthz` and `/readyz` are on `-health-listen-addr` (default `:9091`).
  `ate-env manifest reporter` prints its namespace, ServiceAccount, ClusterRole
  and Deployment, and with `--access-policy` the global `viewer` binding to
  merge. No image is published yet. The snapshot schema v1 gains
  `workers[].labels` and `actors[].worker_selector`, and requires `project`
  and `cluster` to be one path segment each (no `/` or `\`, not `.` or `..`);
  the reporter refuses such a `-project` or `-cluster` at start, and `-out-dir`
  writes stay under the directory.
- Pre-flight knobs on `FleetConfig` (they apply with `preflight=True`):
  `preflight_probes_per_node` (2, at most 8), `preflight_rounds` (3, at most
  10), `preflight_probe_timeout_s` (600, at least a microsecond and at most
  315,576,000,000),
  `preflight_golden_timeout_s` (1800) and
  `preflight_golden`: `"wait"` (the default, as before), `"skip"` (no golden
  wait: the images are probed at once and the goldens keep baking) or
  `"fail"` (an image whose golden is not ready is not probed and `run()`
  raises `PreflightError` before any task; the error's `coverage` is the
  pass's `PreflightCoverage`, which the fleet also records in
  `fleet.preflight_coverage` and the run report). `PreflightError` has a
  `coverage` attribute (None unless a pass set it) that survives pickling.
  `FleetConfig` and `PreflightWarmer` refuse the values that ate-env-api's
  `WarmImage` refuses (more than 8 probes per node, more than 10 rounds, a
  probe timeout below a microsecond), which until now showed only as a short
  coverage, and `PreflightWarmer` refuses a bool or a string for
  `golden_timeout` and `golden_poll` and a value that is not a bool for
  `golden_wait` and `probe_without_golden`; `swebench-loader preflight` refuses the same `-probes-per-node`
  and `-max-rounds`. `PreflightWarmer` gains
  `golden_wait` and `probe_without_golden`. The golden wait of both warmers
  ends at its timeout also when that is shorter than the poll interval (it
  ended at the next poll, 10 s by default). The run report's pre-flight
  summary gains `waited_ms` (time spent waiting for probe slots other passes
  held) and `goldens_not_ready`. `benchmarking/swebench/run_fleet_bench.py`
  gains `--preflight` and `--preflight-probes-per-node`, `--preflight-rounds`,
  `--preflight-probe-timeout`, `--preflight-golden-timeout`,
  `--preflight-golden`.
- `FleetConfig(preflight=True)`: `SandboxFleet.run()` and
  `AsyncSandboxFleet.run()` put every loaded task's digest-pinned image on
  every node of the pools their environments can land on (the base template's pools,
  narrowed by `node_selector` and the image's `placement` labels) through
  ate-env-api's `WarmImage`, before the strategy starts. The coverage is
  `fleet.preflight_coverage`; the run report carries its summary under the
  backend's `environment` block and the phases `preflight_images`,
  `ensure_template`, `preflight_golden` and `preflight_probe`. An incomplete
  pre-flight is logged and the run goes on; without the global viewer
  binding (`ate-env manifest access-policy --global`) `run()` raises
  `PreflightError` up front (a transient failure of the pool listing is
  repeated twice first). Tasks with `Task.template`, images routed through
  `image_templates` and images not pinned by digest are skipped; a pool's
  `node_slots` bounds the probes per node. `BackendDriver.preflight_images()` is the
  hook (the mock backend has nothing to warm). The Kind E2E grants the
  binding and runs a `sliding` fleet with the flag on.
- `swebench-loader plan-placement` takes the output of
  `ate-env placement-domains --template <base> --json` as its `-domains`
  file: `workers` and `worker_namespace` are kept in the plan (and read by
  `ate_env_rl.ImagePlacement`, which now refuses a domain field of the wrong
  type where it read a falsy one as absent), a domain with `workers` gets a
  per-node `node_slots` of `min(the worker's capacity, 3) x workers per node`
  (a stand-in until Substrate reports what a node sustains), and
  `-node-cache-bytes` supplies the cache budget for domains that carry none
  (a zero budget is refused). `image-placement.json` (schema v1) gains the
  two optional domain fields.
- `swebench-loader plan-placement` skips a pool of
  `ate-env placement-domains --json` that no selector addresses: workers
  that share no label, a shared label with an empty value, workers that
  report no node. It prints each as `skipped pool <namespace>/<name>:
  <reason>` and plans over the rest; a file whose pools are all skipped is an
  error. A hand-written domain with the same fault is refused, and so is a
  malformed entry (a negative count, a planner field set, an empty label key,
  a worker count that overflows `node_slots`) in a skipped pool.
- Python client: `Client(credentials=...)` connects to ate-env-api over TLS
  with the given `grpc.ChannelCredentials`, as the Go client can with
  `DialOptions`. Without it the channel stays plaintext.
- `swebench-loader run -preflight`: with `-placement`, runs the pre-flight
  over the plan inside the same command before the run (the `preflight`
  subcommand's defaults), writing `preflight-events.jsonl` and
  `preflight-coverage.json` next to the run's events (above an `events/`
  directory, never in it); an incomplete pre-flight is logged and the run
  goes on, as it does after a complete one that could not delete all its
  probes (the log then says so and counts them), a cancelled one ends the
  command with an error.
- Reports carry `validity.evidence`: the inputs Validity Rule 1 held on (a
  complete pre-flight coverage, the prewarm events, the node cache delta),
  in `report.json` and `report.md`.
- `bench` scenarios take `preflight: {plan, coverage}` (the coverage path
  relative to the scenario file): for a warm footing `bench run` puts the
  named coverage into the bundle as `preflight-coverage.json`, writes no
  synthetic `prewarm` event, checks that the coverage names the plan when
  both are given, and the report's evidence names the coverage by its place
  in the bundle. `scenario.schema.json` takes `preflight` (at least one
  non-empty path) and `footing` (`warm` or `cold`), and `Scenario.Validate`
  refuses any other `footing` too, so `bench` fails on one at load.
- `bench validate -preflight-coverage PATH` gives single-file validation
  (`-report`) a coverage file; a bundle's own (`-dest`) was already found.
- Harbor provider (`integrations/harbor`): `python -m ate_env_harbor.warm
  <tasks-dir>` (and `examples/harbor/run-tb2.sh warm`) warms a dataset's
  images before a run: it ensures the derived template of each (class
  template, image) pair the tasks use, waits for the golden snapshots and
  puts the images on every node of a placement domain (`--selector` or
  `--domain`) with `WarmImage`; one line per pair, `--json` for the report,
  exit 1 when a pair is not on every node, 2 for a usage error.
- Harbor provider (`integrations/harbor`): `--ek worker_selector=key=value,...`
  (the `worker_selector` kwarg; a string in the syntax of `ate-env create
  --worker-selector`, or a mapping) pins a job's environments to a worker
  pool. Substrate ANDs the labels with the template's own selector, and
  `ate_env.json` records them.
- `run-in-cluster.sh` bakes a `-placement` plan into the loader image next to
  the dataset (under the plan's own file name, so the run's `placement` label
  tells plans apart) and rewrites the flag to the baked path; relative paths
  resolve against the caller's directory.
- Go client: `clients/go/README.md` covers the Go client API and operations in
  the Python README's section order, with compiled examples in
  `clients/go/example_test.go` (`go test` runs `ExampleNewClient`, which needs
  no server).
- guest: `Process.timed_out` (`optional bool`, set on every exited process)
  is true when the guest killed the process at `StartProcessRequest.timeout`
  or its default timeout, and false for any other exit, so an OOM kill at the
  deadline no longer reads as a timeout. The Go client (`ShellResponse.TimedOut`),
  the Python client (`ProcessInfo.timed_out`, `ShellResult.timed_out`,
  `ExecResult.timed_out`) and the swebench loader use it, and fall back to the
  exit-137 heuristic only for a guest that predates the field.
- Go client: `ClientOptions.MetricsRegisterer` registers the Prometheus
  histograms `ate_env_client_exec_seconds{status}`,
  `ate_env_client_suspend_seconds{status}` and
  `ate_env_client_ready_to_first_command_seconds{status,source}`
  (`source="direct"`, shared `0.05`–`600` s buckets), idempotently per
  registerer (importing `clients/go` now links
  `github.com/prometheus/client_golang/prometheus`). `Env.StartProcess` emits a
  `<TraceServiceName>.start_process` span (`Env.Run` keeps `exec` without
  nesting `start_process`), the first `Run` or `StartProcess` on an `Env` sets
  `ate_env.first_command=true` (`AttrFirstCommand`), `exec` sets
  `ate_env.timed_out` (`AttrTimedOut`), and `WriteFile` / `ReadFile` record
  `ate_env.file.size` only when the transfer completes without error, matching
  `ate_env_rl.Env`.
- `swebench-loader report` and `bench report`: `fault_classification` in
  `report.json` (top level and per job), in `stats.jsonl` (an
  `infra_fault_rate` measurement on the trial row and a `fault_classification`
  row) and in `comparison.md` (an `Infra Fault Rate` column). Each failed
  rollout lands in one of three tiers: infrastructure before the episode
  (`create_timeout`, `no_capacity`, `image_pull_unpack`, `transport_error`,
  `other`), policy during it (`nonzero_exit`, `command_timeout`, `guest_oom`,
  left out of the rate) and infrastructure during it (`wake_resume_failure`,
  `sandbox_crash`, `snapshot_restore_failure`, `eviction_preemption`,
  `other`), with the faults a retry recovered and one example error per
  category; an `other` row is an error no matcher names yet.
  `infra_fault_rate` is the infrastructure drops over all rollouts, held
  against a `target_rate` of 2%. A report written before the field exists
  has no classification and the renderers say so. `swebench-loader report
  -thresholds` takes `max_infra_fault_rate`, which fails the comparison at
  or above the rate, and when the candidate has no classification.
- ate-env-api: `-wake-state-read-cap` (default 5 s) bounds each control-plane
  state read during a wake; 0 or less means the default.
- Go client: end-to-end suites in `clients/go/e2e_test.go`, gated by
  `ATE_ENV_GUEST_TARGET` (`TestE2EGuestDaemon`, the 24 guest-daemon cases of
  `test_guest_daemon.py`, run in `ci-go`) and `ATE_ENV_API_TARGET`
  (`TestE2EFullStack`, the 5 client cases of `test_full_stack.py`, run in
  `e2e-kind`).
- Go client: `env.DefaultAtespace`. The client no longer imports `internal/ate`, and with it
  `internal/routertls` and Substrate's API package (`go.ate.dev/substrate/api/ateapi/v1`);
  `hack/verify/go-client-deps.sh` keeps it that way in the Go CI job.
- Go client: a guest call (a process or file call) that failed because the
  environment cannot start returns a `*env.StartError` with the `Reason`
  (`env.StartFailureCrashed`, `env.StartFailureNoCapacity`), the status `Code`
  and the `EnvID`, from the `ate-env-start-failure` trailer ate-env-api sets,
  as the Python client's `SandboxStartError`. It is read on unary and streaming
  calls (the `Recv` of `Process.Output` included), on the client's own
  connection and on a `GRPCConn`. `env.Retryable(err)` reports whether the work
  can succeed on a fresh environment, with the Python client's table. New
  sentinels `env.ErrInvalidArgument`, `env.ErrPermissionDenied` and
  `env.ErrFailedPrecondition` (an `ErrProcessExited` error also wraps
  `ErrFailedPrecondition`).
- Python client: `SweBenchSource` (a SWE-bench-style Hugging Face dataset,
  default `R2E-Gym/SWE-Bench-Verified`; reads only the `limit`/`offset` slice
  into memory, and an `offset` past the end reads no tasks; `name=` picks a
  config; `keep_row=True` keeps the row under `metadata["ds"]`; needs `pip
  install datasets`, imported on `load()`), `ImageListSource` (an image
  manifest, one task per image) and `read_bytes()`. `JsonlSource` and
  `ImageListSource` read `gs://` objects through `google-cloud-storage` when
  installed, else `gcloud storage cat` or `gsutil cat`. No new dependency.
- Python client: task sources and dataset-row parsing. `load_tasks()` and
  `add_tasks()` (and their `AsyncSandboxFleet` forms) take `image_field=` and
  `id_field=` (a dotted path, several paths tried in order, or a callable), or
  a `TaskSource`: `ListSource` and `JsonlSource` (a UTF-8 JSON-lines file,
  local or `gs://`; a bad row names its file and line). By default a row's image is read
  from `image`, then the R2E-Gym and SWE-bench keys (`docker_image`,
  `image_name`, under `metadata` and `env_config.entry` too), and its id from
  `task_id`, `id` or `instance_id`, so SWE-bench rows load without mapping. An
  id is never made up from a row's position. `extract_image`, `row_to_task`,
  `to_tasks` and `FieldSpec` are public. What changes for an existing dict
  row is under Changed.
- `ate-env manifest template --sidecar-env NAME=VALUE`: a variable that only
  the `--sidecar` processes get. The container carries it as
  `ATE_SIDECAR_ENV_NAME`; the guest takes it out of its own environment before
  it starts anything and gives `NAME` to the sidecars only, thus the commands
  that ate-env-api starts do not inherit it (`internal/sidecarenv`). Needs a
  guest image with this change: the template also carries the guest flag
  `-sidecar-env-required`, thus an older guest exits at start instead of
  running the sidecars without the values. The OpenHands base template passes
  `OH_SESSION_API_KEYS_0` this way; a base registered with the key in `--env`,
  and each template derived from it, must be deleted and registered again
  (`RECREATE=1` in `examples/openhands/demo.sh`).
- ate-env-api: `ListPlacementDomains` (the worker pools a worker selector can
  address: labels, nodes, workers, slots per node, from Substrate's workers;
  with a `template`, only the pools its worker selector matches) and
  `WarmImage` (server-streaming: puts one `rootfs_image` on every node of one
  domain with probe environments it creates, resumes, holds until `RUNNING`,
  reads the node of and deletes, round after round; a probe event each, then
  the coverage). Features `placement_domains`, `warm_image`. `ate-env
  placement-domains`, `ate-env warm-image <image> --worker-selector k=v`;
  `ate-env manifest access-policy --global` prints the global viewer binding
  listing workers takes; Go client `ListPlacementDomains`, `WarmImage`; Python
  `list_placement_domains()`, `warm_image()`, `PlacementDomain`, `WarmProbe`,
  `WarmImageCoverage`; an `ImagePlacement` plan's domains are `PlacementDomain`
  too, thus a plan can carry what `list_placement_domains()` returns. A
  `nodes` above the distinct nodes of the workers the selector matches is
  `InvalidArgument` when the server can list them, the per-node cap
  multiplies the count the workers report for every call on the selector,
  and only the missing global viewer binding leaves a request's `nodes`
  trusted. A probe's create and resume are retried within the
  probe's timeout on the transient failures of a loaded control plane
  (`Unavailable`, `DeadlineExceeded`, a derived template not visible yet),
  its delete on `Aborted`, `Unavailable` and `ResourceExhausted`; the probes
  a call could not delete (a `CRASHED` one cannot be, today) are listed in
  the coverage's `undeleted_probes` for a sweep, which `ate-env warm-image`
  prints and exits non-zero on. `ate-env-api -warm-max-probes-per-node`
  (default 2) caps the probes in flight per node of one worker selector
  across calls; a request's `probes_per_node` above it is refused, and the
  coverage's `waited` is the time its rounds waited for slots.

- `benchmarking/swebench/run-r2e-swebench.sh`: the mixed R2E-Gym + SWE-bench
  recipe, with `make-r2e-dataset.py` (R2E-Gym `CommandAdapter` manifests),
  `run_fleet_bench.py` (the Python `SandboxFleet` arm) and the `r2e-swebench`
  skill and `REPORT_TEMPLATE.md`; `swebench-loader run -strategy none|naive|sliding|pipelined` and
  `-window-size`, `swebench-loader render-plan`, `swebench-loader report -md`
  over several runs (the arm table), and the `-atenet-*` and `-ateapi-*`
  flags of the direct Substrate backend.
- `run-r2e-swebench.sh --collect-telemetry` (`COLLECT_TELEMETRY`): each arm
  scrapes the node image caches before and after, harvests the `atelet`
  restore records, takes a `bench measure -k8s` snapshot every
  `--measure-interval` seconds (`MEASURE_INTERVAL_SEC`, default 60) and ends
  in `bench report -dest`, which folds it all into one `v1` bundle;
  `--ate-endpoint`, `--prom-url`, `--preflight-coverage` and `--footing` feed
  the reap, the measurements and the validity rules. `--validate-bundle` runs
  `bench validate` on each bundle; `--gcs-bucket` (`GCS_BENCHMARK_BUCKET`,
  never in the repository), `--gcs-folder`, `--gcs-nickname` and
  `--gcs-dry-run` upload it with `bench upload`. `--trace` traces the loader
  (`-enable-tracing` on the Go arms, `traces/{r2e,swe}.jsonl` on `--client
  python`) and `--otlp-endpoint` names the collector. `run-in-cluster.sh`
  takes the same as environment variables (`COLLECT_TELEMETRY`,
  `ARTIFACT_DIR`, `MEASURE_INTERVAL_SEC`, `SAMPLE_CACHE`, `ATE_ENDPOINT`,
  `PROM_URL`, `PREFLIGHT_COVERAGE`, `FOOTING`, `ENABLE_TRACING`,
  `VALIDATE_BUNDLE`, `GCS_BENCHMARK_BUCKET`, `GCS_BENCHMARK_FOLDER`,
  `GCS_BENCHMARK_NICKNAME`, `GCS_DRY_RUN`), and `LOADER_NODEPOOL` /
  `LOADER_TOLERATION_KEY` to pin the loader Job to a node pool and tolerate
  its taint.
- `run_fleet_bench.py --enable-tracing` and `--trace-out PATH`: the Python arm
  points the client's trace recorder at `traces/<job>.jsonl` next to its
  events, one `v1` `TraceEvent` line per sandbox operation; a bundle with a
  trace uploads to a `_traced` folder.
- `bench report -dest` joins the raw `atelet_restores.jsonl` into `restores`
  and writes `restore_timings.jsonl` from it, so `bench validate` and the
  upload carry the samples. When shards share one bundle (`r2e` and `swe`),
  the run tags merge, `load_*` totals and the dataset tag
  `r2e-dataset+swe-dataset`, instead of keeping the last shard's, and a
  dataset tag loses its `/var/run/ko/` prefix. `bench reap -prefix run-` writes `run` as the run id of
  `events/reap.jsonl`, and `bench upload` copies the bundle's files into
  `gs://<bucket>/<folder>/` itself, not into a `<folder>/<dir>/` subdirectory.
- swebench-loader: `run -delete-rate` caps the environment deletes per second
  through ate-env-api (default 200; 0 removes the cap), and a delete the
  bucket rate-limits (429, `ResourceExhausted`) is retried with backoff for
  up to two minutes instead of failing. The run records the cap as the
  `delete_rate` label, so `report -arm` keeps runs with different caps apart.
- `swebench-loader run -placement image-placement.json`: each create carries
  the worker selector of one of its image's domains, round-robin per image
  (`EnvBackend` `WithPlacement`); `report -preflight-coverage` feeds the
  pre-flight's result to Validity Rule 1, which takes a complete pre-flight
  as prewarm evidence on warm footing and fails on an incomplete one.
- `swebench-loader preflight`, the PreflightWarmer: from an image placement
  plan, ensures each image's derived template, then per domain, as each
  golden becomes ready, probes every node of the domain with the image
  (one `WarmImage` call per image and domain, see Changed) until
  each node has run it, `-image-concurrency` images at once;
  `preflight-events.jsonl` and `preflight-coverage.json`. `EnvBackend` gains
  `CreateWith(CreateOptions{WorkerSelector})`, `Node`, `ImageStatus`,
  `EnsureImageTemplate` and `WarmImage` (the last three are the
  `ImageWarmer` interface); `domains.json` takes
  an optional `node_slots` per domain.
- ate-env-api: `GetImageStatus` and `EnsureImageTemplate`, the ActorTemplate
  derived for a `rootfs_image` and the state of its golden snapshot (`NONE`,
  `PENDING`, `READY` with the tag, `FAILED` with the reason); ensure creates
  the template, which starts the bake, with no environment. Feature
  `image_status`; `ate-env image-status <image> [--ensure]` (`image-status` joins
  the reserved environment ids); the Python
  client's `image_status()` and `ImageStatus`; the Go client's `ImageStatus`
  and `EnsureImageTemplate`.
- `examples/Qwen3.5-397B-A17B-verl`: the agentic GRPO recipe from 2 to 16 GPU
  nodes with profiles for Qwen3.5-35B-A3B and Qwen3.5-397B-A17B, steps of
  thousands of trajectories, and guarded verl patches for 64K responses.
- uni-agent provider `ate_env`: `start_wait_s` (wait for sandbox capacity
  instead of failing), `startup_concurrency` (a job-wide start-up gate through
  one Ray actor per job, which also reclaims orphaned environments) and
  `stop_retry_s` (retry deletes while the actor is mid-operation).
- `uni_agent_substrate.datasets --shuffle-seed` for datasets sorted by
  repository; `resolve_images.sh` `MODE=hubapi` (Docker Hub tags API, no
  pulls), `MODE=arapi` (Artifact Registry listing) and `MIRROR_NAME_SEP`.
- Python client: `ate_env_client_ttfi_seconds` (time to first instruction:
  the ask for a sandbox to the first command being issued) and
  `ate_env_client_{grade,read_file,write_file}_seconds` histograms, so the
  sandbox latency breakdown exists without a trace; `exec(..., phase="grade")`
  marks a grading command explicitly.
- `bench report` reads the Prometheus histograms before trace-derived stats;
  traces are the fallback, and each `Stat` records its `source` (`prom` or
  `events`). The histograms count only when sampled through the run window (two
  or more samples per series: the `measurements_snapshots/` copy loop; PR #252
  adds the sampler flags); a single post-run scrape holds one sample and is
  skipped. The Python client also records `<family>_max_seconds` and
  `<family>_min_seconds` gauges, which give the report an exact max and min
  where buckets only bound them.
- Python client: `FleetConfig.node_selector` is sent as every environment's
  `worker_selector`, and `FleetConfig.placement` adds labels per task image;
  `SubstrateBackendDriver(worker_selector=, placement=)` and
  `worker_selector_for(image)`.
- Python client: `ate_env_rl.PreflightWarmer` fills the node caches an image
  placement plan (`ImagePlacement`, from `image-placement.json`) describes:
  ensures each image's derived template, then per domain, as each golden
  becomes ready, probes every node with the image through `warm_image()`
  (see Changed);
  `PreflightCoverage` writes `preflight-coverage.json` in the loader's schema.
  `Observer.record_phase()` and the phases `preflight_golden`,
  `preflight_probe`.
- `swebench-loader plan-placement`: an image placement plan
  (`image-placement.json`) that assigns each task image to placement domains
  (worker pools a per-environment worker selector addresses) under each
  node's image-cache budget, largest image first, with replication and a hot
  rule from the schedule's demand; `domains.json` describes the domains.
- ate-env-api: `worker_selector` on `CreateEnvironment`, labels the worker pool
  must carry, ANDed with the template's own selector (`Actor.worker_selector`,
  so it holds for wakes too); `node` and `worker_selector` on `Environment`.
  `ate-env create --worker-selector key=value`, `ate-env get <id>` (`get` joins
  the reserved environment ids);
  `worker_selector=` on the Python client's `create()` and `node`,
  `worker_selector` on `EnvironmentInfo`.
- Observability across the stack, all off by default except the run report;
  see [docs/observability.md](docs/observability.md).
- Python client: OpenTelemetry tracing behind the `tracing` extra
  (`TracerConfig`, `FleetConfig.tracing`, `ObservabilityConfig.enable_tracing`).
  `ate_env_client.*` and `ate_env_fleet.*` spans, W3C `traceparent` on every gRPC
  call, context carried across `LoopRunner` threads and `process_parallel`.
- Python client: `ate_env_rl.observability`. `fleet.report` is a `RunReport`
  (per-phase count, total and max, claims, task outcomes, warm replicas) with
  `summary()`, `to_dict()` and `write(report_dir)`; `ate_env_fleet_*` and
  `ate_env_client_*` Prometheus series behind the `metrics` extra, served by
  `serve_metrics(port)`.
- ate-env-api: `--enable-tracing` (OTLP over `OTEL_EXPORTER_OTLP_*`, trace
  continued to ateapi and the guest) and `--metrics-listen-addr` (off by
  default, for example `:9090`) serving the `ate_env_api_*` families documented in
  [docs/metrics.md](docs/metrics.md).
- `ate-env manifest`: `--enable-tracing`, `--otlp-endpoint`, `--otlp-insecure`
  and `--metrics-port`.
- ate-env-guest: `--enable-tracing` and `--enable-metrics`
  (`ate_env_guest_process_duration_seconds`).
- Go client: `ClientOptions.TracerProvider` and `TraceServiceName`,
  `NewTracerProvider`, the `Attr*` span attribute keys, and `Client.Get`.
- Go client: `ShellRequest.Poll` runs a command without holding a stream
  through the router: `Env.Run` starts the process, polls it (50 ms doubling
  to 2 s, ten consecutive `Unavailable` or `DeadlineExceeded` polls
  tolerated), reads the output once after the exit, and kills the process if
  it returns first. `Poll` is off by default, and `Run` streams as before.
  `ShellResponse.Duration` and `TimedOut` (exit 137 at or past `Timeout`, the
  Python client's heuristic), in both modes.
- Go client: `ClientOptions.Connections` dials that many connections (0 and 1
  mean one). `Create` and `Env` hand them out round robin, and an `Env` sends
  every call on the connection it got; the other `Client` calls round robin.
  `ClientOptions.DialOptions` adds gRPC dial options to every connection the
  client dials, after its own, as the Python client's `channel_options`. Both
  are errors with `GRPCConn` (`Connections` above one). `Close` closes every
  connection the client dialed. The swebench-loader's `-connections` uses
  `Connections`.
- Go client: `Env.Atespace`, `Env.Info`, `Process.Env` and `Process.Info`, as
  the Python client's `Env.atespace`, `Env.info()`, `Process.env` and
  `Process.info()`.
- swebench-loader: `run -enable-tracing` records every event as a span under one
  `swebench-loader.run` span; the JSONL is unchanged.
- `examples/grpo` and the NeMo Gym rollout benchmark record their phases with
  the client's `Observer` and write the run report to `REPORT_DIR`.
  `examples/grpo/report.py` also reads that report; it keeps reading the old
  per-event JSONL for one more release.
- `benchmarking/telemetry_report`: builds a self-contained HTML report from run
  reports, OTLP/JSON spans and Prometheus or collector metrics.
- Release publishing for `ate-env-rl`: `clients/python` derives its version
  from the Git tag (`setuptools-scm`) and is published to PyPI via OIDC Trusted
  Publishing when a GitHub Release is published; `hack/release/create-release.py`
  drafts release notes and cuts draft releases (see [docs/release.md](docs/release.md)).
- `integrations/nemo-gym`: the provider implements NeMo Gym's
  `SupportsSandboxIndependentUploads`, which `SandboxSession.expire` needs to
  stop a running harness. `upload_file` and `download_file` use a gRPC
  connection of their own, so a file transfer no longer queues behind running
  commands at ate-env-api's `-max-concurrent-streams`. The suspend and the
  state reads of a park use that connection too, because a transfer waits for
  the park of its sandbox. `uploads_independent_of_execs()` is false for a provider built with a
  caller-owned `client=`.
- Python client: `ate_env_rl.adapters.r2egym`, the R2E-Gym adapter
  (`make_fleet_repo_env`, `fresh_eval_env`, `r2egym_command_files`;
  `AteEnvRuntime` and `AteEnvRepoEnv` are built on first access), moved from
  the OpenHands integration so a caller without OpenHands installs only
  `ate-env-rl`. Python >= 3.10, no extra dependency: R2E-Gym itself is still
  installed from its checkout and imported on first use.

### Changed
- Go and Python clients: the `ate_env_client_*` histograms in both SDKs and the
  Python `ate_env_fleet_*` latency histograms gain `0.01` and `0.025` s buckets
  (`SEC_BUCKETS`, 10 ms to 600 s) for sub-50 ms operations; server and guest
  histograms keep 50 ms to 600 s.
- The `ttfi` event from traces and the loader now ends when the first command
  is issued, matching `ate_env_client_ttfi_seconds`; the former span (through
  the first command's return, which holds the boot or wake wait) is the new
  `ttfic` event, and atelet restore records pair against it.
- `swebench-loader preflight` and `ate_env_rl.PreflightWarmer` warm the nodes
  through `ate-env-api`'s `WarmImage`, one call per (image, domain), instead
  of creating, running and deleting the probes themselves. The server's
  retries, per-node cap across calls (`-warm-max-probes-per-node`, which
  also bounds `-probes-per-node` / `probes_per_node`) and `undeleted_probes`
  now apply to both, the coverage's `waited_ms` is filled, and a call the
  control plane cuts off is repeated with its own run id, for up to the
  probe timeout from the first failure and nine attempts, unless its stream
  showed every node already. A pair's coverage is the sum of its attempts
  (the nodes of all, the counters added up). The server holds a call's
  probes in memory only, so an api server that restarts under a call leaves
  them: every probe a broken call streamed is listed in `undeleted_probes`
  (the loader's pass then exits non-zero and the Python coverage is not
  `clean`), and each log line about a cut-off call carries the call's run
  id, which prefixes its probes' names. The golden wait, the fan-out over
  domains and images (`-image-concurrency`, `node_slots`), the events file
  and the coverage file are unchanged. The pass's run id plus the
  `-<domain>-<image>-<attempt>` indexes it appends per call (the plan
  indexes of the pairs the pass warms, in base 36) must fit
  ate-env-api's 20 characters; a longer one is refused up front: the default
  run id fits 46,656 domains with 1,679,616 images. The probe timeout bounds
  a probe's create, resume and node read; the delete has the server's own
  60 s. The pre-flight needs an
  `ate-env-api` with the `warm_image` feature (every release since #235).

- The two examples outside `examples/` moved there: `internal/examples/env` is
  `examples/env-lifecycle` (`go run ./examples/env-lifecycle`) and
  `clients/python/examples/quickstart.py` is
  `examples/python-quickstart/quickstart.py`. `make verify-layers`
  (`hack/verify/layers.py`, in the Go CI job) fails a change that puts an
  example outside `examples/`, a third-party framework import outside
  `integrations/` and `examples/`, an undeclared import in the Python client,
  or a Go import across the layers of `docs/dev/code-layout.md`.
- Python client: `server.address` drops a `dns:///` or `passthrough:///`
  scheme, as the Go client does.
- Python client: `SubstrateEnvClientRuntime.exec_async` and `exec` raise a
  non-retryable `SandboxProtocolError` for a
  `PERMISSION_DENIED` or `FAILED_PRECONDITION` guest error (`ProcessExitedError`
  included), as the Go client's `Retryable` classifies them; it raised a
  retryable `InfrastructureError`.
- Go and Python clients: shared vectors in `proto/testdata/client-parity`
  (executable, semconv mode, `rpc.*` attributes, image ref, error mapping with
  `retryable`, metric families, RPC coverage) replace the tables that each
  suite mirrored by hand (Go parity PR 7).
- Go client: every error keeps the gRPC status in its chain, thus
  `status.Code(err)` now reports `codes.NotFound` for an `env.ErrNotFound`
  error, and `codes.FailedPrecondition` for an `env.ErrProcessExited` error,
  instead of `codes.Unknown`. The `Recv` of `Process.Output` returns the SDK's
  errors instead of the raw status error; `status.Code(err)` still reads it.
- Releases after v0.1.0 no longer build or publish the OpenHands agent-server
  layer (`oh-agent-server-layer`). Build it from `integrations/openhands/layer`
  and push it to your own registry (see `integrations/openhands/README.md`).
  The v0.1.0 image (`oh-agent-server-layer:1.55.0-v0.1.0`) gets no updates: it
  predates the #267 security re-lock. An org admin can delete the package.
- Built against Substrate `274605f1` (2026-11-05), from `475f49b0`. The
  control plane API is unchanged for ate-env (`MintActorCertificate`, which
  ate-env never called, is gone). Three runtime changes reach a template.
  Since `5d226a50` the container runs as the image's `USER` in its `WORKDIR`
  (before: root in `/`); `ate-env-guest` sets no user, so a `--rootfs-image`
  with a non-root `USER` runs the guest, the processes it starts and its
  sidecars as that user, and gVisor snapshots of such templates from before
  the upgrade do not restore: recreate them. Since `fd576d34` `/tmp` and
  `/dev/shm` are a `tmpfs` on both runtimes (`/dev/shm` 64 MiB on gVisor,
  `/tmp` capped at half the actor's memory) and `/run` comes from the image;
  files an image ships under `/tmp` are hidden, and gVisor snapshots from
  before it, goldens included, do not restore. Since `17fa8586`
  `securityContext.capabilities.add` accepts 12 capabilities only; ate-env
  templates set none. The Kind e2e installs Substrate with
  `ate-setup --kind deploy ate-system`, since Substrate removed
  `hack/install-ate-kind.sh`.
- `ate-env manifest template`: `--snapshots-bucket` is optional. Without it
  the template names no `snapshotConfig.storageLocation` and its snapshots go
  to the cluster's `--snapshot-location`. With it, ate-api-server needs the
  `PerTemplateSnapshotLocation` preview gate and the template must be created
  with `kubectl-ate --preview`; ate-env-api sends the same opt-in when it
  derives a template from such a base. Substrate since `d86746d8` rejects the
  field without the gate, so a base template created earlier with a bucket
  must be recreated without one on a cluster that has the gate off.
- Python client: renamed distribution package from `ate-env-client` to
  `ate-env-rl` and import module from `ate_env` to `ate_env_rl`.
- Python client: `warm_pool` pools the actors it created before it raises
  `SandboxStartError`. An overlapping `warm_pool(wait=True)` counts the actors
  another call is still creating and can return before the pool is full.
- Python client: `unwarm_pool`, and a `resize_pool` that shrinks the pool,
  cancel `warm_pool` calls in flight for that pool: those calls delete the
  actors they create instead of pooling them.
- Python client: `load_tasks()` and `add_tasks()` read a dict row as follows.
  The id changes rename a row that loaded before, so `acquire()` by id and
  stored results see the new id.
  - Only `instance_id`: loads with that id (was `ValueError`).
  - No image: `ValueError` (was `KeyError('image')`).
  - Image under `docker_image`, `image_name`, or those keys under `metadata`
    or `env_config.entry`: loads (was `KeyError('image')`).
  - Image with surrounding whitespace: stripped.
  - Blank `image`: the next key is tried (was the image `""`).
  - Non-string image, such as `123`: `ValueError` (was `Task.image == 123`).
  - One-element list id: unwrapped, `["one"]` gives `"one"` (was
    `"['one']"`).
  - Bool id: refused (was `"True"`).
  - Dict or multi-element list id: skipped for the next key (was its
    `str()`).
  - Blank or non-string `template`: no template (was the value).
  - Whitespace-only or binary id: skipped for the next key (was the value);
    a UTF-8 bytes id is decoded (was `"b'...'"`).
- Python client: the R2E-Gym adapter's logger is named
  `ate_env_rl.adapters.r2egym` (was `ate_env_openhands.r2egym`); a logging
  configuration keyed on the old name must follow. `ate_env_openhands.r2egym`
  stays as a re-export of the new module, with the same names.

### Fixed
- Python client: `close_async()` on the caller's loop ends an image pre-flight
  (`FleetConfig.preflight`, `preflight_images_async`) at once, also one that
  is between two requests: in its pool-listing backoff, in the golden poll,
  in a warm call's backoff, or between two batches. Before, the pre-flight
  woke after the close. From the listing backoff it dialed a new client,
  warmed the pools and left that client open until the next close. From the
  other sleeps it returned a short coverage (or `PreflightError` with
  `preflight_golden="fail"`), and `setup()` or `run()` went on to create
  environments on a new client. It now stops with `CancelledError`; a
  pre-flight started after the close runs as before. The sync `close()` from
  another thread ends it too, also on a caller's `client` and before the
  pass dialed its own (before, that `close()` returned without a cancel),
  and a pre-flight that starts while a close runs cannot escape the cancel.
- Go client: `Env.Run` returns `ctx.Err()` (`context.Canceled` or
  `context.DeadlineExceeded`) when the context ends while `StartProcess` is in
  flight or while the streaming output is followed, as the polled run already
  did. Before, those paths returned the gRPC `Canceled` or `DeadlineExceeded`
  status, which `errors.Is(err, context.Canceled)` does not match; this also
  made `TestRunPollCancelKillsGuestProcess` flaky (#333).
- Python client: `SandboxFleet.run()` keeps the error of a failed run when the
  teardown at its end also fails. Before, the teardown's
  `InfrastructureError` replaced it in each strategy (`none`, `naive`,
  `sliding`, `pipelined`): a `KeyboardInterrupt` from a task or a failed warm
  came out as "could not delete N environment(s)". The teardown error is now
  logged as a warning and the run's error is raised; after a run that
  succeeded, the teardown error is raised as before. This is the rule
  `with fleet:` already follows. `with fleet:` on a `SandboxFleet` now also
  logs the teardown error it does not raise, as `async with fleet:` on an
  `AsyncSandboxFleet` does; before, it dropped it without a trace.
  `SandboxFleet.close()` logs a backend close error when the teardown also
  failed; it raises the teardown error, as before.
- `internal/benchmarking/schema/v1/scenario.schema.json` describes every
  field `bench run` reads: it lacked 41 of them (`description`, `seed`,
  `window`, `overlays`, and fields of the SUT, job, window and overlay
  blocks), required a job's `kind` where `mode` is accepted, and refused
  the `hold`, `snapshot` and `release` park policies, so the scenarios
  under `benchmarking/scenarios/` did not validate against it. A test now
  fails when a scenario type gains a field the schema lacks, or when a
  field's type, a `$ref` target or an element type differs.
- `validate_schema.py` without the `jsonschema` package enforces `anyOf`
  (it accepted a scenario job with neither `kind` nor `mode`), and
  `--schema` takes the names `scenario`, `measurements`, `node_cache` and
  `restore_sample`.
- `swebench-loader preflight -events-dir <run>/events` writes its two files
  to `<run>`, next to the shards directory, as documented, where it wrote
  them among the shards; `-out` and `-coverage-out` on one path are refused.
- `swebench-loader preflight -events-dir <run>/events` writes its two files
  to `<run>`, next to the shards directory, as documented, where it wrote
  them among the shards; `-out` and `-coverage-out` on one path are refused.
- `ate_env_openhands.agent_sandbox_rl.SandboxFleet.run` tears down once in its
  own `finally`. Before, a `preflight()` or `plan()` failure of the `sliding`
  or `pipelined` strategy on the last pass skipped the teardown, because those
  strategies call both before their own `try`. The pools the caller warmed
  (`warm_image`, `set_pool_replicas`) and the environments whose earlier
  delete failed then stayed. The pools of earlier `sliding` or `pipelined`
  passes were not among them: those strategies drain each window after use.
  When a pass fails and the teardown fails too, `run` raises the error of the
  pass (a `PreflightError`, a `KeyboardInterrupt`) and logs the teardown
  error; a teardown error after clean passes still raises.
- Go and Python clients: the executable attribute (`ate_env.command.executable`)
  splits shell operators (`;`, `&`, `|`, `&&`, `||`, redirections) and skips
  comments, so an assignment after an operator or in a comment is never
  recorded. It holds the first command of a compound line (`cmd1 && A=s cmd2`
  gives `cmd1`) and records nothing when an assignment's value or a redirection
  target holds `$` or a backtick (`A="$(echo my secret)" cmd`,
  `<<<"$(echo my secret)" cmd`) or when a redirection has no target or is
  followed by an operator (`2>&1<<<x cmd`). A quoted or escaped character is
  never shell syntax (`"A=b" x` is not an assignment, `2''>out` has no fd
  number, `'(' x` gives `(`), a carriage return is part of a word as in bash,
  and a name longer than 255 bytes records nothing. An arithmetic command
  (`((x+1))`), or a command word that holds `{` or `[` (a brace expansion
  such as `{mysql,-pSEKRET}`, an array subscript), records nothing. Two
  cases that recorded a name now record nothing: an argv list whose first
  item is an assignment (`["API_KEY=s3cr3t", "python3", "x.py"]` gave
  `python3`), and a first word that holds `=`, `$` or a backtick (`./a=b`
  gave `a=b`).
- `preflight-coverage.json` (schema v1) gains two optional fields per image,
  `undeleted_probes` and `waited_ms`, the ones `WarmImage`'s coverage already
  carries. `swebench-loader preflight` and `ate_env_rl.PreflightWarmer` list
  the probes whose delete failed there (and the probes of a call that broke
  before it reported its deletes) instead of only logging them, so an
  operator has the list to sweep. The loader's pass exits non-zero naming
  them even when the coverage is complete; the Python `run()` returns
  normally with `complete` still `True`, and `PreflightCoverage.clean`
  (`complete` and no `undeleted_probes`) is the flag to gate a run on;
  `shortfall()` names the leftovers in both.
- `swebench-loader preflight` and `ate_env_rl.PreflightWarmer` no longer delete an
  environment whose name a probe collided with: the probes are `WarmImage`'s,
  which reports a name that exists as a failed probe and leaves it alone
  (the warmers create and delete nothing themselves).
  The default run id is `pf-<6 random characters>`, unique per
  pass, as `WarmImage`'s is, instead of the fixed `pf`.

- ate-env-guest: the concurrency slot of a process is released before its
  exit is observable, through the exit message of the output stream or
  `GetProcess`. A caller that chains a wait and a start at
  `MaxConcurrentProcesses` no longer gets `ResourceExhausted` from the start
  (#332).
- `bench run -overlay`: every behavioral overlay field now takes effect.
  Before, only `prewarm_policy` (the footing) and `warm_pool_size` (the
  acquisition source) did; `park`, `park_policy`, `prefetch`,
  `lookahead_steps`, `snapshot_mode`, `sut`, `env_vars` and `flags` were
  read and dropped, and `warm_pool_size` did not reach the manifest's
  `pool_size_per_image`. An A/B pair that set those fields compared two
  equal runs, so it gives different results after the upgrade. `flags`
  reach each real job's `trainer_command` only, as `-name=value`; the
  ate-env-api arguments come from `sut.extra_args`. `snapshot_mode` sets
  the jobs' snapshot loop and does not change their park policy, and a
  cold `prewarm_policy` keeps the base pool size. The executed schedule's
  prefetch hints come from the schedule generator's function, so a job
  gets the same hints with and without an overlay; the generator now
  honors a job's `lookahead_steps` (it always looked one step ahead), which
  changes `schedule.json` for a job with `lookahead_steps` above 1.
  `system_description.json` `overlays[0]` records the resolved
  `park_policy`, `snapshot_loop`, `prefetch` and `lookahead_steps`, because
  `schedule.json` on disk stays the base schedule on both sides of a pair,
  and the overlay's inputs in three fields: `sut_overrides` holds its
  `sut.extra_args`, and the new `flags` and `env_var_names` hold its
  `flags` and the sorted names of its `env_vars`; an `env_vars` value never
  reaches the file, which `bench upload` publishes.
  A pool size from the overlay (`warm_pool_size` or
  `sut.pool_size_per_image`) turns `sut.pool_enabled` on unless the overlay
  sets it, so `pool-on` on a base with the pool off acquires from its pool;
  before, both arms of `pool-on,pool-off` acquired cold on such a base. The
  simulated acquisition source now follows the scenario's footing and pool
  fields with and without `-overlay`: a run without one on a scenario with
  `footing: cold` or `sut.pool_enabled: false` records `cold_start`; before,
  it always recorded `warm_pool`.
- `bench run`: the `prewarm` event, `lifecycle_policy` and the footing
  Validity Rule 1 checks follow the acquisition source, with and without
  `-overlay`. Before, the three followed the scenario's `footing` alone, so
  a pool on a cold or omitted footing recorded `warm_pool` tasks beside
  `mode: "ephemeral"` and no `prewarm` event, and `sut.pool_enabled: false`
  on a warm footing recorded `cold_start` tasks beside `mode: "prewarmed"`.
- `bench run`: a job with `prefetch: false` emits no `prefetch_hint` events,
  with and without `-overlay`. Before, the base schedule's hints ran unless
  the overlay set `prefetch` or `lookahead_steps`. `schedule.json` on disk
  stays the base schedule.
- `bench run -overlay`: an `overlays:` entry in the scenario takes its name
  from the map key, as documented; an inner `name` must match the key.
  Before, an entry without an inner `name` failed the scenario's validation.
  A name must not be `.`, `..` or hold `/` or `\`, and a pair needs two
  different names; before, `../x` wrote outside `-dest` and
  `pool-on,pool-on` overwrote arm A with arm B. `flags` need a real job with
  a `trainer_command`; before, a `trainer_manifest` job dropped them while
  `overlays[0].flags` recorded them as active. An empty `env_vars` or
  `flags` name is an error, and so are a `prewarm_policy` other than `warm`,
  `cold` or `none`, a `snapshot_mode` other than `sync`, `async`, `none`,
  `off` or `disabled`, a negative `warm_pool_size` or `lookahead_steps` and
  a `sut` the scenario's own rules reject; before, `prewarm_policy: "off"`
  or `warm_pool_size: -4` was dropped and the run kept the base footing.
  A negative `sut.pool_size_per_image` or `sut.max_active_pods` is an error
  on the scenario's `sut` and on an overlay's `sut`, as the schema says;
  before, an overlay's negative count was dropped, and one on the scenario's
  `sut` reached a `system_description.json` that fails its schema.
  `overlays[0]` `park_policy`, `prefetch` and `lookahead_steps` come from
  the closed-loop jobs; before, an open-loop job, which ignores them, could
  leave them unset.
- `scenario.schema.json` lists every `sut`, scenario and window field
  (`pool_enabled`, `pool_size_per_image`, `overlays`, `footing`, `seed`,
  `description`, `window`, `cooldown_steps` and the rest). Before, a scenario
  with one of them failed the schema with "unexpected additional
  properties" although `bench run` accepted it. The schema rejects an empty
  `env_vars` or `flags` name in an overlay, as `bench run` does, and a
  decoded scenario marshals an `overlays:` entry without its empty `name`,
  so the output passes the schema; before, it emitted `name: ""`.
  `validate_schema.py` enforces `propertyNames` when the `jsonschema`
  package is absent, and so `allOf` and `minProperties`; before, its
  fallback skipped the three keywords, so an empty `overlays` key, a
  `variants` entry of any shape and an empty `measurements` object passed
  it. The fallback now fails on a schema keyword it does not enforce
  instead of skipping the rule.
- swebench-loader: the working-set counters in `report.json`. In a
  `-park fork` run, `created` counts the golden base after its create and
  each child after its fork, and `retired` counts a base whose suspend
  failed or that lost the race to another cold acquire of its image; before,
  `created` counted one per cold image and no child, thus `retired` ran far
  above it. In a `-park suspend-async` run, a parked sandbox whose local
  suspend failed is deleted and counted in `retired`, and the next task of
  its image goes to a cold create; before, that task failed with the suspend
  error: one more in `failed`, and a tier 1 dropped rollout in
  `fault_classification` next to the tier 3 `snapshot_restore_failure` the
  suspend event already carries, thus `infra_fault_rate` goes down by those
  drops. A prewarmed sandbox the pool deletes counts in `retired` too, and
  an acquire cancelled while its parked sandbox is still suspending puts the
  sandbox back in the pool, or retires it when the pool filled up while the
  acquire waited, instead of leaving it behind or evicting a settled
  sandbox for it. The pool deletes a sandbox it gives up on in the
  background and the run's cleanup waits for those deletes, thus a slow
  delete does not hold the acquire that goes on to a cold create; a delete
  that fails is logged with the sandbox id. A `-prefetch` run cancelled
  while a prewarm is in flight waits for the prewarm at cleanup and deletes
  the sandbox it made, instead of leaving it parked in a pool already
  closed. An acquire whose context expired before it ran leaves the pool as
  it is and counts nothing, and a `-park fork` acquire whose fork fails
  counts no hit; before, both could count a hit for a task that got no
  sandbox. A task whose acquire fails (a cancelled run, a fork error, a
  failed base suspend) or whose trajectory does not render is recorded as
  failed and the run goes on to its cleanup; before, the task event
  crashed the loader on the missing trajectory before the cleanup ran, and
  the step's sandboxes and a prewarm in flight stayed behind.
- swebench-loader: a step the guest kills at its timeout (exit 137 at or
  past the deadline) fails the task and stops the trajectory, as
  `run_fleet_bench.py` does on its `TimeoutError`. Before, the Go arm marked
  the step ok and ran on, so the two arms disagreed on `failed`.
- `integrations/nemo-gym`: `create` seeds a `bytes` value of
  `SandboxSpec.files` unchanged. NeMo Gym `main` allows `str | bytes` there
  (binary files such as images), and a `bytes` value failed the create with
  `AttributeError`.
- `integrations/nemo-gym`: `exec(user=...)` no longer raises `ValueError`.
  NeMo Gym `main` sends `user` on many commands (the Harbor environment
  server's default user, `user="root"` from the transfer helpers), and each
  one failed. The provider now probes the image once per sandbox and runs the
  command through `sudo -n -u <user>` when the image's user has a passwordless
  sudo, or through `su` when the image runs as root; a user that is the
  image's own runs as is. An image with neither, one whose sudo wants a
  password, and a uid under `su` return code 125 with
  `error_type="user_switch_unavailable"`, which Gym's transfer helpers answer
  by running as the default user.
- `integrations/nemo-gym`: `serialize_handle` waits for an idle park that is
  under way, and an idle park that finds the sandbox shared once it holds the
  parking lock does nothing. Before, a descriptor could name a sandbox the
  owner was still parking, and the borrower's first command failed with
  `error_type="sandbox"`.
- `integrations/nemo-gym`: concurrent `exec(user=...)` calls on a new sandbox
  share one user probe. Before, each ran its own.
- ate-env-api: `WarmImage` deletes a probe whose create the control plane
  commits after the probe's delete (a create attempt cut off by its deadline,
  a cancel or `Unavailable` while the control plane queues, even when a later
  attempt made the probe). Before, such a probe stayed `SUSPENDED`, untracked
  and missing from `undeleted_probes`. The delete of such a probe is now
  repeated for a short settle window, and each round lists the atespace once
  before it gives its probe slots back and deletes any of its probes that
  appeared later. When that list fails, those probes are listed in
  `undeleted_probes`. A create that the control plane commits after the
  settle window and the round's list still leaks and is not listed. A round
  with such a probe holds its slots about 2 s longer, and a cancelled call
  returns about 2 s later.
- OpenHands layer: security re-lock of the agent-server dependencies for the
  open Dependabot alerts: pillow 13.0.0, pypdf 6.21.4, pyasn1 0.6.5, certifi
  2026.10.23, fastmcp and fastmcp-slim 3.4.11, oauthlib 4.0.1, setuptools
  84.2.1. filelock stays at 3.32.7, because each stable litellm from 1.104
  requires filelock<4.0. `lock.sh` now starts from the committed lock and
  passes its arguments to `uv pip compile`.
- ate-env-api port proxy: both forms forward the path exactly as sent when it
  holds bytes such as `{ } | ^` or raw UTF-8. They were re-escaped, and an
  escaped slash next to them was decoded: `/a{b%2Fc` reached the daemon as
  `/a%7Bb/c`. The path form now matches its routing prefix on the path as sent,
  so an escaped slash in the prefix no longer routes to an environment.
- Python client: `RunReport.to_dict()` keeps every phase's
  `0 <= max_s <= total_s <= count * max_s`. It rounded `total_s` and `max_s`
  to 0.01 separately, so 3 phases of 0.004 s reported `total_s` 0.01 and
  `max_s` 0.0. `max_s` now rounds up to the next 0.01 and `total_s` stays
  between `max_s` and `count * max_s`, which can leave it one float step off
  the 0.01 grid (5 x 0.09 s gives 0.44999999999999996). `summary()` prints
  the same rounded values (#273).
- swebench-loader `-park-each`: the last step deletes each environment right
  after its replay instead of suspending it (unless `-keep`). A 37,000
  environment run suspended them all, one more snapshot upload each. The
  cleanup then deleted them in one burst at about 1,850 per second; 15,031
  deletes failed on GCS 429 and leaked.
- swebench-loader: `-park suspend-async` suspends locally through ate-env-api
  (`SuspendLocal`: the call returns once the snapshot is on the node and the
  upload goes on in the background), as it already did through direct
  Substrate access. It sent a durable suspend and waited for the upload: p50
  0.8 s, p95 9 s, max 46.5 s over 180,000 suspends in one run. `-park-each`
  parks with `suspend-sync` and `suspend-async`, which it silently skipped,
  and rejects `fork`. A run that parks with `suspend-async` or `fork` (whose
  park is a local suspend too) checks that ate-env-api serves `suspend_mode`
  before it starts. `report` takes `park_mode` from the run's `park` label,
  so a `-park-each` run, which records no `park` event, reports
  `suspend-async` or `suspend-sync` instead of `suspend`.
- ate-env-api: deriving an image template from a base that already has a
  non-image volume named `guest`, or a mount at or under `/ate`, fails with
  `InvalidArgument` naming the clash, instead of creating a template that
  Substrate rejects for a duplicate volume or nested mounts.
- ate-env-api: deriving an image template from a base that already mounts
  the guest image volume fails with `InvalidArgument` when another volume is
  mounted at or under `/ate` or the guest is not mounted exactly once at
  `/ate`, instead of creating a template whose guest is not alone at `/ate`.
  It also fails when the base's command does not start the guest from the
  mount (no command, or a first argument that is not the guest binary under
  `/ate`, such as `/ko-app/ate-env-guest`), instead of creating a template
  whose guest is not found at boot, and when its guest volume's image is not
  pinned by digest, as a base that runs the guest as its image already must
  be.
- ate-env-api: a guest call that waits for its environment to wake keeps
  waiting through a transient control-plane error on the state read
  (`Unavailable`, `DeadlineExceeded`, or `ResourceExhausted` such as a quota)
  instead of failing at once with it. A slow state read no longer holds the
  wait past `-wake-budget`, and no longer delays the probe that ends a
  no-capacity run past `-wake-no-capacity-grace`. A state read that hangs
  without an error is cut at `-wake-state-read-cap`, and the wait probes again
  instead of sitting with no probe until `-wake-budget`. When all reads are
  cut, the wait probes once per `-wake-state-read-cap` plus backoff and ends at
  `-wake-budget` with `Unavailable` "while UNKNOWN".
- ate-env-api: a guest call whose caller context cancels or expires while its
  wake probe is parked at the router returns `Canceled` or `DeadlineExceeded`
  instead of treating the unwoken environment as awake and recording
  `create_to_ready` and `wake_wait` as ready.
- ate-env-api port proxy: the path form
  (`/v1alpha/envs/<id>/ports/<port>/...`) forwards the rest of the path
  exactly as sent; it re-escaped `! ' ( ) *` (`/report(1).txt` reached the
  daemon as `/report%281%29.txt`) when the path held no other escape.
- ate-env-api: a `CreateEnvironment` whose derived image template was created
  by a concurrent call for another image (two digests sharing their first 12
  hex digits) fails with `FailedPrecondition` instead of running that image.
- ate-env-api: a base template whose guest command climbs out with `..`
  (`/../ko-app/ate-env-guest`) is re-rooted under `/ate` instead of deriving
  templates whose guest is not found at boot.
- Python client: environments no longer outlive `teardown()`. A recycle,
  `warm_pool` call or failed warm claim that overlaps a teardown deletes its
  environment instead of pooling it, and the warm-replica gauge stays equal to
  the warm pool. An environment whose delete fails or is cancelled stays
  owned, and the next `reap` of any run deletes it. An environment that an `acquire`
  creates while its own run's teardown runs is also deleted by the next `reap`
  of any run, unless the caller releases it first.
- Python client: an `acquire` cancelled while its environment's create is in
  flight, for example by its own `timeout_s`, no longer leaks the environment
  if the create reached Substrate. The driver keeps it, and the next `reap` of
  any run deletes it; if a teardown ran during the create, the driver starts
  its delete in a background task, which `reap` and `close()` wait for (next
  entry).
- Python client: a warm claim that pauses in `get()` across its own run's
  teardown and then creates an environment, or claims one that another run
  pooled after the teardown, no longer leaves that environment under the torn
  down run id: the next `reap` of any run deletes it, unless the caller
  releases it first. `reap` and `close()` now wait, up to
  `SubstrateBackendDriver(background_delete_timeout_s=10.0)`, for the
  background deletes that cancelled `acquire` and `warm_pool` calls start, so
  a teardown no longer returns, and `close()` no longer stops the event loop,
  with such a delete in flight. A delete still running at the deadline, or
  one that failed since the last such wait, is named in the
  `InfrastructureError` the reap raises (and in a warning from `close()`),
  and its environment stays tracked for the next `reap` of any run.
  `background_delete_timeout_s` must be a finite number `>= 0`; `0` means
  no wait. `LoopRunner.stop()` waits for the tasks a cancel handler starts
  before it stops the loop. `close()` first cancels the calls still in flight
  on its own event loop, so the delete of an `acquire` still in `create()`
  lands before the channels close; that wait skips a delete whose
  environment a `reap` already deleted. `AsyncSandboxFleet.close_async()` is
  new, and `async with` awaits it on exit instead of blocking the event loop
  in `close()`. `close()` and `close_async()` count as a teardown: an
  `acquire` or `warm_pool` call they cancel, or whose create their channel
  close cancels, deletes its environment even when no `reap` ran since the
  call began, and a recycle or `warm_pool` that finishes during them deletes
  instead of pooling. A `release` (recycling or not) that they cancel, and
  the delete of a `warm_pool` call that finished after the `reap` and that
  they cancel, start a background delete too, which they wait for the same
  way: before, the cancelled call kept its environment tracked for a `reap`
  that never followed. A `reap` still in flight when `close()` or
  `close_async()` starts (a `teardown()` on another thread, say), which they
  cancel or whose deletes their channel close cuts off, starts a background
  delete for each environment it had not deleted, which they wait for too:
  before, those environments stayed alive and tracked by a driver that was
  closed. `close_async()` cancels no task of the caller's, so its channel
  close let the part of a `reap` or `warm_pool` call that waited for one of
  the call's 16 slots go on: it sent its delete or create on a new channel,
  and `close_async()` returned with those environments alive, their deletes
  not waited for and that channel open. Such a part now stops when it gets
  its slot during a close and deletes in the background like the ones the
  close cut off, and `close_async()` repeats its wait (4 rounds at most)
  while a channel that a delete opened is still there. An `unwarm_pool` or
  a shrinking `resize_pool` that is cancelled now leaves every environment
  it took out of the pool tracked for the next `reap` of any run: before,
  one whose delete had not started (more than 16 at once, or a cancel
  right after the call began) kept the run id it was warmed under, so only
  that run's `reap` deleted it. When `close()` or `close_async()` cuts such
  a call off, each environment it had not deleted gets a background delete
  that they wait for, as for a `reap`; the same holds for the deletes of a
  `warm_pool` call that finished after a teardown and that `close()`
  cancels before they start.
  A `release` (recycling or not) that is cancelled, or whose delete
  fails, after a `reap` of any run began since the release started now
  deletes its environment in a background task, which a `reap` still running
  waits for, as a cancelled `acquire` does: before, the environment stayed
  alive and tracked until the next `reap`, which a fleet that reaps its run
  once never ran. A background delete that fails leaves the environment
  tracked for the next `reap` of any run, as before; with no `reap` since
  the release started nothing changes. An `acquire` cancelled while it
  checks a warm
  environment (by its `timeout_s`, the caller, or `close()`) returns the
  environment to the warm pool, or deletes it if a teardown ran since the
  claim: before, it stayed alive outside the pool until a `reap` of the run
  that created it, which for an environment recycled by an earlier run never
  came.
