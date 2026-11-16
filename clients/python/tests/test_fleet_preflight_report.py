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

"""preflight_report() returns a PreflightReport: FL-1 to FL-4 of docs/plans/fleet-sdk."""

from __future__ import annotations

import asyncio

import grpc
import pytest

from ate_env_rl import (
    AsyncSandboxFleet,
    FleetConfig,
    PreflightError,
    PreflightReport,
    SandboxFleet,
    Task,
)
from ate_env_rl._gen.ateenv.v1alpha import env_pb2
from ate_env_rl.backend.substrate import _PREFLIGHT_STATUS_CONCURRENCY
from ate_env_rl.client import Client
from ate_env_rl.errors import RpcError

IMG_A = "docker.io/library/python@sha256:" + "a" * 64
IMG_B = "docker.io/library/python@sha256:" + "b" * 64
BASE = "tmpl-test"


def _golden(fakes, image: str, state, error: str = "") -> None:
    digest = image.split("@sha256:", 1)[1]
    fakes.environments.image_templates[("ate-env", f"{BASE}-{digest[:12]}")] = env_pb2.ImageStatus(
        golden=state, golden_tag="g1" if state == env_pb2.GOLDEN_SNAPSHOT_STATE_READY else "",
        golden_error=error,
    )


def _setup(fakes) -> None:
    fakes.environments.domain_nodes = {"d0": ["node-1", "node-2"]}
    fakes.environments.template_selectors[("ate-env", BASE)] = {}
    _golden(fakes, IMG_A, env_pb2.GOLDEN_SNAPSHOT_STATE_READY)
    _golden(fakes, IMG_B, env_pb2.GOLDEN_SNAPSHOT_STATE_FAILED, "bake timed out")


def _cfg(client, **kw) -> FleetConfig:
    return FleetConfig(backend="substrate", endpoint=client.endpoint, template=BASE, **kw)


def _tasks() -> list[Task]:
    return [Task(id="a", image=IMG_A), Task(id="b", image=IMG_B)]


def _statuses(report: PreflightReport) -> dict[str, str]:
    return {c.name: c.status for c in report.checks}


def _sync_report(client, strict: bool = False, tasks=None, **kw) -> PreflightReport:
    fleet = SandboxFleet(_cfg(client, **kw))
    try:
        fleet.load_tasks(_tasks() if tasks is None else tasks)
        return fleet.preflight_report(strict=strict)
    finally:
        fleet.close()


async def test_fl1_preflight_returns_a_report(fake_stack):
    # FL-1: one check each for connectivity, each image's template and golden,
    # and the placement domains.
    client, fakes = fake_stack
    _setup(fakes)
    report = await asyncio.to_thread(_sync_report, client)
    assert _statuses(report) == {
        "connectivity": "ok",
        f"image:{IMG_A}": "ok",
        f"image:{IMG_B}": "failed",
        "placement": "ok",
    }
    assert "golden=READY" in report.check(f"image:{IMG_A}").detail
    assert "bake timed out" in report.check(f"image:{IMG_B}").detail
    assert "2 nodes" in report.check("placement").detail
    assert not report.ok and [c.name for c in report.failed] == [f"image:{IMG_B}"]


async def test_fl1_placement_without_the_global_role_is_a_failed_check(fake_stack):
    client, fakes = fake_stack
    _setup(fakes)
    # No derived template exists yet, and the base template does.
    fakes.environments.image_templates.clear()
    fakes.environments.deny_list_workers = True
    report = await asyncio.to_thread(_sync_report, client)
    assert report.check("placement").status == "failed"
    assert "PermissionDenied" in report.check("placement").detail
    # Only a missing base template (NotFound) fails the image checks.
    assert report.check(f"image:{IMG_A}").status == "ok"
    assert report.check(f"image:{IMG_B}").status == "ok"


@pytest.mark.parametrize("domain_nodes", [{"d0": []}, {}], ids=["zero-nodes", "no-domain"])
async def test_fl1_placement_without_nodes_is_failed(fake_stack, domain_nodes):
    # FL-1: placement is ok only when a domain has nodes.
    client, fakes = fake_stack
    _setup(fakes)
    fakes.environments.domain_nodes = domain_nodes
    report = await asyncio.to_thread(_sync_report, client)
    assert report.check("placement").status == "failed"


