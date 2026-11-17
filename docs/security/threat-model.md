# Threat model

This document describes the trust boundaries of ate-env as the code on `main` implements them:
what each boundary protects, who can attack it, what mitigates each threat today, and the known
gaps. It describes merged behavior only.

A pull request that adds network surface, privileges, wire or API fields, or dependencies must
update this document in the same pull request. The pull request template asks for it.

## System

```
 callers ──(1)──▶ ate-env-api ──(2)──▶ Substrate control plane (ateapi)
 (CLI, SDKs,       │  gRPC (h2c)
  MCP clients,     │  MCP (HTTP)                ┌───────────── actor (gVisor) ─────────────┐
  port proxy       │  port proxy (HTTP)         │ ate-env-guest (PID 1, port 80)           │
  clients)         └──────(3)──▶ atenet router ─┼─▶  processes, files, sidecars, daemons   │
                                                └──────────────────────────────────────────┘
```

- **ate-env-api** serves three things on one plaintext listener (`-listen`, default
  `0.0.0.0:7777`): the gRPC API (`EnvironmentService`, `ProcessService`, `FileSystemService`), the
  MCP endpoint `/v1alpha/envs/{id}/mcp`, and the port proxy (header form `X-Env-Id` and
  `X-Env-Port`, path form `/v1alpha/envs/{id}/ports/{port}/...` and, only with
  `--port-proxy-host-suffix <domain>`, hostname form: a `Host` of `<port>-<id>.<domain>` or
  `<port>-<id>.<atespace>.<domain>`).
- **Lifecycle calls** (create, get, suspend, delete) go to the Substrate control plane over TLS
  with a bearer token.
- **Guest calls, MCP and the port proxy** go to the atenet router over mutual TLS (HTTPS for guest
  gRPC on port 443 and TLS HTTP CONNECT on port 8444) when `-atenet-client-cert` and
  `-atenet-ca-file` are set, which `ate-env manifest` enables by default using kubelet-projected pod
  identity (`podidentity.podcert.ate.dev/identity`) and `servicedns`
  (`servicedns.podcert.ate.dev/identity`) bundles reloaded at each handshake; without
  `-atenet-client-cert`, `ate-env-api` dials plaintext ports 80 and 8081 (served only by test
  fakes). The router forwards requests to the actor named in the `ate-target-actor` header.
