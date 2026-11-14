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

# Runs the E2B class against the newest e2b release below 3, not the one in
# uv.lock (docs/plans/e2b-runtime-support, section 7, criterion E2B-37).
#
# What it does:
#   1. Makes a virtual environment with the system Python and installs the
#      Python client, integrations/e2b with its test extra, and the newest
#      e2b that the package's own bounds allow. pip does not read uv.lock.
#   2. Prints the e2b version, and adds it to $GITHUB_STEP_SUMMARY when set.
#   3. Runs the unit tests of integrations/e2b, which hold the structural
#      tests (E2B-28): a public name or an argument that a new release adds
#      fails them.
#   4. Runs the Kind E2E pipeline (hack/ci/e2e.py) with that environment's
#      pytest for the test of the class (E2B-27), through E2B_PYTEST_BIN.
#
# When it runs:
#   .github/workflows/e2b-drift.yaml: each week, on demand, and on a pull
#   request that changes the workflow or this script. It gates no other pull
#   request: a new upstream release must not fail an unrelated change.
#
# How to run:
#   ./hack/ci/e2b-drift.sh            # both steps
#   SKIP_KIND=1 ./hack/ci/e2b-drift.sh  # the unit tests only
#
# Prerequisites:
#   - python3 (3.11 or newer) with venv and pip.
#   - For the Kind step: what hack/ci/e2e.py needs (docker, go, ko, kind,
#     kubectl), from `nix develop` when nix is on PATH, else from PATH.
#
# Environment variables:
#   E2B_DRIFT_VENV  (a temporary directory)  Where the environment goes. The
#                                            script replaces this directory and
#                                            keeps it. A relative path is
#                                            relative to the caller's directory.
#                                            Unset: the environment is removed
#                                            when the script ends.
#   SKIP_KIND       (unset)                  Set to 1 to skip step 4
#
# Outputs / exit codes:
#   - 0 when both steps pass. Non-zero names the e2b version on stderr.
#   - It changes no pin and opens no pull request.
#
# See also:
#   integrations/e2b/pyproject.toml (the e2b bounds), integrations/e2b/tests,
#   hack/ci/e2e.py, hack/ci/test_e2b_drift.py (the tests of this script).
set -o errexit -o nounset -o pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
if [[ -n "${E2B_DRIFT_VENV:-}" ]]; then
  # The script changes directory later: make a relative path absolute now.
  case "${E2B_DRIFT_VENV}" in
    /*) VENV="${E2B_DRIFT_VENV}" ;;
    *) VENV="$(pwd)/${E2B_DRIFT_VENV}" ;;
  esac
  rm -rf "${VENV}"
else
  VENV="$(mktemp -d "${TMPDIR:-/tmp}/e2b-drift-venv.XXXXXX")"
  trap 'rm -rf "${VENV}"' EXIT
fi
python3 -m venv "${VENV}"
"${VENV}/bin/pip" install --quiet --upgrade pip
# The client and the package in one command: pip takes ate-env-rl from the
# checkout that the command names and does not look for it on an index, also
# when an index has a newer ate-env-rl. The environment is new, thus each
# other dependency is the newest release that the bounds allow.
"${VENV}/bin/pip" install --quiet -e "${ROOT}/clients/python" -e "${ROOT}/integrations/e2b[test]"

E2B_VERSION="$("${VENV}/bin/python" -c 'import importlib.metadata as m; print(m.version("e2b"))')"
LOCKED="$(awk '/^name = "e2b"$/ { getline; gsub(/version = |"/, ""); print; exit }' "${ROOT}/uv.lock")"
echo "==> e2b ${E2B_VERSION} (uv.lock has ${LOCKED})"
if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  echo "e2b ${E2B_VERSION} (uv.lock has ${LOCKED})" >>"${GITHUB_STEP_SUMMARY}"
fi

failed() {
  echo "ERROR: $1 failed with e2b ${E2B_VERSION} (uv.lock has ${LOCKED})." >&2
  exit 1
}

echo "==> Unit tests of integrations/e2b, with the structural tests (E2B-28)..."
# The live tests in the same directory start when these two are set. Step 4
# is the one that runs them: a caller's values must not start them here.
(cd "${ROOT}/integrations/e2b" && SUBSTRATE_E2E_API_URL="" E2B_E2E_TEMPLATE="" "${VENV}/bin/python" -m pytest -q) || failed "the unit tests"

if [[ "${SKIP_KIND:-}" == "1" ]]; then
  echo "==> SKIP_KIND=1: no Kind test of the class."
  exit 0
fi

echo "==> Kind E2E, with the test of the class (E2B-27) on e2b ${E2B_VERSION}..."
export E2B_PYTEST_BIN="${VENV}/bin/pytest"
if command -v nix >/dev/null; then
  (cd "${ROOT}" && nix develop -c ci-e2e) || failed "the Kind E2E"
else
  python3 "${ROOT}/hack/ci/e2e.py" || failed "the Kind E2E"
fi
