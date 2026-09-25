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
	"testing"
)

func TestTrufflePrice(t *testing.T) {
	canned := `[
	  {"instance_type":"g7e.2xlarge","region":"us-east-1","availability_zone":"us-east-1a","spot_price":1.10,"on_demand_price":3.30},
	  {"instance_type":"g7e.2xlarge","region":"us-west-2","availability_zone":"us-west-2b","spot_price":1.25,"on_demand_price":3.30}
	]`
	r := &stubRunner{out: []byte(canned)}
	tr := NewTruffle(r)

	quotes, err := tr.Price(context.Background(), "g7e.2xlarge", "us-east-1", "us-west-2")
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	if len(quotes) != 2 {
		t.Fatalf("want 2 quotes, got %d", len(quotes))
	}
	if quotes[0].PriceUSDHr != 1.10 || quotes[0].Region != "us-east-1" {
		t.Errorf("first quote = %+v", quotes[0])
	}
	// Arg construction: the instance type, json output, sort, and joined regions.
	if r.gotName != "truffle" || r.gotArgs[0] != "spot" || r.gotArgs[1] != "g7e.2xlarge" {
		t.Errorf("args = %v", r.gotArgs)
	}
	if !r.hasFlagValue("-o", "json") {
		t.Errorf("missing -o json: %v", r.gotArgs)
	}
	if !r.hasArg("--sort-by-price") {
		t.Errorf("missing --sort-by-price: %v", r.gotArgs)
	}
	if !r.hasFlagValue("--regions", "us-east-1,us-west-2") {
		t.Errorf("regions not joined as CSV: %v", r.gotArgs)
	}
}

func TestTrufflePriceNoRegions(t *testing.T) {
	r := &stubRunner{out: []byte(`[]`)}
	tr := NewTruffle(r)
	if _, err := tr.Price(context.Background(), "g7.xlarge"); err != nil {
		t.Fatalf("Price: %v", err)
	}
	if r.hasArg("--regions") {
		t.Errorf("should not pass --regions when none given: %v", r.gotArgs)
	}
}

func TestTrufflePriceRunnerError(t *testing.T) {
	r := &stubRunner{err: errors.New("boom")}
	tr := NewTruffle(r)
	if _, err := tr.Price(context.Background(), "g7.xlarge"); err == nil {
		t.Fatal("want error from runner, got nil")
	}
}

func TestTrufflePriceBadJSON(t *testing.T) {
	r := &stubRunner{out: []byte(`not json`)}
	tr := NewTruffle(r)
	if _, err := tr.Price(context.Background(), "g7.xlarge"); err == nil {
		t.Fatal("want parse error, got nil")
	}
}

// The fixture is the shape truffle's quotaRow actually emits (cmd/quotas.go):
// vCPU-denominated quota_vcpus/usage_vcpus/available_vcpus. The previous fixture
// invented `limit`/`in_use`, so it agreed with foray's wrong tags and the pair
// passed while reading nothing from real output.
func TestTruffleQuota(t *testing.T) {
	r := &stubRunner{out: []byte(`[{"region":"us-east-1","family":"G","type":"spot","quota_vcpus":128,"usage_vcpus":8,"available_vcpus":120,"status":"ok"}]`)}
	tr := NewTruffle(r)
	q, err := tr.Quota(context.Background(), "G", "us-east-1")
	if err != nil {
		t.Fatalf("Quota: %v", err)
	}
	if q.Limit != 128 || q.InUse != 8 || q.Available != 120 || q.Family != "G" {
		t.Errorf("quota = %+v", q)
	}
	if !r.hasFlagValue("--family", "G") || !r.hasFlagValue("--regions", "us-east-1") {
		t.Errorf("args = %v", r.gotArgs)
	}
}

func TestTruffleQuotaEmpty(t *testing.T) {
	r := &stubRunner{out: []byte(`[]`)}
	tr := NewTruffle(r)
	if _, err := tr.Quota(context.Background(), "G", "us-east-1"); err == nil {
		t.Fatal("want error on empty quota list, got nil")
	}
}

// `truffle find -o json` emits instance-type *objects*, one per type per region
// it is offered in — not a list of names. Decoding into []string failed on every
// real call. Names are de-duplicated in truffle's ranking order.
func TestTruffleDiscover(t *testing.T) {
	r := &stubRunner{out: []byte(`[
	  {"instance_type":"g7e.xlarge","region":"us-east-1","vcpus":4},
	  {"instance_type":"g7e.xlarge","region":"us-west-2","vcpus":4},
	  {"instance_type":"g7.xlarge","region":"us-east-1","vcpus":4}
	]`)}
	tr := NewTruffle(r)
	types, err := tr.Discover(context.Background(), "nvidia gpu")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := []string{"g7e.xlarge", "g7.xlarge"}
	if len(types) != len(want) || types[0] != want[0] || types[1] != want[1] {
		t.Errorf("types = %v, want %v (deduped, order preserved)", types, want)
	}
	if r.gotArgs[0] != "find" || r.gotArgs[1] != "nvidia gpu" {
		t.Errorf("args = %v", r.gotArgs)
	}
}

func TestFakeTruffle(t *testing.T) {
	tr := NewFake().Truffle
	ctx := context.Background()

	quotes, err := tr.Price(ctx, "g7e.2xlarge")
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	if len(quotes) != 1 || quotes[0].PriceUSDHr != 1.20 {
		t.Errorf("fake price = %+v", quotes)
	}
	// On-demand should exceed Spot — the headline truffle savings.
	if quotes[0].OnDemandHr <= quotes[0].PriceUSDHr {
		t.Errorf("on-demand %.2f should exceed spot %.2f", quotes[0].OnDemandHr, quotes[0].PriceUSDHr)
	}

	q, err := tr.Quota(ctx, "G", "us-east-1")
	if err != nil || q.Limit <= 0 {
		t.Errorf("fake quota = %+v, err %v", q, err)
	}

	types, err := tr.Discover(ctx, "anything")
	if err != nil || len(types) == 0 {
		t.Errorf("fake discover = %v, err %v", types, err)
	}
}
