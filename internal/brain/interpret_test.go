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

	"github.com/scttfrdmn/foray/internal/sizing"
)

// TestInterpretParsesFindingAndSignal proves the interpreter pulls a finding and
// the honest-negative signal out of the model's JSON, tolerating a fence, and
// stamps the rung index via Brain.Interpret.
func TestInterpretParsesFindingAndSignal(t *testing.T) {
	ctx := context.Background()
	inv := &cannedInvoker{reply: "```json\n{\"finding\":\"Paris emerges by layer 9\",\"effect_present\":true}\n```"}
	b := &Brain{Interp: &AgentCoreInterpreter{Invoker: inv}}
	l := &Ladder{Question: Question{Text: "why Paris?"}, Rungs: []Rung{{Index: 0, Model: someModel("gpt2")}}}

	res, err := b.Interpret(ctx, l, &l.Rungs[0], RawResult{SaveRef: "s3://b/saves/", VizRef: "viz://x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rung != 0 || res.Finding != "Paris emerges by layer 9" || !res.EffectPresent {
		t.Fatalf("interpret = %+v", res)
	}
	if res.VizRef != "viz://x" {
		t.Errorf("viz ref not carried through: %q", res.VizRef)
	}
	// The question must reach the prompt — results are framed against it.
	if !strings.Contains(inv.gotPrompt, "why Paris?") {
		t.Errorf("question not in interpret prompt: %q", inv.gotPrompt)
	}
}

// TestInterpretHonestNegative proves an absent effect is reported, not inflated.
func TestInterpretHonestNegative(t *testing.T) {
	ctx := context.Background()
	inv := &cannedInvoker{reply: `{"finding":"no sharpening at any layer; the association is absent in GPT-2","effect_present":false}`}
	b := &Brain{Interp: &AgentCoreInterpreter{Invoker: inv}}
	l := &Ladder{Question: Question{Text: "q"}, Rungs: []Rung{{Index: 0, Model: someModel("gpt2")}}}

	res, err := b.Interpret(ctx, l, &l.Rungs[0], RawResult{})
	if err != nil {
		t.Fatal(err)
	}
	if res.EffectPresent {
		t.Fatal("interpreter inflated an absent effect into a positive one")
	}
}

// TestAssessStopsOnHonestNegative is the invariant: a null result stops the
// climb regardless of remaining budget ("don't pay to confirm nothing").
func TestAssessStopsOnHonestNegative(t *testing.T) {
	ctx := context.Background()
	b := NewFake()
	l, _, err := b.Propose(ctx, "q")
	if err != nil {
		t.Fatal(err)
	}
	// Plenty of envelope, but the lower rung found nothing.
	rec, err := b.Assess(ctx, l, &Result{Rung: 0, Finding: "no effect", EffectPresent: false})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Decision != Stop {
		t.Fatalf("honest negative must Stop the climb, got %s (%s)", rec.Decision, rec.Reason)
	}
	if !strings.Contains(rec.Reason, "confirm nothing") {
		t.Errorf("Stop reason should name the honest-negative rationale: %q", rec.Reason)
	}
}

// TestAssessClimbsOnPositiveWithBudget proves a positive lower rung with envelope
// room climbs — the complement of the honest-negative gate.
func TestAssessClimbsOnPositiveWithBudget(t *testing.T) {
	ctx := context.Background()
	b := NewFake()
	l, _, err := b.Propose(ctx, "q")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := b.Assess(ctx, l, &Result{Rung: 0, EffectPresent: true})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Decision != Climb {
		t.Fatalf("positive result with envelope room should Climb, got %s (%s)", rec.Decision, rec.Reason)
	}
}

func someModel(name string) sizing.Model { return sizing.Model{Name: name} }
