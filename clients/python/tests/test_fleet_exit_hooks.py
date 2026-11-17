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

import asyncio
import os
import signal
import subprocess
import sys
import textwrap
import threading
import time

import pytest

from ate_env_rl import AsyncSandboxFleet, FleetConfig, SandboxFleet, Task
from ate_env_rl.backend.mock import MockBackendDriver
from ate_env_rl.types import RawSandboxInstance

# Each test that installs a signal handler or an atexit hook runs in a child
# interpreter, so the handlers of the pytest process are never touched.
_PRELUDE = """
import atexit, os, signal, sys, threading, time
from ate_env_rl import AsyncSandboxFleet, FleetConfig, SandboxFleet
from ate_env_rl.backend.mock import MockBackendDriver

def say(*a):
    print(*a, flush=True)

class TrackingDriver(MockBackendDriver):
    def __init__(self, name="f", fail_reap=False):
        super().__init__()
        self.name, self.fail_reap, self.reaps = name, fail_reap, 0

    def reap(self, run_id):
        self.reaps += 1
        say(f"reap {self.name} pid={os.getpid()}")
        if self.fail_reap:
            raise RuntimeError("reap failed")
        return super().reap(run_id)

def hooked(driver):
    return SandboxFleet(FleetConfig(backend="mock", exit_hooks=True), driver=driver)

def wait_for_signal():
    for _ in range(25):
        time.sleep(0.005)
"""


def _run(body: str, timeout: float = 60) -> subprocess.CompletedProcess:
    code = _PRELUDE + textwrap.dedent(body)
    return subprocess.run(
        [sys.executable, "-c", code], capture_output=True, text=True, timeout=timeout
    )


def _lines(result: subprocess.CompletedProcess) -> list[str]:
    return result.stdout.splitlines()


def test_fl5_exit_hooks_tear_down_once():
    r = _run("""
        d = TrackingDriver()
        atexit.register(lambda: say(f"reaps={d.reaps}"))  # runs after the fleet's hook
        hooked(d).acquire("t1")
    """)
    assert r.returncode == 0, r.stderr
    assert [line.split(" pid=")[0] for line in _lines(r)] == ["reap f", "reaps=1"]

    # The default (False) installs no handler and no atexit hook.
    r = _run("""
        d = TrackingDriver()
        before = (signal.getsignal(signal.SIGINT), signal.getsignal(signal.SIGTERM))
        SandboxFleet(FleetConfig(backend="mock"), driver=d).acquire("t1")
        after = (signal.getsignal(signal.SIGINT), signal.getsignal(signal.SIGTERM))
        atexit.register(lambda: say(f"reaps={d.reaps} same={before == after}"))
    """)
    assert r.returncode == 0, r.stderr
    assert _lines(r) == ["reaps=0 same=True"]


def test_fl5_default_sigterm_tears_down_and_terminates():
    r = _run("""
        signal.signal(signal.SIGTERM, signal.SIG_DFL)
        d = TrackingDriver()
        hooked(d).acquire("t1")
        os.kill(os.getpid(), signal.SIGTERM)
        wait_for_signal()
        say("STILL ALIVE")
    """)
    assert r.returncode == -signal.SIGTERM, (r.stdout, r.stderr)
    assert [line.split()[0] for line in _lines(r)] == ["reap"]


def test_fl6_default_sigint_tears_down_and_raises_keyboard_interrupt():
    r = _run("""
        signal.signal(signal.SIGINT, signal.default_int_handler)
        d = TrackingDriver()
        hooked(d).acquire("t1")
        try:
            os.kill(os.getpid(), signal.SIGINT)
            wait_for_signal()
            say("no KeyboardInterrupt")
        except KeyboardInterrupt:
            say("KeyboardInterrupt")
        say(f"reaps={d.reaps} restored={signal.getsignal(signal.SIGINT) is signal.default_int_handler}")
    """)
    assert r.returncode == 0, r.stderr
    assert _lines(r)[1:] == ["KeyboardInterrupt", "reaps=1 restored=True"]


