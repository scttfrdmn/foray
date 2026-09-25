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

func TestSpawnLaunchArgs(t *testing.T) {
	r := &stubRunner{out: []byte(`{"instance_id":"i-abc","name":"sess-1","instance_type":"g7e.2xlarge","state":"running"}`)}
	sp := NewSpawn(r)

	inst, err := sp.Launch(context.Background(), LaunchSpec{
		Name:         "sess-1",
		InstanceType: "g7e.2xlarge",
		Region:       "us-east-1",
		Spot:         true,
		SpotMaxPrice: "1.50",
		TTL:          8 * time.Hour,
		IdleGrace:    5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if inst.ID != "i-abc" || inst.State != "running" {
		t.Errorf("instance = %+v", inst)
	}
	for _, want := range []struct{ flag, val string }{
		{"--name", "sess-1"},
		{"--instance-type", "g7e.2xlarge"},
		{"--region", "us-east-1"},
		{"--spot-max-price", "1.50"},
		{"--ttl", "8h0m0s"},
		{"--idle-timeout", "5m0s"},
		{"-o", "json"},
	} {
		if !r.hasFlagValue(want.flag, want.val) {
			t.Errorf("missing %s %s in args %v", want.flag, want.val, r.gotArgs)
		}
	}
	if !r.hasArg("--spot") {
		t.Errorf("missing --spot: %v", r.gotArgs)
	}
}

func TestSpawnLaunchValidates(t *testing.T) {
	r := &stubRunner{out: []byte(`{}`)}
	sp := NewSpawn(r)
	if _, err := sp.Launch(context.Background(), LaunchSpec{InstanceType: "g7.xlarge"}); err == nil {
		t.Error("want error when Name missing")
	}
	if _, err := sp.Launch(context.Background(), LaunchSpec{Name: "x"}); err == nil {
		t.Error("want error when InstanceType missing")
	}
	if r.gotCalls != 0 {
		t.Errorf("should not exec on invalid spec, ran %d times", r.gotCalls)
	}
}

func TestSpawnLaunchNoSpot(t *testing.T) {
	r := &stubRunner{out: []byte(`{"instance_id":"i-1"}`)}
	sp := NewSpawn(r)
	if _, err := sp.Launch(context.Background(), LaunchSpec{Name: "s", InstanceType: "g7.xlarge"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if r.hasArg("--spot") || r.hasArg("--spot-max-price") {
		t.Errorf("should not pass spot flags for on-demand: %v", r.gotArgs)
	}
	if r.hasArg("--ttl") || r.hasArg("--idle-timeout") {
		t.Errorf("should not pass ttl/idle when zero: %v", r.gotArgs)
	}
}

func TestSpawnTerminate(t *testing.T) {
	r := &stubRunner{out: []byte(`{}`)}
	sp := NewSpawn(r)
	if err := sp.Terminate(context.Background(), "i-abc"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if r.gotArgs[0] != "terminate" || r.gotArgs[1] != "i-abc" {
		t.Errorf("args = %v", r.gotArgs)
	}
}

// KeepWarm must not shell out to `spawn extend`. That verb moves the hard TTL
// (spawn rewrites spawn:ttl and the termination deadline), not the idle timer, so
// it never did the bridge's job — and because TTL accumulates from launch,
// calling it once per trace walked the hard terminate deadline outward and
// dissolved the per-session cost ceiling. The idle timer is reset on the instance
// itself via LaunchSpec.ActivePorts; the durable signal is the gateway's
// last_request_time row. This guards the regression, since reintroducing the call
// would look superficially reasonable.
func TestSpawnKeepWarmMakesNoExtendCall(t *testing.T) {
	r := &stubRunner{out: []byte(`{}`)}
	sp := NewSpawn(r)
	if err := sp.KeepWarm(context.Background(), "i-abc", time.Now()); err != nil {
		t.Fatalf("KeepWarm: %v", err)
	}
	if r.gotArgs != nil {
		t.Errorf("KeepWarm ran a spawn command (%v); it must not touch the TTL", r.gotArgs)
	}
}

// The worker's port has to reach spawn's idle daemon as --active-ports, or a
// trace in flight is read as idleness and the model-holding instance is reaped
// mid-request.
func TestSpawnLaunchPassesActivePorts(t *testing.T) {
	r := &stubRunner{out: []byte(`{"instance_id":"i-abc","name":"foray-x","state":"running"}`)}
	sp := NewSpawn(r)
	_, err := sp.Launch(context.Background(), LaunchSpec{
		Name:         "foray-x",
		InstanceType: "g7e.xlarge",
		ActivePorts:  []int{8000, 8001},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if got := argValue(r.gotArgs, "--active-ports"); got != "8000,8001" {
		t.Errorf("--active-ports = %q, want %q (args %v)", got, "8000,8001", r.gotArgs)
	}
}

// argValue returns the value following flag in args, or "" if absent.
func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestFakeSpawnLifecycle(t *testing.T) {
	ctx := context.Background()
	sp := NewFake().Spawn

	inst, err := sp.Launch(ctx, LaunchSpec{Name: "sess-A", InstanceType: "g7e.xlarge"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if inst.State != "running" || inst.ID == "" {
		t.Fatalf("launched = %+v", inst)
	}
	// The instance carries a real-shaped public address (spawn reports an IP) and
	// a TTL deadline derived from launch time + budget.
	if inst.PublicIP == "" {
		t.Error("launched instance has no PublicIP — the worker URL would be unbuildable")
	}
	if !inst.TTLDeadline().After(fakeEpoch) {
		t.Errorf("TTL deadline not in the future: %v", inst.TTLDeadline())
	}

	got, err := sp.Status(ctx, inst.ID)
	if err != nil || got.ID != inst.ID {
		t.Fatalf("Status = %+v, err %v", got, err)
	}

	// A trace rolls the modeled in-instance idle deadline to lastRequest + the
	// session's idle window.
	fake := sp.(*fakeSpawn)
	req := fakeEpoch.Add(30 * time.Minute)
	if err := sp.KeepWarm(ctx, inst.ID, req); err != nil {
		t.Fatalf("KeepWarm: %v", err)
	}
	idle, ok := fake.IdleDeadline(inst.ID)
	if !ok {
		t.Fatal("no modeled idle deadline for the instance")
	}
	if want := req.Add(inst.IdleTimeout); !idle.Equal(want) {
		t.Errorf("idle deadline = %v, want %v", idle, want)
	}

	if err := sp.Terminate(ctx, inst.ID); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	got, _ = sp.Status(ctx, inst.ID)
	if got.State != "terminated" {
		t.Errorf("state after terminate = %q", got.State)
	}
}

func TestFakeSpawnUnknownID(t *testing.T) {
	ctx := context.Background()
	sp := NewFake().Spawn
	if _, err := sp.Status(ctx, "i-nope"); err == nil {
		t.Error("want error for unknown id")
	}
	if err := sp.KeepWarm(ctx, "i-nope", time.Now()); err == nil {
		t.Error("want error keeping warm an unknown id")
	}
	if err := sp.Terminate(ctx, "i-nope"); err == nil {
		t.Error("want error terminating an unknown id")
	}
}

func TestFakeSpawnValidates(t *testing.T) {
	sp := NewFake().Spawn
	if _, err := sp.Launch(context.Background(), LaunchSpec{Name: "x"}); err == nil {
		t.Error("want error when InstanceType missing")
	}
}

func TestSpawnListArgsAndFilter(t *testing.T) {
	// spawn list returns a mix; List scopes to foray-named instances only.
	r := &stubRunner{out: []byte(`[
	  {"instance_id":"i-1","name":"foray-rung0-gpt2","instance_type":"g7e.xlarge","state":"running"},
	  {"instance_id":"i-2","name":"someone-elses-job","instance_type":"g7.xlarge","state":"running"}
	]`)}
	sp := NewSpawn(r)

	got, err := sp.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !r.hasFlagValue("-o", "json") || !r.hasArg("list") {
		t.Errorf("list args = %v", r.gotArgs)
	}
	if len(got) != 1 || got[0].ID != "i-1" {
		t.Fatalf("List should scope to foray sessions, got %+v", got)
	}
}

func TestFakeSpawnList(t *testing.T) {
	ctx := context.Background()
	sp := NewFake().Spawn
	if _, err := sp.Launch(ctx, LaunchSpec{Name: "foray-rung0-gpt2", InstanceType: "g7e.xlarge"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if _, err := sp.Launch(ctx, LaunchSpec{Name: "foray-rung1-llama", InstanceType: "g7e.2xlarge"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	got, err := sp.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List = %d instances, want 2", len(got))
	}
	// Launch stamps LaunchedAt so sessions can compute age / $-so-far.
	if got[0].LaunchedAt.IsZero() {
		t.Errorf("LaunchedAt not set: %+v", got[0])
	}
}
