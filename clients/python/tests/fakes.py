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

"""In-process grpc.aio fake servicers mirroring the real server semantics.

The metadata contract and error codes/messages replicate
internal/apiservice/server.go and the guest services, so the client is
exercised against realistic behavior without a cluster.
"""

from __future__ import annotations

import asyncio
import re
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone

import grpc

from ate_env_rl._gen.ateenv.v1alpha import (
    env_pb2,
    env_pb2_grpc,
    guest_pb2,
    guest_pb2_grpc,
)

CHUNK_SIZE = 64 * 1024
# ate-env-api's warmRunIDPattern (internal/apiservice/placement.go).
_WARM_RUN_ID = re.compile(r"^[a-z0-9]([a-z0-9-]*[a-z0-9])?$")


async def _require_env(context: grpc.aio.ServicerContext) -> tuple[str, str]:
    """Extract (env_id, atespace) metadata like the API server's proxy does."""
    md = {key: value for key, value in (context.invocation_metadata() or ())}
    env_id = md.get("x-env-id", "")
    if not env_id:
        await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "x-env-id header is required")
    return env_id, md.get("x-env-atespace", "ate-env")


class FakeEnvironmentService(env_pb2_grpc.EnvironmentServiceServicer):
    def __init__(self):
        self.image_templates: dict = {}
        # Base templates ListPlacementDomains can filter by: (atespace, name)
        # to the template's own worker selector. The default template has
        # none, so it addresses every pool with a selector.
        self.template_selectors: dict[tuple[str, str], dict[str, str]] = {
            ("ate-env", "default-template"): {},
        }
        self.environments: dict[tuple[str, str], env_pb2.Environment] = {}
        self.last_create_request: env_pb2.CreateEnvironmentRequest | None = None
        self.last_suspend_request: env_pb2.SuspendEnvironmentRequest | None = None
        # Whether the server runs with the environment store, thus accepts a
        # time to live. Off, as ate-env-api is by default.
        self.time_to_live = False
        self.last_fork_request: env_pb2.ForkEnvironmentRequest | None = None
        # Placement, for creates, ListPlacementDomains and WarmImage: the nodes
        # behind each value of the ate-env/domain selector label, the slots per node
        # per domain, whether the global role is missing, whether every probe
        # lands on the first node, and the WarmImage requests seen.
        self.domain_nodes: dict[str, list[str]] = {}
        self._domain_counter: dict[str, int] = {}
        self.node_slots: dict[str, int] = {}
        self.deny_list_workers = False
        # How many ListPlacementDomains calls answer UNAVAILABLE first.
        self.list_unavailable_first = 0
        # GetImageStatus (not the ensure): the calls seen, and the seconds
        # each is held before its answer, so a test can act with one in flight.
        self.status_calls = 0
        self.status_hold = 0.0
        # The order of creates and warm calls, for tests of what came first.
        self.call_log: list[str] = []
        self.warm_same_node = False
        self.warm_requests: list[env_pb2.WarmImageRequest] = []
        # Environment ids whose delete fails and leaves them in place, and
        # ids whose delete answers NOT_FOUND (gone already).
        self.delete_fail: set[str] = set()
        self.delete_not_found: set[str] = set()
        # Probe ids the next WarmImage reports undeleted, and the seconds it
        # reports waited for probe slots.
        self.warm_undeleted: list[str] = []
        self.warm_waited = 0.0
        # WarmImage: seconds a round's probes are held before their delete
        # (so a cancel can land mid-round and concurrent calls overlap); how
        # many calls fail with UNAVAILABLE before any event; a (code, message)
        # every call fails with; the calls seen.
        self.warm_hold = 0.0
        # When set, warm_hold applies to calls for these images only; the
        # images in warm_hold_before_images are held before a round's
        # creates instead of after them.
        self.warm_hold_images: set[str] = set()
        self.warm_hold_before_images: set[str] = set()
        self.warm_unavailable_first = 0
        # ate-env-api's -warm-max-probes-per-node: a request's
        # probes_per_node above it is refused (2 unless raised).
        self.warm_max_probes_per_node = 2
        self.warm_fail: tuple[grpc.StatusCode, str] | None = None
        self.warm_calls = 0
        # How many calls end with UNAVAILABLE right after their first round's
        # creates, leaving the probes in place, as an api server restart does.
        self.warm_cut_after_create = 0
        # An environment created without a known domain reports no node, as
        # before its placement; creates hand the domain's nodes out round-robin.
        self.first_node_creates = 0

    async def CreateEnvironment(self, request, context):
        self.call_log.append("create")
        self.last_create_request = request
        atespace = request.atespace or "ate-env"
        key = (atespace, request.id)
        if not request.id:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "id is required")
        # Mirror ate-env-api: a negative time to live is INVALID_ARGUMENT, and
        # a positive one needs the environment store; both create nothing,
        # and both come before the control plane is asked about the id.
        ttl = request.time_to_live.ToTimedelta()
        if ttl < timedelta(0):
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, f"time_to_live {ttl} is negative")
        if ttl > timedelta(0) and not self.time_to_live:
            await context.abort(
                grpc.StatusCode.FAILED_PRECONDITION, "time_to_live needs the environment store"
            )
        if key in self.environments:
            await context.abort(
                grpc.StatusCode.ALREADY_EXISTS,
                f'environment "{request.id}" already exists',
            )
        template = env_pb2.Template(name="default-template", atespace="ate-env")
        if request.HasField("template"):
            if request.template.name:
                template.name = request.template.name
            if request.template.atespace:
                template.atespace = request.template.atespace
        if request.rootfs_image:
            # Mirror ate-env-api: the template becomes the base of one derived
            # per image, named after the digest.
            if "@sha256:" not in request.rootfs_image:
                await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "image is not pinned by digest")
            digest = request.rootfs_image.split("@sha256:", 1)[1]
            template.name = f"{template.name}-{digest[:12]}"
        environment = self._new_environment(key, template, request.worker_selector)
        if ttl > timedelta(0):
            # The deadline counts from the create time.
            environment.expire_time.FromDatetime(environment.create_time.ToDatetime() + ttl)
        return env_pb2.CreateEnvironmentResponse(environment=environment)

    async def SetEnvironmentTimeToLive(self, request, context):
        # Mirror ate-env-api: the new deadline counts from the call, zero
        # removes it, a negative value is INVALID_ARGUMENT, and the call
        # needs the environment store. The fake deletes nothing at a deadline.
        ttl = request.time_to_live.ToTimedelta()
        if ttl < timedelta(0):
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, f"time_to_live {ttl} is negative")
        if not self.time_to_live:
            await context.abort(
                grpc.StatusCode.FAILED_PRECONDITION,
                "SetEnvironmentTimeToLive needs the environment store",
            )
        environment = await self._lookup(request, context)
        environment.ClearField("expire_time")
        if ttl > timedelta(0):
            environment.expire_time.FromDatetime(datetime.now(timezone.utc).replace(tzinfo=None) + ttl)
        return env_pb2.SetEnvironmentTimeToLiveResponse(environment=environment)

    def _new_environment(self, key, template, worker_selector) -> env_pb2.Environment:
        """Place and store an environment, as a create and a WarmImage probe do."""
        atespace, env_id = key
        environment = env_pb2.Environment(
            id=env_id,
            atespace=atespace,
            template=template,
            status=env_pb2.ENVIRONMENT_STATUS_RUNNING,
        )
        environment.create_time.GetCurrentTime()
        environment.worker_selector.update(worker_selector)
        domain = worker_selector.get("ate-env/domain", "")
        if nodes := self.domain_nodes.get(domain):
            n = self._domain_counter.get(domain, 0)
            # The first first_node_creates creates of a domain all land on its
            # first node, as a scheduler that favours a warm node does; the
            # rest go round-robin (always the first with warm_same_node).
            first = n < self.first_node_creates or self.warm_same_node
            environment.node = nodes[0 if first else n % len(nodes)]
            self._domain_counter[domain] = n + 1
        self.environments[key] = environment
        return environment

    async def GetEnvironment(self, request, context):
        environment = await self._lookup(request, context)
        return env_pb2.GetEnvironmentResponse(environment=environment)

    async def ListEnvironments(self, request, context):
        # Mirror ate-env-api: one page of the atespace in id order, 100 per
        # page unless asked, at most 1000; the status filter applies to the
        # page after the read and leaves the token alone. The token is the
        # offset of the next page.
        if request.page_size < 0:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, f"page_size {request.page_size} is negative")
        for s in request.statuses:
            if s not in env_pb2.EnvironmentStatus.values():
                await context.abort(grpc.StatusCode.INVALID_ARGUMENT, f"status {s} is not an EnvironmentStatus")
        atespace = request.atespace or "ate-env"
        ids = sorted(env_id for space, env_id in self.environments if space == atespace)
        start = 0
        if request.page_token:
            if not request.page_token.isdigit() or int(request.page_token) > len(ids):
                await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "invalid page_token")
            start = int(request.page_token)
        end = start + min(request.page_size or 100, 1000)
        page = [self.environments[(atespace, env_id)] for env_id in ids[start:end]]
        return env_pb2.ListEnvironmentsResponse(
            environments=[e for e in page if not request.statuses or e.status in request.statuses],
            next_page_token=str(end) if end < len(ids) else "",
        )

    async def SuspendEnvironment(self, request, context):
        environment = await self._lookup(request, context)
        self.last_suspend_request = request
        # Mirror ate-env-api: an unknown mode is INVALID_ARGUMENT; a suspended
        # or paused environment is left as it is, because Substrate's
        # SuspendActor returns at once on it; a local suspend of a running one
        # is PAUSED until its upload lands, a durable one SUSPENDED.
        if request.mode not in (
            env_pb2.SUSPEND_MODE_UNSPECIFIED,
            env_pb2.SUSPEND_MODE_DURABLE,
            env_pb2.SUSPEND_MODE_LOCAL,
        ):
            await context.abort(
                grpc.StatusCode.INVALID_ARGUMENT, f"mode {request.mode} is not a SuspendMode"
            )
        if environment.status in (
            env_pb2.ENVIRONMENT_STATUS_SUSPENDED,
            env_pb2.ENVIRONMENT_STATUS_PAUSED,
        ):
            return env_pb2.SuspendEnvironmentResponse()
        if request.mode == env_pb2.SUSPEND_MODE_LOCAL:
            environment.status = env_pb2.ENVIRONMENT_STATUS_PAUSED
        else:
            environment.status = env_pb2.ENVIRONMENT_STATUS_SUSPENDED
        return env_pb2.SuspendEnvironmentResponse()

    async def DeleteEnvironment(self, request, context):
        await self._lookup(request, context)
        if request.id in self.delete_fail:
            # As a CRASHED actor's delete fails today: the environment stays.
            await context.abort(grpc.StatusCode.INTERNAL, f"DeleteVolumes prerequisite not met for {request.id}")
        if request.id in self.delete_not_found:
            self.environments.pop((request.atespace or "ate-env", request.id), None)
            await context.abort(grpc.StatusCode.NOT_FOUND, f'environment "{request.id}" not found')
        del self.environments[(request.atespace or "ate-env", request.id)]
        return env_pb2.DeleteEnvironmentResponse()

    async def ForkEnvironment(self, request, context):
        # Mirror ate-env-api: validate count and ids, suspend the source
        # locally, then create every copy or none.
        self.last_fork_request = request
        source = await self._lookup(request, context)
        atespace = request.atespace or "ate-env"
        count = request.count or len(request.ids)
        if not 1 <= count <= 64:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, f"count {request.count} is outside 1 to 64")
        if request.ids and (request.id_prefix or len(request.ids) != count or len(set(request.ids)) != count):
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "bad ids")
        ids = list(request.ids) or [f"{request.id_prefix or request.id + '-abc123'}-{i}" for i in range(count)]
        if any((atespace, i) in self.environments for i in ids):
            await context.abort(grpc.StatusCode.ALREADY_EXISTS, "a copy exists")
        if source.status == env_pb2.ENVIRONMENT_STATUS_RUNNING:
            source.status = env_pb2.ENVIRONMENT_STATUS_PAUSED
        selector = request.worker_selector or source.worker_selector
        copies = []
        for i in ids:
            copy = self._new_environment((atespace, i), source.template, selector)
            copy.status = env_pb2.ENVIRONMENT_STATUS_SUSPENDED
            copies.append(copy)
        return env_pb2.ForkEnvironmentResponse(environments=copies, source_tag="fork-0123456789abcdef")

    def _image_status(self, request):
        template = env_pb2.Template(name="default-template", atespace="ate-env")
        if request.HasField("template"):
            if request.template.name:
                template.name = request.template.name
            if request.template.atespace:
                template.atespace = request.template.atespace
        atespace = request.atespace or template.atespace
        if not re.search(r"@sha256:[0-9a-f]{64}$", request.rootfs_image):
            return None, atespace, template
        digest = request.rootfs_image.split("@sha256:", 1)[1]
        derived = env_pb2.Template(name=f"{template.name}-{digest[:12]}", atespace=atespace)
        return (atespace, derived.name), atespace, derived

    async def GetImageStatus(self, request, context):
        self.status_calls += 1
        if self.status_hold:
            await asyncio.sleep(self.status_hold)
        return await self._get_image_status(request, context)

    async def _get_image_status(self, request, context):
        key, atespace, derived = self._image_status(request)
        if key is None:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "image is not pinned by digest")
        golden = self.image_templates.get(key)
        if golden is None:
            return env_pb2.ImageStatus(
                rootfs_image=request.rootfs_image, template=derived, template_exists=False,
                golden=env_pb2.GOLDEN_SNAPSHOT_STATE_NONE,
            )
        return env_pb2.ImageStatus(
            rootfs_image=request.rootfs_image, template=derived, template_exists=True,
            golden=golden.golden, golden_tag=golden.golden_tag, golden_error=golden.golden_error,
        )

    async def EnsureImageTemplate(self, request, context):
        key, _, _ = self._image_status(request)
        if key is None:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "image is not pinned by digest")
        # Mirror ate-env-api: a missing template is created and bakes; a
        # template whose bake failed is recreated and bakes again.
        existing = self.image_templates.get(key)
        if existing is None or existing.golden == env_pb2.GOLDEN_SNAPSHOT_STATE_FAILED:
            self.image_templates[key] = env_pb2.ImageStatus(golden=env_pb2.GOLDEN_SNAPSHOT_STATE_PENDING)
        return await self._get_image_status(request, context)

    async def ListPlacementDomains(self, request, context):
        # Mirror ate-env-api: one domain per value of the ate-env/domain label
        # with the nodes behind it; PermissionDenied without the global role.
        selector = None
        if request.HasField("template") or request.atespace:
            name = request.template.name or "default-template"
            atespace = request.atespace or request.template.atespace or "ate-env"
            if (atespace, name) not in self.template_selectors:
                await context.abort(grpc.StatusCode.NOT_FOUND, f"actor template {name!r} not found")
            selector = self.template_selectors[(atespace, name)]
        if self.deny_list_workers:
            await context.abort(grpc.StatusCode.PERMISSION_DENIED, "not allowed to list workers")
        if self.list_unavailable_first > 0:
            self.list_unavailable_first -= 1
            await context.abort(grpc.StatusCode.UNAVAILABLE, "api server restarting")
        resp = env_pb2.ListPlacementDomainsResponse()
        for name in sorted(self.domain_nodes):
            labels = {"ate-env/domain": name}
            if selector is not None and any(labels.get(k) != v for k, v in selector.items()):
                continue
            d = resp.domains.add(
                name=f"pool-{name}", worker_namespace="ate-system",
                nodes=len(set(self.domain_nodes[name])), workers=len(self.domain_nodes[name]),
                node_slots=self.node_slots.get(name, 0),
            )
            d.selector["ate-env/domain"] = name
        return resp

    async def WarmImage(self, request, context):
        # Mirror ate-env-api: rounds of nodes x probes_per_node probes named
        # <run>-<round>-<slot>, placed as creates are (round-robin on the
        # domain's nodes, the first node with warm_same_node), held for
        # warm_hold, then deleted (delete_fail lists one undeleted); a probe
        # event each, then the coverage. A name that exists already fails the
        # probe and is left alone. A cancelled call deletes what it created.
        self.warm_calls += 1
        self.call_log.append("warm")
        if self.warm_fail is not None:
            await context.abort(*self.warm_fail)
        if self.warm_unavailable_first > 0:
            self.warm_unavailable_first -= 1
            await context.abort(grpc.StatusCode.UNAVAILABLE, "api server restarting")
        if not re.search(r"@sha256:[0-9a-f]{64}$", request.rootfs_image):
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "image is not pinned by digest")
        # What ate-env-api refuses before it creates a probe (parseWarmOptions
        # and the checks after it in internal/apiservice/placement.go), in the
        # server's order: 0 means the default. warm_max_probes_per_node is the
        # server's -warm-max-probes-per-node.
        if request.nodes < 0:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "nodes must be >= 0")
        if not 0 <= request.probes_per_node <= 8:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "probes_per_node must be between 1 and 8")
        if request.probes_per_node > self.warm_max_probes_per_node:
            await context.abort(
                grpc.StatusCode.INVALID_ARGUMENT,
                f"probes_per_node {request.probes_per_node} exceeds the {self.warm_max_probes_per_node} "
                "probes per node this server runs on one worker selector at once "
                "(ate-env-api -warm-max-probes-per-node)",
            )
        if not 0 <= request.max_rounds <= 10:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "max_rounds must be between 1 and 10")
        if request.HasField("probe_timeout") and request.probe_timeout.ToNanoseconds() <= 0:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "probe_timeout must be positive")
        if request.run_id and (len(request.run_id) > 20 or not _WARM_RUN_ID.match(request.run_id)):
            await context.abort(
                grpc.StatusCode.INVALID_ARGUMENT,
                f"run_id must match {_WARM_RUN_ID.pattern} and be at most 20 characters",
            )
        domain = request.worker_selector.get("ate-env/domain", "")
        pool = self.domain_nodes.get(domain)
        wanted = request.nodes
        # The server counts the distinct nodes behind the selector and refuses
        # a request for more. With no pool for the selector here, or without
        # the permission to list workers, the request's nodes are trusted.
        listed = 0 if self.deny_list_workers else len(set(pool or ()))
        if listed and wanted > listed:
            await context.abort(
                grpc.StatusCode.INVALID_ARGUMENT,
                f"nodes {wanted} exceeds the {listed} nodes the selector's workers run on",
            )
        width = (wanted or listed) * (request.probes_per_node or min(2, self.warm_max_probes_per_node))
        if width > 256:
            await context.abort(
                grpc.StatusCode.INVALID_ARGUMENT,
                f"{wanted or listed} nodes x {width // (wanted or listed)} probes per node exceed "
                "the 256 probes a round may start",
            )
        if wanted == 0:
            if self.deny_list_workers:
                await context.abort(
                    grpc.StatusCode.PERMISSION_DENIED,
                    "nodes is 0 and ate-env-api may not list workers: pass nodes",
                )
            if not pool:
                await context.abort(
                    grpc.StatusCode.FAILED_PRECONDITION,
                    f"no active worker matches the selector {dict(request.worker_selector)}",
                )
            wanted = len(set(pool))
        per_node = request.probes_per_node or min(2, self.warm_max_probes_per_node)
        rounds = request.max_rounds or 3
        run_id = request.run_id or "warm"
        atespace = request.atespace or "ate-env"
        template = env_pb2.Template(name="default-template", atespace="ate-env")
        if request.HasField("template"):
            if request.template.name:
                template.name = request.template.name
            if request.template.atespace:
                template.atespace = request.template.atespace
        digest = request.rootfs_image.split("@sha256:", 1)[1]
        template.name = f"{template.name}-{digest[:12]}"
        self.warm_requests.append(request)
        covered: set[str] = set()
        probes = failed = 0
        error = ""
        rnd = 0
        undeleted = list(self.warm_undeleted)
        created: list[str] = []
        try:
            while rnd < rounds and len(covered) < wanted:
                if self.warm_hold and request.rootfs_image in self.warm_hold_before_images:
                    await asyncio.sleep(self.warm_hold)
                rnd += 1
                created = []
                started = round_failed = 0
                # As ate-env-api: the round's probes are all created before
                # any event goes out, so two concurrent calls on one pool do
                # not take turns at the node counter probe by probe.
                events: list[env_pb2.WarmImageEvent] = []
                for k in range(wanted * per_node):
                    probe_id = f"{run_id}-{rnd}-{k}"
                    key = (atespace, probe_id)
                    probes += 1
                    started += 1
                    probe = env_pb2.WarmProbe(id=probe_id, round=rnd)
                    probe.duration.FromMilliseconds(5)
                    if key in self.environments:
                        probe.error = f'environment "{probe_id}" already exists'
                    else:
                        environment = self._new_environment(key, template, request.worker_selector)
                        created.append(probe_id)
                        probe.node = environment.node
                        if not probe.node:
                            probe.error = "running but the control plane reports no node"
                    if probe.error:
                        failed += 1
                        round_failed += 1
                        error = probe.error
                    else:
                        covered.add(probe.node)
                    events.append(env_pb2.WarmImageEvent(probe=probe))
                for ev in events:
                    yield ev
                if self.warm_cut_after_create > 0:
                    self.warm_cut_after_create -= 1
                    created = []  # the probes stay
                    await context.abort(grpc.StatusCode.UNAVAILABLE, "api server restarted; its probes stay")
                if self.warm_hold and (
                    not self.warm_hold_images or request.rootfs_image in self.warm_hold_images
                ):
                    await asyncio.sleep(self.warm_hold)
                for probe_id in created:
                    if probe_id in self.delete_fail:
                        undeleted.append(probe_id)
                    else:
                        # A delete that answers NotFound is gone all the same.
                        self.environments.pop((atespace, probe_id), None)
                created = []
                if started and round_failed == started:
                    break
        finally:
            # A cancelled call: the server deletes the round's probes.
            for probe_id in created:
                if probe_id not in self.delete_fail:
                    self.environments.pop((atespace, probe_id), None)
        coverage = env_pb2.WarmImageCoverage(
            nodes=sorted(covered), wanted=wanted, probes=probes, failed=failed, rounds=rnd,
            complete=len(covered) >= wanted, error=error, undeleted_probes=undeleted,
        )
        if self.warm_waited:
            coverage.waited.FromTimedelta(timedelta(seconds=self.warm_waited))
        yield env_pb2.WarmImageEvent(coverage=coverage)

    async def GetServerInfo(self, request, context):
        return env_pb2.GetServerInfoResponse(
            server="sandboxenv-rl",
            version="v0.0.0-fake",
            substrate_version="unknown",
            features=["rootfs_image", "pty", "suspend_mode", "image_status", "placement_domains", "warm_image"]
            + (["time_to_live"] if self.time_to_live else []),
        )

    async def _lookup(self, request, context):
        key = (request.atespace or "ate-env", request.id)
        environment = self.environments.get(key)
        if environment is None:
            await context.abort(
                grpc.StatusCode.NOT_FOUND, f'environment "{request.id}" not found'
            )
        return environment


