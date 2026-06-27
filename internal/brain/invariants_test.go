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
	"strings"
	"testing"

	"github.com/scttfrdmn/foray/internal/device"
	"github.com/scttfrdmn/foray/internal/sizing"
)

// countingExecutor records how many times a rung was actually launched, so the
// invariant tests can prove nothing runs without an explicit Approve.
type countingExecutor struct{ launches int }

func (e *countingExecutor) Execute(_ context.Context, _ Question, r *Rung) (string, error) {
	e.launches++
	return "sess", nil
}

// TestNoAutoClimbOrAutoLaunch enforces "the brain proposes and interprets; it
// never accepts" (#34): Propose, Assess, and NextProposal never launch a rung —
// only Approve (the human's Go) does.
func TestNoAutoClimbOrAutoLaunch(t *testing.T) {
	ctx := context.Background()
	exec := &countingExecutor{}
	b := &Brain{Plan: fakePlanner{}, Policy: fakePolicy{}, Exec: exec}

	ladder, prop, err := b.Propose(ctx, "q")
	if err != nil {
		t.Fatal(err)
	}
	// Proposing must not launch anything.
	if exec.launches != 0 {
		t.Fatalf("Propose launched %d rungs; it must launch none", exec.launches)
	}

	// Assessing a result must not launch the next rung, even on a Climb.
	rec, err := b.Assess(ctx, ladder, FakeResult("sess", 0))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Decision != Climb {
		t.Fatalf("fake first-rung assessment should Climb, got %s", rec.Decision)
	}
	if exec.launches != 0 {
		t.Fatalf("Assess launched %d rungs; a recommendation is advice, not an action", exec.launches)
	}

	// NextProposal hands back the next rung but still must not launch it.
	next := b.NextProposal(ctx, ladder)
	if next == nil {
		t.Fatal("expected a next proposal")
	}
	if exec.launches != 0 {
		t.Fatalf("NextProposal launched %d rungs; climbing is a fresh Go", exec.launches)
	}

	// Only Approve launches.
	if _, err := b.Approve(ctx, ladder, prop); err != nil {
		t.Fatal(err)
	}
	if exec.launches != 1 {
		t.Fatalf("after one Approve, launches = %d, want 1", exec.launches)
	}
}

