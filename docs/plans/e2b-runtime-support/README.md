# Plan: E2B runtime support

Status: the design after the review of 2026-11-13, checked on 2026-11-13 against `main` at
`891160ac`. This plan is merged (#326); no implementation is. Pull requests 0, 1 and 2 are open and
have this design: #363 at `d6a657dd`, #364 at `e266fcbb` and #365 at `f4260135`. The core pull
request for the wake hold is #384 at `885f8490` (issue #382). Checked against `e2b` 2.63.0 (PyPI)
and envd 0.10.2 (`e2b-dev/runtime`, `packages/envd`). The criteria are in
[acceptance-criteria.md](./acceptance-criteria.md).

## 1. Goal

A program written for the E2B Python SDK runs its sandboxes as ate-env environments. The only change
to the program is the import of the sandbox class:

```python
from ate_env_e2b import Sandbox  # was: from e2b import Sandbox
```

`sandbox.commands`, `sandbox.files` and `sandbox.pty` keep E2B's signatures, return types and
exceptions. The endpoint, the token and the atespace come from `SUBSTRATE_ENV_API`,
`SUBSTRATE_ENV_AUTH_TOKEN` and `SUBSTRATE_ENV_ATESPACE`, as in the other integrations.

Non-goals:

- The E2B JavaScript SDK and the `e2b` CLI. They need a server that speaks E2B's REST API (option B
  in section 3).
- E2B's template build API (`Template.build`), forks, named snapshots, volumes and the MCP gateway.
- E2B's cloud as a backend for `SandboxFleet`.
- The code interpreter (`e2b_code_interpreter`), in this series (D-6).

## 2. Research

### 2.1 E2B

- The SDK has two planes. The control plane is REST with an `X-API-KEY` header. The data plane is
  `envd`, a daemon inside each sandbox on port 49983.
- `envd` serves two Connect-RPC services and HTTP routes (`/files`, `/health`, `/init`). The SDK's
  `Commands`, `Pty` and `Filesystem` classes are built on these and have no transport seam:
  replacing envd means rewriting them.
- `envd` is one static Go binary, Apache-2.0. With `-isnotfc -no-cgroups` it runs as a plain
  container process. `/init` sets its access token, environment variables and default user; E2B's
  orchestrator calls it, the SDK never does.
- With `ConnectionConfig.sandbox_url` set, the SDK reaches envd at that one URL and sends
  `extra_sandbox_headers` on each envd call. The SDK constructor builds a working object without a
  REST call.
- The control plane has no injection point: the instance methods call `SandboxApi._cls_*` by class
  name, and `get_api_client` has no transport hook. The private names change between minor releases.
- The SDK needs Python 3.11 or newer, and marks `sandbox_url` as deprecated for removal in `e2b` 3.

### 2.2 What sandboxenv-rl has

