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

"""teardown(verify=True): FL-18 and FL-19 of docs/plans/fleet-sdk."""

from __future__ import annotations

import asyncio

import grpc
import pytest

from ate_env_rl import (
    AsyncSandboxFleet,
    EnvironmentStatus,
    FleetConfig,
    InfrastructureError,
    SandboxFleet,
    TeardownReport,
)
from ate_env_rl._gen.ateenv.v1alpha import env_pb2
from ate_env_rl.backend.base import REAP_TARGET_RUNS, BackendDriver
from ate_env_rl.backend.mock import MockBackendDriver
from ate_env_rl.backend.substrate import SubstrateBackendDriver
from ate_env_rl.errors import RpcError

IMG = "docker.io/library/python@sha256:" + "d" * 64


def _ids(fakes) -> set[str]:
    return {aid for (_space, aid) in fakes.environments.environments}


async def test_fl18_teardown_verify_reports_survivors(fake_stack):
    # FL-18: the report lists the targets of the reap that still exist, read
    # with ListEnvironments, with their status.
    client, fakes = fake_stack
    driver = SubstrateBackendDriver(client=client, atespace="ate-env")
    await driver.resize_pool_async("tmpl", 2, rootfs_image=IMG)
    claimed = await driver.acquire_async("tmpl", run_id="r1", rootfs_image=IMG)
    created = _ids(fakes)
    assert claimed.instance_id in created and len(created) == 2

    stuck = sorted(created)[0]
    fakes.environments.delete_fail.add(stuck)
    with pytest.raises(InfrastructureError):
        await driver.reap_async("r1")

    report = await driver.verify_teardown_async("r1")
    assert report == TeardownReport(
        checked=2,
        surviving=(stuck,),
        by_status={EnvironmentStatus(fakes.environments.environments[("ate-env", stuck)].status): 1},
    )
    assert not report.clean
    # The verify drops the target it read as gone and keeps the survivor.
    again = await driver.verify_teardown_async("r1")
    assert again.checked == 1 and again.surviving == (stuck,)


async def test_fl18_clean_teardown_reports_nothing_left(fake_stack):
    client, fakes = fake_stack
    driver = SubstrateBackendDriver(client=client, atespace="ate-env")
    await driver.acquire_async("tmpl", run_id="r1", rootfs_image=IMG)
    await driver.reap_async("r1")
    report = await driver.verify_teardown_async("r1")
    assert report.checked == 1 and report.clean and dict(report.by_status) == {}


async def test_fl19_teardown_verify_ignores_other_environments(fake_stack):
    # FL-19: an environment that this driver did not create is never listed,
    # also when it is in the same atespace and survives.
    client, fakes = fake_stack
    other = await client.create("someone-else", atespace="ate-env", template_name="tmpl")
    driver = SubstrateBackendDriver(client=client, atespace="ate-env")
    await driver.acquire_async("tmpl", run_id="r1", rootfs_image=IMG)
    await driver.reap_async("r1")
    report = await driver.verify_teardown_async("r1")
    assert ("ate-env", other.id) in fakes.environments.environments
    assert report.checked == 1 and report.surviving == ()


async def test_fl19_teardown_verify_checks_only_the_run(fake_stack):
    # Two runs on one driver: the second verify does not list the first
    # run's survivor.
    client, fakes = fake_stack
    driver = SubstrateBackendDriver(client=client, atespace="ate-env")
    first = await driver.acquire_async("tmpl", run_id="r1", rootfs_image=IMG)
    fakes.environments.delete_fail.add(first.instance_id)
    with pytest.raises(InfrastructureError):
        await driver.reap_async("r1")
    fakes.environments.delete_fail.clear()
    second = await driver.acquire_async("tmpl", run_id="r2", rootfs_image=IMG)
    await driver.reap_async("r2")
    report = await driver.verify_teardown_async("r2")
    assert first.instance_id not in report.surviving
    assert second.instance_id not in report.surviving


