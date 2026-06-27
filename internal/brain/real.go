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
	"time"

	"github.com/scttfrdmn/foray/internal/spore"
)

// SpawnExecutor launches an approved rung via the spawn adapter. It is the real
// Executor seam: Approve (the human's Go) is the only caller, so an instance is
// summoned only after acceptance — never during planning. The ephemerality
// guardrails (TTL + idle) ride along so cost stays per-session, not per-hour.
type SpawnExecutor struct {
	Spawn     spore.Spawn
	Region    string        // optional; empty ⇒ spawn picks by availability/price
	Spot      bool          // Spot for the cheap path
	TTL       time.Duration // hard auto-terminate ceiling
	IdleGrace time.Duration // post-trace warmth before idle reap (forayd bridges this)
}

// DefaultTTL and DefaultIdleGrace bound a session when the caller does not set
// them. The idle grace keeps a model-holding worker warm a few minutes between
// traces; re-cold-start is seconds, so the grace-vs-restream tradeoff is near
// free (ARCHITECTURE.md §6.1).
const (
	DefaultTTL       = 2 * time.Hour
	DefaultIdleGrace = 5 * time.Minute
)

// Execute implements Executor: summon the rung's chosen instance and return the
// spawn instance id as the session id. The gateway (forayd) maps the session to
// this instance and bridges activity into spawn's idle signal.
func (e SpawnExecutor) Execute(ctx context.Context, q Question, r *Rung) (string, error) {
	if r.Chosen.InstanceType == "" {
		return "", fmt.Errorf("execute rung %d: no instance type chosen", r.Index)
	}
	ttl, idle := e.TTL, e.IdleGrace
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if idle <= 0 {
		idle = DefaultIdleGrace
	}
	inst, err := e.Spawn.Launch(ctx, spore.LaunchSpec{
		Name:         sessionName(r),
		InstanceType: r.Chosen.InstanceType,
		Region:       e.Region,
		Spot:         e.Spot,
		TTL:          ttl,
		IdleGrace:    idle,
	})
	if err != nil {
		return "", fmt.Errorf("execute rung %d: %w", r.Index, err)
	}
	return inst.ID, nil
}

// sessionName gives spawn a session-scoped name (spawn requires --name).
func sessionName(r *Rung) string {
	return fmt.Sprintf("foray-rung%d-%s", r.Index, sanitize(r.Model.Name))
}

// sanitize reduces a model id to a spawn-name-safe token.
func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			out = append(out, c)
		case c == '-' || c == '.':
			out = append(out, c)
		default:
			out = append(out, '-')
		}
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return string(out)
}

// Config carries everything NewReal needs to wire the real brain. The Invoker
// and Spawn come from already-configured collaborators (a Bedrock client, the
// spore adapters) so this package stays free of SDK construction and credential
// handling.
type Config struct {
	Invoker    Invoker       // Bedrock Converse (see BedrockInvoker)
	Truffle    spore.Truffle // backs $/session pricing
	Spawn      spore.Spawn   // launches approved rungs
	Principal  Principal     // Cedar principal: budget ceiling, allowed tiers, toggles
	Techniques []string      // worker-supported techniques, constrains planning
	BudgetUSD  float64       // per-question envelope (0 ⇒ defaultQuestionBudgetUSD)
	Region     string        // optional spawn/pricing region scope
	Spot       bool          // Spot launch for the cheap path
}

// NewReal wires the real brain: Bedrock planner + Cedar policy + spawn executor,
// behind the same Planner/Policy/Executor seams the fake uses. It errors only on
// a build-time fault (the embedded policy failing to parse).
func NewReal(cfg Config) (*Brain, error) {
	pol, err := NewCedarPolicy(cfg.Principal)
	if err != nil {
		return nil, err
	}
	var regions []string
	if cfg.Region != "" {
		regions = []string{cfg.Region}
	}
	planner := &AgentCorePlanner{
		Invoker:    cfg.Invoker,
		Pricer:     NewTrufflePricer(cfg.Truffle, regions...),
		Techniques: cfg.Techniques,
		BudgetUSD:  cfg.BudgetUSD,
	}
	exec := SpawnExecutor{Spawn: cfg.Spawn, Region: cfg.Region, Spot: cfg.Spot}
	return &Brain{Plan: planner, Policy: pol, Exec: exec}, nil
}
