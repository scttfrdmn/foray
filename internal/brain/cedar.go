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
	_ "embed"
	"fmt"

	"github.com/cedar-policy/cedar-go"
	"github.com/cedar-policy/cedar-go/types"

	"github.com/scttfrdmn/foray/internal/device"
	"github.com/scttfrdmn/foray/internal/sizing"
)

// policyCedar is the policy spine, embedded so the binary carries it and the
// drift-guard test reads the same bytes the evaluator does.
//
//go:embed policy/foray.cedar
var policyCedar []byte

// Principal is the user the brain evaluates policy against: the per-session
// budget ceiling Cedar enforces (distinct from the brain's per-question
// envelope — both hold), the tiers they may use, and the opt-in toggles. It maps
// to the Cedar User entity in foray.cedar.
type Principal struct {
	Subject          string   // User::"<sub>"
	BudgetCeilingUSD float64  // per-session ceiling
	AllowedTiers     []string // device tiers permitted ("slice"/"small"/"mid"/"large")
	AllowLargeSaves  bool     // opt-in for gradients / full-residual captures
	AllowExport      bool     // false ⇒ org forbids export (data-residency)
}

// CedarPolicy evaluates foray.cedar per rung. It is the real Policy seam: deny
// reasons come straight from the matching forbid policy's @reason annotation, so
// the message the user sees is authored in policy, not in Go (the
// aws-agentcore-demo Gateway pattern). See ARCHITECTURE.md §6.2, §8.
type CedarPolicy struct {
	set       *cedar.PolicySet
	principal Principal
}

// NewCedarPolicy parses the embedded policy and binds it to a principal. It
// returns an error only if the embedded policy fails to parse — a build-time
// fault, surfaced at construction rather than per-request.
func NewCedarPolicy(p Principal) (*CedarPolicy, error) {
	set, err := cedar.NewPolicySetFromBytes("foray.cedar", policyCedar)
	if err != nil {
		return nil, fmt.Errorf("parse foray.cedar: %w", err)
	}
	return &CedarPolicy{set: set, principal: p}, nil
}

// Permit implements Policy: it authorizes the rung's experiment against the
// bound principal. On deny it returns the verbatim @reason of the matching
// forbid (or a generic message when no permit matched and no forbid carried a
// reason — e.g. an un-allowed tier the permit simply did not cover).
func (c *CedarPolicy) Permit(_ context.Context, r *Rung) (bool, string) {
	const expID = "this"
	entities := types.EntityMap{
		userUID(c.principal.Subject): userEntity(c.principal),
		experimentUID(expID):         experimentEntity(expID, r),
	}
	req := cedar.Request{
		Principal: userUID(c.principal.Subject),
		Action:    cedar.NewEntityUID("Action", "run"),
		Resource:  experimentUID(expID),
	}
	ok, diag := cedar.Authorize(c.set, entities, req)
	if ok == cedar.Allow {
		return true, ""
	}
	return false, c.denyReason(diag)
}

// denyReason pulls the @reason annotation off the matching forbid policy so the
// user sees the policy-authored message verbatim. Falls back to a generic
// message when the denial is "no permit matched" (the permit's own when-clause
// failed, e.g. a tier not in allowedTiers — Cedar reports no forbid for that).
func (c *CedarPolicy) denyReason(diag cedar.Diagnostic) string {
	for _, d := range diag.Reasons {
		if p := c.set.Get(d.PolicyID); p != nil {
			if reason, ok := p.Annotations()["reason"]; ok {
				return string(reason)
			}
		}
	}
	return "denied by policy: the experiment is not permitted for this user (check allowed tiers and model source)"
}

// CedarExportPolicy evaluates the `export` action in foray.cedar. It satisfies
// export.Policy (structurally) so cmd/foray can wire it into an export.Exporter.
// Export is opt-in egress of the user's OWN data; the policy permits it by
// default and an org disables it via principal.allowExport == false. Deny
// reasons surface verbatim, same as the run path. See ARCHITECTURE.md §6.9, §8.
type CedarExportPolicy struct {
	set       *cedar.PolicySet
	principal Principal
	// owners resolves a session id to its owning subject. The session store
	// supplies this in prod; export is permitted only when owner == principal.
	owners func(sessionID string) (owner string, ok bool)
}

