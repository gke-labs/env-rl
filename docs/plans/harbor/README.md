# Plan: Harbor environment provider

Status: living document, last checked 2026-11-15 against Harbor `v0.26.0` (the latest release,
2026-11-13) and Harbor `main` at `ec6aeab7`, with `sandboxenv-rl` `main` at `1cd63f30`. The criteria
are in [acceptance-criteria.md](./acceptance-criteria.md).

## 1. Goal

[`integrations/harbor`](../../../integrations/harbor/README.md) is a Harbor environment provider:
`harbor run ... -e ate_env_harbor:AteEnvEnvironment` runs each trial of a Harbor dataset in an
ate-env environment, a Substrate actor in gVisor that runs the task image unmodified with the
ate-env guest mounted in. Any Harbor dataset (Terminal-Bench 2.0 first) must run on Substrate with
the environment flag and its `--ek` settings as the only change.

Harbor releases often and its `main` moves between releases: the pin names the minors the suite
passed on (`harbor>=0.25,<0.27`), and 0.26 already changed one contract the provider implements
(`preflight` takes the job kwargs, #269, and the base hook validates them). `uv.lock`, and thus the
"Harbor integration" CI job, stays on Harbor 0.25: Harbor 0.26 needs `fastmcp>=4.1.6` (`mcp>=2.3`),
which `openhands-sdk` 1.55.0 (`fastmcp<4`) and `nemo-gym` (`mcp<2`) in the same uv workspace refuse
(HB-27). The `harbor-drift` workflow runs the tests on 0.26 outside the workspace (HB-29). This plan
records what the provider promises as criteria with a status and the tests that check them, what it
does not cover and why, and the procedure that finds drift against a new Harbor release.

## 2. What the provider covers today

- The `BaseEnvironment` contract: `start`, `stop`, `exec`, `upload_file`, `upload_dir`,
  `download_file`, `download_dir`, `preflight`, `capabilities`, `resource_capabilities` and the
  definition validation, over the `ate_env_rl` client.
- Images by digest: a prebuilt `docker_image` as is, a tag resolved once per process, a Dockerfile
  built and pushed as `<registry>/<task>:<environment_id>` with buildx (`build=local`) or Cloud
  Build (`build=cloud-build`); `build=none` for prebuilt datasets.
- Resources as class templates: `cpus` and `memory_mb` select `<prefix>c<cpus>-m<memory_mb>`; `auto`
  falls back to `template` with a warning, `limit` refuses; `hack/register-templates.sh` prints the
  registration commands for a dataset.
- Placement: the `worker_selector` kwarg (`--ek worker_selector=pool=a,zone=b`, or a mapping) pins a
  job's environments to the worker pools that carry the labels; Substrate ANDs it with the
  template's own selector, thus it narrows the template's pools and never widens them.
- `exec` with Harbor's cwd precedence, the environment, the timeout (exit 137 at the deadline) and
  `user` (the image's user as is, another through `sudo -n` or `su`).
- Files as tar archives owned by root, keeping modes, symlinks and empty directories.
- `--no-delete` as suspend; a separate verifier environment with its own record file; the caller
  token from `SUBSTRATE_ENV_AUTH_TOKEN`, never recorded.
- Runtime layers: `tool_paths` puts a layer's directories first on `PATH` for every command, with a
  `/etc/profile.d` drop-in for login shells
  ([`examples/harbor-tools-layer`](../../../examples/harbor-tools-layer/README.md)).
- Node pre-warming: `python -m ate_env_harbor.warm <tasks-dir>` (`examples/harbor/run-tb2.sh warm`)
  runs before `harbor run`: per (class template, image) pair it ensures the derived template, waits
  for the golden snapshot and puts the image on each node of the pool (`WarmImage`), and exits
  non-zero when a pair is not on each node or a golden snapshot failed.

## 3. What it does not cover, and why

| Harbor capability                                  | Decision                                                                                                                                          |
| -------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| Docker Compose tasks (`docker-compose.yaml`)       | not planned: one environment runs one container; the task is refused before start                                                                 |
| `network_mode` other than `public`, egress control | not planned: ate-env sets no actor egress policy; what an actor reaches follows the Substrate deployment                                          |
| GPU and TPU tasks                                  | not planned now: no accelerator class templates; refused before start                                                                             |
| Host mounts (`volumes`), SSH stream mode           | not planned: an actor has no host filesystem; `attach` names the `ate-env <id> shell` command instead                                             |
| Labels on environments                             | not planned: ate-env has no labels; `ate_env.json` and `ate_env.verifier.<step>.json` carry the mapping to Harbor's ids                           |
| Run-as-user without `sudo` or `su` in the image    | deferred: needs user switching in the guest; today the call fails and says so                                                                     |
| A warm pool of environments across jobs            | superseded: a pool lives in one process; node warming (`WarmImage`, the warm step, HB-23) outlives it                                             |
| The in-tree `ate-env` environment type             | in progress upstream: harbor-framework/harbor#4137 is a draft until `ate-env-rl` is on PyPI; the import-path form is the supported way until then |

## 4. Drift check for a new Harbor release or `main`

Run this before each pin bump, and at least once a month while the upstream drafts are open. Put the
clone in a directory of its own; do not run code from it.

1. Pin: `git fetch` in the Harbor clone and note the tag or commit.
2. Contract diff:
   `git diff v0.26.0..<new> -- src/harbor/environments/base.py src/harbor/environments/factory.py src/harbor/environments/options.py src/harbor/environments/capabilities.py src/harbor/environments/definition.py src/harbor/environments/resource_policies.py src/harbor/environments/tar_transfer.py`.
   Look for new abstract methods or changed signatures (`v0.25.0` to `v0.26.0`: `preflight` gained
   `kwargs`, and `options_model` with `parse_options` arrived), new capability fields, the resource
   checks `base.py` runs against `resource_capabilities`, the cwd precedence in `exec`,
   `require_agent_environment_definition`, and the extra mechanics in the factory.
3. Caller diff:
   `git diff v0.26.0..<new> -- src/harbor/trial/trial.py src/harbor/verifier/verifier.py src/harbor/cli/jobs.py src/harbor/models/trial/paths.py src/harbor/models/task/config.py src/harbor/models/trial/config.py`.
   Look for the verifier's commands as root (`rm`, `chmod`), the `/logs` download, the session-id
   construction (the `__env` suffix and the truncated verifier ids), the `run_preflight` call sites,
   `--no-delete` handling, and the `EnvironmentConfig` fields and `ResourceMode` values the provider
   reads.
4. Suite: in a scratch venv with the provider installed, `pip install --no-deps harbor==<new>` (the
   pin `harbor>=0.25,<0.27` conflicts with a new minor until the bump), then
   `python -m pytest integrations/harbor/tests -q`, then `integrations/harbor/tests/test_trial.py`
   and `integrations/harbor/tests/test_tools_layer_docker.py` with a docker daemon. One pull request
   then moves the upper bound of the pin. While `uv.lock` holds an older Harbor than the newest
   minor the pin accepts (HB-27), the "Harbor integration" job does not run that minor. The
   `harbor-drift` workflow (`hack/ci/harbor-drift.sh`, HB-29) runs it, in an environment outside the
   uv workspace, on each pull request that changes the provider or the Python client.
5. Update [acceptance-criteria.md](./acceptance-criteria.md): one row per new capability or
   contract, the status of each row, and the "last checked" line above. Open an issue for each
   break.

## 5. Order of the open work

1. HB-24: the warm column of the comparison table in
   [`examples/harbor/README.md`](../../../examples/harbor/README.md): `run-tb2.sh warm`, then the
   run, live, once the cluster runs `ate-env-api` from current `main`.
2. HB-22: `hello-world` and a Terminal-Bench 2.0 subset live, once actors have egress or a Kind
   cluster with egress runs the e2e job.
3. HB-27: `uv.lock` and the "Harbor integration" job on Harbor 0.26, once `openhands-sdk` and
   `nemo-gym` accept `mcp>=2.3` in the versions the workspace pins, or the workspace declares the
   conflict (`[tool.uv] conflicts`, with one Nix package set per side). The lower bound of the pin
   then moves to 0.26.
4. HB-28: an `options_model` for the provider's kwargs, after HB-27 (the model needs Harbor 0.26).
5. HB-26: the upstream drafts leaving draft once `ate-env-rl` is on PyPI.