@dataclass
class FakeProc:
    """Scripted behavior for one process started against the fake.

    chunks are (field, data) pairs where field is "stdout" or "stderr";
    they are replayed by StreamProcessOutput. exit_code/signal describe the
    final state. running_polls is the number of GetProcess calls that still
    report RUNNING before the process is considered exited; a follow stream
    exits it immediately once the chunks are replayed. echo_stdin appends
    written input as stdout, like cat.
    """

    chunks: list[tuple[str, bytes]] = field(default_factory=list)
    exit_code: int = 0
    signal: int = 0
    running_polls: int = 0
    echo_stdin: bool = False
    hang_follow: bool = False  # a follow stream never ends (process never exits)
    timed_out: bool | None = None  # None leaves Process.timed_out unset (an old guest)
    # request capture, filled by StartProcess / WriteProcessInput / SignalProcess:
    command: list[str] = field(default_factory=list)
    cwd: str = ""
    env: dict[str, str] = field(default_factory=dict)
    stdin: bool = False
    timeout_seconds: float | None = None
    stdin_data: bytearray = field(default_factory=bytearray)
    stdin_closed: bool = False
    signals: list[int] = field(default_factory=list)
    exited: bool = False


async def _start_failure(context, failure):
    """Answer like ate-env-api for an environment that cannot start: the
    ate-env-start-failure trailer, then the status. failure is
    (grpc.StatusCode, message, reason)."""
    code, message, reason = failure
    context.set_trailing_metadata((("ate-env-start-failure", reason),))
    await context.abort(code, message)


