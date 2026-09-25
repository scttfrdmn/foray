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

"""End-to-end FastAPI surface under FORAY_FAKE=1 — the CI shape. Asserts the wire
contract (field names fixed by step 4) and the no-automatic-egress invariant: the
/trace response carries references only, never tensors. No GPU, no AWS, no torch."""

import base64
import importlib
import json

import pytest
from fastapi.testclient import TestClient


@pytest.fixture
def client(monkeypatch):
    # Force the fake path before app import so SETTINGS resolves fake=True and no
    # heavy import is ever attempted.
    monkeypatch.setenv("FORAY_FAKE", "1")
    monkeypatch.setenv("FORAY_SESSION_ID", "sess-fake000001")
    import worker.app as app_module

    importlib.reload(app_module)  # re-resolve SETTINGS under the patched env
    return TestClient(app_module.app)


def _payload(**fields) -> str:
    return base64.b64encode(json.dumps(fields).encode()).decode()


def test_healthz_shape(client):
    r = client.get("/healthz")
    assert r.status_code == 200
    body = r.json()
    assert body["status"] == "ok"
    assert body["fake"] is True
    assert body["device"] == "cuda"
    assert body["engine_default"] == "eager"


def test_trace_returns_contract_fields(client):
    r = client.post(
        "/trace",
        json={"engine": "eager", "payload": _payload(prompt="France's capital is")},
    )
    assert r.status_code == 200
    body = r.json()
    # The exact field names the Go decoder (gateway.TraceResult) expects.
    assert set(body) == {"session_id", "save_ref", "viz_ref", "nnsight"}
    assert body["session_id"] == "sess-fake000001"
    assert body["save_ref"].startswith("s3://")
    assert "model.trace" in body["nnsight"]


def test_trace_no_tensor_egress(client):
    # The load-bearing invariant: only references leave /trace. The response must
    # carry no tensor-shaped payload — just s3:// refs, a viz ref, and code.
    r = client.post("/trace", json={"payload": _payload(prompt="x", saves=["lm_head.output"])})
    assert r.status_code == 200
    body = r.json()
    forbidden = {"tensor", "tensors", "activations", "values", "data", "array"}
    assert forbidden.isdisjoint(body.keys())
    assert body["save_ref"].startswith("s3://")  # the bytes stay in-region


def test_trace_rejects_bad_base64(client):
    r = client.post("/trace", json={"payload": "not!!base64"})
    assert r.status_code == 400
    assert "base64" in r.json()["detail"]


def test_trace_rejects_empty_payload(client):
    r = client.post("/trace", json={"payload": base64.b64encode(b"").decode()})
    assert r.status_code == 400


def test_trace_rejects_payload_without_prompt(client):
    r = client.post("/trace", json={"payload": _payload(saves=["lm_head.output"])})
    assert r.status_code == 400
    assert "prompt" in r.json()["detail"]


def test_default_session_id_when_env_absent(monkeypatch):
    # No env at all still yields a usable fake configuration (turnkey offline).
    for var in ("FORAY_SESSION_ID", "FORAY_MODEL_URI", "FORAY_SAVE_BUCKET"):
        monkeypatch.delenv(var, raising=False)
    monkeypatch.setenv("FORAY_FAKE", "1")
    import worker.app as app_module

    importlib.reload(app_module)
    client = TestClient(app_module.app)
    r = client.post("/trace", json={"payload": _payload(prompt="x")})
    assert r.status_code == 200
    assert r.json()["session_id"]  # non-empty default


class TestSessionToken:
    """Token auth guards the tunnel (issue #66).

    The worker binds loopback and is reached through spawn's ssh -L forward, so the
    token is defense in depth rather than the only boundary — but anything that can
    reach the loopback port (another process on the instance) must still present it.
    """

    @pytest.fixture
    def tokened(self, monkeypatch):
        monkeypatch.setenv("FORAY_FAKE", "1")
        import worker.app as app_module

        importlib.reload(app_module)
        app_module.set_token("s3cret")
        try:
            yield TestClient(app_module.app)
        finally:
            # Leave the module unguarded for the other tests, which assume no token.
            app_module.set_token(None)

    def _body(self) -> dict:
        return {"engine": "eager", "payload": _payload(prompt="hi", saves=["x"], layers=[0])}

    def test_bearer_header_accepted(self, tokened) -> None:
        r = tokened.post("/trace", json=self._body(), headers={"Authorization": "Bearer s3cret"})
        assert r.status_code == 200, r.text

    def test_query_param_accepted(self, tokened) -> None:
        # The URL spawn prints for a human carries ?token=...; it should work.
        r = tokened.post("/trace?token=s3cret", json=self._body())
        assert r.status_code == 200, r.text

    def test_missing_token_rejected(self, tokened) -> None:
        r = tokened.post("/trace", json=self._body())
        assert r.status_code == 401

    def test_wrong_token_rejected(self, tokened) -> None:
        r = tokened.post("/trace", json=self._body(), headers={"Authorization": "Bearer nope"})
        assert r.status_code == 401

    def test_healthz_needs_no_token(self, tokened) -> None:
        # Liveness must not fail for want of a credential.
        assert tokened.get("/healthz").status_code == 200

    def test_no_token_configured_allows_plain_requests(self, client) -> None:
        # make worker-fake / the tests run unguarded: an unguarded local dev server
        # is not a boundary worth defending.
        assert client.post("/trace", json=self._body()).status_code == 200
