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

// Package brain plans, proposes, and interprets experiments; it never accepts.
// Work is organized as a result-gated ladder of rungs ordered cheapest-first.
// Only the first rung runs on the human's "Go"; the brain proposes climbing
// only when a lower rung warrants it, recommends stopping on honest negatives,
// and never advances a rung or declares success on its own. The human at "Go"
// is the sole acceptance node. See ARCHITECTURE.md §6.2 and CLAUDE.md.
package brain

import (
	"context"
	"fmt"

	"github.com/scttfrdmn/foray/internal/sizing"
)

// Question is the load-bearing invariant: the user writes a question, not a
// structure, and every rung serves it. BudgetUSD is the per-question envelope
// the brain enforces across the whole ladder (distinct from Cedar's per-session
// ceiling — both hold).
type Question struct {
	Text      string
	BudgetUSD float64
}

// Rung is one experiment in the ladder: a model + technique + engine + sized
// hardware + cost estimate, plus the nnsight the worker will run.
type Rung struct {
	Index       int
	Technique   string
	Model       sizing.Model
	ModelSource string // catalog kind: "hf" | "s3" | "upload" — the Cedar modelSource
	Rationale   string
	NNSight     string
	Engine      sizing.Engine
	Gradients   bool            // retains autograd graph; gates the Cedar large-save policy
	Options     []sizing.Option // hardware that fits, tightest-first
	Chosen      sizing.Option   // the option the brain picked (Options[0])
	EstCostUSD  float64
}

// Ladder is the ordered, cheapest-first plan for a question. Cursor is the next
// un-run rung; Spent accumulates approved cost against Question.BudgetUSD.
type Ladder struct {
	Question Question
	Rungs    []Rung
	Cursor   int
	Spent    float64
}

// Proposal is what the brain puts in front of the human. Exactly one of Clarify
// (a question back to the user when the ask underdetermines the experiment) or
// Rung (the next experiment awaiting "Go") is set.
type Proposal struct {
	Clarify string
	Rung    *Rung
}

// RawResult is the brain-local view of what a trace yields: references to saved
// values, never the values themselves. It mirrors gateway.TraceResult without
// importing it, so the brain keeps no dependency on the gateway (the CLI bridges
// the two). No tensor field ever belongs here — honoring the no-automatic-egress
// invariant (CLAUDE.md, ARCHITECTURE.md §6.1).
type RawResult struct {
	SaveRef string // s3:// in-region; the saved activations/outputs
	VizRef  string // rendered viz reference (pixels, not tensors)
	NNSight string // the generated code that produced this trace
}

// Result is a rung's outcome interpreted against the question. EffectPresent is
// the honest-negative signal the brain reads in Assess: false means the lower
// rung showed no effect, so don't pay to confirm nothing on a larger one. The
// brain produces this (Interpret); it never decides acceptance from it.
type Result struct {
	Rung          int
	VizRef        string
	Finding       string
	EffectPresent bool
}

// Decision is the brain's post-result recommendation. It is advice, never an
// action — the human decides whether to climb.
type Decision string

const (
	Climb Decision = "climb"
	Stop  Decision = "stop"
)

// Recommendation pairs a Decision with a reason tied to the finding and the
// question.
type Recommendation struct {
	Decision Decision
	Reason   string
}

// Planner produces a ladder from a question, or a clarifying proposal when the
// ask is underdetermined. Backed by Bedrock AgentCore in prod; a fake offline.
type Planner interface {
	PlanLadder(ctx context.Context, question string) (*Ladder, *Proposal, error)
}

// Policy gates a rung before it runs (Cedar in prod). On deny it returns a
// reason that surfaces verbatim to the user.
type Policy interface {
	Permit(ctx context.Context, r *Rung) (ok bool, reason string)
}

// Executor launches the approved rung and returns a session id (spawn in prod).
type Executor interface {
	Execute(ctx context.Context, q Question, r *Rung) (sessionID string, err error)
}

// Interpreter turns a rung's raw trace references into a Result framed against
// the question: a finding and the honest-negative signal (EffectPresent) Assess
// reads. Backed by Bedrock in prod (the LLM interprets, never touches the money
// path); a canned interpreter offline. This is how the brain "interprets" —
// distinct from accepting, which only the human's Go does.
type Interpreter interface {
	Interpret(ctx context.Context, q Question, r *Rung, raw RawResult) (*Result, error)
}

// Brain wires the four seams. It proposes and interprets; Approve (the human's
// "Go") is the only place a rung runs.
type Brain struct {
	Plan   Planner
	Policy Policy
	Exec   Executor
	Interp Interpreter
}

