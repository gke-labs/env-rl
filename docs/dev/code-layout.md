# sandboxenv-rl code layout

Companion to [docs/architecture.md](../architecture.md) and [AGENTS.md](../../AGENTS.md).

## Repository layout

```
cmd/
  ate-env/               # CLI: environment management and manifest generation (ate-env manifest)
  ate-env-api/           # API server daemon: gRPC services, MCP handler, port proxy
  ate-env-guest/         # Guest daemon: Linux PID 1 subreaper init, sidecar supervision
  ate-env-reporter/      # Fleet inventory reporter daemon: one snapshot per interval to a bucket or a directory
  ate-env-fleet-site/    # Fleet site server: read-only pages and JSON API over the snapshots, behind IAP on Cloud Run
  benchmarking/
    bench/               # Composable RL benchmark CLI (scenario lifecycle subcommands)
    swebench-loader/     # Benchmark driver: SWE-bench trajectories, placement, preflight, event recording, reports
internal/
  apiservice/            # gRPC server implementation: auth, image template derivation, wake retries
  ate/                   # Substrate client adapter: dials ateapi control plane and atenet router
  benchmarking/          # Benchmark packages: job, measure, overlay, schema, sut, swebench, trace
  envstore/              # Environment metadata store: per-environment ConfigMaps (labels/deadlines) and orphan sweep
  fleetsite/             # Fleet site: IAP assertion check, snapshot cache, BigQuery actor events, pages, JSON API
  hostcheck/             # HTTP middleware: DNS-rebinding defense on Host headers
  internaltest/          # Test doubles and bridges: fakecontrol, fakerouter, synctestrapid, testcerts
  inventory/             # Fleet inventory snapshot: v1 JSON Schema, strict decoder, Substrate state vocabulary
  mcp/                   # Streamable HTTP MCP server: maps shell and file tools to guest gRPC
  portproxy/             # HTTP/CONNECT reverse proxy: tunnels ports inside actors via atenet
  reporter/              # Fleet inventory snapshot writer: control-plane and Kubernetes lists, bucket and directory stores
  routertls/             # TLS config: dynamic SPIFFE client certificate and CA reload
  sidecarenv/            # Sidecar-only env: ATE_SIDECAR_ENV_<NAME> on the container; Take strips it for the sidecars
  telemetry/             # OpenTelemetry tracing setup and Prometheus metric collectors
  tool/                  # Generic MCP tool registry
  unknownfields/         # gRPC interceptors rejecting requests with unknown protobuf fields
guest/
  server.go              # Guest gRPC server listener configuration
  filesystem/            # FileSystemService: workspace-confined file streaming (DefaultReadBufferSize = 64 KiB)
  process/               # ProcessService: process tracker, PTY allocator, signal delivery, log spools
proto/ateenv/v1alpha/
  env.proto, guest.proto # EnvironmentService, ProcessService, FileSystemService protobuf definitions
  *.pb.go, *_grpc.pb.go  # [generated] Go protobuf and gRPC stubs; never hand-edit
proto/testdata/client-parity/ # shared Go/Python client parity conformance JSON vectors
clients/
  go/                    # Go SDK client: package env (Client, Env, Process, StartError)
  python/                # Python SDK: distribution package ate-env-rl, import name ate_env_rl
    src/ate_env_rl/      # Core library: Client, Env, Process, SandboxFleet, AsyncSandboxFleet, config.py, sources.py
    src/ate_env_rl/_gen/ # [generated] Python protobuf and gRPC stubs; never hand-edit
    src/ate_env_rl/adapters/ # Benchmark task converters and evaluation adapters (SweBenchAdapter, r2egym)
    src/ate_env_rl/backend/ # Fleet drivers: SubstrateBackendDriver, MockBackendDriver
    src/ate_env_rl/runtime/ # Guest hooks: SubstrateEnvClientRuntime, MockRuntimeHook
    src/ate_env_rl/telemetry/ # Telemetry: Prometheus metric collectors and OpenTelemetry tracer
integrations/            # Framework integrations: e2b (envd runtime layer), harbor (Harbor), nemo-gym (NeMo Gym), openhands, uni-agent (R2E-Gym)
benchmarking/            # Benchmark scenarios (scenarios/), SWE-bench (swebench/), telemetry report (telemetry_report/)
docs/                    # Architecture, glossary, release, rootfs images, security, dev guides
examples/                # Examples: e2b-envd-layer, env-lifecycle, grpo, harbor, harbor-tools-layer, openhands, python-quickstart, verl_swebench, Qwen3-1.7B-verl, Qwen3.5-35B-A3B-verl, Qwen3.5-397B-A17B-verl, runtime-layers, guest-daemon, mcp
hack/                    # Repository scripts: CI (ci/), release (release/), verification (verify/, boilerplate/)
```

## Decision rules

