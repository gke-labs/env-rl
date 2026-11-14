# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""An E2B SDK program on ate-env: the import line is the only change.

    python program.py [<image>@sha256:<digest>]

With `from e2b import Sandbox` this program runs on E2B's cloud. With the
import below it runs each sandbox as an ate-env environment on the base
template that demo.sh registers, where envd is an added runtime layer. The
optional argument is a second image: it runs on the same base template and
keeps the layer.

SUBSTRATE_ENV_API names ate-env-api, and SUBSTRATE_ENV_AUTH_TOKEN its caller
token (integrations/e2b/README.md).
"""

import sys

from ate_env_e2b import Sandbox  # was: from e2b import Sandbox


def show(sandbox: Sandbox) -> None:
    """Commands and files through envd, which the image itself does not have."""
    release = sandbox.commands.run(". /etc/os-release && echo $PRETTY_NAME").stdout.strip()
    print(f"  image:      {release}")
    # The layer is a read-only mount next to the image's own files.
    layer = sandbox.commands.run("ls /opt/e2b").stdout.split()
    print(f"  /opt/e2b:   {layer}")
    result = sandbox.commands.run("echo $GREETING from $(id -un) in $PWD")
    print(f"  command:    {result.stdout.strip()} (exit {result.exit_code})")
    sandbox.files.write("/tmp/note.txt", "written through envd")
    print(f"  file:       {sandbox.files.read('/tmp/note.txt')!r}")


def main() -> None:
    print("a sandbox from the base template")
    with Sandbox.create(envs={"GREETING": "hello"}) as sandbox:
        print(f"  sandbox_id: {sandbox.sandbox_id} (the environment id)")
        show(sandbox)

        # A pause is a durable suspend; connect wakes the same environment.
        sandbox.pause()
        print(f"  paused:     is_running={sandbox.is_running()}")
        same = Sandbox.connect(sandbox.sandbox_id)
        print(f"  connected:  file kept: {same.files.read('/tmp/note.txt')!r}")
    print("  killed at the end of the with block")

    if len(sys.argv) > 1:
        image = sys.argv[1]
        print(f"a sandbox from {image} on the same base template")
        # A digest-pinned image as E2B's `template`: the layer is inherited.
        with Sandbox.create(image, envs={"GREETING": "hello"}) as sandbox:
            show(sandbox)


if __name__ == "__main__":
    main()
