# Test plan: OpenHands on sandboxenv-rl

Status: describes `main` at `40d7ef88` (2026-11-12). How to re-run each criterion in
[acceptance-criteria.md](./acceptance-criteria.md) with the tools in this repository. The recorded
runs used a test harness outside the repository; the commands below reproduce the same checks with
the repository's examples, CLI and packages. Put the results in the pull request's Tests section, or
in [acceptance-criteria.md](./acceptance-criteria.md) when a criterion's status changes.

## 0. Before each run

- Record the commit of each image and branch under test, the cluster, its Substrate version and its
  node pools.
- Snapshot what must not change: `kubectl get ns -o name | sort`, the deployments in `ate-system`,
  the node pools. Compare after the run.
- Record the actor count in `ate-env`, the "no leak" baseline:
  `kubectl ate get actor -a ate-env | wc -l`.
- Never put a token or a session key on a command line that a log keeps. The snippets below read
  them from the environment or from a header file with mode 0600.

## 1. Cluster preparation

1. A cluster that meets the [root README](../../../README.md#get-started): Substrate at the version
   under test (v1.0.0 for `main`, which has the same runtime code as `274605f1`), and ate-env-api's
   SPIFFE ID (`spiffe://cluster.local/ns/ate-env/sa/ate-env-api`) in the router's ingress allow-list
   (#265). Check the ate-env-api startup log: no rejected router certificate.
2. ate-env from the branch under test with the caller token required (`-auth-token-file`), images in
   `<registry>`, as in the root README.
3. The layer, built into your own registry (D-5):

   ```bash
   docker buildx build --platform linux/amd64,linux/arm64 \
     -t <registry>/oh-agent-server-layer:1.55.0 --push integrations/openhands/layer
   crane digest --platform linux/amd64 <registry>/oh-agent-server-layer:1.55.0
   ```

4. The base template and a smoke test, with
   [`examples/openhands/demo.sh`](../../../examples/openhands/demo.sh). It registers
   `oh-agent-server` with `--sidecar-env` when it does not exist, creates one environment for each
   image, drives each agent-server through the port proxy, checks that a command does not see the
   key and that a call without the key gets 401, and deletes the environments:

   ```bash
   kubectl -n ate-env port-forward svc/ate-env-api 7777:7777 &
   export API_TOKEN="$(kubectl -n ate-env get secret ate-env-api-token -o jsonpath='{.data.token}' | base64 -d)"
   GUEST_IMAGE=<registry>/ate-env-guest@sha256:<digest> \
   LAYER_IMAGE=<registry>/oh-agent-server-layer@sha256:<digest> \
   IMAGES="docker.io/library/python@sha256:<digest> docker.io/library/ubuntu@sha256:<digest>" \
   SESSION_KEY=<key> examples/openhands/demo.sh
   ```

   `SESSION_KEY` is required when the template exists; otherwise the script generates one and never
   prints it. `RECREATE=1` deletes the base and its derived templates and registers them again; use
   it only for templates that nobody else uses.

5. The CLI smoke: `ate-env create`, a shell command, `ate-env suspend`, a second command (it wakes
   the environment), `ate-env delete`; the same without the token gets `Unauthenticated`.

   ```bash
   export SUBSTRATE_ENV_AUTH_TOKEN="$API_TOKEN"
   ate-env create smoke1 --template oh-agent-server
   ate-env smoke1 shell 'echo marker > /tmp/m'
   ate-env suspend smoke1 && ate-env get smoke1
   ate-env smoke1 shell 'cat /tmp/m'
   ate-env delete smoke1
   ```

The Python venv for the criteria:

```bash
python3.12 -m venv .venv && . .venv/bin/activate
pip install './clients/python[dev]' './integrations/openhands[test,examples]'
```

## 2. Through the port proxy with curl

The path form reaches a port in an environment:
`http://127.0.0.1:7777/v1alpha/atespaces/ate-env/envs/<env>/ports/8000/<path>`.

```bash
umask 077
printf 'X-Env-Authorization: Bearer %s\nX-Session-API-Key: %s\n' "$API_TOKEN" "$SESSION_KEY" > hdr
P=http://127.0.0.1:7777/v1alpha/atespaces/ate-env/envs/$ENV/ports/8000
curl -s -o /dev/null -w '%{http_code}\n' -H @hdr "$P/health"                          # 200
curl -s -o /dev/null -w '%{http_code}\n' "$P/health"                                   # 401, no token
curl -s -D - -o /dev/null -H @hdr "$P/api/bash/bash_events/search" | grep -i -E '^HTTP|x-env-state|retry-after'
rm -f hdr
```

## 3. `AteEnvWorkspace`

```python
import os
from ate_env_openhands import AteEnvWorkspace

with AteEnvWorkspace(
    template="oh-agent-server",
    image=os.environ["TASK_IMAGE"],            # repo@sha256:..., or omit for the base image
    api_url="127.0.0.1:7777",
    api_token=os.environ["API_TOKEN"],
    api_key=os.environ["SESSION_KEY"],
) as ws:
    assert ws.execute_command("echo ok").exit_code == 0
    ws.execute_command("echo marker > /workspace/m")
    ws.pause()                                 # `ate-env get <id>` shows SUSPENDED
    ws.resume()
    assert ws.execute_command("cat /workspace/m").stdout.strip() == "marker"
```

`AteEnvWorkspace(env_id=..., ...)` attaches to an existing environment and leaves it in place.

## 4. Per criterion

| Criterion | Procedure                                                                                                                                                                                                                                                                                                                                                      |
| --------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| OH-1      | `go build ./... && go vet ./... && go test -race ./...`; `make verify-gofmt verify-boilerplate verify-docs`; `pytest integrations/openhands/tests`; `ruff check integrations/openhands examples/openhands`; the required CI checks                                                                                                                             |
| OH-2      | `diff` upstream `examples/02_remote_agent_server/17_convo_with_agent_sandbox_server.py` (openhands software-agent-sdk v1.55.0) against `examples/openhands/convo_with_ate_env.py`; run `convo_with_ate_env.py` with the scripted LLM (below), once with `TASK_IMAGE` and once without; the pause and resume marker of section 3; actor counts before and after |
| OH-3      | Copy agent-sandbox's `examples/agent-sandbox-rl/examples/run_openhands_fleet.py`; change only its two imports (below); run 8 tasks over 2 images with concurrency 4; `diff` the copy against upstream; actor counts                                                                                                                                            |
| OH-4      | R2E-Gym from its checkout; 2 instances that grade offline (`django__django-11099`, `sympy__sympy-20590`), each with the gold patch and the empty patch, through `make_fleet_repo_env` and `fresh_eval_env` (below); rewards; actor counts                                                                                                                      |
| OH-5      | Out of scope (D-5)                                                                                                                                                                                                                                                                                                                                             |
| OH-6      | Section 3 with and without `api_token`; a `RemoteConversation` that receives events; section 2 with the token in `X-Env-Authorization`, in `Authorization`, absent and wrong; a small daemon in the environment that logs each request's headers and query, read back with `ate-env <env> read`                                                                |
| OH-7      | Timings with `ate_env_rl.SandboxFleet` (below): 100 warm acquires at concurrency 16; 10 cold creates on new images; contended creates, 16 images x 16; a burst of 32 creates on 2 new images and 64 on 4                                                                                                                                                       |
| OH-8      | A doc review of `integrations/openhands/README.md`, `examples/openhands/README.md` and `docs/security/threat-model.md` against the code on `main`                                                                                                                                                                                                              |
| OH-12     | Section 1 on a cluster with ate-env from `main`, then the checks below                                                                                                                                                                                                                                                                                         |
| OH-13     | The cluster on Substrate `274605f1`, then OH-2, OH-3, OH-4 and OH-12                                                                                                                                                                                                                                                                                           |
| OH-14     | From a command in the environment: `cat /proc/*/environ \| tr '\0' '\n' \| grep -c OH_SESSION_API_KEYS`; through ate-env-api: `ate-env <env> read /proc/1/environ`; both must find no key once the fix lands                                                                                                                                                   |

### OH-2: example 17 with the scripted LLM

The agent-server calls the model from inside the environment, so the scripted LLM must run there.
Run the example file unchanged from a small wrapper (for example with `runpy`) that, once the
workspace exists, starts the stand-in with `codeact_agent.start_scripted_llm(workspace)` from
[`examples/openhands/codeact_agent.py`](../../../examples/openhands/codeact_agent.py). It listens on
port 9999 inside the environment:

```bash
SESSION_KEY=<key> API_TOKEN=<token> [TASK_IMAGE=<repo>@sha256:<digest>] \
  LLM_API_KEY=scripted LLM_MODEL=openai/scripted LLM_BASE_URL=http://127.0.0.1:9999/v1 \
  python <wrapper that runs examples/openhands/convo_with_ate_env.py>
```

With D-2 the scripted trajectory writes `/tmp/codeact-demo/hello.txt`, not the example's own file.
[`examples/openhands/README.md`](../../../examples/openhands/README.md) describes the scripted LLM.

### OH-3: the upstream fleet program with the import change

```diff
-from agent_sandbox_rl import (
+from ate_env_openhands.agent_sandbox_rl import (
     ClusterConfig, FleetConfig, ResourceSpec, SandboxFleet, Task, TemplateSpec,
 )
-from agent_sandbox_rl.adapters.openhands import make_handle_workspace
+from ate_env_openhands.agent_sandbox_rl.adapters.openhands import make_handle_workspace
```

```bash
export SUBSTRATE_ENV_API=127.0.0.1:7777 SUBSTRATE_ENV_AUTH_TOKEN="$API_TOKEN" \
  SUBSTRATE_ENV_TEMPLATE=oh-agent-server \
  SANDBOX_SESSION_KEY="$SESSION_KEY" AGENT_SERVER_IMAGE=<repo>@sha256:<digest>
python run_openhands_fleet.py   # with the program's own task and concurrency settings
```

The program reads `AGENT_SERVER_IMAGE` and `SANDBOX_SESSION_KEY` itself. ate-env ignores
`TemplateSpec.extra_pod_spec`, thus the key must be the base template's: with another key, or none,
each task fails in `_check_session_key()` with `PermissionError` (HTTP 401).

### OH-4: an R2E-Gym episode

```bash
pip install -e path/to/R2E-Gym
```

```python
from ate_env_rl import FleetConfig, SandboxFleet, Task
from ate_env_rl.adapters.r2egym import fresh_eval_env, make_fleet_repo_env, r2egym_command_files

fleet = SandboxFleet(FleetConfig(backend="substrate", endpoint="127.0.0.1:7777",
                                 template="oh-agent-server", auth_token=API_TOKEN))
handle = fleet.acquire(Task(id=row["instance_id"], image=IMAGE, metadata={"ds": row}))
try:
    env = make_fleet_repo_env(handle, command_files=r2egym_command_files())
    patch = GOLD_PATCH                         # then "" for the empty patch
    with fresh_eval_env(fleet, handle) as (_eval_handle, grader):
        grader.runtime.apply_patch(patch)
        print(grader.compute_reward())         # gold 1.0, empty 0.0
finally:
    fleet.release(handle)
```

### OH-7: timings

```python
import time
fleet = SandboxFleet(FleetConfig(backend="substrate", endpoint="127.0.0.1:7777",
                                 template="oh-agent-server", auth_token=API_TOKEN))
fleet.load_tasks([Task(id=f"t{i}", image=IMAGE) for i in range(100)])
fleet.set_pool_replicas(IMAGE, 100)            # warm: suspended environments
t0 = time.monotonic(); h = fleet.acquire("t0"); t1 = time.monotonic()
h.exec("true"); t2 = time.monotonic()          # acquire, and to first command
fleet.release(h)
fleet.set_pool_replicas(IMAGE, 0)
```

For cold and contended creates, use images that have no derived template yet
(`ate-env image-status <image> --template oh-agent-server` shows it), and record the time to the
first command. `ate-env warm-image <image> --template oh-agent-server` puts an image on the nodes
before a burst; it is optional and not part of OH-7. Both commands default `--template` to the
server default and do not read `SUBSTRATE_ENV_TEMPLATE`; the derived template is named after the
base, thus the base must be named.

### OH-12: the regression checks

1. Section 3 with `api_token`, and a `RemoteConversation` that receives an event. The derived
   template has `timeoutSeconds: 120` on its wakeup probe
   (`kubectl ate get actor-template --atespace ate-env -o json`).
2. Crash fail-fast: register a throwaway template whose sidecar readiness URL never answers, with
   `--wakeup-timeout 5s`, and create an environment from it. Section 2's request gets 410 with
   `X-Env-State: CRASHED` in about 5 s; `AteEnvWorkspace(env_id=...)` raises in under 15 s;
   `ate-env delete`, then 404. Record the two known blockers: Substrate refuses `DeleteActor` on a
   `CRASHED` actor (`FailedPrecondition` on `DeleteVolumes`), and for 2 s after a delete that passed
   the port proxy can still answer 410 `CRASHED` from its state cache (`stateCacheTTL`).
3. Slow first wake: create an environment on a large image no node has pulled, and send section 2's
   request at once: 503 with `Retry-After: 1` and `X-Env-State`, then 200.
4. Sidecar-only key: `demo.sh` (section 1) checks that a command does not see
   `OH_SESSION_API_KEYS_0` and that the agent-server answers 401 without the key.
5. `SUBSTRATE_ENV_AUTH_TOKEN=sentinel-1234 ate-env create --help | grep -c sentinel-1234` prints 0.
6. Before the cleanup, while an environment is still `RUNNING`, read its actor log with
   `kubectl ate logs actor <env> -a ate-env` (`logs actor` needs `-a` and a running actor). After
   the cleanup, the actor count returns to the baseline. Neither that log nor
   `kubectl -n ate-env logs deploy/ate-env-api` contains the token or the session key.

## 5. Failure triage

- 502 or a long wait on the first request: the daemon listens on 127.0.0.1 only, or the image lacks
  glibc 2.31 (the layer's Python does not start). Check the sidecar log in the actor.
- 401 from the proxy: the token or the `X-Env-Authorization` header. 401 from the agent-server: the
  session key differs from the template's.
- A guest call hangs, then fails: the router rejected ate-env-api's certificate (the allow-list).
- The environment crashes at start with a template that uses `--sidecar-env`: the guest image is
  older than #268 and exits on `-sidecar-env-required`.
- An environment left over after a run: a cleanup path in `AteEnvWorkspace` or the fleet; record a
  finding with the environment id and the run step.

## 6. Cleanup

Delete the environments and templates the run created and the test tags you pushed to `<registry>`.
Leave the cluster installation in place.
