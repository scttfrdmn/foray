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

"""FORAY_FAKE=1 path: canned results with no GPU, no AWS, no torch.

This is the dev/rehearse path and the CI gate — the Python mirror of Go's
internal/gateway/fake.go `fakeWorker`. The shape it returns is deliberately
identical to the Go fake (an s3:// save ref, a viz ref, a logit-lens nnsight
snippet) so a fake worker is a drop-in behind forayd's HTTPWorker for an offline
end-to-end run. It imports nothing heavy — that is the whole point.
"""

from __future__ import annotations

from .config import Settings
from .engine import TraceResult
from .graph import Intervention


def run(settings: Settings, iv: Intervention) -> TraceResult:
    """Return a deterministic TraceResult. References only — never tensors — so the
    no-automatic-egress invariant holds even offline (matches gateway.fakeWorker)."""
    sid = settings.session_id
    save_ref = f"s3://{settings.save_bucket}/{settings.save_prefix}"
    return TraceResult(
        session_id=sid,
        save_ref=save_ref,
        viz_ref=f"viz://{sid}/logit-lens.png",
        nnsight=(
            "with model.trace(prompt) as t:\n"
            "    logits = model.lm_head.output.save()"
        ),
    )
