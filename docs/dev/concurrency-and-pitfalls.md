# System invariants and concurrency pitfalls

Companion to [architecture](../architecture.md) and [code layout](./code-layout.md).

## In-guest process lifecycle and signaling

### 1. PID 1 child subreaper re-exec avoids `cmd.Wait` race

- **`cmd/ate-env-guest/init_linux.go` (`runIfPID1`, `supervise`)**: `wait4(-1, ...)` races
  `os/exec.Cmd.Wait()` in `guest/process.Tracker`. `ate-env-guest` sets `PR_SET_CHILD_SUBREAPER` and
  re-execs (`os.StartProcess`) so PID 1 reaps with `syscall.Wait4` (`WNOHANG`) and relays signals.

### 2. Process groups and negative-PID signal delivery

- **`guest/process/tracker.go` (`Setpgid`, `Tracker.Signal`, `Tracker.Start`),
  `guest/process/pty_linux.go` (`Setsid`), `cmd/ate-env-guest/sidecar.go`**: Signaling only the
  parent PID strands its children. Non-PTYs set `Setpgid: true`, PTYs `Setsid: true`, and
  `(*Service).SignalProcess` and timeouts call `Tracker.Signal` (`syscall.Kill(-pid, sig)`). If
  `setpgid` emptied the group, `Tracker.Signal` signals the process through `os.Process`, which
  never hits a reused PID. If descendants stay in the old group, `kill(-pid)` reaches only them.

### 3. Spool cursor subscription and concurrency-slot release before exit

- **`guest/process/{service,tracker}.go` (`StreamProcessOutput`, `reap`, `decrementActive`,
  `waitReaped`)**: Subscribing after reading misses writes, and `PROCESS_STATE_EXITED` before the
  slot release fails chained `StartProcess` at `MaxConcurrentProcesses` (`ErrTooManyProcesses`).
  `StreamProcessOutput` takes `OutputChanged()` first (writers close it under mutex after flushing),
  and `Tracker.reap` syncs spools, then calls `decrementActive()` before `PROCESS_STATE_EXITED`,
  `done`, and `output`. `ESRCH`, `EPIPE`, and `os.ErrClosed` do not prove an exit: they can precede
  the slot release or come from a live process. So `Tracker.Signal` and `Tracker.WriteInput` (which
  drops `stdinMu` first, as `reap` takes it before `done`) return `ErrExited` only after
  `waitReaped` sees the release (`#25`). Past `reapWaitTimeout` (a slow sync, or a descendant
  outside the group holding the pipes), `WriteInput` returns `ErrStdinClosed` and `Signal` `nil`, as
  `GetProcess` still reports `RUNNING` (`ResizeProcess`: `Internal`, `#37`).

## Server networking and Substrate boundaries

### 4. Router `ate-target-actor` header deduplication, SPIFFE mTLS, and `guestSession`

- **`internal/apiservice/{server,router_interceptor,wakeretry}.go` (`guestSession`, `Close`,
  `routerUnaryInterceptor`, `routerStreamInterceptor`, `routerCredentials`, `ensureAwake`),
  `internal/ate/client.go` (`DialGuest`), `internal/routertls/routertls.go`**: Forwarding caller
  metadata duplicates `ate-target-actor` (`atenet-router` rejects it). `guestSession`/`DialGuest`
  append it alone, and `guestSession` returns `noopClose` on error or `-router-pool-size` (`Close`
  returns `Unavailable`, `StreamProcessOutput`/`ReadFile` swap `closeFn` on retry).
  `internal/routertls` reloads `-atenet-client-cert` and `servicedns` `-atenet-ca-file` per
  handshake (`routertls.TransportCredentials` `alertConn.Write` reads TLS 1.3 alerts on HTTP/2
  `EPIPE` for `routertls.RejectedCertificate` in `ensureAwake`/`DialGuest`).

### 5. Actor wake budgets, fail-fast start errors, `boundedAwakeCache`, `StartProcess` dedup, and deletion

