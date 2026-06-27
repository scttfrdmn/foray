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
	"testing"

	"github.com/scttfrdmn/foray/internal/device"
	"github.com/scttfrdmn/foray/internal/sizing"
)

// stubPricer returns a fixed $/session and records the instance it priced.
type stubPricer struct {
	usd     float64
	gotType string
}

func (p *stubPricer) SessionCostUSD(_ context.Context, instanceType string) (float64, error) {
	p.gotType = instanceType
	return p.usd, nil
}

func TestExpertLadderBuildsOnePricedRung(t *testing.T) {
	ctx := context.Background()
	hw, ok := device.ByInstanceType("g7e.xlarge")
	if !ok {
		t.Fatal("g7e.xlarge should resolve")
	}
	pricer := &stubPricer{usd: 0.23}

	l, err := ExpertLadder(ctx, ExpertSpec{
		Question:    "why does it refuse X?",
		ModelSource: "hf",
		ModelRef:    "openai-community/gpt2",
		Technique:   "logit-lens",
		Instance:    hw,
	}, pricer, 5.0)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Rungs) != 1 {
		t.Fatalf("expert ladder should be one rung, got %d", len(l.Rungs))
	}
	r := l.Rungs[0]
	if r.Chosen.InstanceType != "g7e.xlarge" || pricer.gotType != "g7e.xlarge" {
		t.Errorf("priced the wrong instance: chosen=%q priced=%q", r.Chosen.InstanceType, pricer.gotType)
	}
	if r.EstCostUSD != 0.23 {
		t.Errorf("est cost = %v, want 0.23", r.EstCostUSD)
	}
	if r.ModelSource != "hf" || r.Model.Name != "openai-community/gpt2" {
		t.Errorf("rung model = %+v", r)
	}
	// Default engine is the universal eager path.
	if r.Engine != sizing.EngineEager {
		t.Errorf("default engine = %q, want eager", r.Engine)
	}
	// The user's question is preserved (the load-bearing invariant).
	if l.Question.Text != "why does it refuse X?" {
		t.Errorf("question = %q, want the user's own", l.Question.Text)
	}
}

func TestExpertLadderValidates(t *testing.T) {
	ctx := context.Background()
	hw, _ := device.ByInstanceType("g7e.xlarge")
	pricer := &stubPricer{usd: 0.10}

	if _, err := ExpertLadder(ctx, ExpertSpec{Technique: "logit-lens", Instance: hw}, pricer, 0); err == nil {
		t.Error("want error when model ref missing")
	}
	if _, err := ExpertLadder(ctx, ExpertSpec{ModelRef: "gpt2", Technique: "logit-lens"}, pricer, 0); err == nil {
		t.Error("want error when no hardware chosen")
	}
}

func TestExpertLadderAssessStopsAtTop(t *testing.T) {
	ctx := context.Background()
	hw, _ := device.ByInstanceType("g7e.xlarge")
	l, err := ExpertLadder(ctx, ExpertSpec{ModelRef: "gpt2", Technique: "logit-lens", Instance: hw}, &stubPricer{usd: 0.10}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// A one-rung ladder is the top of the ladder: even a positive result Stops
	// (there is nothing to climb to).
	b := &Brain{}
	rec, err := b.Assess(ctx, l, &Result{Rung: 0, EffectPresent: true})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Decision != Stop {
		t.Fatalf("expert one-rung ladder should Stop, got %s", rec.Decision)
	}
}