class FakeProcessService(guest_pb2_grpc.ProcessServiceServicer):
    def __init__(self):
        # (code, message, reason): fail every call as an environment that
        # cannot start (see _start_failure).
        self.start_failure: tuple[grpc.StatusCode, str, str] | None = None
        self.next_procs: list[FakeProc] = []  # scripts for upcoming StartProcess calls
        self.procs: dict[str, FakeProc] = {}
        self.last_env: tuple[str, str] | None = None
        self._counter = 0

    def _to_pb(self, process_id: str, proc: FakeProc, *, exited: bool) -> guest_pb2.Process:
        pb = guest_pb2.Process(
            process_id=process_id,
            command=proc.command,
            pid=1000 + int(process_id.rsplit("-", 1)[1]),
            state=guest_pb2.PROCESS_STATE_EXITED if exited else guest_pb2.PROCESS_STATE_RUNNING,
        )
        pb.started_at.GetCurrentTime()
        if exited:
            pb.finished_at.GetCurrentTime()
            pb.exit_code = 128 + proc.signal if proc.signal else proc.exit_code
            if proc.timed_out is not None:
                pb.timed_out = proc.timed_out
        return pb

    async def StartProcess(self, request, context):
        self.last_env = await _require_env(context)
        if self.start_failure:
            await _start_failure(context, self.start_failure)
        if not request.command:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "command cannot be empty")
        proc = self.next_procs.pop(0) if self.next_procs else FakeProc()
        proc.command = list(request.command)
        proc.cwd = request.cwd
        proc.env = dict(request.env)
        proc.stdin = request.stdin
        if request.HasField("timeout"):
            proc.timeout_seconds = request.timeout.ToTimedelta().total_seconds()
        self._counter += 1
        process_id = f"proc-{self._counter}"
        self.procs[process_id] = proc
        return self._to_pb(process_id, proc, exited=False)

    async def GetProcess(self, request, context):
        self.last_env = await _require_env(context)
        proc = await self._lookup(request.process_id, context)
        if not proc.exited and proc.running_polls > 0:
            proc.running_polls -= 1
            return self._to_pb(request.process_id, proc, exited=False)
        proc.exited = True
        return self._to_pb(request.process_id, proc, exited=True)

    async def StreamProcessOutput(self, request, context):
        self.last_env = await _require_env(context)
        proc = await self._lookup(request.process_id, context)
        stdout_skip = request.stdout_offset
        stderr_skip = request.stderr_offset
        for source, data in proc.chunks:
            if source == "stdout":
                data, stdout_skip = data[stdout_skip:], max(0, stdout_skip - len(data))
                if data:
                    yield guest_pb2.ProcessOutput(stdout=data)
            else:
                data, stderr_skip = data[stderr_skip:], max(0, stderr_skip - len(data))
                if data:
                    yield guest_pb2.ProcessOutput(stderr=data)
        if request.follow and proc.hang_follow:
            await asyncio.Event().wait()
        if request.follow:
            # Simulate a live process: yield to the loop between chunks and exit.
            for _ in range(proc.running_polls):
                await asyncio.sleep(0)
            proc.running_polls = 0
            proc.exited = True
        if proc.exited or proc.running_polls == 0:
            proc.exited = True
            yield guest_pb2.ProcessOutput(exit=self._to_pb(request.process_id, proc, exited=True))

    async def WriteProcessInput(self, request_iterator, context):
        self.last_env = await _require_env(context)
        proc = None
        process_id = ""
        written = 0
        async for request in request_iterator:
            if proc is None:
                process_id = request.process_id
                if not process_id:
                    await context.abort(
                        grpc.StatusCode.INVALID_ARGUMENT,
                        "process_id is required on the first message",
                    )
                proc = await self._lookup(process_id, context)
                if not proc.stdin:
                    await context.abort(
                        grpc.StatusCode.FAILED_PRECONDITION,
                        f'process "{process_id}" was started without stdin',
                    )
            if proc.exited:
                await context.abort(
                    grpc.StatusCode.FAILED_PRECONDITION, f'process "{process_id}" has exited'
                )
            if request.data and proc.stdin_closed:
                await context.abort(
                    grpc.StatusCode.FAILED_PRECONDITION,
                    f'stdin of process "{process_id}" is closed',
                )
            proc.stdin_data.extend(request.data)
            written += len(request.data)
            if request.data and proc.echo_stdin:
                proc.chunks.append(("stdout", bytes(request.data)))
            if request.close:
                proc.stdin_closed = True
        if proc is None:
            await context.abort(
                grpc.StatusCode.INVALID_ARGUMENT, "process_id is required on the first message"
            )
        return guest_pb2.WriteProcessInputResponse(bytes_written=written)

    async def SignalProcess(self, request, context):
        self.last_env = await _require_env(context)
        proc = await self._lookup(request.process_id, context)
        if request.signal == guest_pb2.SIGNAL_UNSPECIFIED:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "unsupported signal")
        if proc.exited:
            await context.abort(
                grpc.StatusCode.FAILED_PRECONDITION,
                f'process "{request.process_id}" has exited',
            )
        proc.signals.append(request.signal)
        if request.signal in (guest_pb2.SIGNAL_KILL, guest_pb2.SIGNAL_TERM):
            proc.signal = request.signal
            proc.running_polls = 0
        return self._to_pb(request.process_id, proc, exited=False)

    async def _lookup(self, process_id, context):
        proc = self.procs.get(process_id)
        if proc is None:
            await context.abort(
                grpc.StatusCode.NOT_FOUND, f'process "{process_id}" not found'
            )
        return proc


