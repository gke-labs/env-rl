# Decisions: OpenHands on sandboxenv-rl

Status: describes `main` at `40d7ef88` (2026-11-12). D-1 to D-5 are the owner's decisions. The
entries after them are the design decisions that the merged pull requests carry, taken in review or
from a live finding. The day-by-day log stays outside the repository; cluster names, project ids,
registry paths and local paths are left out.

## Owner decisions

### D-1: one test cluster (2026-10-26)

- Decision: run the live criteria on one existing GKE test cluster, upgraded in place to GKE 1.37
  with Workload Identity, with Substrate v0.6.0 installed. Blast radius: `ate-system`, `ate-env` and
  `oh-*`.
- Why: Substrate v0.6.0 needs PodCertificateRequest and ClusterTrustBundle (GKE 1.37 by default) and
  Workload Identity for snapshots (F-1).
- Consequence: the cluster ran Substrate v0.6.0 for the recorded runs and `475f49b0` from
  2026-10-29. `main` pinned `274605f1` in #254 and now pins v1.0.0 (#419, which has the same runtime
  code as `274605f1`), thus OH-13.

### D-2: the scripted LLM for OH-2 (2026-10-26)

- Decision: OH-2 uses the scripted LLM in `examples/openhands/scripted_llm.py` only; a real model is
  optional.
- Why: a deterministic run, with no model endpoint or key.
- Consequence: the conversation, the tools and the event stream run for real; only the decisions are
  canned.

### D-3: the layer registry (2026-10-26)

- Decision: the live runs use a layer pushed to a test registry (`<registry>`); GHCR publishing from
  `release.yaml` lands as a pull request.
- Why: the live runs must not wait for the GHCR organization settings.
- Consequence: #128 added the publish step. D-5 reverses the publishing part.

### D-4: Tunix and upstream OpenHands later (2026-10-26)

- Decision: OH-10 (Tunix) and OH-11 (upstream OpenHands) start after OH-1 to OH-9 pass.
- Why: both are external repositories; the API settles here first.
- Consequence: both are stretch goals, not started.

### D-5: no published OpenHands image (2026-11-09)

- Decision: the project publishes no OpenHands image. The layer stays a Dockerfile in
  `integrations/openhands/layer` that users build into their own registry. OH-5 is out of scope.
- Why: the owner does not want to publish and maintain an OpenHands image.
- Consequence: #295 removed the layer from `release.yaml`, `docs/release.md` and the release
  tooling, and the integration README says to build it. The v0.1.0 layer image stays as it is and
  gets no updates.

## Design decisions in the pull requests

### OH-4's reference is the dataset's ground truth (2026-10-26)

- Decision: the gold patch must give reward 1.0 and the empty patch 0.0; a same-instance run on
  agent-sandbox is optional.
- Why: no agent-sandbox cluster was available for a reference run.
- Consequence: OH-4's pass rule is 4 of 4 rewards equal to the ground truth.

### A port capability for the event WebSocket (2026-10-26, #129, #130)

- Decision: the event WebSocket carries a port capability in the URL user info, not the token and
  not a query parameter. The capability is `v1.<expiry>.<mac>`, where `mac` is the hex HMAC-SHA256,
  keyed with the token, of `"ate-env port capability v1"`, the atespace, the environment id, the
  port and the expiry, each followed by a newline except the last
  (`internal/portproxy/capability.go`, `port_capability()` in `workspace.py`). It is valid
  `ws_capability_ttl` (default one day, at most seven days less five minutes). The proxy accepts it
  on WebSocket upgrades only and always strips it.
- Why: openhands-sdk 1.55.0 sends no headers on the WebSocket and drops the query string, but keeps
  the user info, which `websockets` sends as `Authorization: Basic`. A leaked host opens one port of
  one environment until the expiry and never shows the token. Stateless, so it works across API
  replicas.
- Consequence: a token rotation invalidates live capabilities; `resume()` and
  `renew_ws_capability()` put a new one in the host. The threat model lists it.

