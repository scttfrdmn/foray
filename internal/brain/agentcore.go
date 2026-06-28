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
	"sort"
	"strings"

	"github.com/scttfrdmn/foray/internal/sizing"
)

// Invoker is the brain's one call into Bedrock: a single Converse round-trip
// returning the model's text. Keeping it an interface isolates the AWS SDK to
// bedrock.go and lets the planner be exercised with a canned invoker — no AWS,
// no creds, CI-green.
type Invoker interface {
	Converse(ctx context.Context, system, prompt string) (text string, err error)
}

// AgentCorePlanner is the real Planner: it asks Bedrock for a cheapest-first
// ladder (or a clarifying question), then sizes and prices each rung locally so
// the cost numbers stay deterministic and the LLM never touches the money path.
// The LLM proposes *what* to run; Go owns *how big* and *how much*. See
// ARCHITECTURE.md §6.2.
type AgentCorePlanner struct {
	Invoker    Invoker
	Pricer     Pricer
	Techniques []string // techniques the worker supports; constrains the model's choice
	BudgetUSD  float64  // per-question envelope to stamp on the ladder (brain-enforced)
}

// planResponse is the JSON contract the planner asks Bedrock to return. Exactly
// one of Clarify or Rungs is populated. The model chooses the experiment shape;
// foray sizes and prices it.
type planResponse struct {
	Clarify string         `json:"clarify"`
	Rungs   []planRungSpec `json:"rungs"`
}

// planRungSpec is one rung as the model describes it. The numeric model fields
// feed sizing.Model; the intervention flags feed sizing.Intervention; the rest
// is rationale and the generated nnsight the worker will run.
type planRungSpec struct {
	Model         string  `json:"model"`        // HF id, s3:// URI, or upload:<id>
	ModelSource   string  `json:"model_source"` // "hf" | "s3" | "upload"
	ParamsB       float64 `json:"params_b"`     // parameter count, billions
	BytesPer      int     `json:"bytes_per"`    // 2=fp16/bf16, 1=fp8
	Layers        int     `json:"layers"`
	HiddenDim     int     `json:"hidden_dim"`
	CtxTokens     int     `json:"ctx_tokens"`
	Technique     string  `json:"technique"`
	SaveAllLayers bool    `json:"save_all_layers"`
	Gradients     bool    `json:"gradients"`
	Prompts       int     `json:"prompts"`
	Rationale     string  `json:"rationale"`
	NNSight       string  `json:"nnsight"`
}

// PlanLadder implements Planner. It returns a clarifying proposal when the model
// says the ask is underdetermined, otherwise a sized, priced, cheapest-first
// ladder. It never runs anything.
func (p *AgentCorePlanner) PlanLadder(ctx context.Context, question string) (*Ladder, *Proposal, error) {
	raw, err := p.Invoker.Converse(ctx, p.systemPrompt(), question)
	if err != nil {
		return nil, nil, fmt.Errorf("agentcore converse: %w", err)
	}
	var resp planResponse
	if err := json.Unmarshal([]byte(extractJSON(raw)), &resp); err != nil {
		return nil, nil, fmt.Errorf("agentcore: parse plan json: %w", err)
	}

	// A clarifying question short-circuits: there is no ladder yet (the ask
	// underdetermines the experiment). The brain puts the question to the user.
	if strings.TrimSpace(resp.Clarify) != "" {
		return nil, &Proposal{Clarify: resp.Clarify}, nil
	}
	if len(resp.Rungs) == 0 {
		return nil, nil, fmt.Errorf("agentcore: planner returned no rungs and no clarification")
	}

	rungs := make([]Rung, 0, len(resp.Rungs))
	for _, spec := range resp.Rungs {
		r, err := p.buildRung(ctx, spec)
		if err != nil {
			return nil, nil, err
		}
		rungs = append(rungs, r)
	}

	// Order cheapest-first by the priced estimate, then re-index so Rung.Index is
	// the rung's position in the climb. Cheap-before-expensive is the discipline,
	// and the cost numbers — not the model's word — decide the order. When two
	// rungs share a hardware tier (so $/session ties), the smaller model goes
	// first: it is the cheaper experiment to reason about and climb from.
	sort.SliceStable(rungs, func(i, j int) bool {
		if rungs[i].EstCostUSD != rungs[j].EstCostUSD {
			return rungs[i].EstCostUSD < rungs[j].EstCostUSD
		}
		return rungs[i].Model.ParamsB < rungs[j].Model.ParamsB
	})
	for i := range rungs {
		rungs[i].Index = i
	}

	budget := p.BudgetUSD
	if budget <= 0 {
		budget = defaultQuestionBudgetUSD
	}
	return &Ladder{
		Question: Question{Text: question, BudgetUSD: budget},
		Rungs:    rungs,
		Cursor:   0,
	}, nil, nil
}

// defaultQuestionBudgetUSD is the per-question envelope when the caller does not
// set one. Modest by design: the brain refuses to climb past it.
const defaultQuestionBudgetUSD = 5.00

