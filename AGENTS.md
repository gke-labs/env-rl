# sandboxenv-rl

<!-- Last full audit: 2026-11-16 (commit 637a24fb) -->

Isolated execution environments for reinforcement learning and evaluation on Agent Substrate. Each
environment runs as a gVisor actor supporting on-demand rootfs derivation, PTYs, file streaming, MCP
tools, and Python fleet scheduling. See [README.md](./README.md) and
[docs/architecture.md](./docs/architecture.md).

## Rules for contributors

- Do not add AI co-author or assisted-by trailers to commit messages.
- Review your diff before requesting review. For substantial changes, have a fresh agent review the
  diff adversarially, then confirm and address what it identifies.
- One change per pull request (separate refactors and follow-ups). Search open PRs and issues first.
- Be brief: short active sentences, subject first. Avoid em-dashes, semicolon chains and "X, not Y".

## Workflow

- Before changing behavior, read the owning plan's `docs/plans/<name>/acceptance-criteria.md`. If
  none owns it, land the criteria first in their own PR with the four-column table (`ID`,
  `Criterion`, `Status`, `Tests`, as in `ate-env-api`, `nemo-gym`, `port-proxy`) and a status line
  naming the commit it describes.
- Update criteria rows before code (one observable input/output behavior, stable plan-prefixed ID,
  and test names). Tests must name the ID in their name or doc comment.
- List criteria under `## Criteria` (`AE-4 met`, `NG-12 partial`, `PP-2 added`, or `None: <reason>`
  for chores, dependency bumps, docs, or tests) and mark met rows with the PR number in the same
  pull request. A behavioral PR under a plan's scope without a criterion is not ready for review.

## Code

- Search the repo and dependencies before writing a helper, cache, client, flag set, or fixture.
  Reuse or extend what exists and match nearby patterns.
- Build only what the change needs: no speculative options, interfaces, wrappers, flags, exports, or
  API fields. Prefer sensible defaults and delete newly unused code.
- Pass dependencies through constructors. Do not add package globals or test setters.
- Fail loudly: ignore only expected errors (e.g., `os.ErrNotExist`) and never fall back silently to
  an insecure or no-op mode.
- Check every claim in comments, docs, and PR descriptions against the code, and update docs with
  behavior changes. Find root causes before fixing symptoms and back performance claims with data.
- Planned: Do not assume kind or GKE, or a specific CPU architecture (dev scripts must support both
  clusters).
- Put shell logic in standalone `.sh` files under `hack/` or `benchmarking/`, not inline in workflow
  YAML.

## Architecture invariants

- `internal/apiservice` owns host adapter translation and proxy routing; `guest/process` and
  `guest/filesystem` own in-environment execution; callers forward through `s.guestConn` with
  `ensureAwake` and must not buffer full files or track process state in `ate-env-api`; because
  `ate-env-api` is an unbuffered horizontal adapter (not enforced).
- `apiservice.(*Server).guestConn` owns per-call guest gRPC dialing with wake verification; new
  proxy handlers call it and must not dial `atenet` directly; because `ensureAwake` handles actor
  restoration from storage (not enforced). `internal/ate.Client.DialGuest` dials `atenet` for
  `internal/mcp` without `ensureAwake` (known gap; not enforced). `internal/portproxy` owns raw
  `CONNECT` dialing, managing its own tunnel wake loop and holding `X-Env-Wake-Wait` requests via
  `apiservice.(*Server).Wake` (not enforced).
- `proto/ateenv/v1alpha` owns the contract between `ate-env-api` and `ate-env-guest`; `internal/`
  and `clients/` call generated stubs and must not import `guest/` in production (`guest/` and
  `cmd/ate-env-guest` must not import `internal/ate` or `go.ate.dev/substrate`); because
  `ate-env-guest` runs inside task images without control-plane dependencies (enforced by
  `hack/verify/layers.py` in the Go CI job).
- `clients/go` is the public Go SDK; outside tests it imports only `proto/ateenv/v1alpha` from this
  repository, and nothing under `internal/`, `go.ate.dev/` or `k8s.io/`; because an external module
  that imports the client must not pull the Substrate control-plane module (enforced by
  `hack/verify/go-client-deps.sh` in the Go CI job). Its tests keep the fixture's imports.
- `internal/apiservice` owns on-demand `ActorTemplate` derivation; `apiservice.Server` calls
  `DeriveImageTemplate` and `ImageTemplateName` and callers must not build derived templates outside
  them (base templates via `ate-env manifest template`, `buildActorTemplate` in
  `cmd/ate-env/manifest.go`); because derived names (`<base>-<digest:12>`) must be deterministic and
  idempotent across races (not enforced).