// Propose plans the ladder for a question and returns the first thing to put in
// front of the human: either a clarifying question or the first rung. It does
// not run anything.
func (b *Brain) Propose(ctx context.Context, question string) (*Ladder, *Proposal, error) {
	ladder, prop, err := b.Plan.PlanLadder(ctx, question)
	if err != nil {
		return nil, nil, fmt.Errorf("plan ladder: %w", err)
	}
	// A clarifying proposal short-circuits: there is no ladder to climb yet.
	if prop != nil && prop.Clarify != "" {
		return ladder, prop, nil
	}
	if ladder == nil || len(ladder.Rungs) == 0 {
		return nil, nil, fmt.Errorf("planner returned no rungs and no clarification")
	}
	ladder.Cursor = 0
	return ladder, &Proposal{Rung: &ladder.Rungs[0]}, nil
}

// Approve is the HITL acceptance node: the human said "Go" to this rung. It
// checks policy, launches the rung, advances the cursor, and books the spend.
// Nothing runs without passing through here.
func (b *Brain) Approve(ctx context.Context, l *Ladder, p *Proposal) (string, error) {
	if p == nil || p.Rung == nil {
		return "", fmt.Errorf("approve: nil proposal")
	}
	if ok, reason := b.Policy.Permit(ctx, p.Rung); !ok {
		return "", fmt.Errorf("policy denied rung %d: %s", p.Rung.Index, reason)
	}
	sid, err := b.Exec.Execute(ctx, l.Question, p.Rung)
	if err != nil {
		return "", fmt.Errorf("execute rung %d: %w", p.Rung.Index, err)
	}
	l.Cursor++
	l.Spent += p.Rung.EstCostUSD
	return sid, nil
}

// Interpret turns a rung's raw trace references into a Result framed against the
// question. It delegates to the Interpreter seam and stamps the rung index so
// the caller need not. This is the brain interpreting a result — it neither
// advances the ladder nor accepts; Assess recommends and the human decides.
func (b *Brain) Interpret(ctx context.Context, l *Ladder, r *Rung, raw RawResult) (*Result, error) {
	if r == nil {
		return nil, fmt.Errorf("interpret: nil rung")
	}
	res, err := b.Interp.Interpret(ctx, l.Question, r, raw)
	if err != nil {
		return nil, fmt.Errorf("interpret rung %d: %w", r.Index, err)
	}
	res.Rung = r.Index
	return res, nil
}

// Assess interprets a rung's result and recommends climbing or stopping. It is
// a recommendation only — the brain never advances the ladder or declares
// success itself. It stops at the top of the ladder, stops on an honest negative
// rather than paying to confirm nothing, and stops when the next rung would
// exceed the per-question envelope.
func (b *Brain) Assess(ctx context.Context, l *Ladder, res *Result) (*Recommendation, error) {
	next := res.Rung + 1
	if next >= len(l.Rungs) {
		return &Recommendation{Decision: Stop, Reason: "answered — the ladder is complete"}, nil
	}
	// Honest negative: the lower rung showed no effect, so don't pay to confirm
	// nothing on a larger model. This is checked before the budget gate — a null
	// result stops the climb regardless of how much envelope is left.
	if !res.EffectPresent {
		return &Recommendation{
			Decision: Stop,
			Reason: fmt.Sprintf("no effect at %s; likely absent — don't pay for %s to confirm nothing",
				l.Rungs[res.Rung].Model.Name, l.Rungs[next].Model.Name),
		}, nil
	}
	if l.Spent+l.Rungs[next].EstCostUSD > l.Question.BudgetUSD {
		return &Recommendation{
			Decision: Stop,
			Reason: fmt.Sprintf("budget envelope: next rung (~$%.2f) would exceed the $%.2f left for this question",
				l.Rungs[next].EstCostUSD, l.Question.BudgetUSD-l.Spent),
		}, nil
	}
	return &Recommendation{
		Decision: Climb,
		Reason:   "the lower rung warrants confirming the effect scales",
	}, nil
}

// NextProposal returns the next un-run rung as a proposal, or nil if the ladder
// is exhausted. Climbing is always a fresh proposal awaiting another "Go".
func (b *Brain) NextProposal(ctx context.Context, l *Ladder) *Proposal {
	if l.Cursor < 0 || l.Cursor >= len(l.Rungs) {
		return nil
	}
	return &Proposal{Rung: &l.Rungs[l.Cursor]}
}
