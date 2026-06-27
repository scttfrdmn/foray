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

// foray.cedar — policy spine for the brain (internal/brain).
// Mirrors the aws-agentcore-demo AgentCore Gateway Cedar pattern: deny reasons
// surface verbatim to the user. Policy, not code, governs what foray may launch.
//
// Cedar is deny-by-default: a request is permitted only if some `permit` matches
// and no `forbid` matches (a single matching forbid always wins). The Go
// evaluator (internal/brain/cedar.go) reads the `@reason` annotation off the
// matching `forbid` so the message the user sees is authored here, in policy —
// not in Go. The `@id` annotation names each policy for that lookup.
//
// Entities (supplied by the Go evaluator as a proposal entity set):
//   Experiment::"<id>"  attrs: {
//     modelSource:  "hf" | "s3" | "upload"   (mirrors internal/catalog kinds)
//     instanceTier: "slice" | "small" | "mid" | "large"  (internal/device tiers)
//     estCostUSD:   decimal   ($/session estimate, truffle-priced)
//     gradients:    bool      (retains autograd graph / forces eager + headroom)
//     engine:       "eager" | "vllm" | "neuron"
//   }
//   User::"<sub>"  attrs: {
//     budgetCeilingUSD: decimal       (per-session ceiling; distinct from the
//                                       brain's per-question envelope — both hold)
//     allowedTiers:     Set<String>   (which device tiers this principal may use)
//     allowLargeSaves:  bool          (opt-in for gradients / full-residual saves)
//     allowExport:      bool          (optional; absent ⇒ export allowed)
//   }
//
// estCostUSD and budgetCeilingUSD are Cedar decimals; the Go evaluator builds
// them with cedar.NewDecimalFromFloat so the `<=` comparison is exact.

// Permit running an experiment within the user's budget and allowed tiers,
// from an approved model source. One permit; the forbids below carve out the
// hard limits that even a matching permit cannot override.
permit (
    principal,
    action == Action::"run",
    resource is Experiment
)
when {
    resource.estCostUSD.lessThanOrEqual(principal.budgetCeilingUSD) &&
    principal.allowedTiers.contains(resource.instanceTier) &&
    (resource.modelSource == "hf" ||
     resource.modelSource == "s3" ||
     resource.modelSource == "upload")
};

// Over-budget is a hard limit with its own reason. The permit above also checks
// the ceiling, but a permit that simply fails to match yields no message; this
// forbid is what lets the brain surface "over budget" verbatim from policy.
@id("forbid-over-budget")
@reason("estimated $/session exceeds the per-session budget ceiling")
forbid (
    principal,
    action == Action::"run",
    resource is Experiment
)
when { resource.estCostUSD.greaterThan(principal.budgetCeilingUSD) };

// Hard ceiling: the "large" tier (multi-GPU / 405B-class) requires explicit
// opt-in, independent of budget — it is the easiest way to spend real money.
@id("forbid-large-tier")
@reason("instance tier large denied: requires explicit opt-in (allowedTiers must include \"large\")")
forbid (
    principal,
    action == Action::"run",
    resource is Experiment
)
when { resource.instanceTier == "large" }
unless { principal.allowedTiers.contains("large") };

// Gradient / full-residual-stream captures force the eager path and large
// activation headroom; gate them so a casual session can't silently size up.
@id("forbid-gradients")
@reason("gradient / large-save capture denied: requires allowLargeSaves opt-in")
forbid (
    principal,
    action == Action::"run",
    resource is Experiment
)
when { resource.gradients == true }
unless { principal.allowLargeSaves == true };

// Trainium stays dark at the policy layer too until TorchNeuron GAs, regardless
// of any beta env flag in the device registry.
@id("forbid-neuron")
@reason("neuron engine denied: Trainium support is GA-gated and not yet enabled")
forbid (
    principal,
    action == Action::"run",
    resource is Experiment
)
when { resource.engine == "neuron" };

// Export is a user downloading their OWN results — opt-in, their data. Permit it
// by default; an org that requires saved values stay in-region sets
// principal.allowExport == false to deny. (Auto-egress on every trace is never
// expressed here; only this user-initiated action is.)
permit (
    principal,
    action == Action::"export",
    resource is Session
)
when { resource.owner == principal }
unless { principal has allowExport && principal.allowExport == false };

// Export denied when the requester is not the session owner. Named so the Go
// evaluator can surface the reason verbatim.
@id("forbid-export-not-owner")
@reason("export denied: only the session owner may export its saved values")
forbid (
    principal,
    action == Action::"export",
    resource is Session
)
unless { resource.owner == principal };

// Export denied by org data-residency policy (principal.allowExport == false).
@id("forbid-export-residency")
@reason("export denied: organization policy requires saved values stay in-region")
forbid (
    principal,
    action == Action::"export",
    resource is Session
)
when { principal has allowExport && principal.allowExport == false };
