# Acceptance criteria: OpenHands on sandboxenv-rl

Status: describes `main` at `40d7ef88` (2026-11-12). Met: OH-1 to OH-4 and OH-6 to OH-9. Out of
scope: OH-5 (D-5). Stretch, not started: OH-10, OH-11. Open, not run: OH-12, OH-13. Follow-up:
OH-14. The live criteria were measured from 2026-10-26 to 2026-10-27 on the plan's branch heads,
combined before they merged (the "combined states" below); merged `main` has not been run live since
(OH-12). [test-plan.md](./test-plan.md) says how to re-run each criterion.

Conventions:

- Test cluster: a GKE 1.37 test cluster, gVisor workers on e2-standard-16 nodes, 2 CPU / 4 GiB per
  environment, Substrate v0.6.0, later `475f49b0`. Five workers, one for each node; 35 environments
  run at once. Snapshots in a bucket in another region than the cluster.
- `$API` is ate-env-api, port-forwarded
  (`kubectl -n ate-env port-forward svc/ate-env-api 7777:7777`). `$TOKEN` is the caller token from
  the `ate-env-api-token` Secret; ate-env-api runs with `-auth-token-file`.
- `$BASE` is the base OpenHands template `oh-agent-server` (layer, sidecar, readiness URL, session
  key `$OHKEY`), registered as [`examples/openhands/demo.sh`](../../../examples/openhands/demo.sh)
  does. `<registry>` is the registry that holds the layer and the ate-env images.
- `$VENV` is a Python 3.12 venv with `clients/python[dev]`, `integrations/openhands[test]` and
  openhands-sdk 1.55.0.
- "No leak" means the actor count in the `ate-env` atespace returns to the count before the run
  (`kubectl ate get actor -a ate-env | wc -l`).
- Evidence: transcripts and timing files of the recorded runs are kept outside the repository; the
  numbers below are copied from them, with the commit under test.
- Combined states (`f06f1dc`, `bcabb35`, `00e697e`, `389f428`) are local merges of the open branch
  heads with `main`, built into the images under test. They were not pushed; the pull requests they
  combine are named with each result.