- **Telemetry**, off by default: with `--metrics-listen-addr`, `ate-env-api` serves Prometheus
  metrics on a second plaintext listener; with `--enable-tracing`, it exports traces over OTLP and
  passes the caller's W3C trace context on to the control plane and the guests (see
  [Server telemetry](#server-telemetry)).
- **ate-env-guest** runs as PID 1 in each actor. It serves processes and files on port 80, reaps
  orphaned processes, and starts the sidecars that the template names.
- **`ate-env manifest`** deploys the namespace, the WorkerPool (or one WorkerPool per placement
  domain with `--domains`), and the `ate-env-api` ServiceAccount, Deployment and Service.
  `ate-env manifest workerpool` prints WorkerPool manifests alone, and `ate-env manifest access-policy`
  grants that ServiceAccount the `editor` role on an atespace (`--global` grants the global `viewer`
  role that `ListPlacementDomains` needs).

## Assets

- The contents of each environment: code, data, and the credentials that callers put in it.
- The control-plane credential of `ate-env-api`: a projected ServiceAccount token, audience
  `api.ate-system.svc`, that expires after `tokenExpiration` in `cmd/ate-env/manifest.go`.
- The caller token in the Secret that `ate-env manifest` mounts: `ate-env-api-token`, or the one
  that `--auth-token-secret` names. There is no token only with `--insecure-no-auth`.
- The session key of the OpenHands agent-server (`OH_SESSION_API_KEYS_0`), in the container
  environment of each template that `integrations/openhands` registers, and of each template that
  `ate-env-api` derives from one. `examples/openhands/demo.sh` and the README register it with
  `--sidecar-env`, thus the container carries it as `ATE_SIDECAR_ENV_OH_SESSION_API_KEYS_0`.
- ActorTemplates, golden snapshots and suspended environments in the snapshots bucket.
- Worker capacity: the CPU, memory and actors of the WorkerPools.

## Attackers

| Attacker | Capability |
|---|---|
| A network peer | Reaches the `ate-env-api` listener: a workload in the cluster, or anyone behind a port-forward or a LoadBalancer. |
| A token holder | Has the caller token. Has all the powers of the API. |
| Code in an environment | Runs as the guest user inside an actor: agent actions, model output, task code. Treat it as untrusted. |
| A web page | Runs in the browser of a developer who port-forwards the API to localhost, or of a person who opens a hostname-form URL of the port proxy. With the hostname form, it can be a page that code in another environment serves under the same domain. |
| A template author | Registers ActorTemplates with `kubectl-ate`. Trusted: chooses the image, its command, the sidecars and the resources. |
| A dependency or image | A compromised Go module, Python package, base image or GitHub Action. |

## Boundary 1: callers to ate-env-api

| Threat | Current mitigation | Gap |
|---|---|---|
| Unauthenticated use of the API | `-auth-token-file` makes the gRPC API, MCP and the port proxy require `Authorization: Bearer <token>`. The port proxy also accepts the token in `X-Env-Authorization` and does not forward it. A WebSocket upgrade that cannot carry a header can present a port capability (`ate-env-capability` in the URL user info): an HMAC of one environment port and an expiry of at most seven days, keyed with the token. The proxy accepts it only on a WebSocket handshake (`GET` with `Connection: Upgrade` and exactly one `Upgrade` field whose whole value is `websocket`, thus the daemon gets the same handshake) and only as the request's single `Authorization` credential, and never forwards it. The check uses a constant-time compare. The server reads the file again when it changes. A token file that it cannot read gives the caller only `internal error`. Without a token, `ate-env-api` logs a warning when it listens on a non-loopback address. The CLI, the Go and Python clients, the SWE-bench loader, the MCP example and the NeMo Gym provider can send the token. | `ate-env manifest` mounts the token by default, and `--insecure-no-auth` turns authentication off. The `ate-env-api` binary itself has no token unless `-auth-token-file` is set: a deployment that is not made with `ate-env manifest` must set it. A leaked port capability stays valid until its expiry, for WebSocket handshakes to its port. It binds the atespace, the environment id and the port, not one environment instance: an environment that the caller creates again with the same id accepts the old capability until it expires. Callers choose the id, thus they limit this with a short expiry and by not reusing an id while a capability for it is valid. Only a token rotation revokes it, and the rotation also revokes all other capabilities and needs each caller to get the new token. The proxy limits a capability to a WebSocket handshake, but the daemon decides what it does with that handshake: a server that routes only by path, such as a Go `net/http` handler, serves its `GET` handler to it. Thus a capability opens each `GET` endpoint of a daemon that does not route handshakes separately. A FastAPI or Starlette application on uvicorn serves a handshake only from its WebSocket routes. |
| Token theft on the network | The manifest Service sets no `type`, thus it is `ClusterIP`. | The listener has no TLS: the token and all data travel in clear text. The Go and Python clients can use TLS, but only to a proxy or load balancer that terminates TLS in front of `ate-env-api`. The manifest ships no NetworkPolicy. |
| One caller acts on the environments of another caller | None. | One shared token. No per-caller identity and no authorization: each token holder can use each environment. |
| Reach beyond the environments of ate-env | Lifecycle calls use the `ate-env-api` identity, which the access policy limits to its atespaces. The port proxy forwards only to the atespaces that `--port-proxy-atespaces` names (default `ate-env`) and answers 403 for the others. | Guest calls and MCP go to the router. The control plane is asked only while an environment wakes. Thus these paths reach any actor that the router serves, in any atespace, including actors that ate-env did not create. With `--port-proxy-atespaces='*'`, the port proxy does the same. |
| A web page drives the API through a port-forward (CSRF) | The port proxy path form and hostname form refuse a state-changing request or a WebSocket handshake from another origin (`--port-proxy-trusted-origins` adds origins). The header form needs custom headers, and MCP needs a JSON content type: a browser sends either one to another origin only after a CORS preflight, which the API never approves. | — |
| DNS rebinding: a page on an attacker's name resolves to the forwarded listener and becomes same-origin | `internal/hostcheck.Checker` wraps every HTTP route on the listener (`/healthz`, MCP, and both header and path forms of the port proxy) and returns `403 Forbidden` unless `r.Host` is an IP address, `localhost` or `*.localhost`, an in-cluster name of `--service-name` (`ate-env-api` by default), or a host listed in `--allowed-hosts` (`--api-allowed-hosts` in the manifest). The MCP handler sets `DisableLocalhostProtection: true` because `hostcheck` enforces this check in front of it. With `--port-proxy-host-suffix`, `Checker.WrapFor` lets a name under the suffix pass on the port proxy's route only, and only when `portproxy.(*Proxy).ServesHost` parses it as an environment port; `/healthz` and MCP keep `Wrap`, and `portproxy.Dispatch` sends no request under the suffix to them or to gRPC. A name under the suffix that is not a target gets the proxy's 400, which serves nothing. | `Host` validation is a browser defense: non-browser clients can set `Host` arbitrarily (caller authentication is `--auth-token-file`). The raw gRPC handler on `/` stays unwrapped so gRPC clients can send their dial target as `:authority` (guarded by bearer auth). The names under `--port-proxy-host-suffix` are safe from rebinding only while the operator controls that domain, as for a name in `--allowed-hosts`: under a domain of another party, that party can point a valid name at a forwarded listener, and with `--insecure-no-auth` its page then reads and drives that environment port. ate-env does not check who controls the domain; it refuses only a suffix of one label, such as `com`, and reads no list of public suffixes (`co.uk` passes). |
| The hostname form takes the target from `Host` (`--port-proxy-host-suffix`, off by default): a caller, or a front proxy, names an environment port by the name it sends | The form gives no reach that the other forms lack. The same caller check runs before anything is forwarded: the token, or a port capability on a WebSocket handshake for the atespace, id and port that the `Host` names. Then `--port-proxy-atespaces` applies, also to the default atespace of the short name, and port 80 is refused. The proxy reads `Host` only (`:authority` on HTTP/2), never `X-Forwarded-Host`. A name is a target only when it parses whole: `<port>-<id>` and at most one atespace label, each a DNS label, compared in ASCII, thus a rune that Unicode case folding maps to a letter does not pass. Each other name under the suffix gets 400. A request takes one form: under the suffix, `X-Env-Id`, `X-Env-Atespace` or `X-Env-Port` gets 400, the path is not read as the path form, and no request reaches `/healthz`, MCP or gRPC. The daemon gets no API credential and no `X-Env-*` header. | One shared token, as in the other forms: each token holder reaches each port in the allowed atespaces by its name. A front proxy that builds `Host` from caller input lets the caller choose the target; the caller check still applies. `ate-env manifest` renders no Ingress, DNS record or certificate: the wildcard records and the TLS in front are the operator's, and the listener itself stays plaintext. The API answers 400 on each name under the suffix: `ate-env-api` refuses at start a suffix that covers `localhost`, a Service name or a name in `--allowed-hosts`, but it cannot see a name that only a front proxy or a gRPC client uses, and such a name loses the API. |
| Environments under one domain (hostname form): a page or a daemon of one environment uses the browser of a person to act on another environment's port or on the API | Each environment port is an origin of its own, thus the same-origin policy keeps a page of one port from reading the answers of another, which the path form does not give (there all ports share the listener's origin). The proxy refuses a state-changing request or a WebSocket handshake whose origin is neither the request's own `Host` nor a trusted origin, also from another port under the suffix (`Sec-Fetch-Site: same-site`). The API authenticates with a bearer token or a port capability and reads no cookie, thus a browser has no credential that it sends by itself on a plain request. A capability that a browser keeps from a URL opens only a WebSocket handshake, which the origin check covers. No name under the suffix reaches an API route. | The ports are the same site. Code in one environment can set a cookie with `Domain=<suffix>`, or a parent of it that is not a public suffix, and the browser sends that cookie to each other environment port and to each other host under that registrable domain. A daemon that trusts a cookie can thus be given a session by another environment (cookie tossing, session fixation), and domain-wide cookies of the operator's other hosts reach the daemons. The proxy does not remove or rewrite `Cookie` or `Set-Cookie`, and `SameSite` does not separate the ports. The mitigation is the operator's: a registrable domain that serves nothing else, and daemons that check their own credential. A cross-origin `GET` passes the origin check, as in the path form: with `--insecure-no-auth` a page can send a `GET` to a port by its name. It cannot read the answer with `fetch` or XHR, but an answer that parses as a script can be read through a `<script>` element, and the size and the timing of an answer leak. |
| A URL with an environment id or atespace that an attacker chose, which another person opens (hostname form) | The URL carries no credential, and the proxy accepts none from a query. A browser that opens it sends no token and gets 401 before anything is forwarded. A client that adds the API token reaches the attacker's daemon, but the daemon does not get the token: the proxy removes `X-Env-Authorization`, and `Authorization` when it carried the token or a capability. The atespace in the name is subject to `--port-proxy-atespaces`. A capability names its atespace, id and port, thus it does not open the attacker's port. | With the token sent, the client reads content of the attacker's daemon under the operator's domain, with the cookie reach of the row above. A client that sends a daemon credential in `Authorization` next to the token in `X-Env-Authorization` gives that credential to the daemon that the name selects. With `--insecure-no-auth` the URL opens for each person who can reach the listener. A name has no secret part: an id can be guessed, and the token is the only control. |
| The port proxy reaches the guest port | The port proxy refuses port 80, where the guest listens. | — |
| Resource exhaustion | The port proxy bounds its idle tunnels with `idleTunnelsPerPort`, `idleTunnelsTotal` and `idleTunnelTimeout` in `internal/portproxy/portproxy.go`. A port-proxy request with `X-Env-Wake-Wait` holds its stream, one goroutine, one probe connection to the router and at most 64 KiB of its body while the environment wakes. The proxy holds it only after the caller check and the atespace check, for at most `--port-proxy-wake-wait-cap` (default 1 minute; `0` turns the hold off), and nothing reaches the environment's port before the wake. In `ate-env-api`, the awake-environment and guest-capability cache (`boundedAwakeCache` in `internal/apiservice/awake_cache.go`) caps entries at 65,536 with a 10m TTL. In `ate-env-guest`, the `StartProcess` idempotency table (`guest/process/idempotency.go`) caps `request_id` at 128 printable ASCII bytes and bounds retained entries to 16,384 (15m TTL), storing only the compact `process_id` string rather than process output or full proto payloads. Worker capacity bounds the running actors. | The number of held requests has no limit of its own, as the number of guest calls that wait for a wake (up to `--wake-budget`) has none. Over HTTP/1.x, a held request with a body above 64 KiB or a chunked body stays held until its wait ends after the caller leaves. A token holder can create environments without a quota or a rate limit; suspended environments keep their snapshots in the bucket. The MCP handler keeps one server for each environment, and the client keeps one guest connection for each environment. Neither is evicted while the process runs. |
| Control-plane detail in guest-call errors | A guest call to an environment that cannot start reports why: for one that the control plane moved to CRASHED while it was waking, the error carries Substrate's crash reason (`ActorStatus.crash.message`, at most 4096 characters), and with `--wake-no-capacity-grace` a call that no worker has room for carries the router's no-capacity text. Only a caller that may call that environment, thus one that has the token, gets it. The `ate-env-start-failure` trailer carries only `crashed` or `no-capacity`. | Some crash reasons include an atelet error verbatim, such as the error of a failed restore, and a message can name a node or a snapshot. A caller sees that detail for its own environments; it does not reveal other environments or credentials. |

