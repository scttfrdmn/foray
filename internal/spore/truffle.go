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

// Quota is an account's Service Quota for an instance family in a region. EC2
// quotas are vCPU-denominated, which is why the numbers are vCPU counts and not
// instance counts.
//
// The field names mirror truffle's quotaRow (truffle cmd/quotas.go): the earlier
// `limit`/`in_use` guess matched nothing truffle emits, so every quota check
// silently read 0/0.
type Quota struct {
	Family    string `json:"family"`
	Region    string `json:"region"`
	Limit     int32  `json:"quota_vcpus"`
	InUse     int32  `json:"usage_vcpus"`
	Available int32  `json:"available_vcpus"`
	Status    string `json:"status"`
}

// truffle is the real adapter: it execs the CLI with `-o json` and parses the
// result. The struct tags above are verified against truffle's own output types
// (pkg/aws.SpotPriceResult, cmd/quotas.go quotaRow), not inferred — see
// TestWireContract, which pins them against captured CLI output.
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

// Discover lists the instance types matching a truffle query.
//
// `truffle find -o json` emits an array of instance-type *objects* (truffle
// pkg/output Printer.PrintJSON over []aws.InstanceTypeResult), not an array of
// names — decoding straight into []string failed outright on every real call.
// One type appears once per region it is offered in, so names are de-duplicated
// while preserving truffle's ranking order (the CLI has already sorted them, and
// that order is the menu foray shows behind a hardware override).
func (t truffle) Discover(ctx context.Context, query string) ([]string, error) {
	out, err := t.run.Run(ctx, "truffle", "find", query, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("truffle find %q: %w", query, err)
	}
	var rows []struct {
		InstanceType string `json:"instance_type"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("truffle find %q: parse json: %w", query, err)
	}
	types := make([]string, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, r := range rows {
		if r.InstanceType == "" || seen[r.InstanceType] {
			continue
		}
		seen[r.InstanceType] = true
		types = append(types, r.InstanceType)
	}
	return types, nil
}
