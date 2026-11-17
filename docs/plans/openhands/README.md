# Plan: OpenHands on sandboxenv-rl

Status: describes `main` at `40d7ef88` (2026-11-12). Workstreams W0 to W10 are done: every plan pull
request and every follow-up is merged. Criteria OH-1 to OH-4, OH-6, OH-7 and OH-9 are met, measured
on the live test cluster from the plan's branch heads before they merged. OH-8 (docs) is met: the
review of 2026-10-27 passed, and #345 removed the drift that `integrations/openhands/README.md` and
`docs/security/threat-model.md` had on 2026-11-09. OH-5 (a published layer image) is out of scope by
decision D-5. OH-12 (a live regression of merged `main`) and OH-13 (the same on Substrate v1.0.0,
which has the same runtime code as `274605f1`, the version `main` pinned in #254) are open and not
run. OH-14 (the session key not readable through `/proc`) is a follow-up. OH-10 and OH-11 (Tunix,
upstream OpenHands) are stretch goals and not started.

## Goal

The task runs that people write against agent-sandbox run on sandboxenv-rl with a small, mechanical
change:

- **OpenHands SDK users.** `AteEnvWorkspace` in
  [`integrations/openhands`](../../../integrations/openhands/README.md) (`ate-env-openhands`, module
  `ate_env_openhands`) is an OpenHands `RemoteWorkspace` that mirrors OpenHands' own
  `AgentSandboxWorkspace`. Code moves by changing the workspace constructor: upstream example 17
  runs as
  [`examples/openhands/convo_with_ate_env.py`](../../../examples/openhands/convo_with_ate_env.py).
- **agent-sandbox-rl fleet users.** `ate_env_openhands.agent_sandbox_rl` has agent-sandbox-rl's
  fleet API (`ClusterConfig`, `FleetConfig`, `ResourceSpec`, `TemplateSpec`, `SandboxFleet`, `Task`,
  `adapters.openhands`) on top of `ate_env_rl`. A program such as agent-sandbox's
  `run_openhands_fleet.py` moves by changing its imports only.
- **R2E-Gym.** `ate_env_rl.adapters.r2egym` (since #314; `ate_env_openhands.r2egym` re-exports it)
  runs R2E-Gym's `RepoEnv` on a sandbox the fleet acquired, with clean grading in a second sandbox
  (`fresh_eval_env`).
- **The agent-server layer.** Any unmodified task image with glibc 2.31 or newer gets the OpenHands
  agent-server at run time: a runtime layer mounted read-only at `/opt/oh`, started by the guest as
  a sidecar, reached through ate-env-api's port proxy. No per-image rebuild and no agent-server in
  the image. Users build the layer from
  [`integrations/openhands/layer`](../../../integrations/openhands/layer/Dockerfile) into their own
  registry (D-5).

Non-goals: MLPerf compliance, the policy endpoint in front of a trainer's sampler, and a published
OpenHands image.

## Companion documents

- [acceptance-criteria.md](./acceptance-criteria.md): OH-1 to OH-14, their status and evidence.
- [execution-plan.md](./execution-plan.md): roles, workstreams W0 to W12, the loop, the state on
  `main` and the open work.
- [test-plan.md](./test-plan.md): how to re-run each criterion with the tools in this repository.
- [findings.md](./findings.md): each failure the live runs and reviews found, and the fix.
- [decisions.md](./decisions.md): D-1 to D-5 and the design decisions the pull requests carry.

Related criteria that the repository owns, cited and not repeated here:
[ate-env-api](../ate-env-api/acceptance-criteria.md) (AE-1 to AE-5, WF-1 to WF-8) and
[port proxy](../port-proxy/acceptance-criteria.md) (PP-1, PP-2).

## What shipped

| Scope                                                                                                               | Pull requests |
| ------------------------------------------------------------------------------------------------------------------- | ------------- |
| `integrations/openhands`: `AteEnvWorkspace`, the layer, examples, fleet and R2E-Gym adapters, CI job                | #14           |
| Release publishing of the layer (later removed by #295, D-5)                                                        | #128          |
| Port capability for the event WebSocket: proxy (#129), workspace (#130)                                             | #129, #130    |
| Secret lookups that the agent-server answers itself, without the caller token                                       | #131          |
| Base template in a glibc root filesystem, so its own golden actor starts                                            | #134          |
| HTTP client kept open across `pause()` and `resume()`                                                               | #135          |
| agent-sandbox-rl's fleet API on ate-env (`ate_env_openhands.agent_sandbox_rl`)                                      | #136          |
| R2E-Gym copy and `PATH` fixes                                                                                       | #137          |
| Delete a crashed environment without the suspend                                                                    | #138          |
| Wakeup-probe timeout (120 s with a sidecar readiness URL), port-proxy fail-fast (410, 503 with `Retry-After`)       | #139          |
| Recreate an unused derived template whose golden snapshot failed                                                    | #140          |
| Guest sidecar readiness check with its own deadline                                                                 | #141          |
| Port-proxy wake loop stops when the caller leaves                                                                   | #142          |
| `ate-env <cmd> --help` does not print the auth token                                                                | #143          |
| Follow-up: README states that the router allow-list must list ate-env-api's SPIFFE ID                               | #265          |
| Follow-up: minimal re-lock of the layer's dependencies for the security alerts                                      | #267          |
| Follow-up: `ate-env manifest template --sidecar-env`, the session key reaches only the agent-server                 | #268          |
| Follow-up: releases build no layer image (D-5)                                                                      | #295          |
| Follow-up: the OpenHands README and the threat model match `main` after v0.1.0 and the Substrate `274605f1` bump    | #345          |
| Follow-up: the agent-sandbox-rl compatibility fleet tears down once, also after a failed preflight on the last pass | #346          |
| Superseded: `--snapshots-bucket` optional (#266, closed); shipped in the Substrate `274605f1` bump                  | #254          |
