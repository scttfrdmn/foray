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
