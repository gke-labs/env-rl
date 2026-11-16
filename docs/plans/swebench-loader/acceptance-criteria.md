# swebench-loader: acceptance criteria

Status: describes `main` at `3606ba8d` with #401. `SL-1` to `SL-4` are met (#401): the step loop's
park sends a suspend again after a transient error. Each criterion names the tests that check it on
every pull request (the Go job's `go test -race ./...`).

## Scope

`swebench-loader run`
([internal/benchmarking/swebench/run.go](../../../internal/benchmarking/swebench/run.go)) parks the
fleet of a `-mode steps` run between its steps. This file covers one behavior of that park: what the
loader does when a suspend fails.

The rows apply to the step loop: `-mode steps` without `-strategy` and without the working-set flags
(`-schedule`, `-tasks-per-step`, `-group-size`, `-step-pause`, `-prefetch`). The suspends of a
`-strategy` run and of a working-set run are out of scope, and so is `run_fleet_bench.py`.

A transient error is a gRPC status with the code `Unavailable`, `DeadlineExceeded`, `Unknown`,
`Internal` or `Aborted`. These are the codes on which the loader already retries the first command
of a task (`isTransient`).

## Criteria

| ID   | Criterion                                                                                                                                                                                                                                                                                                                                                                                | Status     | Tests                                                                        |
| ---- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------- | ---------------------------------------------------------------------------- |
| SL-1 | A suspend of the park (`-park suspend`, `suspend-sync`, `suspend-async`, and the local suspend of `fork`; at the step barrier or with `-park-each`) that fails with a transient error is sent again until it succeeds. The environment is parked once, and its `suspend` event is ok.                                                                                                    | met (#401) | `TestSuspendRetriesTransientErrors`, `TestSuspendRetriesOnTheEnvBackend`     |
| SL-2 | The loader pauses `-wake-backoff` before the second attempt and doubles the pause up to five times `-wake-backoff`. It sends no further attempt when the next pause would end later than `-wake-budget` after the first attempt started, or when the run is cancelled. The suspend then fails with the last error. With the defaults (3 minutes, 2 seconds) that is at most 20 attempts. | met (#401) | `TestSuspendRetriesStopAtTheWakeBudget`                                      |
| SL-3 | A suspend that fails with any other error is not sent again: another gRPC code, such as `NotFound`, `FailedPrecondition`, `InvalidArgument` or `ResourceExhausted`, or an error without a gRPC status.                                                                                                                                                                                   | met (#401) | `TestSuspendDoesNotRetryOtherErrors`                                         |
| SL-4 | The `suspend` event of the park records the number of attempts as `attempts`. Its `dur_ms` runs from the first attempt to the end of the last, pauses included. `swebench-loader report` counts a suspend with more than one attempt as `retried`.                                                                                                                                       | met (#401) | `TestSuspendRetriesTransientErrors`, `TestSuspendRetriesStopAtTheWakeBudget` |
