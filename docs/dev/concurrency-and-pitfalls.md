# System invariants and concurrency pitfalls

Companion to [architecture](../architecture.md) and [code layout](./code-layout.md).

## In-guest process lifecycle and signaling

### 1. PID 1 child subreaper re-exec avoids `cmd.Wait` race

- **`cmd/ate-env-guest/init_linux.go` (`runIfPID1`, `supervise`)**: Direct `wait4(-1, ...)` in the
  daemon races `os/exec.Cmd.Wait()` in `guest/process.Tracker`; `ate-env-guest` sets
  `PR_SET_CHILD_SUBREAPER`, re-execs via `os.StartProcess`, and the outer PID 1 runs
  `syscall.Wait4(-1, &status, syscall.WNOHANG, nil)` and forwards signals.

### 2. Process groups and negative-PID signal delivery

- **`guest/process/tracker.go` (`Setpgid`, `Tracker.Signal`, `Tracker.Start`),
  `guest/process/pty_linux.go` (`Setsid`), `cmd/ate-env-guest/sidecar.go`**: Signaling only the
  parent PID leaves child trees running; non-PTYs set `Setpgid: true` and PTYs `Setsid: true`, and
  `(*Service).SignalProcess` and timeouts call `Tracker.Signal` (`syscall.Kill(-pid, sig)`). A
  process can leave its group (`setpgid`), so when the group is empty `Tracker.Signal` signals the
  process itself through `os.Process`. That signals through a pidfd, or, without one, `Process.Wait`
  marks the process done before `wait4` reaps it, so it never reaches a reused PID. A process that
  left its group while descendants stayed in it is not reached, because `kill(-pid)` succeeds on the
  descendants.

### 3. Spool cursor subscription and concurrency-slot release before exit

- **`guest/process/{service,tracker}.go` (`StreamProcessOutput`, `reap`, `decrementActive`,
  `waitReaped`)**: Subscribing after reading misses concurrent disk writes, and marking
  `PROCESS_STATE_EXITED` before releasing the slot fails chained `StartProcess` calls at
  `MaxConcurrentProcesses` (`ErrTooManyProcesses`). `StreamProcessOutput` acquires `OutputChanged()`
  before reading (writers close it under mutex after flushing), and `Tracker.reap` syncs spools and
  calls `decrementActive()` before setting `PROCESS_STATE_EXITED`, closing `done`, and notifying
  `output`. A syscall error (`ESRCH`, `EPIPE`, `os.ErrClosed`) does not prove an exit: it can arrive
  before the slot release, or from a process that still runs. `Tracker.Signal` and
  `Tracker.WriteInput` then wait for `reap` (`waitReaped`) and return `ErrExited` only once `reap`
  has released the slot (`#25`). If `reap` has not finished within `reapWaitTimeout` (a slow spool
  sync, or a descendant outside the group that still holds the output pipes, so `cmd.Wait` does not
  return), `WriteInput` returns `ErrStdinClosed` and `Signal` returns `nil` (nothing is left to
  signal), as `GetProcess` still reports the process as running. `WriteInput` releases `stdinMu`
  before it waits, because `reap` takes it before it closes `done`. `ResizeProcess` returns
  `Internal` in that window instead (`#37`).

## Server networking and Substrate boundaries

### 4. Router `ate-target-actor` header deduplication, SPIFFE mTLS, and `guestSession`

- **`internal/apiservice/{server,router_interceptor,wakeretry}.go` (`guestSession`, `Close`,
  `routerUnaryInterceptor`, `routerStreamInterceptor`, `routerCredentials`, `ensureAwake`),
  `internal/ate/client.go` (`DialGuest`), `internal/routertls/routertls.go`**: Forwarding caller
  metadata duplicates `ate-target-actor` (`atenet-router` rejects it). `guestSession` and
  `DialGuest` append it alone; `guestSession` returns `noopClose` on error or `-router-pool-size`
  (`Close` returns `Unavailable`, `StreamProcessOutput`/`ReadFile` swap `closeFn` on retry).
  `internal/routertls` reloads `-atenet-client-cert` and `servicedns` `-atenet-ca-file` on
  handshakes (`routertls.TransportCredentials` `alertConn.Write` reads TLS 1.3 alerts on HTTP/2
  `EPIPE` for `routertls.RejectedCertificate` in `ensureAwake` and `DialGuest`).

### 5. Actor wake budgets, fail-fast start errors, `boundedAwakeCache`, and deletion

