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

import json
from pathlib import Path

import grpc
import pytest

from ate_env_rl import (
    FailedPreconditionError,
    InvalidArgumentError,
    NotFoundError,
    PermissionDeniedError,
    ProcessExitedError,
    RpcError,
    SandboxStartError,
)
from ate_env_rl._gen.ateenv.v1alpha import env_pb2
from ate_env_rl.client import Client
from ate_env_rl.errors import map_rpc_error
from ate_env_rl.runtime.substrate_env_client import guest_infra_error
from ate_env_rl.telemetry import tracing


def repo_root() -> Path:
    d = Path(__file__).resolve().parent
    while True:
        if (d / "proto" / "testdata" / "client-parity").exists():
            return d
        if d.parent == d:
            raise RuntimeError("repo root not found")
        d = d.parent


def load_vector(name: str):
    with open(repo_root() / "proto" / "testdata" / "client-parity" / name) as f:
        return json.load(f)


@pytest.mark.parametrize("vector", load_vector("image_ref.json"))
def test_image_ref_attr(vector):
    # Python client image_ref_attr returns None instead of empty string for empty inputs
    assert tracing.image_ref_attr(vector["ref"]) == vector["want"]


@pytest.mark.parametrize("vector", load_vector("executable.json"))
def test_executable_attr(vector):
    # "argv" is a command given as words, "line" one for a shell.
    command = vector["argv"] if "argv" in vector else vector["line"]
    assert tracing.executable_attr(command) == vector["want"]


@pytest.mark.parametrize("vector", load_vector("semconv_mode.json"))
def test_rpc_semconv_mode(vector):
    assert tracing._rpc_semconv_mode(vector["value"]) == vector["want"]


@pytest.mark.parametrize("vector", load_vector("rpc_attrs.json"))
def test_rpc_attrs(vector, monkeypatch):
    mode = vector["mode"]
    monkeypatch.setenv("OTEL_SEMCONV_STABILITY_OPT_IN", mode)
    # The python client uses _RPC_SEMCONV_MODE internally which is set at import
    # we need to reload it or parse it
    if mode == "rpc/old":
        monkeypatch.setattr(tracing, "_RPC_SEMCONV_MODE", tracing._SEMCONV_OLD)
    elif mode == "rpc/dup":
        monkeypatch.setattr(tracing, "_RPC_SEMCONV_MODE", tracing._SEMCONV_DUP)
    else:
        monkeypatch.setattr(tracing, "_RPC_SEMCONV_MODE", tracing._SEMCONV_NEW)

    # An empty target in the vector is no target (None in Python).
    got = tracing.rpc_attrs(vector["service"], vector["method"], vector["target"] or None)
    assert got == vector["want"]


class _FakeRpcError(grpc.RpcError):
    def __init__(self, code, details, trailing=None):
        self._code, self._details, self._trailing = code, details, trailing

    def code(self):
        return self._code

    def details(self):
        return self._details

    def trailing_metadata(self):
        return self._trailing


@pytest.mark.parametrize("vector", load_vector("error_mapping.json"))
def test_error_mapping(vector):
    code_enum = [c for c in grpc.StatusCode if c.value[0] == vector["code"]][0]
    trailing = (
        (("ate-env-start-failure", vector["trailer"]),) if vector["trailer"] else ()
    )
    err = _FakeRpcError(code_enum, vector["message"], trailing)

    mapped = map_rpc_error(err)

    want = vector["want"]
    kind = want["kind"]
    expected_class = {
        "not_found": NotFoundError,
        "invalid_argument": InvalidArgumentError,
        "permission_denied": PermissionDeniedError,
        "failed_precondition": FailedPreconditionError,
        "process_exited": ProcessExitedError,
        "start_error": SandboxStartError,
        "rpc": RpcError,
    }[kind]

    assert isinstance(mapped, expected_class)

    if kind == "start_error":
        assert mapped.reason == want.get("reason")
        assert mapped.status == f"grpc {code_enum.name}"
        assert code_enum.value[0] == want["code"]

    # retryable is the runtime's call: exec_async turns each guest error into
    # an InfrastructureError, as the Go client's Retryable classifies it.
    assert guest_infra_error(mapped, "dev1").retryable == want["retryable"]


