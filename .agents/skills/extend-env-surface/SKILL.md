---
name: extend-env-surface
description: >-
  Guide to adding or extending an RPC, environment operation, guest capability, MCP tool, or SDK
  method in sandboxenv-rl without re-implementing runtime logic, bypassing Substrate, or introducing
  race conditions.
---

# Extending the Environment Surface

Use this procedure when adding an RPC, environment operation, guest capability, Model Context
Protocol (MCP) tool, or SDK method to `sandboxenv-rl`.

See [docs/architecture.md](../../../docs/architecture.md),
[docs/glossary.md](../../../docs/glossary.md), and
[docs/dev/concurrency-and-pitfalls.md](../../../docs/dev/concurrency-and-pitfalls.md) for
background.

## Architecture boundaries

Keep responsibilities strictly separated across layers:

1. **`guest/process` and `guest/filesystem` own execution**: In-environment logic (starting
   processes, streaming spools, reading or writing files) lives exclusively inside `ate-env-guest`.
2. **`internal/apiservice` is a thin proxy**: `ate-env-api` handles authentication, manages wake
   retries via `ensureAwake`, and forwards requests to `ate-env-guest` over `atenet` via
   `s.guestSession`. Never execute commands or buffer whole files inside `ate-env-api`.
3. **Router dial paths and wake boundaries (Position A)**: All new gRPC proxy RPCs on `ate-env-api`
   must dial via `s.guestSession(ctx, atespace, envID, forceWake)`, which executes `s.ensureAwake`
   (or uses `boundedAwakeCache` under `-reactive-wake` when `forceWake` is `false`) and attaches
   `ate-target-actor`. `internal/ate.Client.DialGuest` is used exclusively by `internal/mcp` and
   lacks `ensureAwake` (known gap). Never dial `atenet` directly without going through
   `guestSession` or `DialGuest`.
4. **`internal/mcp` adapts to guest RPCs**: Tools (`shell`, `read_file`, `write_file`) translate
   JSON inputs into `ProcessService` and `FileSystemService` calls over `DialGuest`. Never bypass
   the guest gRPC interface.
5. **Python sync methods bridge via `LoopRunner`**: Synchronous calls on `SandboxHandle` or
   `SandboxFleet` submit coroutines to `LoopRunner.run()` rather than invoking `asyncio.run()`.

## Step-by-step implementation checklist

### 1. Update service definitions (`proto/ateenv/v1alpha/`)

- In-environment operations belong in `proto/ateenv/v1alpha/guest.proto` (`ProcessService` or
  `FileSystemService`).
- Environment lifecycle operations belong in `proto/ateenv/v1alpha/env.proto`
  (`EnvironmentService`).
- Regenerate Go stubs with `buf generate`.
- Regenerate Python stubs with `make python-protos`.
- Never edit generated `*.pb.go` or `*_pb2*.py*` files by hand.

### 2. Implement the in-guest handler (`guest/`)

- Implement the service method in `guest/process/service.go` or `guest/filesystem/service.go`.
- If executing processes: ensure `Setpgid: true` (or `Setsid: true` for PTYs) is configured and
  signals target `-pid`. If streaming spools, subscribe to `ProcessState.OutputChanged()` before
  reading disk offsets.
- If transferring files: `guest/filesystem.Service` confines paths to the configured `-workspace`
  flag root (the default `"/"` in `cmd/ate-env-guest` disables confinement). File reads stream in
  chunks up to `filesystem.DefaultReadBufferSize` (64 KiB).

### 3. Add the proxy handler in `ate-env-api` (`internal/apiservice/`)

- Add the corresponding handler in `internal/apiservice/server.go`, extracting `envID, atespace` via
  `envFromContext(ctx)` (which also stamps `ate.atespace` and `ate.actor.name` on the server span;
  on lifecycle or template RPCs, call the `setSpan*Attributes` helpers in `span_identity.go`).
- For unary and server-streaming RPCs, dial via
  `conn, outCtx, closeFn, fastPath, err := s.guestSession(ctx, atespace, envID, false)` and call
  `s.evictOnGuestUnavailable(ctx, targetActor, err)` on the forwarded RPC error. When `fastPath` is
  `true` and `evictOnGuestUnavailable` returns `true` on an idempotent RPC (or before the first
  chunk of a server stream), redial once with `s.guestSession(ctx, atespace, envID, true)` (for a
  mutating unary RPC with request-ID deduplication like `StartProcess`, gate replay on
  `s.hasGuestCap`; non-idempotent RPCs like `SignalProcess` evict without replaying).
- For client-streaming RPCs (`WriteProcessInput`, `WriteFile`), call `envFromContext(ctx)` first,
  read the first message via `nextMessage(stream)` before waking the guest, dial with
  `s.guestSession(ctx, atespace, envID, true)` (consumed streams cannot replay), and call
  `s.evictOnGuestUnavailable` on stream errors.
- Always invoke `closeFn()` (swapping `closeFn` when a server-streaming retry redials) and forward
  over the generated client using `outCtx`.

### 4. Expose in MCP (`internal/mcp/`)

- If the operation should be available to agent tools, define the tool schema in
  `internal/mcp/tools.go` using `tool.New`.
- Delegate tool execution to the client method on `ProcessServiceClient` or
  `FileSystemServiceClient`.

### 5. Expose in Go and Python SDKs

- **Go SDK** (`clients/go/env.go`): add helper methods on `*Env` or `*Process`.
- **Python low-level SDK** (`clients/python/src/ate_env_rl/env.py`): add async methods on `Env` or
  `Process`.
- **Parity**: add the new RPC to `proto/testdata/client-parity/rpc_coverage.json`.
- **Python high-level fleet** (`clients/python/src/ate_env_rl/handle.py`): add synchronous methods
  on `SandboxHandle` (delegating through `self.runtime` or `self.backend`, which dispatch coroutines
  via `LoopRunner.run(...)`) and asynchronous methods `..._async`.

### 6. Verify and test

- Write Go unit tests using `internal/internaltest/fakecontrol` and
  `internal/internaltest/fakerouter`.
- Update telemetry vectors in `proto/testdata/client-parity/` (e.g., `rpc_attrs.json`,
  `error_mapping.json`) if needed.
- Write Python unit tests using `src/ate_env_rl/backend/mock.py` (`MockBackendDriver`),
  `src/ate_env_rl/runtime/mock.py` (`MockRuntimeHook`), and `tests/fakes.py` (`Fake*Service` gRPC
  fakes).
- Run `make verify-boilerplate`, `make verify-gofmt`, `make vet`, `make test`, and
  `make python-test`.
