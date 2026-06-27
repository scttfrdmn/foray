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
	"encoding/json"
	"fmt"
)

// Truffle wraps the truffle binary: Spot pricing across regions/AZs, Service
// Quota checks, and instance-type discovery. It backs every cost number foray
// shows (ARCHITECTURE.md §6.6). Do not reimplement it.
type Truffle interface {
	// Price returns Spot price quotes for an instance type, cheapest-first. The
	// sizing/brain layers turn the per-hour quote into a $/session estimate
	// bounded by spawn's TTL + idle.
	Price(ctx context.Context, instanceType string, regions ...string) ([]SpotQuote, error)
	// Quota reports the Service Quota for an instance family in a region — the
	// answer to "can this account even launch a large tier?" before we offer it.
	Quota(ctx context.Context, family, region string) (Quota, error)
	// Discover lists instance types matching a truffle query (natural language,
	// pattern, or specs) — the menu behind a hardware override.
	Discover(ctx context.Context, query string) ([]string, error)
}

// SpotQuote is one region/AZ Spot price for an instance type.
type SpotQuote struct {
	InstanceType string  `json:"instance_type"`
	Region       string  `json:"region"`
	AZ           string  `json:"availability_zone"`
	PriceUSDHr   float64 `json:"spot_price"`      // Spot $/hour
	OnDemandHr   float64 `json:"on_demand_price"` // On-Demand $/hour, when truffle reports it
}

// Quota is an account's Service Quota for an instance family in a region.
type Quota struct {
	Family string  `json:"family"`
	Region string  `json:"region"`
	Limit  float64 `json:"limit"` // vCPU (EC2 quotas are vCPU-denominated) or instance count
	InUse  float64 `json:"in_use"`
}

// truffle is the real adapter: it execs the CLI with `-o json` and parses the
// result. The exact JSON field names are confirmed against live truffle output
// when AWS credentials are available; the struct tags above encode the observed
// convention. See TODO(verify-json).
type truffle struct{ run Runner }

// NewTruffle returns a Truffle backed by the real truffle binary.
func NewTruffle(r Runner) Truffle { return truffle{run: r} }

func (t truffle) Price(ctx context.Context, instanceType string, regions ...string) ([]SpotQuote, error) {
	args := []string{"spot", instanceType, "--sort-by-price", "-o", "json"}
	if len(regions) > 0 {
		args = append(args, "--regions", joinCSV(regions))
	}
	out, err := t.run.Run(ctx, "truffle", args...)
	if err != nil {
		return nil, fmt.Errorf("truffle spot %s: %w", instanceType, err)
	}
	var quotes []SpotQuote
	if err := json.Unmarshal(out, &quotes); err != nil {
		return nil, fmt.Errorf("truffle spot %s: parse json: %w", instanceType, err)
	}
	return quotes, nil
}

func (t truffle) Quota(ctx context.Context, family, region string) (Quota, error) {
	out, err := t.run.Run(ctx, "truffle", "quotas", "--family", family, "--regions", region, "-o", "json")
	if err != nil {
		return Quota{}, fmt.Errorf("truffle quotas %s/%s: %w", family, region, err)
	}
	// truffle quotas returns a list (one row per matched quota); take the first.
	var quotas []Quota
	if err := json.Unmarshal(out, &quotas); err != nil {
		return Quota{}, fmt.Errorf("truffle quotas %s/%s: parse json: %w", family, region, err)
	}
	if len(quotas) == 0 {
		return Quota{}, fmt.Errorf("truffle quotas %s/%s: no quota reported", family, region)
	}
	return quotas[0], nil
}

func (t truffle) Discover(ctx context.Context, query string) ([]string, error) {
	out, err := t.run.Run(ctx, "truffle", "find", query, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("truffle find %q: %w", query, err)
	}
	var types []string
	if err := json.Unmarshal(out, &types); err != nil {
		return nil, fmt.Errorf("truffle find %q: parse json: %w", query, err)
	}
	return types, nil
}

// TODO(verify-json): truffle spot/quotas JSON field names are inferred from the
// CLI's documented `-o json` convention and confirmed against live output once
// AWS credentials are available. The fake (fake.go) is the source of truth for
// tests, so this verification is decoupled from the package's green state.
