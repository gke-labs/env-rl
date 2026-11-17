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

"""The time to live of an environment against the fake API.

The fake mirrors what docs/plans/ate-env-api/acceptance-criteria.md asks of
ate-env-api (EM-1, EM-9, EM-10, the server half of EM-16); these tests check
the client's side of it: the create argument, ``set_time_to_live`` and
``EnvironmentInfo.expire_time``.
"""

from __future__ import annotations

from datetime import datetime, timedelta, timezone

import pytest

from ate_env_rl.errors import (
    FailedPreconditionError,
    InvalidArgumentError,
    NotFoundError,
)


async def test_create_with_time_to_live_reports_the_deadline(fake_stack):
    # EM-9: expire_time is the create time plus the time to live.
    client, fakes = fake_stack
    fakes.environments.time_to_live = True
    assert "time_to_live" in (await client.server_info()).features

    env = await client.create("timed", time_to_live=90)
    assert fakes.environments.last_create_request.time_to_live.ToTimedelta() == timedelta(seconds=90)
    info = await env.info()
    assert info.expire_time is not None and info.expire_time.tzinfo is timezone.utc
    assert info.expire_time == info.create_time + timedelta(seconds=90)
    listed = (await client.list_environments()).environments[0]
    assert listed.expire_time == info.expire_time

    # A timedelta is the same argument.
    await client.create("delta", time_to_live=timedelta(minutes=2))
    assert fakes.environments.last_create_request.time_to_live.ToTimedelta() == timedelta(minutes=2)


async def test_no_time_to_live_sends_no_field(fake_stack):
    # None and zero mean no deadline: the request carries no time_to_live,
    # thus a server that predates the field takes the create.
    client, fakes = fake_stack
    for env_id, ttl in (("none", None), ("zero", 0), ("zero-delta", timedelta(0))):
        await client.create(env_id, time_to_live=ttl)
        assert not fakes.environments.last_create_request.HasField("time_to_live")
        assert (await client.get(env_id)).expire_time is None


async def test_set_time_to_live_moves_and_removes_the_deadline(fake_stack):
    # EM-10: the new deadline counts from the call; None or zero removes it.
    client, fakes = fake_stack
    fakes.environments.time_to_live = True
    env = await client.create("timed")
    assert (await env.info()).expire_time is None

    before = datetime.now(timezone.utc)
    info = await client.set_time_to_live("timed", 3600)
    after = datetime.now(timezone.utc)
    assert info.id == "timed"
    assert before + timedelta(hours=1) <= info.expire_time <= after + timedelta(hours=1)
    assert (await env.info()).expire_time == info.expire_time

    moved = await env.set_time_to_live(timedelta(hours=2))
    assert moved.expire_time > info.expire_time

    for removed in (None, 0):
        await env.set_time_to_live(60)
        assert (await env.set_time_to_live(removed)).expire_time is None
        assert (await env.info()).expire_time is None

    with pytest.raises(InvalidArgumentError):
        await env.set_time_to_live(-1)
    with pytest.raises(NotFoundError):
        await client.set_time_to_live("unknown", 60)


async def test_time_to_live_is_refused_without_the_store(fake_stack):
    # EM-1: with the store off, a time to live is FAILED_PRECONDITION and
    # creates or changes nothing; the feature is not listed.
    client, fakes = fake_stack
    assert "time_to_live" not in (await client.server_info()).features
    with pytest.raises(FailedPreconditionError):
        await client.create("timed", time_to_live=60)
    assert ("ate-env", "timed") not in fakes.environments.environments
    with pytest.raises(InvalidArgumentError):
        await client.create("negative", time_to_live=-1)

    env = await client.create("plain")
    with pytest.raises(FailedPreconditionError):
        await env.set_time_to_live(60)
    assert (await env.info()).expire_time is None


async def test_seconds_below_one_microsecond_are_refused(fake_stack):
    # A timedelta rounds 0.4 microseconds to zero, which would mean "no
    # deadline": the client refuses the value before any RPC.
    client, fakes = fake_stack
    fakes.environments.time_to_live = True
    with pytest.raises(ValueError):
        await client.create("tiny", time_to_live=4e-7)
    assert ("ate-env", "tiny") not in fakes.environments.environments
    env = await client.create("timed", time_to_live=60)
    with pytest.raises(ValueError):
        await env.set_time_to_live(4e-7)
    assert (await env.info()).expire_time is not None
