# sandboxenv-rl code layout

Companion to [docs/architecture.md](../architecture.md) and [AGENTS.md](../../AGENTS.md).

## Repository layout

```
cmd/
  ate-env/               # CLI: environment management and manifest generation (ate-env manifest)
  ate-env-api/           # API server daemon: gRPC services, MCP handler, port proxy
  ate-env-guest/         # Guest daemon: Linux PID 1 subreaper init, sidecar supervision
  ate-env-reporter/      # Fleet inventory reporter daemon: one snapshot per interval to a bucket or a directory
  benchmarking/
    bench/               # Composable RL benchmark CLI (scenario lifecycle subcommands)
    swebench-loader/     # Benchmark driver: SWE-bench trajectories, placement, preflight, event recording, reports
internal/
  apiservice/            # gRPC server implementation: auth, image template derivation, wake retries
  ate/                   # Substrate client adapter: dials ateapi control plane and atenet router
  benchmarking/          # Benchmark packages: job, measure, overlay, schema, sut, swebench, trace
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

### `internal/apiservice` vs. `internal/ate` vs. `guest/`

Put code in `internal/ate` for Substrate `ateapi` calls or `internal/mcp` connection pools, `guest/`
for in-actor execution, and `internal/apiservice` for API handlers. `apiservice` calls
`internal/ate` for lifecycle and dials `guest/` via `s.guestConn`; it must not run local commands or
import `guest/`.

### `clients/go` vs. `internal/ate`

Put public Go SDK code (`package env`) in `clients/go` and internal control-plane client code in
`internal/ate` (`cmd/ate-env` imports `clients/go`, `apiservice`, `hostcheck`, `routertls`, and
`sidecarenv`). External clients talk to `ate-env-api`, never to Substrate directly. Outside tests,
`clients/go` imports only `proto/ateenv/v1alpha` from this repo (never `internal/`, `go.ate.dev/`,
or `k8s.io/`); shared values like `DefaultAtespace` are copied and tested equal, enforced by
`hack/verify/go-client-deps.sh` in the Go CI job. A feature that one Go consumer needs goes into
`clients/go` when a second consumer can use it.

### `internal/` vs. `guest/`

Put code in `internal/` if only this repository's binaries use it, and in-actor services in `guest/`
(`guest/` imports `internal/unknownfields`; `cmd/ate-env-guest` imports `internal/sidecarenv` and
`internal/telemetry`). Neither must import `internal/ate` or `go.ate.dev/substrate`.
`hack/verify/layers.py` enforces this, and that `internal/`, `clients/` and `cmd/` (the guest's own
binary aside) do not import `guest/` outside tests, in the Go CI job.

### New binary vs. new package

Create a `cmd/<binary-name>` binary only for an independently deployed daemon or CLI; place library
code under `internal/<package-name>`.

### Where protos and generated stubs go

Define in-guest services (`ProcessService`, `FileSystemService`) in
`proto/ateenv/v1alpha/guest.proto` and lifecycle services (`EnvironmentService`) in
`proto/ateenv/v1alpha/env.proto`. Never hand-edit `*.pb.go`, `*_grpc.pb.go`, or Python `*_pb2*.py*`
files. Run `buf generate` for Go stubs and `make python-protos`
(`clients/python/scripts/gen-protos.sh`) for Python stubs in `clients/python/src/ate_env_rl/_gen/`.

### Python SDK: low-level vs. fleet vs. adapters

Put low-level gRPC wrappers in `clients/python/src/ate_env_rl/{client,env,handle}.py`, and pool
scheduling, warm pools, and rollout drivers in `ate_env_rl/{fleet,async_fleet,strategies}.py` (sync
methods must run via `LoopRunner.run()` for HTTP/2 channels). Put task converters and handle-bound
evaluation adapters without third-party SDK deps in `clients/python/src/ate_env_rl/adapters/`
(`swebench.py`; `r2egym.py`, re-exported by `ate_env_openhands.r2egym`), and packages that pin
external agent frameworks or ship runtime layers under `integrations/`.

### Benchmarking: `cmd/benchmarking` vs. `internal/benchmarking` vs. `benchmarking/`

Put CLI entrypoints in `cmd/benchmarking/bench` and `cmd/benchmarking/swebench-loader`. Put reusable
Go benchmark packages in `internal/benchmarking/` (`job`, `measure`, `overlay`, `schema`, `sut`,
`swebench`, `trace`). Put scenario manifests in `benchmarking/scenarios/`, report tools in
`benchmarking/telemetry_report/`, and shell scripts in `benchmarking/swebench/`.

### Integrations and examples

Put an integration with a third-party framework in `integrations/<framework>/`: its own Python
distribution (`pyproject.toml`, `src/`, `tests/`, `README.md`), a uv workspace member, and a row in
`integrations/README.md`. It imports `ate_env_rl`; `ate_env_rl` never imports it. Only that
integration and `examples/` import the framework; nothing under `clients/`, `benchmarking/`,
`internal/` or `hack/` imports a framework, an integration or an example.

Put a runnable example in `examples/<name>/` with a `README.md` and a row in `examples/README.md`. A
Go example is a `main` package that imports no `internal/` package; no other directory is named
`examples`, and an example has no `pyproject.toml`. `ate_env_rl` imports only the dependencies its
`pyproject.toml` declares, the optional ones and `r2egym` (`CORE_LAZY_OPTIONAL`) lazily; only the
packages the script lists import `go.ate.dev/`. `hack/verify/layers.py` enforces this section and
the top-level directory list (`make verify-layers`, Go CI job); add a new top-level directory to the
tree above and to the script in the same pull request.

### Test doubles and fixtures

Never write custom Substrate mocks; reuse `internal/internaltest`: `fakecontrol` (in-memory
`ateapi`), `fakerouter` (`atenet` router), `synctestrapid` (`rapid` in `testing/synctest`), and
`testcerts` (SPIFFE certs).

### Agent workflows in `.agents/skills/`

- [create-release](../../.agents/skills/create-release/SKILL.md): release notes, tags, PyPI, GHCR.
- [extend-env-surface](../../.agents/skills/extend-env-surface/SKILL.md): proto, stubs, guest,
  `s.guestConn`/`ensureAwake`, and SDKs.
- [maintain-docs](../../.agents/skills/maintain-docs/SKILL.md): style and validation paths.
