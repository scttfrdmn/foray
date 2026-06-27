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
	"strings"
	"testing"

	"github.com/scttfrdmn/foray/internal/device"
	"github.com/scttfrdmn/foray/internal/sizing"
)

// baseRung is a permitted rung; tests mutate one field to probe one policy.
func baseRung() *Rung {
	return &Rung{
		ModelSource: "hf",
		Engine:      sizing.EngineEager,
		EstCostUSD:  0.20,
		Chosen:      sizing.Option{Tier: device.TierSmall, Backend: device.BackendNVIDIA, InstanceType: "g7.xlarge"},
	}
}

// basePrincipal can afford baseRung on a non-large tier, with large-saves opt-in.
func basePrincipal() Principal {
	return Principal{
		Subject:          "alice",
		BudgetCeilingUSD: 5.00,
		AllowedTiers:     []string{"slice", "small", "mid"},
		AllowLargeSaves:  true,
	}
}

// TestCedarRunMatrix is the allow/deny matrix (#44): each row flips one input and
// asserts the decision and — on deny — the verbatim policy reason.
func TestCedarRunMatrix(t *testing.T) {
	tests := []struct {
		name       string
		principal  func() Principal
		rung       func() *Rung
		wantOK     bool
		wantReason string // exact, when deny
	}{
		{
			name:      "permitted: in budget, allowed tier, hf source",
			principal: basePrincipal,
			rung:      baseRung,
			wantOK:    true,
		},
		{
			name:      "over budget",
			principal: basePrincipal,
			rung: func() *Rung {
				r := baseRung()
				r.EstCostUSD = 99.0
				return r
			},
			wantOK:     false,
			wantReason: "estimated $/session exceeds the per-session budget ceiling",
		},
		{
			name:      "large tier without opt-in",
			principal: basePrincipal,
			rung: func() *Rung {
				r := baseRung()
				r.Chosen.Tier = device.TierLarge
				r.Chosen.InstanceType = "p5e.48xlarge"
				return r
			},
			wantOK:     false,
			wantReason: `instance tier large denied: requires explicit opt-in (allowedTiers must include "large")`,
		},
		{
			name: "large tier with opt-in is permitted",
			principal: func() Principal {
				p := basePrincipal()
				p.AllowedTiers = append(p.AllowedTiers, "large")
				return p
			},
			rung: func() *Rung {
				r := baseRung()
				r.Chosen.Tier = device.TierLarge
				r.Chosen.InstanceType = "p5e.48xlarge"
				return r
			},
			wantOK: true,
		},
		{
			name: "gradients without allowLargeSaves",
			principal: func() Principal {
				p := basePrincipal()
				p.AllowLargeSaves = false
				return p
			},
			rung: func() *Rung {
				r := baseRung()
				r.Gradients = true
				return r
			},
			wantOK:     false,
			wantReason: "gradient / large-save capture denied: requires allowLargeSaves opt-in",
		},
		{
			name:      "gradients with allowLargeSaves is permitted",
			principal: basePrincipal,
			rung: func() *Rung {
				r := baseRung()
				r.Gradients = true
				return r
			},
			wantOK: true,
		},
		{
			name:      "neuron engine denied (GA-gated)",
			principal: basePrincipal,
			rung: func() *Rung {
				r := baseRung()
				r.Chosen.Backend = device.BackendNeuron
				return r
			},
			wantOK:     false,
			wantReason: "neuron engine denied: Trainium support is GA-gated and not yet enabled",
		},
		{
			name:      "mid tier is permitted (in allowedTiers)",
			principal: basePrincipal,
			rung: func() *Rung {
				r := baseRung()
				r.Chosen.Tier = device.TierMid
				r.Chosen.InstanceType = "g7e.2xlarge"
				return r
			},
			wantOK: true,
		},
		{
			name: "tier not in allowedTiers falls to the no-permit deny",
			principal: func() Principal {
				p := basePrincipal()
				p.AllowedTiers = []string{"slice"} // small (baseRung's tier) no longer allowed
				return p
			},
			rung:       baseRung, // small tier
			wantOK:     false,
			wantReason: "denied by policy: the experiment is not permitted for this user (check allowed tiers and model source)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pol, err := NewCedarPolicy(tt.principal())
			if err != nil {
				t.Fatal(err)
			}
			ok, reason := pol.Permit(context.Background(), tt.rung())
			if ok != tt.wantOK {
				t.Fatalf("decision = %v, want %v (reason %q)", ok, tt.wantOK, reason)
			}
			if !ok && reason != tt.wantReason {
				t.Fatalf("deny reason = %q, want %q", reason, tt.wantReason)
			}
			if ok && reason != "" {
				t.Fatalf("allow should carry no reason, got %q", reason)
			}
		})
	}
}

// TestCedarExportMatrix covers the export action: owner vs non-owner and the
// org data-residency deny (#45). Deny reasons assert verbatim.
func TestCedarExportMatrix(t *testing.T) {
	owners := func(_ string) (string, bool) { return "alice", true }

	tests := []struct {
		name       string
		principal  Principal
		wantOK     bool
		wantReason string
	}{
		{
			name:      "owner may export",
			principal: Principal{Subject: "alice", AllowExport: true},
			wantOK:    true,
		},
		{
			name:       "non-owner denied",
			principal:  Principal{Subject: "mallory", AllowExport: true},
			wantOK:     false,
			wantReason: "export denied: only the session owner may export its saved values",
		},
		{
			name:       "owner but org forbids export (data residency)",
			principal:  Principal{Subject: "alice", AllowExport: false},
			wantOK:     false,
			wantReason: "export denied: organization policy requires saved values stay in-region",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pol, err := NewCedarExportPolicy(tt.principal, owners)
			if err != nil {
				t.Fatal(err)
			}
			ok, reason := pol.PermitExport(context.Background(), "sess-1")
			if ok != tt.wantOK {
				t.Fatalf("decision = %v, want %v (reason %q)", ok, tt.wantOK, reason)
			}
			if !ok && reason != tt.wantReason {
				t.Fatalf("deny reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

// TestCedarPolicyDriftGuard asserts the policy file's tier and source literals
// match the Go enums, so a rename on either side fails loudly (mirrors the
// catalog package's drift guard).
func TestCedarPolicyDriftGuard(t *testing.T) {
	policy := string(policyCedar)

	// Every device tier string must appear in the policy.
	for _, tier := range []device.Tier{device.TierSlice, device.TierSmall, device.TierMid, device.TierLarge} {
		if !strings.Contains(policy, `"`+tier.String()+`"`) {
			t.Errorf("policy is missing device tier literal %q", tier.String())
		}
	}
	// The catalog model-source kinds the permit allows.
	for _, src := range []string{"hf", "s3", "upload"} {
		if !strings.Contains(policy, `"`+src+`"`) {
			t.Errorf("policy is missing model source literal %q", src)
		}
	}
	// The neuron gate must name the engine literal the evaluator emits.
	if !strings.Contains(policy, `"neuron"`) {
		t.Error("policy is missing the neuron engine gate")
	}
}