async def test_fl1_placement_lists_the_base_templates_pools(fake_stack):
    # FL-1: the listing is for the base template, whose own selector here
    # matches no pool.
    client, fakes = fake_stack
    _setup(fakes)
    fakes.environments.template_selectors[("ate-env", BASE)] = {"ate-env/domain": "d9"}
    report = await asyncio.to_thread(_sync_report, client)
    assert report.check("placement").status == "failed"


@pytest.mark.parametrize(
    ("kw", "status"),
    [
        ({"node_selector": {"pool": "does-not-exist"}}, "failed"),
        ({"node_selector": {"ate-env/domain": "d0"}}, "ok"),
        ({"placement": {IMG_A: {"ate-env/domain": "d9"}}}, "failed"),
        ({"placement": {IMG_A: {"ate-env/domain": "d0"}}}, "ok"),
    ],
    ids=["selector-no-pool", "selector-pool", "placement-no-pool", "placement-pool"],
)
async def test_fl1_placement_applies_the_fleet_selector(fake_stack, kw, status):
    # FL-1: placement narrows the base template's pools by node_selector and
    # each image's placement labels, as the creates send them.
    client, fakes = fake_stack
    _setup(fakes)
    report = await asyncio.to_thread(_sync_report, client, **kw)
    assert report.check("placement").status == status
    if status == "failed":
        selector = kw.get("node_selector") or kw["placement"][IMG_A]
        assert f"no pool with nodes matches the worker selector {selector}" in report.check("placement").detail


async def test_fl1_placement_ignores_the_selector_of_a_routed_image(fake_stack):
    # FL-1: an image routed through image_templates creates from another
    # template, so its placement labels do not narrow the base template's pools.
    client, fakes = fake_stack
    _setup(fakes)
    fakes.environments.template_selectors[("ate-env", "fixed")] = {}
    report = await asyncio.to_thread(
        _sync_report, client,
        image_templates={IMG_B: "fixed"}, placement={IMG_B: {"ate-env/domain": "d9"}},
    )
    assert report.check("placement").status == "ok"


async def test_fl1_placement_without_tasks_applies_the_node_selector(fake_stack):
    # FL-1: with no loaded task, the placement check uses node_selector.
    client, fakes = fake_stack
    _setup(fakes)
    report = await asyncio.to_thread(
        _sync_report, client, tasks=[], node_selector={"pool": "does-not-exist"}
    )
    assert _statuses(report) == {"connectivity": "ok", "placement": "failed"}


async def test_fl1_pending_golden_is_ok_and_unrouted_images_are_skipped(fake_stack):
    # FL-1: only a FAILED golden fails an image; an image that is not pinned
    # by digest or is routed through image_templates derives no template.
    client, fakes = fake_stack
    _setup(fakes)
    _golden(fakes, IMG_A, env_pb2.GOLDEN_SNAPSHOT_STATE_PENDING)
    fakes.environments.template_selectors[("ate-env", "fixed")] = {}
    tasks = [Task(id="a", image=IMG_A), Task(id="u", image="python:3.12"), Task(id="b", image=IMG_B)]
    report = await asyncio.to_thread(
        _sync_report, client, tasks=tasks, image_templates={IMG_B: "fixed"}
    )
    assert _statuses(report) == {
        "connectivity": "ok",
        f"image:{IMG_A}": "ok",
        "image:python:3.12": "skipped",
        f"image:{IMG_B}": "skipped",
        "placement": "ok",
    }
    assert "golden=PENDING" in report.check(f"image:{IMG_A}").detail


async def test_fl1_missing_base_template_fails_the_image_checks(fake_stack):
    # FL-1: without the base template no create can derive a missing
    # template, so that image check fails along with placement. A derived
    # template that exists is used without the base template: still ok.
    client, fakes = fake_stack
    _setup(fakes)
    del fakes.environments.template_selectors[("ate-env", BASE)]
    fakes.environments.image_templates.clear()
    _golden(fakes, IMG_A, env_pb2.GOLDEN_SNAPSHOT_STATE_READY)
    report = await asyncio.to_thread(_sync_report, client)
    assert _statuses(report) == {
        "connectivity": "ok",
        f"image:{IMG_A}": "ok",
        f"image:{IMG_B}": "failed",
        "placement": "failed",
    }
    assert "exists=True" in report.check(f"image:{IMG_A}").detail
    assert f"the base template '{BASE}' does not exist" in report.check(f"image:{IMG_B}").detail
    assert "NotFound" in report.check("placement").detail


