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

	"github.com/scttfrdmn/foray/internal/sizing"
	"github.com/scttfrdmn/foray/internal/spore"
)

// Fakes for FORAY_FAKE=1: a deterministic GPT-2 -> 8B ladder so the whole
// propose -> Go -> assess -> climb loop runs with no AWS. Used by the CLI and the
// CI gate (make demo-fake). The findings are canned but honest in shape: the
// cheap rung shows the effect, the next confirms it scales.

// NewFake builds a Brain wired with offline collaborators. It launches through a
// fresh spore fake spawn so the executor produces real, lookup-able session ids
// (the CLI's gateway tracer resolves them via Spawn.Status).
func NewFake() *Brain {
	return NewFakeWith(spore.NewFake().Spawn)
}

// NewFakeWith builds the offline Brain over a caller-supplied spawn so the CLI
// can share one fake spawn between the brain's SpawnExecutor and the gateway's
// idle bridge — the launched session then exists for KeepWarm/Status. The
// planner, policy, and interpreter stay canned; only the executor is real, so
// the offline loop exercises the same SpawnExecutor code the real path uses.
func NewFakeWith(sp spore.Spawn) *Brain {
	return &Brain{
		Plan:   fakePlanner{},
		Policy: fakePolicy{},
		Exec:   SpawnExecutor{Spawn: sp},
		Interp: fakeInterpreter{},
	}
}

type fakePlanner struct{}

func (fakePlanner) PlanLadder(_ context.Context, question string) (*Ladder, *Proposal, error) {
	gpt2 := Rung{
		Index:       0,
		Technique:   "logit-lens",
		Model:       sizing.Model{Name: "openai-community/gpt2", ParamsB: 0.124, BytesPer: 2, Layers: 12, HiddenDim: 768, CtxTokens: 1024},
		ModelSource: "hf",
		Rationale:   "cheapest model that could show the effect — cents to find out",
		NNSight: `with model.trace("The Eiffel Tower is in the city of"):
    layers = [model.transformer.h[i].output[0].save() for i in range(12)]`,
	}
	eight := Rung{
		Index:       1,
		Technique:   "logit-lens",
		Model:       sizing.Model{Name: "meta-llama/Llama-3.1-8B", ParamsB: 8, BytesPer: 2, Layers: 32, HiddenDim: 4096, CtxTokens: 8192},
		ModelSource: "hf",
		Rationale:   "confirm the effect scales beyond a toy model",
		NNSight: `with model.trace("The Eiffel Tower is in the city of"):
    layers = [model.model.layers[i].output[0].save() for i in range(32)]`,
	}
	sizeRung(&gpt2)
	sizeRung(&eight)

	l := &Ladder{
		Question: Question{Text: question, BudgetUSD: 5.00},
		Rungs:    []Rung{gpt2, eight},
		Cursor:   0,
	}
	return l, nil, nil
}

// sizeRung fills in engine + hardware options from the model + intervention.
// Logit lens captures per-layer hidden states, so SaveAllLayers is true, which
// forces the eager engine.
func sizeRung(r *Rung) {
	plan := sizing.Size(r.Model, sizing.Intervention{Technique: r.Technique, SaveAllLayers: true})
	r.Engine = plan.Footprint.Engine
	r.Options = plan.Options
	if len(plan.Options) > 0 {
		r.Chosen = plan.Options[0] // cheapest/tightest that fits
	}
	// A toy model on a tiny footprint should still cost ~cents; price the chosen
	// tier at a stub session rate. Real pricing comes from truffle.
	r.EstCostUSD = stubSessionCost(r.Model)
}

// stubSessionCost is a placeholder $/session: a couple cents floor, scaling with
// model size. GPT-2 -> ~$0.02, 8B -> ~$0.20. Replaced by truffle Spot pricing.
func stubSessionCost(m sizing.Model) float64 {
	c := 0.02 + m.ParamsB*0.0225
	return float64(int(c*100+0.5)) / 100 // round to cents
}

type fakePolicy struct{}

func (fakePolicy) Permit(_ context.Context, r *Rung) (bool, string) {
	// Permit everything in the fake; the per-rung Cedar gate is exercised in prod.
	_ = r
	return true, ""
}

// fakeInterpreter is the offline Interpreter: it frames a canned finding against
// the question per rung. Both fake findings are positive (EffectPresent: true)
// so the offline ladder climbs — the honest-negative path is exercised by unit
// tests that hand Assess an EffectPresent:false result directly.
type fakeInterpreter struct{}

func (fakeInterpreter) Interpret(_ context.Context, _ Question, r *Rung, raw RawResult) (*Result, error) {
	return &Result{
		Rung:          r.Index,
		VizRef:        orStr(raw.VizRef, raw.SaveRef),
		Finding:       fakeFinding(r.Index),
		EffectPresent: true,
	}, nil
}

// fakeFinding returns the canned, question-framed finding for a rung. The cheap
// rung shows the effect; the next confirms it scales — canned but honest in
// shape (ARCHITECTURE.md §6.2).
func fakeFinding(rungIndex int) string {
	findings := []string{
		"GPT-2: the top token sharpens to 'Paris' by layer 9 (p≈0.41). The association is present even in a toy model.",
		"Llama-3.1-8B: 'Paris' emerges around layer 20 (p≈0.78) — stronger and earlier-resolved. The effect scales.",
	}
	if rungIndex >= 0 && rungIndex < len(findings) {
		return findings[rungIndex]
	}
	return "result captured."
}

// FakeResult returns a canned result for a rung so tests can Assess and climb
// without going through the gateway. Both findings are positive, so the loop
// climbs; the honest-negative gate is tested with a hand-built negative Result.
func FakeResult(sessionID string, rungIndex int) *Result {
	return &Result{
		Rung:          rungIndex,
		VizRef:        "s3://your-bucket/sessions/" + sessionID + "/saves/",
		Finding:       fakeFinding(rungIndex),
		EffectPresent: true,
	}
}
