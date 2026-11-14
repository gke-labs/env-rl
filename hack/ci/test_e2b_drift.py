#!/usr/bin/env python3

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

"""Tests of hack/ci/e2b-drift.sh against a caller's shell that works against it.

Each class runs the script one time with SKIP_KIND=1 (no cluster). The script
installs from PyPI, thus the tests need the network and take some minutes;
they are skipped unless E2B_DRIFT_TEST=1. The e2b-drift workflow sets it.

Run:  E2B_DRIFT_TEST=1 python3 hack/ci/test_e2b_drift.py
"""

from __future__ import annotations

import base64
import hashlib
import os
import re
import subprocess
import tempfile
import unittest
import zipfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "hack" / "ci" / "e2b-drift.sh"
ENABLED = os.environ.get("E2B_DRIFT_TEST") == "1"
REASON = "needs the network and some minutes; set E2B_DRIFT_TEST=1"

# Variables of the caller that change what the script does.
CALLER_VARIABLES = (
    "E2B_DRIFT_VENV",
    "PIP_FIND_LINKS",
    "SUBSTRATE_E2E_API_URL",
    "E2B_E2E_TEMPLATE",
    "GITHUB_STEP_SUMMARY",
)


def write_index_wheel(directory: Path) -> None:
    """Writes ate_env_rl 0.2.0, newer than the checkout, that cannot be imported."""
    name = "ate_env_rl-0.2.0"
    files = {
        "ate_env_rl/__init__.py": 'raise ImportError("ate_env_rl from an index, not from the checkout")\n',
        f"{name}.dist-info/METADATA": "Metadata-Version: 2.1\nName: ate-env-rl\nVersion: 0.2.0\n",
        f"{name}.dist-info/WHEEL": (
            "Wheel-Version: 1.0\nGenerator: test\nRoot-Is-Purelib: true\nTag: py3-none-any\n"
        ),
    }
    record = []
    with zipfile.ZipFile(directory / f"{name}-py3-none-any.whl", "w") as wheel:
        for path, text in files.items():
            data = text.encode()
            digest = (
                base64.urlsafe_b64encode(hashlib.sha256(data).digest())
                .rstrip(b"=")
                .decode()
            )
            record.append(f"{path},sha256={digest},{len(data)}")
            wheel.writestr(path, data)
        record.append(f"{name}.dist-info/RECORD,,")
        wheel.writestr(f"{name}.dist-info/RECORD", "\n".join(record) + "\n")


def run_script(cwd: Path, **variables: str) -> subprocess.CompletedProcess[str]:
    env = {k: v for k, v in os.environ.items() if k not in CALLER_VARIABLES}
    env.update(variables, SKIP_KIND="1")
    return subprocess.run(
        ["bash", str(SCRIPT)],
        cwd=cwd,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        check=False,
    )


@unittest.skipUnless(ENABLED, REASON)
class HostileCaller(unittest.TestCase):
    """A relative E2B_DRIFT_VENV, a newer ate-env-rl on an index, and E2E variables."""

    @classmethod
    def setUpClass(cls) -> None:
        cls.tmp = tempfile.TemporaryDirectory()
        cls.cwd = Path(cls.tmp.name).resolve()
        wheels = cls.cwd / "wheels"
        wheels.mkdir()
        write_index_wheel(wheels)
        cls.result = run_script(
            cls.cwd,
            E2B_DRIFT_VENV="rel-venv",
            PIP_FIND_LINKS=str(wheels),
            # Port 9 (discard): a test that connects fails at once.
            SUBSTRATE_E2E_API_URL="127.0.0.1:9",
            E2B_E2E_TEMPLATE="e2b-base",
        )

    @classmethod
    def tearDownClass(cls) -> None:
        cls.tmp.cleanup()

    def test_a_relative_venv_is_relative_to_the_callers_directory(self) -> None:
        self.assertTrue(
            (self.cwd / "rel-venv" / "bin" / "python").exists(), self.result.stdout
        )
        self.assertNotIn("bin/python: No such file or directory", self.result.stdout)
        self.assertIn("==> Unit tests of integrations/e2b", self.result.stdout)

    def test_the_client_is_the_checkout_and_not_a_newer_release_on_an_index(
        self,
    ) -> None:
        python = self.cwd / "rel-venv" / "bin" / "python"
        where = subprocess.run(
            [str(python), "-c", "import ate_env_rl; print(ate_env_rl.__file__)"],
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertEqual(where.returncode, 0, where.stderr)
        client = (ROOT / "clients" / "python").resolve()
        self.assertTrue(
            Path(where.stdout.strip()).resolve().is_relative_to(client), where.stdout
        )

    def test_the_e2e_variables_of_the_caller_do_not_start_the_live_tests(self) -> None:
        self.assertEqual(self.result.returncode, 0, self.result.stdout)
        summary = re.search(r"(\d+) passed, (\d+) skipped", self.result.stdout)
        self.assertIsNotNone(summary, self.result.stdout)
        self.assertGreater(int(summary.group(2)), 0)
        self.assertNotIn("failed", summary.string[summary.start() : summary.end() + 40])


@unittest.skipUnless(ENABLED, REASON)
class DefaultVenv(unittest.TestCase):
    """With no E2B_DRIFT_VENV the script leaves no environment behind."""

    def test_the_default_environment_is_removed_at_exit(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            result = run_script(Path(tmp), TMPDIR=tmp)
            self.assertEqual(result.returncode, 0, result.stdout)
            left = [
                p.name
                for p in Path(tmp).iterdir()
                if p.name.startswith("e2b-drift-venv")
            ]
            self.assertEqual(left, [])


if __name__ == "__main__":
    unittest.main()