def test_fl6_signal_handler_chains_and_runs_once():
    r = _run("""
        seen = []
        def prev(signum, frame):
            seen.append(signum)
        signal.signal(signal.SIGTERM, prev)
        d = TrackingDriver()
        hooked(d)
        os.kill(os.getpid(), signal.SIGTERM)
        wait_for_signal()
        say(f"seen={len(seen)} reaps={d.reaps} restored={signal.getsignal(signal.SIGTERM) is prev}")
        os.kill(os.getpid(), signal.SIGTERM)
        wait_for_signal()
        say(f"seen={len(seen)} reaps={d.reaps}")
    """)
    assert r.returncode == 0, r.stderr
    assert _lines(r)[1:] == ["seen=1 reaps=1 restored=True", "seen=2 reaps=1"]


@pytest.mark.parametrize("close", ["teardown", "close"])
def test_fl6_torn_down_fleet_restores_the_handlers_and_does_not_tear_down_again(close):
    r = _run(f"""
        seen = []
        def prev(signum, frame):
            seen.append(signum)
        signal.signal(signal.SIGTERM, prev)
        signal.signal(signal.SIGINT, prev)
        d = TrackingDriver()
        f = hooked(d)
        installed = signal.getsignal(signal.SIGTERM) is not prev
        f.{close}()
        restored = (signal.getsignal(signal.SIGTERM) is prev, signal.getsignal(signal.SIGINT) is prev)
        os.kill(os.getpid(), signal.SIGTERM)
        wait_for_signal()
        atexit.register(lambda: say(f"installed={{installed}} restored={{restored}} seen={{len(seen)}} reaps={{d.reaps}}"))
    """)
    assert r.returncode == 0, r.stderr
    assert _lines(r)[1:] == ["installed=True restored=(True, True) seen=1 reaps=1"]


def test_fl6_teardown_error_is_logged_and_the_chain_still_runs():
    # A custom earlier handler is still called.
    r = _run("""
        seen = []
        signal.signal(signal.SIGTERM, lambda s, f: seen.append(s))
        hooked(TrackingDriver(fail_reap=True))
        os.kill(os.getpid(), signal.SIGTERM)
        wait_for_signal()
        say(f"seen={len(seen)}")
    """)
    assert r.returncode == 0, r.stderr
    assert _lines(r)[1] == "seen=1"
    assert "teardown at signal SIGTERM failed" in r.stderr
    assert "reap failed" in r.stderr

    # The default disposition still stops the process.
    r = _run("""
        signal.signal(signal.SIGTERM, signal.SIG_DFL)
        hooked(TrackingDriver(fail_reap=True))
        os.kill(os.getpid(), signal.SIGTERM)
        wait_for_signal()
        say("STILL ALIVE")
    """)
    assert r.returncode == -signal.SIGTERM, (r.stdout, r.stderr)
    assert "STILL ALIVE" not in r.stdout
    assert "teardown at signal SIGTERM failed" in r.stderr

    # A KeyboardInterrupt out of the teardown still runs the earlier handler.
    r = _run("""
        seen = []
        signal.signal(signal.SIGTERM, lambda s, f: seen.append(s))
        class InterruptedDriver(TrackingDriver):
            def reap(self, run_id):
                raise KeyboardInterrupt
        hooked(InterruptedDriver())
        try:
            os.kill(os.getpid(), signal.SIGTERM)
            wait_for_signal()
        except KeyboardInterrupt:
            say("KeyboardInterrupt")
        say(f"seen={len(seen)}")
    """)
    assert r.returncode == 0, r.stderr
    assert _lines(r) == ["KeyboardInterrupt", "seen=1"]


