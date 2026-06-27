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

import (
	"sort"
	"testing"
)

func TestNeuronGatedByDefault(t *testing.T) {
	p, err := Lookup(BackendNeuron)
	if err != nil {
		t.Fatalf("neuron should be registered: %v", err)
	}
	if p.Enabled() {
		t.Fatal("neuron must be disabled until TorchNeuron GAs")
	}
	for _, o := range Options(8) {
		if o.Backend == BackendNeuron {
			t.Fatalf("gated neuron option surfaced: %+v", o)
		}
	}
}

func TestOptionsTierFit(t *testing.T) {
	tiers := func(opts []Option) []Tier {
		var ts []Tier
		for _, o := range opts {
			ts = append(ts, o.Tier)
		}
		sort.Slice(ts, func(i, j int) bool { return ts[i] < ts[j] })
		return ts
	}
	cases := []struct {
		name   string
		minHBM int
		want   []Tier
	}{
		{"tiny fits all", 16, []Tier{TierSlice, TierSmall, TierMid, TierLarge}},
		{"40GB skips slice+small", 40, []Tier{TierMid, TierLarge}},
		{"120GB only large", 120, []Tier{TierLarge}},
		{"405B-class only large", 800, []Tier{TierLarge}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := tiers(Options(c.minHBM))
			if len(got) != len(c.want) {
				t.Fatalf("minHBM=%d: got tiers %v, want %v", c.minHBM, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("minHBM=%d: got %v, want %v", c.minHBM, got, c.want)
				}
			}
		})
	}
}

func TestOptionsSortedByTier(t *testing.T) {
	opts := Options(16)
	for i := 1; i < len(opts); i++ {
		if opts[i].Tier < opts[i-1].Tier {
			t.Fatalf("options not tier-ordered at %d: %v then %v", i, opts[i-1].Tier, opts[i].Tier)
		}
	}
}