- **Wake budgets (`internal/apiservice/{wakeretry,awake_cache}.go`, `ensureAwake`)**: `atenet`
  resumes suspended actors as `ensureAwake` polls `GetProcess("ate-env-wake-probe")` under
  `DefaultWakeBudget` (5m) and `DefaultWakeRunningBudget` (20s once `RUNNING`), returning `Canceled`
  or `DeadlineExceeded` probe errors immediately (`-reactive-wake` pairs `AddIfGen`/`Generation`,
  clears `caps` and bumps generation on `Remove`, and keeps `expiresAt` on `RecordCap`). Post-probe
  state reads get remaining `WakeBudget` (and `WakeNoCapacityGrace` in a no-room run), capped at
  `WakeStateReadCap` (`-wake-state-read-cap`, `DefaultWakeStateReadCap` = 5s; control dial has no
  timeout). Read `Unavailable`, `DeadlineExceeded`, or `ResourceExhausted` (or deadline cut) leaves
  state unknown and waits on; if all reads cut, it probes per `-wake-state-read-cap` plus backoff
  and ends at `-wake-budget` with `Unavailable` "while UNKNOWN".
- **Fail-fast (`StartFailureKey`)**: `routertls.RejectedCertificate` returns
  `codes.FailedPrecondition` (`routertls.RejectionAdvice`, no trailer), `CRASHED` returns
  `codes.FailedPrecondition` (`ActorStatus.Crash.Message`, `ate-env-start-failure: crashed`),
  `DELETING` returns `codes.NotFound`, and `"no worker has room for the actor"` over
  `WakeNoCapacityGrace` while not `RUNNING` (default `0` waits `WakeBudget`) returns
  `codes.ResourceExhausted` (`ate-env-start-failure: no-capacity`).
- **Delete (`internal/ate/client.go`: `Delete`, `evictGuest`)**: `internal/ate.Client.Delete` calls
  `DeleteActor` directly in any state including `CRASHED` (stopping workloads/slots/snapshots
  without pre-suspending) and `evictGuest` to drop `guestPool`.

### 6. Idempotent derived `ActorTemplate` creation and failed golden-snapshot recovery

- **`internal/apiservice/{imagetemplate.go,templaterecover.go,server.go}` (`DeriveImageTemplate`,
  `keptUID`)**: Concurrent workers race on `<base>-<digest:12>` (`imageTemplateDigestLen = 12`),
  Substrate leaves failed `golden_snapshot_status.error_message` unretried, and deleting an in-use
  template strips rootfs volumes on resume. `CreateEnvironment` takes `imageTemplateLock` only when
  `rootfs_image` is set (direct `Template.Name` creates skip it), and `ensureImageTemplate` holds a
  process-local per-template read lock (`imageTemplateLocks`) through `CreateActor`
  (`docs/plans/ate-env-api/acceptance-criteria.md`). `getOrCreateImageTemplate` reuses on cross-path
  `sameImageDigest`, fails with `codes.FailedPrecondition` on mismatch, and treats
  `codes.AlreadyExists` as success after the winner's digest check. If a golden failed
  `>= failedTemplateRecreateAfter` (10m) ago, `ensureImageTemplate` write-locks and calls
  `recreateFailedImageTemplate` (by UID at 0 `templateUsers`).

## Client connection pooling and event loops

### 7. Python `LoopRunner` thread bridging and HTTP/2 stream concurrency

- **`clients/python/src/ate_env_rl/{loop_runner.py,fleet.py,backend/substrate.py}`,
  `cmd/ate-env-api/main.go`**: `asyncio.run()` destroys HTTP/2 gRPC channels on exit; `LoopRunner`
  runs a daemon loop dispatching coroutines via `asyncio.run_coroutine_threadsafe`, `SandboxFleet`
  sizes connections via `FleetConfig.connections` or `ceil(max_concurrent / 200)`, and
  `SubstrateBackendDriver` pools gRPC channels against `DefaultMaxConcurrentStreams = 4096`.

### 8. Python release error tracking, once-per-handle release, and exit hooks

- **`clients/python/src/ate_env_rl/{backend/substrate,fleet,async_fleet,handle,strategies,types}.py`**:
  `SandboxHandle.release()` and `recycle()` run once per handle (`_begin_release`/`_end_release`,
  retryable on failure/cancel), `release_all()` retries `_active_handles`, `SubstrateBackendDriver`
  logs failed deletes in `RunReport.release_errors` and keeps `_owned_actors`, `fleet.teardown()`
  retries (`InfrastructureError` on failure, without replacing a body error or interrupt in
  `SandboxFleet.run()`/`with fleet:`), `teardown(verify=True)` calls `verify_teardown` for its run
  (the driver keeps reap targets of the last `REAP_TARGET_RUNS` runs) returning `TeardownReport` or
  attaching `teardown_report`/`teardown_report_error`, and `FleetConfig(exit_hooks=True)` registers
  `_ExitHooks` (`atexit` + `SIGINT`/`SIGTERM` on `SandboxFleet`, rejected on `AsyncSandboxFleet`).

