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
)

func TestFakeLadderWalk(t *testing.T) {
	ctx := context.Background()
	b := NewFake()

	ladder, prop, err := b.Propose(ctx, "why does it store France as Paris?")
	if err != nil {
		t.Fatal(err)
	}
	if prop == nil || prop.Clarify != "" {
		t.Fatalf("expected a rung proposal, got %+v", prop)
	}
	if len(ladder.Rungs) != 2 {
		t.Fatalf("fake ladder should have 2 rungs, got %d", len(ladder.Rungs))
	}
	if ladder.Cursor != 0 {
		t.Fatalf("cursor should start at 0, got %d", ladder.Cursor)
	}

	sid, err := b.Approve(ctx, ladder, prop)
	if err != nil {
		t.Fatal(err)
	}
	if ladder.Cursor != 1 {
		t.Fatalf("approve should advance cursor to 1, got %d", ladder.Cursor)
	}
	if ladder.Spent <= 0 {
		t.Fatal("approve should accumulate spend")
	}

	rec, err := b.Assess(ctx, ladder, FakeResult(sid, 0))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Decision != Climb {
		t.Fatalf("after the cheap rung, expected Climb, got %s", rec.Decision)
	}

	prop = b.NextProposal(ctx, ladder)
	if prop == nil {
		t.Fatal("expected a proposal for the second rung")
	}
	sid, _ = b.Approve(ctx, ladder, prop)
	if ladder.Cursor != 2 {
		t.Fatalf("cursor should be 2 after the second rung, got %d", ladder.Cursor)
	}

	rec, _ = b.Assess(ctx, ladder, FakeResult(sid, 1))
	if rec.Decision != Stop {
		t.Fatalf("a complete ladder should Stop, got %s", rec.Decision)
	}
	if np := b.NextProposal(ctx, ladder); np != nil {
		t.Fatal("there should be no proposal past the top rung")
	}
}

func TestBudgetEnvelopeStopsClimb(t *testing.T) {
	ctx := context.Background()
	b := NewFake()
	ladder, prop, _ := b.Propose(ctx, "q")
	// Afford only the first rung.
	ladder.Question.BudgetUSD = ladder.Rungs[0].EstCostUSD + 0.001

	sid, _ := b.Approve(ctx, ladder, prop)
	rec, _ := b.Assess(ctx, ladder, FakeResult(sid, 0))
	if rec.Decision != Stop {
		t.Fatalf("budget envelope should force Stop, got %s (%s)", rec.Decision, rec.Reason)
	}
}