async def test_fl18_verify_falls_back_to_get_without_list_environments(fake_stack, monkeypatch):
    # Against an ate-env-api without ListEnvironments (UNIMPLEMENTED), each
    # target is read with GetEnvironment, in the driver's atespace; NotFound
    # means gone.
    client, fakes = fake_stack
    calls = {"list": 0}

    async def unimplemented(**kw):
        calls["list"] += 1
        raise RpcError("unknown method ListEnvironments", grpc.StatusCode.UNIMPLEMENTED)

    monkeypatch.setattr(client, "list_environments", unimplemented)
    driver = SubstrateBackendDriver(client=client, atespace="team-a")
    a = await driver.acquire_async("tmpl", run_id="r1", rootfs_image=IMG)
    b = await driver.acquire_async("tmpl", run_id="r1", rootfs_image=IMG)
    fakes.environments.delete_fail.add(a.instance_id)
    with pytest.raises(InfrastructureError):
        await driver.reap_async("r1")
    report = await driver.verify_teardown_async("r1")
    assert report.checked == 2
    assert report.surviving == (a.instance_id,)
    assert b.instance_id not in report.surviving
    assert calls["list"] == 1


def test_fl18_fleet_teardown_verify_returns_report_and_plain_teardown_returns_none():
    fleet = SandboxFleet(FleetConfig(backend="mock"))
    fleet.acquire("t1")
    report = fleet.teardown(verify=True)
    assert isinstance(report, TeardownReport) and report.checked == 1 and report.clean
    fleet.acquire("t2")
    assert fleet.teardown() is None


def test_fl18_failed_reap_keeps_raising_and_carries_the_report():
    driver = _StuckDriver()
    fleet = SandboxFleet(driver=driver)
    h = fleet.acquire("t1")
    with pytest.raises(InfrastructureError) as err:
        fleet.teardown(verify=True)
    report = err.value.teardown_report
    assert report.surviving == (h.sandbox_id,)
    assert dict(report.by_status) == {EnvironmentStatus.RUNNING: 1}
    # Gone out of band: the next verify reads it as gone and drops it, and
    # the one after checks nothing.
    del driver.instances[h.sandbox_id]
    assert driver.verify_teardown(fleet.run_id) == TeardownReport(checked=1)
    assert driver.verify_teardown(fleet.run_id) == TeardownReport(checked=0)


def test_fl18_verify_on_a_driver_without_support_raises():
    class Bare(BackendDriver):
        def preflight(self):
            pass

        def acquire(self, *a, **k):
            raise NotImplementedError

        def release(self, instance_id, recycle=False):
            pass

        def reap(self, run_id):
            return 0

        def warm_pool(self, *a, **k):
            pass

        def unwarm_pool(self, *a, **k):
            pass

        def ensure_template(self, *a, **k):
            return "tmpl"

    fleet = SandboxFleet(driver=Bare())
    with pytest.raises(NotImplementedError):
        fleet.teardown(verify=True)


async def test_fl18_async_fleet_teardown_verify():
    fleet = AsyncSandboxFleet(FleetConfig(backend="mock"))
    await fleet.acquire("t1")
    report = await fleet.teardown(verify=True)
    assert report.checked == 1 and report.clean
    await fleet.acquire("t2")
    assert await fleet.teardown() is None


class _StuckDriver(MockBackendDriver):
    """A mock whose reap targets the run's instances and deletes none."""

    def reap(self, run_id):
        self.reap_targets[run_id] = [i for i, inst in self.instances.items() if inst.run_id == run_id]
        raise InfrastructureError("could not delete")


class _VerifyFails(_StuckDriver):
    def verify_teardown(self, run_id):
        raise RpcError("ate-env-api is down", grpc.StatusCode.UNAVAILABLE)


class _NoVerify(BackendDriver):
    """A driver without verify_teardown whose reap raises."""

    def preflight(self):
        pass

    def acquire(self, *a, **k):
        raise NotImplementedError

    def release(self, instance_id, recycle=False):
        pass

    def reap(self, run_id):
        raise InfrastructureError("could not delete")

    def warm_pool(self, *a, **k):
        pass

    def unwarm_pool(self, *a, **k):
        pass

    def ensure_template(self, *a, **k):
        return "tmpl"