### Secret lookups stay inside the sandbox (2026-10-27, #131)

- Decision: `AteEnvWorkspace.get_secrets()` names
  `http://127.0.0.1:<server_port>/api/settings/secrets/<name>` with the session key only, and the
  agent-server answers the lookup in process.
- Why: the inherited SDK method put the caller token into each lookup's headers, which the
  agent-server receives (F-2).
- Consequence: no caller credential enters the sandbox through secrets.

### agent-sandbox-rl's names in a compatibility module (2026-10-27, #136)

- Decision: OH-3 stays "imports only". The gaps (F-AC3-1 to F-AC3-4) close with
  `ate_env_openhands.agent_sandbox_rl`, which exposes agent-sandbox-rl's names on top of
  `ate_env_rl`, ignores the Kubernetes-only fields with one INFO log, and refuses what ate-env
  cannot do.
- Why: `ate_env_rl` keeps its own API; the compatibility layer belongs to the integration.
- Consequence: the import diff is the only diff in the upstream program.

### The base template runs in a glibc image (2026-10-27, #134)

- Decision: the base template registers with `--rootfs-image` `debian:bookworm-slim` (pinned).
- Why: the base's own golden actor starts the layer too, and the guest image has no C library (F-3).
- Consequence: an environment without `image=` runs in Debian 12. Derived templates are unchanged.

### Delete without a suspend (2026-10-27, #138)

- Decision: `DeleteEnvironment` calls `DeleteActor` without the suspend first.
- Why: Substrate v0.6.0's `DeleteActor` takes any state, terminates the workload and deletes the
  snapshot files; the suspend added failure modes only, and refused a crashed actor (F-AC7-2).
