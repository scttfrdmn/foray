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

"""Run one trace handed over at launch, then exit (issue #66).

The deployed control plane cannot reach the worker. A Lambda cannot hold the SSH
forward that `spawn service` gives the CLI, VPC-attaching it would add
hourly-billing endpoints and NAT, and opening the worker's port would reverse the
loopback-only posture the tunnel exists to provide.

It does not need to reach it. A session runs **exactly one trace** — each rung
launches a fresh instance, runs one graph, and the instance is terminated — so the
work is fully known before the instance boots. The control plane writes the graph to
the session's own prefix in the user's bucket and launches the instance with this
module as its command; the worker reads the graph, runs it, writes back a *reference*,
and exits.

    sessions/<id>/graph.json    written by the control plane, read here
    sessions/<id>/result.json   written here, read by the control plane

The result carries references only — `save_ref`, `viz_ref`, the generated `nnsight` —
never tensors, exactly as the HTTP path does (CLAUDE.md, no automatic egress). A
failure is written as `{"error": "..."}` rather than left absent, because an absent
result is indistinguishable from a trace still running: the control plane would poll
forever instead of telling the user what went wrong.

Run it the way the control plane does:

    python3 -m worker.batch
"""

from __future__ import annotations

import base64
import binascii
import json
import sys
from dataclasses import asdict

from . import config, device, engine, graph

# Exit codes. The instance is terminated either way, so these are for a human reading
# the console log — the authoritative outcome is result.json.
EXIT_OK = 0
EXIT_FAILED = 1


def graph_key(session_id: str) -> str:
    return f"sessions/{session_id}/graph.json"


def result_key(session_id: str) -> str:
    return f"sessions/{session_id}/result.json"


def _s3(settings):
    """The S3 client, imported lazily so the fake path needs no boto3."""
    import boto3  # noqa: PLC0415

    return boto3.client("s3", region_name=settings.save_region)


def fetch_graph(settings) -> graph.Intervention:
    """Read the handed-over graph and parse it into an Intervention.

    The envelope is what internal/gateway writes: {"engine": str, "payload": base64}.
    `payload` is base64 because Go marshals []byte that way — the same encoding the
    HTTP path uses, so graph.parse is reached identically either way.
    """
    body = _s3(settings).get_object(
        Bucket=settings.save_bucket, Key=graph_key(settings.session_id)
    )["Body"].read()
    return parse_envelope(body, settings)


def parse_envelope(body: bytes, settings) -> graph.Intervention:
    """Decode the handoff envelope. Separate from fetch_graph so it is testable with no
    S3 and no boto3."""
    try:
        env = json.loads(body)
    except (json.JSONDecodeError, UnicodeDecodeError) as exc:
        raise graph.GraphError(f"handoff envelope is not JSON: {exc}") from exc
    if not isinstance(env, dict):
        raise graph.GraphError("handoff envelope must be a JSON object")

    raw = env.get("payload") or ""
    try:
        payload = base64.b64decode(raw, validate=True)
    except (binascii.Error, ValueError) as exc:
        raise graph.GraphError(f"handoff payload not base64: {exc}") from exc

    hint = env.get("engine") or settings.default_engine
    return graph.parse(payload, engine_hint=hint)


def write_result(settings, result: dict) -> None:
    """Write result.json for the control plane to collect."""
    _s3(settings).put_object(
        Bucket=settings.save_bucket,
        Key=result_key(settings.session_id),
        Body=json.dumps(result).encode(),
        ContentType="application/json",
    )


def run(settings) -> dict:
    """Run the handed-over trace and return the result document.

    Routing and engine selection are the same code the HTTP path uses (engine.run /
    fake.run), so the two entrypoints cannot diverge in what a trace actually does —
    only in how the work arrives and how the answer is returned.
    """
    iv = fetch_graph(settings)
    if settings.fake:
        from . import fake  # noqa: PLC0415  (keep the heavy-free path heavy-free)

        return asdict(fake.run(settings, iv))
    return asdict(engine.run(settings, iv))


def main(argv: list[str] | None = None) -> int:
    _ = argv  # no arguments: everything comes from the launch environment
    settings = config.load()

    try:
        result = run(settings)
    except (graph.GraphError, engine.EngineError, device.DeviceError) as exc:
        # A refusal the caller caused — a bad envelope, gradients on vllm (#49), a
        # GA-gated device. Report it as the result rather than exiting silently.
        return _fail(settings, str(exc))
    except Exception as exc:  # noqa: BLE001 - the last line before the instance dies
        # Anything else: still write a result. The instance is about to be terminated,
        # so an unwritten failure would leave the control plane polling an object that
        # never appears, and the user staring at a spinner with no reason given.
        return _fail(settings, f"{type(exc).__name__}: {exc}")

    write_result(settings, result)
    print(f"trace complete: {result.get('save_ref', '')}", file=sys.stderr)
    return EXIT_OK


def _fail(settings, message: str) -> int:
    """Record a failure as the session's result, then give up."""
    print(f"trace failed: {message}", file=sys.stderr)
    try:
        write_result(settings, {"session_id": settings.session_id, "error": message})
    except Exception as exc:  # noqa: BLE001
        # If even that fails there is nothing left to try; say so on the console, which
        # is the only remaining channel.
        print(f"could not write the failure result: {exc}", file=sys.stderr)
    return EXIT_FAILED


if __name__ == "__main__":  # pragma: no cover - process entrypoint
    sys.exit(main())