async def test_fl1_image_status_calls_are_bounded(fake_stack, monkeypatch):
    # FL-1: the GetImageStatus calls run together, with a bound on the
    # number in flight.
    client, fakes = fake_stack
    _setup(fakes)
    images = [f"docker.io/library/python@sha256:{i:064x}" for i in range(20)]
    in_flight = peak = 0
    real = Client.image_status

    async def counted(self, *a, **k):
        nonlocal in_flight, peak
        in_flight += 1
        peak = max(peak, in_flight)
        try:
            await asyncio.sleep(0.02)
            return await real(self, *a, **k)
        finally:
            in_flight -= 1

    monkeypatch.setattr(Client, "image_status", counted)
    tasks = [Task(id=str(i), image=image) for i, image in enumerate(images)]
    report = await asyncio.to_thread(_sync_report, client, tasks=tasks)
    assert [c.name for c in report.checks[1:-1]] == [f"image:{i}" for i in images]
    # The bound is the module's constant: this checks that one applies, not
    # its value.
    assert 1 < peak <= _PREFLIGHT_STATUS_CONCURRENCY


async def test_fl2_preflight_strict_names_each_failed_check(fake_stack):
    # FL-2: with several failed checks, the error names each one.
    client, fakes = fake_stack
    _setup(fakes)
    _golden(fakes, IMG_A, env_pb2.GOLDEN_SNAPSHOT_STATE_FAILED, "disk full")
    fakes.environments.domain_nodes = {}
    with pytest.raises(PreflightError) as err:
        await asyncio.to_thread(_sync_report, client, True)
    for name in (f"image:{IMG_A}", f"image:{IMG_B}", "placement"):
        assert name in str(err.value)


def test_fl2_observer_records_its_own_phase():
    # The report is the phase preflight_report, apart from preflight(): ok
    # when it returns (also with a failed check), error when strict raises.
    fleet = SandboxFleet(FleetConfig(backend="mock"))
    seen = []
    fleet._obs.record_phase = lambda name, dur, **k: seen.append((name, k["status"]))
    fleet.backend.fail_preflight = True
    fleet.preflight_report()
    with pytest.raises(PreflightError):
        fleet.preflight_report(strict=True)
    assert seen == [("preflight_report", "ok"), ("preflight_report", "error")]


async def test_fl4_async_observer_records_its_own_phase(fake_stack):
    # FL-4: the event-loop path (Substrate driver) records the same phase,
    # with the strict raise inside it.
    client, fakes = fake_stack
    _setup(fakes)
    fleet = AsyncSandboxFleet(_cfg(client))
    try:
        assert hasattr(fleet.backend, "preflight_async")
        seen = []
        fleet.observer.record_phase = lambda name, dur, **k: seen.append((name, k["status"]))
        fleet.load_tasks(_tasks())
        assert not (await fleet.preflight_report()).ok
        with pytest.raises(PreflightError):
            await fleet.preflight_report(strict=True)
        assert seen == [("preflight_report", "ok"), ("preflight_report", "error")]
    finally:
        await fleet.close_async()


async def test_fl2_preflight_strict_raises_failed_checks(fake_stack):
    # FL-2: the failed image check is reported without strict and raised
    # with it, by name.
    client, fakes = fake_stack
    _setup(fakes)
    report = await asyncio.to_thread(_sync_report, client)
    assert report.check(f"image:{IMG_B}").status == "failed"
    with pytest.raises(PreflightError, match=f"image:{IMG_B}"):
        await asyncio.to_thread(_sync_report, client, True)