@pytest.mark.parametrize("order", ["ab", "ba"])
def test_fl5_two_fleets_share_one_handler_in_each_teardown_order(order):
    # A custom earlier handler: the fleet still live tears down on the signal,
    # and the last teardown restores the earlier handler.
    r = _run(f"""
        seen = []
        def prev(signum, frame):
            seen.append(signum)
        signal.signal(signal.SIGTERM, prev)
        drivers = {{"a": TrackingDriver("a"), "b": TrackingDriver("b")}}
        fleets = {{k: hooked(d) for k, d in drivers.items()}}
        first, second = "{order}"
        fleets[first].teardown()
        say(f"installed={{signal.getsignal(signal.SIGTERM) is not prev}}")
        os.kill(os.getpid(), signal.SIGTERM)
        wait_for_signal()
        say(f"seen={{len(seen)}} restored={{signal.getsignal(signal.SIGTERM) is prev}}")
        fleets[second].teardown()
        os.kill(os.getpid(), signal.SIGTERM)
        wait_for_signal()
        say(f"seen={{len(seen)}} a={{drivers['a'].reaps}} b={{drivers['b'].reaps}}")
    """)
    first, second = order
    assert r.returncode == 0, r.stderr
    assert [line.split(" pid=")[0] for line in _lines(r)] == [
        f"reap {first}",
        "installed=True",
        f"reap {second}",
        "seen=1 restored=True",
        f"reap {second}",
        # The second fleet's own teardown reaps again; the signal reaped it once.
        "seen=2 a=1 b=2" if order == "ab" else "seen=2 a=2 b=1",
    ]

    # The default disposition: after both teardowns a SIGTERM stops the process.
    r = _run(f"""
        signal.signal(signal.SIGTERM, signal.SIG_DFL)
        fleets = {{"a": hooked(TrackingDriver("a")), "b": hooked(TrackingDriver("b"))}}
        for k in "{order}":
            fleets[k].teardown()
        say(f"default={{signal.getsignal(signal.SIGTERM) == signal.SIG_DFL}}")
        os.kill(os.getpid(), signal.SIGTERM)
        wait_for_signal()
        say("STILL ALIVE")
    """)
    assert r.returncode == -signal.SIGTERM, (r.stdout, r.stderr)
    assert [line.split(" pid=")[0] for line in _lines(r)] == [
        f"reap {first}",
        f"reap {second}",
        "default=True",
    ]


def test_fl6_teardown_off_the_main_thread_keeps_the_chain():
    # A teardown on a worker thread cannot restore the handlers; a later
    # setup() on the main thread must not take the fleet's own handler for
    # the earlier one.
    r = _run("""
        signal.signal(signal.SIGINT, signal.default_int_handler)
        d = TrackingDriver()
        f = hooked(d)
        t = threading.Thread(target=f.teardown)
        t.start()
        t.join()
        f.setup([])
        try:
            os.kill(os.getpid(), signal.SIGINT)
            wait_for_signal()
            say("no KeyboardInterrupt")
        except KeyboardInterrupt:
            say("KeyboardInterrupt")
        say(f"reaps={d.reaps} restored={signal.getsignal(signal.SIGINT) is signal.default_int_handler}")
    """)
    assert r.returncode == 0, r.stderr
    assert _lines(r)[2:] == ["KeyboardInterrupt", "reaps=2 restored=True"]


def test_fl5_setup_and_run_register_again():
    r = _run("""
        def prev(signum, frame):
            pass
        signal.signal(signal.SIGTERM, prev)
        d = TrackingDriver()
        f = hooked(d)
        f.teardown()
        say(f"after teardown={signal.getsignal(signal.SIGTERM) is not prev}")
        f.setup([])
        say(f"after setup={signal.getsignal(signal.SIGTERM) is not prev}")
        f.teardown()
        f.load_tasks([{"id": "t1", "image": "img"}])
        # The strategy tears the fleet down when the run ends: look from inside.
        f.run(lambda task, handle: say(f"in run={signal.getsignal(signal.SIGTERM) is not prev}"))
        say(f"after run={signal.getsignal(signal.SIGTERM) is not prev}")
        atexit.register(lambda: say(f"reaps={d.reaps}"))
    """)
    assert r.returncode == 0, r.stderr
    out = [line for line in _lines(r) if not line.startswith("reap ")]
    assert out == [
        "after teardown=False", "after setup=True", "in run=True", "after run=False", "reaps=3",
    ]


