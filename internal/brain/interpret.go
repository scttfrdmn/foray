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
	"fmt"
	"strings"
)

// AgentCoreInterpreter is the real Interpreter: it asks Bedrock to read a rung's
// trace (the saved-value references + the nnsight that produced them) and frame
// a finding against the question, plus the honest-negative signal Assess gates
// on. It reuses the planner's Invoker seam — the LLM interprets the result; it
// never touches the money path or the acceptance decision. See ARCHITECTURE.md
// §6.2 ("results are framed against the question") and CLAUDE.md.
type AgentCoreInterpreter struct {
	Invoker Invoker
}

// interpretResponse is the strict-JSON contract the interpreter asks Bedrock for.
type interpretResponse struct {
	Finding       string `json:"finding"`        // one or two sentences, framed against the question
	EffectPresent bool   `json:"effect_present"` // false ⇒ honest negative; Assess stops the climb
}

// Interpret implements Interpreter. The brain only interprets here; it does not
// accept or advance — Assess turns this into a recommendation and the human
// decides. The caller (Brain.Interpret) stamps the rung index.
func (a *AgentCoreInterpreter) Interpret(ctx context.Context, q Question, r *Rung, raw RawResult) (*Result, error) {
	raw.NNSight = orStr(raw.NNSight, r.NNSight) // the worker echoes its nnsight; fall back to the rung's.
	prompt := fmt.Sprintf(`QUESTION: %s

RUNG: %s on %s (technique: %s)
NNSIGHT THAT RAN:
%s

SAVED VALUES: %s
VIZ: %s

Interpret this trace against the QUESTION.`,
		q.Text, r.Model.Name, r.Chosen.InstanceType, r.Technique, raw.NNSight, raw.SaveRef, raw.VizRef)

	text, err := a.Invoker.Converse(ctx, a.systemPrompt(), prompt)
	if err != nil {
		return nil, fmt.Errorf("agentcore interpret: %w", err)
	}
	var resp interpretResponse
	if err := json.Unmarshal([]byte(extractJSON(text)), &resp); err != nil {
		return nil, fmt.Errorf("agentcore interpret: parse json: %w", err)
	}
	if strings.TrimSpace(resp.Finding) == "" {
		return nil, fmt.Errorf("agentcore interpret: empty finding")
	}
	return &Result{
		VizRef:        raw.VizRef,
		Finding:       resp.Finding,
		EffectPresent: resp.EffectPresent,
	}, nil
}

// systemPrompt encodes the interpretation discipline: frame the result against
// the user's question, and report honestly whether the effect is present —
// because a null result must stop the climb ("don't pay to confirm nothing").
func (a *AgentCoreInterpreter) systemPrompt() string {
	return `You interpret a mechanistic-interpretability trace for foray (AWS Deep Inference).

You are given the user's QUESTION, the nnsight that ran, and references to the
saved values and the rendered viz. Frame what was learned against the QUESTION —
"here is what we learned about your question," not "here is a logit lens."

Report honestly whether the effect the experiment looked for is PRESENT. A null
result is a real result: if the effect is absent, say so, because the brain will
stop the climb rather than pay for a larger model to confirm nothing. Never
inflate a weak or absent effect into a positive one.

Return STRICT JSON, no prose, no markdown fences:

  {"finding": "<one or two sentences framed against the question>",
   "effect_present": <true if the experiment found the effect, false if absent>}`
}

// orStr returns a if non-empty, else b.
func orStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