// TestApproveRefusesOverCeiling enforces the cost invariant (#35): a rung whose
// $/session exceeds the Cedar per-session ceiling is refused at Approve, and the
// policy reason surfaces. Nothing launches.
func TestApproveRefusesOverCeiling(t *testing.T) {
	ctx := context.Background()
	pol, err := NewCedarPolicy(Principal{
		Subject:          "alice",
		BudgetCeilingUSD: 0.10, // tight ceiling
		AllowedTiers:     []string{"slice", "small", "mid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	exec := &countingExecutor{}
	b := &Brain{Plan: fakePlanner{}, Policy: pol, Exec: exec}

	ladder := &Ladder{
		Question: Question{Text: "q", BudgetUSD: 5},
		Rungs: []Rung{{
			Index:       0,
			ModelSource: "hf",
			Engine:      sizing.EngineEager,
			EstCostUSD:  1.50, // over the $0.10 ceiling
			Chosen:      sizing.Option{Tier: device.TierSmall, Backend: device.BackendNVIDIA},
		}},
	}
	_, err = b.Approve(ctx, ladder, &Proposal{Rung: &ladder.Rungs[0]})
	if err == nil {
		t.Fatal("Approve should refuse a rung over the Cedar ceiling")
	}
	if exec.launches != 0 {
		t.Fatalf("a denied rung must not launch; launches = %d", exec.launches)
	}
	// The Cedar reason surfaces verbatim through the wrapped error.
	if got := err.Error(); !strings.Contains(got, "exceeds the per-session budget ceiling") {
		t.Fatalf("deny error %q should carry the policy reason verbatim", got)
	}
}

// TestEveryProposalCarriesCost enforces "$/session on every surface" (#35): the
// real planner's proposals all carry a positive estimate.
func TestEveryProposalCarriesCost(t *testing.T) {
	p, _ := testPlanner(twoRungReplyBigFirst)
	ladder, _, err := p.PlanLadder(context.Background(), "q")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range ladder.Rungs {
		if r.EstCostUSD <= 0 {
			t.Errorf("rung %d (%s) carries no $/session estimate", r.Index, r.Model.Name)
		}
	}
}

// TestEnvelopeAndCeilingAreDistinct enforces "budget has two scopes" (#43): the
// brain's per-question envelope and Cedar's per-session ceiling are separate
// gates, and each can stop the climb independently of the other.
func TestEnvelopeAndCeilingAreDistinct(t *testing.T) {
	ctx := context.Background()

	// A two-rung ladder: rung 0 ~$0.10, rung 1 ~$1.00 (distinct $/session).
	newLadder := func() *Ladder {
		return &Ladder{
			Question: Question{Text: "q", BudgetUSD: 5},
			Rungs: []Rung{
				{Index: 0, ModelSource: "hf", Engine: sizing.EngineEager, EstCostUSD: 0.10,
					Chosen: sizing.Option{Tier: device.TierSlice, Backend: device.BackendNVIDIA, InstanceType: "g7e.xlarge"}},
				{Index: 1, ModelSource: "hf", Engine: sizing.EngineEager, EstCostUSD: 1.00,
					Chosen: sizing.Option{Tier: device.TierMid, Backend: device.BackendNVIDIA, InstanceType: "g7e.2xlarge"}},
			},
		}
	}

	// Case A — the per-question ENVELOPE stops the climb even though Cedar would
	// permit the next rung. Ceiling is generous ($5); envelope is tight.
	t.Run("envelope stops climb under a generous ceiling", func(t *testing.T) {
		pol, _ := NewCedarPolicy(Principal{Subject: "a", BudgetCeilingUSD: 5, AllowedTiers: []string{"slice", "small", "mid"}})
		exec := &countingExecutor{}
		b := &Brain{Plan: fakePlanner{}, Policy: pol, Exec: exec}
		l := newLadder()
		l.Question.BudgetUSD = 0.50 // can afford rung 0 but not rung 0+1

		// Approve rung 0 (Cedar permits: $0.10 <= $5).
		if _, err := b.Approve(ctx, l, &Proposal{Rung: &l.Rungs[0]}); err != nil {
			t.Fatalf("rung 0 should be permitted by Cedar: %v", err)
		}
		// Cedar WOULD permit rung 1 ($1.00 <= $5)...
		if ok, reason := pol.Permit(ctx, &l.Rungs[1]); !ok {
			t.Fatalf("Cedar should permit rung 1 under a $5 ceiling, denied: %s", reason)
		}
		// ...but the brain's envelope stops the climb ($0.10 spent + $1.00 > $0.50).
		// EffectPresent so the Stop is the envelope's doing, not an honest negative.
		rec, err := b.Assess(ctx, l, &Result{Rung: 0, Finding: "effect present", EffectPresent: true})
		if err != nil {
			t.Fatal(err)
		}
		if rec.Decision != Stop {
			t.Fatalf("envelope should force Stop, got %s (%s)", rec.Decision, rec.Reason)
		}
	})

	// Case B — the per-session CEILING denies the next rung even though the
	// envelope has plenty of room. Envelope is generous; ceiling is tight.
	t.Run("ceiling denies a rung the envelope could afford", func(t *testing.T) {
		// Ceiling $0.50: permits rung 0 ($0.10) but denies rung 1 ($1.00).
		pol, _ := NewCedarPolicy(Principal{Subject: "a", BudgetCeilingUSD: 0.50, AllowedTiers: []string{"slice", "small", "mid"}})
		exec := &countingExecutor{}
		b := &Brain{Plan: fakePlanner{}, Policy: pol, Exec: exec}
		l := newLadder() // envelope $5 — plenty for both rungs combined

		if _, err := b.Approve(ctx, l, &Proposal{Rung: &l.Rungs[0]}); err != nil {
			t.Fatalf("rung 0 within ceiling should be permitted: %v", err)
		}
		// The envelope alone would happily climb ($0.10 + $1.00 <= $5)...
		rec, _ := b.Assess(ctx, l, &Result{Rung: 0, Finding: "effect present", EffectPresent: true})
		if rec.Decision != Climb {
			t.Fatalf("envelope has room; Assess should recommend Climb, got %s", rec.Decision)
		}
		// ...but Cedar's per-session ceiling denies rung 1 at Approve.
		_, err := b.Approve(ctx, l, &Proposal{Rung: &l.Rungs[1]})
		if err == nil {
			t.Fatal("ceiling should deny rung 1 at Approve")
		}
		if exec.launches != 1 {
			t.Fatalf("only rung 0 should have launched; launches = %d", exec.launches)
		}
	})
}