def test_fl7_no_signal_handler_outside_main_thread():
    r = _run("""
        before = (signal.getsignal(signal.SIGINT), signal.getsignal(signal.SIGTERM))
        d = TrackingDriver()
        errors = []
        def make():
            try:
                hooked(d)
            except Exception as e:
                errors.append(e)
        t = threading.Thread(target=make)
        t.start()
        t.join()
        after = (signal.getsignal(signal.SIGINT), signal.getsignal(signal.SIGTERM))
        say(f"errors={errors} same={before == after}")
    """)
    assert r.returncode == 0, r.stderr
    # The atexit hook still tears the fleet down.
    assert [line.split(" pid=")[0] for line in _lines(r)] == ["errors=[] same=True", "reap f"]


def test_fl6_signal_handler_does_not_deadlock_while_fleet_lock_is_held():
    # A signal handler runs on the main thread between bytecodes, possibly
    # while that thread holds the fleet's _lock (a plain Lock). The handler's
    # teardown must not wait on it.
    r = _run("""
        seen = []
        signal.signal(signal.SIGTERM, lambda s, f: seen.append(s))
        d = TrackingDriver()
        f = hooked(d)
        f.acquire("t1")
        with f._lock:
            os.kill(os.getpid(), signal.SIGTERM)
            wait_for_signal()
        say(f"seen={len(seen)} reaps={d.reaps}")
    """)
    assert r.returncode == 0, r.stderr
    assert _lines(r)[1:] == ["seen=1 reaps=1"]


@pytest.mark.skipif(not hasattr(os, "fork"), reason="needs os.fork")
def test_fl5_forked_child_does_not_tear_down_the_parent_run():
    r = _run("""
        signal.signal(signal.SIGTERM, signal.SIG_DFL)
        d = TrackingDriver()
        f = hooked(d)
        f.acquire("t1")
        pid = os.fork()
        if pid == 0:
            os.kill(os.getpid(), signal.SIGTERM)
            wait_for_signal()
            os._exit(3)
        _, status = os.waitpid(pid, 0)
        say(f"child1 sigterm={os.WIFSIGNALED(status) and os.WTERMSIG(status) == signal.SIGTERM}")
        pid = os.fork()
        if pid == 0:
            sys.exit(0)  # runs the atexit hooks of the child
        _, status = os.waitpid(pid, 0)
        say(f"child2 exit={os.WEXITSTATUS(status)}")
        f.teardown()
    """)
    assert r.returncode == 0, r.stderr
    # Only the parent's own teardown reaps; a child's reap line would come first.
    assert [line.split(" pid=")[0] for line in _lines(r)] == [
        "child1 sigterm=True",
        "child2 exit=0",
        "reap f",
    ]


def test_fl5_async_fleet_refuses_exit_hooks():
    with pytest.raises(ValueError, match="issues/398"):
        AsyncSandboxFleet(FleetConfig(backend="mock", exit_hooks=True))
    with pytest.raises(ValueError, match="issues/398"):
        AsyncSandboxFleet(FleetConfig(backend="substrate", exit_hooks=True))
    # Also for a sync fleet passed in (made in a child, since it installs hooks).
    r = _run("""
        try:
            AsyncSandboxFleet(sync_fleet=hooked(TrackingDriver()))
        except ValueError as e:
            say("refused" if "issues/398" in str(e) else str(e))
    """)
    assert _lines(r)[0] == "refused", r.stderr


class _PoolingDriver(MockBackendDriver):
    """A mock whose recycled sandboxes go back to a pool that acquire claims from."""

    def __init__(self):
        super().__init__()
        self.pool: list[str] = []
        self.calls: list[tuple[str, bool]] = []

    def acquire(self, template_id, run_id, timeout_s=180.0, rootfs_image=None):
        if self.pool:
            inst = self.instances[self.pool.pop()]
            return RawSandboxInstance(
                instance_id=inst.instance_id, endpoint=inst.endpoint, template_id=template_id,
                run_id=run_id, metadata={**inst.metadata, "claim_source": "warm_pool"},
                rootfs_image=rootfs_image,
            )
        return super().acquire(template_id, run_id, timeout_s=timeout_s, rootfs_image=rootfs_image)

    def release(self, instance_id, recycle=False):
        self.calls.append((instance_id, recycle))
        super().release(instance_id, recycle=recycle)
        if recycle:
            self.pool.append(instance_id)