### 9. Python warm-pool bookkeeping across awaits

- **`clients/python/src/ate_env_rl/{backend/substrate,loop_runner}.py`,
  `clients/python/tests/{test_warm_pool_accounting,test_loop_runner}.py`**: Pooling recycled actors
  after a concurrent `reap` leaks past teardown, and unreserved shortfalls over-provision.
- **Teardown & claim checks**: `reap` (`_reap_internal`) bumps `_teardowns` on entry/exit
  (`_reaping` counts active reaps); recycles (`_release_internal`), `warm_pool`
  (`_warm_pool_internal`), and claims (`_acquire_internal`) read `_teardowns` before awaiting and
  pool only via `_may_pool` (`_teardowns` unchanged, `_reaping == 0`), else delete; claims re-check
  after `get()` in one lock or delete stale still-owned actors and retry.
- **Ownership & generations**: Actors stay in `_owned_actors` till deleted or reaped (`_reap_one`
  also pops failed deletes, which `reap` raises and `_reap_targets` retains). Cold `acquire`
  overlapping its run's reap and cancelled creates (`_own_abandoned_create`, e.g. `timeout_s`) tag
  `WARM_POOL_RUN_ID`. `_reap_internal` clears `_warm_inflight` (per key, `_teardowns`, `_pool_gens`;
  minus shortfalls) on entry/exit. `unwarm_pool` and `resize_pool` below pooled + in-flight bump
  `_pool_gens` (`_cancel_warm_calls`), so earlier `warm_pool` (`drained`) deletes rather than pools.
  `_delete_unpooled` re-tags failed/cancelled deletes `WARM_POOL_RUN_ID` via `_keep_for_any_reap`
  (vs `release` via `_return_popped`/`_may_pool`); `reap` updates warm-replica gauges.
- **Entry snapshot & background deletes**: `_acquire_internal` snapshots `entry_teardowns` and
  checks `_run_reaped_since` when owning or in `_own_abandoned_create`, so a warm claim paused in
  `get()` across its run's reap tags its cold-fallback/retried actor under `WARM_POOL_RUN_ID` (and
  deletes on cancel). Post-reap cancelled cold creates/`warm_pool` calls delete via
  `_spawn_background_delete` (`_background_deletes`); `_reap_actors`, `close()`, and `close_async()`
  join same-loop deletes of still-owned actors via `_wait_background_deletes` (bounded by finite
  `background_delete_timeout_s >= 0`, default 10s, `0` = no wait). A delete still running at the
  deadline (`late`) is not cancelled and stays under `WARM_POOL_RUN_ID`, while joined
  failed/cancelled deletes record in `_failed_background_deletes` via `_background_delete` (done
  callbacks record pre-start cancels; `NotFound` counts as gone); `reap` adds both to
  `InfrastructureError` and `close()` logs them. `close()` and `close_async()` call
  `_count_teardown` (bumping `_teardowns`, setting `_closing`, and cancelling
  `preflight_images_async` tasks in `_preflights` via `_cancel_preflights` so a pass waking after
  close never dials an unclosed client or makes `PreflightWarmer` return short coverage;
  `_preflight_client` rejects unregistered tasks with `CancelledError`). `close()` cancels/awaits
  `LoopRunner` tasks (`_quiesce`) before channel close and `LoopRunner.stop` drains cancel-handler
  tasks in up to `_STOP_ROUNDS` rounds; `close_async()` (`AsyncSandboxFleet.__aexit__`) never
  cancels caller tasks while `_shutdown_async` repeats join-and-close for up to `_STOP_ROUNDS`
  rounds. `_return_popped` background-deletes after any teardown since pop (keeping
  `WARM_POOL_RUN_ID` on failure or restoring run id if none); under `_closing`, cancelled
  `_delete_unpooled` and `_delete_unreaped_in_close` (`_reap_one`, `_delete_one`, `gather` in
  `_reap_actors`, `_delete_drained`, `_warm_pool_internal`) background-delete unreaped actors
  without duplicates (a reap cancelled outside close leaves actors owned, and a cancelled background
  delete starts no second one). `unwarm_pool_async` and shrinking `resize_pool_async` re-tag popped
  actors `WARM_POOL_RUN_ID` in one lock (`_own_drained_for_any_reap`); `_stop_if_closing` raises
  `CancelledError` at the 16-slot semaphore in `_reap_one`, `_warm_one` (also between `create()` and
  `suspend()`), and `_delete_one`; a warm claim cancelled in `get()` re-pools or (post-teardown)
  re-tags `WARM_POOL_RUN_ID` and background-deletes; and `warm_pool` re-raises channel-close
  `CancelledError` from `gather(return_exceptions=True)` so started actors delete.
