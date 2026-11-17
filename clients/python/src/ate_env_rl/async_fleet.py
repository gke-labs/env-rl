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

"""`AsyncSandboxFleet` — an awaitable, event-loop-native fleet.

Enables RL post-training frameworks (TorchRL, VeRL, SkyRL) to overlap LLM token
generation with background sandbox pre-warming and non-blocking batch acquisition.
"""

from __future__ import annotations

import asyncio
import inspect
import logging
import time
from collections.abc import Callable, Iterable, Sequence
from typing import Any

from .config import FleetConfig
from .errors import PreflightError
from .fleet import SandboxFleet, _report, _skipped_checks
from .handle import SandboxHandle
from .observability import Observer, RunReport, repo_family
from .sources import FieldSpec, TaskSource
from .telemetry.metrics import status_of
from .telemetry.trace_recorder import (
    current_slot_wait_s,
    is_trace_mode,
    record_trace_event,
    register_env_meta,
    task_context,
)
from .types import FleetPlan, PreflightCheck, PreflightReport, Task, TeardownReport

logger = logging.getLogger("sandbox_sdk.async_fleet")


class AsyncSandboxFleet:
    """Awaitable, natively asynchronous wrapper over `SandboxFleet`.

    Enables asynchronous acquisition, batch claiming, and pipelining
    concurrency without blocking the asyncio event loop.
    """

    def __init__(
        self,
        config: FleetConfig | None = None,
        driver: Any | None = None,
        *,
        sync_fleet: SandboxFleet | None = None,
    ):
        cfg = sync_fleet.config if sync_fleet is not None else config
        if cfg is not None and cfg.exit_hooks:
            # The hooks call the sync teardown(), whose reap cannot reach
            # clients that belong to the caller's event loop.
            raise ValueError(
                "AsyncSandboxFleet does not support FleetConfig(exit_hooks=True) "
                "(https://github.com/ai-on-gke/sandboxenv-rl/issues/398); await "
                "fleet.teardown() in a try/finally, or from a handler that "
                "loop.add_signal_handler installs"
            )
        self._fleet = sync_fleet or SandboxFleet(config, driver)

    def close(self, *, wait: bool = True) -> None:
        """Close fleet resources; blocks the calling thread.

        From a coroutine, await ``close_async`` instead: a Substrate backend's
        ``close()`` waits for the deletes a cancelled ``acquire`` started, and
        a blocked event loop cannot serve them.
        """
        if hasattr(self.backend, "close"):
            self.backend.close()

    async def close_async(self) -> None:
        """Close fleet resources on the event loop: awaits the backend's
        ``close_async`` (its ``close`` in a thread when it has none)."""
        backend = self.backend
        if hasattr(backend, "close_async"):
            await backend.close_async()
        elif hasattr(backend, "close"):
            await asyncio.to_thread(backend.close)

    def __del__(self) -> None:
        try:
            self.close(wait=False)
        except Exception:
            pass

    # --- Synchronous passthroughs ---
    @property
    def config(self) -> FleetConfig:
        return self._fleet.config

    @property
    def backend(self) -> Any:
        return self._fleet.backend

    @property
    def tasks(self) -> list[Task]:
        return self._fleet.tasks

    @property
    def observer(self) -> Observer:
        return self._fleet.observer

    @property
    def report(self) -> RunReport | None:
        """The RunReport of the last run()."""
        return self._fleet.report

    @property
    def _backend_label(self) -> str:
        return self._fleet.backend_label

    def load_tasks(self, tasks: Iterable[Task | dict] | TaskSource, *, image_field: FieldSpec = None,
                   id_field: FieldSpec = None) -> None:
        """Load tasks, rows or a source (see ``SandboxFleet.load_tasks``).

        Synchronous: a source's ``load()`` runs on the calling thread, the
        event loop's when called from a coroutine.
        """
        self._fleet.load_tasks(tasks, image_field=image_field, id_field=id_field)

    def add_tasks(self, tasks: Iterable[Task | dict] | TaskSource, *, image_field: FieldSpec = None,
                  id_field: FieldSpec = None) -> list[Task]:
        """Append tasks and extend the plan (see ``SandboxFleet.add_tasks``)."""
        return self._fleet.add_tasks(tasks, image_field=image_field, id_field=id_field)

    def image_counts(self) -> dict[str, int]:
        return self._fleet.image_counts()

    @property
    def preflight_coverage(self) -> Any:
        """The image pre-flight's coverage (``FleetConfig.preflight``), set by ``setup()`` or ``run()``."""
        return self._fleet.preflight_coverage

    # --- Awaitable Lifecycle Methods ---
    async def preflight(self) -> None:
        if hasattr(self.backend, "preflight_async"):
            with self.observer.phase("preflight", backend=self._backend_label):
                await self.backend.preflight_async()
        else:
            await asyncio.to_thread(self._fleet.preflight)

    async def preflight_report(self, strict: bool = False) -> PreflightReport:
        """As SandboxFleet.preflight_report, on the event loop (FL-4)."""
        if not hasattr(self.backend, "preflight_async"):
            return await asyncio.to_thread(self._fleet.preflight_report, strict)
        images = self._fleet._preflight_image_list()
        with self.observer.phase("preflight_report", backend=self._backend_label):
            try:
                await self.backend.preflight_async()
                checks = [PreflightCheck("connectivity", "ok")]
            except Exception as e:  # noqa: BLE001 - reported; strict=True raises
                checks = [PreflightCheck("connectivity", "failed", f"{type(e).__name__}: {e}")]
            if checks[0].status != "ok":
                checks += _skipped_checks(images, "the connectivity check failed")
            elif hasattr(self.backend, "preflight_checks_async"):
                checks += await self.backend.preflight_checks_async(images)
            elif hasattr(self.backend, "preflight_checks"):
                checks += await asyncio.to_thread(self.backend.preflight_checks, images)
            return _report(checks, strict)

    async def plan(self) -> FleetPlan:
        return await asyncio.to_thread(self._fleet.plan)

    async def _preflight_images(self, report: Any) -> None:
        """``FleetConfig.preflight`` for the async fleet (see
        ``SandboxFleet._preflight_images``), on this loop when the backend
        has ``preflight_images_async``; a pass ``setup()`` made for the same
        images is not repeated by ``run()``."""
        if not hasattr(self.backend, "preflight_images_async"):
            await asyncio.to_thread(self._fleet._preflight_images, report)
            return
        fleet = self._fleet
        images = fleet._preflight_image_list()
        if fleet._preflight_reused(images, report):
            return
        fleet.preflight_coverage = None
        fleet._preflight_for = None
        try:
            with self.observer.phase("preflight_images", backend=self._backend_label):
                coverage = await self.backend.preflight_images_async(
                    images, observer=self.observer, **fleet._preflight_knobs()
                )
        except PreflightError as e:
            # As SandboxFleet._preflight_images: record the refused pass.
            fleet._record_preflight(report, e.coverage)
            raise
        fleet._preflight_for = frozenset(images)
        fleet._record_preflight(report, coverage)

    async def setup(self, tasks: list[Any] | None = None) -> AsyncSandboxFleet:
        if tasks is not None:
            self._fleet.load_tasks(tasks)
        await self.preflight()
        # The image pre-flight comes before the pools: a pool filled first
        # would boot on cold nodes.
        if self.config.preflight and self._fleet.tasks:
            await self._preflight_images(None)
        plan = await self.plan()
        await self._warm_many(
            [(e.image, e.template_id, e.rootfs_image, e.replicas) for e in plan.entries],
            wait=True,
        )
        return self

    async def _ensure_template_for_image_async(self, image: str) -> tuple[str, str | None]:
        cached = self._fleet._image_to_template.get(image)
        if cached is not None:
            return cached
        return await asyncio.to_thread(self._fleet._ensure_template_for_image, image)

    async def _warm_pool_for_template(
        self,
        template_id: str,
        replicas: int,
        wait: bool = True,
        rootfs_image: str | None = None,
        family: str = "-",
    ) -> None:
        with self.observer.phase("warm_pool", backend=self._backend_label, family=family):
            if hasattr(self.backend, "warm_pool_async"):
                await self.backend.warm_pool_async(
                    template_id, replicas, wait=wait, run_id=self._fleet.run_id,
                    rootfs_image=rootfs_image,
                )
            else:
                await asyncio.to_thread(
                    self.backend.warm_pool,
                    template_id,
                    replicas,
                    wait=wait,
                    run_id=self._fleet.run_id,
                    rootfs_image=rootfs_image,
                )

    async def warm_images(
        self, images: list[str], replicas: int | None = None, wait: bool = True
    ) -> None:
        """Warm pools for ``images``, sized as ``SandboxFleet.warm_images``.

        Up to ``warm_parallelism`` images warm at once; the first error is
        raised after every image was tried.
        """
        if replicas is not None:
            sized = {img: replicas for img in images}
        else:
            sized = self._fleet._size_pools(self.image_counts(), images)
        work = []
        for img in dict.fromkeys(images):
            template_id, rootfs_image = await self._ensure_template_for_image_async(img)
            work.append((img, template_id, rootfs_image, sized[img]))
        await self._warm_many(work, wait=wait)

    async def _warm_many(
        self, work: list[tuple[str, str, str | None, int]], wait: bool
    ) -> None:
        work = [w for w in work if w[3] > 0]
        if not work:
            return
        sem = asyncio.Semaphore(self.config.warm_parallelism)

        async def _warm_one(item: tuple[str, str, str | None, int]) -> None:
            img, template_id, rootfs_image, rep = item
            async with sem:
                await self._warm_pool_for_template(
                    template_id, rep, wait=wait, rootfs_image=rootfs_image,
                    family=repo_family(img),
                )

        results = await asyncio.gather(*(_warm_one(w) for w in work), return_exceptions=True)
        errors = [r for r in results if isinstance(r, BaseException)]
        for r in errors:
            if not isinstance(r, Exception):  # CancelledError, KeyboardInterrupt
                raise r
        if errors:
            if len(errors) > 1:
                logger.warning("%d of %d images failed to warm; raising the first",
                               len(errors), len(work))
            raise errors[0]

    async def warm_image(
        self, image: str, replicas: int | None = None, wait: bool = True
    ) -> None:
        """Warm one image's pool (agent-sandbox-rl's ``warm_image``)."""
        await self.warm_images([image], replicas=replicas, wait=wait)

    async def set_pool_replicas(self, image: str, replicas: int, wait: bool = True) -> None:
        """Size one image's warm pool to exactly ``replicas``, up or down; see
        ``SandboxFleet.set_pool_replicas``."""
        replicas = max(0, int(replicas))
        template_id, rootfs_image = await self._ensure_template_for_image_async(image)
        family = repo_family(image)
        try:
            with self.observer.phase("warm_pool", backend=self._backend_label, family=family):
                if hasattr(self.backend, "resize_pool_async"):
                    await self.backend.resize_pool_async(
                        template_id, replicas, wait=wait, rootfs_image=rootfs_image
                    )
                else:
                    await asyncio.to_thread(
                        self.backend.resize_pool,
                        template_id,
                        replicas,
                        wait=wait,
                        rootfs_image=rootfs_image,
                    )
        except NotImplementedError:
            if replicas == 0:
                await self.unwarm_image(image)
            else:
                logger.warning(
                    "backend %s has no resize_pool: set_pool_replicas(%s, %d) can grow the "
                    "pool but cannot shrink it below its current size",
                    self._backend_label, image, replicas,
                )
                await self._warm_pool_for_template(
                    template_id, replicas, wait=wait, rootfs_image=rootfs_image, family=family
                )

    async def unwarm_image(self, image: str) -> None:
        entry = self._fleet._image_to_template.get(image)
        if entry:
            template_id, rootfs_image = entry
            if hasattr(self.backend, "unwarm_pool_async"):
                await self.backend.unwarm_pool_async(template_id, rootfs_image=rootfs_image)
            else:
                await asyncio.to_thread(
                    self.backend.unwarm_pool, template_id, rootfs_image=rootfs_image
                )

    def acquired_count(self, image: str) -> int:
        """Sandboxes acquired for ``image``, warm or cold (see ``SandboxFleet.acquired_count``)."""
        return self._fleet.acquired_count(image)

    def pool_size(self, image: str) -> int | None:
        """Idle warm replicas for ``image`` (see ``SandboxFleet.pool_size``)."""
        return self._fleet.pool_size(image)

    async def acquire(self, task: Task | str, timeout_s: float | None = None) -> SandboxHandle:
        """Asynchronously acquire a single sandbox."""
        task_obj: Task
        if isinstance(task, str):
            found = next((t for t in self.tasks if t.id == task), None)
            if found:
                task_obj = found
            else:
                task_obj = Task(id=task, image="default")
        else:
            task_obj = task

        with self.observer.phase(
            "acquire", backend=self._backend_label, family=repo_family(task_obj)
        ):
            return await self._acquire_task(task_obj, timeout_s)

    async def _acquire_task(self, task_obj: Task, timeout_s: float | None) -> SandboxHandle:
        # ttfi starts when the caller asked for a sandbox: before the slot
        # wait, which ran already and left its length in the context.
        acquire_started_at = time.monotonic() - (current_slot_wait_s.get() or 0.0)
        if task_obj.template:
            template_id = task_obj.template
            rootfs_image = None
        else:
            template_id, rootfs_image = await self._ensure_template_for_image_async(task_obj.image)
        to_s = timeout_s or self.config.acquire_timeout_s

        if hasattr(self.backend, "acquire_async"):
            with task_context(
                run_id=self._fleet.run_id,
                instance=task_obj.id,
                image=task_obj.image,
            ):
                raw_inst = await self.backend.acquire_async(
                    template_id, self._fleet.run_id, timeout_s=to_s, rootfs_image=rootfs_image
                )
        else:
            with task_context(
                run_id=self._fleet.run_id,
                instance=task_obj.id,
                image=task_obj.image,
            ):
                raw_inst = await asyncio.to_thread(
                    self.backend.acquire,
                    template_id,
                    self._fleet.run_id,
                    timeout_s=to_s,
                    rootfs_image=rootfs_image,
                )

        register_env_meta(
            raw_inst.instance_id,
            instance=task_obj.id,
            image=task_obj.image,
            run_id=self._fleet.run_id,
        )
        if self._backend_label == "mock" and is_trace_mode():
            record_trace_event(
                "create",
                env=raw_inst.instance_id,
                dur_ms=0.0,
                ok=True,
                run=self._fleet.run_id,
                instance=task_obj.id,
                image=task_obj.image,
            )
        self._fleet._count_acquire(task_obj.image)

        try:
            runtime, data_plane = self._fleet._build_runtime(raw_inst)
        except Exception:
            try:
                if hasattr(self.backend, "release_async"):
                    await self.backend.release_async(raw_inst.instance_id, recycle=False)
                else:
                    await asyncio.to_thread(self.backend.release, raw_inst.instance_id, recycle=False)
            except Exception:
                logger.warning(
                    "Failed to release sandbox %s after runtime setup error",
                    raw_inst.instance_id,
                    exc_info=True,
                )
            raise

        claim_source = str(raw_inst.metadata.get("claim_source", "direct"))
        def _on_release(h):
            with self._fleet._lock:
                self._fleet._active_handles.pop(id(h), None)

        handle = SandboxHandle(
            sandbox_id=raw_inst.instance_id,
            endpoint=data_plane.address if data_plane else raw_inst.endpoint,
            task=task_obj,
            run_id=self._fleet.run_id,
            backend=self.backend,
            runtime=runtime,
            data_plane=data_plane,
            default_timeout_s=self.config.step_timeout_s,
            tracer=self._fleet._tracer,
            claim_source=claim_source,
            _on_release=_on_release,
            _acquire_started_at=acquire_started_at,
        )

        with self._fleet._lock:
            self._fleet._active_handles[id(handle)] = handle
        return handle

    async def acquire_batch(
        self, tasks: list[Task | str], timeout_s: float | None = None
    ) -> list[SandboxHandle]:
        """Asynchronously acquire a batch of sandboxes in parallel.

        All-or-nothing: if any acquisition fails, the sandboxes that were
        acquired are released before the first error is re-raised, so a partial
        failure never leaks sandboxes.
        """
        results = await asyncio.gather(
            *(self.acquire(t, timeout_s=timeout_s) for t in tasks), return_exceptions=True
        )
        errors = [r for r in results if isinstance(r, BaseException)]
        if not errors:
            return list(results)  # type: ignore[arg-type]
        acquired = [r for r in results if isinstance(r, SandboxHandle)]
        # Through self.release, so these releases are in the run report and
        # the release-phase metrics like any other.
        released = await asyncio.gather(
            *(self.release(h) for h in acquired), return_exceptions=True
        )
        for h, rel in zip(acquired, released):
            if isinstance(rel, BaseException):
                logger.warning(
                    "Failed to release sandbox %s after batch acquire error",
                    h.sandbox_id,
                    exc_info=rel,
                )
        raise errors[0]

    async def release(self, handle: SandboxHandle) -> None:
        await self._release_one(handle, recycle=False)

    async def _release_one(self, h: SandboxHandle, recycle: bool) -> bool:
        """Release or recycle one handle; returns whether this call did it.

        A handle that is released already, or being released, records no
        phase and no trace event.
        """
        if h._release_started():
            return False
        phase, event = ("recycle", "suspend") if recycle else ("release", "delete")
        with self.observer.phase(phase, backend=self._backend_label, family=repo_family(h.task)):
            if not await h._release_async(recycle=recycle):
                return False
            if self._backend_label == "mock" and is_trace_mode():
                record_trace_event(
                    event,
                    env=h.sandbox_id,
                    dur_ms=0.0,
                    ok=True,
                    run=self._fleet.run_id,
                    instance=h.task.id if h.task else None,
                    image=h.task.image if h.task else None,
                )
        return True

    async def teardown(self, verify: bool = False) -> TeardownReport | None:
        """Tear down as SandboxFleet.teardown, with the same ``verify``."""
        try:
            with self.observer.phase("teardown", backend=self._backend_label):
                if hasattr(self.backend, "reap_async"):
                    await self.backend.reap_async(self._fleet.run_id)
                else:
                    await asyncio.to_thread(self.backend.reap, self._fleet.run_id)
            # The reap deleted every handed-out environment.
            self._fleet._active_handles = {}
        except Exception as e:
            if verify:
                try:
                    e.teardown_report = await self._verify_teardown()  # type: ignore[attr-defined]
                except Exception as verify_error:
                    e.teardown_report_error = verify_error  # type: ignore[attr-defined]
            raise
        finally:
            self.observer.warm_reset()
            # The next setup() or run() warms again.
            self._fleet._preflight_for = None
        if verify:
            return await self._verify_teardown()
        return None

    async def _verify_teardown(self) -> TeardownReport:
        if hasattr(self.backend, "verify_teardown_async"):
            return await self.backend.verify_teardown_async(self._fleet.run_id)
        return await asyncio.to_thread(self.backend.verify_teardown, self._fleet.run_id)

    # --- Async Context Manager ---
    async def __aenter__(self) -> AsyncSandboxFleet:
        await self.setup()
        return self

    async def __aexit__(self, exc_type, exc_val, exc_tb) -> None:
        # Like SandboxFleet.__exit__: a teardown or close error is raised only
        # when the body succeeded, so it never replaces the body's exception
        # (for example the InfrastructureError of a reap that cannot delete).
        try:
            try:
                await self.teardown()
            finally:
                await self.close_async()
        except Exception:
            if exc_type is None:
                raise
            logger.warning("Fleet teardown failed after an error in the body", exc_info=True)

    # --- Parallel Processing ---
    async def _call_process_fn(
        self, process_fn: Callable[..., Any], task: Task, handle: SandboxHandle
    ) -> Any:
        if inspect.iscoroutinefunction(process_fn) or inspect.iscoroutinefunction(
            getattr(process_fn, "__call__", None)
        ):
            return await process_fn(task, handle)
        result = await asyncio.to_thread(process_fn, task, handle)
        if inspect.isawaitable(result):
            return await result
        return result

    async def run(
        self,
        process_fn: Callable[[Task, SandboxHandle], Any],
        concurrency: int | None = None,
    ) -> list[Any]:
        """Execute tasks asynchronously with bounded concurrency.

        The run's RunReport is available as ``fleet.report`` afterwards;
        ``fleet.report.summary()`` gives the table (logged only at DEBUG). It
        holds the phases that run inside ``run()`` (``acquire``, ``process``,
        ``release``, ``ensure_template`` on first use of an image, and with
        ``FleetConfig(preflight=True)`` the image pre-flight's
        ``preflight_images``, ``preflight_golden`` and ``preflight_probe``
        when ``setup()`` did not run it first).
        ``setup()`` and ``teardown()`` (``async with fleet:``) run outside
        it: their phases go to the Prometheus series and spans, not to the
        report.
        """
        # One snapshot for the whole run: tasks added meanwhile are not run.
        tasks = self.tasks
        c = concurrency or min(self.config.max_concurrent, len(tasks) or 1)
        sem = asyncio.Semaphore(max(1, c))
        results: list[Any] = [None] * len(tasks)
        obs = self.observer
        backend = self._backend_label

        async def _release(handle: SandboxHandle) -> None:
            # release_async keeps the delete RPC off the event loop thread, so
            # one release does not stall (and inflate) the other tasks' phases.
            with obs.phase("release", backend=backend, family=repo_family(handle.task)):
                await handle.release_async()
                if backend == "mock" and is_trace_mode():
                    record_trace_event(
                        "delete",
                        env=handle.sandbox_id,
                        dur_ms=0.0,
                        ok=True,
                        run=self._fleet.run_id,
                        instance=handle.task.id if handle.task else None,
                        image=handle.task.image if handle.task else None,
                    )

        async def _worker(idx: int, task: Task) -> None:
            wait_start = time.monotonic()
            async with sem:
                family = repo_family(task)
                start = time.monotonic()
                slot_token = current_slot_wait_s.set(max(0.0, start - wait_start))
                status = "ok"
                try:
                    with task_context(
                        run_id=self._fleet.run_id,
                        instance=task.id,
                        image=task.image,
                    ):
                        handle = await self.acquire(task)
                        try:
                            with obs.phase("process", backend=backend, family=family):
                                res = await self._call_process_fn(process_fn, task, handle)
                            results[idx] = res
                            await _release(handle)
                        except Exception as e:
                            status = "error"
                            logger.error("Async execution failed for task %s: %s", task.id, e)
                            await _release(handle)
                            results[idx] = e
                except BaseException as e:
                    status = status_of(e)
                    raise
                finally:
                    current_slot_wait_s.reset(slot_token)
                    obs.task_done(backend, family, status, time.monotonic() - start)

        strategy = self.config.strategy
        with obs.run(strategy, backend=backend) as report:
            self._fleet.report = report
            report.environment = self._fleet.describe_environment(concurrency=max(1, c))
            try:
                if self.config.preflight:
                    await self._preflight_images(report)
                await asyncio.gather(*(_worker(i, t) for i, t in enumerate(tasks)))
            finally:
                # As in SandboxFleet.run(): the pass is spent when the run ends.
                if self.config.preflight:
                    self._fleet._preflight_spent()
        logger.debug("\n%s", report.summary())
        return results

    async def recycle(self, handle: SandboxHandle | Sequence[SandboxHandle]) -> None:
        """Return sandbox instance(s) to the warm pool."""
        for h in handle if isinstance(handle, (list, tuple)) else (handle,):
            await self._release_one(h, recycle=True)

    async def release_all(self) -> int:
        """Release each handed-out handle that is not yet released or recycled.

        Returns how many this call released. Every handle is tried; the first
        release error is raised after the others ran. A handle stays in the
        fleet until its release succeeds, so a later call retries the ones
        that failed or that a cancellation stopped.
        """
        with self._fleet._lock:
            handles = list(self._fleet._active_handles.values())
        results = await asyncio.gather(
            *(self._release_one(h, recycle=False) for h in handles), return_exceptions=True
        )
        errors = [r for r in results if isinstance(r, BaseException)]
        if errors:
            raise errors[0]
        return sum(1 for r in results if r is True)
