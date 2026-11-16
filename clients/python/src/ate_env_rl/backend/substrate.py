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

import asyncio
import dataclasses
import logging
import math
import os
import threading
import time
import uuid
import zlib
from collections.abc import Mapping, Sequence
from typing import Any

import grpc

from ..client import DEFAULT_ATESPACE, Client
from ..config import WARM_POOL_RUN_ID, TracerConfig
from ..errors import (
    FailedPreconditionError,
    InfrastructureError,
    NotFoundError,
    PermissionDeniedError,
    PreflightError,
    RpcError,
    SandboxStartError,
)
from ..loop_runner import _STOP_ROUNDS, LoopRunner
from ..preflight import (
    _DIGEST,
    DomainCoverage,
    ImagePlacement,
    PlacementEntry,
    PreflightCoverage,
    PreflightWarmer,
    _transient,
)
from ..telemetry import metrics
from ..telemetry.trace_recorder import (
    current_slot_wait_s,
    current_warm_claim,
    in_driver_span,
)
from ..telemetry.tracing import (
    CLIENT_SPAN_PREFIX,
    mark_error,
    set_attributes,
    start_span,
    tracer_from_config,
)
from ..types import (
    DataPlaneEndpoint,
    EnvironmentSpec,
    GoldenSnapshotState,
    PlacementDomain,
    PreflightCheck,
    RawSandboxInstance,
)
from .base import BackendDriver

logger = logging.getLogger("ate_env_rl.backend.substrate")


def _preflight_error_check(name: str, err: BaseException) -> PreflightCheck:
    """A preflight check for an RPC error: skipped for UNIMPLEMENTED (FL-3)."""
    if isinstance(err, RpcError) and err.code == grpc.StatusCode.UNIMPLEMENTED:
        return PreflightCheck(name, "skipped", f"the server does not serve it: {err}")
    return PreflightCheck(name, "failed", f"{type(err).__name__}: {err}")


def _pools_with_nodes(
    pools: Sequence[PlacementDomain], selector: Mapping[str, str]
) -> list[PlacementDomain]:
    """The pools with active nodes whose labels match ``selector``: where a
    create that sends that worker selector can land."""
    return [
        p for p in pools
        if p.nodes > 0 and all(p.selector.get(k) == v for k, v in selector.items())
    ]


# How many GetImageStatus calls preflight_report() keeps in flight. The
# client sets no per-call deadline and the driver has no setting for one, so
# a call that does not answer holds its slot until the caller gives up.
_PREFLIGHT_STATUS_CONCURRENCY = 8


# How long a reap, and close(), wait for the background deletes a cancelled
# acquire or warm_pool call started before naming the actors still deleting.
# A Substrate delete takes well under a second; the bound keeps a hung
# delete from holding teardown, and the tasks run on until the loop stops.
DEFAULT_BACKGROUND_DELETE_TIMEOUT_S = 10.0

# Loop iterations _wait_background_deletes yields before each check of the
# background deletes. The join is complete only if a task cancelled just
# before or during the reap reaches _spawn_background_delete in its cancel
# handler within this many iterations: the fake client needs one or two, and
# an interceptor-wrapped grpc.aio call a few more. A handler slower than that
# starts a delete the join does not see, and the actor stays owned under
# WARM_POOL_RUN_ID for a later reap.
_CANCEL_SETTLE_YIELDS = 8


def _loop_running() -> bool:
    """Whether an event loop runs in this thread. A GeneratorExit thrown into
    a task by a closing loop finds none: there the delete is left to a later
    reap instead of failing in asyncio.create_task."""
    try:
        asyncio.get_running_loop()
    except RuntimeError:
        return False
    return True


class _WarmPoolDict(dict):
    """Warm paused pool dictionary keyed by (template, image).

    Allows string keys for backwards compatibility, treating 'template' as (template, None).
    """

    def _normalize(self, key: Any) -> Any:
        if isinstance(key, str):
            return (key, None)
        return key

    def __getitem__(self, key: Any) -> Any:
        return super().__getitem__(self._normalize(key))

    def __setitem__(self, key: Any, value: Any) -> None:
        super().__setitem__(self._normalize(key), value)

    def __contains__(self, key: Any) -> bool:
        return super().__contains__(self._normalize(key))

    def get(self, key: Any, default: Any = None) -> Any:
        return super().get(self._normalize(key), default)

    def pop(self, key: Any, *args: Any) -> Any:
        return super().pop(self._normalize(key), *args)

    def setdefault(self, key: Any, default: Any = None) -> Any:
        return super().setdefault(self._normalize(key), default)


