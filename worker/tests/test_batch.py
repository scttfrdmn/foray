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

"""Tests for the launch-time handoff entrypoint (issue #66).

No S3, no boto3, no GPU: the S3 calls are the two seams (fetch_graph / write_result),
so everything else is exercised directly.
"""

from __future__ import annotations

import base64
import json

import pytest

from worker import batch, config, graph


def envelope(engine: str = "eager", **payload) -> bytes:
    """Build the handoff envelope the gateway writes."""
    raw = base64.b64encode(json.dumps(payload).encode()).decode()
    return json.dumps({"engine": engine, "payload": raw}).encode()


@pytest.fixture
def settings(monkeypatch):
    monkeypatch.setenv("FORAY_FAKE", "1")
    monkeypatch.setenv("FORAY_SESSION_ID", "i-0abc")
    monkeypatch.setenv("FORAY_SAVE_BUCKET", "foray-data")
    return config.load()


class TestKeys:
    """The keys must match internal/gateway/handoff.go exactly — a mismatch means the
    worker reads nothing and the control plane polls an object that never appears."""

    def test_graph_key(self) -> None:
        assert batch.graph_key("i-0abc") == "sessions/i-0abc/graph.json"

    def test_result_key(self) -> None:
        assert batch.result_key("i-0abc") == "sessions/i-0abc/result.json"


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


class TestRun:
    def test_returns_references_only(self, settings, monkeypatch) -> None:
        """The result carries references and the generated code — never tensors. Same
        invariant the HTTP path enforces (CLAUDE.md, no automatic egress)."""
        monkeypatch.setattr(
            batch, "fetch_graph", lambda s: batch.parse_envelope(envelope(prompt="hi"), s)
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
            batch, "fetch_graph", lambda s: batch.parse_envelope(envelope(prompt="hi"), s)
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

        monkeypatch.setattr(batch, "fetch_graph", bad)
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

        monkeypatch.setattr(batch, "fetch_graph", boom)
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

        monkeypatch.setattr(batch, "fetch_graph", boom)
        monkeypatch.setattr(batch, "write_result", no_write)

        assert batch.main([]) == batch.EXIT_FAILED


class TestFetchGraphWaits:
    """The graph's key contains the instance id, so the control plane can only write it
    AFTER launching. Booting takes a minute or more, so the object is normally already
    there — but giving up on the first miss would fail a session for being early."""

    @staticmethod
    def _client_error(code: str):
        import botocore.exceptions

        return botocore.exceptions.ClientError({"Error": {"Code": code}}, "GetObject")

    def test_retries_until_the_graph_appears(self, settings, monkeypatch) -> None:
        calls = {"n": 0}

        class Client:
            def get_object(self, **_kw):
                calls["n"] += 1
                if calls["n"] < 3:
                    raise TestFetchGraphWaits._client_error("NoSuchKey")
                return {"Body": _Body(envelope(prompt="hi"))}

        monkeypatch.setattr(batch, "_s3", lambda _s: Client())
        iv = batch.fetch_graph(settings, sleep=lambda _s: None)
        assert iv.prompt == "hi"
        assert calls["n"] == 3

    def test_gives_up_with_a_clear_message(self, settings, monkeypatch) -> None:
        class Client:
            def get_object(self, **_kw):
                raise TestFetchGraphWaits._client_error("NoSuchKey")

        monkeypatch.setattr(batch, "_s3", lambda _s: Client())
        monkeypatch.setattr(batch, "GRAPH_WAIT_SECONDS", 0)

        with pytest.raises(graph.GraphError, match="never handed one over"):
            batch.fetch_graph(settings, sleep=lambda _s: None)

    def test_does_not_wait_on_a_permissions_error(self, settings, monkeypatch) -> None:
        """AccessDenied will not fix itself; polling it for two minutes would only delay
        a clear failure."""
        import botocore.exceptions

        class Client:
            def get_object(self, **_kw):
                raise TestFetchGraphWaits._client_error("AccessDenied")

        monkeypatch.setattr(batch, "_s3", lambda _s: Client())
        with pytest.raises(botocore.exceptions.ClientError):
            batch.fetch_graph(settings, sleep=lambda _s: None)


class _Body:
    """Minimal stand-in for S3's streaming body."""

    def __init__(self, data: bytes) -> None:
        self._data = data

    def read(self) -> bytes:
        return self._data
