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

"""Engine routing — the §3 decision and the two serving paths.

Two engines bake into one worker image and route per request (ARCHITECTURE.md §3):

  - eager (nnsight LanguageModel): full transparency, arbitrary module access,
    activation edits, and **gradients**. The universal path and the reason
    "any model" is true. Empty/unknown engine defers here.
  - vllm (nnsight VLLM): paged-attention throughput over many prompts, text-gen
    only, **no gradients** (paged-attention kernels retain no autograd graph),
    numerically != transformers.

Routing rule: gradients/exotic arch -> eager; throughput over many prompts on a
mainstream arch -> vllm. A gradient request routed to vllm is rejected loudly
(issue #49) rather than silently differing.

torch / nnsight / vllm are imported lazily *inside* the real run paths, never at
module import — so the fake path (and CI) needs none of them.
"""

from __future__ import annotations

from dataclasses import dataclass

from .device import resolve as resolve_device
from .graph import Intervention

EAGER = "eager"
VLLM = "vllm"


class EngineError(Exception):
    """A routing/engine error whose message is meant for the user.

    The Go gateway folds the worker's error body into its own error
    (internal/gateway/worker.go), so this text surfaces to whoever ran the trace.
    """


@dataclass(frozen=True)
class TraceResult:
    """The result of one trace — references only, never tensors.

    Field names are the contract fixed by step 4 (internal/gateway/gateway.go
    TraceResult): session_id, save_ref, viz_ref, nnsight. Honoring
    no-automatic-egress means this never grows a tensor field; a user exports their
    own saved values through the separate opt-in path (internal/export).
    """

    session_id: str
    save_ref: str
    viz_ref: str
    nnsight: str


def select_engine(iv: Intervention, default_engine: str) -> str:
    """Resolve the engine for a trace: the intervention's hint, else the worker
    default. Normalizes to a known engine; an unrecognized hint defers to eager,
    the universal path."""
    choice = (iv.engine or default_engine or EAGER).strip().lower()
    return choice if choice in {EAGER, VLLM} else EAGER


def guard_routing(engine: str, iv: Intervention) -> None:
    """Enforce the §3 routing rule before any model work. Raises EngineError when a
    request is incompatible with its engine — currently the one hard rule: gradients
    on vllm (issue #49)."""
    if engine == VLLM and iv.backward:
        raise EngineError(
            "vllm: paged-attention retains no autograd graph, so gradient "
            "(backward) requests are unsupported; route gradients to the eager "
            "LanguageModel path (omit engine or set engine=eager)"
        )


def run(settings, iv: Intervention) -> TraceResult:
    """Run one trace. In fake mode this is never reached (app.py short-circuits to
    fake.run); the real path loads the model via the GDS loader, builds the engine,
    and executes the intervention interleaved with the forward pass.

    `settings` is config.Settings (typed loosely to avoid a circular import).
    """
    engine = select_engine(iv, settings.default_engine)
    guard_routing(engine, iv)  # reject before paying for a forward pass
    dev = resolve_device(settings.device)

    if engine == VLLM:
        return _run_vllm(settings, iv, dev)
    return _run_eager(settings, iv, dev)


def _run_eager(settings, iv: Intervention, dev) -> TraceResult:
    """Eager LanguageModel path: full transparency + gradients. Heavy imports are
    local so the module loads without torch/nnsight on the fake path."""
    from nnsight import LanguageModel  # noqa: PLC0415  (lazy by design)

    from . import loader, saves  # local: pull in heavy deps only here

    handle = loader.load(settings.model_uri, dev)  # GDS stream S3 -> HBM
    model = LanguageModel(handle.model, device_map=dev.torch_device)

    captured = {}
    with model.trace(iv.prompt):
        # The intervention graph fires hooks at module boundaries during the eager
        # forward pass. We .save() the requested module outputs; gradients are
        # available on this path when iv.backward is set.
        for path in iv.saves or ["lm_head.output"]:
            captured[path] = _resolve_module(model, path).output.save()
        if iv.backward:
            model.output.logits.sum().backward()

    save_ref = saves.put(settings, captured)  # -> s3:// in-region; refs only
    return TraceResult(
        session_id=settings.session_id,
        save_ref=save_ref,
        viz_ref=f"viz://{settings.session_id}/logit-lens.png",
        nnsight=_codegen(iv, EAGER),
    )


def _run_vllm(settings, iv: Intervention, dev) -> TraceResult:
    """VLLM path: paged-attention throughput, no gradients (already guarded)."""
    from nnsight.modeling.vllm import VLLM  # noqa: PLC0415  (lazy by design)

    from . import loader, saves  # local: heavy deps only here

    handle = loader.load(settings.model_uri, dev)
    model = VLLM(handle.model, device=dev.torch_device)

    captured = {}
    with model.trace(iv.prompt):
        for path in iv.saves or ["logits"]:
            captured[path] = _resolve_module(model, path).output.save()

    save_ref = saves.put(settings, captured)
    return TraceResult(
        session_id=settings.session_id,
        save_ref=save_ref,
        viz_ref=f"viz://{settings.session_id}/throughput.png",
        nnsight=_codegen(iv, VLLM),
    )


def _resolve_module(model, path: str):
    """Walk a dotted module path (e.g. "model.layers.5.mlp.output") to its proxy.

    The trailing ".output"/".input" attributes are nnsight proxy accessors; we stop
    at the module and let the caller take .output, so a bare "lm_head" and
    "lm_head.output" both work via the caller's `.output`.
    """
    node = model
    for part in path.split("."):
        if part in {"output", "input"}:
            break
        node = node[int(part)] if part.isdigit() else getattr(node, part)
    return node


def _codegen(iv: Intervention, engine: str) -> str:
    """Return the generated nnsight that produced this trace — the escape hatch the
    GUI shows so watching the code get written teaches the library (§5)."""
    saves = iv.saves or (["lm_head.output"] if engine == EAGER else ["logits"])
    lines = [f"with model.trace({iv.prompt!r}) as t:"]
    for path in saves:
        var = path.replace(".", "_")
        lines.append(f"    {var} = model.{path}.save()")
    if iv.backward:
        lines.append("    model.output.logits.sum().backward()")
    return "\n".join(lines)
