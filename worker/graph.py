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

"""Deserialize the opaque trace payload.

forayd (internal/gateway) treats Graph.Payload as opaque bytes — it routes them,
it does not look inside (ARCHITECTURE.md §6.7). This module owns the *interior* of
that payload: the structured intervention request the worker actually runs. It is
deliberately a thin, explicit envelope — NOT a reimplementation of nnsight's wire
format. When real nnsight graph (de)serialization lands, `Intervention` becomes the
adapter target and `parse()` is where `nnsight`'s deserializer plugs in.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field


class GraphError(Exception):
    """Raised when the payload envelope is malformed."""


@dataclass(frozen=True)
class Intervention:
    """One trace's intervention request.

    This is the explicit, JSON-friendly stand-in for a serialized nnsight
    intervention graph. The fields capture what the routing rule (§3) needs to
    decide eager-vs-vllm and what the engine needs to run a logit-lens-class trace:

      prompt    the input to run the forward pass over
      saves     module paths whose activations to .save() (e.g. "lm_head.output")
      layers    optional explicit layer indices for per-layer captures
      backward  whether the trace requests gradients (forces the eager path; a
                vllm request with this set is rejected — issue #49)
      engine    caller's engine hint ("eager" | "vllm" | ""); "" defers to default
    """

    prompt: str
    saves: list[str] = field(default_factory=list)
    layers: list[int] = field(default_factory=list)
    backward: bool = False
    engine: str = ""


def parse(payload: bytes, engine_hint: str = "") -> Intervention:
    """Deserialize the payload envelope into an Intervention.

    `engine_hint` is Graph.engine from the HTTP envelope; the payload may also carry
    its own "engine" — the envelope wins only when the payload omits it, so the
    routing decision has a single, predictable source. This is the seam where a real
    nnsight deserializer would replace the JSON decode.
    """
    if not payload:
        raise GraphError("empty trace payload")
    try:
        obj = json.loads(payload)
    except (json.JSONDecodeError, UnicodeDecodeError) as exc:
        raise GraphError(f"payload is not valid JSON: {exc}") from exc
    if not isinstance(obj, dict):
        raise GraphError("payload envelope must be a JSON object")

    prompt = obj.get("prompt")
    if not isinstance(prompt, str) or not prompt:
        raise GraphError("payload missing required string field 'prompt'")

    engine = obj.get("engine") or engine_hint
    return Intervention(
        prompt=prompt,
        saves=list(obj.get("saves", [])),
        layers=list(obj.get("layers", [])),
        backward=bool(obj.get("backward", False)),
        engine=str(engine or ""),
    )