| ID    | Criterion                                                                                                         | Status                   | Evidence                                                                                                                                                            |
| ----- | ----------------------------------------------------------------------------------------------------------------- | ------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| OH-1  | #14 up to `main`; all suites and CI green                                                                         | met                      | `5ffe3fe` (2026-10-26): OpenHands 68 passed, 1 skipped; Go and Python suites; CI green                                                                              |
| OH-2  | Upstream example 17 runs live with only the workspace line changed; pause and resume keep a file                  | met                      | `00e697e` (combined state 3): 22.0 s and 23.1 s runs; marker survives; no leak                                                                                      |
| OH-3  | An agent-sandbox-rl OpenHands rollout runs live with only its imports changed                                     | met                      | `00e697e`: smoke 8/8 in 13.6 s; conversation 8/8 in 35.4 s; diff is the 2 imports                                                                                   |
| OH-4  | An R2E-Gym episode runs live and gives the reference rewards                                                      | met                      | `00e697e`: django-11099 and sympy-20590, gold 1.0 and empty 0.0, 4/4                                                                                                |
| OH-5  | The layer is published multi-arch and signed, and the README pins its digest                                      | out of scope (D-5, #295) | No published OpenHands image; users build the layer                                                                                                                 |
| OH-6  | With the caller token required, commands, files and the event WebSocket work; the token never reaches the sandbox | met                      | `f06f1dc` (combined state): 20/20 checks                                                                                                                            |
| OH-7  | Warm, cold and contended acquire numbers recorded next to agent-sandbox's                                         | met (record only)        | `bcabb35` (combined state 2): table below; burst on new images 0/32 and 0/64 crashed after the fixes                                                                |
| OH-8  | Docs and threat model describe the merged behavior                                                                | met                      | Doc review round 2 (2026-10-27): no blocking or before-merge finding; the drift found on 2026-11-09 is fixed by #345 (`40d7ef88`), checked on `main` (section OH-8) |
| OH-9  | The plan pull requests are approved and merged                                                                    | met                      | All 15 squash-merged by 2026-11-02; CI green on `main`                                                                                                              |
| OH-10 | A Tunix DeepSWE episode runs with `--sandbox_backend ate-env` on a fork branch                                    | stretch, not started     | —                                                                                                                                                                   |
| OH-11 | A draft upstream pull request adds `AteEnvWorkspace` to OpenHands                                                 | stretch, not started     | —                                                                                                                                                                   |
| OH-12 | Merged `main` passes the live regression on the test cluster                                                      | open, not run            | —                                                                                                                                                                   |
| OH-13 | OH-2, OH-3, OH-4 and OH-12 pass on Substrate `274605f1`                                                           | open, not run            | —                                                                                                                                                                   |
| OH-14 | Code in an environment cannot read the agent-server session key, also through `/proc`                             | follow-up                | #268 removed the key from commands; the `/proc` read remains                                                                                                        |

## Notes added 2026-11-09

- The Python client is `ate-env-rl` (module `ate_env_rl`) since #232. Where a criterion or an older
  transcript says `ate_env`, read `ate_env_rl`. OH-3's rule is unchanged: the upstream file changes
  only its two imports, to `ate_env_openhands.agent_sandbox_rl`.
- OH-5 is out of scope (D-5): the project publishes no OpenHands image. #295 removed the layer from
  the release pipeline.
- The repository owns property-level criteria that overlap this plan's findings:
  [ate-env-api](../ate-env-api/acceptance-criteria.md) (AE-1 to AE-5, derived templates; WF-1 to
  WF-8, wake, fail-fast and cancellation for guest calls) and
  [port proxy](../port-proxy/acceptance-criteria.md) (PP-1 and PP-2, the path and the query
  forwarded as sent). This plan checks only what an OpenHands user sees (OH-12, OH-13) and cites
  those.

## OH-1: #14 up to `main`

- Setup: a checkout of the branch under test; `$VENV`.
- Commands: `go build ./... && go vet ./... && go test -race ./...` (macOS, and Linux in
  `golang:1.27` with `--init`); `make verify-gofmt`; `hack/verify/boilerplate.sh`; the `ci.yaml`
  Python steps; `pytest integrations/openhands/tests` and
  `ruff check integrations/openhands examples/openhands` in `$VENV`; `gh pr checks 14 --required`.
- Expected: all pass; the OpenHands job and the required CI jobs green; `git merge-tree` with
  `origin/main` is clean.
- Pass rule: all of the above on one commit.
- Evidence: met at `5ffe3fe` (2026-10-26), after merging `main` `e5fd731`. Go build, vet and
  `test -race` on macOS and Linux, gofmt and boilerplate pass. Python client 225 tests (`[dev]`),
  246 (`+tracing`), 265 (`+tracing,metrics`); swebench 12 + 12; NeMo Gym 50; OpenHands 68 passed, 1
  skipped (the gated e2e test); ruff clean. GitHub checks green. #14 merged 2026-10-28.

## OH-2: Upstream example 17, live

- Setup: `$BASE` registered; the scripted LLM in
  [`examples/openhands/scripted_llm.py`](../../../examples/openhands/scripted_llm.py) (D-2).
- Commands:
  1. `diff` upstream
     `software-agent-sdk/examples/02_remote_agent_server/17_convo_with_agent_sandbox_server.py`
     (v1.55.0) against `examples/openhands/convo_with_ate_env.py`.
  2. Run `convo_with_ate_env.py` against `$API` with `$TOKEN` and `$OHKEY`, once on a task image and
     once on the base image.
  3. In the same run: write a marker file, `pause()`, check at once that the environment is
     `SUSPENDED` (`ate-env get` right after `pause()` returns: the example keeps the conversation
     open, as upstream does, and the SDK's WebSocket reconnect wakes the environment about 1 s
     later; to keep it suspended, close the conversation before `pause()` and reattach it after
     `resume()`), `resume()`, read the marker, send a second message.
- Expected: the diff touches only the imports and the workspace construction; the conversation
  finishes; the marker survives; at exit the environment is deleted.
- Pass rule: all expected results, and no leak.
- Evidence: met at `00e697e` (combined state 3, 2026-10-27). Task image `python:3.12-slim` 22.0 s,
  base image 23.1 s; second turn after pause and resume; marker survives `SUSPENDED` (pause 2.1 s)
  to `RUNNING` (resume 0.8 s); the `with` body is AST-identical to upstream (one comment line
  differs); no leak. Fixes found on the way: F-AC2-1 (#135), F-AC2-2 (#135), F-3 (#134).

## OH-3: agent-sandbox-rl OpenHands rollout, live

- Setup: `$BASE` registered; two digest-pinned task images with glibc 2.31 or newer
  (`python:3.12-slim` and `ubuntu`).
- Commands: copy agent-sandbox's `examples/agent-sandbox-rl/examples/run_openhands_fleet.py`; change
  only its imports to `ate_env_openhands.agent_sandbox_rl`; run it against `$API` with 8 tasks over
  the 2 images, concurrency 4, once as a smoke run (commands) and once with a conversation for each
  task.
- Expected: the import diff is the only diff; all 8 tasks finish; each handle is released; the warm
  pools shrink back after the run.
- Pass rule: 8 of 8 tasks finish, and no leak.
- Evidence: met at `00e697e` (#136 at `6632c83`, 2026-10-27). Upstream file byte-identical to the
  earlier copy, diff only in the 2 imports. Smoke 8/8 in 13.6 s; conversation 8/8 `FINISHED` over
  the event WebSocket with the token required, 35.4 s. One warm pool of 4 for each image served all
  8 tasks; nothing left. An empty or unknown strategy raises upstream's `ValueError`; the token is
  masked in `repr`, `model_dump()`, `model_dump_json()` and validator errors. Fixes: F-AC3-1 to
  F-AC3-4 (#136).

## OH-4: R2E-Gym episode, live

- Setup: R2E-Gym installed from its checkout into `$VENV`; two instances whose images run the layer
  (Debian or Ubuntu bases) and grade offline (F-AC4-3).
- Commands: for each instance: `fleet.acquire(Task(id, image))`, `make_fleet_repo_env(handle)`,
  apply the gold patch, reward with clean grading through `fresh_eval_env`; then the same with the
  empty patch; `fleet.release` for each handle.
- Expected: gold patch reward 1.0, empty patch reward 0.0, for both instances.
- Pass rule: 4 of 4 rewards equal the reference (the dataset's ground truth, see
  [decisions.md](./decisions.md)), and no leak.
- Evidence: met at `00e697e` (#137 at `747b0da`, 2026-10-27). `django__django-11099` and
  `sympy__sympy-20590`: gold 1.0, empty 0.0 (4/4); the django empty patch fails 2 of 22 tests, gold
  passes 22; 172 s in total; all 6 environments gone. The copy-timeout and list-environment paths
  are covered by hermetic tests only. Fixes: F-AC4-1, F-AC4-2 (#137).

## OH-5: Layer published (out of scope)

- Out of scope since 2026-11-09 (D-5): the project publishes no OpenHands image. The layer stays a
  Dockerfile that users build into their own registry
  ([integrations/openhands README, section 2](../../../integrations/openhands/README.md#2-build-the-layer)).
  #295 removed the layer from `release.yaml`, `docs/release.md` and the release tooling.
- The criterion as written: `docker buildx imagetools inspect <layer ref>` shows `linux/amd64` and
  `linux/arm64`; `cosign verify` passes; the README pins the digest; the layer starts on
  `python:3.12-slim` and on one SWE-bench image.
- v0.1.0 (2026-11-07) published a layer image before #267's re-lock; it gets no updates.

## OH-6: Caller token required

- Setup: ate-env-api runs with `-auth-token-file`.
- Commands: an `AteEnvWorkspace` with `api_token=$TOKEN`: one command, one file write and read, and
  a `RemoteConversation` that receives its events over the WebSocket; the same requests without the
  token; a probe daemon in the environment that logs the method, path, query and headers of each
  request it receives; raw requests through the port proxy in the header form and the path form.
- Expected: with the token, all work and events arrive; without it, the proxy answers 401 before it
  dials; the daemon never sees `X-Env-Authorization`, the caller's `Authorization`, the token or a
  port capability.
- Pass rule: all expected results.
- Evidence: met at `f06f1dc` (combined state, 2026-10-27), 20 of 20 checks. Command exit 0; file
  content identical; `ConversationStateUpdateEvent`, `SystemPromptEvent` and `MessageEvent` over the
  WebSocket, with a port capability in the workspace host and no token. Proxy: 200 with the token in
  either header, 401 without or with a wrong token, 401 for a raw token as a capability and for a
  capability on a non-upgrade request, 400 for a capability next to another credential. The daemon
  received only the four accepted requests and none of the credentials; a dump of the sandbox files
  and the actor's container log had 0 hits. Fixes: F-2 (#131).

## OH-7: Performance next to agent-sandbox

- Setup: the test cluster above (35 environments at most at once); `$BASE`; images pinned by digest.
- Commands: warm acquire (pool replicas at least the concurrency), 100 acquires; cold create from a
  new image, 10 images; contended creates, target 256 with group size 16. Record the cluster, node
  shapes and Substrate version.
- Expected: the runs finish with at most 1 % infrastructure errors.
- Pass rule: the table exists next to agent-sandbox's numbers. No threshold.
- Evidence: met at `bcabb35` (combined state 2, 2026-10-27), Substrate v0.6.0.

| Path                                      | ate-env                                                                          | agent-sandbox                  |
| ----------------------------------------- | -------------------------------------------------------------------------------- | ------------------------------ |
| Warm acquire, n=100, concurrency 16       | `acquire()` p50 0.098 s; to first command p50 1.62 s, p95 2.26 s; 0/100 errors   | claim to Ready p50 0.23-0.32 s |
| Cold create, 10 new images, one at a time | to first command p50 23.0 s, max 26.0 s; 0/10 errors                             | cold claim 10-55 s, quiet      |
| Contended, 256 creates, 16 images x 16    | images with golden snapshots: p50 45.3 s, p95 83.0 s; 0/256 errors               | contended p50 228 s            |
| Contended, new images                     | 16 on one image: p50 27.9 s, 0 errors; 32 on two images: 32/32 crashed (F-AC7-1) | —                              |
| `AteEnvWorkspace`, baked image, n=3       | to first command 3.2-3.5 s                                                       | —                              |

- After the fixes (#139, #140, #141; 2026-10-27, `389f428`): 32 creates on 2 new images 0/32 crashed
  (p50 39.5 s, max 41.2 s); 32 on the two OH-7 images 0/32; 64 on 4 new images 0/64 (two waves over
  capacity, max 78.7 s with the queue). Each boot under load took over 30 s (min 31.4 s) and under
  120 s, the new wakeup-probe timeout.
- No leak after cleanup. The 256 run is 256 concurrent create requests, not 256 running
  environments, because the cluster runs 35 at most.

## OH-8: Docs

- Setup: the merged heads of the plan pull requests.
- Commands: a reviewer's doc review of `integrations/openhands/README.md`,
  `examples/openhands/README.md` and `docs/security/threat-model.md` against the code.
- Expected: the READMEs match the code; the threat model covers the OpenHands points (the session
  key readable through `GetActorTemplate`, the agent-server checks only the session key, the
  WebSocket capability); docs reference only merged code.
- Pass rule: the doc review has no blocking finding.
- Evidence: the review passed 2026-10-27. Round 2: 16 of 18 findings fixed, the rest nits; no
  blocking or before-merge finding left. #268 later added `/proc/<pid>/environ` and snapshot memory
  to the threat model.
- Drift found on 2026-11-09 (`main` `27d4bfd8`) and fixed by #345 (`40d7ef88`):
  `integrations/openhands/README.md` called the live R2E-Gym episode "the acceptance step still
  open" (met, OH-4); it said that a failed constructor deletes only what it created, while a failed
  create leaves its environment with `delete_on_exit=False`, and
  `AteEnvWorkspace(env_id=..., delete_on_exit=True)` deletes the attached environment when the
  constructor fails after the attach (`cleanup()` in `workspace.py`).
  `docs/security/threat-model.md` said that the repository has published no release (v0.1.0 shipped
  2026-11-07) and described the image `USER` change (Substrate `5d226a50`) as a future bump, while
  `274605f1` (#254) carries it. Each of these four statements is correct on `main` at `40d7ef88`.

## OH-9: Merged

- Expected: the plan pull requests are approved by a reviewer other than the author and
  squash-merged; CI is green on `main` after each merge.
- Pass rule: all merged.
- Evidence: met 2026-11-02, `main` `f46ae9c`. #14 (`e0914539`), #128, #129, #130, #131, #134, #135,
  #136, #137, #138, #139, #140, #141, #142, #143, each with an approval. Follow-ups #265, #267, #268
  merged 2026-11-08 and #295 on 2026-11-09.

## OH-10 and OH-11 (stretch, D-4)

- OH-10: a branch of `google/tunix` with `--sandbox_backend {agent-sandbox,ate-env}` in
  `examples/deepswe`; one r2egym-scaffold episode (no training step) against `$API` gives the OH-4
  reward for the same instance and patch. Pass rule: the reward matches. Not started.
- OH-11: a draft pull request to `OpenHands/software-agent-sdk` with
  `openhands.workspace.AteEnvWorkspace` and an example. With D-5 it documents building the layer; it
  ships no image. Pass rule: the draft exists. Not started.

## OH-12: Live regression of merged `main`

- Setup: the test cluster with ate-env-api, the guest and the templates built from current `main`.
  `oh-agent-server` registered again by `examples/openhands/demo.sh` with `--sidecar-env` (#268).
- Commands and expected:
  1. `AteEnvWorkspace`: a command, a file upload and download, a `RemoteConversation` event over the
     WebSocket with the caller token required; the derived template has a wakeup-probe
     `timeoutSeconds` of 120.
  2. Crash fail-fast: a raw request through the port proxy to a crashed environment gets 410 with
     `X-Env-State: CRASHED` in about 5 s; `AteEnvWorkspace(env_id=...)` raises in under 15 s;
     `ate-env delete`, then 404. WF-2 covers the guest-call side. Two blockers on `main` for the
     delete: Substrate refuses `DeleteActor` on a `CRASHED` actor (`FailedPrecondition`, the
     `DeleteVolumes` prerequisite; the `deleteWarmProbe` comment in
     `internal/apiservice/placement.go` records it), and the port proxy answers from a 2 s state
     cache (`stateCacheTTL` in `internal/portproxy/portproxy.go`), thus a request within 2 s of a
     delete that passed can still get 410 `CRASHED`. Record what the run shows for both.
  3. Slow first wake on a never-pulled image: 503 with `Retry-After: 1` and `X-Env-State`, then 200.
  4. Sidecar-only key: a command started through ate-env-api does not see `OH_SESSION_API_KEYS_0`
     (demo.sh checks it), and the agent-server answers 401 without the key and 200 with it.
  5. `ate-env create --help` with a sentinel `SUBSTRATE_ENV_AUTH_TOKEN` does not print it (the root
     command registers no `--auth-token` flag; each subcommand does).
  6. No leak; the token and the session key appear 0 times in the logs.
- Pass rule: all six.
- Evidence: not run. Checks 2 and 3 passed on branch heads before #139 merged (2026-10-27: 410 in
  5.30-5.36 s, `AteEnvWorkspace` raises after 5.38 s; slow first wake 503 at 10.3 s, then 200), and
  check 5 is the #143 fix; none was run on merged `main`.

## OH-13: Substrate `274605f1`

- Setup: the test cluster on Substrate v1.0.0, which has the same runtime code as `274605f1` (`main`
  pins v1.0.0 since #419 and pinned `274605f1` in #254), ate-env from `main`.
- Commands: the OH-2, OH-3, OH-4 runs and OH-12.
- Expected: unchanged results. Watch for the tmpfs `/tmp` and `/dev/shm` mounts (Substrate #3533)
  under the agent-server and R2E-Gym (copy targets, temp files, size), and the `capabilities.add`
  restriction and nodev mounts (Substrate #3580) for the guest and the layer.
- Pass rule: all four criteria pass.
- Evidence: not run. The test cluster runs `475f49b0`.

## OH-14: Session key not readable in the environment (follow-up)

- State on `main`: since #268, commands started through ate-env-api do not get the key in their
  environment. Code that runs as the same user can still read it from `/proc/<pid>/environ` of the
  guest, PID 1 and the agent-server, and any ate-env-api caller can `ReadFile` those paths. The
  [threat model](../../security/threat-model.md) states it.
- Options: `prctl(PR_SET_DUMPABLE, 0)` in the guest after it takes the sidecar environment, plus a
  small launcher for the agent-server (needs a gVisor check); or a separate uid for the
  agent-server.
- Pass rule: from a command in the environment, and through `ReadFile`, no path yields the key; the
  agent-server still accepts it.
- Evidence: none yet; waits for the owner's choice.