class SubstrateBackendDriver(BackendDriver):
    """Agent Substrate Backend Driver.

    Manages environment lifecycle directly against the ate-env-api gRPC service
    via ate_env_rl.Client: creating environments, suspending for pre-warmed pools,
    and deleting on release.

    tracer_config enables ``ate_env_client.ensure_template`` / ``.warm_pool`` /
    ``.acquire`` / ``.release`` / ``.reap`` spans and is handed to the Client
    the driver builds (needs the ``tracing`` extra).

    background_delete_timeout_s is how long ``reap`` and ``close()`` wait for
    the background deletes that cancelled ``acquire`` and ``warm_pool`` calls
    started: a finite number of seconds, ``>= 0`` (a bool, NaN, infinity or a
    negative value raises ``ValueError``), 10 by default. ``0`` means no wait:
    the reap still lets the cancel handlers start their deletes, then names
    every delete still running in the ``InfrastructureError`` it raises, and
    ``close()`` logs them, as both do for a delete still running when a longer
    wait ends. A joined delete that failed, or was cancelled, counts as a
    failed reap delete: the reap names its environment too, and ``close()``
    logs it; one whose environment is already gone counts as done. ``reap``
    cancels none of those deletes, and their environments stay tracked for
    the next ``reap`` of any run. ``close()`` first cancels the calls still
    in flight on its own loop, so a cancelled ``create()`` starts its delete
    while the channels are open, waits the same bound, and then closes the
    channels it owns and stops its loop, which ends a delete still running.
    ``close()`` and ``close_async()`` count as a teardown: that delete starts
    even when no ``reap`` ran since the call began, and a recycle or
    ``warm_pool`` that finishes during them deletes instead of pooling. A
    ``release`` (recycling or not) that is cancelled, or whose delete fails,
    after a teardown began since the call started (a ``reap`` of any run, or
    a close) starts a background delete as well, joined the same way: that
    reap could not see the environment. With no teardown the environment
    stays tracked under its run id, for the run's ``reap``. The delete of a
    ``warm_pool`` call that finished after the ``reap``, when a close
    cancels it, starts one too. So does a ``reap`` still in flight
    (another thread's teardown) that they cancel, or whose deletes their
    channel close cuts off, for each environment it had not deleted.
    ``close_async()`` on the caller's loop cancels no task: the part of a
    ``reap`` or ``warm_pool`` call that waited for one of the call's 16
    slots stops when the channel close frees one, and deletes in the
    background as the parts that were cut off do. ``close_async()`` repeats
    its join, 4 rounds at most, while a client that a delete dialed is open.
    An ``unwarm_pool`` or a shrinking ``resize_pool`` tracks the
    environments it takes out of the pool for the next ``reap`` of any run
    from that moment, so one that is cancelled leaves none of them to the
    ``reap`` of the run that warmed them alone. Cut off by a close, it
    starts a background delete for each one it had not deleted, as a
    ``reap`` does, and so does a ``warm_pool`` call that deletes its
    environments after a teardown.
    """

    backend_name = "substrate"

    def __init__(
        self,
        api_endpoint: str | None = None,
        atespace: str = DEFAULT_ATESPACE,
        worker_family: str = "c2",
        auth_token: str | None = None,
        *,
        client: Client | None = None,
        image_templates: Mapping[str, str] | None = None,
        default_template: str | None = None,
        labels: Mapping[str, str] | None = None,
        acquire_timeout_s: float = 180.0,
        ready_timeout_s: float = 180.0,
        loop_runner: LoopRunner | None = None,
        tracer_config: TracerConfig | None = None,
        connections: int = 1,
        worker_selector: Mapping[str, str] | None = None,
        placement: Mapping[str, Mapping[str, str]] | None = None,
        background_delete_timeout_s: float = DEFAULT_BACKGROUND_DELETE_TIMEOUT_S,
    ):
        if not isinstance(connections, int) or connections < 1:
            raise ValueError(f"connections must be >= 1, got {connections!r}")
        # A bool is not a timeout; NaN never satisfies the join's deadline
        # check, so _wait_background_deletes would never time out.
        if (
            isinstance(background_delete_timeout_s, bool)
            or not isinstance(background_delete_timeout_s, (int, float))
            or not math.isfinite(background_delete_timeout_s)
            or background_delete_timeout_s < 0
        ):
            raise ValueError(
                "background_delete_timeout_s must be a finite number of seconds >= 0, "
                f"got {background_delete_timeout_s!r}"
            )
        target = (
            api_endpoint
            or os.environ.get("ATE_ENV_API_TARGET")
            or os.environ.get("SUBSTRATE_API_ENDPOINT")
            or "127.0.0.1:7777"
        )
        self.api_endpoint = target
        self.atespace = atespace or DEFAULT_ATESPACE
        self.worker_family = worker_family
        self.auth_token = auth_token
        self.image_templates = dict(image_templates or {})
        self.default_template = default_template
        self.labels = dict(labels or {})
        # The per-environment worker selector every create sends, and the
        # per-image labels merged over it (FleetConfig.node_selector and
        # FleetConfig.placement).
        self.worker_selector = dict(worker_selector or {})
        self.placement = {img: dict(sel) for img, sel in (placement or {}).items()}
        # Backoff, seconds, between repeats of the pre-flight's pool listing;
        # and the images one pre-flight pass takes at most (0: by the run id
        # budget, preflight_batch_size), which tests shrink.
        self.preflight_list_backoff = 1.0
        self.preflight_batch_images = 0
        self.acquire_timeout_s = acquire_timeout_s
        self.ready_timeout_s = ready_timeout_s
        # How long reap and close() wait for background deletes
        # (_wait_background_deletes) before naming the actors still deleting;
        # 0 is no wait.
        self.background_delete_timeout_s = background_delete_timeout_s
        self.tracer_config = tracer_config or TracerConfig()
        self.tracer = tracer_from_config(self.tracer_config)

        self._loop_runner = loop_runner
        self._owns_loop_runner = loop_runner is None

        if client is not None:
            self._client = client
            self._owns_client = False
        else:
            self._client = None
            self._owns_client = True
        # A caller-provided client is used for every call; the pool only
        # applies to clients the driver creates.
        self.connections = connections if self._owns_client else 1
        # Extra clients 1..connections-1; index 0 is self._client.
        self._extra_clients: list[Client] = []

        self._lock = threading.Lock()
        self._owned_actors: dict[str, RawSandboxInstance] = {}
        self._warm_paused_pool: _WarmPoolDict = _WarmPoolDict()
        # Bumped when a reap starts and when it ends, and when close() starts
        # (_count_teardown); _reaping counts the reaps in progress. A recycle,
        # warm_pool or warm-claim put-back that awaited across a reap, or
        # finishes during one, deletes its actor instead of pooling it
        # (_may_pool).
        self._teardowns = 0
        self._reaping = 0
        # Whether close() or close_async() is running. A delete of an unpooled
        # actor, or a reap, that close cancels then starts a background delete
        # per actor, which close joins: no reap follows to delete the actor.
        # A release lost during a close does too (_return_popped), as after
        # any teardown.
        self._closing = False
        # The image pre-flights in progress, each a task of the driver's on
        # its caller's loop. A close cancels them (_cancel_preflights):
        # close_async() on the caller's loop cancels no task of the caller,
        # and a pre-flight that sleeps between two requests has no call for
        # the channel close to cut off.
        self._preflights: set[asyncio.Task[Any]] = set()
        # Per run id: reaps of that run in progress, and the _teardowns
        # value when its last reap ended. An acquire whose own run was reaped
        # while it created the actor owns it under WARM_POOL_RUN_ID (AC-15.10).
        self._run_reaps_running: dict[str, int] = {}
        self._run_reap_ended: dict[str, int] = {}
        # Bumped per pool key by unwarm_pool and by a resize_pool that shrinks
        # below what is pooled and in flight: a warm_pool call that started
        # before deletes what it created instead of pooling it.
        self._pool_gens: dict[tuple[str, str | None], int] = {}
        # Actors that warm_pool calls are still creating, per (pool key,
        # _teardowns, pool generation), so overlapping calls do not each
        # provision the full shortfall. A reap drops every reservation with
        # its generation; unwarm_pool and resize_pool start a new one.
        self._warm_inflight: dict[tuple[tuple[str, str | None], int, int], int] = {}
        self._background_tasks: set[asyncio.Task[Any]] = set()
        # The background deletes a cancelled acquire or warm_pool call started
        # (AC-15.2, AC-15.8), each with the id of the actor it deletes, so a
        # reap can join them and name the ones still running at its deadline.
        self._background_deletes: dict[asyncio.Task[Any], str] = {}
        # The actors whose background deletes did not land (failed, or
        # cancelled) since the last join, in order, written by
        # _background_delete before its task ends. The next join reports the
        # ones still owned (_take_failed_background_deletes): a delete that
        # fails fast would otherwise leave the task done and the reap
        # returning as if the actor were gone.
        self._failed_background_deletes: dict[str, None] = {}

    def _get_loop_runner(self) -> LoopRunner:
        with self._lock:
            if self._loop_runner is None:
                self._loop_runner = LoopRunner(name="ate-substrate-driver-sync")
                self._owns_loop_runner = True
            return self._loop_runner

    def _new_client(self) -> Client:
        # With more than one connection, every channel gets its own
        # subchannel pool: otherwise gRPC shares one TCP connection between
        # channels with the same target and arguments, and that connection's
        # HTTP/2 stream limit caps the calls in flight.
        options = [("grpc.use_local_subchannel_pool", 1)] if self.connections > 1 else None
        return Client(
            self.api_endpoint,
            auth_token=self.auth_token,
            tracer_config=self.tracer_config,
            channel_options=options,
        )

    @property
    def client(self) -> Client:
        # Under the lock, so concurrent first calls (for example from
        # _client_for with index 0) create one channel, not several.
        with self._lock:
            if self._client is None:
                self._client = self._new_client()
            return self._client

    def _client_for(self, actor_id: str) -> Client:
        """The client that carries every call for actor_id.

        Actors are spread over the pool by a stable hash, so all calls for
        one actor share a connection (and so one ate-env-api pod).
        """
        if self.connections == 1:
            return self.client
        index = zlib.crc32(actor_id.encode()) % self.connections
        if index == 0:
            return self.client
        with self._lock:
            while len(self._extra_clients) < self.connections - 1:
                self._extra_clients.append(self._new_client())
            return self._extra_clients[index - 1]

    def _take_owned_clients(self) -> list[Client]:
        with self._lock:
            clients = [c for c in [self._client, *self._extra_clients] if c is not None]
            self._client = None
            self._extra_clients = []
        return clients

    @staticmethod
    async def _close_clients(clients: list[Client]) -> None:
        """Close clients concurrently; every close runs even if one fails."""
        results = await asyncio.gather(*(c.close() for c in clients), return_exceptions=True)
        for result in results:
            if isinstance(result, BaseException):
                raise result

    def _sync(self, coro: Any) -> Any:
        return self._get_loop_runner().run(coro)

    async def _quiesce(self, cancel_calls: bool) -> None:
        """Join the background deletes on this loop, before the clients close.

        The reap's own join can miss one: a cold acquire still in create()
        when the reap took its snapshot, and cancelled after the reap's join
        returned (by its timeout_s, say), starts its delete between the reap
        and close(). Late ones, and ones that failed since the reap's join,
        are logged by actor id.

        With ``cancel_calls``, every other task on this loop is cancelled and
        waited for first. On an owned LoopRunner the tasks are all the
        driver's (the sync facades', the runtimes'), and the cancel handler
        of an acquire or warm_pool call still in create() starts its delete,
        which then runs on a client that is still open. The caller's loop
        is not quiesced that way: its tasks are the caller's.
        """
        if cancel_calls:
            me = asyncio.current_task()
            with self._lock:
                deletes = set(self._background_deletes)
            tasks = [t for t in asyncio.all_tasks() if t is not me and t not in deletes]
            for t in tasks:
                t.cancel()
            if tasks:
                await asyncio.gather(*tasks, return_exceptions=True)
        await self._wait_background_deletes()

    def _count_teardown(self) -> None:
        """Count a teardown before close() cancels a call or closes a channel.

        The cancel handler of an acquire or warm_pool call in create() then
        sees one (_may_pool) and starts its delete, which the shutdown joins,
        and a recycle or warm_pool that finishes during the shutdown deletes
        instead of pooling. Without it, a call that started after the run's
        reap, or with no reap at all, left its environment live. It also
        marks the driver as closing (_closing), so a release, or a delete of
        an unpooled actor, that the shutdown cancels starts a background
        delete too (_return_popped, _delete_unpooled), and a reap it cuts off
        starts one for each actor it had not deleted
        (_delete_unreaped_in_close). The image pre-flights in progress are
        cancelled (_cancel_preflights).
        """
        with self._lock:
            self._teardowns += 1
            self._closing = True
        self._cancel_preflights()

    def _cancel_preflights(self) -> None:
        """Cancel the image pre-flights in progress, each on its own loop.

        They are the driver's tasks (preflight_images_async awaits one), so a
        close on the caller's loop ends them too. A pre-flight in a request
        is cut off by the channel close, but one that sleeps (the pool-listing
        backoff, the golden poll, a warm call's backoff) would wake after the
        close: on the closed channel the warmer reports a short coverage and
        the fleet goes on to create environments, or the pre-flight dials a
        new client through the ``client`` property that no close closes. The
        cancelled warmer ends its calls in flight, and the server deletes
        their probes.

        A cancel for another loop arrives at the task's next await, which
        can be after this close returned. The task is taken out of
        _preflights here, and a pass that is not in it dials no client
        (_preflight_client).
        """
        with self._lock:
            preflights = list(self._preflights)
            self._preflights.clear()
        for task in preflights:
            loop = task.get_loop()
            try:
                running = asyncio.get_running_loop()
            except RuntimeError:
                running = None
            if loop is running:
                task.cancel("the driver is closing")
                continue
            try:
                loop.call_soon_threadsafe(task.cancel, "the driver is closing")
            except RuntimeError:
                # The loop is closed: the task cannot run on.
                pass

    def _preflight_client(self) -> Client:
        """The ``client`` property for the pre-flight pass that calls it.

        A pass that a close cancelled (_cancel_preflights) gets
        CancelledError instead of a client the close would not close: the
        cancel of a close in another thread reaches the pass only at its
        next await. Under one lock hold with the dial, so a close either
        cancelled the pass before, or takes the client dialed here.
        """
        task = asyncio.current_task()
        with self._lock:
            if task not in self._preflights:
                raise asyncio.CancelledError("the driver is closing")
            if self._client is None:
                self._client = self._new_client()
            return self._client

    async def _shutdown_async(self, cancel_calls: bool) -> None:
        """close()'s work on one loop: count a teardown, join the deletes, then
        close the owned clients."""
        self._count_teardown()
        await self._quiesce(cancel_calls)
        if not self._owns_client:
            return
        await self._close_clients(self._take_owned_clients())
        if not cancel_calls:
            # Closing the channels cancelled the calls still in flight on
            # this loop. The cancel handler of an acquire or warm_pool call
            # in create() starts a delete, which dials a new client (the
            # client property): join those too, then close that client.
            # Closing it can cut off a call that was not a background delete
            # (a caller's task that was not cancelled and went on to its next
            # call), whose handler starts a delete on one more client, so
            # repeat while a client is open, for as many rounds as
            # LoopRunner.stop() takes for the tasks its cancels start.
            for _ in range(_STOP_ROUNDS):
                await self._quiesce(cancel_calls=False)
                clients = self._take_owned_clients()
                if not clients:
                    break
                await self._close_clients(clients)

    async def close_async(self) -> None:
        runner = self._loop_runner if self._owns_loop_runner else None
        if runner is not None and runner.on_loop_thread():
            # Awaited on the runner's own loop: stop() would be a no-op there,
            # so the runner is kept for a later close().
            runner = None
        try:
            if runner is not None:
                # The sync facades' and the runtimes' tasks run on the owned
                # runner: cancel them there and join the deletes they start,
                # while the clients are still open. Their cancel handlers look
                # for a teardown too.
                self._count_teardown()
                await asyncio.wrap_future(runner.submit(self._quiesce(cancel_calls=True)))
            await self._shutdown_async(cancel_calls=False)
            if runner is not None:
                runner.stop()
                self._loop_runner = None
        finally:
            with self._lock:
                self._closing = False

    def close(self) -> None:
        runner = self._loop_runner
        if runner is None:
            # No loop of the driver's ever ran, but a pre-flight can be in
            # progress on its caller's loop: end it first. It dials no client
            # from then on, so the channels read below are all there are.
            with self._lock:
                preflights = bool(self._preflights)
            if preflights:
                self._count_teardown()
            with self._lock:
                clients = self._owns_client and bool(self._client or self._extra_clients)
                if preflights and not clients:
                    self._closing = False
            if not clients:
                return
            # Channels, but no loop of the driver's ever ran: close them on
            # one, stopped below.
            runner = self._get_loop_runner()
        owns_runner = self._owns_loop_runner
        # On an owned runner the tasks are all the driver's: cancel the calls
        # still in flight, join the deletes they start, then close the
        # clients. A caller's runner keeps its tasks.
        try:
            runner.run(self._shutdown_async(cancel_calls=owns_runner))
            if owns_runner:
                runner.stop()
                self._loop_runner = None
        finally:
            with self._lock:
                self._closing = False

    async def preflight_async(self) -> None:
        """Validate ate-env-api connectivity."""
        logger.info(
            "Running Substrate preflight checks (Atespace: %s, Endpoint: %s)...",
            self.atespace,
            self.api_endpoint,
        )
        try:
            await self.client.get(
                f"preflight-probe-{uuid.uuid4().hex[:6]}", atespace=self.atespace
            )
        except NotFoundError:
            # NotFoundError confirms the gRPC server is reachable and active.
            logger.debug("Preflight probe successfully reached ate-env-api")
        except Exception as e:
            raise PreflightError(
                f"Substrate preflight check failed to reach ate-env-api at {self.api_endpoint}: {e}"
            ) from e

    def preflight(self) -> None:
        self._sync(self.preflight_async())

    async def preflight_checks_async(self, images: Sequence[str]) -> list[PreflightCheck]:
        """The checks of ``SandboxFleet.preflight_report()`` after connectivity (FL-1).

        A ``placement`` check (``ListPlacementDomains`` for the base
        template): ok when, for each worker selector the creates of
        ``images`` send (the fleet's ``node_selector`` with each image's
        ``placement`` labels over it, as ``worker_selector_for`` builds it),
        a pool with nodes matches; with no image, the ``node_selector``
        alone. An image routed through ``image_templates`` creates from
        another template and adds no selector.

        One ``image:<image>`` check per digest-pinned image that creates
        derive a template for (``GetImageStatus``, at most
        ``_PREFLIGHT_STATUS_CONCURRENCY`` calls in flight): failed when its
        golden snapshot's bake failed or when neither the base template nor
        the derived template exists, else ok with the template and golden
        state. An RPC that answers UNIMPLEMENTED makes its checks skipped
        (FL-3); any other error makes them failed with the error.
        """
        template = self.default_template or "default-template"
        base_missing = False
        try:
            domains = await self.client.list_placement_domains(
                template_name=template, atespace=self.atespace
            )
        except Exception as e:  # noqa: BLE001 - reported, not raised
            base_missing = isinstance(e, NotFoundError)
            placement = _preflight_error_check("placement", e)
        else:
            placement = self._placement_check(images, domains)

        derived = [i for i in images if i not in self.image_templates and _DIGEST.search(i)]
        slots = asyncio.Semaphore(_PREFLIGHT_STATUS_CONCURRENCY)

        async def status(image: str) -> Any:
            async with slots:
                return await self.client.image_status(
                    image, template_name=template, atespace=self.atespace
                )

        answers = await asyncio.gather(*(status(i) for i in derived), return_exceptions=True)
        by_image = dict(zip(derived, answers, strict=True))
        checks: list[PreflightCheck] = []
        for image in images:
            name = f"image:{image}"
            if image not in by_image:
                checks.append(PreflightCheck(name, "skipped", "no derived template"))
                continue
            st = by_image[image]
            if isinstance(st, BaseException):
                if not isinstance(st, Exception):
                    raise st
                checks.append(_preflight_error_check(name, st))
                continue
            detail = f"template {st.template.name} exists={st.template_exists} golden={st.golden.name}"
            if st.golden == GoldenSnapshotState.FAILED:
                checks.append(PreflightCheck(name, "failed", f"{detail}: {st.golden_error}"))
            elif base_missing and not st.template_exists:
                # A derived template that exists is used as is: the server
                # reads the base template only to derive a missing one.
                checks.append(PreflightCheck(
                    name, "failed", f"{detail}: the base template {template!r} does not exist"
                ))
            else:
                checks.append(PreflightCheck(name, "ok", detail))
        return [*checks, placement]

    def _placement_check(
        self, images: Sequence[str], domains: Sequence[PlacementDomain]
    ) -> PreflightCheck:
        """The ``placement`` check of preflight_checks_async for the base
        template's pools ``domains``."""
        selectors: dict[tuple[tuple[str, str], ...], dict[str, str]] = {}
        for image in images:
            if image not in self.image_templates:
                sel = self.worker_selector_for(image) or {}
                selectors[tuple(sorted(sel.items()))] = sel
        if not selectors:
            selectors[()] = self.worker_selector_for(None) or {}
        pools = ", ".join(f"{d.name}: {d.nodes} nodes, {d.node_slots} slots/node" for d in domains)
        unmatched = [sel for sel in selectors.values() if not _pools_with_nodes(domains, sel)]
        if not unmatched:
            return PreflightCheck("placement", "ok", pools)
        missing = "; ".join(f"no pool with nodes matches the worker selector {sel}" for sel in unmatched)
        return PreflightCheck("placement", "failed", f"{missing} ({pools or 'no placement domain'})")

    def preflight_checks(self, images: Sequence[str]) -> list[PreflightCheck]:
        return self._sync(self.preflight_checks_async(images))

    async def preflight_images_async(
        self,
        images: Sequence[str],
        *,
        observer: Any = None,
        run_id: str = "",
        probes_per_node: int = 2,
        image_concurrency: int = 4,
        max_rounds: int = 3,
        golden_timeout_s: float = 30 * 60,
        probe_timeout_s: float = 10 * 60,
        golden: str = "wait",
    ) -> PreflightCoverage | None:
        """Put the images on every node of the pools their creates can land
        on, through ate-env-api's ``WarmImage`` (``FleetConfig.preflight``).

        The pools are the ones ``ListPlacementDomains`` reports for the base
        template (ate-env-api needs the global viewer binding for that:
        ``ate-env manifest access-policy --global``), narrowed per image to
        the ones its worker selector (``worker_selector_for``: the fleet's
        ``node_selector`` with the image's ``placement`` labels over it)
        matches. An image routed to a fixed template (``image_templates``)
        or not pinned by digest carries no rootfs image to warm and is
        skipped with a log line (the fleet leaves out tasks with
        ``Task.template`` before calling). Each matching pool bounds its own
        probes per node by its ``node_slots``: a pool with one slot per node
        gets one probe per node and does not lower another pool's. Returns the
        ``PreflightCoverage``, or None when nothing was left to warm; raises
        ``PreflightError`` when the pools cannot be listed (a transient
        failure is repeated twice first).

        ``golden`` is ``FleetConfig.preflight_golden``: ``wait`` waits for
        each image's golden snapshot and probes the image whether or not
        it got ready; ``skip`` probes at once and leaves the goldens
        baking; ``fail`` waits, does not probe an image whose golden is not
        ready, and raises ``PreflightError`` naming those images once the
        pass is over (the other images are on their nodes by then); the
        error's ``coverage`` attribute is the pass's ``PreflightCoverage``.
        A large image list goes in batches, each with its own golden wait
        of ``golden_timeout_s`` without progress; with ``fail`` the pass
        stops at the first batch that has an unready golden.

        A ``close()`` or ``close_async()`` of the driver ends the pre-flight
        with ``CancelledError``, at any point: in a request, in a sleep
        between two requests, or between two batches.
        """
        # The pass is a task of the driver's, so a close can cancel it
        # (_cancel_preflights); a cancel of the caller's task reaches it
        # through this await, which then waits for the pass to end its calls.
        task = asyncio.ensure_future(
            self._preflight_images_internal(
                images,
                observer=observer,
                run_id=run_id,
                probes_per_node=probes_per_node,
                image_concurrency=image_concurrency,
                max_rounds=max_rounds,
                golden_timeout_s=golden_timeout_s,
                probe_timeout_s=probe_timeout_s,
                golden=golden,
            )
        )
        # A pre-flight that starts while a close runs would dial a client
        # the close does not close. The check and the registration are one
        # step under the lock: a close of another thread either sees the
        # task and cancels it, or is seen here.
        with self._lock:
            closing = self._closing
            if not closing:
                self._preflights.add(task)
        if closing:
            task.cancel("the driver is closing")
        try:
            return await task
        finally:
            with self._lock:
                self._preflights.discard(task)

    async def _preflight_images_internal(
        self,
        images: Sequence[str],
        *,
        observer: Any,
        run_id: str,
        probes_per_node: int,
        image_concurrency: int,
        max_rounds: int,
        golden_timeout_s: float,
        probe_timeout_s: float,
        golden: str,
    ) -> PreflightCoverage | None:
        if golden not in ("wait", "skip", "fail"):
            raise ValueError(f"golden must be wait, skip or fail, got {golden!r}")
        pinned: list[str] = []
        skipped: list[str] = []
        for image in dict.fromkeys(images):
            if image in self.image_templates or not _DIGEST.search(image):
                skipped.append(image)
            else:
                pinned.append(image)
        if skipped:
            logger.info(
                "image pre-flight: %d images are not digest-pinned rootfs images and are not warmed: %s",
                len(skipped), ", ".join(skipped[:5]) + (", ..." if len(skipped) > 5 else ""),
            )
        if not pinned:
            return None
        template = self.default_template or "default-template"
        # The pool listing is repeated on the failures a control plane under
        # load hands out for a while (an api server restarting); a missing
        # binding or any other refusal is final.
        for attempt in range(1, 4):
            try:
                pools = await self._preflight_client().list_placement_domains(
                    template_name=template, atespace=self.atespace
                )
                break
            except PermissionDeniedError as e:
                raise PreflightError(
                    "image pre-flight (FleetConfig.preflight) needs ate-env-api's global viewer "
                    "binding to list the worker pools: apply `ate-env manifest access-policy "
                    f"--global` or run the pre-flight yourself with PreflightWarmer: {e}"
                ) from e
            except Exception as e:
                if attempt == 3 or not _transient(e):
                    raise PreflightError(
                        f"image pre-flight: listing the worker pools failed: {e}"
                    ) from e
                logger.warning("image pre-flight: listing the worker pools failed, repeating: %s", e)
                await asyncio.sleep(self.preflight_list_backoff * attempt)
        # The plan's domain names must be unique; ate-env-api keys a pool by
        # its namespace and name, so a name two namespaces share is prefixed.
        names: dict[tuple[str, str], str] = {}
        seen_names: dict[str, int] = {}
        for p in pools:
            seen_names[p.name] = seen_names.get(p.name, 0) + 1
        for p in pools:
            names[(p.worker_namespace, p.name)] = (
                p.name if seen_names[p.name] == 1 else f"{p.worker_namespace}/{p.name}"
            )
        by_key: dict[tuple[str, str], PlacementDomain] = {}
        entries: list[PlacementEntry] = []
        for image in pinned:
            selector = self.worker_selector_for(image) or {}
            hit = _pools_with_nodes(pools, selector)
            if not hit:
                logger.warning(
                    "image pre-flight: no pool of template %s with active nodes matches the "
                    "selector %s of %s; its creates would not place either",
                    template, selector, image,
                )
                continue
            for p in hit:
                key = (p.worker_namespace, p.name)
                if key not in by_key:
                    by_key[key] = PlacementDomain(
                        name=names[key],
                        worker_namespace=p.worker_namespace,
                        selector=p.selector,
                        nodes=p.nodes,
                        workers=p.workers,
                        node_slots=p.node_slots,
                    )
            entries.append(
                PlacementEntry(image, tuple(names[(p.worker_namespace, p.name)] for p in hit))
            )
        if not entries:
            return None
        domains = tuple(by_key.values())
        # The warmer's run id carries the domain and image indexes within
        # ate-env-api's 20 characters, so a large image list goes in batches
        # whose index fits; the batches' coverages are merged into one.
        try:
            batch, batch_ids = preflight_batches(
                len(domains), len(entries), run_id, self.preflight_batch_images
            )
        except ValueError as e:
            raise PreflightError(f"image pre-flight: {e}") from e
        coverages: list[PreflightCoverage] = []
        left_out = 0
        for b, start in enumerate(range(0, len(entries), batch)):
            plan = ImagePlacement(domains, tuple(entries[start : start + batch]))
            try:
                # The warmer bounds the probes per domain by its node_slots.
                warmer = PreflightWarmer(
                    self._preflight_client(),
                    plan,
                    atespace=self.atespace,
                    template_name=template,
                    run_id=batch_ids[b],
                    probes_per_node=probes_per_node,
                    image_concurrency=image_concurrency,
                    max_rounds=max_rounds,
                    golden_timeout=golden_timeout_s,
                    golden_wait=golden != "skip",
                    probe_without_golden=golden != "fail",
                    probe_timeout=probe_timeout_s,
                    observer=observer,
                    backend=self.backend_name,
                )
            except ValueError as e:
                raise PreflightError(f"image pre-flight: {e}") from e
            coverages.append(await warmer.run())
            if golden == "fail" and any(not g.settled for g in coverages[-1].goldens):
                # The run is refused already. Each batch has its own golden
                # wait, so the later batches would only add theirs.
                left_out = len(entries) - (start + len(plan.images))
                break
        coverage = merge_preflight_coverages(coverages)
        if run_id:
            # The caller's name for the pass, not the first batch's.
            coverage.run_id = run_id
        if golden == "fail" and coverage is not None:
            unready = [g for g in coverage.goldens if not g.settled]
            if unready:
                first = "; ".join(
                    f"{g.image}: {g.state or 'unknown'}{' (' + g.error + ')' if g.error else ''}"
                    for g in unready[:3]
                )
                raise PreflightError(
                    f"image pre-flight: {len(unready)} of {len(coverage.goldens)} golden snapshots "
                    f"are not ready and preflight_golden is \"fail\": {first}"
                    + ("; ..." if len(unready) > 3 else "")
                    + (
                        f"; {left_out} more images were not warmed, the pass stopped at the "
                        "first batch with an unready golden"
                        if left_out
                        else ""
                    ),
                    # The whole report, for a caller that wants every image.
                    coverage=coverage,
                )
        return coverage

    def preflight_images(self, images: Sequence[str], **kwargs: Any) -> PreflightCoverage | None:
        return self._sync(self.preflight_images_async(images, **kwargs))


    def worker_selector_for(self, rootfs_image: str | None) -> dict[str, str] | None:
        """The worker selector a create for rootfs_image sends: the fleet-wide
        labels with the image's placement labels merged over them, or None
        when there are none, which leaves the template's selection alone."""
        selector = dict(self.worker_selector)
        if rootfs_image:
            selector.update(self.placement.get(rootfs_image, {}))
        return selector or None

    def ensure_template(self, env: EnvironmentSpec | str) -> str:
        """Resolve ActorTemplate name from template ID or task image.

        Note: ResourceLimits, runtime_bundle, snapshot_storage_uri, and placement
        fields in EnvironmentSpec are reserved for future scheduling extensions.
        Current Substrate environments are provisioned from pre-registered ActorTemplates.
        """
        with start_span(self.tracer, f"{CLIENT_SPAN_PREFIX}.ensure_template") as span:
            template = self._resolve_template(env)
            set_attributes(span, {"ate.template.name": template})
            return template

    def _resolve_template(self, env: EnvironmentSpec | str) -> str:
        if isinstance(env, str):
            image = env
            if image in self.image_templates:
                return self.image_templates[image]
            return self.default_template or "default-template"

        image = env.image or ""
        mapped = self.image_templates.get(image)
        if mapped:
            return mapped
        if self.default_template:
            return self.default_template
        if isinstance(env, EnvironmentSpec):
            return env.template_key()
        return "default-template"

    def _make_instance(
        self, actor_id: str, template_id: str, run_id: str, rootfs_image: str | None = None
    ) -> RawSandboxInstance:
        planes = {
            "ate_env": DataPlaneEndpoint(
                protocol="grpc",
                address=self.api_endpoint,
                headers={"x-env-id": actor_id, "x-env-atespace": self.atespace},
            ),
        }
        metadata = {
            "created_at": time.time(),
            "template": template_id,
            "atespace": self.atespace,
        }
        if rootfs_image:
            metadata["rootfs_image"] = rootfs_image
        if self.labels:
            metadata["labels"] = dict(self.labels)
        return RawSandboxInstance(
            instance_id=actor_id,
            endpoint=self.api_endpoint,
            template_id=template_id,
            run_id=run_id,
            metadata=metadata,
            data_planes=planes,
            rootfs_image=rootfs_image,
        )

    async def acquire_async(
        self,
        template_id: str,
        run_id: str,
        timeout_s: float = 180.0,
        rootfs_image: str | None = None,
    ) -> RawSandboxInstance:
        """Acquire a live sandbox instance within timeout_s.

        An acquire that times out or is cancelled while its create() is in
        flight leaves the environment, if the create landed, to the driver:
        the next reap of any run deletes it (AC-15.2). One cancelled while
        it checks a warm actor returns that actor to the warm pool, or
        deletes it if a teardown ran since the claim (AC-15.7).
        """
        to_s = timeout_s or self.acquire_timeout_s
        slot_wait_s = current_slot_wait_s.get() or 0.0
        drv_token = in_driver_span.set(True)
        try:
            with start_span(
                self.tracer,
                f"{CLIENT_SPAN_PREFIX}.acquire",
                {"ate.template.name": template_id, "ate_env.run_id": run_id},
            ) as span:
                try:
                    with metrics.timed("acquire"):
                        instance = await asyncio.wait_for(
                            self._acquire_internal(template_id, run_id, rootfs_image=rootfs_image),
                            timeout=to_s,
                        )
                except asyncio.TimeoutError as e:
                    self._observe("claim", "error")
                    metrics.observe_subphase(
                        "slot_wait", slot_wait_s, status="error", source="direct"
                    )
                    raise SandboxStartError(
                        f"Timed out acquiring sandbox for template {template_id} after {to_s:g}s"
                    ) from e
                except BaseException as e:
                    st = metrics.status_of(e)
                    self._observe("claim", st)
                    metrics.observe_subphase("slot_wait", slot_wait_s, status=st, source="direct")
                    raise
                self._observe("claim", "ok")
                claim_source = str(instance.metadata.get("claim_source", "direct"))
                metrics.observe_subphase(
                    "slot_wait", slot_wait_s, status="ok", source=claim_source
                )
                set_attributes(span, {"ate.actor.name": instance.instance_id})
                return instance
        finally:
            in_driver_span.reset(drv_token)

    async def _acquire_internal(
        self,
        template_id: str,
        run_id: str,
        rootfs_image: str | None = None,
    ) -> RawSandboxInstance:
        """Acquire or claim an environment.

        Note on readiness: Substrate resumes suspended environments implicitly on first
        guest traffic, so ready_timeout_s does not measure readiness here.
        """
        pool_key = (template_id, rootfs_image)
        claim_source = "cold_create"
        # _teardowns as read on entry: a warm claim paused in get() across
        # its own run's reap compares against it on every later pass.
        entry_teardowns: int | None = None
        while True:
            actor_id: str | None = None
            with self._lock:
                teardowns = self._teardowns
                if entry_teardowns is None:
                    entry_teardowns = teardowns
                warm_list = self._warm_paused_pool.get(pool_key)
                if warm_list:
                    actor_id = warm_list.pop()

            if actor_id:
                claim_source = "warm_pool"
                self._observe("warm_remove", 1)
                claim_start = time.monotonic()
                logger.info(
                    "Claimed warm paused actor %s for template %s (rootfs_image=%s)",
                    actor_id,
                    template_id,
                    rootfs_image,
                )
                # Verify warm actor still exists on the server
                wc_token = current_warm_claim.set(True)
                try:
                    await asyncio.wait_for(
                        self._client_for(actor_id).get(actor_id, atespace=self.atespace),
                        timeout=min(self.ready_timeout_s, 15.0),
                    )
                except NotFoundError:
                    logger.warning(
                        "Warm actor %s not found on server, dropping from pool", actor_id
                    )
                    with self._lock:
                        self._owned_actors.pop(actor_id, None)
                    continue
                except Exception as e:
                    # Put back on unexpected error and raise; the claim above
                    # counted it out of the warm pool, so count it back in.
                    # After a reap the pool is gone: delete the actor instead.
                    with self._lock:
                        put_back = self._may_pool(teardowns)
                        if put_back:
                            self._warm_paused_pool.setdefault(pool_key, []).append(actor_id)
                            stale = None
                        else:
                            # Still owned (a claim does not pop it): the delete
                            # below removes it, and a reap can still see it.
                            stale = self._owned_actors.get(actor_id)
                    if put_back:
                        self._observe("warm_add", 1)
                    elif stale is not None:
                        await self._delete_unpooled(actor_id, "after teardown")
                    metrics.observe_subphase(
                        "wire_claim",
                        time.monotonic() - claim_start,
                        status="error",
                        source="warm_pool",
                    )
                    raise SandboxStartError(f"Error checking warm actor {actor_id}: {e}") from e
                except BaseException:
                    # Cancelled in get() (by acquire's timeout_s, the caller,
                    # or close()): the actor is out of the pool and no caller
                    # will hold it (AC-15.7). Put it back, as above. After a
                    # teardown the pool is gone, and the actor may carry the
                    # run id of an earlier run (a recycled one), which no
                    # later reap matches: own it under WARM_POOL_RUN_ID and
                    # delete it in a background task, so the cancel is not
                    # delayed (reap and close() join it).
                    with self._lock:
                        put_back = self._may_pool(teardowns)
                        stale = None
                        if put_back:
                            self._warm_paused_pool.setdefault(pool_key, []).append(actor_id)
                        else:
                            stale = self._owned_actors.get(actor_id)
                            if stale is not None:
                                self._own_for_any_reap(actor_id, stale)
                    if put_back:
                        self._observe("warm_add", 1)
                    elif stale is not None and _loop_running():
                        self._spawn_background_delete(actor_id, "after a cancelled warm claim")
                    metrics.observe_subphase(
                        "wire_claim",
                        time.monotonic() - claim_start,
                        status="error",
                        source="warm_pool",
                    )
                    raise
                finally:
                    current_warm_claim.reset(wc_token)
                instance = self._make_instance(
                    actor_id, template_id, run_id, rootfs_image=rootfs_image
                )
                instance.metadata["claim_source"] = claim_source
                # Check for a reap and take ownership in one lock hold, so a
                # reap on another thread cannot run in between (AC-15.7).
                with self._lock:
                    reaped_since = not self._may_pool(teardowns)
                    if reaped_since:
                        stale = self._owned_actors.get(actor_id)
                    elif self._run_reaped_since(run_id, entry_teardowns):
                        self._own_for_any_reap(actor_id, instance)
                    else:
                        self._owned_actors[actor_id] = instance
                if reaped_since:
                    # A reap ran during get(). It deleted the actor if it
                    # matched it (the actor's run, or WARM_POOL_RUN_ID); one of
                    # another run, or a reap cancelled before its delete, left
                    # it alive and unpooled. Do not hand it out; delete it if
                    # it is still owned, then claim or create another.
                    logger.info(
                        "Teardown ran while claiming warm actor %s; not handing it out", actor_id
                    )
                    # One wire_claim sample for the abandoned claim, as for a
                    # failed one; the claim that follows records its own.
                    metrics.observe_subphase(
                        "wire_claim",
                        time.monotonic() - claim_start,
                        status="error",
                        source="warm_pool",
                    )
                    if stale is not None:
                        await self._delete_unpooled(actor_id, "after teardown")
                    continue
                wire_dur = time.monotonic() - claim_start
                metrics.observe("warm_claim", wire_dur)
                metrics.observe_subphase("wire_claim", wire_dur, status="ok", source="warm_pool")
                return instance
            else:
                claim_source = "cold_create"
                actor_id = f"sb-{uuid.uuid4().hex[:16]}"
                logger.info(
                    "Creating environment %s (template=%s, atespace=%s, rootfs_image=%s)...",
                    actor_id,
                    template_id,
                    self.atespace,
                    rootfs_image,
                )
                wire_start = time.monotonic()
                last_err: Exception | None = None
                try:
                    for attempt in range(4):
                        try:
                            await self._client_for(actor_id).create(
                                actor_id,
                                template_name=template_id,
                                atespace=self.atespace,
                                rootfs_image=rootfs_image,
                                worker_selector=self.worker_selector_for(rootfs_image),
                            )
                            last_err = None
                            break
                        except (NotFoundError, FailedPreconditionError) as e:
                            metrics.observe_subphase(
                                "wire_claim",
                                time.monotonic() - wire_start,
                                status="error",
                                source="cold_create",
                            )
                            raise SandboxStartError(
                                f"ActorTemplate '{template_id}' invalid or does not exist on Substrate server: {e}"
                            ) from e
                        except Exception as e:
                            last_err = e
                            if "already exists" in str(e).lower():
                                last_err = None
                                break
                            if attempt < 3:
                                await asyncio.sleep(0.05 * (attempt + 1))
                except BaseException as e:
                    if isinstance(e, Exception):
                        raise
                    # Cancelled (for example by acquire's timeout_s), or the
                    # loop is closing, with a create() sent: the server may
                    # have made the actor, and no caller will hold it
                    # (AC-15.2). One wire_claim sample, as for a failed create.
                    metrics.observe_subphase(
                        "wire_claim",
                        time.monotonic() - wire_start,
                        status="error",
                        source="cold_create",
                    )
                    self._own_abandoned_create(
                        actor_id, template_id, rootfs_image, entry_teardowns
                    )
                    raise
                if last_err is not None:
                    metrics.observe_subphase(
                        "wire_claim",
                        time.monotonic() - wire_start,
                        status="error",
                        source="cold_create",
                    )
                    raise SandboxStartError(
                        f"Failed to create environment {actor_id}: {last_err}"
                    ) from last_err
                metrics.observe_subphase(
                    "wire_claim",
                    time.monotonic() - wire_start,
                    status="ok",
                    source="cold_create",
                )
                break

        instance = self._make_instance(actor_id, template_id, run_id, rootfs_image=rootfs_image)
        instance.metadata["claim_source"] = claim_source
        with self._lock:
            if self._run_reaped_since(run_id, entry_teardowns):
                # The run's own reap overlapped this acquire (during an earlier
                # warm claim or this cold create) and may have missed the
                # actor, and a fleet's last teardown reaps its run once: own it
                # under WARM_POOL_RUN_ID, which every reap deletes. The
                # caller's handle keeps its run_id, and its release deletes the
                # actor as usual (AC-15.10).
                self._own_for_any_reap(actor_id, instance)
            else:
                self._owned_actors[actor_id] = instance
        return instance

    def acquire(
        self,
        template_id: str,
        run_id: str,
        timeout_s: float = 180.0,
        rootfs_image: str | None = None,
    ) -> RawSandboxInstance:
        return self._sync(
            self.acquire_async(
                template_id, run_id, timeout_s=timeout_s, rootfs_image=rootfs_image
            )
        )

    async def warm_pool_async(
        self,
        template_id: str,
        replicas: int,
        wait: bool = True,
        run_id: str | None = None,
        rootfs_image: str | None = None,
    ) -> None:
        """Pre-warm a pool of actors by creating and suspending them.

        Overlapping calls for one (template, image) share the work: a call
        counts the actors other calls are still creating and does not wait
        for them, so it can return before the pool is full.
        """
        with start_span(
            self.tracer,
            f"{CLIENT_SPAN_PREFIX}.warm_pool",
            {
                "ate.template.name": template_id,
                "ate_env.replicas": replicas,
                "ate_env.run_id": run_id,
            },
        ):
            await self._warm_pool_internal(
                template_id, replicas, wait=wait, run_id=run_id, rootfs_image=rootfs_image
            )

    async def _warm_pool_internal(
        self,
        template_id: str,
        replicas: int,
        wait: bool,
        run_id: str | None,
        rootfs_image: str | None,
    ) -> None:
        logger.info(
            "Sizing warm pool for template %s (rootfs_image=%s) to %d paused actors...",
            template_id,
            rootfs_image,
            replicas,
        )
        pool_key = (template_id, rootfs_image)
        with self._lock:
            teardowns = self._teardowns
            gen = self._pool_gens.get(pool_key, 0)
            reservation = (pool_key, teardowns, gen)
            current = len(self._warm_paused_pool.get(pool_key, []))
            inflight = self._warm_inflight.get(reservation, 0)
            needed = replicas - current - inflight
            if needed <= 0:
                return
            self._warm_inflight[reservation] = inflight + needed

        def _release_reservation() -> None:
            # Caller holds self._lock. A reap since then already dropped it.
            left = self._warm_inflight.get(reservation, 0) - needed
            if left > 0:
                self._warm_inflight[reservation] = left
            else:
                self._warm_inflight.pop(reservation, None)

        sem = asyncio.Semaphore(16)
        effective_run_id = run_id or WARM_POOL_RUN_ID
        # Every id a create() was sent for, so a cancelled call can still
        # account for the actors it made (AC-15.8).
        started: list[str] = []

        async def _warm_one() -> str | None:
            actor_id = f"warm-{uuid.uuid4().hex[:10]}"
            async with sem:
                # Woken by a call that close cut off, not cancelled itself:
                # no create on a channel that close has to open for it.
                self._stop_if_closing()
                started.append(actor_id)
                try:
                    drv_tok = in_driver_span.set(True)
                    try:
                        await self._client_for(actor_id).create(
                            actor_id,
                            template_name=template_id,
                            atespace=self.atespace,
                            rootfs_image=rootfs_image,
                            worker_selector=self.worker_selector_for(rootfs_image),
                        )
                    finally:
                        in_driver_span.reset(drv_tok)
                except (NotFoundError, FailedPreconditionError) as e:
                    raise SandboxStartError(
                        f"ActorTemplate '{template_id}' invalid or does not exist on Substrate server: {e}"
                    ) from e
                except Exception as e:
                    logger.warning("Failed to provision warm actor %s: %s", actor_id, e)
                    return None
                if wait:
                    # Created during a close: no suspend; the call's handler
                    # below owns the actor and deletes it.
                    self._stop_if_closing()
                    try:
                        await self._client_for(actor_id).suspend(actor_id, atespace=self.atespace)
                    except Exception as e:
                        # Created but not paused: neither pooled nor returned.
                        # Own it under WARM_POOL_RUN_ID and delete it, so a
                        # failed delete leaves it for any reap (AC-15.8).
                        logger.warning("Failed to suspend warm actor %s: %s", actor_id, e)
                        with self._lock:
                            self._owned_actors.setdefault(
                                actor_id,
                                self._make_instance(
                                    actor_id,
                                    template_id,
                                    run_id=WARM_POOL_RUN_ID,
                                    rootfs_image=rootfs_image,
                                ),
                            )
                        await self._delete_unpooled(actor_id, "after failed warm suspend")
                        return None
                else:
                    task = asyncio.create_task(
                        self._client_for(actor_id).suspend(actor_id, atespace=self.atespace)
                    )
                    self._background_tasks.add(task)

                    def _done(t: asyncio.Task[Any]) -> None:
                        self._background_tasks.discard(t)
                        if not t.cancelled() and t.exception():
                            logger.warning(
                                "Background suspend for warm actor %s failed: %s",
                                actor_id,
                                t.exception(),
                            )

                    task.add_done_callback(_done)
                return actor_id

        tasks = [_warm_one() for _ in range(needed)]
        try:
            results = await asyncio.gather(*tasks, return_exceptions=True)
            for res in results:
                if isinstance(res, BaseException) and not isinstance(res, Exception):
                    # A create() or suspend() was cancelled under a call that
                    # was not: the channel closed (close_async on the caller's
                    # loop). gather hands that back as a result; treat it as
                    # this call's own cancel, so the actors are accounted for.
                    raise res
        except BaseException:
            # Cancelled, or one actor's call was: the actors created so far,
            # and any whose create() was in flight, are neither pooled nor
            # owned. Own them under WARM_POOL_RUN_ID, so any run's reap
            # deletes them (one that was never created is NotFound there,
            # which counts as gone).
            with self._lock:
                _release_reservation()
                reaped_since = not self._may_pool(teardowns)
                for aid in started:
                    self._owned_actors.setdefault(
                        aid,
                        self._make_instance(
                            aid, template_id, run_id=WARM_POOL_RUN_ID, rootfs_image=rootfs_image
                        ),
                    )
            if reaped_since and _loop_running():
                # A reap ran or is running and may have taken its snapshot
                # before this handler, and a fleet reaps its run once; or
                # close() started, and no reap follows: delete them now, in
                # background tasks so the cancel is not delayed, as the
                # wait=False suspends do (close() joins them before its
                # channels close). A delete that fails or never runs leaves
                # them for any later reap (AC-15.8).
                for aid in started:
                    self._spawn_background_delete(aid, "after a cancelled warm_pool")
            raise

        created = [r for r in results if isinstance(r, str)]
        # The reservation turns into pool entries under one lock, so a
        # concurrent warm_pool always counts these actors in one of the two.
        with self._lock:
            _release_reservation()
            # After a reap, unwarm_pool or a shrinking resize_pool, the pool
            # these actors were made for is gone: delete them instead.
            torn_down = not self._may_pool(teardowns)
            drained = self._pool_gens.get(pool_key, 0) != gen
            # Owned either way. Actors to delete are owned under
            # WARM_POOL_RUN_ID from the start: the run's reap may already
            # have run, and a cancel can land before any delete below starts
            # (AC-15.9).
            owner = WARM_POOL_RUN_ID if (torn_down or drained) else effective_run_id
            for aid in created:
                self._owned_actors[aid] = self._make_instance(
                    aid, template_id, run_id=owner, rootfs_image=rootfs_image
                )
                if not (torn_down or drained):
                    self._warm_paused_pool.setdefault(pool_key, []).append(aid)
        if torn_down or drained:
            if created:
                logger.info(
                    "%s ran while warming %d actors for template %s; deleting them",
                    "Teardown" if torn_down else "unwarm_pool or resize_pool",
                    len(created),
                    template_id,
                )
            why = "after teardown" if torn_down else "after unwarm_pool or resize_pool"
            try:
                await asyncio.gather(*(self._delete_unpooled(aid, why) for aid in created))
            except BaseException:
                # A delete that close() cancelled before its first step ran
                # no handler of its own.
                for aid in created:
                    self._delete_unreaped_in_close(aid)
                raise
        else:
            self._observe("warm_add", len(created))

        for res in results:
            if isinstance(res, SandboxStartError):
                raise res
            if isinstance(res, Exception):
                logger.warning("Error provisioning warm actor: %s", res)

    def _spawn_background_delete(self, actor_id: str, why: str) -> None:
        """Delete ``actor_id`` in a background task that a reap can join.

        A cancel handler calls it: the delete must not delay the cancel, and
        the reap that is running, or the next one, waits for it
        (_wait_background_deletes). A delete that fails, or is cancelled,
        leaves the actor owned under WARM_POOL_RUN_ID and its id in
        _failed_background_deletes, so that join reports it; one that never
        runs leaves the actor for any later reap.
        """
        task = asyncio.create_task(self._background_delete(actor_id, why))
        with self._lock:
            self._background_tasks.add(task)
            self._background_deletes[task] = actor_id

        def _done(t: asyncio.Task[Any]) -> None:
            with self._lock:
                self._background_tasks.discard(t)
                self._background_deletes.pop(t, None)
                if t.cancelled():
                    # A task cancelled before its first step never entered
                    # _background_delete, so its finally recorded nothing;
                    # one cancelled later recorded the actor already.
                    self._failed_background_deletes[actor_id] = None

        task.add_done_callback(_done)

    async def _background_delete(self, actor_id: str, why: str) -> None:
        """One background delete. If it does not land (the delete failed, or
        the task is cancelled), records the actor in _failed_background_deletes
        before the task ends, so a join that finds the task done sees the
        outcome too."""
        gone = False
        try:
            gone = await self._delete_unpooled(actor_id, why, background=True)
        finally:
            if not gone:
                with self._lock:
                    self._failed_background_deletes[actor_id] = None

    def _own_abandoned_create(
        self, actor_id: str, template_id: str, rootfs_image: str | None, teardowns: int
    ) -> None:
        """Own an actor whose cold create() was cancelled in flight.

        As a cancelled warm_pool does (AC-15.8): own it under
        WARM_POOL_RUN_ID, so any run's reap deletes it (one whose create never
        landed is NotFound there, which counts as gone). If a reap ran since
        the caller read ``teardowns``, that reap may have taken its snapshot
        first, and if close() started, no reap follows: delete it now too, in
        a background task so the cancel is not delayed (close() joins it). A
        delete that fails or never runs leaves it for any later reap.
        """
        with self._lock:
            reaped_since = not self._may_pool(teardowns)
            self._owned_actors.setdefault(
                actor_id,
                self._make_instance(
                    actor_id, template_id, run_id=WARM_POOL_RUN_ID, rootfs_image=rootfs_image
                ),
            )
        if reaped_since and _loop_running():
            self._spawn_background_delete(actor_id, "after a cancelled acquire")

    def _stop_if_closing(self) -> None:
        """Raise CancelledError while close() or close_async() runs.

        For a call that waited on a semaphore, at the point it gets it.
        close_async() on the caller's loop cancels no task: closing the
        channels cuts off the calls in flight, which frees the semaphore for
        the ones that waited, and those would send their create() or delete()
        on a client dialed for them, as a foreground call that close does not
        join. Stopped here, they take the same path as the calls that were
        cut off: the cancel handler starts the background delete.
        """
        with self._lock:
            closing = self._closing
        if closing:
            raise asyncio.CancelledError("the driver is closing")

    def _may_pool(self, teardowns: int) -> bool:
        """Whether an actor may enter the pool: no reap ran, and no close()
        started, since the caller read ``teardowns``, and no reap is running.
        Caller holds self._lock."""
        return self._reaping == 0 and self._teardowns == teardowns

    async def _delete_unpooled(self, actor_id: str, why: str, *, background: bool = False) -> bool:
        """Delete an owned actor that is not in the pool and that this run's
        reap may already have missed (``why`` goes in the log).

        Returns whether the actor is gone: deleted, or not found. The actor
        leaves _owned_actors only then. A failed delete is counted as a
        release failure and returns False; a failed or cancelled one re-tags
        the actor with WARM_POOL_RUN_ID, so whichever run reaps next deletes
        it (AC-15.5, AC-15.9). One that close() or close_async() cancels has
        no reap after it: it starts a background delete, which close joins,
        unless it is one itself (``background``).
        """
        try:
            await self._client_for(actor_id).delete(actor_id, atespace=self.atespace)
        except NotFoundError:
            pass
        except Exception as e:
            self._observe("release_failed")
            logger.warning("Failed deleting actor %s %s: %s", actor_id, why, e)
            self._keep_for_any_reap(actor_id)
            return False
        except BaseException:
            # Cancelled: the run's reap may already have run, so leave the
            # actor where any later reap finds it, then let the cancel through.
            # Cancelled by close: delete it in the background, before the
            # channels close.
            if self._keep_for_any_reap(actor_id) and not background and _loop_running():
                self._spawn_background_delete(actor_id, f"{why}, cancelled by close")
            raise
        with self._lock:
            self._owned_actors.pop(actor_id, None)
        return True

    def _return_popped(self, actor_id: str, instance: RawSandboxInstance, teardowns: int) -> None:
        """Own again an actor that release popped and did not delete.

        With no teardown since the pop (one read ``teardowns``), under its
        own run id, for the run's reap. After a reap, during one, or during
        close() or close_async(): that reap could not see the actor, a fleet
        reaps its run once, and no reap follows a close. Then own it under
        WARM_POOL_RUN_ID and delete it in a background task, as a cancelled
        acquire does (_own_abandoned_create); the reap that is running, or
        close, joins it. A delete that fails or never runs leaves the actor
        for any later reap (AC-15.9). The check and the update share one lock
        hold, so a reap that starts in between cannot miss the actor. A copy,
        so the caller's handle keeps its run_id.
        """
        with self._lock:
            torn_down = self._closing or not self._may_pool(teardowns)
            if torn_down:
                self._own_for_any_reap(actor_id, instance)
            else:
                self._owned_actors.setdefault(actor_id, instance)
        if torn_down and _loop_running():
            self._spawn_background_delete(actor_id, "after a release lost across a teardown")

    def _delete_unreaped_in_close(self, actor_id: str) -> None:
        """During close() or close_async(), delete in the background an actor
        that a reap, or a call that deletes unpooled actors, has not deleted.

        close() cancels a reap still in flight on its own runner (another
        thread's teardown), and closing the channels cuts off the deletes of
        one on the caller's loop; the same goes for an unwarm_pool, a
        shrinking resize_pool, and the deletes of a warm_pool call that
        finished after a teardown. A task cancelled while it waited on the
        call's semaphore, or before its first step, started no delete of its
        own. No reap follows a close, so the actor, if still owned, goes
        under WARM_POOL_RUN_ID and to a background delete, which close joins
        before its channels close. One delete per actor: an actor whose
        background delete is pending gets no second one. Outside a close it
        does nothing: a cancelled call leaves its actors owned as they were,
        for the next reap.
        """
        with self._lock:
            inst = self._owned_actors.get(actor_id)
            if not self._closing or inst is None:
                return
            if any(
                aid == actor_id and not t.done() for t, aid in self._background_deletes.items()
            ):
                return
            self._own_for_any_reap(actor_id, inst)
        if _loop_running():
            self._spawn_background_delete(actor_id, "after a call cut off by close")

    def _keep_for_any_reap(self, actor_id: str) -> bool:
        """Keep an owned actor under WARM_POOL_RUN_ID, which every reap deletes.

        An actor a reap already popped is not added back. Returns whether
        the actor is still owned and close() or close_async() is running:
        then no reap follows, and the caller deletes it in the background.
        """
        with self._lock:
            inst = self._owned_actors.get(actor_id)
            if inst is not None:
                self._own_for_any_reap(actor_id, inst)
            return inst is not None and self._closing

    def _own_for_any_reap(self, actor_id: str, instance: RawSandboxInstance) -> None:
        """Own ``instance`` under WARM_POOL_RUN_ID. Caller holds self._lock.

        A copy, so the caller's handle keeps its run_id.
        """
        self._owned_actors[actor_id] = dataclasses.replace(instance, run_id=WARM_POOL_RUN_ID)

    def warm_pool(
        self,
        template_id: str,
        replicas: int,
        wait: bool = True,
        run_id: str | None = None,
        rootfs_image: str | None = None,
    ) -> None:
        self._sync(
            self.warm_pool_async(
                template_id,
                replicas,
                wait=wait,
                run_id=run_id,
                rootfs_image=rootfs_image,
            )
        )

    async def resize_pool_async(
        self,
        template_id: str,
        replicas: int,
        wait: bool = True,
        rootfs_image: str | None = None,
    ) -> None:
        """Set the warm pool of (template, image) to exactly ``replicas`` paused
        actors: grow through warm_pool_async, shrink by deleting idle paused
        actors. Claimed actors are not in the pool and are never touched, so
        ``held + warm`` stays what the caller asked for (the shape Tunix's
        prewarm loop keeps with set_pool_replicas). A resize below what is
        pooled plus what warm_pool calls are still creating cancels those
        calls: they delete their actors instead of pooling them."""
        replicas = max(0, replicas)
        pool_key = (template_id, rootfs_image)
        with self._lock:
            pool = self._warm_paused_pool.get(pool_key, [])
            current = len(pool)
            reservation = (pool_key, self._teardowns, self._pool_gens.get(pool_key, 0))
            if replicas < current + self._warm_inflight.get(reservation, 0):
                self._cancel_warm_calls([pool_key])
            if replicas >= current:
                surplus = []
            else:
                surplus = pool[replicas:]
                del pool[replicas:]
                self._own_drained_for_any_reap(surplus)
        if replicas > current:
            await self.warm_pool_async(
                template_id, replicas, wait=wait, rootfs_image=rootfs_image
            )
            return
        if not surplus:
            return
        # The actors left the pool above; the shared warm-replica gauge and the
        # run report follow, as in unwarm_pool_async.
        self._observe("warm_remove", len(surplus))
        logger.info(
            "Shrinking warm pool for template %s (rootfs_image=%s) from %d to %d paused actors",
            template_id, rootfs_image, current, replicas,
        )
        await self._delete_drained(surplus, "shrinking the warm pool")

    def resize_pool(
        self,
        template_id: str,
        replicas: int,
        wait: bool = True,
        rootfs_image: str | None = None,
    ) -> None:
        self._sync(
            self.resize_pool_async(template_id, replicas, wait=wait, rootfs_image=rootfs_image)
        )

    def pool_size(self, template_id: str, rootfs_image: str | None = None) -> int | None:
        """Paused actors waiting in the warm pool of (template_id, rootfs_image)."""
        with self._lock:
            return len(self._warm_paused_pool.get((template_id, rootfs_image), []))

    async def unwarm_pool_async(self, template_id: str, rootfs_image: str | None = None) -> None:
        """Drain and delete paused actors in the warm pool.

        warm_pool calls for the same pools that are still creating actors
        are cancelled: they delete their actors instead of pooling them.
        """
        logger.info(
            "Unwarming pool for template %s (rootfs_image=%s)...", template_id, rootfs_image
        )
        with self._lock:
            if rootfs_image is not None:
                keys_to_pop = [(template_id, rootfs_image)]
            else:
                pending = [r[0] for r in self._warm_inflight]
                keys_to_pop = [
                    k
                    for k in dict.fromkeys([*self._warm_paused_pool.keys(), *pending])
                    if (isinstance(k, tuple) and k[0] == template_id)
                ]
            to_delete = []
            for k in keys_to_pop:
                to_delete.extend(self._warm_paused_pool.pop(k, []))
            self._own_drained_for_any_reap(to_delete)
            self._cancel_warm_calls(keys_to_pop)
        self._observe("warm_remove", len(to_delete))
        await self._delete_drained(to_delete, "unwarming the pool")

    def _cancel_warm_calls(self, pool_keys: list[tuple[str, str | None]]) -> None:
        """Start a new generation of these pools, so warm_pool calls in
        flight delete what they create. Caller holds self._lock."""
        for k in pool_keys:
            self._pool_gens[k] = self._pool_gens.get(k, 0) + 1

    def _own_drained_for_any_reap(self, actor_ids: list[str]) -> None:
        """Own under WARM_POOL_RUN_ID the actors unwarm_pool or resize_pool
        just took out of the pool. Caller holds self._lock, since the pop.

        In the pool, any run's reap deleted them. Out of it they would carry
        the run id warm_pool or a recycle left on them, which may be an
        earlier run's: a drain cancelled before an actor's delete started
        (it waited on the semaphore, or its task never took a step) would
        leave it to that run's reap alone.
        """
        for aid in actor_ids:
            inst = self._owned_actors.get(aid)
            if inst is not None:
                self._own_for_any_reap(aid, inst)

    async def _delete_drained(self, actor_ids: list[str], why: str) -> None:
        """Delete actors just taken out of the pool, 16 at a time.

        Cut off by close() or close_async(), it starts a background delete
        for each actor it had not deleted, as a reap does
        (_delete_unreaped_in_close): no reap follows a close.
        """
        sem = asyncio.Semaphore(16)

        async def _delete_one(actor_id: str) -> None:
            try:
                async with sem:
                    # Woken by a delete that close cut off, not cancelled
                    # itself: to the background, as theirs go.
                    self._stop_if_closing()
                    await self._delete_unpooled(actor_id, why)
            except BaseException:
                # Cancelled while it waited for the semaphore, or stopped
                # above; one cancelled in delete() has its delete already.
                self._delete_unreaped_in_close(actor_id)
                raise

        try:
            await asyncio.gather(*[_delete_one(aid) for aid in actor_ids], return_exceptions=True)
        except BaseException:
            # A _delete_one that close() cancelled before its first step ran
            # no handler of its own.
            for aid in actor_ids:
                self._delete_unreaped_in_close(aid)
            raise

    def unwarm_pool(self, template_id: str, rootfs_image: str | None = None) -> None:
        self._sync(self.unwarm_pool_async(template_id, rootfs_image=rootfs_image))

    async def release_async(self, instance_id: str, recycle: bool = False) -> None:
        """Release actor: deletes environment, or suspends if recycling.

        Note: Recycling suspends the actor and returns it to the warm pool
        without resetting guest filesystem or processes.

        A failed delete does not raise, so a cleanup error never stops a
        trainer, but it is not hidden either: the ``ate_env_client.release``
        span and the ``ate_env_client_release_seconds`` sample get status
        error, the fleet observer counts it
        (``ate_env_fleet_release_failures_total``, ``RunReport.release_errors``),
        and the environment stays owned by this driver so the run's ``reap``
        tries again and raises if it still cannot delete it. An environment
        that is already gone (NotFound) counts as released.
        """
        with start_span(
            self.tracer, f"{CLIENT_SPAN_PREFIX}.release", {"ate.actor.name": instance_id}
        ) as span:
            try:
                with metrics.timed("release"):
                    await self._release_internal(instance_id, recycle)
            # Exception, not BaseException: a cancellation (CancelledError)
            # must still propagate. _release_internal catches BaseException
            # only to keep the environment owned for reap, then re-raises.
            except Exception as e:
                # Only the error type reaches the trace; the message can carry
                # bucket paths and other server details.
                mark_error(span, f"release failed: {type(e).__name__}")
                set_attributes(span, {"error.type": type(e).__name__})
                self._observe("release_failed")
                logger.warning("Failed to delete environment %s: %s", instance_id, e)

    async def _release_internal(self, instance_id: str, recycle: bool) -> None:
        """Suspend into the warm pool or delete; raises when the delete fails."""
        with self._lock:
            instance = self._owned_actors.pop(instance_id, None)
            teardowns = self._teardowns

        if recycle and instance:
            try:
                await self._client_for(instance_id).suspend(instance_id, atespace=self.atespace)
                pool_key = (instance.template_id, instance.rootfs_image)
                with self._lock:
                    pooled = self._may_pool(teardowns)
                    if pooled:
                        self._warm_paused_pool.setdefault(pool_key, []).append(instance_id)
                        self._owned_actors[instance_id] = instance
                if pooled:
                    self._observe("warm_add", 1)
                    logger.info("Recycled actor %s to paused warm pool", instance_id)
                    return
                # The reap could not see this actor (it was popped above), so
                # pooling it now would outlive the teardown: delete it below.
                logger.info("Teardown ran while recycling actor %s; deleting it", instance_id)
            except Exception as e:
                logger.warning(
                    "Failed to suspend actor %s for recycle, deleting: %s", instance_id, e
                )
            except BaseException:
                # Cancelled during suspend(): own it again, for the run's
                # reap; after a teardown since the pop, delete it.
                self._return_popped(instance_id, instance, teardowns)
                raise

        try:
            await self._client_for(instance_id).delete(instance_id, atespace=self.atespace)
        except NotFoundError:
            logger.info("Environment %s was already deleted", instance_id)
            return
        except BaseException:
            # Own it again, so the run's reap retries the delete. After a
            # teardown since the pop (the recycle saw it, or it ran during
            # this delete) no reap of the run follows: a background delete
            # retries it, and the actor stays owned for any reap.
            if instance is not None:
                self._return_popped(instance_id, instance, teardowns)
            raise
        logger.info("Deleted environment %s", instance_id)

    def release(self, instance_id: str, recycle: bool = False) -> None:
        self._sync(self.release_async(instance_id, recycle=recycle))

    async def reap_async(self, run_id: str) -> int:
        """Force delete all environments tagged with this run_id and all warm pool actors."""
        with start_span(self.tracer, f"{CLIENT_SPAN_PREFIX}.reap", {"ate_env.run_id": run_id}):
            return await self._reap_internal(run_id)

    async def _reap_internal(self, run_id: str) -> int:
        with self._lock:
            self._reaping += 1
            self._teardowns += 1
            self._warm_inflight.clear()
            self._run_reaps_running[run_id] = self._run_reaps_running.get(run_id, 0) + 1
        try:
            return await self._reap_actors(run_id)
        finally:
            # Work that started during the reap's own awaits also sees a
            # teardown, so nothing is pooled before the fleet resets the gauge.
            with self._lock:
                self._reaping -= 1
                self._teardowns += 1
                self._warm_inflight.clear()
                left = self._run_reaps_running.get(run_id, 1) - 1
                if left > 0:
                    self._run_reaps_running[run_id] = left
                else:
                    self._run_reaps_running.pop(run_id, None)
                self._run_reap_ended[run_id] = self._teardowns

    def _run_reaped_since(self, run_id: str, teardowns: int) -> bool:
        """Whether a reap of ``run_id`` is running, or ended after the caller
        read ``teardowns``: that reap may have taken its snapshot before an
        actor the caller is about to own. Caller holds self._lock."""
        return (
            self._run_reaps_running.get(run_id, 0) > 0
            or self._run_reap_ended.get(run_id, -1) > teardowns
        )

    async def _reap_actors(self, run_id: str) -> int:
        with self._lock:
            warm_actors: list[str] = []
            for tid, actors in list(self._warm_paused_pool.items()):
                warm_actors.extend(actors)
            self._warm_paused_pool.clear()

            to_reap = [
                aid
                for aid, inst in self._owned_actors.items()
                if inst.run_id == run_id or aid in warm_actors or inst.run_id == WARM_POOL_RUN_ID
            ]
            to_reap = list(dict.fromkeys(to_reap + warm_actors))
        # The pool is empty now; the gauge follows, so a direct reap (not
        # through a fleet, which also resets it) keeps the two equal (P-9).
        if warm_actors:
            self._observe("warm_remove", len(warm_actors))

        sem = asyncio.Semaphore(16)
        failed_actors: list[str] = []
        reaped_count = 0
        reap_lock = asyncio.Lock()

        async def _reap_one(aid: str) -> None:
            nonlocal reaped_count
            try:
                async with sem:
                    # Woken by a delete that close cut off, not cancelled
                    # itself: its delete goes to the background as theirs do
                    # (the handler below), where close joins it.
                    self._stop_if_closing()
                    try:
                        await self._client_for(aid).delete(aid, atespace=self.atespace)
                        async with reap_lock:
                            reaped_count += 1
                    except NotFoundError:
                        pass  # already gone (e.g. a release that timed out after the delete landed)
                    except Exception as e:
                        logger.warning("Failed deleting actor %s during reap: %s", aid, e)
                        async with reap_lock:
                            failed_actors.append(aid)
                    with self._lock:
                        self._owned_actors.pop(aid, None)
            except BaseException:
                # Cancelled in delete(), or while it waited for the semaphore.
                # By close(): no reap follows, so delete the actor in the
                # background, before the channels close. By anything else:
                # the actor stays owned as it was, for the next reap.
                self._delete_unreaped_in_close(aid)
                raise

        try:
            await asyncio.gather(*[_reap_one(aid) for aid in to_reap], return_exceptions=True)
        except BaseException:
            # A _reap_one that close() cancelled before its first step ran no
            # handler of its own.
            for aid in to_reap:
                self._delete_unreaped_in_close(aid)
            raise
        # A background delete that failed since the last join, or that still
        # runs at the deadline, has the same outcome for the caller as a
        # failed reap delete: the actor may be alive after teardown. Name it
        # in the error, so the caller can sweep it; it stays owned under
        # WARM_POOL_RUN_ID for a later reap.
        failed_background, late = await self._wait_background_deletes()
        failed_actors = list(dict.fromkeys(failed_actors + failed_background + late))

        if failed_actors:
            notes = []
            if failed_background:
                notes.append(f"{len(failed_background)} background delete(s) failed")
            if late:
                notes.append(f"{len(late)} still deleting in the background")
            detail = f" ({', '.join(notes)})" if notes else ""
            raise InfrastructureError(
                f"Failed to delete {len(failed_actors)} environments during reap: "
                f"{failed_actors}{detail}"
            )

        logger.info("Reaped %d Substrate environments for run %s", reaped_count, run_id)
        return reaped_count

    async def _wait_background_deletes(self) -> tuple[list[str], list[str]]:
        """Join the background deletes started so far, up to background_delete_timeout_s.

        Returns ``(failed, late)``: the ids of the actors whose background
        deletes failed, or were cancelled, since the last join and that are
        still owned (_take_failed_background_deletes), and the ids of those
        whose deletes still run at the deadline, in the order the deletes
        started. Both are logged. The late tasks are not cancelled:
        _delete_unpooled keeps their actors under WARM_POOL_RUN_ID, as it
        does the failed ones. With background_delete_timeout_s 0 every delete
        still pending after the settle yields is late.

        A delete whose actor is no longer owned is not waited for: a reap's
        own delete of that actor landed (or failed, and the reap names it),
        so a delete that hangs on it holds nothing up. Nor is a delete on
        another loop, which cannot be awaited from this one.

        Yields before each check so a task cancelled just before or during
        reap_async (with no await in between) runs its cancel handler and
        starts its delete tasks while _reaping is nonzero.
        """
        deadline = time.monotonic() + self.background_delete_timeout_s
        loop = asyncio.get_running_loop()
        while True:
            for _ in range(_CANCEL_SETTLE_YIELDS):
                await asyncio.sleep(0)
            with self._lock:
                pending: dict[asyncio.Task[Any], str] = {}
                for t, aid in self._background_deletes.items():
                    if t.done():
                        # Its _done callback runs on the next iteration; a
                        # task cancelled before its first step is recorded
                        # here as well, so this join does not miss it.
                        if t.cancelled():
                            self._failed_background_deletes[aid] = None
                    elif aid in self._owned_actors and t.get_loop() is loop:
                        pending[t] = aid
            if not pending:
                return self._take_failed_background_deletes(), []
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                late = list(pending.values())
                logger.warning(
                    "Timed out after %gs waiting for %d background environment delete(s); "
                    "still deleting: %s",
                    self.background_delete_timeout_s,
                    len(late),
                    late,
                )
                return self._take_failed_background_deletes(), late
            await asyncio.wait(pending, timeout=remaining)

    def _take_failed_background_deletes(self) -> list[str]:
        """The actors whose background deletes failed, or were cancelled, since
        the last join and that are still owned, in the order the deletes
        ended; clears the record and logs them. One whose actor is no longer
        owned is dropped: a reap's own delete of it (it was under
        WARM_POOL_RUN_ID) landed, or failed and is already in that reap's
        failed_actors."""
        with self._lock:
            failed = [aid for aid in self._failed_background_deletes if aid in self._owned_actors]
            self._failed_background_deletes.clear()
        if failed:
            logger.warning(
                "%d background environment delete(s) failed; still owned for a later reap: %s",
                len(failed),
                failed,
            )
        return failed

    def reap(self, run_id: str) -> int:
        return self._sync(self.reap_async(run_id))

    def build_runtime(
        self, instance: RawSandboxInstance, data_plane: str = "ate_env"
    ) -> tuple[Any, Any | None]:
        from ..runtime.substrate_env_client import SubstrateEnvClientRuntime

        ep = instance.data_planes.get(data_plane) or instance.data_planes.get("ate_env")
        return SubstrateEnvClientRuntime(
            env_id=instance.instance_id,
            endpoint=ep.address if ep else instance.endpoint,
            atespace=self.atespace,
            client=self._client_for(instance.instance_id),
            loop_runner=self._get_loop_runner(),
        ), ep


