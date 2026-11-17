# Plan: client-only fleet gaps (#123)

Status: proposal, 2026-11-14, checked against `main` at `456353fd`. Row 10 (FL-1 to FL-4) is built
by #397, and rows 32 and 31 (FL-5 to FL-9) by #395; the rest is open. The criteria are in
[acceptance-criteria.md](./acceptance-criteria.md), group `FL`.

## 1. Scope

The Python fleet (`clients/python/src/ate_env_rl`: `fleet.py`, `async_fleet.py`, `handle.py`,
`backend/substrate.py`) gaps from #123 that need no server change. The Go client has no fleet (the
Go client parity plan defers it), so no row here has a Go counterpart.

| #123 row | Gap                                              | Criteria       |
| -------- | ------------------------------------------------ | -------------- |
| 10       | Preflight checks with a report                   | FL-1 to FL-4   |
| 32       | Teardown at interpreter exit or signal           | FL-5 to FL-7   |
| 31       | `recycle` and `release_all`, both kinds of fleet | FL-8, FL-9     |
| 29       | Recycle with a verified reset                    | FL-10 to FL-13 |
| 17       | `wait_pools_ready`                               | FL-14, FL-15   |
| 15       | Staged warm-up                                   | FL-16, FL-17   |
| 35       | `teardown(verify=True)`                          | FL-18, FL-19   |

This plan takes the client-only rows that #123 lists before GA (#10, #15, #17), the rows its
suggested order names next (#32, #31, #29), and #35, which `ListEnvironments` (#392) unblocks. The
other client-only rows of #123 (#4, #6, #7, #9, #12, #18, #19, #24, #27, #28, #37, #38, #42) are
neither on that list nor next in that order, and stay open in #123. Two parts of the listed rows
have no criterion here: for #10, snapshot storage needs snapshot RPCs that `ate-env-api` does not
have yet, and the name-length check stays open in #123; for #17, `warm_image(wait=False)` already
exists on `SandboxFleet` and `AsyncSandboxFleet`, so only `wait_pools_ready` is left.

Out of scope: the server rows of #123 (#22, #23, #26), and labels and a time to live on an
environment (#391).

## 2. What the code does today

- Preflight: `SandboxFleet.preflight` and `AsyncSandboxFleet.preflight` call the backend's
  `preflight`. `SubstrateBackendDriver.preflight_async` only reads an environment that does not
  exist and treats `NotFoundError` as success; anything else raises `PreflightError`. Nothing is
  reported per check.
- Exit: `SandboxFleet.__exit__` and `close` tear down; no `atexit` or signal handler exists, so a
  trainer that is killed leaves its environments.
- `AsyncSandboxFleet` has `release` only. `SandboxFleet` has `release` and `recycle` (each takes one
  handle or a sequence). Neither has `release_all`.
- Recycle: `SandboxHandle.recycle` and `recycle_async` call the backend's `release(recycle=True)`,
  which suspends the actor and puts it back in the pool. Their doc says that nothing is reset: the
  next claim gets the state that the previous workload left.
- Warm-up: `SandboxFleet._warm_many` warms up to `FleetConfig.warm_parallelism` images at once; each
  image's `warm_pool` creates its replicas. No limit counts creates across images, and no call waits
  for the pools without warming them.
- Teardown: `SandboxFleet.teardown` calls the backend's `reap(run_id)`, which deletes the actors
  that this process tracks (`SubstrateBackendDriver._owned_actors`) and the warm-pool actors. It
  returns nothing and checks nothing afterwards.

## 3. Decisions

- **The report is a new method.** `preflight_report()` returns a `PreflightReport` (one
  `PreflightCheck` per check: name, `ok` / `failed` / `skipped`, detail) and raises only with
  `strict=True`. `preflight()` keeps its contract (the connectivity check, raising), because
  `setup()`, `run()` and callers outside this package (OpenHands strategies call `preflight()` and
  its tests replace the method) rely on it, and a run start should not pay for the image and
  placement calls. A server that answers `Unimplemented` for an RPC marks that check `skipped`, so
  the report works against an older `ate-env-api`.
- **Exit hooks are opt-in** (`FleetConfig.exit_hooks`, default off). A library does not take over
  the signals of the process that imports it. When on, the fleet tears down once, at `atexit`,
  `SIGINT` or `SIGTERM`, and then calls the handler that was installed before it. It installs signal
  handlers only from the main thread. `AsyncSandboxFleet` refuses `exit_hooks=True`: its driver's
  clients belong to the caller's event loop, which a signal handler or `atexit` cannot use. #398
  tracks exit hooks for it.
- **No default reset.** The fleet does not know the task's working directory or what "clean" means
  for a task. `FleetConfig.recycle_reset` (a command) and `FleetConfig.recycle_verify` (a command
  whose exit code 0 means clean) are unset by default, and then `recycle` behaves as today. When
  set, `recycle` runs them before it returns the sandbox to the pool, and deletes the sandbox
  instead when either fails. `FleetConfig.recycle_timeout_s` bounds each of the two commands; a
  command that runs longer counts as failed. `FleetConfig.max_reuses` (default unset) deletes a
  sandbox at its `max_reuses`-th recycle.
- **`wait_pools_ready` never raises on a timeout.** It is a barrier for a training loop that wants
  to start once the pools are warm, and it returns the counts so the caller decides.
- **`warm_create_budget` counts creates, not images.** `warm_parallelism` stays the number of images
  warmed at once; the budget caps the warm creates in flight across all of them. Unset keeps today's
  behavior.
- **`verify_teardown` checks only the ids this fleet created.** Without labels, an atespace that
  other callers share holds environments the fleet cannot attribute. With labels (#391), a later
  pull request can widen it to the run's label.

## 4. Pull requests

| Pull request | Content                                                 | After               |
| ------------ | ------------------------------------------------------- | ------------------- |
| this         | This plan and the `FL` criteria                         |                     |
| 1            | Preflight report (FL-1 to FL-4)                         | this                |
| 2            | Exit hooks and async parity (FL-5 to FL-9)              | this                |
| 3            | `teardown(verify=True)` (FL-18, FL-19)                  | this; #392 optional |
| 4            | Recycle with a verified reset (FL-10 to FL-13)          | 2                   |
| 5            | `wait_pools_ready` and the warm budget (FL-14 to FL-17) | this                |

FL-18 does not need #392: without `ListEnvironments` the verify reads each id with `GetEnvironment`.
#392 is a draft, and #396 carries its commits until it merges.
