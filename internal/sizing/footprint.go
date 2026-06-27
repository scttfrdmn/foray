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

// Package sizing turns a model + the shape of the intended intervention into a
// memory footprint and a ranked list of hardware options. It sizes to model +
// intervention, not model alone: the same 8B is ~16 GB for a logit lens with
// light saves but balloons when the user captures the full residual stream
// across every layer and token, or forces a retained autograd graph for
// gradient work. See ARCHITECTURE.md §6.4.
package sizing

import (
	"math"

	"github.com/scttfrdmn/foray/internal/device"
)

// Engine selects the serving path. It is a string so it renders directly in CLI
// output and serializes cleanly.
type Engine string

const (
	// EngineEager is the universal path: eager transformers, arbitrary module
	// access, activation edits, gradients. Required whenever we need autograd or
	// per-layer captures.
	EngineEager Engine = "eager"
	// EngineVLLM is paged-attention, continuous batching: high throughput over
	// many prompts, but no gradients and text-gen only.
	EngineVLLM Engine = "vllm"
)

// Model is the minimal shape needed to size a footprint. ParamsB is in billions.
type Model struct {
	Name      string
	ParamsB   float64 // parameter count, in billions
	BytesPer  int     // bytes per parameter (2 = fp16/bf16, 1 = fp8)
	Layers    int
	HiddenDim int
	CtxTokens int // context window the experiment uses
}

// Intervention is the shape of what the user wants to do — the other half of
// the footprint. Technique is informational; the booleans and Prompts drive
// engine routing and memory.
type Intervention struct {
	Technique     string // "logit-lens", "attribution", "steering", "generate", ...
	SaveAllLayers bool   // capture per-layer hidden states (residual stream)
	Gradients     bool   // retain the autograd graph (attribution, steering training)
	Prompts       int    // number of prompts in a batch/sweep (drives vLLM routing)
}

// vllmPromptThreshold is the batch size above which a gradient-free generation
// sweep is better served by vLLM's continuous batching than the eager path.
const vllmPromptThreshold = 8

// Footprint is the estimated GPU-memory breakdown for a sized experiment.
type Footprint struct {
	Engine        Engine
	WeightsGB     float64
	ActivationsGB float64 // captures + (for gradients) retained graph
	KVPoolGB      float64 // vLLM paged-attention KV cache pool; 0 on the eager path
	TotalGB       float64
}

// Option is one ranked hardware choice for a sized footprint. It carries the
// device tier plus the fields the CLI and page render.
type Option struct {
	Backend        device.Backend
	Tier           device.Tier
	InstanceType   string
	GPU            string
	GPUMemGB       int
	UtilizationPct float64 // footprint / GPUMemGB, as a percentage
}

// Plan is the sizing result: a footprint and the hardware options that fit it,
// cheapest/tightest first.
type Plan struct {
	Footprint Footprint
	Options   []Option
}

// bytesToGB converts raw bytes to gibibytes.
func bytesToGB(b float64) float64 { return b / (1024 * 1024 * 1024) }

// residualStreamGB is the memory to hold hidden states captured by an
// intervention. Capturing across all tokens (the full residual stream) is
// CtxTokens times larger than capturing a single token's stream.
func residualStreamGB(m Model, allTokens bool) float64 {
	tokens := 1
	if allTokens {
		tokens = m.CtxTokens
	}
	elems := float64(m.Layers) * float64(m.HiddenDim) * float64(tokens)
	return bytesToGB(elems * float64(m.BytesPer))
}

// weightsGB is the memory to hold the model parameters.
func weightsGB(m Model) float64 {
	return bytesToGB(m.ParamsB * 1e9 * float64(m.BytesPer))
}

// kvPoolGB sizes the vLLM KV cache pool: 2 (K and V) × layers × hidden × ctx ×
// bytes × prompts.
func kvPoolGB(m Model, prompts int) float64 {
	if prompts < 1 {
		prompts = 1
	}
	elems := 2 * float64(m.Layers) * float64(m.HiddenDim) * float64(m.CtxTokens) * float64(prompts)
	return bytesToGB(elems * float64(m.BytesPer))
}

// routeEngine applies the routing rule (ARCHITECTURE.md §3): gradients or
// per-layer captures force the eager path; a large gradient-free prompt sweep
// chooses vLLM; otherwise eager (the universal default).
func routeEngine(iv Intervention) Engine {
	if iv.Gradients || iv.SaveAllLayers {
		return EngineEager
	}
	if iv.Prompts > vllmPromptThreshold {
		return EngineVLLM
	}
	return EngineEager
}

// Size estimates the footprint for a model + intervention and returns the
// hardware options that fit it, ranked tightest-first.
func Size(m Model, iv Intervention) Plan {
	fp := Footprint{
		Engine:    routeEngine(iv),
		WeightsGB: weightsGB(m),
	}

	switch fp.Engine {
	case EngineVLLM:
		// Throughput path: the KV pool dominates; no autograd graph.
		fp.KVPoolGB = kvPoolGB(m, iv.Prompts)
		// A small working set for the forward activations of the batch.
		fp.ActivationsGB = residualStreamGB(m, false) * float64(maxInt(iv.Prompts, 1))
	default:
		// Eager path: captures live in activation memory. Per-layer saves hold
		// the residual stream; gradients retain the backward graph on top.
		captured := residualStreamGB(m, iv.SaveAllLayers)
		if iv.Gradients {
			// Retaining the autograd graph roughly doubles activation memory
			// (forward saved tensors + backward), plus a gradient buffer the
			// size of the captured stream.
			captured = captured*2 + residualStreamGB(m, iv.SaveAllLayers)
		}
		fp.ActivationsGB = captured
	}

	// Headroom for fragmentation, optimizer/runtime scratch, CUDA context.
	const overheadGB = 2.0
	fp.TotalGB = fp.WeightsGB + fp.ActivationsGB + fp.KVPoolGB + overheadGB

	need := int(math.Ceil(fp.TotalGB))
	var opts []Option
	for _, d := range device.Options(need) {
		util := 0.0
		if d.GPUMemGB > 0 {
			util = fp.TotalGB / float64(d.GPUMemGB) * 100
		}
		opts = append(opts, Option{
			Backend:        d.Backend,
			Tier:           d.Tier,
			InstanceType:   d.InstanceType,
			GPU:            d.GPU,
			GPUMemGB:       d.GPUMemGB,
			UtilizationPct: util,
		})
	}

	return Plan{Footprint: fp, Options: opts}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
