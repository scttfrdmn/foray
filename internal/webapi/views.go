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

package webapi

import (
	"github.com/scttfrdmn/foray/internal/brain"
	"github.com/scttfrdmn/foray/internal/gateway"
)

// rungView is the flat, page-friendly projection of a brain.Rung: just the
// fields the proposed-experiment card renders (model + technique + hardware + $,
// the rationale, and the generated nnsight escape hatch). The full Rung lives in
// the carried Ladder; this view spares the client from reaching into nested
// sizing types. Carries no tensors — references only, by construction.
type rungView struct {
	Index      int     `json:"index"`
	Model      string  `json:"model"`
	Layers     int     `json:"layers"` // seeds the page's illustrative strata viz
	Technique  string  `json:"technique"`
	Engine     string  `json:"engine"`
	Hardware   string  `json:"hardware"`   // tier label, e.g. "g7e RTX PRO 6000 MIG"
	Instance   string  `json:"instance"`   // EC2 instance type, e.g. "g7e.2xlarge"
	GPU        string  `json:"gpu"`        // e.g. "RTX PRO 6000 · 96GB"
	GPUMemGB   int     `json:"gpuMemGB"`   //
	EstCostUSD float64 `json:"estCostUSD"` // the live cost meter binds to this (#52)
	Rationale  string  `json:"rationale"`  // "why" — cheapest first, confirm it scales
	NNSight    string  `json:"nnsight"`    // the code the worker will run (the escape hatch)
}

// viewRung projects a Rung. nil-safe so callers can pass NextProposal's rung.
func viewRung(r *brain.Rung) *rungView {
	if r == nil {
		return nil
	}
	v := &rungView{
		Index:      r.Index,
		Model:      r.Model.Name,
		Layers:     r.Model.Layers,
		Technique:  r.Technique,
		Engine:     string(r.Engine),
		Instance:   r.Chosen.InstanceType,
		GPU:        r.Chosen.GPU,
		GPUMemGB:   r.Chosen.GPUMemGB,
		EstCostUSD: r.EstCostUSD,
		Rationale:  r.Rationale,
		NNSight:    r.NNSight,
	}
	v.Hardware = r.Chosen.Tier.String()
	return v
}

// resultView is a rung's interpreted outcome for the page: the finding framed
// against the question, the honest-negative signal, and the save/viz references
// (no tensors — the no-automatic-egress invariant; only pixels and s3:// refs).
type resultView struct {
	Rung          int    `json:"rung"`
	Finding       string `json:"finding"`
	EffectPresent bool   `json:"effectPresent"`
	SaveRef       string `json:"saveRef"` // s3:// in-region; download via /api/export
	VizRef        string `json:"vizRef"`  // rendered viz reference (pixels, not tensors)
}

func viewResult(res *brain.Result, tr gateway.TraceResult) *resultView {
	return &resultView{
		Rung:          res.Rung,
		Finding:       res.Finding,
		EffectPresent: res.EffectPresent,
		SaveRef:       tr.SaveRef,
		VizRef:        tr.VizRef,
	}
}

// recommendation is the brain's climb/stop advice. Advice only — the human
// decides whether to climb, and only a fresh approve launches the next rung.
type recommendation struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}