def test_fl2_failed_connectivity_is_reported_and_still_fails_setup():
    # FL-2: preflight_report() reports a failed connectivity check (and
    # skips the rest); preflight() and setup() still raise as before.
    fleet = SandboxFleet(FleetConfig(backend="mock"))
    fleet.backend.fail_preflight = True
    fleet.load_tasks([Task(id="a", image=IMG_A)])
    report = fleet.preflight_report()
    assert _statuses(report) == {"connectivity": "failed", f"image:{IMG_A}": "skipped", "placement": "skipped"}
    with pytest.raises(PreflightError, match="connectivity"):
        fleet.preflight_report(strict=True)
    with pytest.raises(RuntimeError, match="cluster unreachable"):
        fleet.preflight()
    with pytest.raises(RuntimeError, match="cluster unreachable"):
        fleet.setup()


def test_fl2_setup_does_not_build_the_report():
    # setup() and run() keep calling preflight(), the connectivity check
    # alone: no image status or placement call on each run start.
    fleet = SandboxFleet(FleetConfig(backend="mock"))
    calls = []
    fleet.backend.preflight_checks = lambda images: calls.append(images) or []
    fleet.load_tasks([Task(id="a", image=IMG_A)])
    fleet.setup()
    fleet.run(lambda task, handle: task.id)
    assert calls == []


async def test_fl3_preflight_unimplemented_is_skipped(fake_stack, monkeypatch):
    # FL-3: an RPC that answers UNIMPLEMENTED makes its checks skipped, and
    # strict=True does not raise for them.
    client, fakes = fake_stack
    _setup(fakes)
    _golden(fakes, IMG_B, env_pb2.GOLDEN_SNAPSHOT_STATE_READY)

    async def unimplemented(self, *a, **k):
        raise RpcError("unknown method", grpc.StatusCode.UNIMPLEMENTED)

    monkeypatch.setattr(Client, "image_status", unimplemented)
    monkeypatch.setattr(Client, "list_placement_domains", unimplemented)
    report = await asyncio.to_thread(_sync_report, client, True)
    assert _statuses(report) == {
        "connectivity": "ok",
        f"image:{IMG_A}": "skipped",
        f"image:{IMG_B}": "skipped",
        "placement": "skipped",
    }
    assert report.ok


async def test_fl4_async_preflight_matches_sync(fake_stack):
    # FL-4: the async fleet reports the same checks for the same answers,
    # with the same strict behavior.
    client, fakes = fake_stack
    _setup(fakes)
    sync_report = await asyncio.to_thread(_sync_report, client)
    fleet = AsyncSandboxFleet(_cfg(client))
    try:
        fleet.load_tasks(_tasks())
        async_report = await fleet.preflight_report()
        assert async_report == sync_report
        with pytest.raises(PreflightError, match=f"image:{IMG_B}"):
            await fleet.preflight_report(strict=True)
    finally:
        await fleet.close_async()


async def test_fl4_async_failed_connectivity_matches_sync():
    fleet = AsyncSandboxFleet(FleetConfig(backend="mock"))
    fleet.backend.fail_preflight = True
    fleet.load_tasks([Task(id="a", image=IMG_A)])
    report = await fleet.preflight_report()
    assert _statuses(report) == {"connectivity": "failed", f"image:{IMG_A}": "skipped", "placement": "skipped"}
    with pytest.raises(PreflightError, match="connectivity"):
        await fleet.preflight_report(strict=True)


async def test_fl4_async_dead_endpoint_matches_sync():
    # FL-4 on the Substrate driver, which takes the event-loop path: an
    # endpoint that does not answer gives the sync report, strict included.
    cfg = FleetConfig(backend="substrate", endpoint="127.0.0.1:1", template=BASE)
    tasks = [Task(id="a", image=IMG_A)]

    def sync_report() -> PreflightReport:
        fleet = SandboxFleet(cfg)
        try:
            fleet.load_tasks(tasks)
            return fleet.preflight_report()
        finally:
            fleet.close()

    sync = await asyncio.to_thread(sync_report)
    assert _statuses(sync) == {"connectivity": "failed", f"image:{IMG_A}": "skipped", "placement": "skipped"}
    fleet = AsyncSandboxFleet(cfg)
    try:
        fleet.load_tasks(tasks)
        assert hasattr(fleet.backend, "preflight_async")
        report = await fleet.preflight_report()
        assert report == sync
        with pytest.raises(PreflightError, match="connectivity"):
            await fleet.preflight_report(strict=True)
    finally:
        await fleet.close_async()
