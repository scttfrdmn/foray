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

"""Engine routing (§3) and the gradients-on-vllm rejection (issue #49). Pure
logic — no torch, no GPU, no AWS (the heavy paths are never entered here)."""

import pytest

from worker import engine
from worker.graph import Intervention


def test_select_engine_defaults_to_eager():
    # Empty hint + empty default -> eager, the universal path.
    assert engine.select_engine(Intervention(prompt="x"), "") == engine.EAGER


def test_select_engine_honors_intervention_hint():
    iv = Intervention(prompt="x", engine="vllm")
    assert engine.select_engine(iv, "eager") == engine.VLLM


def test_select_engine_unknown_hint_falls_back_to_eager():
    iv = Intervention(prompt="x", engine="banana")
    assert engine.select_engine(iv, "") == engine.EAGER


def test_vllm_rejects_gradients():
    # The §3 hard rule: paged-attention retains no autograd graph, so a backward
    # request on vllm must fail loudly, not silently differ (#49).
    iv = Intervention(prompt="x", backward=True)
    with pytest.raises(engine.EngineError, match="autograd"):
        engine.guard_routing(engine.VLLM, iv)


def test_eager_allows_gradients():
    # Same request on the eager path is fine — gradients are its whole point.
    iv = Intervention(prompt="x", backward=True)
    engine.guard_routing(engine.EAGER, iv)  # does not raise


def test_codegen_returns_nnsight_for_question():
    iv = Intervention(prompt="The Eiffel Tower is in", saves=["lm_head.output"])
    code = engine._codegen(iv, engine.EAGER)
    assert "model.trace(" in code
    assert "lm_head.output" in code
    assert ".save()" in code
