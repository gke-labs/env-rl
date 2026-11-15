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

"""list_environments and EnvironmentInfo.create_time against the fake API.

The fake mirrors what docs/plans/ate-env-api/acceptance-criteria.md asks of
ate-env-api (AE-6 to AE-10); these tests check the client's side of it.
"""

from __future__ import annotations

from datetime import datetime, timedelta, timezone

import pytest

from ate_env_rl import EnvironmentPage, EnvironmentStatus
from ate_env_rl._gen.ateenv.v1alpha import env_pb2
from ate_env_rl.errors import InvalidArgumentError


async def test_list_environments_reads_one_atespace(fake_stack):
    client, _ = fake_stack
    for env_id in ("b", "a"):
        await client.create(env_id)
    await client.create("c", atespace="other")

    page = await client.list_environments()
    assert isinstance(page, EnvironmentPage)
    assert [e.id for e in page.environments] == ["a", "b"]
    assert page.next_page_token == ""
    assert page.environments[0] == await client.get("a")
    assert [e.id for e in (await client.list_environments(atespace="other")).environments] == ["c"]
    assert (await client.list_environments(atespace="empty")) == EnvironmentPage(environments=())


async def test_list_environments_follows_the_token(fake_stack):
    client, fakes = fake_stack
    for i in range(5):
        await client.create(f"env-{i}")

    seen, token, pages = [], "", 0
    while True:
        page = await client.list_environments(page_size=2, page_token=token)
        seen += [e.id for e in page.environments]
        pages += 1
        token = page.next_page_token
        if not token:
            break
    assert seen == [f"env-{i}" for i in range(5)]
    assert pages == 3

    with pytest.raises(InvalidArgumentError):
        await client.list_environments(page_token="nope")
    with pytest.raises(InvalidArgumentError):
        await client.list_environments(page_size=-1)


async def test_list_environments_page_size_outside_int32(fake_stack):
    """A page size that does not fit the int32 field behaves as a large or a
    negative one does: the server caps the first and refuses the second."""
    client, _ = fake_stack
    for i in range(3):
        await client.create(f"env-{i}")

    page = await client.list_environments(page_size=10**10)
    assert [e.id for e in page.environments] == ["env-0", "env-1", "env-2"]
    assert page.next_page_token == ""
    page = await client.list_environments(page_size=2**31)
    assert len(page.environments) == 3

    with pytest.raises(InvalidArgumentError):
        await client.list_environments(page_size=-(10**10))


async def test_list_environments_status_filter_keeps_the_token(fake_stack):
    client, _ = fake_stack
    for i in range(4):
        await client.create(f"env-{i}")
    await client.suspend("env-2")

    page = await client.list_environments(statuses=[EnvironmentStatus.SUSPENDED])
    assert [e.id for e in page.environments] == ["env-2"]
    assert page.environments[0].status == EnvironmentStatus.SUSPENDED
    both = await client.list_environments(
        statuses=(EnvironmentStatus.SUSPENDED, EnvironmentStatus.RUNNING)
    )
    assert len(both.environments) == 4

    # The first page of two holds no suspended environment and still has a
    # token; the second holds env-2.
    first = await client.list_environments(page_size=2, statuses=[EnvironmentStatus.SUSPENDED])
    assert first.environments == () and first.next_page_token
    second = await client.list_environments(
        page_size=2, page_token=first.next_page_token, statuses=[EnvironmentStatus.SUSPENDED]
    )
    assert [e.id for e in second.environments] == ["env-2"]
    assert second.next_page_token == ""


async def test_create_time_is_a_utc_datetime(fake_stack):
    client, fakes = fake_stack
    before = datetime.now(timezone.utc) - timedelta(seconds=1)
    await client.create("timed")
    info = await client.get("timed")
    assert info.create_time is not None and info.create_time.tzinfo is timezone.utc
    assert before <= info.create_time <= datetime.now(timezone.utc) + timedelta(seconds=1)
    listed = (await client.list_environments()).environments[0]
    assert listed.create_time == info.create_time

    # A server that predates the field reports none.
    fakes.environments.environments[("ate-env", "old")] = env_pb2.Environment(
        id="old", atespace="ate-env", status=env_pb2.ENVIRONMENT_STATUS_RUNNING
    )
    assert (await client.get("old")).create_time is None
