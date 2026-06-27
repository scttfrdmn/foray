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

package spore

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"
)

// Fake-path sentinels. The real adapters surface the tool's own stderr; the
// fakes have no tool, so they return these for the few invariants worth
// preserving offline (a launch needs a name + type; a lookup needs a known id).
var (
	errFakeLaunch  = errors.New("spawn launch (fake): Name and InstanceType are required")
	errFakeUnknown = errors.New("spawn (fake): unknown instance id")
	errFakeWatch   = errors.New("lagotto watch (fake): InstanceType is required")
)

// Fakes for FORAY_FAKE=1: deterministic pricing/launch/watch data so the whole
// control loop runs with no AWS. Same pattern as internal/export and
// internal/brain (NewFake). Used by the CLI's offline path and the CI gate
// (make demo-fake).

// Fake bundles a fake of each tool. FromEnv hands this back when FORAY_FAKE=1 so
// callers can wire all three from one place.
type Fake struct {
	Truffle Truffle
	Spawn   Spawn
	Lagotto Lagotto
}

// Enabled reports whether the fake path is active (FORAY_FAKE=1).
func Enabled() bool { return os.Getenv("FORAY_FAKE") == "1" }

// FromEnv returns fakes when FORAY_FAKE=1, or real exec-backed adapters
// otherwise. ok is false when running against real AWS so the caller can decide
// how to surface "not wired yet" (the CLI does this today).
func FromEnv() (f Fake, fake bool) {
	if Enabled() {
		return NewFake(), true
	}
	r := NewExecRunner()
	return Fake{Truffle: NewTruffle(r), Spawn: NewSpawn(r), Lagotto: NewLagotto(r)}, false
}

// NewFake builds the offline trio.
func NewFake() Fake {
	return Fake{Truffle: fakeTruffle{}, Spawn: newFakeSpawn(), Lagotto: fakeLagotto{}}
}

// --- truffle fake -----------------------------------------------------------

type fakeTruffle struct{}

// fakeSpotUSDHr is a plausible Spot $/hour by instance type so cost numbers are
// stable and recognizable in the rehearsal. Unknown types fall back to a
// mid-range GPU rate.
var fakeSpotUSDHr = map[string]float64{
	"g7e.xlarge":   0.45,  // slice
	"g7.xlarge":    0.55,  // small
	"g7e.2xlarge":  1.20,  // mid
	"p5e.48xlarge": 22.00, // large (H200 ×8)
}

func (fakeTruffle) Price(_ context.Context, instanceType string, regions ...string) ([]SpotQuote, error) {
	hr, ok := fakeSpotUSDHr[instanceType]
	if !ok {
		hr = 1.00
	}
	region := "us-east-1"
	if len(regions) > 0 {
		region = regions[0]
	}
	return []SpotQuote{{
		InstanceType: instanceType,
		Region:       region,
		AZ:           region + "a",
		PriceUSDHr:   hr,
		OnDemandHr:   hr * 3, // Spot is ~⅓ on-demand, the headline truffle savings
	}}, nil
}

func (fakeTruffle) Quota(_ context.Context, family, region string) (Quota, error) {
	// Plenty of headroom in the fake so the brain never trips a quota gate offline.
	return Quota{Family: family, Region: region, Limit: 256, InUse: 0}, nil
}

func (fakeTruffle) Discover(_ context.Context, _ string) ([]string, error) {
	// The enabled NVIDIA menu (mirrors internal/device tiers), tightest-first.
	return []string{"g7e.xlarge", "g7.xlarge", "g7e.2xlarge", "p5e.48xlarge"}, nil
}

// --- spawn fake -------------------------------------------------------------

// fakeSpawn keeps a tiny in-memory instance table so Launch → Status →
// KeepWarm → Terminate behave like a coherent lifecycle in the rehearsal.
type fakeSpawn struct {
	mu   sync.Mutex
	seq  int
	inst map[string]Instance
	// now is fixed so the fake is deterministic (Date.now is non-deterministic
	// and the demo must reproduce); tests can read deadlines relative to it.
	now time.Time
}

func newFakeSpawn() *fakeSpawn {
	return &fakeSpawn{inst: map[string]Instance{}, now: fakeEpoch}
}

// fakeEpoch is a fixed reference time so launch/idle/TTL deadlines are stable
// across runs of make demo-fake.
var fakeEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func (s *fakeSpawn) Launch(_ context.Context, spec LaunchSpec) (Instance, error) {
	if spec.Name == "" || spec.InstanceType == "" {
		return Instance{}, errFakeLaunch
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	ttl := spec.TTL
	if ttl == 0 {
		ttl = time.Hour
	}
	grace := spec.IdleGrace
	if grace == 0 {
		grace = defaultKeepWarmGrace
	}
	inst := Instance{
		ID:           fakeInstanceID(s.seq),
		Name:         spec.Name,
		InstanceType: spec.InstanceType,
		Region:       orDefault(spec.Region, "us-east-1"),
		State:        "running",
		PublicDNS:    spec.Name + ".fake.spore.host",
		TTLDeadline:  s.now.Add(ttl),
		IdleDeadline: s.now.Add(grace),
	}
	s.inst[inst.ID] = inst
	return inst, nil
}

func (s *fakeSpawn) Status(_ context.Context, instanceID string) (Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.inst[instanceID]
	if !ok {
		return Instance{}, errFakeUnknown
	}
	return inst, nil
}

func (s *fakeSpawn) Terminate(_ context.Context, instanceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.inst[instanceID]; !ok {
		return errFakeUnknown
	}
	inst := s.inst[instanceID]
	inst.State = "terminated"
	s.inst[instanceID] = inst
	return nil
}

func (s *fakeSpawn) KeepWarm(_ context.Context, instanceID string, lastRequest time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.inst[instanceID]
	if !ok {
		return errFakeUnknown
	}
	// Roll the idle deadline forward to lastRequest + grace — the very thing the
	// real adapter does via spawn extend, observable in the fake for tests.
	inst.IdleDeadline = lastRequest.Add(defaultKeepWarmGrace)
	s.inst[instanceID] = inst
	return nil
}

// --- lagotto fake -----------------------------------------------------------

type fakeLagotto struct{}

func (fakeLagotto) Watch(_ context.Context, spec WatchSpec) (CapacityWatch, error) {
	if spec.InstanceType == "" {
		return CapacityWatch{}, errFakeWatch
	}
	region := "us-east-1"
	if len(spec.Regions) > 0 {
		region = spec.Regions[0]
	}
	return CapacityWatch{
		ID:           "fake-watch-" + spec.InstanceType,
		InstanceType: spec.InstanceType,
		State:        "active",
		Region:       region,
	}, nil
}

func (fakeLagotto) List(_ context.Context) ([]CapacityWatch, error) {
	return []CapacityWatch{}, nil
}

func (fakeLagotto) Status(_ context.Context, watchID string) (CapacityWatch, error) {
	return CapacityWatch{ID: watchID, State: "active", Region: "us-east-1"}, nil
}

// --- shared fake helpers ----------------------------------------------------

func fakeInstanceID(seq int) string { return "i-fake" + pad6(seq) }

// pad6 zero-pads a small sequence into a 6-char suffix, mimicking an EC2 id tail
// without importing fmt just for one call site.
func pad6(n int) string {
	s := []byte("000000")
	for i := len(s) - 1; i >= 0 && n > 0; i-- {
		s[i] = byte('0' + n%10)
		n /= 10
	}
	return string(s)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