## Boundary 2: ate-env-api to the Substrate control plane

| Threat | Current mitigation | Gap |
|---|---|---|
| Theft of the control-plane credential | A projected token with audience `api.ate-system.svc` and the lifetime that `tokenExpiration` sets. The ServiceAccount mounts no Kubernetes API token. | `-skip-verify` defaults to `true`, and the manifest does not set it. Thus `ate-env-api` does not verify the TLS certificate of the control plane. A peer that intercepts the connection can read the token. |
| The environment store widens what `ate-env-api` can do in Kubernetes | Off by default: without `--environment-store=configmaps`, `ate-env-api` has no Kubernetes API token in its pod and makes no call to the Kubernetes API. `ate-env manifest --environment-metadata` turns it on with a Role in the namespace of `ate-env-api` only, no cluster-wide permission, and the token. The store writes one ConfigMap for each environment that has a label or a deadline, named `ate-env.<atespace>.<id>`. It deletes an object only as it read it, with the uid and the resource version of that read as the precondition, and its informer holds only objects with the store's label. A write deletes nothing: for a name that is taken it returns the object, and the write over it, which the create path decides, removes that object only, with or without the label. | The Role covers each ConfigMap of the namespace, not only the store's: Kubernetes RBAC restricts `create`, `list` and `watch` by kind and not by name. The Role grants `list`, `watch`, `create`, `update` and `delete`, the calls the store makes, and no `get`. Thus `ate-env-api`, and whoever takes it over, can read, change and delete each ConfigMap in that namespace and create one with a name of its choice; also `kube-root-ca.crt`, which Kubernetes puts in each namespace. Keep other ConfigMaps out of it. Whoever can read ConfigMaps there reads the labels and the deadlines of each atespace. |
| Excess privilege | A dedicated ServiceAccount. The access policy grants `editor` on each atespace that an operator adds. Creating an atespace takes a global owner, which `ate-env-api` is not. | `editor` includes creating ActorTemplates. Refer to on-demand rootfs images. |

## Boundary 3: the router and the actors

| Threat | Current mitigation | Gap |
|---|---|---|
| Use of a guest without the API | The router runs in `ate-system`; its TLS listeners (the only ones Substrate v0.6.0+ serves) require a client certificate (`spiffe://cluster.local/ns/<namespace>/sa/ate-env-api`) whose SPIFFE ID the operator adds to the router's `--ingress-allowed-spiffe-ids` (see README). `ate-env manifest` mounts the certificate by default (`--api-router-tls`), and the guest listens only inside the actor on port 80. | Without `-atenet-client-cert` (`--api-router-tls=false`), `ate-env-api` dials plaintext ports that only test fakes serve (calls against a Substrate v0.6.0+ router fail). Inside the cluster between the router and the actor, traffic on port 80 is unauthenticated HTTP/h2c and the guest does not authenticate callers: any workload permitted to reach the router (or an actor's network directly) can run commands as the guest user. |
| Code in an environment attacks the node or other environments | Each actor runs in gVisor, the sandbox class of the WorkerPool. | ate-env sets no actor egress policy. Egress follows the Substrate defaults. |
| Ambiguous transport failures cause duplicate command execution under `-reactive-wake` | When `-reactive-wake` (`ATE_ENV_REACTIVE_WAKE`, off by default) is enabled on `ate-env-api`, fast-path `codes.Unavailable` responses evict the `boundedAwakeCache` entry and only replay operations that are idempotent (`GetProcess`, `ResizeProcess`, `ReadFile`, `StreamProcessOutput` before the first message) or deduplicated by `StartProcessRequest.request_id`. `StartProcess` attaches a UUID or preserves the caller's `request_id` (deduplicated in `ate-env-guest` over deterministic protobuf SHA-256 request fingerprints, rejecting parameter mismatches with `InvalidArgument`) only after observing the `x-ate-env-guest-caps: start-request-id` response trailer from that guest. Calls without replay deduplication (`SignalProcess`, or `StartProcess` against an older guest without `start-request-id`) clear any `request_id` and return `Unavailable` without replaying. | On an environment suspended outside this `ate-env-api` replica (for example by another replica or idle timeout), the first `SignalProcess` or `StartProcess` to an older guest returns `Unavailable` while evicting the stale cache entry. Cached guest capabilities in `ate-env-api` can also outlive an environment delete-and-recreate performed through a different `ate-env-api` replica for up to the 10m cache TTL: a `StartProcess` to an older guest in the new environment can then be sent again, and the command can run two times. |
| Processes escape supervision | When the guest is PID 1, it runs as an init: it reaps orphans and forwards signals. A process timeout kills the process group. The guest refuses PTY window sizes that do not fit the 16-bit kernel fields. | — |
| A sidecar gains extra privilege | Only a template author sets sidecars (`--sidecar`, `--sidecar-readyz`, `--sidecar-env`), and that author already controls the image and its command. Sidecars run inside the CPU and memory limits of the actor. | — |
| A template widens the privileges of the code in an environment | An actor's container has Substrate's default capabilities (`AUDIT_WRITE`, `KILL`, `NET_BIND_SERVICE`). `ate-env manifest template --add-capability NAME` adds one to the template, and adds none by default. Substrate accepts only `AUDIT_WRITE`, `CHOWN`, `DAC_OVERRIDE`, `FOWNER`, `FSETID`, `KILL`, `NET_BIND_SERVICE`, `SETFCAP`, `SETGID`, `SETPCAP`, `SETUID` and `SYS_CHROOT`, and rejects a template with another name. The flag refuses another name with the same list (`addCapabilityNames` in `cmd/ate-env/manifest.go`, a copy of the Substrate list at the pin in `go.mod`). gVisor implements them in the sandbox kernel of the actor: they are not capabilities on the node. Only a template author sets the flag. | The capabilities are those of the container, not of one sidecar: when the container runs as root, the guest, each sidecar and each command that a caller starts hold them. Substrate runs the container as the image's `USER` and sets no ambient capabilities: a process of a non-root `USER` starts with no effective capability (`CapEff` is 0), thus the flag does not help a daemon there, while the added capabilities stay in the bounding set (`CapBnd`), which is the limit of what a setuid-root program in the image can gain. A derived template (an environment created with `rootfs_image`) keeps them, thus they also apply to an image that the caller chooses. |
| A command reads a credential of a sidecar | `ate-env manifest template --sidecar-env NAME=VALUE` sets `ATE_SIDECAR_ENV_NAME` on the container. At start, before it starts anything, the guest removes each such variable from its own environment and adds `NAME` to the environment of its sidecars only (`internal/sidecarenv`, `cmd/ate-env-guest/main.go`). The process service builds the environment of each command from the guest's environment (`guest/process/tracker.go`), thus the commands that the guest starts through `ate-env-api` and their children do not inherit the variable. A suspend checkpoints the memory of the guest, thus a resumed guest keeps the environment without the variable, and a sidecar that it restarts gets the variable again. The template also carries the guest flag `-sidecar-env-required`. A guest older than `--sidecar-env` does not know the flag and exits at flag parse, thus the actor crashes at start and does not run the sidecars without the variable. A new guest exits before it listens or starts a sidecar when the flag is set and no prefixed variable exists. | A `--env` variable reaches each command: use `--sidecar-env` for a credential. The sidecars run in the same container as the code in the environment, and as the same user, because the guest starts both as the container's user. At the Substrate pin in `go.mod`, which runs containers as the image's `USER` (since Substrate `5d226a50`), the guest, the sidecars and each command run as that one user, and a process reads `/proc/<pid>/environ` of another process of the same user. On a Substrate from before `5d226a50`, that user is root, and root reads each `/proc/<pid>/environ`. Thus that code can read a `--sidecar-env` variable from `/proc/<pid>/environ` of a sidecar, which gets it through its environment, or of the guest, whose `/proc/<pid>/environ` keeps the environment it was started with. When the guest is PID 1, the same holds for `/proc/1/environ`. Each caller of `ate-env-api` (a token holder, or each network peer when authentication is off) can also read `/proc/1/environ` or `/proc/<pid>/environ` with `ReadFile`, without a command; this was also true before `--sidecar-env`. The guest keeps the values in its memory to give them to a restarted sidecar, and the sidecar holds them too, thus each snapshot of the environment holds them. Follow-up options: `prctl(PR_SET_DUMPABLE, 0)` in the guest after it takes the variables, and in a small launcher for the sidecar, can block same-user reads of `environ` and `mem` (not verified on gVisor; a process with `CAP_SYS_PTRACE` still reads them). The real boundary is a separate user for the sidecar. Refer to the OpenHands agent-server. |

