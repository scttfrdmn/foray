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

"""Worker configuration, env-driven and single-tenant.

The control plane (forayd / spawn) injects everything via environment variables
when it launches the per-session instance: which model to load, where saves land,
which device target. Mirrors the Go side's FORAY_FAKE convention (spore.FromEnv,
brain, export) so the whole worker runs offline with no GPU and no AWS.
"""

from __future__ import annotations

import os
from dataclasses import dataclass


def _env_truthy(name: str) -> bool:
    """A var is "on" only for the canonical truthy tokens, matching the Go side's
    FORAY_FAKE=1 convention (an empty or "0" value is off, not on)."""
    return os.environ.get(name, "").strip().lower() in {"1", "true", "yes", "on"}


def fake_mode() -> bool:
    """True when FORAY_FAKE=1 — the offline dev/rehearse path and the CI gate.

    Read live (not cached) so tests can toggle the env per-case the way the Go
    fakes do; the worker process itself reads it once at startup.
    """
    return _env_truthy("FORAY_FAKE")


@dataclass(frozen=True)
class Settings:
    """Resolved worker settings for one session's lifetime.

    Fields map 1:1 onto launch-time env vars so the control plane is the single
    source of truth; nothing here is discovered at runtime.
    """

    fake: bool
    device: str  # accelerator target: "cuda" now, "neuron" GA-gated (see device.py)
    session_id: str  # the foray session this worker serves (echoed in TraceResult)
    model_uri: str  # HF id / s3:// checkpoint the GDS loader streams on boot
    save_bucket: str  # S3 bucket for saved activations (in-region, no egress)
    save_region: str  # bucket region; asserted in-region by the control plane
    default_engine: str  # "eager" (universal) unless the launch overrides it

    @property
    def save_prefix(self) -> str:
        """Per-session key prefix under which activations/outputs are saved."""
        return f"sessions/{self.session_id}/activations/"


def load() -> Settings:
    """Build Settings from the environment. Defaults keep the fake path turnkey:
    no env at all yields a usable offline configuration."""
    return Settings(
        fake=fake_mode(),
        device=os.environ.get("FORAY_DEVICE", "cuda").strip().lower(),
        session_id=os.environ.get("FORAY_SESSION_ID", "sess-fake000001"),
        model_uri=os.environ.get("FORAY_MODEL_URI", "gpt2"),
        save_bucket=os.environ.get("FORAY_SAVE_BUCKET", "your-bucket-us-east-1"),
        save_region=os.environ.get("FORAY_SAVE_REGION", "us-east-1"),
        default_engine=os.environ.get("FORAY_DEFAULT_ENGINE", "eager").strip().lower(),
    )
