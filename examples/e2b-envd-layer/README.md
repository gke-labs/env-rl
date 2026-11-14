# Example: an E2B-compatible sandbox with envd as an added layer

An E2B sandbox is an image plus `envd`, the daemon that the E2B SDK talks to for commands, files and
PTYs. On E2B's cloud, envd is part of the sandbox template. Here it is an added runtime layer: an
image of its own that is mounted next to the root filesystem image. The task image is not rebuilt
and not changed, and any image can be an E2B-compatible sandbox.

```text
            one environment (a gVisor actor)
  +---------------------------------------------------+
  |  root filesystem: your image, unmodified          |   --rootfs-image
  |    /bin/bash, python, your code ...               |
  |                                                   |
  |  /ate       ate-env guest (read-only mount)       |   --guest-image
  |               PID 1, starts and watches envd      |
  |  /opt/e2b   envd layer (read-only mount)          |   --layer envd=<image>=/opt/e2b
  |               /opt/e2b/envd on port 49983         |   --sidecar '/opt/e2b/envd ...'
  +---------------------------------------------------+
        ^ gRPC: create, pause, kill          ^ HTTP: commands, files, PTYs
        |                                    |
  ate-env-api  <----------------------- port proxy
        ^
        |  from ate_env_e2b import Sandbox
   your E2B program
```

Three images make one environment, and none is built from another:

| Part                  | Where it comes from                                                    | Who chooses it                                             |
| --------------------- | ---------------------------------------------------------------------- | ---------------------------------------------------------- |
| Root filesystem image | Any digest-pinned image that runs as root and has `/bin/bash`          | The template, or each `Sandbox.create("<image>@sha256:...")` |
| ate-env guest         | The `ate-env-guest` image of this repository                           | The template                                               |
| envd layer            | [`integrations/e2b/layer`](../../integrations/e2b/layer): upstream's envd binary | The template                                     |

[docs/rootfs-images/RUNTIMES.md](../../docs/rootfs-images/RUNTIMES.md) describes the mechanism, and
[`examples/runtime-layers`](../runtime-layers/README.md) shows it with a stand-in binary. This
example uses the real envd and the E2B SDK through
[`integrations/e2b`](../../integrations/e2b/README.md).

## Run it

```bash
pip install ./clients/python -e integrations/e2b     # Python 3.11 or newer
kubectl -n ate-env port-forward svc/ate-env-api 7777:7777 &
export SUBSTRATE_ENV_AUTH_TOKEN=<caller token>         # when ate-env-api has one

GUEST_IMAGE=<registry>/ate-env-guest@sha256:<digest> \
ROOTFS_IMAGE=docker.io/library/python@sha256:<digest> \
LAYER_REPO=<registry>/ate-env-e2b-envd \
OTHER_IMAGE=docker.io/library/node@sha256:<digest> \
examples/e2b-envd-layer/demo.sh
```

`demo.sh` does three things:

1. It builds the envd layer and pushes it to `LAYER_REPO` (`docker buildx`), then pins it by
   digest. Set `LAYER_IMAGE` to a digest-pinned layer image instead to skip the build.
2. It registers the base template `e2b-base` with
   [`base-template.sh`](../../integrations/e2b/hack/base-template.sh): `ROOTFS_IMAGE`, the guest,
   the layer and the sidecar. A template of that name that exists is used as it is.
3. It runs [`program.py`](./program.py), an E2B SDK program.

`OTHER_IMAGE` is optional. With it, the program creates a second sandbox from that image on the
same base template: the layer, the sidecar and the capabilities are inherited, thus one template
serves each task image.

The header of `demo.sh` lists the other variables (`PLATFORM`, `TEMPLATE`, `ATESPACE`, the
binaries). Digests for public images come from `crane digest --platform linux/amd64 python:3.13-slim`
or `docker manifest inspect`.

## The program

`program.py` uses only the public E2B SDK. Its import line is the one ate-env part:

```python
from ate_env_e2b import Sandbox  # was: from e2b import Sandbox

with Sandbox.create(envs={"GREETING": "hello"}) as sandbox:
    sandbox.commands.run("ls /opt/e2b")          # the layer: LICENSE and envd
    sandbox.files.write("/tmp/note.txt", "written through envd")
    sandbox.pause()                              # a durable suspend
    same = Sandbox.connect(sandbox.sandbox_id)   # wakes the same environment

with Sandbox.create("docker.io/library/node@sha256:<digest>") as sandbox:
    ...                                          # another image, the same layer
```

`Sandbox.create()` without a template uses E2B's default name, `base`, which is the template
`e2b-base`. A digest-pinned image as the template runs that image on the base template.

## The template

What `base-template.sh` prints, shortened to the parts that make the layer:

```yaml
containers:
  - command:
      - /ate/ko-app/ate-env-guest
      - -sidecar
      - /opt/e2b/envd -isnotfc -no-cgroups -port 49983
      - -sidecar-readyz
      - http://127.0.0.1:49983/health
    image: docker.io/library/python@sha256:<digest> # the root filesystem image
    name: guest
    securityContext:
      capabilities:
        add: [SETUID, SETGID, CHOWN, DAC_OVERRIDE, FOWNER]
    volumeMounts:
      - { name: envd, mountPath: /opt/e2b }
      - { name: guest, mountPath: /ate }
volumes:
  - { name: envd, image: { reference: <registry>/ate-env-e2b-envd@sha256:<digest> } }
  - { name: guest, image: { reference: <registry>/ate-env-guest@sha256:<digest> } }
```

- The two `volumes` are image volumes: each image is mounted read-only into the container.
- The guest is PID 1. It starts envd as a sidecar and reports the environment ready only after
  envd's `/health` answers.
- The capabilities are the ones envd needs to start a process as a user and to give a file to a
  user ([`integrations/e2b`](../../integrations/e2b/README.md#capabilities)).

## What the image needs

- It runs as root: its `USER` is root or not set. `Sandbox.create` refuses another image and says
  why.
- `/bin/sh`, which envd starts each process through, and `/bin/bash`, which the E2B SDK runs each
  command and each PTY in. In an image without `/bin/bash`, such as Alpine, envd runs and the file
  calls work, and each command fails.

## Limits

[`integrations/e2b`](../../integrations/e2b/README.md#limits) lists what differs from E2B's cloud:
no time to live, the methods and arguments that are refused, and what a call gets on an
environment that is gone or suspended.