- **Wake & replay (`internal/apiservice/{wakeretry,awake_cache,server}.go`, `ensureAwake`,
  `guest/process/idempotency.go`)**: `atenet` resumes actors as `ensureAwake` polls
  `GetProcess("ate-env-wake-probe")` under `DefaultWakeBudget` (5m) and `DefaultWakeRunningBudget`
  (20s once `RUNNING`), returning `Canceled`/`DeadlineExceeded` immediately. The guest dedups
  `StartProcessRequest.request_id` in `idempotencyTable` (`x-ate-env-guest-caps: start-request-id`);
  `-reactive-wake` pairs `AddIfGen`/`Generation`, clears `caps`/bumps gen on `Remove`, keeps
  `expiresAt` on `RecordCap`, and sets `request_id` to replay `StartProcess` once only after
  `hasGuestCap` sees it (else clears `request_id`, no replay). Post-probe reads get remaining
  `WakeBudget` (`+ WakeNoCapacityGrace` on no-room), capped at `WakeStateReadCap`
  (`-wake-state-read-cap`, `DefaultWakeStateReadCap` = 5s; no dial timeout). Read `Unavailable`,
  `DeadlineExceeded`, or `ResourceExhausted` (or cut) leaves state unknown; if all cut, it probes
  per `-wake-state-read-cap` + backoff and fails at `-wake-budget` (`Unavailable` "while UNKNOWN").
- **Fail-fast (`StartFailureKey`)**: `routertls.RejectedCertificate` returns
  `codes.FailedPrecondition` (`routertls.RejectionAdvice`, no trailer), `CRASHED` returns
  `codes.FailedPrecondition` (`ActorStatus.Crash.Message`, `ate-env-start-failure: crashed`),
  `DELETING` returns `codes.NotFound`, and `"no worker has room for the actor"` over
  `WakeNoCapacityGrace` while non-`RUNNING` (default `0` waits `WakeBudget`) returns
  `codes.ResourceExhausted` (`ate-env-start-failure: no-capacity`).
- **Delete (`internal/ate/client.go`: `Delete`, `evictGuest`)**: `internal/ate.Client.Delete` calls
  `DeleteActor` in any state (even `CRASHED`, no pre-suspend) and `evictGuest` (`guestPool`).

### 6. Idempotent derived `ActorTemplate` creation and failed golden-snapshot recovery

- **`internal/apiservice/{imagetemplate.go,templaterecover.go,server.go}` (`DeriveImageTemplate`,
  `keptUID`)**: Races hit `<base>-<digest:12>` (`imageTemplateDigestLen = 12`), Substrate leaves
  failed `golden_snapshot_status.error_message` unretried, and deleting live templates strips rootfs
  on resume. `CreateEnvironment` read-locks `imageTemplateLock` (`ensureImageTemplate`,
  `imageTemplateLocks`, local) over `CreateActor` when `rootfs_image` is set (`Template.Name` skips
  it; `docs/plans/ate-env-api/acceptance-criteria.md`). `getOrCreateImageTemplate` reuses cross-path
  `sameImageDigest`, fails (`codes.FailedPrecondition`) on mismatch, and accepts
  `codes.AlreadyExists`. Goldens failed `>= failedTemplateRecreateAfter` (10m) ago write-lock
  `recreateFailedImageTemplate` (UID, 0 `templateUsers`).

## Client connection pooling and event loops

### 7. Python `LoopRunner` thread bridging and HTTP/2 stream concurrency

- **`clients/python/src/ate_env_rl/{loop_runner.py,fleet.py,backend/substrate.py}`,
  `cmd/ate-env-api/main.go`**: `asyncio.run()` destroys HTTP/2 gRPC channels on exit; `LoopRunner`
  runs a daemon loop dispatching coroutines via `asyncio.run_coroutine_threadsafe`, `SandboxFleet`
  sizes connections via `FleetConfig.connections` or `ceil(max_concurrent / 200)`, and
  `SubstrateBackendDriver` pools gRPC channels against `DefaultMaxConcurrentStreams = 4096`.

### 8. Python release error tracking, once-per-handle release, and exit hooks