| E2B needs                          | sandboxenv-rl on `main`                                                                  |
| ---------------------------------- | ---------------------------------------------------------------------------------------- |
| envd inside any image              | Runtime layers: an image volume plus a guest sidecar with a readiness URL                |
| One URL, header routing to a port  | The port proxy's header form (`X-Env-Id`, `X-Env-Atespace`, `X-Env-Port`), streaming     |
| A credential the daemon never sees | `X-Env-Authorization`; the proxy checks and removes it, and forwards `Authorization`     |
| create, kill, pause, resume        | `CreateEnvironment`, `DeleteEnvironment`, `SuspendEnvironment`, wake on the next request |
| Template ID to image               | A registered template by name, or `rootfs_image` on a base template (derived template)   |
| List, metadata                     | `ListEnvironments` lists one atespace (#392). Missing: no labels on an environment       |
| Timeout (kill after N seconds)     | Missing: no time to live (#76 covers auto-suspend only)                                  |
| `get_host(port)` hostname          | Missing: the port proxy has header and path forms, no hostname form                      |
| Egress rules                       | Missing: no per-environment network policy                                               |

## 3. Options

- **A (chosen).** A subclass of `e2b.Sandbox`, with real envd as a runtime layer. Covers the Python
  SDK, sync and async. Server change: two generic core changes with no E2B dialect, the
  `--add-capability` flag (#363) and the wake hold of the port proxy (section 4.2).
- **B.** E2B's REST API on ate-env-api, with the same layer. Covers Python, JS and the CLI with no
  code change. Cost: a second public API, and wildcard DNS and TLS once `sandbox_url` is removed. It
  stays possible later, because the layer of A is its data plane.
- **C (rejected).** A duck-typed class over `ate_env_rl`, without envd. It rewrites `commands`,
  `files` and `pty`, and drifts with each SDK release.

## 4. Design

This section describes planned code: the layer and its template are pull request 1 (#364), the class
is pull request 2 (#365).

### 4.1 The envd runtime layer

`integrations/e2b/layer` builds an image that holds upstream's released envd binary and its
Apache-2.0 licence. The binary is static, and upstream publishes a sha256 next to it:
`https://storage.googleapis.com/e2b-artifact-binaries/envd/v<version>/envd` for amd64 and
`.../v<version>/arm64/envd` for arm64. The pins are one envd version and two checksums. There is no
source build, thus no builder image and no module proxy at build time; only a patched envd would
need one, and the series carries no patch (D-9). A base template mounts the layer:

```bash
ate-env manifest template --template e2b-base \
  --rootfs-image   <default image>@sha256:<digest> \
  --guest-image    <registry>/ate-env-guest@sha256:<digest> \
  --layer          envd=<registry>/ate-env-e2b-envd@sha256:<digest>=/opt/e2b \
  --sidecar        '/opt/e2b/envd -isnotfc -no-cgroups -port 49983' \
  --sidecar-readyz http://127.0.0.1:49983/health \
  --add-capability SETUID --add-capability SETGID --add-capability CHOWN \
  --add-capability DAC_OVERRIDE --add-capability FOWNER
```

envd sets the user, the group and the groups of each process it starts, also for root. An actor's
container has only `AUDIT_WRITE`, `KILL` and `NET_BIND_SERVICE`, thus without `SETUID` and `SETGID`
no process starts. `CHOWN`, `DAC_OVERRIDE` and `FOWNER` are for the files of other users.

`--add-capability` is pull request 0 (#363). The CLI validates each name against Substrate's
accepted list, and a repeated name is harmless. The capabilities are effective only for a container
that runs as root (section 5, item 6).

The rootfs image needs `/bin/sh` (envd starts each process through it) and `/bin/bash` (the SDK runs
each command and each PTY in it). Template derivation keeps the layer, thus an environment on any
`rootfs_image` starts envd.

### 4.2 The class

Package `ate-env-e2b` in `integrations/e2b`, import `ate_env_e2b`, depending on `ate-env-rl` and
`e2b>=2.63,<3`. It exports `Sandbox(e2b.Sandbox)` and `AsyncSandbox(e2b.AsyncSandbox)`, and
re-exports E2B's exceptions and result types.

**Public names only.** The class overrides only public names of the SDK: the public `create`, and
`connect`, `kill`, `pause`, `get_info` and the other methods with an instance form and a class form,
through a descriptor of our own. It does not use `_create`, `_cls_*`, `_envd_api`,
`_resolve_api_params` or `class_method_variant`. Three couplings remain: the SDK constructor,
`ConnectionConfig(sandbox_url=..., extra_sandbox_headers=...)` and `ApiParams`. The
`ConnectionConfig` carries an API key and an API URL that cannot reach E2B, thus an SDK path that
the class does not map fails closed.

**One async core.** The control-plane mapping is written once, as coroutines. `Sandbox` calls it
through `LoopRunner`, never `asyncio.run()`, and `AsyncSandbox` awaits it. For an `https://`
endpoint the core passes `credentials=grpc.ssl_channel_credentials()` to `ate_env_rl.Client`.

**`create`.** Resolve the template (section 4.4) and call `CreateEnvironment`. Wait for envd's
`/health` through the port proxy. `POST /init` with `envs` only: commands run as `root` in `/root`,
envd's default, and `user=` and `cwd=` on a call change them (D-4). Refuse an image that does not
run as root (D-9). Build the object with the headers `X-Env-Id`, `X-Env-Atespace`,
`X-Env-Port: 49983`, `X-Env-Authorization` and the wake-hold header. A failure after the create
deletes the environment. From there the SDK's own `commands`, `files` and `pty` run unchanged
against real envd.

**No envd access token (D-3).** The class sends no `accessToken` to `/init` and no `X-Access-Token`.
A token adds no protection: each holder of the API token can derive an HMAC token, the port proxy
already requires the API token, and code in the environment runs as root in the same container as
envd. It also adds a race (section 5, item 3).

**A suspended environment (D-8a).** The port proxy answers `503` with `Retry-After: 1` when a wake
takes longer than the router's timeout, and the SDK retries no status on the data plane. The port
proxy therefore gets a generic, opt-in hold: a request with the header `X-Env-Wake-Wait` (working
name) waits while the environment wakes. It is a separate core pull request with no E2B dialect
(section 6). The class sends the header.

**A gone or crashed environment (D-8b).** The control-plane methods read `GetEnvironment` first. On
the data plane the port proxy answers `404` for a deleted environment and `410` for a crashed one,
where E2B's proxy answers `502`. Phase 1 does not translate: a translating transport in the class
needs the SDK's private clients in three objects. A program gets the SDK's stock mapping of the
proxy's answer, and `integrations/e2b/README.md` documents which exception that is.

### 4.3 Method mapping

| E2B                                                         | ate-env                                                         |
| ----------------------------------------------------------- | --------------------------------------------------------------- |
| `create`, `with Sandbox.create()`                           | `CreateEnvironment`, health wait, `/init`                       |
| `kill`                                                      | `DeleteEnvironment`                                             |
| `pause`, `beta_pause`                                       | `GetEnvironment`, then `SuspendEnvironment`, durable (D-5)      |
| `connect` (resumes a paused sandbox)                        | `GetEnvironment`, then the health wait, which wakes it          |
| `get_info`, `is_running`                                    | `GetEnvironment`; envd `/health` only on a running environment  |
| `commands.*`, `pty.*`, `files.*`, `git.*`                   | Inherited: envd through the port proxy                          |
| `timeout`, `set_timeout`                                    | Refused or warned (D-2) until an environment has a time to live |
| `metadata`, `Sandbox.list`                                  | Refused until environments have labels (the list RPC is #392)   |
| `get_host`, `download_url`, `upload_url`                    | Refused until the port proxy has a hostname form                |
| `allow_internet_access=False`, `network`, `update_network`  | Refused: needs per-environment egress policy in Substrate       |
| Forks, snapshots, metrics, volumes, MCP, `iam`, `lifecycle` | Refused: no equivalent                                          |

Refusals are one table bound to the SDK's public signatures, and the default is to refuse: an
argument that is not `None` and not on the allow-list, and a public callable of the SDK base that is
neither mapped nor on the explicit inherited list. A new SDK argument or method is thus refused,
never ignored. Group R of the criteria has the rule.

### 4.4 Templates

E2B's `template` is an opaque string that the server resolves. Here:

1. A name in the `templates` mapping (`SUBSTRATE_ENV_E2B_TEMPLATES`, `name=template` pairs) uses
   that registered template.
2. A digest-pinned image reference (`repo@sha256:...`) runs as `rootfs_image` on the base template
   (`SUBSTRATE_ENV_TEMPLATE`, default `e2b-base`).
3. No template, or E2B's default `base`, uses the base template itself.
4. Anything else raises `TemplateException`.

`e2b.Dockerfile` templates are built and pushed with Docker, then used by digest. E2B's start
command is a second `--sidecar` on a registered template.

## 5. Findings of the checks

From 2026-11-11: Kind, gVisor, Substrate `274605f1`, envd 0.10.2 built from source, `e2b` 2.63.0,
images that run as root. Group V of the criteria has the results that are criteria.

1. envd under gVisor holds, with the five capabilities of section 4.1 (E2B-1, E2B-2).
2. Users: envd's default user is `root`, and `/init` with `defaultUser` changes it. envd refuses a
   command when the home of its user does not exist, thus a default user other than root needs
   `defaultWorkdir` with it. The class sends neither field.
3. `/init` and the token: the first `/init` with a token sets it, and envd then answers `401` to a
   call without it. Before that, a process inside the environment can reach envd on localhost and
   set a token first: a `create` that sends a token then fails. This is one reason for D-3.
4. Transport: the stock SDK works through the port proxy on plaintext HTTP/1.1 and on HTTP/2 through
   a TLS front (E2B-5).
5. `sandbox_url` removal in `e2b` 3: not checked; no `e2b` 3 exists. The fallback is a
   `ConnectionConfig` subclass that overrides `get_sandbox_url` and `get_sandbox_direct_url`.
6. The image's `USER`. At the Substrate pin a container runs as the image's `USER`. Substrate sets
   no ambient capabilities, thus `--add-capability` does not help a non-root process, and the actor
   template has no field for the user. The Kind test of the layer in #364 records the case (E2B-34):
   envd answers `/health` and starts no process. D-9 has the decision.
7. `/bin/bash`. An image without it (Alpine) serves the file calls and fails each command. The class
   must say so in the error, not pass envd's `nice: can't execute '/bin/bash'` through.
8. TLS trust. The SDK verifies an `https://` endpoint against the system certificate store only, and
   on macOS it ignores `SSL_CERT_FILE`. A private CA must be in the system store of the client.

## 6. Phases and pull requests

| Pull request | Content                                                                             | After |
| ------------ | ----------------------------------------------------------------------------------- | ----- |
| 0 (#363)     | `ate-env manifest template --add-capability`                                        |       |
| 1 (#364)     | The envd layer, its template script, the Kind test of the layer                     | 0     |
| 2 (#365)     | `ate_env_e2b.Sandbox`, `AsyncSandbox` if it stays thin, the Kind test of the class  | 1     |
| core         | The opt-in wake hold of the port proxy: a separate core pull request, with an issue |       |
| later        | Environment time to live: API field, enforcement, `timeout` and `set_timeout`       | 2     |
| later        | Environment labels (`ListEnvironments` is #392): `metadata`, `Sandbox.list`         | 2     |
| later        | Hostname form on the port proxy: `get_host`, signed file URLs                       | 2     |
| #390         | The scheduled drift job (section 7)                                                 | 2     |

The core pull request for the wake hold is #384, and its issue is #382: the header is
`X-Env-Wake-Wait: <seconds>`, capped by `-port-proxy-wake-wait-cap`.

- The Kind test of the layer proves only our layer: envd starts, the capabilities suffice, the port
  proxy reaches it, `401` without the API token, suspend and resume, and what envd does on an image
  with a non-root `USER` (E2B-34).
- Pull request 2 also has the refusal table, the unit tests against a fake ate-env-api, the CI job,
  the Nix shell and the README. Its Kind test is the conformance proof: the class against real envd
  through the public SDK surface (E2B-27). If `AsyncSandbox` does not stay thin, it is a pull
  request of its own after 2.
- The class sends the wake-hold header from pull request 2 on; the hold works when the core pull
  request merges.
- Each "later" item gets its own issue before work starts. The first three are useful beyond E2B.
- Not in this series: the code interpreter (D-6) and an envd change for non-root images (D-9).

## 7. Drift check for a new `e2b` release

A scheduled CI job (E2B-37, `.github/workflows/e2b-drift.yaml` and `hack/ci/e2b-drift.sh`, #390):
each week, against the newest `e2b` below 3, it runs the unit tests of `integrations/e2b`, which
hold the structural tests (public names only; the refuse-by-default guards, E2B-28), and the Kind
test of the class (E2B-27). It is not a required check: a new `e2b` release must not fail an
unrelated pull request.

Manual, because a job cannot do it:

1. When the job fails: download the wheel into a directory of its own, read it without an import,
   and decide for each new name whether it is mapped, inherited or refused. Then move the bounds.
2. An envd bump: one version and two checksums. The SDK's own envd gates (`e2b/envd/versions.py`)
   are far below the pin (the highest gate of `e2b` 2.63.0 is 0.6.4, against envd 0.10.2), thus they
   rarely force a bump.
3. The legs that CI cannot run: the conformance script against E2B's cloud (E2B-25) and the HTTP/2
   check through a TLS front (E2B-5).
4. For `e2b` 3: the `sandbox_url` fallback of section 5, item 5.

## 8. Decisions

All are closed, by the owner, on 2026-11-13.

| ID  | Decision                                                                                            | Status |
| --- | --------------------------------------------------------------------------------------------------- | ------ |
| D-1 | Option A, with public SDK names only and one async core. B later if the JS SDK or the CLI is needed | closed |
| D-2 | `timeout`: a `TimeoutNotEnforcedWarning` on each `create` or `connect` call; `set_timeout` refused  | closed |
| D-3 | No envd access token in phase 1                                                                     | closed |
| D-4 | Default user: root in `/root`, envd's default                                                       | closed |
| D-5 | `pause` is a durable suspend                                                                        | closed |
| D-6 | The code interpreter is out of this series; it comes back when a user asks                          | closed |
| D-7 | Python floor 3.11 (the SDK's), for this package only                                                | closed |
| D-8 | (a) A generic wake hold on the port proxy. (b) No translation of a gone environment in phase 1      | closed |
| D-9 | Phase 1 takes images that run as root; no changed envd in the layer                                 | closed |

D-6: the code interpreter needs a second layer and more private overrides.

D-9: `create` refuses an image whose `USER` is not root (E2B-35), and the Kind test of the layer
records what envd does on one (E2B-34). The full answer is a run-as-user field in Substrate
(agent-substrate/substrate#3245). A changed envd in our layer (option 3) is dropped, because it
means carrying a patch to third-party code; this reverses the "3 next" part of the recommendation of
2026-11-12. Ambient and file capabilities (option 4) stay rejected.

## 9. Changes on 2026-11-13

- The class uses public SDK names only; before, it overrode `_create` and `_cls_*`.
- One async core; `AsyncSandbox` (E2B-29) moves into pull request 2.
- Refusals are one refuse-by-default table; E2B-22, E2B-23 and E2B-28 state the rule.
- D-3: no envd token. E2B-21 and the token half of E2B-4 are retired; E2B-9 says "no token".
- D-8: a generic wake hold in core (E2B-16); the "gone" fidelity is not planned (E2B-19).
- D-6: the code interpreter leaves the series (E2B-30).
- D-9: option 3 is dropped, with its phases row, drift step 6 and E2B-36.
- The layer takes upstream's released envd binary, not a source build (E2B-6).
- The Kind test of the class (E2B-27) is in pull request 2, with E2B-35; the former pull request 3
  is gone. The Kind test of the layer is reduced (E2B-2 follows it), and gets E2B-34.
- Criteria that test E2B's SDK or envd are folded into E2B-27: E2B-17, E2B-20, and the working
  halves of E2B-16 and E2B-19. E2B-3 is retired. E2B-14 states the behaviour, not `LoopRunner`.
  E2B-18 keeps only the `envs` of `create`. E2B-5 and E2B-25 each have a manual leg.
- E2B-15 adds that the `ConnectionConfig` holds nothing that reaches E2B.
- The drift check becomes a scheduled CI job (E2B-37, new).
- `--add-capability` validates names in the CLI, and a repeated name is harmless.
- D-1, D-2, D-4, D-5 and D-7 are closed as pull request 2 implements them.
- Removed: the research on AgentENV and agent-sandbox, the pull request history of section 4.2, and
  the prose that repeated the criteria.