async def test_fl18_teardown_after_a_failed_teardown_reports_the_survivor(fake_stack):
    # FL-18: teardown() raises, then teardown(verify=True) reports the actor
    # whose delete failed, on an async fleet over the Substrate driver in an
    # atespace that is not the default. A later verify reports it again.
    client, fakes = fake_stack
    driver = SubstrateBackendDriver(client=client, atespace="team-a")
    fleet = AsyncSandboxFleet(FleetConfig(run_id="r1"), driver)
    a = await driver.acquire_async("tmpl", run_id="r1", rootfs_image=IMG)
    b = await driver.acquire_async("tmpl", run_id="r1", rootfs_image=IMG)
    fakes.environments.delete_fail.add(a.instance_id)
    with pytest.raises(InfrastructureError):
        await fleet.teardown()

    report = await fleet.teardown(verify=True)
    assert report.checked == 2 and report.surviving == (a.instance_id,)
    assert dict(report.by_status) == {EnvironmentStatus.RUNNING: 1}
    assert b.instance_id not in report.surviving and not report.clean
    again = await fleet.teardown(verify=True)
    assert again.checked == 1 and again.surviving == (a.instance_id,)


@pytest.mark.parametrize("fleet_kind", ["sync", "async"])
async def test_fl18_failed_verify_after_a_failed_reap_raises_the_reap_error(fleet_kind):
    # FL-18: when the reap raises and the verify raises too, the caller gets
    # the reap's error, with the verify's error as teardown_report_error.
    driver = _VerifyFails()
    if fleet_kind == "sync":
        fleet = SandboxFleet(driver=driver)
        fleet.acquire("t1")
        with pytest.raises(InfrastructureError) as err:
            fleet.teardown(verify=True)
    else:
        fleet = AsyncSandboxFleet(driver=driver)
        await fleet.acquire("t1")
        with pytest.raises(InfrastructureError) as err:
            await fleet.teardown(verify=True)
    assert isinstance(err.value.teardown_report_error, RpcError)
    assert err.value.teardown_report_error.code == grpc.StatusCode.UNAVAILABLE
    assert not hasattr(err.value, "teardown_report")


@pytest.mark.parametrize("fleet_kind", ["sync", "async"])
async def test_fl18_failed_reap_on_a_driver_without_verify_raises_the_reap_error(fleet_kind):
    if fleet_kind == "sync":
        with pytest.raises(InfrastructureError) as err:
            SandboxFleet(driver=_NoVerify()).teardown(verify=True)
    else:
        with pytest.raises(InfrastructureError) as err:
            await AsyncSandboxFleet(driver=_NoVerify()).teardown(verify=True)
    assert isinstance(err.value.teardown_report_error, NotImplementedError)


async def test_fl18_async_failed_reap_keeps_raising_and_carries_the_report():
    driver = _StuckDriver()
    fleet = AsyncSandboxFleet(driver=driver)
    h = await fleet.acquire("t1")
    with pytest.raises(InfrastructureError) as err:
        await fleet.teardown(verify=True)
    assert err.value.teardown_report.surviving == (h.sandbox_id,)