// NewCedarExportPolicy parses the embedded policy and binds it to a principal
// and a session-ownership resolver.
func NewCedarExportPolicy(p Principal, owners func(sessionID string) (string, bool)) (*CedarExportPolicy, error) {
	set, err := cedar.NewPolicySetFromBytes("foray.cedar", policyCedar)
	if err != nil {
		return nil, fmt.Errorf("parse foray.cedar: %w", err)
	}
	return &CedarExportPolicy{set: set, principal: p, owners: owners}, nil
}

// PermitExport authorizes a user-initiated export of one session's saves.
func (c *CedarExportPolicy) PermitExport(_ context.Context, sessionID string) (bool, string) {
	owner, ok := c.owners(sessionID)
	if !ok {
		return false, "export denied: unknown session"
	}
	sUID := cedar.NewEntityUID("Session", types.String(sessionID))
	entities := types.EntityMap{
		userUID(c.principal.Subject): userEntity(c.principal),
		sUID: {
			UID: sUID,
			Attributes: cedar.NewRecord(types.RecordMap{
				"owner": userUID(owner),
			}),
		},
	}
	req := cedar.Request{
		Principal: userUID(c.principal.Subject),
		Action:    cedar.NewEntityUID("Action", "export"),
		Resource:  sUID,
	}
	ok2, diag := cedar.Authorize(c.set, entities, req)
	if ok2 == cedar.Allow {
		return true, ""
	}
	return false, exportDenyReason(c.set, diag)
}

// exportDenyReason mirrors denyReason for the export path.
func exportDenyReason(set *cedar.PolicySet, diag cedar.Diagnostic) string {
	for _, d := range diag.Reasons {
		if p := set.Get(d.PolicyID); p != nil {
			if reason, ok := p.Annotations()["reason"]; ok {
				return string(reason)
			}
		}
	}
	return "export denied by policy"
}

// userUID and experimentUID name the Cedar entities consistently across the
// entity set and the request.
func userUID(sub string) types.EntityUID {
	if sub == "" {
		sub = "anonymous"
	}
	return cedar.NewEntityUID("User", types.String(sub))
}

func experimentUID(id string) types.EntityUID {
	return cedar.NewEntityUID("Experiment", types.String(id))
}

// userEntity builds the Cedar User from a Principal.
func userEntity(p Principal) types.Entity {
	tiers := make([]types.Value, 0, len(p.AllowedTiers))
	for _, t := range p.AllowedTiers {
		tiers = append(tiers, types.String(t))
	}
	return types.Entity{
		UID: userUID(p.Subject),
		Attributes: cedar.NewRecord(types.RecordMap{
			"budgetCeilingUSD": mustDecimal(p.BudgetCeilingUSD),
			"allowedTiers":     cedar.NewSet(tiers...),
			"allowLargeSaves":  types.Boolean(p.AllowLargeSaves),
			"allowExport":      types.Boolean(p.AllowExport),
		}),
	}
}

// experimentEntity maps a rung to the Cedar Experiment resource.
func experimentEntity(id string, r *Rung) types.Entity {
	return types.Entity{
		UID: experimentUID(id),
		Attributes: cedar.NewRecord(types.RecordMap{
			"modelSource":  types.String(r.ModelSource),
			"instanceTier": types.String(r.Chosen.Tier.String()),
			"estCostUSD":   mustDecimal(r.EstCostUSD),
			"gradients":    types.Boolean(r.Gradients),
			"engine":       types.String(cedarEngine(r)),
		}),
	}
}

// cedarEngine collapses the device backend + sizing engine into the policy's
// engine literal. A neuron-backed option is "neuron" regardless of the serving
// engine, so the GA gate in foray.cedar can deny it; otherwise the sizing engine
// ("eager"/"vllm") passes through.
func cedarEngine(r *Rung) string {
	if r.Chosen.Backend == device.BackendNeuron {
		return "neuron"
	}
	if r.Engine == "" {
		return string(sizing.EngineEager)
	}
	return string(r.Engine)
}

// mustDecimal converts a USD float to a Cedar decimal. Cedar decimals are
// fixed-point; the conversion cannot fail for the finite, bounded dollar values
// foray prices, so a parse fault here is a programming error.
func mustDecimal(v float64) types.Decimal {
	d, err := cedar.NewDecimalFromFloat(v)
	if err != nil {
		// Bounded, finite dollars only reach here; an error means a bad caller.
		panic(fmt.Sprintf("brain: cannot represent $%v as a Cedar decimal: %v", v, err))
	}
	return d
}