- **`clients/python/src/ate_env_rl/{backend/substrate,fleet,async_fleet,handle,strategies,types}.py`**:
  `SandboxHandle.release()`/`recycle()` run once per handle (`_begin_release`/`_end_release`,
  retryable on failure/cancel), `release_all()` retries `_active_handles`, `SubstrateBackendDriver`
  logs failed deletes in `RunReport.release_errors` keeping `_owned_actors`, `fleet.teardown()`
  retries (`InfrastructureError` on failure without replacing body errors/interrupts in
  `SandboxFleet.run()`/`with fleet:`), `teardown(verify=True)` calls `verify_teardown` for its run
  (driver keeps `REAP_TARGET_RUNS` runs' targets) returning `TeardownReport` or attaching
  `teardown_report`/`teardown_report_error`, and `FleetConfig(exit_hooks=True)` registers
  `_ExitHooks` (`atexit` + `SIGINT`/`SIGTERM` on `SandboxFleet`, rejected on `AsyncSandboxFleet`).

### 9. Python warm-pool bookkeeping across awaits

- **`clients/python/src/ate_env_rl/{backend/substrate,loop_runner}.py`,
  `clients/python/tests/{test_warm_pool_accounting,test_loop_runner}.py`**: Pooling recycled actors
  past a concurrent `reap` leaks after teardown, and unreserved shortfalls over-provision.
- **Teardown & claim checks**: `reap` (`_reap_internal`) bumps `_teardowns` on entry/exit
  (`_reaping` counts active reaps). Recycles (`_release_internal`), `warm_pool`
  (`_warm_pool_internal`), and claims (`_acquire_internal`) read `_teardowns` pre-await and pool
  only via `_may_pool` (`_teardowns` unchanged, `_reaping == 0`), else delete; claims re-check
  post-`get()` in one lock or delete stale owned actors and retry.
- **Ownership & generations**: Actors stay in `_owned_actors` till deleted/reaped (`_reap_one` pops
  failed deletes too, which `reap` raises and `_reap_targets` keeps). Cold `acquire` overlapping its
  run's reap and cancelled creates (`_own_abandoned_create`, `timeout_s`) tag `WARM_POOL_RUN_ID`.
  `_reap_internal` clears `_warm_inflight` (per key, `_teardowns`, `_pool_gens`, minus shortfalls)
  on entry/exit; `warm_pool(wait=False)` holds `_warm_inflight` and defers
  `_owned_actors`/`_warm_paused_pool` in `_suspend_and_pool` till `suspend()` ends. `unwarm_pool`
  and `resize_pool` below pooled + in-flight bump `_pool_gens` (`_cancel_warm_calls`), so earlier
  `warm_pool` (`drained`) deletes, `_delete_unpooled` re-tags failed/cancelled `WARM_POOL_RUN_ID`
  (`_keep_for_any_reap` vs `release` `_return_popped`/`_may_pool`), and `reap` updates gauges.
- **Entry snapshot & background deletes**: `_acquire_internal` snapshots `entry_teardowns` and
  checks `_run_reaped_since` on own/`_own_abandoned_create`, so a warm claim paused in `get()` over
  its run's reap tags its cold/retried actor `WARM_POOL_RUN_ID` (deleting on cancel). Post-reap
  cancelled cold creates/`warm_pool` delete via `_spawn_background_delete` (`_background_deletes`),
  and `_reap_actors`/`close()`/`close_async()` join same-loop owned deletes via
  `_wait_background_deletes` (finite `background_delete_timeout_s >= 0`, 10s, `0` = no wait).
  Deadline-running deletes (`late`) stay uncanceled on `WARM_POOL_RUN_ID`, while joined
  failed/cancelled land in `_failed_background_deletes` via `_background_delete` (done callbacks log
  pre-start cancels, `NotFound` = gone); `reap` adds both to `InfrastructureError` and `close()`
  logs them. `close()`/`close_async()` call `_count_teardown` (bumping `_teardowns`, setting
  `_closing`, cancelling `preflight_images_async` in `_preflights` via `_cancel_preflights`,
  `_preflight_client` rejecting unregistered with `CancelledError`), blocking post-close dials/short
  `PreflightWarmer` coverage. `close()` cancels/awaits `LoopRunner` tasks (`_quiesce`) pre-close
  (`LoopRunner.stop` drains handlers up to `_STOP_ROUNDS` rounds), while `close_async()`
  (`AsyncSandboxFleet.__aexit__`) keeps caller tasks and `_shutdown_async` loops join-and-close up
  to `_STOP_ROUNDS` rounds. `_return_popped` background-deletes post-teardown (`WARM_POOL_RUN_ID` on
  fail, else run id). Under `_closing`, cancelled `_delete_unpooled` and `_delete_unreaped_in_close`
  (`_reap_one`, `_delete_one`, `_reap_actors` `gather`, `_delete_drained`, `_warm_pool_internal`)
  background-delete unreaped once (outside close, cancelled reap keeps owned; cancelled delete
  starts none). `unwarm_pool_async`/shrinking `resize_pool_async` re-tag popped actors
  `WARM_POOL_RUN_ID` in one lock (`_own_drained_for_any_reap`). `_stop_if_closing` raises
  `CancelledError` at the 16-slot sem in `_reap_one`, `_warm_one` (taking `_warm_budget`, skipping
  `create()` if torn down/drained, checking pre-`suspend()`), and `_delete_one`. Cancelled `get()`
  warm claims re-pool or post-teardown re-tag `WARM_POOL_RUN_ID` and background-delete; `warm_pool`
  re-raises close `CancelledError` from `gather(return_exceptions=True)`, deleting started actors.