// buildRung turns a model-proposed spec into a sized, priced Rung. Sizing and
// pricing are local and deterministic; the model only chose the experiment.
func (p *AgentCorePlanner) buildRung(ctx context.Context, spec planRungSpec) (Rung, error) {
	m := sizing.Model{
		Name:      spec.Model,
		ParamsB:   spec.ParamsB,
		BytesPer:  spec.BytesPer,
		Layers:    spec.Layers,
		HiddenDim: spec.HiddenDim,
		CtxTokens: spec.CtxTokens,
	}
	iv := sizing.Intervention{
		Technique:     spec.Technique,
		SaveAllLayers: spec.SaveAllLayers,
		Gradients:     spec.Gradients,
		Prompts:       spec.Prompts,
	}
	plan := sizing.Size(m, iv)

	r := Rung{
		Technique:   spec.Technique,
		Model:       m,
		ModelSource: spec.ModelSource,
		Rationale:   spec.Rationale,
		NNSight:     spec.NNSight,
		Engine:      plan.Footprint.Engine,
		Gradients:   spec.Gradients,
		Options:     plan.Options,
	}
	if len(plan.Options) == 0 {
		return Rung{}, fmt.Errorf("agentcore: no hardware fits %q (%.0f GB needed)", spec.Model, plan.Footprint.TotalGB)
	}
	r.Chosen = plan.Options[0] // cheapest/tightest that fits

	cost, err := p.Pricer.SessionCostUSD(ctx, r.Chosen.InstanceType)
	if err != nil {
		return Rung{}, fmt.Errorf("agentcore: price rung %q: %w", spec.Model, err)
	}
	r.EstCostUSD = cost
	return r, nil
}

// systemPrompt encodes the planning discipline: a cheapest-first ladder that
// serves the question, or a single clarifying question when naming a model would
// be premature. The model returns strict JSON; foray sizes and prices it.
func (p *AgentCorePlanner) systemPrompt() string {
	techniques := strings.Join(p.Techniques, ", ")
	if techniques == "" {
		techniques = "logit-lens, attribution, steering, sae, generate"
	}
	return `You plan mechanistic-interpretability experiments for foray (AWS Deep Inference).

A user gives you a QUESTION about model internals, not a structure. Your job is
to propose the cheapest experiment that could answer it, then progressively
larger confirmations — a cheapest-first ladder (e.g. GPT-2 for cents, then an 8B
to confirm it scales, then larger only if warranted). Never start large.

If the question underdetermines the experiment (you would have to guess what the
user means), do NOT pick a model. Instead return a single clarifying question.

Return STRICT JSON, no prose, no markdown fences. Exactly one of:

  {"clarify": "<one short question back to the user>"}

or:

  {"rungs": [
    {
      "model": "<HF id | s3:// URI | upload:<id>>",
      "model_source": "hf" | "s3" | "upload",
      "params_b": <number, billions of parameters>,
      "bytes_per": <2 for fp16/bf16, 1 for fp8>,
      "layers": <int>, "hidden_dim": <int>, "ctx_tokens": <int>,
      "technique": "<one of: ` + techniques + `>",
      "save_all_layers": <bool — true for per-layer captures like logit-lens>,
      "gradients": <bool — true for attribution/steering that need autograd>,
      "prompts": <int — batch size; >8 routes to vLLM for gradient-free sweeps>,
      "rationale": "<why this rung, tied to the question>",
      "nnsight": "<the nnsight code the worker runs for this rung>"
    }
  ]}

Order rungs cheapest-first (smallest model first). Two or three rungs is typical.
Set gradients only when the technique truly needs autograd. The nnsight must be
runnable against nnsight's LanguageModel API.`
}

// extractJSON pulls the first JSON object out of a model response, tolerating
// stray prose or ```json fences a model may add despite instructions, and
// repairs the one malformation we see live: a model emitting a multi-line code
// value (the generated nnsight) with *literal* newlines/tabs inside the JSON
// string, which encoding/json rejects ("invalid character '\n' in string
// literal"). The canned fakes are pre-escaped so this only bites the real path.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	// Strip a leading ```json / ``` fence if present.
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j >= i {
			s = s[i : j+1]
		}
	}
	return escapeRawControlsInStrings(s)
}

// escapeRawControlsInStrings rewrites literal control characters (newline, tab,
// carriage return) that appear *inside* JSON string literals into their escaped
// forms, leaving control characters in structural whitespace untouched. A model
// returning indented code as a JSON value produces exactly this; the JSON spec
// forbids unescaped controls in strings, so we repair rather than reject.
func escapeRawControlsInStrings(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inString := false
	escaped := false
	for _, r := range s {
		if inString && !escaped {
			switch r {
			case '\n':
				b.WriteString(`\n`)
				continue
			case '\r':
				b.WriteString(`\r`)
				continue
			case '\t':
				b.WriteString(`\t`)
				continue
			}
		}
		switch {
		case escaped:
			escaped = false
		case r == '\\' && inString:
			escaped = true
		case r == '"':
			inString = !inString
		}
		b.WriteRune(r)
	}
	return b.String()
}
