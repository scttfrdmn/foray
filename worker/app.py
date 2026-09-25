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

"""FastAPI surface — the worker endpoint forayd routes to (issue #13).

Speaks the wire contract fixed by step 4 (internal/gateway/worker.go), verbatim:

  POST /trace    body  Graph{engine, payload}   -> TraceResult{session_id,
                                                     save_ref, viz_ref, nnsight}
  GET  /healthz  liveness (mirrors forayd's /healthz)

Graph.payload arrives base64-encoded because Go marshals []byte as a base64 JSON
string. The worker decodes it, deserializes the intervention envelope (graph.py),
routes per §3 (engine.py), and returns *references* — never tensors (no automatic
egress). In FORAY_FAKE=1 the trace short-circuits to fake.run (no GPU, no AWS).
"""

from __future__ import annotations

import base64
import binascii
import secrets
from dataclasses import asdict

from fastapi import Depends, FastAPI, HTTPException, Request
from pydantic import BaseModel

from . import config, device, engine, graph

app = FastAPI(title="foray worker", docs_url=None, redoc_url=None)

# Settings are launch-time constants for the session; resolve once at import.
SETTINGS = config.load()

# The session access token, set by worker.serve before serving. None means no
# token is required, which is the case for `make worker-fake` and the tests: the
# token exists to guard the tunnel, and an unguarded local dev server is not a
# boundary worth defending. Under `spawn service` it is always set.
#
# Held in a mutable holder rather than rebound as a module global so set_token
# needs no `global` statement.
_STATE: dict[str, str | None] = {"token": None}


def set_token(token: str | None) -> None:
    """Install the session token. Called once by worker.serve at startup."""
    _STATE["token"] = token or None


def require_token(request: Request) -> None:
    """Reject a request that does not present the session token.

    Two presentations are accepted because two kinds of caller exist: foray sends
    `Authorization: Bearer <token>` (a header keeps the credential out of logs and
    error strings), while the URL spawn prints for a human carries `?token=<token>`
    — that URL should work if someone pastes it.

    The comparison is constant-time: a timing-distinguishable check on a
    high-entropy token is a weak attack, but the fix is one call.
    """
    expected = _STATE["token"]
    if expected is None:
        return
    header = request.headers.get("authorization", "")
    presented = ""
    if header.lower().startswith("bearer "):
        presented = header[len("bearer ") :].strip()
    if not presented:
        presented = request.query_params.get("token", "")
    if not presented or not secrets.compare_digest(presented, expected):
        raise HTTPException(status_code=401, detail="missing or invalid session token")


class Graph(BaseModel):
    """The HTTP request body — the JSON shape of gateway.Graph. `payload` is a
    base64 string (Go []byte); `engine` is the routing hint ("" -> worker default)."""

    engine: str = ""
    payload: str = ""  # base64-encoded intervention envelope, opaque to the gateway


@app.get("/healthz")
def healthz() -> dict:
    """Liveness + the device/engine the worker was launched with. Mirrors forayd's
    /healthz so an operator sees a consistent shape across both hops.

    Deliberately unauthenticated: it is reachable only through the tunnel anyway,
    it carries no session data, and a liveness probe that needs a credential is a
    liveness probe that fails for the wrong reasons.
    """
    return {
        "status": "ok",
        "device": SETTINGS.device,
        "engine_default": SETTINGS.default_engine,
        "fake": SETTINGS.fake,
    }


@app.post("/trace", dependencies=[Depends(require_token)])
def trace(req: Graph) -> dict:
    """Run one trace and return a result reference (never tensors).

    Errors map to HTTP the way the Go HTTPWorker expects: a bad payload or an
    invalid route (e.g. gradients on vllm, #49) is a 400 whose body carries the
    diagnostic, which the gateway folds into its own error for the user.
    """
    try:
        payload = base64.b64decode(req.payload, validate=True)
    except (binascii.Error, ValueError) as exc:
        raise HTTPException(status_code=400, detail=f"payload not base64: {exc}") from exc

    try:
        iv = graph.parse(payload, engine_hint=req.engine)
    except graph.GraphError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc

    if SETTINGS.fake:
        from . import fake  # local: keep the heavy-free path heavy-free

        result = fake.run(SETTINGS, iv)
        return asdict(result)

    try:
        result = engine.run(SETTINGS, iv)
    except (engine.EngineError, device.DeviceError) as exc:
        # Routing/device refusals are the caller's fault -> 400 with the message.
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    return asdict(result)
