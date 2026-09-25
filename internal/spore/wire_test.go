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
	"testing"
	"time"
)

// This file pins the wire contract between foray and the spore.host CLIs.
//
// Why it exists: the adapters' JSON tags were originally hand-inferred from the
// tools' documented `-o json` convention and marked TODO(verify-json), with the
// fake as the test source of truth. That decoupling is what let three wrong
// guesses — `public_dns`, `ttl_deadline`/`idle_deadline`, and `limit`/`in_use` —
// sit green indefinitely: every one decoded to a zero value against real output,
// and the fake supplied plausible data so nothing ever looked broken. The empty
// `public_dns` in particular meant the worker URL was built from an empty host on
// every real trace.
//
// The fixtures below are the *tools'* own output shapes, taken from their emitting
// code (spawn cmd/list.go outputJSON, truffle cmd/quotas.go quotaRow, truffle
// pkg/aws.SpotPriceResult, truffle pkg/output.PrintJSON). A test that decodes a
// fixture foray itself invented proves nothing, so when these tools change their
// output, update the fixture from *their* source, never to match ours.

// spawnListFixture is one element of `spawn list -o json`, verbatim in shape.
const spawnListFixture = `[{
  "instance_id": "i-0abc123def456",
  "name": "foray-rung0-gpt2",
  "instance_type": "g7e.xlarge",
  "state": "running",
  "region": "us-west-2",
  "availability_zone": "us-west-2a",
  "public_ip": "203.0.113.17",
  "private_ip": "10.0.1.23",
  "launch_time": "2026-09-25T12:00:00Z",
  "ttl": "2h",
  "idle_timeout": "5m",
  "key_name": "spawn-key",
  "spot": true,
  "iam_role": "foray-spawn-instance",
  "job_array_id": "",
  "sweep_id": ""
}]`

// TestSpawnListWireContract decodes real-shaped spawn output and asserts every
// field foray depends on arrives populated. A zero value here is the exact
// signature of the bug class this guards.
func TestSpawnListWireContract(t *testing.T) {
	r := &stubRunner{out: []byte(spawnListFixture)}
	insts, err := NewSpawn(r).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(insts) != 1 {
		t.Fatalf("got %d instances, want 1 (the foray- name prefix should match)", len(insts))
	}
	got := insts[0]

	if got.ID != "i-0abc123def456" {
		t.Errorf("ID = %q", got.ID)
	}
	if got.Name != "foray-rung0-gpt2" {
		t.Errorf("Name = %q", got.Name)
	}
	if got.InstanceType != "g7e.xlarge" {
		t.Errorf("InstanceType = %q", got.InstanceType)
	}
	if got.State != "running" {
		t.Errorf("State = %q", got.State)
	}
	if got.Region != "us-west-2" {
		t.Errorf("Region = %q", got.Region)
	}
	// The field that was wrong: spawn reports public_ip, and there is no
	// public_dns key anywhere in its output.
	if got.PublicIP != "203.0.113.17" {
		t.Errorf("PublicIP = %q, want 203.0.113.17 — an empty host makes the worker URL unbuildable", got.PublicIP)
	}
	if got.PrivateIP != "10.0.1.23" {
		t.Errorf("PrivateIP = %q", got.PrivateIP)
	}
	wantLaunch := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	if !got.LaunchedAt.Equal(wantLaunch) {
		t.Errorf("LaunchedAt = %v, want %v", got.LaunchedAt, wantLaunch)
	}
	// TTL and idle arrive as duration strings, not absolute deadlines.
	if got.TTL != 2*time.Hour {
		t.Errorf("TTL = %v, want 2h", got.TTL)
	}
	if got.IdleTimeout != 5*time.Minute {
		t.Errorf("IdleTimeout = %v, want 5m", got.IdleTimeout)
	}
	if want := wantLaunch.Add(2 * time.Hour); !got.TTLDeadline().Equal(want) {
		t.Errorf("TTLDeadline() = %v, want %v (derived, not parsed)", got.TTLDeadline(), want)
	}
}

// A just-launched or unmanaged instance has no ttl/idle_timeout and no address.
// Those must degrade to zero values rather than failing the whole listing — a
// session table with a missing column beats an error.
func TestSpawnListWireContractSparse(t *testing.T) {
	r := &stubRunner{out: []byte(`[{"instance_id":"i-1","name":"foray-x","state":"pending","ttl":"","idle_timeout":""}]`)}
	insts, err := NewSpawn(r).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(insts) != 1 {
		t.Fatalf("got %d instances, want 1", len(insts))
	}
	got := insts[0]
	if got.TTL != 0 || got.IdleTimeout != 0 {
		t.Errorf("empty durations should decode to zero, got TTL=%v idle=%v", got.TTL, got.IdleTimeout)
	}
	if !got.TTLDeadline().IsZero() {
		t.Errorf("TTLDeadline() = %v, want zero when TTL is unknown", got.TTLDeadline())
	}
}

// Status must answer from `spawn list` (the EC2 API), not `spawn status`, which
// proxies the in-instance spored daemon over SSH/SSM — a different document shape
// and unreachable from a cold Lambda in the control plane.
func TestStatusUsesListNotStatus(t *testing.T) {
	r := &stubRunner{out: []byte(spawnListFixture)}
	got, err := NewSpawn(r).Status(context.Background(), "i-0abc123def456")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if r.gotArgs[0] != "list" {
		t.Errorf("Status ran `spawn %s`, want `spawn list`", r.gotArgs[0])
	}
	if got.PublicIP != "203.0.113.17" {
		t.Errorf("PublicIP = %q", got.PublicIP)
	}
}

func TestStatusUnknownInstance(t *testing.T) {
	r := &stubRunner{out: []byte(`[]`)}
	_, err := NewSpawn(r).Status(context.Background(), "i-missing")
	if err == nil {
		t.Fatal("want an error for an unknown instance")
	}
}

// truffleSpotFixture is one element of `truffle spot -o json`, matching
// truffle's pkg/aws.SpotPriceResult.
const truffleSpotFixture = `[{
  "instance_type": "g7e.xlarge",
  "region": "us-west-2",
  "availability_zone": "us-west-2a",
  "spot_price": 0.4212,
  "on_demand_price": 1.404,
  "savings_percent": 70.0,
  "timestamp": "2026-09-25T12:00:00Z",
  "product_type": "Linux/UNIX"
}]`

// The one adapter whose tags were already right — pinned so it stays right, since
// every cost number foray shows is derived from these fields.
func TestTruffleSpotWireContract(t *testing.T) {
	r := &stubRunner{out: []byte(truffleSpotFixture)}
	quotes, err := NewTruffle(r).Price(context.Background(), "g7e.xlarge")
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	if len(quotes) != 1 {
		t.Fatalf("got %d quotes, want 1", len(quotes))
	}
	got := quotes[0]
	if got.InstanceType != "g7e.xlarge" || got.Region != "us-west-2" || got.AZ != "us-west-2a" {
		t.Errorf("identity fields = %+v", got)
	}
	if got.PriceUSDHr != 0.4212 {
		t.Errorf("PriceUSDHr = %v, want 0.4212 — a zero price would silently free every experiment", got.PriceUSDHr)
	}
	if got.OnDemandHr != 1.404 {
		t.Errorf("OnDemandHr = %v, want 1.404", got.OnDemandHr)
	}
}
