#!/usr/bin/env bash

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

# Runs the Harbor provider's tests on the newest Harbor that the bounds of
# integrations/harbor/pyproject.toml allow, not the one in uv.lock
# (docs/plans/harbor, criterion HB-29).
#
# Why: uv.lock holds one Harbor version for the whole uv workspace, and it
# can be older than the newest minor that the provider accepts (HB-27). The
# "Harbor integration" job of ci.yaml runs the locked version. This script
# runs the other one, outside the workspace.
#
# What it does:
#   1. Makes a virtual environment with the system Python and installs the
#      Python client and integrations/harbor with its test extra. pip does
#      not read uv.lock, thus Harbor is the newest release inside the bounds.
#   2. Prints the Harbor version and the locked one, and adds them to
#      $GITHUB_STEP_SUMMARY when set.
#   3. Runs integrations/harbor/tests. With a docker daemon this includes
#      tests/test_trial.py and tests/test_tools_layer_docker.py.
#
# When it runs:
#   .github/workflows/harbor-drift.yaml: on a pull request that changes
#   integrations/harbor, the Python client, this script or the workflow, each
#   week, and on demand. It is not a required check.
#
# How to run:
#   ./hack/ci/harbor-drift.sh
#
# Prerequisites:
#   - python3 (3.12 or newer, Harbor's floor) with venv and pip.
#
# Outputs / exit codes:
#   - 0 when the tests pass. Non-zero names the Harbor version on stderr.
#   - It changes no pin and opens no pull request.
#
# See also:
#   integrations/harbor/pyproject.toml (the Harbor bounds),
#   docs/plans/harbor/README.md (section 4, the drift check).
set -o errexit -o nounset -o pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
VENV="$(mktemp -d "${TMPDIR:-/tmp}/harbor-drift-venv.XXXXXX")"
trap 'rm -rf "${VENV}"' EXIT
python3 -m venv "${VENV}"
"${VENV}/bin/pip" install --quiet --upgrade pip
# The client and the package in one command: pip takes ate-env-rl from the
# checkout that the command names and does not look for it on an index.
"${VENV}/bin/pip" install --quiet -e "${ROOT}/clients/python" -e "${ROOT}/integrations/harbor[test]"

HARBOR_VERSION="$("${VENV}/bin/python" -c 'import importlib.metadata as m; print(m.version("harbor"))')"
LOCKED="$(awk '/^name = "harbor"$/ { getline; gsub(/version = |"/, ""); print; exit }' "${ROOT}/uv.lock")"
echo "==> harbor ${HARBOR_VERSION} (uv.lock has ${LOCKED})"
if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  echo "harbor ${HARBOR_VERSION} (uv.lock has ${LOCKED})" >>"${GITHUB_STEP_SUMMARY}"
fi

echo "==> Tests of integrations/harbor on harbor ${HARBOR_VERSION}..."
# tests/test_e2e.py starts when SUBSTRATE_E2E_API_URL is set: a caller's value
# must not start the live tests here.
if ! (cd "${ROOT}" && SUBSTRATE_E2E_API_URL="" "${VENV}/bin/python" -m pytest integrations/harbor/tests -q); then
  echo "ERROR: the tests failed with harbor ${HARBOR_VERSION} (uv.lock has ${LOCKED})." >&2
  exit 1
fi
