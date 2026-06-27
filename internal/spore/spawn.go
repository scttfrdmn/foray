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

package spore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Spawn wraps the spawn binary: launch (< 2 min), TTL auto-termination, idle
// reaping, hibernation. spawn is the data plane's executor — an instance exists
// only while a session runs (ARCHITECTURE.md §6.6, CLAUDE.md invariants). Do not
// reimplement it.
type Spawn interface {
	// Launch summons the right instance for a session and returns its handle.
	Launch(ctx context.Context, spec LaunchSpec) (Instance, error)
	// Status reports an instance's current state and idle/TTL deadlines.
	Status(ctx context.Context, instanceID string) (Instance, error)
	// List enumerates the instances spawn manages — what `foray sessions` shows
	// (age, TTL, $-so-far). The adapter scopes it to foray-launched instances.
	List(ctx context.Context) ([]Instance, error)
	// Terminate destroys the instance (permanent). End of session → $0.
	Terminate(ctx context.Context, instanceID string) error
	// KeepWarm rolls an instance's idle deadline forward to reflect recent
	// session activity. This is the load-bearing idle-bridge seam — see below.
	KeepWarm(ctx context.Context, instanceID string, lastRequest time.Time) error
}

// LaunchSpec is the instance foray asks spawn to summon for one session. It is
// deliberately small: the brain has already chosen the tier (internal/device,
// internal/sizing) and truffle has priced it; spawn just launches it with the
// ephemerality guardrails (TTL + idle) that make the cost per-session, not
// per-hour.
type LaunchSpec struct {
	Name         string        // session-scoped spore name (spawn requires --name)
	InstanceType string        // e.g. "g7e.2xlarge", from internal/device
	Region       string        // empty → spawn picks by availability/price
	Spot         bool          // Spot for the cheap path; on-demand for scarce tiers
	SpotMaxPrice string        // optional ceiling, paired with Spot
	TTL          time.Duration // hard auto-terminate ceiling (--ttl)
	IdleGrace    time.Duration // idle-timeout: short post-trace warmth (--idle-timeout)
}

// Instance is a spawn-managed instance handle.
type Instance struct {
	ID           string    `json:"instance_id"`
	Name         string    `json:"name"`
	InstanceType string    `json:"instance_type"`
	Region       string    `json:"region"`
	State        string    `json:"state"` // pending | running | stopping | terminated | hibernated
	PublicDNS    string    `json:"public_dns"`
	LaunchedAt   time.Time `json:"launch_time"`   // when the instance started; backs session age + $-so-far. TODO(verify-json)
	TTLDeadline  time.Time `json:"ttl_deadline"`  // hard terminate time
	IdleDeadline time.Time `json:"idle_deadline"` // next idle reap time (rolled forward by KeepWarm)
}

// spawnAdapter is the real adapter: it execs the CLI with `-o json` and parses
// the result.
type spawnAdapter struct{ run Runner }

// NewSpawn returns a Spawn backed by the real spawn binary.
func NewSpawn(r Runner) Spawn { return spawnAdapter{run: r} }

func (s spawnAdapter) Launch(ctx context.Context, spec LaunchSpec) (Instance, error) {
	if spec.Name == "" || spec.InstanceType == "" {
		return Instance{}, fmt.Errorf("spawn launch: Name and InstanceType are required")
	}
	args := []string{"launch", "--name", spec.Name, "--instance-type", spec.InstanceType, "-o", "json"}
	if spec.Region != "" {
		args = append(args, "--region", spec.Region)
	}
	if spec.Spot {
		args = append(args, "--spot")
		if spec.SpotMaxPrice != "" {
			args = append(args, "--spot-max-price", spec.SpotMaxPrice)
		}
	}
	if spec.TTL > 0 {
		args = append(args, "--ttl", durStr(spec.TTL))
	}
	if spec.IdleGrace > 0 {
		args = append(args, "--idle-timeout", durStr(spec.IdleGrace))
	}
	out, err := s.run.Run(ctx, "spawn", args...)
	if err != nil {
		return Instance{}, fmt.Errorf("spawn launch %s: %w", spec.Name, err)
	}
	var inst Instance
	if err := json.Unmarshal(out, &inst); err != nil {
		return Instance{}, fmt.Errorf("spawn launch %s: parse json: %w", spec.Name, err)
	}
	return inst, nil
}