async def test_fl18_failed_list_raises_and_keeps_the_targets(fake_stack, monkeypatch):
    # A ListEnvironments error other than UNIMPLEMENTED is raised, with no
    # fallback, and the targets stay: the next verify reports the survivor.
    client, fakes = fake_stack
    driver = SubstrateBackendDriver(client=client, atespace="ate-env")
    a = await driver.acquire_async("tmpl", run_id="r1", rootfs_image=IMG)
    fakes.environments.delete_fail.add(a.instance_id)
    with pytest.raises(InfrastructureError):
        await driver.reap_async("r1")

    real_list, real_get = client.list_environments, client.get
    gets = []

    async def unavailable(**kw):
        raise RpcError("ate-env-api is down", grpc.StatusCode.UNAVAILABLE)

    async def counted_get(*args, **kw):
        gets.append(args)
        return await real_get(*args, **kw)

    monkeypatch.setattr(client, "list_environments", unavailable)
    monkeypatch.setattr(client, "get", counted_get)
    with pytest.raises(RpcError) as err:
        await driver.verify_teardown_async("r1")
    assert err.value.code == grpc.StatusCode.UNAVAILABLE and gets == []

    monkeypatch.setattr(client, "list_environments", real_list)
    report = await driver.verify_teardown_async("r1")
    assert report.checked == 1 and report.surviving == (a.instance_id,)


async def test_fl18_failed_fallback_read_keeps_the_targets(fake_stack, monkeypatch):
    # Without ListEnvironments, a GetEnvironment that fails raises, and the
    # targets stay: the next verify reports the survivor.
    client, fakes = fake_stack

    async def unimplemented(**kw):
        raise RpcError("unknown method ListEnvironments", grpc.StatusCode.UNIMPLEMENTED)

    monkeypatch.setattr(client, "list_environments", unimplemented)
    driver = SubstrateBackendDriver(client=client, atespace="ate-env")
    a = await driver.acquire_async("tmpl", run_id="r1", rootfs_image=IMG)
    fakes.environments.delete_fail.add(a.instance_id)
    with pytest.raises(InfrastructureError):
        await driver.reap_async("r1")

    real_get = client.get

    async def unavailable(*args, **kw):
        raise RpcError("ate-env-api is down", grpc.StatusCode.UNAVAILABLE)

    monkeypatch.setattr(client, "get", unavailable)
    with pytest.raises(RpcError):
        await driver.verify_teardown_async("r1")
    monkeypatch.setattr(client, "get", real_get)
    report = await driver.verify_teardown_async("r1")
    assert report.checked == 1 and report.surviving == (a.instance_id,)


async def test_fl18_verify_finds_a_target_on_the_second_page(fake_stack, monkeypatch):
    # 1000 other environments sort before the survivor, so it is on the
    # second page of 1000; the verify reads the atespace in two calls.
    client, fakes = fake_stack
    driver = SubstrateBackendDriver(client=client, atespace="ate-env")
    a = await driver.acquire_async("tmpl", run_id="r1", rootfs_image=IMG)
    fakes.environments.delete_fail.add(a.instance_id)
    with pytest.raises(InfrastructureError):
        await driver.reap_async("r1")
    for i in range(1000):
        env_id = f" other-{i:04d}"  # a space sorts before any generated id
        fakes.environments.environments[("ate-env", env_id)] = env_pb2.Environment(
            id=env_id, atespace="ate-env", status=env_pb2.ENVIRONMENT_STATUS_RUNNING
        )

    real_list = client.list_environments
    calls = []

    async def counted(**kw):
        calls.append(kw)
        return await real_list(**kw)

    monkeypatch.setattr(client, "list_environments", counted)
    report = await driver.verify_teardown_async("r1")
    assert report.surviving == (a.instance_id,)
    assert len(calls) == 2 and all(kw["page_size"] == 1000 for kw in calls)


async def test_fl18_fallback_reads_targets_concurrently(fake_stack, monkeypatch):
    # The fallback reads the targets as the reap deletes them: at most 16 at
    # once.
    client, fakes = fake_stack

    async def unimplemented(**kw):
        raise RpcError("unknown method ListEnvironments", grpc.StatusCode.UNIMPLEMENTED)

    monkeypatch.setattr(client, "list_environments", unimplemented)
    driver = SubstrateBackendDriver(client=client, atespace="ate-env")
    for _ in range(40):
        await driver.acquire_async("tmpl", run_id="r1", rootfs_image=IMG)
    await driver.reap_async("r1")

    real_get = client.get
    inflight = peak = 0

    async def slow_get(*args, **kw):
        nonlocal inflight, peak
        inflight += 1
        peak = max(peak, inflight)
        try:
            await asyncio.sleep(0.01)
            return await real_get(*args, **kw)
        finally:
            inflight -= 1

    monkeypatch.setattr(client, "get", slow_get)
    report = await driver.verify_teardown_async("r1")
    assert report.checked == 40 and report.clean
    assert 1 < peak <= 16


