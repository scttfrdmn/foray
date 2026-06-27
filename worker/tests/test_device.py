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

"""Device gate — mirrors the Go neuron gate (internal/device/neuron.go). cuda
resolves; neuron is registered but refused and never surfaced. No GPU, no AWS."""

import pytest

from worker import device


def test_cuda_resolves_enabled():
    dev = device.resolve("cuda")
    assert dev.enabled
    assert dev.torch_device == "cuda"


def test_neuron_registered_but_refused():
    # neuron is in the registry (the abstraction accepts it) but GA-gated, so
    # resolve() refuses it loudly rather than silently falling back to cuda.
    with pytest.raises(device.DeviceError, match="not generally available"):
        device.resolve("neuron")


def test_neuron_never_surfaced():
    # The public menu (enabled_names) must not include neuron until its registry
    # entry flips to enabled — the same property as device.Options in Go.
    assert "neuron" not in device.enabled_names()
    assert "cuda" in device.enabled_names()


def test_unknown_device_raises():
    with pytest.raises(device.DeviceError, match="unknown device"):
        device.resolve("tpu")