async def test_fl8_async_recycle_returns_to_pool():
    driver = _PoolingDriver()
    fleet = AsyncSandboxFleet(driver=driver)
    with fleet.observer.run("naive") as report:
        handle = await fleet.acquire(Task("t1", "image1"))
        await fleet.recycle(handle)
        assert driver.calls == [(handle.sandbox_id, True)]
        again = await fleet.acquire(Task("t2", "image1"))
    assert again.sandbox_id == handle.sandbox_id
    assert again.claim_source == "warm_pool"
    assert report.phases["recycle"][0] == 1
    assert "release" not in report.phases
    # A second recycle, or a release after it, does nothing.
    await fleet.recycle(handle)
    await fleet.release(handle)
    assert driver.calls == [(handle.sandbox_id, True)]


def test_fl9_release_all_releases_outstanding_handles():
    driver = _PoolingDriver()
    fleet = SandboxFleet(driver=driver)
    fleet.acquire("t1")
    h2 = fleet.acquire("t2")
    fleet.acquire("t3")
    fleet.recycle(h2)
    assert fleet.release_all() == 2
    assert len(driver.calls) == 3
    assert fleet.release_all() == 0
    assert len(driver.calls) == 3


async def test_fl9_release_all_releases_outstanding_handles_async():
    driver = _PoolingDriver()
    fleet = AsyncSandboxFleet(driver=driver)
    h1 = await fleet.acquire("t1")
    await fleet.acquire("t2")
    await fleet.release(h1)
    assert await fleet.release_all() == 1
    assert len(driver.calls) == 2
    assert await fleet.release_all() == 0


class _FlakyDriver(MockBackendDriver):
    """Raises ``exc`` from the release calls whose 1-based number is in ``fail_on``."""

    def __init__(self, fail_on=(1,), exc=RuntimeError("boom")):
        super().__init__()
        self.fail_on, self.exc, self.calls = set(fail_on), exc, 0

    def release(self, instance_id, recycle=False):
        self.calls += 1
        if self.calls in self.fail_on:
            raise self.exc
        super().release(instance_id, recycle=recycle)


def test_fl9_failed_release_can_be_retried():
    driver = _FlakyDriver()
    fleet = SandboxFleet(driver=driver)
    h = fleet.acquire("t1")
    with pytest.raises(RuntimeError, match="boom"):
        fleet.release(h)
    fleet.release(h)
    assert driver.calls == 2
    assert driver.instances == {}
    assert fleet.release_all() == 0

    # release_all retries a handle whose release failed.
    h = fleet.acquire("t2")
    driver.fail_on = {driver.calls + 1}
    with pytest.raises(RuntimeError, match="boom"):
        fleet.recycle(h)
    assert fleet.release_all() == 1
    assert driver.instances == {}


async def test_fl9_failed_release_can_be_retried_async():
    driver = _FlakyDriver()
    fleet = AsyncSandboxFleet(driver=driver)
    h = await fleet.acquire("t1")
    with pytest.raises(RuntimeError, match="boom"):
        await fleet.release(h)
    await fleet.release(h)
    assert driver.calls == 2
    assert driver.instances == {}
    assert await fleet.release_all() == 0

    h = await fleet.acquire("t2")
    driver.fail_on = {driver.calls + 1}
    with pytest.raises(RuntimeError, match="boom"):
        await fleet.recycle(h)
    assert await fleet.release_all() == 1
    assert driver.instances == {}


