// Copyright 2026 Scott Friedman
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package device

// Trainium / neuron is registered-but-disabled. TorchNeuron's native PyTorch
// backend (PrivateUse1 device, eager dispatch, working autograd) makes nnsight
// portable to Neuron with little or no change once it GAs — so the abstraction
// must accept it from day one. Until then Enabled() returns false and Options
// never surfaces it. See CLAUDE.md §Deferred and ARCHITECTURE.md §6.3.
//
// The gate lives here, in Cedar (engine == "neuron" is forbidden), and in the
// worker — three independent layers, deliberately. Do not flip Enabled() to
// true until TorchNeuron is generally available.
type neuron struct{}

func (neuron) Backend() Backend { return BackendNeuron }
func (neuron) Enabled() bool    { return false }

// options is intentionally empty: even if a future edit flips Enabled(), there
// are no validated tiers to offer yet.
func (neuron) options() []Option { return nil }

func init() { register(neuron{}) }