def test_metric_families(monkeypatch):
    pytest.importorskip("prometheus_client")
    from ate_env_rl.telemetry import metrics

    # Build the collectors without enable_client_metrics(), which is a
    # one-way, process-global switch, in histogram maps of this test's own,
    # as test_telemetry_metrics.py does, so the module's maps are untouched.
    monkeypatch.setattr(metrics, "_HISTOGRAMS", {})
    monkeypatch.setattr(metrics, "_SUBPHASE_HISTOGRAMS", {})
    metrics._histogram("exec")
    metrics._histogram("suspend")
    metrics._subphase_histogram("ready_to_first_command")

    vectors = load_vector("metric_families.json")
    for v in vectors:
        found = False
        name = v["name"]
        all_hists = {**metrics._HISTOGRAMS, **metrics._SUBPHASE_HISTOGRAMS}
        for key, hist in all_hists.items():
            if hist._name == name:
                found = True
                assert hist._documentation == v["help"]
                assert list(hist._labelnames) == v["labels"]
                assert list(hist._kwargs.get("buckets", [])) == v["buckets"]
                break
        assert found, f"metric family {name} not found in client metrics"


def test_rpc_coverage():
    vectors = load_vector("rpc_coverage.json")

    services = env_pb2.DESCRIPTOR.services_by_name
    env_service = services["EnvironmentService"]

    for method in env_service.methods:
        assert method.name in vectors, f"RPC {method.name} not found in coverage vector"
        mapping = vectors[method.name]
        # "image_status(ensure=True)": the method, and the call that sends the RPC.
        python_method_name = mapping["python"].split("(")[0]
        assert hasattr(Client, python_method_name), (
            f"Python Client missing method {python_method_name} for RPC {method.name}"
        )


SHARED_SPAN_OPERATIONS = {
    "create_environment",
    "get_environment",
    "list_environments",
    "set_environment_time_to_live",
    "suspend_environment",
    "delete_environment",
    "fork_environment",
    "get_server_info",
    "get_image_status",
    "ensure_image_template",
    "list_placement_domains",
    "warm_image",
    "start_process",
    "exec",
    "read_file",
    "write_file",
}


def test_spans_coverage():
    ops = {v["operation"] for v in load_vector("spans.json")}
    missing = SHARED_SPAN_OPERATIONS - ops
    assert not missing, f"spans.json missing shared operations: {sorted(missing)}"


