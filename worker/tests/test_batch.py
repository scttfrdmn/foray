# Copyright 2026 Scott Friedman
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

"""Tests for the launch-time handoff entrypoint (issues #66, #103).

No S3, no boto3, no GPU. There is nothing left to mock for the handoff itself: spawn
stages the graph in as a local file and stages the result out after exit, so both
seams are ordinary file I/O against tmp_path.
"""

from __future__ import annotations

import base64
import json
from pathlib import Path

import pytest

from worker import batch, config, graph


def envelope(engine: str = "eager", **payload) -> bytes:
    """Build the handoff envelope the gateway writes."""
    raw = base64.b64encode(json.dumps(payload).encode()).decode()
    return json.dumps({"engine": engine, "payload": raw}).encode()


@pytest.fixture
def settings(monkeypatch, tmp_path):
    monkeypatch.setenv("FORAY_FAKE", "1")
    monkeypatch.setenv("FORAY_SESSION_ID", "i-0abc")
    monkeypatch.setenv("FORAY_SAVE_BUCKET", "foray-data")
    monkeypatch.setenv("FORAY_GRAPH_PATH", str(tmp_path / "graph.json"))
    monkeypatch.setenv("FORAY_RESULT_PATH", str(tmp_path / "result.json"))
    return config.load()


class TestStagedPaths:
    """The defaults must match the task spec's staging manifests in
    internal/brain/real.go. They are flat paths in /tmp and not a tidier layout for a
    verified reason: spawn bind-mounts a staged path's parent as the instance user while
    `docker run` gets no --user, so only /tmp (1777) is writable by the image's own user
    (spore-host/spawn#555)."""

    def test_defaults_are_the_staged_tmp_paths(self, monkeypatch) -> None:
        for var in ("FORAY_GRAPH_PATH", "FORAY_RESULT_PATH"):
            monkeypatch.delenv(var, raising=False)
        settings = config.load()
        assert settings.graph_path == "/tmp/graph.json"
        assert settings.result_path == "/tmp/result.json"


class TestParseEnvelope:
    def test_decodes_the_gateway_envelope(self, settings) -> None:
        iv = batch.parse_envelope(
            envelope(prompt="France's capital is", saves=["lm_head.output"]), settings
        )
        assert iv.prompt == "France's capital is"
        assert iv.saves == ["lm_head.output"]

    def test_engine_hint_comes_from_the_envelope(self, settings) -> None:
        iv = batch.parse_envelope(envelope(engine="vllm", prompt="hi"), settings)
        assert iv.engine == "vllm"

    def test_falls_back_to_the_launch_default_engine(self, settings) -> None:
        # An envelope with no engine defers to what the control plane launched with,
        # matching the HTTP path's precedence.
        iv = batch.parse_envelope(envelope(engine="", prompt="hi"), settings)
        assert iv.engine == settings.default_engine

    @pytest.mark.parametrize(
        "body",
        [b"not json", b"[]", b'{"payload":"!!! not base64 !!!"}'],
    )
    def test_rejects_a_malformed_envelope(self, body: bytes, settings) -> None:
        with pytest.raises(graph.GraphError):
            batch.parse_envelope(body, settings)


class TestReadGraph:
    def test_reads_the_staged_file(self, settings) -> None:
        Path(settings.graph_path).write_bytes(envelope(prompt="hi"))
        assert batch.read_graph(settings).prompt == "hi"

    def test_a_missing_graph_is_a_definite_failure(self, settings) -> None:
        """Staging completes before this process starts, so an absent file is not
        something to wait for — it means stage-in failed. The old entrypoint polled for
        two minutes here; under `spawn task run` that wait would only delay the report."""
        with pytest.raises(graph.GraphError, match="stages it in"):
            batch.read_graph(settings)


class TestWriteResult:
    def test_writes_json_where_spawn_stages_it_out(self, settings) -> None:
        batch.write_result(settings, {"session_id": "i-0abc", "save_ref": "s3://b/x/"})
        assert json.loads(Path(settings.result_path).read_text())["save_ref"] == "s3://b/x/"


class TestRun:
    def test_returns_references_only(self, settings, monkeypatch) -> None:
        """The result carries references and the generated code — never tensors. Same
        invariant the HTTP path enforces (CLAUDE.md, no automatic egress)."""
        monkeypatch.setattr(
            batch, "read_graph", lambda s: batch.parse_envelope(envelope(prompt="hi"), s)
        )
        result = batch.run(settings)

        assert set(result) == {"session_id", "save_ref", "viz_ref", "nnsight"}
        assert result["save_ref"].startswith("s3://")
        for value in result.values():
            assert isinstance(value, str), "a non-string field could carry tensors"


class TestMain:
    def test_writes_the_result_on_success(self, settings, monkeypatch) -> None:
        written = {}
        monkeypatch.setattr(
            batch, "read_graph", lambda s: batch.parse_envelope(envelope(prompt="hi"), s)
        )
        monkeypatch.setattr(batch, "write_result", lambda s, r: written.update(r))

        assert batch.main([]) == batch.EXIT_OK
        assert written["save_ref"].startswith("s3://")
        assert "error" not in written

    def test_writes_an_error_result_on_a_bad_envelope(self, settings, monkeypatch) -> None:
        """A failure must be RECORDED, not just logged. An absent result is
        indistinguishable from a trace still running, so the control plane would poll
        forever and the user would watch a spinner with no reason given."""
        written = {}

        def bad(_s):
            raise graph.GraphError("empty trace payload")

        monkeypatch.setattr(batch, "read_graph", bad)
        monkeypatch.setattr(batch, "write_result", lambda s, r: written.update(r))

        assert batch.main([]) == batch.EXIT_FAILED
        assert written["error"] == "empty trace payload"
        assert written["session_id"] == "i-0abc"

    def test_writes_an_error_result_on_an_unexpected_failure(self, settings, monkeypatch) -> None:
        """Even an unanticipated exception is recorded: the instance is about to be
        terminated, so this is the last chance to say anything."""
        written = {}

        def boom(_s):
            raise RuntimeError("CUDA out of memory")

        monkeypatch.setattr(batch, "read_graph", boom)
        monkeypatch.setattr(batch, "write_result", lambda s, r: written.update(r))

        assert batch.main([]) == batch.EXIT_FAILED
        assert "CUDA out of memory" in written["error"]
        assert "RuntimeError" in written["error"]

    def test_survives_being_unable_to_write_the_failure(self, settings, monkeypatch) -> None:
        """If even the failure write fails there is nothing left to try — it must not
        raise out of main and lose the exit code."""

        def boom(_s):
            raise RuntimeError("no gpu")

        def no_write(_s, _r):
            raise RuntimeError("AccessDenied")

        monkeypatch.setattr(batch, "read_graph", boom)
        monkeypatch.setattr(batch, "write_result", no_write)

        assert batch.main([]) == batch.EXIT_FAILED
