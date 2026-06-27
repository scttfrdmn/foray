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
	"fmt"

	"github.com/scttfrdmn/foray/internal/device"
	"github.com/scttfrdmn/foray/internal/sizing"
)

// ExpertSpec is the expert on-ramp (ARCHITECTURE.md §5, on-ramp 3): the user
// names the model + technique + hardware directly and skips the planning
// dialog. The brain does not propose here — it builds exactly the one rung the
// user specified, still subject to the same Cedar gate and HITL Go on Approve.
type ExpertSpec struct {
	Question    string        // the user's question, if they stated one; else synthesized
	ModelSource string        // catalog Kind: "hf" | "s3" | "upload" (the Cedar modelSource)
	ModelRef    string        // canonical model reference, for display and the worker
	Technique   string        // logit-lens | attribution | steering | sae | generate
	Engine      sizing.Engine // empty ⇒ EngineEager (the universal default)
	Instance    device.Option // the chosen hardware (resolved from --hardware)
	Gradients   bool          // gates the Cedar large-save policy
}

// ExpertLadder builds a single-rung ladder from explicit knobs — no Bedrock. The
// user has chosen the hardware, so we do not run the sizing footprint search;
// we price the chosen instance via the Pricer seam (truffle in prod, a stub in
// tests) and stamp it on the rung. The result feeds the same Approve → trace →
// Assess loop as the planned path; the difference is only how the rung was
// chosen. Assess will Stop after this lone rung (it is the top of the ladder),
// so the expert path is honestly one experiment, not a climb.
func ExpertLadder(ctx context.Context, spec ExpertSpec, pricer Pricer, budgetUSD float64) (*Ladder, error) {
	if spec.ModelRef == "" {
		return nil, fmt.Errorf("expert: model reference is required")
	}
	if spec.Instance.InstanceType == "" {
		return nil, fmt.Errorf("expert: a hardware instance is required")
	}
	engine := spec.Engine
	if engine == "" {
		engine = sizing.EngineEager // the universal path (gradients, arbitrary modules)
	}

	chosen := sizing.Option{
		Backend:      spec.Instance.Backend,
		Tier:         spec.Instance.Tier,
		InstanceType: spec.Instance.InstanceType,
		GPU:          spec.Instance.GPU,
		GPUMemGB:     spec.Instance.GPUMemGB,
	}
	rung := Rung{
		Index:       0,
		Technique:   spec.Technique,
		Model:       sizing.Model{Name: spec.ModelRef},
		ModelSource: spec.ModelSource,
		Rationale:   "expert override: model, technique, and hardware chosen directly",
		Engine:      engine,
		Gradients:   spec.Gradients,
		Options:     []sizing.Option{chosen},
		Chosen:      chosen,
	}

	cost, err := pricer.SessionCostUSD(ctx, chosen.InstanceType)
	if err != nil {
		return nil, fmt.Errorf("expert: price %s: %w", chosen.InstanceType, err)
	}
	rung.EstCostUSD = cost

	budget := budgetUSD
	if budget <= 0 {
		budget = defaultQuestionBudgetUSD
	}
	return &Ladder{
		Question: Question{Text: expertQuestion(spec), BudgetUSD: budget},
		Rungs:    []Rung{rung},
		Cursor:   0,
	}, nil
}

// expertQuestion frames the expert run so the question invariant still holds:
// even when the user skips the dialog, the rung serves a stated question. We
// keep the user's own question when they gave one; otherwise synthesize one from
// the chosen knobs so results are still framed against something.
func expertQuestion(spec ExpertSpec) string {
	if spec.Question != "" {
		return spec.Question
	}
	return fmt.Sprintf("expert run: %s on %s (%s)", spec.Technique, spec.ModelRef, spec.Instance.InstanceType)
}