def test_fl18_mock_keeps_reap_targets_for_a_bounded_number_of_runs():
    driver = MockBackendDriver()
    fleets = [SandboxFleet(FleetConfig(run_id=f"r{i}"), driver) for i in range(REAP_TARGET_RUNS + 2)]
    for fleet in fleets[: REAP_TARGET_RUNS + 1]:
        fleet.acquire("t")
        fleet.teardown()
    assert len(driver.reap_targets) == REAP_TARGET_RUNS
    assert "r0" not in driver.reap_targets and f"r{REAP_TARGET_RUNS}" in driver.reap_targets
    # A run reaped again moves to the newest end: the limit drops the run
    # after it, not that one.
    fleets[1].acquire("t")
    fleets[1].teardown()
    fleets[-1].acquire("t")
    fleets[-1].teardown()
    assert len(driver.reap_targets) == REAP_TARGET_RUNS
    assert "r1" in driver.reap_targets and "r2" not in driver.reap_targets


async def test_fl18_substrate_keeps_reap_targets_for_a_bounded_number_of_runs(fake_stack):
    # A reap without a verify records its targets; past REAP_TARGET_RUNS runs
    # the least recently reaped is dropped.
    client, _fakes = fake_stack
    driver = SubstrateBackendDriver(client=client, atespace="ate-env")
    for i in range(REAP_TARGET_RUNS + 1):
        await driver.acquire_async("tmpl", run_id=f"r{i}", rootfs_image=IMG)
        await driver.reap_async(f"r{i}")
    assert len(driver._reap_targets) == REAP_TARGET_RUNS
    assert (await driver.verify_teardown_async("r0")) == TeardownReport(checked=0)
    last = await driver.verify_teardown_async(f"r{REAP_TARGET_RUNS}")
    assert last.checked == 1 and last.clean


async def test_fl18_second_reap_adds_its_failed_target_to_the_earlier_survivor(fake_stack):
    # A survivor of the first reap stays a target, and the second reap adds
    # its own failed delete: the verify reports both.
    client, fakes = fake_stack
    driver = SubstrateBackendDriver(client=client, atespace="ate-env")
    a = await driver.acquire_async("tmpl", run_id="r1", rootfs_image=IMG)
    fakes.environments.delete_fail.add(a.instance_id)
    with pytest.raises(InfrastructureError):
        await driver.reap_async("r1")
    b = await driver.acquire_async("tmpl", run_id="r1", rootfs_image=IMG)
    fakes.environments.delete_fail.add(b.instance_id)
    with pytest.raises(InfrastructureError):
        await driver.reap_async("r1")
    report = await driver.verify_teardown_async("r1")
    assert report.checked == 2
    assert report.surviving == (a.instance_id, b.instance_id)


async def test_fl19_report_counts_survivors_per_status(fake_stack):
    # FL-19: two survivors in one status count 2 for it.
    client, fakes = fake_stack
    driver = SubstrateBackendDriver(client=client, atespace="ate-env")
    held = [await driver.acquire_async("tmpl", run_id="r1", rootfs_image=IMG) for _ in range(3)]
    for h in held:
        fakes.environments.delete_fail.add(h.instance_id)
    fakes.environments.environments[("ate-env", held[2].instance_id)].status = (
        env_pb2.ENVIRONMENT_STATUS_SUSPENDED
    )
    with pytest.raises(InfrastructureError):
        await driver.reap_async("r1")
    report = await driver.verify_teardown_async("r1")
    assert report.checked == 3 and len(report.surviving) == 3
    assert dict(report.by_status) == {EnvironmentStatus.RUNNING: 2, EnvironmentStatus.SUSPENDED: 1}