def preflight_batch_size(domains: int, run_id_len: int = 9) -> int:
    """How many images one PreflightWarmer pass takes at most, so that the
    run id of a call, ``<run id>-<domain>-<image>-<attempt>``, fits
    ate-env-api's 20 characters: the run id (9 for the warmer's own
    ``pf-<6>``), 4 for the three separators and the attempt digit, the
    domain index's digits, and what is left for the image index's digits.
    0 when the run id leaves no digit for the image index. The bound counts
    decimal digits; the warmer writes the indexes in base 36, which is never
    longer, so a batch of this size always fits."""
    left = 20 - run_id_len - 4 - len(str(max(domains - 1, 0)))
    return 10**left if left >= 1 else 0


def preflight_batches(
    domains: int, images: int, run_id: str = "", batch_images: int = 0
) -> tuple[int, list[str]]:
    """The batch size and each batch's run id for a pass over ``images``
    images in ``domains`` domains.

    Every batch numbers its images from 0, so two batches must not share a
    run id: their probe names would collide, and a probe one batch left
    behind would fail the next one's create. Without a ``run_id`` each batch
    gets the warmer's own random ``pf-<6>`` (an empty id here). A caller's
    ``run_id`` is used as it is when one batch takes every image, and gets
    the batch index appended (``<run_id>-<batch>``) otherwise, with the
    batch size computed for the longer id. ``batch_images`` overrides the
    size (tests). Raises ``ValueError`` when the run id is too long for the
    indexes to fit ate-env-api's 20 characters.
    """
    if not run_id:
        size = batch_images or preflight_batch_size(domains)
        return size, [""] * -(-images // size)
    size = batch_images or preflight_batch_size(domains, len(run_id))
    if size and images <= size:
        return size, [run_id]
    # More than one batch: find the batch-index width whose batches fit it.
    for digits in range(1, 8):
        size = preflight_batch_size(domains, len(run_id) + 1 + digits)
        if batch_images:
            size = min(size, batch_images)
        if not size:
            break
        count = -(-images // size)
        if len(str(count - 1)) <= digits:
            return size, [f"{run_id}-{b}" for b in range(count)]
    raise ValueError(
        f"run_id {run_id!r} is too long for {images} images in {domains} domains: with the "
        "domain, image and attempt indexes (and a batch index when the images need more than "
        "one batch) it must fit ate-env-api's 20 characters; use a shorter run_id or none"
    )


def merge_preflight_coverages(coverages: list[PreflightCoverage]) -> PreflightCoverage:
    """One coverage for the batches of one pass: the goldens of all, the
    domains merged by name (images appended, nodes seen joined), complete
    when every batch was; the first batch's run id names the pass."""
    if len(coverages) == 1:
        return coverages[0]
    first = coverages[0]
    merged = PreflightCoverage(
        run_id=first.run_id,
        started_at=min(c.started_at for c in coverages),
        finished_at=max((c.finished_at for c in coverages if c.finished_at), default=None),
        plan=first.plan,
    )
    by_name: dict[str, DomainCoverage] = {}
    for c in coverages:
        merged.goldens.extend(c.goldens)
        for d in c.domains:
            if d.name not in by_name:
                by_name[d.name] = DomainCoverage(name=d.name, nodes=d.nodes)
            m = by_name[d.name]
            m.images.extend(d.images)
            m.nodes_seen = sorted(set(m.nodes_seen) | set(d.nodes_seen))
            m.complete = m.complete and d.complete
    merged.domains = list(by_name.values())
    merged.complete = all(c.complete for c in coverages)
    return merged
