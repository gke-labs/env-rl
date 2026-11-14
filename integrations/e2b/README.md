# E2B on ate-env

A program written for the [E2B](https://github.com/e2b-dev/E2B) Python SDK runs its sandboxes as
ate-env environments. The import of the sandbox class is the only change:

```python
# before
from e2b import Sandbox       # or AsyncSandbox

# after
from ate_env_e2b import Sandbox
```

## How it works: envd is an added layer

An E2B sandbox is an image plus
[envd](https://github.com/e2b-dev/runtime/tree/main/packages/envd), the daemon the E2B SDK talks
to inside a sandbox. Here envd is not built into the image. It is a
[runtime layer](../../docs/rootfs-images/RUNTIMES.md): an image of its own, mounted read-only next
to the root filesystem image. The task image is not rebuilt and not changed, thus any image that
meets [two conditions](#what-the-image-needs) is an E2B-compatible sandbox.

| Part                  | What it is                                                              | Set by                                            |
| --------------------- | ----------------------------------------------------------------------- | ------------------------------------------------- |
| Root filesystem image | Your image, unmodified                                                  | The base template, or `Sandbox.create("<image>")` |
| ate-env guest         | Mounted at `/ate`. PID 1: it starts envd and watches it                 | The base template                                 |
| envd layer            | [`layer/`](./layer): upstream's envd binary, mounted at `/opt/e2b`      | The base template                                 |
| envd process          | `/opt/e2b/envd` on port 49983, a sidecar that the guest starts          | The base template                                 |

The two halves of this directory:

1. **The layer and its base template**, once for each cluster: build the layer image and
   [register the base template](#register-the-base-template) `e2b-base`. An environment created
   from another image on that base keeps the layer.
2. **The classes**, in each program: the package [`ate-env-e2b`](./src/ate_env_e2b) keeps the SDK's
   own `commands`, `files` and `pty` code, which talks to that envd through the port proxy, and
   sends the lifecycle calls to `ate-env-api` instead of E2B's API.

[`examples/e2b-envd-layer`](../../examples/e2b-envd-layer/README.md) does both ends in one script:
it builds the layer, registers the template and runs an E2B program on two images.

## Use the classes

The cluster needs the base template first ([Register the base template](#register-the-base-template)).

```bash
pip install ./clients/python -e integrations/e2b   # Python 3.11 or newer, e2b 2.63 to below 3

export SUBSTRATE_ENV_API=127.0.0.1:7777        # host:port, http://host:port or https://host
export SUBSTRATE_ENV_AUTH_TOKEN=<caller token> # when ate-env-api has one
```

| Variable                      | Meaning                                                              | Default   |
| ----------------------------- | -------------------------------------------------------------------- | --------- |
| `SUBSTRATE_ENV_API`           | The `ate-env-api` endpoint. Required.                                |           |
| `SUBSTRATE_ENV_AUTH_TOKEN`    | The caller token.                                                    | none      |
| `SUBSTRATE_ENV_ATESPACE`      | The atespace of the environments.                                    | `ate-env` |
| `SUBSTRATE_ENV_E2B_TEMPLATES` | E2B template names for registered templates, as `name=template,...`. | none      |
| `SUBSTRATE_ENV_E2B_WAKE_WAIT` | Seconds that the port proxy holds a request to a waking environment. | `60`      |

`ate_env_e2b.Sandbox` is a subclass of `e2b.Sandbox`, and `ate_env_e2b.AsyncSandbox` of
`e2b.AsyncSandbox`. `sandbox_id` is the environment id. The classes do not read `E2B_API_KEY` or
`E2B_DOMAIN`, and send no E2B key: an `api_key` argument is accepted and not used. The endpoint is
one address: a path, a query or a user in it is an error, and only `https` has a default port.

`template` in `Sandbox.create(template)` resolves in this order:

1. No template is E2B's default name, `base`.
2. A name in `SUBSTRATE_ENV_E2B_TEMPLATES` is that registered template.
3. `base` is the base template `e2b-base`. `base=<template>` in the variable names another one.
4. A digest-pinned image (`repo@sha256:...`) runs as `rootfs_image` on the base template.
5. Anything else raises `TemplateException` and creates nothing.

| E2B                                     | On ate-env                                                                                                                                                                                         |
| --------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `create`, `with Sandbox.create() as s:` | Creates the environment, waits for envd's `/health`, sends `envs` with `/init`, and starts one process through envd as the root check. A failure, a deadline or an interrupt deletes the environment. |
| `kill`                                  | Deletes the environment. `False` when it is gone.                                                                                                                                                  |
| `pause`, `beta_pause`                   | A durable suspend, with memory. `False` when already suspended.                                                                                                                                    |
| `connect`                               | Wakes a suspended environment and returns when envd answers. `Sandbox.connect(sandbox_id)` works from another process.                                                                             |
| `is_running`                            | Reads the environment's status first: it never wakes a suspended environment. `False` for one that is gone.                                                                                        |
| `get_info`                              | The state and the template. `started_at` and `end_at` are the Unix epoch, and `cpu_count` and `memory_mb` are 0: ate-env has none of them.                                                         |
| `commands`, `files`, `pty`, `git`       | The SDK's own code, on envd. Commands run as `root` in `/root`, which is envd's default; `user=` and `cwd=` on a call change them.                                                                 |

### Limits

- **The image must run as root.** envd sets the user of each process it starts, and an image whose
  `USER` is not root gives it no capability to do that. `create` refuses such an image with a
  `SandboxException` that names the user, and deletes the environment. A template without the
  capabilities of the base template is refused in the same step.
- **No time to live.** Nothing ends an environment after a time. A program that relies on E2B's
  default timeout of 300 s leaves its environments running unless it calls `kill` or uses the `with`
  block. A `timeout` argument of `create` or `connect` emits a `TimeoutNotEnforcedWarning` on each
  call.
- **No envd access token.** envd accepts each caller that the port proxy lets through: the caller
  token of `ate-env-api` is the guard.
- **Deadlines.** Each call to `ate-env-api` has a deadline. `request_timeout` sets it for one call,
  and as an argument of `create` or `connect` for the later calls of that sandbox, as in E2B.
  Without it, `create`, `connect` and `pause` have 300 s and the other calls 60 s. E2B's
  `request_timeout=0` (no timeout) is 300 s here. The deadline of `create` covers the wait for envd;
  the cleanup of a failed `create` has 30 s more, and the error says when the environment may remain.
- **A gone environment.** A data-plane call gets the port proxy's answer through the SDK's own
  mapping, which expects E2B's proxy. For an environment that is deleted, a command or a PTY call
  raises `NotFoundException` and a file call `FileNotFoundException`; on E2B both raise
  `SandboxNotRunningException`. For a crashed environment each raises `SandboxException`. Use
  `is_running` or `get_info` to read the state.
- **A suspended environment.** Each request to envd carries `X-Env-Wake-Wait: <seconds>`
  (`SUBSTRATE_ENV_E2B_WAKE_WAIT`, whole seconds, 60 by default). An `ate-env-api` with the wake hold
  then wakes the environment and forwards the request when the guest answers; the server caps the
  time (`-port-proxy-wake-wait-cap`, 1 minute by default). Each request with the header costs one
  probe of the guest, also when the environment runs; `0` sends no header. Without the hold, or
  when it ends, the port proxy answers `503` while the environment wakes: a command, a PTY call or
  `files.list` then raises `SandboxUnreachableException`, and `files.read` or `files.write` raises
  `SandboxException`. `connect` waits in each case; call it after a `pause`.
- **TLS.** For an `https://` endpoint the gRPC channel verifies against gRPC's default roots, the
  SDK's data plane against the system certificate store and its built-in roots, and the class's own
  requests (`/health`, `/init`, the root check) against `httpx`'s default, the `certifi` bundle or
  `SSL_CERT_FILE`. A private CA must be in each of them.
- **`os.fork()`.** gRPC's asyncio channels do not survive a fork. In a child of a process that had
  used the classes, each call raises `SandboxException` at once. Make the first call after the fork,
  or start workers with the `spawn` or `forkserver` method.

### What is refused

The classes override public names of the SDK only, and refuse by default. Each refusal is a
`SandboxException` that names this document, and `create` then creates nothing.

- Methods: each public method of the SDK class that is not in the table above, also one that a
  newer SDK adds. At e2b 2.63: `set_timeout`, `list`, `get_host`, `download_url`, `upload_url`,
  `update_network`, `fork`, `create_snapshot`, `list_snapshots`, `delete_snapshot`, `get_metrics`,
  `get_mcp_url`, `get_mcp_token`.
- Arguments: each argument with a value that is not `None` and not the SDK's default, except the
  ones below. An empty container is a value: `network={}` and `metadata={}` raise. At e2b 2.63 that
  refuses `metadata`, `secure`, `allow_internet_access=False`, `mcp`, `network`, `iam`, `lifecycle`,
  `volume_mounts` and `idempotency_key` of `create`; `on_resume="reboot"` of `connect`;
  `mode="filesystem"`, `keep_memory=False` and `strict=True` of `pause`; and the options `domain`,
  `api_url`, `headers`, `api_headers`, `proxy`, `retries` and `debug=True`.
- Accepted: `template`, `timeout`, `envs`, `logger` and `allow_internet_access=True` of `create`;
  `timeout` and `logger` of `connect`; `keep_memory=True` and `mode="full"` of `pause`; and the
  options `request_timeout`, `sandbox_url`, `http_version`, `debug=False`, `api_key` and
  `validate_api_key`.

`e2b` is held to `>=2.63,<3`. A newer 2.x cannot widen what the classes do: a new method or a new
argument is refused, and the SDK object holds no API key and an API address that does not resolve
(`ate-env-e2b.invalid`), thus an SDK code path that the classes do not map cannot reach E2B.
A weekly job (`.github/workflows/e2b-drift.yaml`, `hack/ci/e2b-drift.sh`) runs these tests and the
Kind test of the classes against the newest `e2b` below 3, which `uv.lock` does not hold. It is not
a required check.

### How it is tested

- `make ci-e2b` runs the unit tests against a fake `ate-env-api` with a fake envd for `/health`,
  `/init` and one process start: the lifecycle, the cleanup, the deadlines, the refusals and the
  structure of the classes. No command, file or PTY call runs there.
- The Kind E2E job runs [`tests/test_sandbox_e2e.py`](./tests/test_sandbox_e2e.py) on a cluster,
  under gVisor, through the port proxy on plaintext HTTP/1.1, with the public SDK calls only:
  - `create` with `envs`, and the `with` block;
  - `commands.run` with its output, exit code, callbacks, `envs=`, `cwd=` and `user=`, also a user
    whose home is missing; a background command with `wait`, `list` and `kill`; streamed stdin;
  - a PTY with input, output and `kill`;
  - `files.write`, `read`, `exists`, `list` and `make_dir`, a file for another user, a 100 MiB file
    up and down, and a directory watcher;
  - `pause`, then `Sandbox.connect(id)`: a file, a process and a watcher of before the pause;
  - `kill`, and `is_running`, `get_info` and a command after it;
  - the file calls in a musl image; the refusal of an image whose `USER` is not root;
  - one command and one file call with `AsyncSandbox`.
- Not tested: an `https://` endpoint, HTTP/2, a data-plane call on a suspended environment, and
  the same program against E2B's cloud.

## The runtime layer

| Path                                                 | What it is                                                                        |
| ---------------------------------------------------- | --------------------------------------------------------------------------------- |
| [`layer/Dockerfile`](./layer/Dockerfile)             | An image with upstream's static `envd` 0.10.2 release binary and its licence.   |
| [`hack/base-template.sh`](./hack/base-template.sh)   | Prints the base template: a rootfs image, the guest, the layer and the sidecar. |
| [`tests/test_envd_e2e.py`](./tests/test_envd_e2e.py) | Live checks of the layer in an environment, through the port proxy.             |

## Register the base template

No release publishes the layer image: build it and push it to your own registry. The build
downloads upstream's release binary for the platform and checks its sha256. Then register the
template with digest-pinned images:

```bash
docker buildx build --platform <platform of the worker nodes> --push -t <registry>/ate-env-e2b-envd:0.10.2 integrations/e2b/layer
LAYER=<registry>/ate-env-e2b-envd@$(docker buildx imagetools inspect --format '{{.Manifest.Digest}}' <registry>/ate-env-e2b-envd:0.10.2)

integrations/e2b/hack/base-template.sh "$LAYER" \
  --rootfs-image <image>@sha256:<digest> \
  --guest-image  <registry>/ate-env-guest@sha256:<digest> |
  kubectl-ate create actor-template -f -
```

The template is `e2b-base`. The guest starts `/opt/e2b/envd -isnotfc -no-cgroups -port 49983` as a
sidecar and reports ready only after envd's `/health` answers. An environment created with
`rootfs_image` on this base keeps the layer, the sidecar and the capabilities.

## Capabilities

An actor's container has `AUDIT_WRITE`, `KILL` and `NET_BIND_SERVICE` by default. The script adds
five more with `--add-capability`:

| Capability               | Why envd needs it                                                                                                                                     |
| ------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------- |
| `SETUID`, `SETGID`       | envd sets the user, the group and the groups of each process it starts, also for root. Without them each start fails with `operation not permitted`. |
| `CHOWN`                  | envd gives a file it writes to the user of the call.                                                                                                  |
| `DAC_OVERRIDE`, `FOWNER` | Root in the environment reads and re-modes the files of the other users, as root does in an E2B sandbox.                                              |

The guest and each command in the environment hold them too.

## Reach envd

envd listens on port 49983 inside the environment. A caller reaches it through the
[port proxy](../../docs/rootfs-images/RUNTIMES.md#who-can-reach-the-second-runtime) of `ate-env-api`,
with the API's caller token when the API has one.

## What the image needs

- It runs as root: its `USER` is root or not set. Substrate runs the container as the image's
  `USER`, and gives a non-root user no effective capability, thus `--add-capability` does not help
  it. In such an image envd answers `/health`, and the environment is created, but each process start
  fails with `fork/exec /bin/sh: operation not permitted`.
- `/bin/sh`: envd starts each process through it. envd itself is static and starts in a musl image
  (Alpine, for example).
- `/bin/bash`, for the E2B SDK: it runs each command and each PTY in `/bin/bash`. In an image
  without it, envd's file calls work and each command fails.

## Tests

`tests/test_envd_e2e.py` checks the layer on a live cluster, and the Kind E2E job runs it: envd
serves in a glibc image and, through a derived template, in a musl image; envd and a process it
started survive a suspend; a file written for another user belongs to that user, which fails on a
template without `CHOWN`, `DAC_OVERRIDE` or `FOWNER`; a request without the API token gets `401`
from the port proxy. One case records what envd does in an image with a non-root `USER`.

## Limits

- An image with a non-root `USER` is not supported (see [What the image needs](#what-the-image-needs)).
- The image holds envd's own licence. It does not hold the notices of the Go modules that the
  static binary contains. Add them to the image you build when your use needs them.
- The pins are the sha256 of upstream's binary for each architecture. Upstream tags no git release
  for this envd version, and its build can replace the bucket object of a version; a change fails
  the build at the checksum.