## The OpenHands agent-server

`integrations/openhands` runs the OpenHands agent-server as a sidecar from a runtime layer. The
base template sets its session key as a sidecar-only variable (`ate-env manifest template
--sidecar-env OH_SESSION_API_KEYS_0=<key>`). The workspace reaches the agent-server through the port
proxy. A base template registered before `--sidecar-env` has the key in `--env`, and so has each
template derived from it; the rows below hold for those only after they are registered again.

| Threat | Current mitigation | Gap |
|---|---|---|
| A caller reads the session key from a template | Reading a template needs viewer access to the atespace. Substrate logs redact the values of environment variables. Use a dedicated atespace for these environments, as `integrations/openhands/README.md` says. | Each caller with viewer access to the atespace reads the key with `GetActorTemplate`: reads do not redact it. Each template that `ate-env-api` derives from the base copies it. A template cannot refer to a secret. |
| A holder of the session key drives the agent-server | With `-auth-token-file`, the port proxy also asks for the caller token. Thus the key alone does not pass the port proxy. | The agent-server checks only the session key (`check_session_api_key` in `openhands/agent_server/dependencies.py`). Without `-auth-token-file`, each network peer of `ate-env-api` that has the key controls the agent-server of each environment whose id it knows. |
| A secret lookup carries the caller token into the sandbox | `AteEnvWorkspace.get_secrets()` gives `LookupSecret` URLs on `127.0.0.1:<server_port>` that carry the session key only, and the agent-server answers them in process. Thus a secret lookup does not go through the port proxy and does not need the caller token. | — |
| Code in one environment drives the agent-server of another environment | Each actor runs in gVisor. The guest gives the `--sidecar-env` key to the agent-server only, thus the commands that the guest starts through `ate-env-api` do not inherit it: `ate-env exec`, the Go and Python clients, the fleet's `handle.exec`, and the run and copy of the R2E-Gym adapter. The agent-server removes the key from the environment of the commands that it starts: the bash tool, the commands of the CodeAct agent, and the terminal steps of `run_trajectory.py`. `examples/openhands/demo.sh` checks with `ate-env shell` that a command does not see the key. With `-auth-token-file`, the key alone does not pass the port proxy. | All environments of one base template share one key. All these commands run in the same container as the agent-server and as the same user, thus they can read the key from `/proc/<pid>/environ` of the agent-server or of the guest. This read is not verified on a cluster. On a base template registered with the key in `--env`, each command that the guest starts gets the key with `env`. In both cases, the code needs a route to `ate-env-api` to reach the agent-server of another environment. With that route, it needs nothing more when authentication is off, and a credential that the port proxy accepts when authentication is on. |
| The workspace exposes the caller token to the agent-server or a log | With `api_token`, REST requests carry the token in `X-Env-Authorization`, which the port proxy removes before it forwards them. The event WebSocket carries only a port capability for the agent-server port of its environment, never the token; an ate-env-api with port capability support does not forward it. The client default for `ws_capability_ttl` is one day. `repr`, the model dumps, error messages and the workspace's loggers mask the capability. The agent-server is a FastAPI application on uvicorn, thus a capability opens only its WebSocket routes. | A leaked capability opens WebSocket handshakes to that port until it expires, at most seven days less five minutes after the workspace made it. |

## Snapshots and the snapshots bucket

A snapshot holds the memory and the files of an environment, and thus the credentials that callers
put in it. Golden snapshots of templates and the snapshots of suspended environments go to the
cluster's snapshot location (ate-api-server's `--snapshot-location`), or to the bucket that
`ate-env manifest template --snapshots-bucket` names on a cluster with Substrate's
`PerTemplateSnapshotLocation` preview gate.

What ate-env controls: with `--snapshots-bucket`, `ate-env manifest template` writes the bucket URL
into the template's `snapshotConfig.storageLocation`; templates that `ate-env-api` derives keep it,
and their create opts in to the preview feature. `ate-env-api` does not read or write the bucket
itself.

What ate-env does not control: Substrate writes and reads the snapshots. The operator owns the
access control, the encryption and the retention of the bucket. What stays in the bucket after an
environment or a template is deleted follows Substrate.

A local suspend (`SuspendEnvironment` with `mode: SUSPEND_MODE_LOCAL`, `ate-env suspend --mode
local`) returns before the upload: the snapshot sits on the worker's node, under Substrate's state
directory, until atelet's background upload lands. That is the same data the bucket holds, for a
shorter time, on the host that already ran the environment's memory; ate-env adds no reader to it,
and the node's own access control applies. What a local suspend does trade is availability: a node
lost before the upload lands takes the snapshot, and the environment, with it.

Operator requirements:

- Grant access to the bucket only to the Substrate identities that write and read snapshots. Block
  public access.
- Use encryption with a customer-managed key if your data requires it.
- Set a lifecycle rule that matches your retention policy.
- Use a separate bucket or prefix for each cluster.

