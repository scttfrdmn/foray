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

"""Manual GPU/AWS smoke — NOT run in CI (make worker-smoke gates it).

Runs the real engine path against a small model (gpt2 by default): a logit-lens
trace whose activations are saved to S3 in-region, then prints the resulting
save_ref. This is the by-hand validation that the worker works on real hardware
with real credentials; the architecture's per-session loop automates the same path.
"""

from __future__ import annotations

import os
import sys

from . import config, engine
from .graph import Intervention


def main() -> int:
    if os.environ.get("FORAY_GPU_SMOKE") != "1":
        print(
            "refusing: set FORAY_GPU_SMOKE=1 (and AWS_PROFILE, FORAY_SAVE_BUCKET) to "
            "run the real GPU/AWS smoke; this is never part of CI",
            file=sys.stderr,
        )
        return 2
    if config.fake_mode():
        print("refusing: FORAY_FAKE=1 is set; the smoke must exercise the real path",
              file=sys.stderr)
        return 2

    settings = config.load()
    iv = Intervention(
        prompt="The Eiffel Tower is in the city of",
        saves=["lm_head.output"],
        engine="eager",
    )
    print(f"smoke: model={settings.model_uri} device={settings.device} "
          f"bucket={settings.save_bucket}")
    result = engine.run(settings, iv)
    print(f"smoke OK: save_ref={result.save_ref} viz_ref={result.viz_ref}")
    print(result.nnsight)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
