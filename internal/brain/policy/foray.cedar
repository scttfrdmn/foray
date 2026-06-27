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
// Entities (supplied by the Go evaluator as a proposal entity set):
//   Experiment::"<id>"  attrs: { modelSource, instanceTier, estCostUSD, gradients, engine }
//   User::"<sub>"       attrs: { budgetCeilingUSD, allowedTiers: Set<String>, allowLargeSaves }
//
// TODO(claude-code): finalize schema; wire the Cedar Go SDK in CedarPolicy.

// Default deny — nothing launches unless a permit matches.
forbid (principal, action, resource);

// Permit running an experiment within the user's budget and allowed tiers,
// from an approved model source.
permit (
    principal,
    action == Action::"run",
    resource is Experiment
)
when {
    resource.estCostUSD <= principal.budgetCeilingUSD &&
    principal.allowedTiers.contains(resource.instanceTier) &&
    (resource.modelSource == "hf" ||
     resource.modelSource == "s3" ||
     resource.modelSource == "upload")
};

// Hard ceiling: the "large" tier (multi-GPU / 405B-class) requires explicit
// opt-in, independent of budget — it is the easiest way to spend real money.
forbid (
    principal,
    action == Action::"run",
    resource is Experiment
)
when { resource.instanceTier == "large" }
unless { principal.allowedTiers.contains("large") };

// Gradient / full-residual-stream captures force the eager path and large
// activation headroom; gate them so a casual session can't silently size up.
forbid (
    principal,
    action == Action::"run",
    resource is Experiment
)
when { resource.gradients == true }
unless { principal.allowLargeSaves == true };

// Trainium stays dark at the policy layer too until TorchNeuron GAs, regardless
// of any beta env flag in the device registry.
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
