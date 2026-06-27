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
)

func TestLagottoWatch(t *testing.T) {
	r := &stubRunner{out: []byte(`{"watch_id":"w-1","instance_type":"p5e.48xlarge","state":"active","region":"us-east-1"}`)}
	lg := NewLagotto(r)
	w, err := lg.Watch(context.Background(), WatchSpec{
		InstanceType: "p5e.48xlarge",
		Regions:      []string{"us-east-1", "us-west-2"},
		Spot:         true,
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if w.ID != "w-1" || w.State != "active" {
		t.Errorf("watch = %+v", w)
	}
	if r.gotArgs[0] != "watch" || r.gotArgs[1] != "p5e.48xlarge" {
		t.Errorf("args = %v", r.gotArgs)
	}
	if !r.hasFlagValue("--regions", "us-east-1,us-west-2") {
		t.Errorf("regions not joined: %v", r.gotArgs)
	}
	if !r.hasArg("--spot") {
		t.Errorf("missing --spot: %v", r.gotArgs)
	}
}

func TestLagottoWatchValidates(t *testing.T) {
	r := &stubRunner{out: []byte(`{}`)}
	lg := NewLagotto(r)
	if _, err := lg.Watch(context.Background(), WatchSpec{}); err == nil {
		t.Error("want error when InstanceType missing")
	}
	if r.gotCalls != 0 {
		t.Errorf("should not exec on invalid spec, ran %d", r.gotCalls)
	}
}

func TestLagottoList(t *testing.T) {
	r := &stubRunner{out: []byte(`[{"watch_id":"w-1","state":"active"},{"watch_id":"w-2","state":"matched"}]`)}
	lg := NewLagotto(r)
	ws, err := lg.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ws) != 2 {
		t.Errorf("want 2 watches, got %d", len(ws))
	}
}

func TestLagottoStatus(t *testing.T) {
	r := &stubRunner{out: []byte(`{"watch_id":"w-9","state":"matched","region":"us-west-2"}`)}
	lg := NewLagotto(r)
	w, err := lg.Status(context.Background(), "w-9")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if w.State != "matched" {
		t.Errorf("watch = %+v", w)
	}
	if r.gotArgs[0] != "status" || r.gotArgs[1] != "w-9" {
		t.Errorf("args = %v", r.gotArgs)
	}
}

func TestFakeLagotto(t *testing.T) {
	ctx := context.Background()
	lg := NewFake().Lagotto

	w, err := lg.Watch(ctx, WatchSpec{InstanceType: "p5e.48xlarge"})
	if err != nil || w.State != "active" {
		t.Errorf("fake watch = %+v, err %v", w, err)
	}
	if _, err := lg.Watch(ctx, WatchSpec{}); err == nil {
		t.Error("fake watch should validate InstanceType")
	}
	if ws, err := lg.List(ctx); err != nil || ws == nil {
		t.Errorf("fake list = %v, err %v", ws, err)
	}
	if s, err := lg.Status(ctx, "w-x"); err != nil || s.ID != "w-x" {
		t.Errorf("fake status = %+v, err %v", s, err)
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("FORAY_FAKE", "1")
	f, fake := FromEnv()
	if !fake {
		t.Fatal("want fake=true when FORAY_FAKE=1")
	}
	if f.Truffle == nil || f.Spawn == nil || f.Lagotto == nil {
		t.Errorf("FromEnv returned nil adapter(s): %+v", f)
	}

	t.Setenv("FORAY_FAKE", "0")
	if _, fake := FromEnv(); fake {
		t.Error("want fake=false when FORAY_FAKE!=1")
	}
}
