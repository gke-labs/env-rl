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

from __future__ import annotations

from abc import ABC, abstractmethod
from collections.abc import Sequence
from typing import Any

from ..types import EnvironmentSpec, RawSandboxInstance, TeardownReport

# How many runs a driver keeps reap targets for, for verify_teardown (FL-18).
# A reap cannot tell whether a verify follows, so each one records; past this
# many runs the least recently reaped is dropped. A fleet verifies right after
# its reap, so the run it reads is the newest.
REAP_TARGET_RUNS = 64


def record_reap_targets(store: dict[str, list[str]], run_id: str, targets: Sequence[str]) -> None:
    """Record a reap's targets for run_id in store, after the ones an earlier
    reap of the run recorded that no verify has read as gone. The caller holds
    the lock that guards store."""
    merged = list(dict.fromkeys([*store.pop(run_id, []), *targets]))
    if merged:
        store[run_id] = merged
    while len(store) > REAP_TARGET_RUNS:
        del store[next(iter(store))]


def settle_reap_targets(store: dict[str, list[str]], run_id: str, gone: set[str]) -> None:
    """Drop from run_id's targets in store the ones a verify read as gone. The
    rest stay for the next verify. The caller holds the lock that guards store."""
    left = [aid for aid in store.get(run_id, []) if aid not in gone]
    if left:
        store[run_id] = left
    else:
        store.pop(run_id, None)


class BackendDriver(ABC):
    """
    Control Plane (Backend Protocol) Interface.
    
    Governs fleet provisioning, template creation, warm-pool sizing,
    instance claims, snapshotting, and run isolation / reaping.

    ``observer`` (an ``ate_env_rl.observability.Observer``, set by the fleet)
    receives claim and warm-replica counts under the ``backend_name`` label.
    A driver belongs to one fleet; a second fleet on the same driver takes
    over its events (and logs a warning).
    """

    backend_name: str = "-"
    observer: Any = None

    def _observe(self, event: str, *args: Any) -> None:
        obs = self.observer
        if obs is not None:
            getattr(obs, event)(self.backend_name, *args)

    def preflight_images(self, images: Sequence[str], **kwargs: Any) -> Any:
        """Put the images on the nodes their environments can land on before
        a run (``FleetConfig.preflight``); returns the coverage report, or
        None when the backend has nothing to warm. The Substrate driver
        implements it over ate-env-api's ``WarmImage``; the mock does not."""
        return None

    @abstractmethod
    def preflight(self) -> None:
        """Validate cluster connectivity, permissions, storage, and worker capacity."""

    @abstractmethod
    def ensure_template(self, env: EnvironmentSpec) -> str:
        """Ensure template/golden snapshot exists. Returns unique template ID."""

    @abstractmethod
    def warm_pool(
        self,
        template_id: str,
        replicas: int,
        wait: bool = True,
        run_id: str | None = None,
        rootfs_image: str | None = None,
    ) -> None:
        """Provision warm replicas (Ready pods in K8s or Paused Actors in Substrate)."""

    @abstractmethod
    def unwarm_pool(self, template_id: str, rootfs_image: str | None = None) -> None:
        """Scale down warm pool for the template to 0."""

    def resize_pool(
        self,
        template_id: str,
        replicas: int,
        wait: bool = True,
        rootfs_image: str | None = None,
    ) -> None:
        """Set the warm pool to exactly ``replicas``: grow by provisioning, shrink
        by deleting idle warm replicas, never touching claimed ones. Drivers
        that cannot shrink raise NotImplementedError; the fleet then falls back
        to warm_pool for growth and unwarm_pool for zero."""
        raise NotImplementedError

    def pool_size(self, template_id: str, rootfs_image: str | None = None) -> int | None:
        """Idle warm replicas in the pool, or None when the driver cannot tell."""
        return None

    @abstractmethod
    def acquire(
        self,
        template_id: str,
        run_id: str,
        timeout_s: float = 180.0,
        rootfs_image: str | None = None,
    ) -> RawSandboxInstance:
        """Instantly claim a sandbox instance from warm pool or golden."""

    @abstractmethod
    def release(self, instance_id: str, recycle: bool = False) -> None:
        """Release or recycle instance."""

    @abstractmethod
    def reap(self, run_id: str) -> int:
        """Force delete all resources associated with a run_id."""

    def verify_teardown(self, run_id: str) -> TeardownReport:
        """Report which environments the reaps of run_id targeted and left (FL-18).

        A driver that cannot tell raises NotImplementedError.
        """
        raise NotImplementedError(f"{type(self).__name__} cannot verify a teardown")

    def build_runtime(
        self, instance: RawSandboxInstance, data_plane: str = "ate_env"
    ) -> tuple[Any, Any | None]:
        """Construct runtime guest hook and data plane coordinates for this sandbox."""
        raise NotImplementedError
