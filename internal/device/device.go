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

// Package device maps an abstract accelerator target to concrete EC2 tiers.
// NVIDIA is the only enabled provider; neuron (Trainium) is registered but
// GA-gated so the sizing logic and worker device path accept it from day one
// without it appearing in the public menu (see neuron.go, CLAUDE.md §Deferred).
package device

import (
	"fmt"
	"sort"
)

// Tier is an accelerator size class, ordered smallest-to-largest. The ordering
// is load-bearing: Options returns tiers sorted ascending by Tier, and callers
// compare with <.
type Tier int

const (
	TierSlice Tier = iota // G7e RTX PRO 6000 MIG slice (~24 GB)
	TierSmall             // G7 RTX PRO 4500, whole card (32 GB)
	TierMid               // G7e RTX PRO 6000, whole card (96 GB)
	TierLarge             // H200 / multi-GPU G7e over P2P/EFA (141 GB+)
)

func (t Tier) String() string {
	switch t {
	case TierSlice:
		return "slice"
	case TierSmall:
		return "small"
	case TierMid:
		return "mid"
	case TierLarge:
		return "large"
	default:
		return "unknown"
	}
}

// Backend is an accelerator provider family.
type Backend int

const (
	BackendNVIDIA Backend = iota // enabled: the full EC2 NVIDIA menu
	BackendNeuron                // Trainium — registered but GA-gated (see neuron.go)
)

func (b Backend) String() string {
	switch b {
	case BackendNVIDIA:
		return "nvidia"
	case BackendNeuron:
		return "neuron"
	default:
		return "unknown"
	}
}

// Option is one concrete instance choice the menu (and the sizing layer) can pick.
type Option struct {
	Backend      Backend
	Tier         Tier
	InstanceType string // EC2 instance type family, e.g. "g7e.xlarge"
	GPU          string // human-facing GPU name
	GPUMemGB     int    // usable accelerator memory for this tier, in GB
}

// Provider describes an accelerator family and whether it is currently offerable.
type Provider interface {
	Backend() Backend
	Enabled() bool
	// options returns this provider's tiers, unsorted. Unexported: callers go
	// through the package-level Options, which filters and sorts.
	options() []Option
}

// registry holds every known provider, enabled or not.
var registry = map[Backend]Provider{}

func register(p Provider) { registry[p.Backend()] = p }

// Lookup returns the provider for a backend. It errors only if the backend was
// never registered; a registered-but-disabled provider (neuron) is returned so
// callers can observe Enabled() == false.
func Lookup(b Backend) (Provider, error) {
	p, ok := registry[b]
	if !ok {
		return nil, fmt.Errorf("device: backend %s not registered", b)
	}
	return p, nil
}

// Options returns every enabled tier whose accelerator memory is at least
// minHBM (GB), sorted ascending by Tier. Disabled providers (neuron) never
// appear, regardless of any beta flag — that gate is enforced here and again in
// Cedar (foray.cedar) and the worker.
func Options(minHBM int) []Option {
	var out []Option
	for _, p := range registry {
		if !p.Enabled() {
			continue
		}
		for _, o := range p.options() {
			if o.GPUMemGB >= minHBM {
				out = append(out, o)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tier < out[j].Tier })
	return out
}

// ByInstanceType resolves an EC2 instance type to its enabled tier option — the
// expert path's --hardware override and the `sessions` view both need to label a
// concrete instance with its tier/GPU. Disabled providers (neuron) never match,
// so an override cannot reach a GA-gated backend through this door either. ok is
// false for an unknown or disabled instance type.
func ByInstanceType(it string) (Option, bool) {
	for _, p := range registry {
		if !p.Enabled() {
			continue
		}
		for _, o := range p.options() {
			if o.InstanceType == it {
				return o, true
			}
		}
	}
	return Option{}, false
}

// nvidia is the only enabled provider. Tier capacities come from ARCHITECTURE.md
// §6.3; TierLarge advertises a multi-GPU aggregate so 405B-class footprints map
// to it alone.
type nvidia struct{}

func (nvidia) Backend() Backend { return BackendNVIDIA }
func (nvidia) Enabled() bool    { return true }
func (nvidia) options() []Option {
	return []Option{
		{Backend: BackendNVIDIA, Tier: TierSlice, InstanceType: "g7e.xlarge", GPU: "RTX PRO 6000 (MIG slice)", GPUMemGB: 24},
		{Backend: BackendNVIDIA, Tier: TierSmall, InstanceType: "g7.xlarge", GPU: "RTX PRO 4500", GPUMemGB: 32},
		{Backend: BackendNVIDIA, Tier: TierMid, InstanceType: "g7e.2xlarge", GPU: "RTX PRO 6000", GPUMemGB: 96},
		{Backend: BackendNVIDIA, Tier: TierLarge, InstanceType: "p5e.48xlarge", GPU: "H200 ×8", GPUMemGB: 1128},
	}
}

func init() { register(nvidia{}) }