def test_fl9_release_all_tries_every_handle_and_raises_the_first_error():
    driver = _FlakyDriver()
    fleet = SandboxFleet(driver=driver)
    for i in range(3):
        fleet.acquire(f"t{i}")
    with pytest.raises(RuntimeError, match="boom"):
        fleet.release_all()
    assert driver.calls == 3
    assert len(driver.instances) == 1
    assert fleet.release_all() == 1  # the failed one, again
    assert driver.instances == {}


async def test_fl9_async_release_all_tries_every_handle_and_raises_the_first_error():
    class SlowFlakyDriver(_FlakyDriver):
        async def release_async(self, instance_id, recycle=False):
            # The first release fails at once; the others finish later, and
            # release_all waits for them before it raises.
            if self.calls > 0:
                await asyncio.sleep(0.05)
            self.release(instance_id, recycle=recycle)

    driver = SlowFlakyDriver()
    fleet = AsyncSandboxFleet(driver=driver)
    for i in range(3):
        await fleet.acquire(f"t{i}")
    with pytest.raises(RuntimeError, match="boom"):
        await fleet.release_all()
    assert driver.calls == 3
    assert len(driver.instances) == 1
    assert await fleet.release_all() == 1
    assert driver.instances == {}


def test_fl9_keyboard_interrupt_in_release_all_leaves_handles_retryable():
    driver = _FlakyDriver(exc=KeyboardInterrupt())
    fleet = SandboxFleet(driver=driver)
    for i in range(3):
        fleet.acquire(f"t{i}")
    with pytest.raises(KeyboardInterrupt):
        fleet.release_all()
    assert driver.calls == 1
    assert fleet.release_all() == 3
    assert driver.instances == {}


async def test_fl9_cancelled_async_release_all_leaves_handles_retryable():
    class SlowDriver(MockBackendDriver):
        def __init__(self):
            super().__init__()
            self.started = 0
            self.slow = True

        async def release_async(self, instance_id, recycle=False):
            self.started += 1
            if self.slow:
                await asyncio.sleep(30)
            self.release(instance_id, recycle=recycle)

    driver = SlowDriver()
    fleet = AsyncSandboxFleet(driver=driver)
    for i in range(3):
        await fleet.acquire(f"t{i}")
    task = asyncio.create_task(fleet.release_all())
    while driver.started < 3:
        await asyncio.sleep(0.01)
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    assert len(driver.instances) == 3
    driver.slow = False
    assert await fleet.release_all() == 3
    assert driver.instances == {}


