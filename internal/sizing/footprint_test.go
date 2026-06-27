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

package sizing

import "testing"

func mdl(paramsB float64, bytesPer, layers, hidden, ctx int) Model {
	return Model{ParamsB: paramsB, BytesPer: bytesPer, Layers: layers, HiddenDim: hidden, CtxTokens: ctx}
}

func TestLogitLensForcesEager(t *testing.T) {
	p := Size(mdl(8, 2, 32, 4096, 8192), Intervention{Technique: "logit-lens", SaveAllLayers: true})
	if p.Footprint.Engine != EngineEager {
		t.Fatalf("logit lens should be eager, got %s", p.Footprint.Engine)
	}
	if p.Footprint.TotalGB <= 16 {
		t.Fatalf("8B with per-layer saves should exceed bare weights, got %.1fGB", p.Footprint.TotalGB)
	}
	if len(p.Options) == 0 {
		t.Fatal("expected hardware options for a sized footprint")
	}
}

func TestGradientsForceEagerAndGrow(t *testing.T) {
	m := mdl(8, 2, 32, 4096, 8192)
	base := Size(m, Intervention{Technique: "logit-lens", SaveAllLayers: true})
	grad := Size(m, Intervention{Technique: "attribution", SaveAllLayers: true, Gradients: true})
	if grad.Footprint.Engine != EngineEager {
		t.Fatal("gradient work must take the eager path")
	}
	if grad.Footprint.ActivationsGB <= base.Footprint.ActivationsGB {
		t.Fatalf("gradients should grow activation memory: %.2f !> %.2f",
			grad.Footprint.ActivationsGB, base.Footprint.ActivationsGB)
	}
}

func TestManyPromptsChooseVLLM(t *testing.T) {
	p := Size(mdl(8, 2, 32, 4096, 8192), Intervention{Technique: "generate", Prompts: 128})
	if p.Footprint.Engine != EngineVLLM {
		t.Fatalf("many-prompt sweep should choose vllm, got %s", p.Footprint.Engine)
	}
	if p.Footprint.KVPoolGB <= 0 {
		t.Fatal("vllm path should size a KV pool")
	}
}

func TestAllTokensGrowsResidualStream(t *testing.T) {
	m := mdl(8, 2, 32, 4096, 8192)
	if all, one := residualStreamGB(m, true), residualStreamGB(m, false); all <= one {
		t.Fatalf("all-tokens residual stream %.2f should exceed single-token %.2f", all, one)
	}
}