func (s spawnAdapter) Status(ctx context.Context, instanceID string) (Instance, error) {
	out, err := s.run.Run(ctx, "spawn", "status", instanceID, "-o", "json")
	if err != nil {
		return Instance{}, fmt.Errorf("spawn status %s: %w", instanceID, err)
	}
	var inst Instance
	if err := json.Unmarshal(out, &inst); err != nil {
		return Instance{}, fmt.Errorf("spawn status %s: parse json: %w", instanceID, err)
	}
	return inst, nil
}

// forayNamePrefix scopes List to instances this control plane launched (spawn
// names are session-scoped, set by brain.sessionName as "foray-rung...").
const forayNamePrefix = "foray-"

func (s spawnAdapter) List(ctx context.Context) ([]Instance, error) {
	out, err := s.run.Run(ctx, "spawn", "list", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("spawn list: %w", err)
	}
	var all []Instance
	if err := json.Unmarshal(out, &all); err != nil {
		return nil, fmt.Errorf("spawn list: parse json: %w", err)
	}
	// Scope to foray sessions: spawn may manage instances from other tools sharing
	// the account. A foray session is named "foray-rung<n>-<model>" (see
	// brain.sessionName); filter on that prefix.
	foray := all[:0]
	for _, inst := range all {
		if strings.HasPrefix(inst.Name, forayNamePrefix) {
			foray = append(foray, inst)
		}
	}
	return foray, nil
}

func (s spawnAdapter) Terminate(ctx context.Context, instanceID string) error {
	if _, err := s.run.Run(ctx, "spawn", "terminate", instanceID, "-o", "json"); err != nil {
		return fmt.Errorf("spawn terminate %s: %w", instanceID, err)
	}
	return nil
}

// KeepWarm bridges per-session request activity into spawn's idle signal. This
// is the seam the gateway (forayd, step 4) will drive: spawn's native idle
// detection (CPU/network/process) reads a model-holding-HBM worker as idle even
// when it is exactly what we want alive between two traces. forayd tracks
// last_request_time per session and calls KeepWarm so spawn extends the idle
// deadline from *request* activity rather than OS heuristics (ARCHITECTURE.md
// §6.1, the one load-bearing new contract).
//
// We map it onto `spawn extend` today: roll the deadline forward from the most
// recent request. The interface — KeepWarm(instanceID, lastRequest) — is the
// stable contract; the exact spored mechanism (extend vs. a heartbeat file vs.
// an active-port probe) is finalized when forayd lands and can change behind
// this method without touching the gateway.
func (s spawnAdapter) KeepWarm(ctx context.Context, instanceID string, lastRequest time.Time) error {
	grace := keepWarmGraceFrom(lastRequest, time.Now())
	if _, err := s.run.Run(ctx, "spawn", "extend", instanceID, durStr(grace), "-o", "json"); err != nil {
		return fmt.Errorf("spawn keep-warm %s: %w", instanceID, err)
	}
	return nil
}

// defaultKeepWarmGrace is the post-trace warmth window: keep the worker alive a
// few minutes after the last request so the next trace doesn't re-stream
// weights. Since re-cold-start is seconds (GDS), the grace-vs-restream tradeoff
// is near-free either way (ARCHITECTURE.md §6.1).
const defaultKeepWarmGrace = 5 * time.Minute

// keepWarmGraceFrom computes how far past now the idle deadline should sit given
// the last request time. A request that just happened earns the full grace; an
// older request earns whatever grace remains, floored at a minimum so a single
// late call still buys a moment of warmth rather than an immediate reap.
func keepWarmGraceFrom(lastRequest, now time.Time) time.Duration {
	const minGrace = 1 * time.Minute
	grace := defaultKeepWarmGrace - now.Sub(lastRequest)
	if grace < minGrace {
		grace = minGrace
	}
	return grace
}
