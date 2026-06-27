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

package brain

import (
	"context"
	"fmt"

	"github.com/scttfrdmn/foray/internal/spore"
)

// Pricer turns a chosen instance type into a $/session estimate. The number that
// matters is $/session, not $/hour (ARCHITECTURE.md §7): TTL + idle bound the
// session, so the brain prices the assumed session length, not an open-ended
// hour. Backed by truffle Spot pricing in prod; a stub offline.
type Pricer interface {
	// SessionCostUSD returns the estimated cost to run one session on the given
	// instance type. Implementations price the cheapest Spot quote × the assumed
	// session hours.
	SessionCostUSD(ctx context.Context, instanceType string) (float64, error)
}

// AssumedSessionHours is the planning horizon for a $/session estimate: a short,
// bounded interactive session. The actual bill is whatever the session runs
// before TTL + idle reap it; this is the number shown up front so the user (and
// the Cedar ceiling) can reason about cost before Go.
const AssumedSessionHours = 0.5

// trufflePricer prices sessions from truffle's cheapest Spot quote.
type trufflePricer struct {
	truffle spore.Truffle
	regions []string // optional region scope for the quote; empty ⇒ truffle's default
	hours   float64  // assumed session length; 0 ⇒ AssumedSessionHours
}

// NewTrufflePricer returns a Pricer backed by the truffle adapter. regions may
// be empty to let truffle pick by availability/price.
func NewTrufflePricer(t spore.Truffle, regions ...string) Pricer {
	return trufflePricer{truffle: t, regions: regions, hours: AssumedSessionHours}
}

func (p trufflePricer) SessionCostUSD(ctx context.Context, instanceType string) (float64, error) {
	quotes, err := p.truffle.Price(ctx, instanceType, p.regions...)
	if err != nil {
		return 0, fmt.Errorf("price %s: %w", instanceType, err)
	}
	if len(quotes) == 0 {
		return 0, fmt.Errorf("price %s: truffle returned no quotes", instanceType)
	}
	// truffle returns quotes cheapest-first; take the cheapest Spot rate.
	hours := p.hours
	if hours <= 0 {
		hours = AssumedSessionHours
	}
	cost := quotes[0].PriceUSDHr * hours
	return roundCents(cost), nil
}

// roundCents rounds a dollar amount to whole cents so estimates render cleanly
// and compare stably against the budget ceiling.
func roundCents(c float64) float64 { return float64(int(c*100+0.5)) / 100 }
