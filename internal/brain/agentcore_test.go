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
	"encoding/json"
	"testing"

	"github.com/scttfrdmn/foray/internal/spore"
)

// cannedInvoker returns a fixed response, recording the prompt it saw. It stands
// in for Bedrock so the planner is exercised with no AWS.
type cannedInvoker struct {
	reply     string
	gotSystem string
	gotPrompt string
}

func (c *cannedInvoker) Converse(_ context.Context, system, prompt string) (string, error) {
	c.gotSystem, c.gotPrompt = system, prompt
	return c.reply, nil
}

// testPlanner wires the planner with a canned invoker and the offline truffle
// fake (real pricing logic, deterministic numbers).
func testPlanner(reply string) (*AgentCorePlanner, *cannedInvoker) {
	inv := &cannedInvoker{reply: reply}
	return &AgentCorePlanner{
		Invoker: inv,
		Pricer:  NewTrufflePricer(spore.NewFake().Truffle),
	}, inv
}

// A two-rung reply, deliberately given largest-first so the test proves the
// planner re-orders cheapest-first by the priced estimate (not by reply order).
const twoRungReplyBigFirst = `{"rungs":[
  {"model":"meta-llama/Llama-3.1-8B","model_source":"hf","params_b":8,"bytes_per":2,"layers":32,"hidden_dim":4096,"ctx_tokens":8192,"technique":"logit-lens","save_all_layers":true,"rationale":"confirm it scales","nnsight":"..."},
  {"model":"openai-community/gpt2","model_source":"hf","params_b":0.124,"bytes_per":2,"layers":12,"hidden_dim":768,"ctx_tokens":1024,"technique":"logit-lens","save_all_layers":true,"rationale":"cheapest first","nnsight":"..."}
]}`

func TestAgentCorePlanCheapestFirst(t *testing.T) {
	p, _ := testPlanner(twoRungReplyBigFirst)
	ladder, prop, err := p.PlanLadder(context.Background(), "why does it store France as Paris?")
	if err != nil {
		t.Fatal(err)
	}
	if prop != nil {
		t.Fatalf("expected a ladder, got a clarify proposal: %+v", prop)
	}
	if len(ladder.Rungs) != 2 {
		t.Fatalf("want 2 rungs, got %d", len(ladder.Rungs))
	}
	// Re-ordered cheapest-first: GPT-2 (smaller, cheaper) must be rung 0.
	if ladder.Rungs[0].Model.Name != "openai-community/gpt2" {
		t.Errorf("rung 0 = %q, want the cheaper gpt2", ladder.Rungs[0].Model.Name)
	}
	if ladder.Rungs[0].EstCostUSD > ladder.Rungs[1].EstCostUSD {
		t.Errorf("rungs not cheapest-first: $%.2f then $%.2f", ladder.Rungs[0].EstCostUSD, ladder.Rungs[1].EstCostUSD)
	}
	// Index is reassigned to climb order after sorting.
	for i, r := range ladder.Rungs {
		if r.Index != i {
			t.Errorf("rung at position %d has Index %d", i, r.Index)
		}
		if r.EstCostUSD <= 0 {
			t.Errorf("rung %d carries no $/session estimate", i) // invariant #35
		}
		if r.Chosen.InstanceType == "" {
			t.Errorf("rung %d was not sized to hardware", i)
		}
	}
}

func TestAgentCoreClarifyShortCircuits(t *testing.T) {
	p, _ := testPlanner(`{"clarify":"Which behavior do you mean — refusal, or factual recall?"}`)
	ladder, prop, err := p.PlanLadder(context.Background(), "why does it do that?")
	if err != nil {
		t.Fatal(err)
	}
	if ladder != nil {
		t.Fatalf("a clarify should carry no ladder, got %+v", ladder)
	}
	if prop == nil || prop.Clarify == "" {
		t.Fatalf("expected a clarifying proposal, got %+v", prop)
	}
}

func TestAgentCoreToleratesFencedJSON(t *testing.T) {
	// A model that wraps JSON in a ```json fence despite instructions.
	fenced := "```json\n" + `{"clarify":"name the model?"}` + "\n```"
	p, _ := testPlanner(fenced)
	_, prop, err := p.PlanLadder(context.Background(), "q")
	if err != nil {
		t.Fatalf("should tolerate fenced JSON: %v", err)
	}
	if prop == nil || prop.Clarify == "" {
		t.Fatal("expected the clarify to parse out of the fence")
	}
}

// TestAgentCoreBudgetStamp confirms the planner stamps the configured
// per-question envelope (or the default) on the ladder.
func TestAgentCoreBudgetStamp(t *testing.T) {
	p, _ := testPlanner(twoRungReplyBigFirst)
	p.BudgetUSD = 2.50
	ladder, _, err := p.PlanLadder(context.Background(), "q")
	if err != nil {
		t.Fatal(err)
	}
	if ladder.Question.BudgetUSD != 2.50 {
		t.Errorf("envelope = $%.2f, want $2.50", ladder.Question.BudgetUSD)
	}
}

// TestExtractJSONRawControls verifies the live-path repair: a model returning a
// multi-line code value with LITERAL newlines/tabs inside a JSON string (which
// encoding/json rejects) is escaped so it parses, while structural whitespace
// and already-escaped sequences are left intact. Regression for the real-deploy
// "invalid character '\n' in string literal" failure.
func TestExtractJSONRawControls(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"raw newline in value", "{\"nnsight\": \"with model.trace():\n  out.save()\"}"},
		{"raw tab in value", "{\"nnsight\": \"a\tb\"}"},
		{"fenced with raw newline", "```json\n{\"nnsight\": \"line1\nline2\"}\n```"},
		{"already escaped stays valid", `{"nnsight": "line1\nline2"}`},
		{"structural newlines only", "{\n  \"clarify\": \"which layers?\"\n}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractJSON(tt.in)
			var v map[string]any
			if err := json.Unmarshal([]byte(got), &v); err != nil {
				t.Fatalf("extractJSON output is not valid JSON: %v\n  got: %q", err, got)
			}
		})
	}
}

// TestExtractJSONPreservesContent confirms the escaping round-trips the value:
// the repaired string, once parsed, still carries the original newline.
func TestExtractJSONPreservesContent(t *testing.T) {
	got := extractJSON("{\"nnsight\": \"a\nb\"}")
	var v struct {
		NNSight string `json:"nnsight"`
	}
	if err := json.Unmarshal([]byte(got), &v); err != nil {
		t.Fatal(err)
	}
	if v.NNSight != "a\nb" {
		t.Errorf("value = %q, want %q", v.NNSight, "a\nb")
	}
}