- `internal/mcp` and `internal/portproxy` own external protocol translation; `internal/mcp` dials
  `ate.Client.DialGuest` (outside `apiservice`) and `internal/portproxy` tunnels allowed atespaces;
  callers must not bypass them; because clients need translation and origin checks (not enforced).
- `internal/hostcheck` owns Host header validation, and `internal/apiservice.BearerAuth` owns token
  auth; HTTP handlers wrap with `hostcheck.(*Checker).Wrap`/`WrapFor` (and `BearerAuth` when
  `-auth-token-file` is set) and must not re-implement checks; because browsers can DNS-rebind local
  daemons (not enforced; `GET /healthz` is exempt from auth).
- `cmd/ate-env-guest` (`init_linux.go`, `sidecar.go`) owns PID 1 reaping and sidecars, and
  `guest/process.Tracker` owns subprocess lifecycle; callers invoke `Tracker` methods and must not
  manage children directly; because PID 1's `wait4(-1, WNOHANG)` loop reaps orphans, and non-PTY
  process groups (`Setpgid: true`, PTY `Setsid: true`) need `-pid` signals (enforced by `runIfPID1`,
  `supervise`, and `guest/process.Tracker`).
- `clients/python/src/ate_env_rl`'s `LoopRunner` owns sync-to-async bridging; synchronous callers
  (`SubstrateBackendDriver`, `SubstrateEnvClientRuntime`, `SandboxFleet`, `SandboxHandle`) dispatch
  through `LoopRunner.run()` and must not call `asyncio.run()`; because `asyncio.run()` destroys the
  loop and corrupts cached HTTP/2 gRPC channels (not enforced).

Docs: [docs/architecture.md](./docs/architecture.md), [docs/glossary.md](./docs/glossary.md),
[docs/dev/concurrency-and-pitfalls.md](./docs/dev/concurrency-and-pitfalls.md),
[docs/dev/code-layout.md](./docs/dev/code-layout.md).

## Repository layout

Skills in `.agents/skills/`: [create-release](./.agents/skills/create-release/SKILL.md) (releases,
PyPI/GHCR), [extend-env-surface](./.agents/skills/extend-env-surface/SKILL.md) (add RPC/tool),
[maintain-docs](./.agents/skills/maintain-docs/SKILL.md) (sync/validate docs). Python SDK: dir
`clients/python`, package `ate-env-rl`, module `ate_env_rl`, handles `Env` and `SandboxHandle`.
Third-party integrations live in `integrations/<framework>/` (own uv workspace distribution +
`integrations/README.md` row) and runnable examples in `examples/<name>/` (`README.md` +
`examples/README.md` row). Framework modules are imported only there, and nothing in `clients/`,
`benchmarking/`, `internal/`, or `hack/` imports an integration or example (enforced with top-level
dirs by `hack/verify/layers.py`, per [docs/dev/code-layout.md](./docs/dev/code-layout.md)).

## Build, test, verify

Run before opening a pull request:

```bash
make verify-boilerplate verify-gofmt verify-go-client-deps verify-docs verify-protos verify-layers vet test
make python-test nemo-gym-test harbor-test e2b-test
make ci-dafny   # when touching docs/plans/telemetry/verify or modelled code
python -m pytest integrations/openhands/tests benchmarking/swebench benchmarking/telemetry_report internal/benchmarking/swebench/trajectories examples/Qwen3-1.7B-verl/tests hack/release/test_create_release.py hack/ci/test_e2e_policy.py
ruff check clients/python examples/python-quickstart examples/verl_swebench benchmarking/telemetry_report hack/release hack/ci hack/verify
ruff check --config clients/python/pyproject.toml examples/Qwen3-1.7B-verl
```

Targets: `make build` (Go), `make build-cli` (`bin/ate-env`), `make images` (`ko`),
`make format-docs` (Markdown), `make clean` (`bin/`). Regenerate protos via `buf generate` (Go) and
`make python-protos` (Python), and never hand-edit generated files. Use `internal/internaltest`
(`fakecontrol`, `fakerouter`, `synctestrapid`, `testcerts`) for test doubles and `rapid`/`synctest`
bridges.

## Pull requests and commits

- Fill out `.github/pull_request_template.md`, including `Security impact` (update
  `docs/security/threat-model.md` if any box is checked).
- Lead with why the change is needed. Write `Fixes #N` only when fully resolving the issue.
- Squash-merge with a `(#N)` subject suffix and commit list in the body, with no AI trailers.