def test_handle_release_thread_safety_and_idempotency():
    class TrackingDriver(MockBackendDriver):
        def __init__(self):
            super().__init__()
            self.releases = []

        def release(self, instance_id, recycle=False):
            self.releases.append(instance_id)
            time.sleep(0.01)  # to encourage race conditions

    driver = TrackingDriver()
    fleet = SandboxFleet(driver=driver)
    handle = fleet.acquire("t1")

    threads = [threading.Thread(target=handle.release) for _ in range(2)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert len(driver.releases) == 1

    # Recycle after release should be no-op
    handle.recycle()
    assert len(driver.releases) == 1


def test_second_release_records_no_phase():
    fleet = SandboxFleet(driver=MockBackendDriver())
    h = fleet.acquire("t1")
    with fleet.observer.run("naive") as report:
        fleet.release(h)
        fleet.release(h)
        fleet.recycle(h)
    assert report.phases["release"][0] == 1
    assert "recycle" not in report.phases


async def test_second_release_records_no_phase_async():
    fleet = AsyncSandboxFleet(driver=MockBackendDriver())
    h = await fleet.acquire("t1")
    with fleet.observer.run("naive") as report:
        await fleet.release(h)
        await fleet.release(h)
        await fleet.recycle(h)
    assert report.phases["release"][0] == 1
    assert "recycle" not in report.phases


def test_release_all_after_teardown_calls_no_backend():
    driver = _PoolingDriver()
    fleet = SandboxFleet(driver=driver)
    fleet.acquire("t1")
    fleet.teardown()
    assert fleet.release_all() == 0
    assert driver.calls == []


async def test_release_all_after_teardown_calls_no_backend_async():
    driver = _PoolingDriver()
    fleet = AsyncSandboxFleet(driver=driver)
    await fleet.acquire("t1")
    await fleet.teardown()
    assert await fleet.release_all() == 0
    assert driver.calls == []


async def test_fl9_async_release_all_counts_only_released_by_this_call():
    driver = _PoolingDriver()
    fleet = AsyncSandboxFleet(driver=driver)
    h = await fleet.acquire("t1")
    assert h._begin_release()
    try:
        assert await fleet.release_all() == 0
    finally:
        h._end_release(True)
    assert await fleet.release_all() == 0


def test_fl6_failed_teardown_retries_at_exit():
    r = _run("""
        d = TrackingDriver()
        f = hooked(d)
        f.acquire("t1")
        d.fail_reap = True
        try:
            f.teardown()
            say("no error")
        except RuntimeError:
            say("teardown failed")
        say(f"reaps_after_first={d.reaps}")
        d.fail_reap = False
    """)
    assert r.returncode == 0, r.stderr
    assert [line.split(" pid=")[0] for line in _lines(r)] == [
        "reap f",
        "teardown failed",
        "reaps_after_first=1",
        "reap f",
    ]


def test_fl6_ignored_signal_does_not_tear_down():
    r = _run("""
        signal.signal(signal.SIGINT, signal.SIG_IGN)
        d = TrackingDriver()
        f = hooked(d)
        f.acquire("t1")
        os.kill(os.getpid(), signal.SIGINT)
        wait_for_signal()
        say(f"reaps={d.reaps} still_ign={signal.getsignal(signal.SIGINT) is signal.SIG_IGN}")
        f.teardown()
        say(f"after_teardown_reaps={d.reaps}")
    """)
    assert r.returncode == 0, r.stderr
    assert [line.split(" pid=")[0] for line in _lines(r)] == [
        "reaps=0 still_ign=True",
        "reap f",
        "after_teardown_reaps=1",
    ]


@pytest.mark.skipif(not hasattr(os, "fork"), reason="needs os.fork")
def test_fl5_forked_child_registering_fleet_clears_parent_fleets():
    r = _run("""
        signal.signal(signal.SIGTERM, signal.SIG_DFL)
        d_parent = TrackingDriver("parent")
        f_parent = hooked(d_parent)
        f_parent.acquire("t_parent")
        pid = os.fork()
        if pid == 0:
            d_child = TrackingDriver("child")
            f_child = hooked(d_child)
            f_child.acquire("t_child")
            sys.exit(0)
        _, status = os.waitpid(pid, 0)
        say(f"child exit={os.WEXITSTATUS(status)}")
        say(f"parent reaps before teardown={d_parent.reaps}")
        f_parent.teardown()
        say(f"parent reaps after teardown={d_parent.reaps}")
    """)
    assert r.returncode == 0, r.stderr
    assert [line.split(" pid=")[0] for line in _lines(r)] == [
        "reap child",
        "child exit=0",
        "parent reaps before teardown=0",
        "reap parent",
        "parent reaps after teardown=1",
    ]


def test_fl6_later_handler_not_overwritten_on_teardown():
    r = _run("""
        from ate_env_rl.fleet import _EXIT_HOOKS
        h0_calls = []
        def h0(signum, frame):
            h0_calls.append("h0")
        signal.signal(signal.SIGTERM, h0)
        d = TrackingDriver()
        f = hooked(d)

        h2_calls = []
        def h2(signum, frame):
            h2_calls.append("h2")
            _EXIT_HOOKS._on_signal(signum, frame)

        signal.signal(signal.SIGTERM, h2)
        f.teardown()

        current = signal.getsignal(signal.SIGTERM)
        say(f"current_is_h2={current is h2}")

        os.kill(os.getpid(), signal.SIGTERM)
        say(f"h2_calls={len(h2_calls)} h0_calls={len(h0_calls)}")
    """)
    assert r.returncode == 0, r.stderr
    assert [line.split(" pid=")[0] for line in _lines(r)] == [
        "reap f",
        "current_is_h2=True",
        "h2_calls=1 h0_calls=1",
    ]


def test_fl6_later_fleet_gets_the_handler_when_the_saved_handler_is_back():
    # Fleet A saves SIG_DFL; other code installs h and A's teardown leaves h,
    # keeping the saved SIG_DFL. Once SIG_DFL is current again, fleet B must
    # install the handler and tear down on SIGTERM.
    r = _run("""
        signal.signal(signal.SIGTERM, signal.SIG_DFL)
        a = hooked(TrackingDriver("a"))
        def h(signum, frame):
            pass
        signal.signal(signal.SIGTERM, h)
        a.teardown()
        signal.signal(signal.SIGTERM, signal.SIG_DFL)
        hooked(TrackingDriver("b")).acquire("t1")
        os.kill(os.getpid(), signal.SIGTERM)
        wait_for_signal()
        say("STILL ALIVE")
    """)
    assert r.returncode == -signal.SIGTERM, (r.stdout, r.stderr)
    assert [line.split(" pid=")[0] for line in _lines(r)] == ["reap a", "reap b"]


def test_fl6_later_fleet_gets_the_handler_after_a_restored_teardown():
    # A's teardown restores SIG_DFL and forgets it; other code then installs h.
    # Fleet B must save h and install the handler: the signal reaps B, then h.
    r = _run("""
        from ate_env_rl.fleet import _EXIT_HOOKS
        signal.signal(signal.SIGTERM, signal.SIG_DFL)
        a = hooked(TrackingDriver("a"))
        a.teardown()
        signal.signal(signal.SIGTERM, lambda s, f: say("h"))
        d = TrackingDriver("b")
        hooked(d).acquire("t1")
        say(f"installed={signal.getsignal(signal.SIGTERM) == _EXIT_HOOKS._on_signal}")
        os.kill(os.getpid(), signal.SIGTERM)
        wait_for_signal()
        say(f"reaps={d.reaps}")
    """)
    assert r.returncode == 0, r.stderr
    assert [line.split(" pid=")[0] for line in _lines(r)] == [
        "reap a",
        "installed=True",
        "reap b",
        "h",
        "reaps=1",
    ]


def test_fl6_signal_handler_does_not_deadlock_while_hooks_lock_is_held():
    # A signal that arrives while the main thread is in register() or
    # unregister() runs the handler with the registry's lock held. The
    # handler and the teardown it runs take that lock again.
    r = _run(
        """
        from ate_env_rl.fleet import _EXIT_HOOKS
        seen = []
        signal.signal(signal.SIGTERM, lambda s, f: seen.append(s))
        d = TrackingDriver()
        hooked(d).acquire("t1")
        with _EXIT_HOOKS._lock:
            os.kill(os.getpid(), signal.SIGTERM)
            wait_for_signal()
        say(f"seen={len(seen)} reaps={d.reaps}")
        """,
        timeout=20,
    )
    assert r.returncode == 0, r.stderr
    assert _lines(r)[1:] == ["seen=1 reaps=1"]


def test_teardown_reusability():
    # teardown() reaps on every call, also after an earlier teardown.
    class TrackingDriver(MockBackendDriver):
        def __init__(self):
            super().__init__()
            self.reaps = 0

        def reap(self, run_id):
            self.reaps += 1

    driver = TrackingDriver()
    fleet = SandboxFleet(driver=driver)
    fleet.acquire("t1")
    fleet.teardown()
    fleet.acquire("t2")
    fleet.teardown()
    assert driver.reaps == 2


async def test_async_teardown_reusability():
    # teardown() reaps on every call, also after an earlier teardown.
    class TrackingDriver(MockBackendDriver):
        def __init__(self):
            super().__init__()
            self.reaps = 0

        def reap(self, run_id):
            self.reaps += 1

    driver = TrackingDriver()
    fleet = AsyncSandboxFleet(driver=driver)
    await fleet.acquire("t1")
    await fleet.teardown()
    await fleet.acquire("t2")
    await fleet.teardown()
    assert driver.reaps == 2
