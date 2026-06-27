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

"""Device target — a parameter, not a hardcode (CLAUDE.md "Device-agnostic worker").

nnsight needs eager PyTorch with live module boundaries and working autograd, not
CUDA specifically. So the device is selected by name and the engine never hardcodes
"cuda". This mirrors the Go device registry (internal/device): `cuda` is enabled;
`neuron` (Trainium) is registered-but-disabled and never surfaced until TorchNeuron
GAs. The gate lives here, in Cedar (engine == "neuron" forbidden), and in the Go
registry — three independent layers, deliberately (see internal/device/neuron.go).
"""

from __future__ import annotations

from dataclasses import dataclass


class DeviceError(Exception):
    """Raised when an unknown or GA-gated device target is requested."""


@dataclass(frozen=True)
class Device:
    """An accelerator target. `torch_device` is the string handed to PyTorch /
    nnsight (`model.to(...)`); it stays a parameter so neuron slots in unchanged."""

    name: str
    enabled: bool
    torch_device: str


# Registry of every known device, enabled or not — the parallel of Go's
# device.registry. Disabled entries exist so the abstraction accepts them from day
# one (the worker's device path is built to carry neuron), but resolve() refuses
# them so they never reach a forward pass.
_REGISTRY: dict[str, Device] = {
    "cuda": Device(name="cuda", enabled=True, torch_device="cuda"),
    # Trainium / neuron: registered but GA-gated. TorchNeuron's PrivateUse1 backend
    # (eager dispatch, working autograd) makes nnsight portable to Neuron with
    # little change once it GAs. Until then enabled=False and resolve() refuses it,
    # regardless of any env flag — do not flip until TorchNeuron is GA.
    "neuron": Device(name="neuron", enabled=False, torch_device="privateuseone"),
}


def resolve(name: str) -> Device:
    """Resolve a device name to an *enabled* Device, or raise DeviceError.

    A registered-but-disabled device (neuron) raises with a clear "not GA" message
    rather than silently falling back — the same loud refusal as the Cedar
    `engine == "neuron"` forbid and the Go registry's Enabled() == false.
    """
    dev = _REGISTRY.get(name)
    if dev is None:
        raise DeviceError(
            f"unknown device target {name!r}; enabled: {sorted(enabled_names())}"
        )
    if not dev.enabled:
        raise DeviceError(
            f"device {name!r} is registered but not generally available; "
            "NVIDIA (cuda) only ships today"
        )
    return dev


def enabled_names() -> list[str]:
    """The device targets that may be surfaced — the menu the public sees. neuron
    is never in it until its registry entry flips to enabled."""
    return [name for name, d in _REGISTRY.items() if d.enabled]