- Consequence: on Substrate v0.6.0, crashed environments are deleted through ate-env-api, the
  clients and the fleet. Substrate v1.0.0 (the pin on `main` since #419, which has the same runtime
  code as `274605f1` from #254) refuses `DeleteActor` on a `CRASHED` actor with
  `FailedPrecondition`, thus `ate-env delete` of a crashed environment fails there; see OH-12 check
  2 in [acceptance-criteria.md](./acceptance-criteria.md#oh-12-live-regression-of-merged-main).

### A 120 s wakeup probe with a sidecar readiness URL (2026-10-27, #139)

- Decision: `ate-env manifest template` sets the wakeup-probe timeout to 120 s when
  `--sidecar-readyz` is set (else 30 s, written explicitly), with `--wakeup-timeout` to change it;
  the default wake budget rises so that it exceeds the probe plus a pull.
- Why: under load the agent-server sidecar took over 30 s to answer, and every create in the burst
  crashed (F-AC7-1).
- Consequence: templates registered before #139, and their derived templates, keep 30 s until they
  are registered again. #202 later set the default wake budget to five minutes.

### Recreate a failed template only when unused (2026-10-27, #140)

- Decision: a derived template whose golden snapshot failed is deleted and recreated under the same
  name only when no actor references it, after a backoff, under a per-template lock in ate-env-api,
  with a warning for each recreate and each skip.
- Why: a same-name recreate gets a new uid, and suspended environments of the old template would
  then resume with their volumes only, losing their root filesystem and processes with no error.
- Consequence: the lock is per replica; AE-5 in [ate-env-api](../ate-env-api/acceptance-criteria.md)
  states the property per replica.

### The sidecar readiness check gets its own deadline (2026-10-27, #141)

- Decision: the guest's `/readyz` checks the sidecars with one deadline for each call, all pending
  URLs together, not with the probe request's context.
- Why: Substrate drops the probe request after 250 ms, which cancelled the sidecar check (F-AC7-4).
- Consequence: time to ready equals the slowest sidecar boot.

### 410 and 503 rewrites on the response path (2026-10-27, #139)

- Decision: after the router answers inside the tunnel, the proxy reads the actor state and rewrites
  the response: `CRASHED` gives 410 Gone with `X-Env-State: CRASHED`; `DELETING` gives 404 with
  `X-Env-State: DELETING`; a missing actor gives 404 without `X-Env-State`; an empty 504 or 500 for
  a `SUSPENDED` or `RESUMING` actor gives 503 with `Retry-After: 1` and `X-Env-State`. The proxy
  does not resend. No state read on the fast path.
- Why: the router accepts each CONNECT and answers errors inside the tunnel, so a check on a refused
  CONNECT never ran (F-C4B-1); a wake over Envoy's 10 s timeout gave an empty 504 (F-C4C-1). 410 and
  not 502, because clients retry 502 while a daemon starts.
- Consequence: `AteEnvWorkspace`'s health wait stops at once on 410 or `X-Env-State: CRASHED`. A raw
  client gets a retryable 503 on a slow first wake; the docs say to call `resume()` after `pause()`
  before other requests.

### The wake loop stops with its caller (2026-10-27, #142)

- Decision: a separate pull request stops the proxy's wake loop when the caller leaves, and bounds
  each CONNECT by the budget.
- Why: the transport dialed with a context that ignored the caller, and #139 doubled that wait
  (F-R3-1).
- Consequence: an abandoned request no longer holds a wake for the whole budget.

### The auth token is not a flag default (2026-10-28, #143)

- Decision: `--auth-token` has an empty default; the CLI reads `SUBSTRATE_ENV_AUTH_TOKEN` at run
  time.
- Why: cobra prints flag defaults in `--help`, which printed the token (F-C4-1).
- Consequence: `--help` shows no token.

### `--sidecar-env` with the old-guest guard (2026-11-06, #268)

- Decision: `ate-env manifest template --sidecar-env NAME=VALUE` renders `ATE_SIDECAR_ENV_*`; the
  guest removes them from its environment at start and passes them to the sidecars only. The
  template also carries the guest flag `-sidecar-env-required`: an older guest exits at flag parse,
  and a new guest exits when the flag is set and no value arrived.
- Why: the session key reached each command (F-AC8-1). Without the guard, an older guest would start
  the agent-server without a key, and openhands-agent-server 1.55.0 skips the key check when its key
  list is empty, while `/health` still passes. A guest re-exec with a clean environment was not
  worth it, because the agent-server's own environment keeps the key.
- Consequence: commands do not see the key; same-user code can still read it through
  `/proc/<pid>/environ` (OH-14). Bases registered with the key in `--env`, and their derived
  templates, must be registered again.

### A minimal re-lock of the layer (2026-11-06, #267)

- Decision: `lock.sh` seeds the compile with the committed lock and passes its arguments to
  `uv pip compile`; the re-lock moves only the 7 packages with alerts (certifi, fastmcp,
  fastmcp-slim, oauthlib, pillow, pyasn1, pypdf, with `pypdf>=6.21.4,<7`) plus setuptools.
- Why: a full re-resolve moved 54 unrelated packages, 27 of them inside the 7-day Dependabot wait,
  and some with breaking changes (a diff of about 1,140 lines against about 130).
- Consequence: 62 of 75 alerts fixed; the 13 filelock alerts wait for a stable litellm that allows
  filelock 4 (F-L1).

### `--snapshots-bucket` optional, through the Substrate bump (2026-11-07, #254)

- Decision: close #266 and take the change with the Substrate `274605f1` bump in #254: templates
  name no snapshot location unless `--snapshots-bucket` is set.
- Why: Substrate after `d86746d8` rejects `storageLocation` without its preview gate; the bump and
  the flag change belong together.
- Consequence: old bases keep `storageLocation` and must be registered again after the bump.

### No layer image in releases (2026-11-09, #295)

- Decision: the release pipeline builds and signs the ate-env images only; the layer's build, tag,
  signature and notes entries are removed. `docker login ghcr.io` stays as its own step.
- Why: D-5. ko and cosign in the publish job relied on the layer step's login.
- Consequence: users build the layer as in the integration README, section 2.