class FakeFileSystemService(guest_pb2_grpc.FileSystemServiceServicer):
    def __init__(self):
        self.start_failure: tuple[grpc.StatusCode, str, str] | None = None
        self.files: dict[str, bytes] = {}
        self.modes: dict[str, int] = {}
        self.last_env: tuple[str, str] | None = None
        self.last_write_chunk_sizes: list[int] = []

    async def _validate_path(self, path, context):
        # Mirrors guest/filesystem resolveAndValidatePath's sandbox check.
        if path.startswith(".."):
            await context.abort(
                grpc.StatusCode.PERMISSION_DENIED,
                f'access denied: path "{path}" is outside sandbox root directory "/workspace"',
            )

    async def ReadFile(self, request, context):
        self.last_env = await _require_env(context)
        if self.start_failure:
            await _start_failure(context, self.start_failure)
        await self._validate_path(request.path, context)
        data = self.files.get(request.path)
        if data is None:
            await context.abort(
                grpc.StatusCode.NOT_FOUND,
                f"open {request.path}: no such file or directory",
            )
        for i in range(0, len(data), CHUNK_SIZE):
            yield guest_pb2.ReadFileResponse(chunk=data[i : i + CHUNK_SIZE])

    async def WriteFile(self, request_iterator, context):
        self.last_env = await _require_env(context)
        path = None
        mode = 0
        buf = bytearray()
        chunk_sizes = []
        async for request in request_iterator:
            if path is None:
                # ate-env-api wakes the environment once the first message
                # arrives, so a start failure answers after it.
                if self.start_failure:
                    await _start_failure(context, self.start_failure)
                if not request.path:
                    await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "path is required")
                await self._validate_path(request.path, context)
                path = request.path
                mode = request.mode
                if request.seek_offset < 0:
                    await context.abort(
                        grpc.StatusCode.INVALID_ARGUMENT, "seek_offset cannot be negative"
                    )
                if request.seek_offset > 0:
                    # Keep existing content, zero-fill up to the offset.
                    buf = bytearray(self.files.get(path, b""))
                    buf.extend(b"\0" * max(0, request.seek_offset - len(buf)))
                    pos = request.seek_offset
                else:
                    pos = 0
            buf[pos : pos + len(request.chunk)] = request.chunk
            pos += len(request.chunk)
            chunk_sizes.append(len(request.chunk))
        self.last_write_chunk_sizes = chunk_sizes
        if path is None:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "path is required")
        self.files[path] = bytes(buf)
        self.modes.setdefault(path, mode)
        return guest_pb2.WriteFileResponse(bytes_written=sum(chunk_sizes))