### `internal/apiservice`, `internal/ate`, `internal/`, and `guest/`

Put Substrate `ateapi` calls and `internal/mcp` connection pools in `internal/ate`, repo-internal
code in `internal/`, in-actor execution in `guest/` (`guest/` imports `internal/unknownfields`;
`cmd/ate-env-guest` imports `internal/sidecarenv` and `internal/telemetry`), and API handlers in
`internal/apiservice` (`apiservice` calls `internal/ate` for lifecycle and dials `guest/` via
`s.guestConn`, and must not run local commands). Neither `guest/` nor `cmd/ate-env-guest` must
import `internal/ate` or `go.ate.dev/substrate`, and `internal/`, `clients/`, and `cmd/` (except
`cmd/ate-env-guest`) must not import `guest/` outside tests (`hack/verify/layers.py`, Go CI job).

### `clients/go` vs. `internal/ate`

Put public Go SDK code (`package env`) in `clients/go` and internal control-plane client code in
`internal/ate` (`cmd/ate-env` imports `clients/go`, `apiservice`, `hostcheck`, `inventory`,
`portproxy`, `routertls`, and `sidecarenv`). External clients talk to `ate-env-api`, never to
Substrate directly. Outside tests, `clients/go` imports only `proto/ateenv/v1alpha` from this repo
(never `internal/`, `go.ate.dev/`, or `k8s.io/`); shared values like `DefaultAtespace` are copied
and tested equal (`hack/verify/go-client-deps.sh`, Go CI job). A feature that one Go consumer needs
goes into `clients/go` when a second consumer can use it.

### Binaries, protos, and generated stubs

Create `cmd/<binary-name>` only for an independently deployed daemon or CLI; put library code in
`internal/<package-name>`. Define guest services (`ProcessService`, `FileSystemService`) in
`proto/ateenv/v1alpha/guest.proto` and `EnvironmentService` in `proto/ateenv/v1alpha/env.proto`.
Never hand-edit `*.pb.go`, `*_grpc.pb.go`, or `*_pb2*.py*`; run `buf generate` for Go and
`make python-protos` (`clients/python/scripts/gen-protos.sh`) for Python in
`clients/python/src/ate_env_rl/_gen/`.

### Python SDK: low-level vs. fleet vs. adapters

Put low-level gRPC wrappers in `clients/python/src/ate_env_rl/{client,env,handle}.py`, and pool
scheduling, warm pools, and rollout drivers in `ate_env_rl/{fleet,async_fleet,strategies}.py` (sync
methods must run via `LoopRunner.run()` for HTTP/2 channels). Put task converters and handle-bound
evaluation adapters without third-party SDK deps in `clients/python/src/ate_env_rl/adapters/`
(`swebench.py`; `r2egym.py`, re-exported by `ate_env_openhands.r2egym`), and packages that pin
external agent frameworks or ship runtime layers under `integrations/`.

### Benchmarking, integrations, and examples

Put CLI entrypoints in `cmd/benchmarking/{bench,swebench-loader}`, reusable Go packages in
`internal/benchmarking/` (`job`, `measure`, `overlay`, `schema`, `sut`, `swebench`, `trace`),
scenario manifests in `benchmarking/scenarios/`, report tools in `benchmarking/telemetry_report/`,
and shell scripts in `benchmarking/swebench/`. Put third-party integrations in
`integrations/<framework>/` (`pyproject.toml`, `src/`, `tests/`, `README.md`, uv workspace member,
row in `integrations/README.md`); they import `ate_env_rl`, `ate_env_rl` never imports them, only
that integration and `examples/` import the framework, and nothing under `clients/`,
`benchmarking/`, `internal/`, or `hack/` imports a framework, integration, or example. Put runnable
examples in `examples/<name>/` (`README.md`, row in `examples/README.md`, no `pyproject.toml`, no
other directory named `examples`; Go `main` packages import no `internal/`). `ate_env_rl` imports
only `pyproject.toml` dependencies (optional ones and `r2egym` in `CORE_LAZY_OPTIONAL` lazily), and
only listed packages import `go.ate.dev/`. `hack/verify/layers.py` (`make verify-layers`, Go CI job)
enforces this section and top-level dirs; update the tree above and the script together.

### Test doubles and agent workflows

Never write custom Substrate mocks; reuse `internal/internaltest`: `fakecontrol` (`ateapi`),
`fakerouter` (`atenet`), `synctestrapid` (`rapid` in `testing/synctest`), and `testcerts` (SPIFFE).
Skills in `.agents/skills/`: [create-release](../../.agents/skills/create-release/SKILL.md)
(releases), [extend-env-surface](../../.agents/skills/extend-env-surface/SKILL.md) (proto, guest,
`s.guestConn`/`ensureAwake`), [maintain-docs](../../.agents/skills/maintain-docs/SKILL.md) (docs).