| Threat | Current mitigation | Gap |
|---|---|---|
| A reader of the bucket gets the memory, files and credentials of environments | The operator's bucket settings. | ate-env does not check the access control, the encryption or the retention of the bucket. |
| A reader of a worker node gets the snapshot of a locally suspended environment before its upload lands | The snapshot sits where the running environment's memory sat; the node's own access control. A durable suspend uploads before it returns, but atelet keeps a local copy of the snapshot on the node until the actor detaches, thus both modes leave snapshot files on the worker node. | A node lost before the upload lands loses the environment (availability, not confidentiality). |

## On-demand rootfs images

`CreateEnvironment` with `rootfs_image` makes `ate-env-api` derive an ActorTemplate named
`<base>-<first 12 hex digits of the digest>` from the base template. The image replaces the
container, and the guest is mounted into it as a read-only image volume. `CreateEnvironment` also
accepts optional `worker_selector` labels (`Actor.worker_selector`), and `GetEnvironment` reports
both `node` and `worker_selector`.

| Threat | Current mitigation | Gap |
|---|---|---|
| A caller runs an image of its choice under the settings of the base template (worker selector, snapshots bucket, resources) | The image must be pinned by digest. If `<base>-<12 hex>` already exists, `ate-env-api` reuses it when its container image carries the same SHA-256 digest (even under another repository path, keeping the existing template's reference) and refuses a different digest with `FailedPrecondition`, including when a concurrent create made it. | Any registry is accepted. |
| A caller steers environments across worker pools (`worker_selector`) or inspects placement (`node`) | Substrate ANDs `CreateEnvironmentRequest.worker_selector` with the base template's `WorkerSelector`, so per-environment selectors narrow the allowed worker pools and persist across wakes. | Any token holder can restrict its environments to a subset of the base template's pools and read the hosting Kubernetes node name from `Environment.node`. |
| Template growth | None. | Derived templates have no quota and no garbage collection. |

Accepted risk for now: this hardening is not needed for RL at the moment. Revisit it before the
API serves callers other than trusted RL jobs.

## Listing environments

`ListEnvironments` returns the environments of one atespace: id, template, status, node, worker
selector and create time, the fields `GetEnvironment` reports plus `Environment.create_time`.
`ate-env-api` reads them with Substrate's `ListActors` on that atespace under its own identity.
The `editor` role of the access policy already allows that call, thus the RPC adds no privilege.

| Threat | Current mitigation | Gap |
|---|---|---|
| A caller enumerates the environments of other callers | The token (Boundary 1). The list reads one atespace for each call, with the access that the policy gives `ate-env-api`: the `editor` role on the atespaces an operator adds. | The RPC has no list of allowed atespaces. With the global viewer binding (`ate-env manifest access-policy --global`, which `ListPlacementDomains` needs), `ate-env-api` can list the actors of each atespace of the control plane, also ones that ate-env did not create, thus a token holder can too. Each token holder reads the id, template, status, node and create time of every environment in every atespace that `ate-env-api` can read. Before, a caller had to know an id to read these with `GetEnvironment`. With an id, a token holder can already run commands in that environment (Boundary 1, one shared token). Tracked in #403; no mitigation is decided. |
| Large responses | One call asks Substrate for at most 1000 actors (`MaxListPageSize`), 100 by default, and holds one page in memory. Substrate can return a few more than the page size. | No rate limit, as for the other RPCs. |

## A time to live on an environment

With the environment store on (`--environment-store=configmaps`, off by default),
`CreateEnvironmentRequest.time_to_live` and `SetEnvironmentTimeToLive` give an environment a
deadline, kept in the ConfigMap of the environment. Each `ate-env-api` replica sweeps every 30 s
and deletes the environments whose deadline is past, in any status, with Substrate's `DeleteActor`
under its own identity: the `editor` role that a caller's `DeleteEnvironment` already uses. The
feature adds one RPC, one request field and a delete on a timer. It adds no privilege in Substrate
and one verb, `update`, to the Role of the store. Without the store, both are refused with
`FailedPrecondition`.

| Threat | Current mitigation | Gap |
|---|---|---|
| A caller shortens the life of another caller's environment | The token (Boundary 1). `SetEnvironmentTimeToLive` names an environment by atespace and id, as `DeleteEnvironment` does, and gives no reach that a delete lacks: a token holder who can set a deadline of one second can already delete the environment. | One shared token and no list of allowed atespaces for this RPC: each token holder can set, move or remove the deadline of each environment in each atespace where `ate-env-api` is `editor`, also of environments that another caller created, and can remove a deadline that an operator relies on to bound cost. No audit record names the caller. |
| The sweep deletes an environment that must stay | The sweep takes candidates from its informer, then reads the object from the Kubernetes API server and deletes only when that copy is past its deadline too, thus a late informer after a `SetEnvironmentTimeToLive` on another replica deletes nothing. The delete carries the uid of the actor the deadline was written for (`DeleteOptions.uid`): Substrate refuses it for an environment that was created again with the same id. A read of the API server that fails deletes nothing. The sweep deletes only environments whose object holds a deadline; an environment created with no time to live has no object. | The check and the delete are two calls: a `SetEnvironmentTimeToLive` that lands between them, at the deadline, is answered with success and the environment is deleted all the same. Whoever can write ConfigMaps in the namespace of `ate-env-api` can write a past deadline for each environment whose actor uid they know, or remove a deadline. |
| A clock error | The deadline of a create counts from the create time that Substrate gave the actor; the deadline of `SetEnvironmentTimeToLive` counts from the clock of the replica that serves the call. The sweep compares a deadline with the clock of the replica that sweeps. | No guard against a wrong clock: a replica whose clock is ahead deletes environments early by that error, and one that is behind deletes them late. A difference between the clocks of Substrate and of `ate-env-api` shifts each deadline of a create by that difference. The read from the API server does not help: it reads the same deadline. |
| A deadline is not enforced | Each replica sweeps, with no leader. A delete that fails for a reason other than a refusal (a control plane that is down, busy or fails itself, a credential it does not take, a sweep that ran out of time) is tried again on the next sweep without limit, with a log line for each sweep. A delete that Substrate refuses with `FailedPrecondition` 5 times, at least 15 s apart, stops: the object gets the label `ate-env.dev/deadline=failed`, and one log line and `ate_env_api_deadline_delete_failed_total` report the environment. | An environment whose delete is refused for good is not deleted (Substrate can leave it in `DELETING`) and keeps its resources until an operator acts on the report. A replica that stops between the write of the mark and the report reports nothing: the label is then the only record. A backlog of more deletes than one sweep reaches (16 at a time for 30 s) is deleted late. A crash of `ate-env-api` between the create of the actor and the write of its object leaves an environment with no deadline, and its caller gets an error for a create that left an environment. With the store turned off, no deadline is enforced. With `ate-env-api` down, no sweep runs. An environment that was created before the store was on has no deadline. |
| Load from deadlines | One sweep deletes at most 16 environments at a time and ends after 30 s; the rest waits for the next sweep. The store's Kubernetes client has its own rate limit (`--environment-store-qps`, `--environment-store-burst`). A create or a `SetEnvironmentTimeToLive` writes one ConfigMap of a few hundred bytes. | No quota on environments with a deadline, as on environments. Each such environment is one ConfigMap in the namespace of `ate-env-api` and one object in the informer of each replica. |

## Client tracing

The Python client emits OpenTelemetry spans when `TracerConfig(enable_tracing=True)` is set. The
spans go to the application's `TracerProvider`. Without one, the client builds an OTLP exporter
only when `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is set; otherwise
the spans go nowhere.

| Item | Current state | Gap |
|---|---|---|
| Data in spans | Span names and attributes: environment, atespace and template names, the image reference without its tag, the run id, the executable name (without arguments and without leading `NAME=value` assignments), the exit code, a first-command marker, and file paths and sizes. No command arguments, no file content and no token. | The trace backend learns environment names and file paths. |
| Data on the wire | With tracing on, each gRPC call on a channel that the client creates carries the W3C `traceparent` and `tracestate`, never baggage. Without `--enable-tracing`, `ate-env-api` ignores them; with it, see [Server telemetry](#server-telemetry). | — |
| The export connection | The `OTEL_EXPORTER_OTLP_*` environment variables configure the endpoint, its TLS and its credentials. | An endpoint without TLS sends the spans in clear text. |

## Server telemetry

`ate-env-api` serves metrics only with `--metrics-listen-addr` and traces only with
`--enable-tracing`. `ate-env manifest` sets them with `--metrics-port` and `--enable-tracing`.

| Item | Current state | Gap |
|---|---|---|
| The metrics listener | Off by default. When on, it serves `GET /metrics` on its own listener, with a 5 s header read timeout. It serves request counts and latencies by gRPC method and status code, `create_to_ready` and wake-wait latencies by atespace and outcome, atespace names with environment counts by state, and the build version and commit. Labels never carry environment ids, actor ids, run ids, paths, image references or tokens. Environment counts come from the control plane at most once per `--metrics-environments-ttl` (default 1m). | The listener has no authentication and no `Host` check. Anyone who reaches it (with the manifest, any pod that reaches the pod IP or the Service port) reads the atespace names and the counts. |
| Incoming trace context | With `--enable-tracing`, the otelgrpc server handler continues the W3C `traceparent` and `tracestate` that a caller sends, and the server passes them on to ateapi and the guests as is. Only W3C Trace Context is propagated, never baggage. | The caller is not trusted: it chooses the trace id and the sampled flag, thus it can join the server's spans to its own trace or ask every hop to sample. |
| Data in server spans | With `--enable-tracing`, each server span carries otelgrpc's `rpc.*`, `server.address` and `server.port` attributes, and the identity its call targets under ateapi's keys: `ate.atespace` and `ate.actor.name` (the environment id, from the request or, for guest calls, from `x-env-id` and `x-env-atespace`), with `ate.actor.uid`, `ate.actor.version`, `ate.template.name` and `ate.template.atespace` when the call has them. A name is stamped only when Substrate would accept it (a lowercase DNS label of at most 63 characters), so a caller cannot put a large value in an `ate.*` attribute. No attribute carries a caller identity (callers share one token, see [Known gaps](#known-gaps)), a token, other metadata, a command or a file path. | The trace backend learns which environment and template each call touched, as it does from the clients' and ateapi's spans. The status of a span whose call failed with `Unknown`, `DeadlineExceeded`, `Unimplemented`, `Internal`, `Unavailable` or `DataLoss` carries the gRPC error message (otelgrpc v0.72.0). That message can carry caller-supplied text, such as the `x-env-id` the router's "no actor" answer echoes (a 10,000-character id left a 1,171-character message), or a file path the guest could not read. |
| `x-ate-env-requested-at` | A caller may send this gRPC metadata on `CreateEnvironment` to start `create_to_ready` at its own clock. A value in the future or not in RFC 3339 is ignored. | A caller can send a time in the past and lengthen its own `create_to_ready` samples. |
| The export connection | The `OTEL_EXPORTER_OTLP_*` environment variables configure the endpoint, its TLS and its credentials. Control-plane reads for the environments gauge are not traced. | An endpoint without TLS (`--otlp-insecure`) sends the spans in clear text. |

## Fleet inventory reporter

`ate-env-reporter` is a separate binary, one per cluster. `ate-env manifest reporter` prints its
namespace, ServiceAccount, ClusterRole, binding and Deployment, and with `--access-policy` the
global `viewer` binding that an operator merges into the control plane's global policy.

| Item | Current state | Gap |
|---|---|---|
| Read access | It lists actors, workers, atespaces, templates and tags of all atespaces, which takes the global `viewer` role in the Substrate access policy, and it lists WorkerPools and Deployments of all namespaces; the ClusterRole grants `list` on these two kinds and nothing else. The pod mounts its Kubernetes API token, and a projected token with audience `api.ate-system.svc` for the control plane. It calls no mutating RPC and writes no Kubernetes object. Its one write is the snapshot object in the bucket `-bucket` names, with the application default credentials, or a file under `-out-dir`. | The control-plane connection is the same as in boundary 2: `-skip-verify` defaults to `true`. |
| Snapshot content | Names and states: atespaces, actors with their uid, template, node, worker and crash message, templates with their image references, tags, workers with their labels and capacity, WorkerPools, and the image and flags-derived state of each `ate-env-api` Deployment. It carries no environment variable, no token and no file content. A label or selector key that contains `token`, `password`, `authorization`, `secret` or `credential` is left out and logged once, because the schema refuses it. | Label values and crash messages go in as they are. A reader of the bucket sees the actor names of all atespaces; there is no filter per atespace. |
| The health listener | `-health-listen-addr` (default `:9091`) serves `GET /healthz` and `GET /readyz`, with a 5 s header read timeout. The responses carry no cluster data. | The listener has no authentication. |
| The metrics listener | Off by default. `-metrics-listen-addr` (`ate-env manifest reporter --metrics-port`) serves `GET /metrics` on its own listener, with a 5 s header read timeout. It serves tick durations by `status` (`ok`, `error`) and no cluster data. `--pod-monitoring` has Managed Service for Prometheus scrape it inside the cluster every 5 s. | The listener has no authentication and no `Host` check. Anyone who reaches the pod IP reads how long ticks take and how many fail. |
| Traces | Off by default. With `-enable-tracing` the reporter exports one span per tick, with the `ateapi` calls under it, over OTLP as configured by the `OTEL_EXPORTER_OTLP_*` environment variables. A failed tick's span carries its error message; the `ateapi` spans carry the RPC name, the status code and the server address. No span carries a token. Each tick is a sampled root trace, and the `traceparent` goes to `ateapi`. | An endpoint without TLS (`--otlp-insecure`) sends the spans in clear text. A trace every 5 s per cluster, with `ateapi`'s server spans when it traces, adds trace volume unless `OTEL_TRACES_SAMPLER` thins it. |

## Fleet telemetry collector

`ate-env manifest collector` prints one OpenTelemetry collector per cluster, and
`ate-env manifest --pod-monitoring` and `ate-env manifest reporter --pod-monitoring` a
`PodMonitoring` for the metrics port of `ate-env-api` and of the reporter. All are optional and off
unless an operator applies them.

| Item | Current state | Gap |
|---|---|---|
| Ingress | The Service exposes OTLP on 4317 (gRPC) and 4318 (HTTP), cluster-internal. The pod runs as a non-root user with a read-only root filesystem, no capabilities and no Kubernetes API token. | The receivers have no authentication: any pod that reaches the Service can write spans into Cloud Trace and the bucket under the cluster's project, and fill the bucket. No NetworkPolicy limits the senders. |
| Egress and identity | It writes to `telemetry.googleapis.com` and to one bucket, as the Google service account `--gcp-service-account` binds through Workload Identity: `roles/cloudtrace.agent` in the cluster project, and on the bucket `roles/storage.objectCreator` plus `storage.buckets.get`. | The bucket grants are not limited to `v1/traces/`; the account can create objects under any prefix it does not overwrite. The documented `roles/storage.legacyBucketReader` also grants `storage.objects.list` on the whole bucket, so the account can list every object name; a custom role with `storage.buckets.get` alone avoids that. One Google service account serves every cluster, so it holds `roles/cloudtrace.agent` in each cluster project. |
| Span content | Spans as their senders made them: names and attributes as in [Client tracing](#client-tracing) and [Server telemetry](#server-telemetry). The collector sets `gcp.project_id` to its own `--project` on every span, replacing a value the sender set, so a sender cannot direct spans into another cluster's project; it changes nothing else. | A reader of the bucket's `v1/traces/` sees every sender's span attributes, across atespaces. |
| Metrics scrape | Each `PodMonitoring` has Managed Service for Prometheus scrape `/metrics` on the `metrics` port inside the cluster every 5 s. | The listeners stay unauthenticated, as in [Server telemetry](#server-telemetry) and [Fleet inventory reporter](#fleet-inventory-reporter). |

## Fleet site

`ate-env-fleet-site` is a read-only web server for Cloud Run behind Identity-Aware Proxy (IAP). It
serves the Fleet and Cluster pages and a JSON API over the reporters' snapshots, and one actor's
lifecycle events from a BigQuery view. It holds no cluster credential and no `ateapi` address.

| Item | Current state | Gap |
|---|---|---|
| Authentication | Every route but `GET /healthz` requires a valid `X-Goog-Iap-Jwt-Assertion`: `ES256` only (checked before the library sees the token, because `idtoken` also accepts `RS256` tokens that any Google service account can mint), signed by a key from IAP's key set, `aud` equal to `-iap-audience`, `iss` `https://cloud.google.com/iap`, unexpired, issued in the past, with an `email` claim. Anything else answers 403. The site does not start without `-iap-audience` unless `-insecure-no-iap` is set, and that flag requires a loopback `-listen`. | Authorization is IAP's group binding: every member sees every cluster and atespace (tenancy rule, open in the plan). |
| Methods and rate | `GET` and `HEAD` only; anything else answers 405. Each user gets 10 requests a second, burst 40, per instance, and on the events route, one BigQuery job each, 1 a second, burst 5; more answers 429. The log quotes request paths, so a path cannot forge a log line. | The limit is per Cloud Run instance, not global. |
| Browser hardening | `Content-Security-Policy: default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'` (the pages have no script), `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `Cache-Control: no-store`, HSTS. Pages are `html/template`, which escapes snapshot text such as crash messages and image references. | — |
| Inputs | Path segments must be one segment each and name a listed snapshot; an actor uid matches `[A-Za-z0-9][A-Za-z0-9._-]{0,127}`; `since` is RFC 3339 within 30 days. The events query is parameterized; the view name is checked at start. A query may bill 10 GiB at most and returns the newest 1000 rows; the events table is partitioned by `publish_time`, which the query bounds, so it scans only the days asked for. A snapshot read is capped at 128 MiB. Errors to the user are generic; the detail goes to the log. | The pages show the user's own email from the assertion (FS-7). |
| Identity and data | The service account needs `roles/storage.objectViewer` on the bucket, `roles/bigquery.dataViewer` on the events dataset and `roles/bigquery.jobUser` in its project; the site calls no write API. A snapshot whose content names another cluster than its object path is shown as an error. A member of the IAP group sees, for every cluster: project, cluster and node names, atespace, template and tag names, actor names and uids, image references, WorkerPool and worker details, and crash messages. | Read-only rests on IAM: a broader role on the service account would not be refused by the code. |
| Live, Actor and metrics routes | The Live page and `/api/v1/live` run one fixed query (last hour, 200 rows) at a time, its result or its error cached 10 s for every viewer of an instance; the page reloads itself with a `meta` refresh, not a script. The Actor page shares the events route's limit. `/api/v1/metrics/{name}` runs only the six named PromQL queries; the `project` filter must be a project id and the `cluster` and namespace filters Kubernetes or GKE names before they become label matchers, so a request cannot change the query. Results are cached 10 s, concurrent requests for one query share one upstream call, and each user gets 2 metrics requests a second, burst 10. Trace links go to the Cloud Trace console with `rel="noopener noreferrer"`, made only from a well-formed project and trace id. The service account needs `roles/monitoring.viewer` on the metrics scope for this route. | A metrics response returns every series of a named query across the clusters of the metrics scope, to every member of the IAP group. |
| Runs routes | `/runs` lists the run folders of `v1/runs/` in the inventory bucket and the bundle folders of the `FLEET_BENCH_BUCKETS` buckets whose names pass the bundle check, the first 500 of each by name; a source that cannot be listed is an error row. A run's URL names its source, and a run page reads only that folder. A run file is served only when the run's listing names it, its name one path segment, always as a download (`Content-Disposition: attachment`), `report.html` too, so no script from a run folder runs on the site's origin or in a tab the site opened. Bundle JSON is shown up to 256 KiB, escaped; a run page's reads (RunReports up to 1 MiB each, `spans.jsonl` up to 64 MiB) are cached per change of the folder and at most two run pages are built at a time. With `-dir`, symbolic links that leave the directory are refused, a benchmark bucket's own directory included. The service account needs `roles/storage.objectViewer` on each benchmark bucket. | Whoever can write `v1/runs/` or a benchmark bucket chooses what members download and what the run pages show; run folders carry no tenant filter. |

## Harbor integration

`integrations/harbor` is a Harbor environment provider: `harbor run` creates one ate-env
environment per trial, runs the agent's and the verifier's commands through the guest, and moves
files through the file API. It runs in the operator's Harbor process, next to the agents' model
keys, as a client of the existing gRPC listener; nothing new listens and `ate-env-api` gains no
privilege.

| Item | Current state | Gap |
|---|---|---|
| Caller token | Read from `SUBSTRATE_ENV_AUTH_TOKEN` or the `api_token` kwarg and sent as gRPC metadata. The provider does not log it and does not write it to `trial_dir/ate_env.json`. | Harbor writes the environment kwargs into each trial's `result.json`, thus a token passed with `--ek api_token=` lands on disk with the results. The provider warns when it sees the kwarg; the README says to set the variable. |
| Registry credentials | `build=local` uses the operator's docker login, `build=cloud-build` the operator's gcloud account; the provider reads neither. Images are pushed as `<registry>/<task>:<environment_id>`. | The worker nodes need read access to that registry; granting it is an operator step outside this repository. |
| Task images | A dataset's task images run as actors, pinned by digest, the way any rootfs image does; the provider does not inspect them, and the actor's isolation is the boundary. | Egress follows the Substrate defaults (known gap 8), and most tasks and installed agents download packages at run time. |
| Commands as root | Harbor's verifier removes its reward files and `chmod`s its script as `root`. On an image that runs as root that is the image's own user, and the command runs as is. On another image the provider uses `sudo -n`, or `su` from root, only when the image offers it; otherwise the call fails and says why. | — |
| Dependencies | `harbor`, pinned to one minor, and `dockerfile-parse`, in `integrations/harbor` only; neither is a dependency of the client, the CLI or `ate-env-api`. | Dependabot's `uv` entry proposes updates through `uv.lock` inside `harbor`'s `<0.26` pin; `dockerfile-parse` has no upper bound. A minor bump of `harbor` is a manual step with the trial test as the gate. |

## Compatibility of the API

A protobuf message with fields that the server does not know is valid proto3: the server keeps the
unknown fields and ignores them. Thus a client from another version or fork of this API, which uses
the same `ateenv.v1alpha` names, can get a behavior other than the one it asked for, without an
error. This is a compatibility risk, not a boundary of this threat model.

A new client that talks to an older guest gets responses without the fields that guest predates. A
field whose absence matters is declared `optional`: `Process.timed_out` is absent from an older
guest, and the clients then fall back to the exit-137 heuristic for timeouts; a newer guest always
sets it.

## Supply chain and dependencies

| Item | Current state | Gap |
|---|---|---|
| Images | A template mounts the guest into a rootfs image only when the guest is pinned by digest. `.ko.yaml` pins the guest base image by digest. The README asks for the worker image by digest. `ate-env manifest collector` defaults to the OpenTelemetry Collector contrib image pinned by digest. | — |
| Releases | `.github/workflows/release.yaml` signs container images and CLI release blobs/checksums with Cosign keyless signing (GitHub Actions OIDC) and optionally publishes the Python client (`ate-env-rl`) to PyPI when `vars.PYPI_PUBLISH_ENABLED == 'true'` (or via manual tag `workflow_dispatch` with `publish_pypi=true`) using OIDC Trusted Publishing (`pypa/gh-action-pypi-publish`, `environment: pypi`, `refs/tags/<tag>` checkout, and PEP 440 tag/wheel version verification). Actions are pinned by commit SHA. | The first release is `v0.1.0` (2026-11-07). On 2026-11-10 the repository has branch rulesets only: repository settings must still configure a `v*` tag ruleset, and before enabling `PYPI_PUBLISH_ENABLED=true`, restrict the `pypi` GitHub Environment to `v*` tags with required reviewers (see `docs/release.md`). |
| Dependencies | `ate-env-api` links `prometheus/client_golang`, the OpenTelemetry SDK and OTLP/gRPC trace exporter, and `otelgrpc`; they run only when metrics or tracing is on. `ate-env-reporter` links the same telemetry packages, which run only with `-metrics-listen-addr` or `-enable-tracing`, and alone links `k8s.io/client-go`, Substrate's generated WorkerPool client and `cloud.google.com/go/storage`, at the versions the Substrate module already selected. `ate-env-fleet-site` links `cloud.google.com/go/storage` and, from `google.golang.org/api` (already in the module graph), `idtoken` and `bigquery/v2`. `uv.lock` pins every Python dependency of `clients/python` and the `integrations/*` projects (one uv workspace; `integrations/nemo-gym` and the others get `ate-env-rl` from it as a workspace member) by version and SHA-256, and the Nix envs CI runs are built from it; uni-agent, a git dependency, is pinned by commit. `flake.lock` pins the toolchain (Go, the Python interpreters, uv, ko, kind, kubectl) and the Nix inputs; `go.mod`/`go.sum` pin the Go modules and the protobuf generators. Dependabot covers Go modules (`gomod`), `uv.lock` (`uv`; `openhands-*`, `uni-agent` and `grpcio-tools` are moved by hand), GitHub Actions, the OpenHands layer's base images (`docker`) and `flake.lock` (`nix`); see `.github/dependabot.yml`. `integrations/harbor` is a workspace member too; its `harbor` dependency is pinned to one minor in its `pyproject.toml` and bumped by hand (see Harbor integration). | — |
| Pre-flight probes | `WarmImage` creates probe environments from the caller's template and image in the caller's atespace, under ate-env-api's own Substrate identity as every environment is: at most 256 at once (`nodes x probes_per_node`, both capped, per round; the cap multiplies the count the workers report, a `nodes` above it is refused, and only the missing global viewer binding leaves a request's `nodes` trusted) for up to `max_rounds` (10) rounds, so 2,560 creates per call at most. It resumes them, deletes them when the round settles or the call is cancelled, and leaves a name that exists already alone; the default run id carries a random suffix so two calls never name a probe alike. A probe's create, resume and delete are retried on the transient failures of a loaded control plane, within the probe's timeout (the delete within a minute); a probe still there after that is listed in the coverage's `undeleted_probes` for the caller to sweep, and logged. A probe whose create was cut off can still be committed by the control plane after its delete, even when a retry made it first: the call repeats that delete for a short settle window and, before the round gives its probe slots back, lists the atespace once and deletes any of its probes that appeared later, so such a probe holds a slot until it is gone; when that list fails, such probes are listed in `undeleted_probes`. A probe whose resume failed ends `CRASHED`, which Substrate refuses to delete today, so it stays until the control plane takes the delete and is listed the same way. Across calls, the server caps the probes in flight per node of one worker selector (`-warm-max-probes-per-node`, default 2): a round takes its slots before it creates its probes, waits for them within the call's own deadline, and gives them back once they are deleted. `ListPlacementDomains` reads Substrate's workers, which needs the global viewer role on ate-env-api's identity: that role sees every worker and atespace name on the control plane, so it is a separate, optional binding (`ate-env manifest access-policy --global`); `WarmImage` works without it when the caller passes the node count, `ListPlacementDomains` does not. | A caller with the create permission can keep a pool busy with warm calls; the per-node cap bounds the first boots in flight on a node, not the calls, and there is no per-caller rate limit beyond it and the per-call caps. Probes a call could not delete hold a slot each until swept. A create that the control plane commits after the settle window and the round's list still leaks and is not listed. |
| Integrations and examples | The code in `integrations/` is maintained. | The code in `examples/` is not supported: it gets no security fixes and no compatibility promise. |

## Known gaps

1. Caller authentication uses one shared token, and the listener has no TLS. A deployment that
   is not made with `ate-env manifest` has no authentication unless it sets `-auth-token-file`.
2. Guest calls and MCP reach any actor that the router serves; the access policy does not limit
   them. The port proxy reaches the atespaces that `--port-proxy-atespaces` names.
3. HTTP `Host` validation (`internal/hostcheck`) blocks browser DNS rebinding across `/healthz`,
   MCP, and the port proxy, but the gRPC handler on `/` stays unwrapped and non-browser clients can
   set `Host` arbitrarily (relying on `-auth-token-file`). Between the `atenet` router and the
   actor, traffic on port 80 is unauthenticated HTTP/h2c and the guest does not authenticate
   callers.
4. `ate-env-api` does not verify the TLS certificate of the control plane by default.
5. Environment creation has no quota and no rate limit. The MCP server cache and the guest
   connection pool grow with each environment.
6. ate-env does not check the access control, the encryption or the retention of the snapshots
   bucket.
7. The manifest ships no NetworkPolicy and no `securityContext`.
8. ate-env sets no actor egress policy.
9. On-demand rootfs images accept any registry and create templates without a quota or garbage
   collection (accepted for now).
10. The metrics listeners of `ate-env-api` and `ate-env-reporter`, when on, have no authentication
    and no `Host` check. `ate-env-api` forwards incoming trace context from callers as is.
11. The OpenHands session key is readable from the template by each viewer of the atespace, all
    environments of one base template share it, and it is the only check of the agent-server. Code
    in the environment can read it from `/proc/<pid>/environ` of the agent-server or the guest,
    because it runs as the same user.
12. The hostname form of the port proxy (`--port-proxy-host-suffix`, off by default) puts each
    environment port under one domain. The ports are the same site, the proxy does not filter
    cookies, and ate-env neither manages nor checks the wildcard DNS and the TLS in front or who
    controls the domain.