@pytest.mark.parametrize(
    "v",
    load_vector("spans.json"),
    ids=lambda v: v.get("name", v["operation"]),
)
async def test_spans_parity(v, spans, fake_stack):
    """17.1 and 17.2: span names and ate.* / ate_env.* attributes from spans.json.

    The Go suite reads the same file (TestSpansParity). python_only entries
    add to the expected attributes here; go_only entries apply to Go only.
    """
    from ate_env_rl import CommandTimeoutError, TracerConfig
    from ate_env_rl._gen.ateenv.v1alpha import guest_pb2
    from ate_env_rl.errors import RpcError
    from ate_env_rl.handle import SandboxHandle
    from ate_env_rl.loop_runner import LoopRunner
    from ate_env_rl.runtime.substrate_env_client import SubstrateEnvClientRuntime
    from ate_env_rl.types import Task

    from .fakes import FakeProc

    client, fakes = fake_stack
    client.tracer = tracing.tracer_from_config(TracerConfig(enable_tracing=True))
    fakes.environments.template_selectors[("t-space", "layered")] = {}
    fakes.environments.template_selectors[("x-space", "layered")] = {}
    fakes.filesystem.files["notes.txt"] = b"hello world"

    op = v["operation"]
    inputs = v.get("inputs", {})

    id_val = inputs.get("id")
    atespace = inputs.get("atespace")
    template = inputs.get("template")
    template_atespace = inputs.get("template_atespace")
    rootfs = inputs.get("rootfs_image", "")
    path = inputs.get("path", "")
    content = inputs.get("content", "")
    command = inputs.get("command", [])
    timeout_ms = inputs.get("timeout_ms")

    if id_val and op != "create_environment":
        create_kwargs: dict = {"id": id_val}
        if atespace is not None:
            create_kwargs["atespace"] = atespace
        if template is not None:
            create_kwargs["template_name"] = template
        if template_atespace is not None:
            create_kwargs["template_atespace"] = template_atespace
        try:
            await client.create(**create_kwargs)
        except RpcError as e:
            if e.code != grpc.StatusCode.ALREADY_EXISTS:
                raise

    spans.clear()

    atespace_kw = {"atespace": atespace} if atespace is not None else {}
    env = client.env(id_val, **atespace_kw) if id_val else None
    if op == "create_environment":
        kwargs: dict = {"id": id_val}
        if atespace is not None:
            kwargs["atespace"] = atespace
        if template is not None:
            kwargs["template_name"] = template
        if template_atespace is not None:
            kwargs["template_atespace"] = template_atespace
        if rootfs:
            kwargs["rootfs_image"] = rootfs
        await client.create(**kwargs)
    elif op == "get_environment":
        await client.get(id_val, **atespace_kw)
    elif op == "list_environments":
        await client.list_environments(**atespace_kw)
    elif op == "set_environment_time_to_live":
        fake_stack[1].environments.time_to_live = True
        await client.set_time_to_live(id_val, 60, **atespace_kw)
    elif op == "suspend_environment":
        await client.suspend(id_val, **atespace_kw)
    elif op == "delete_environment":
        await client.delete(id_val, **atespace_kw)
    elif op == "fork_environment":
        await client.fork(id_val, 1, **atespace_kw)
    elif op == "get_server_info":
        await client.server_info()
    elif op in ("get_image_status", "ensure_image_template"):
        kwargs = {"ensure": op == "ensure_image_template"}
        if template is not None:
            kwargs["template_name"] = template
        if template_atespace is not None:
            kwargs["template_atespace"] = template_atespace
        if atespace is not None:
            kwargs["atespace"] = atespace
        await client.image_status(rootfs, **kwargs)
    elif op == "list_placement_domains":
        kwargs = {}
        if template is not None:
            kwargs["template_name"] = template
        if template_atespace is not None:
            kwargs["template_atespace"] = template_atespace
        if atespace is not None:
            kwargs["atespace"] = atespace
        await client.list_placement_domains(**kwargs)
    elif op == "warm_image":
        kwargs = {}
        if template is not None:
            kwargs["template_name"] = template
        if atespace is not None:
            kwargs["atespace"] = atespace
        if template_atespace is not None:
            kwargs["template_atespace"] = template_atespace
        await client.warm_image(rootfs, nodes=1, **kwargs)
    elif op == "start_process":
        await env.start_process(command)
    elif op == "exec":
        if timeout_ms is not None:
            fakes.processes.next_procs.append(
                FakeProc(signal=guest_pb2.SIGNAL_KILL, timed_out=True)
            )
        runtime = SubstrateEnvClientRuntime(
            env_id=id_val,
            atespace=atespace or "ate-env",
            env=env,
            loop_runner=LoopRunner(),
        )
        h = SandboxHandle(
            sandbox_id=id_val,
            endpoint="",
            task=Task(id="mock", image="mock"),
            run_id="r1",
            backend=None,
            runtime=runtime,
        )
        h.tracer = client.tracer
        if timeout_ms is not None:
            with pytest.raises(CommandTimeoutError):
                await h.exec_async(command, timeout_s=timeout_ms / 1000.0)
        else:
            await h.exec_async(command)
    elif op == "read_file":
        [chunk async for chunk in env.read_file(path)]
    elif op == "write_file":
        await env.write_file(path, content.encode("utf-8"))

    ended: dict[str, list] = {}
    for finished in spans.get_finished_spans():
        ended.setdefault(finished.name, []).append(finished)

    expected_name = "ate_env_client." + v["expected_span"]
    assert expected_name in ended, (
        f"span {expected_name} not found in {list(ended.keys())}"
    )
    span = ended[expected_name][-1]
    got_attrs = dict(span.attributes)

    want = dict(v.get("expected_attributes", {}))
    py_only = v.get("python_only")
    if py_only:
        want.update(py_only.get("attributes", {}))

    for k, wv in want.items():
        if not (k.startswith("ate.") or k.startswith("ate_env.")):
            continue
        assert k in got_attrs, f"op {op}: missing {k}, got {got_attrs}"
        gv = got_attrs[k]
        assert type(gv) is type(wv) and gv == wv, (
            f"op {op}: {k} = {gv!r} ({type(gv).__name__}), want {wv!r} ({type(wv).__name__})"
        )

    for k, gv in got_attrs.items():
        if not (k.startswith("ate.") or k.startswith("ate_env.")):
            continue
        assert k in want, f"op {op}: unexpected {k}={gv}"
