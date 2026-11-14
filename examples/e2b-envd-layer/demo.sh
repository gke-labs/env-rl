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

# An E2B-compatible sandbox from any image: envd as an added runtime layer.
#
# envd is the daemon the E2B SDK talks to inside a sandbox. This script adds
# it to an unmodified rootfs image as a runtime layer, the way
# docs/rootfs-images/RUNTIMES.md describes: the layer image is mounted
# read-only at /opt/e2b, and the guest starts /opt/e2b/envd as a sidecar. Then
# it runs program.py, an E2B SDK program whose only ate-env part is its import
# line.
#
# What it does:
#   1. With LAYER_REPO: builds integrations/e2b/layer, pushes it and pins it
#      by digest. With LAYER_IMAGE: uses that image.
#   2. Registers the base template (integrations/e2b/hack/base-template.sh):
#      ROOTFS_IMAGE + the guest + the envd layer + the sidecar. A template of
#      that name that exists is used as it is.
#   3. Runs program.py: a sandbox from the base template, commands and files
#      through envd, a pause and a connect, and, with OTHER_IMAGE, a second
#      sandbox from another image on the same base, which keeps the layer.
#
# Required:
#   GUEST_IMAGE   digest-pinned ate-env-guest image
#   ROOTFS_IMAGE  digest-pinned image that runs as root and has /bin/bash,
#                 e.g. docker.io/library/python@sha256:...
#   LAYER_IMAGE   digest-pinned envd layer image, or
#   LAYER_REPO    a repository to build and push the layer to, e.g.
#                 <registry>/ate-env-e2b-envd (needs docker buildx)
# Optional:
#   OTHER_IMAGE   a second digest-pinned image (root, with /bin/bash)
#   PLATFORM      platform of the layer build (default linux/amd64: the
#                 platform of the worker nodes)
#   TEMPLATE      base template name (default e2b-base)
#   ATESPACE      atespace (default ate-env)
#   SUBSTRATE_ENV_API         ate-env-api address (default 127.0.0.1:7777;
#                             port-forward it)
#   SUBSTRATE_ENV_AUTH_TOKEN  the caller token, when ate-env-api has one
#   ATE_ENV, KUBECTL_ATE, PYTHON  binaries (default: ate-env, kubectl-ate and
#                 python3 on PATH; the Python needs ate-env-e2b installed)
set -euo pipefail

: "${GUEST_IMAGE:?set GUEST_IMAGE to the digest-pinned ate-env-guest image}"
: "${ROOTFS_IMAGE:?set ROOTFS_IMAGE to a digest-pinned image that runs as root and has /bin/bash}"
TEMPLATE=${TEMPLATE:-e2b-base}
ATESPACE=${ATESPACE:-ate-env}
PLATFORM=${PLATFORM:-linux/amd64}
export SUBSTRATE_ENV_API=${SUBSTRATE_ENV_API:-127.0.0.1:7777}
export ATE_ENV=${ATE_ENV:-ate-env}
KUBECTL_ATE=${KUBECTL_ATE:-kubectl-ate}
PYTHON=${PYTHON:-python3}

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2B="${HERE}/../../integrations/e2b"

step() { printf '\n== %s\n' "$*"; }

if [[ -z "${LAYER_IMAGE:-}" ]]; then
  : "${LAYER_REPO:?set LAYER_IMAGE to a digest-pinned envd layer image, or LAYER_REPO to build one}"
  step "build the envd layer for ${PLATFORM} and push it to ${LAYER_REPO}"
  docker buildx build --platform "${PLATFORM}" --push -t "${LAYER_REPO}:0.10.2" "${E2B}/layer"
  digest="$(docker buildx imagetools inspect --format '{{.Manifest.Digest}}' "${LAYER_REPO}:0.10.2")"
  LAYER_IMAGE="${LAYER_REPO}@${digest}"
fi
echo "envd layer: ${LAYER_IMAGE}"

if "${KUBECTL_ATE}" get actor-template --atespace "${ATESPACE}" "${TEMPLATE}" >/dev/null 2>&1; then
  step "use the template ${ATESPACE}/${TEMPLATE} that exists"
else
  step "register ${ATESPACE}/${TEMPLATE}: ${ROOTFS_IMAGE} + guest + envd layer"
  "${E2B}/hack/base-template.sh" "${LAYER_IMAGE}" \
    --template "${TEMPLATE}" --atespace "${ATESPACE}" \
    --rootfs-image "${ROOTFS_IMAGE}" --guest-image "${GUEST_IMAGE}" |
    "${KUBECTL_ATE}" create actor-template -f - >/dev/null
fi
"${KUBECTL_ATE}" get actor-template --atespace "${ATESPACE}" "${TEMPLATE}"

step "run the E2B program"
export SUBSTRATE_ENV_ATESPACE="${ATESPACE}"
if [[ "${TEMPLATE}" != "e2b-base" ]]; then
  # E2B's default template name is `base`; point it at this template.
  export SUBSTRATE_ENV_E2B_TEMPLATES="base=${TEMPLATE}"
fi
"${PYTHON}" "${HERE}/program.py" ${OTHER_IMAGE:+"${OTHER_IMAGE}"}
