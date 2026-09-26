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
	"errors"
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

// IdleWatcher is an optional capability a Spawn may provide to report the
// modeled in-instance idle deadline — the deadline spawn's agent maintains on
// the box itself. The real CLI exposes no such field (which is why this is a
// capability and not part of Spawn), so only the fake implements it: it lets a
// test observe the effect a trace has on the idle timer without a GPU. Same
// optional-capability shape as gateway's enumerator and ReceiptStore.
type IdleWatcher interface {
	IdleDeadline(instanceID string) (time.Time, bool)
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

	// Command runs on the instance after spored setup (--command). The deployed
	// control plane uses it for launch-time handoff (issue #66): the worker is started
	// with `python3 -m worker.batch`, reads the graph the gateway already wrote to the
	// session's bucket prefix, writes back a result reference, and exits — so nothing
	// has to reach the instance. Empty leaves the instance idle for the CLI's
	// `spawn service` tunnel to drive instead.
	Command string

	// ActivePorts are TCP ports whose ESTABLISHED connections spawn's in-instance
	// idle daemon counts as activity (--active-ports). This is the idle bridge's
	// real mechanism: with the worker's port listed, a trace in flight resets the
	// idle timer on the instance itself, so a model-holding-HBM worker is never
	// reaped mid-trace and no per-request control-plane call is needed. See
	// KeepWarm for why the old `spawn extend` mapping was wrong.
	ActivePorts []int
}

// Instance is a spawn-managed instance handle.
//
// The field set mirrors what `spawn list -o json` actually reports (see
// instanceWire) — notably spawn exposes a public *IP*, not a DNS name, and
// reports TTL/idle as durations from launch rather than absolute deadlines.
// Deadlines are derived here instead of parsed.
type Instance struct {
	ID           string
	Name         string
	InstanceType string
	Region       string
	State        string // pending | running | stopping | terminated | hibernated
	PublicIP     string
	PrivateIP    string
	LaunchedAt   time.Time     // when the instance started; backs session age + $-so-far
	TTL          time.Duration // hard-terminate budget, measured from LaunchedAt
	IdleTimeout  time.Duration // idle window before spawn stops/hibernates the box
}

// TTLDeadline is the hard terminate time, derived from the launch time and the
// TTL budget. spawn reports TTL as a duration from first launch, not as an
// absolute timestamp, so there is nothing to parse — it is computed. Zero when
// either input is unknown, which callers must treat as "no deadline to show"
// rather than "expires at the epoch".
func (i Instance) TTLDeadline() time.Time {
	if i.LaunchedAt.IsZero() || i.TTL <= 0 {
		return time.Time{}
	}
	return i.LaunchedAt.Add(i.TTL)
}

// instanceWire mirrors, field for field, the JSON object `spawn list -o json`
// emits (spawn cmd/list.go outputJSON). It exists so the wire contract is
// written down in one place and pinned by a test: the previous hand-inferred
// tags on Instance guessed `public_dns`, `ttl_deadline` and `idle_deadline`,
// none of which spawn emits, so they silently decoded to zero values — an empty
// worker host and a missing TTL — while the fake supplied plausible data and
// kept CI green.
type instanceWire struct {
	InstanceID   string `json:"instance_id"`
	Name         string `json:"name"`
	InstanceType string `json:"instance_type"`
	Region       string `json:"region"`
	State        string `json:"state"`
	PublicIP     string `json:"public_ip"`
	PrivateIP    string `json:"private_ip"`
	LaunchTime   string `json:"launch_time"`  // RFC3339
	TTL          string `json:"ttl"`          // Go duration string, e.g. "8h"; empty when unset
	IdleTimeout  string `json:"idle_timeout"` // ditto
}

// instance converts a wire object to the domain handle. Unparseable durations
// and timestamps degrade to zero rather than failing the whole call: spawn omits
// them for an unmanaged or just-launched instance, and a session listing is more
// useful with a missing column than with an error.
func (w instanceWire) instance() Instance {
	inst := Instance{
		ID:           w.InstanceID,
		Name:         w.Name,
		InstanceType: w.InstanceType,
		Region:       w.Region,
		State:        w.State,
		PublicIP:     w.PublicIP,
		PrivateIP:    w.PrivateIP,
	}
	if w.LaunchTime != "" {
		if t, err := time.Parse(time.RFC3339, w.LaunchTime); err == nil {
			inst.LaunchedAt = t
		}
	}
	if d, err := time.ParseDuration(w.TTL); err == nil {
		inst.TTL = d
	}
	if d, err := time.ParseDuration(w.IdleTimeout); err == nil {
		inst.IdleTimeout = d
	}
	return inst
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
	if len(spec.ActivePorts) > 0 {
		args = append(args, "--active-ports", joinPorts(spec.ActivePorts))
	}
	if spec.Command != "" {
		args = append(args, "--command", spec.Command)
	}
	out, err := s.run.Run(ctx, "spawn", args...)
	if err != nil {
		return Instance{}, fmt.Errorf("spawn launch %s: %w", spec.Name, err)
	}
	var wire instanceWire
	if err := json.Unmarshal(out, &wire); err != nil {
		return Instance{}, fmt.Errorf("spawn launch %s: parse json: %w", spec.Name, err)
	}
	return wire.instance(), nil
}

// ErrInstanceNotFound is returned when spawn manages no instance with the given
// ID — a terminated session, or one launched by another tool.
var ErrInstanceNotFound = errors.New("spore: instance not found")

// Status reports one instance's current state.
//
// It is derived from `spawn list`, deliberately NOT from `spawn status`:
// `spawn status -o json` proxies the *in-instance* spored daemon's own status
// document over SSH/SSM (spawn cmd/status.go passes `--output json` to the
// remote spored and forwards its stdout verbatim). That document has a different
// shape than the EC2-level listing, and reaching it needs a live SSH/SSM path to
// the box — which a cold Lambda in the control plane does not have. `spawn list`
// answers from the EC2 API, so it works from anywhere the control plane runs.
func (s spawnAdapter) Status(ctx context.Context, instanceID string) (Instance, error) {
	all, err := s.list(ctx)
	if err != nil {
		return Instance{}, fmt.Errorf("spawn status %s: %w", instanceID, err)
	}
	for _, inst := range all {
		if inst.ID == instanceID || inst.Name == instanceID {
			return inst, nil
		}
	}
	return Instance{}, fmt.Errorf("spawn status %s: %w", instanceID, ErrInstanceNotFound)
}

// forayNamePrefix scopes List to instances this control plane launched (spawn
// names are session-scoped, set by brain.sessionName as "foray-rung...").
const forayNamePrefix = "foray-"

func (s spawnAdapter) List(ctx context.Context) ([]Instance, error) {
	all, err := s.list(ctx)
	if err != nil {
		return nil, err
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

// list is the unfiltered `spawn list` call shared by List and Status. Status must
// not apply the foray- name filter: it is asked about one known instance by ID,
// and silently reporting "not found" for a correctly-named-by-someone-else box
// would be a worse answer than the truth.
func (s spawnAdapter) list(ctx context.Context) ([]Instance, error) {
	out, err := s.run.Run(ctx, "spawn", "list", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("spawn list: %w", err)
	}
	var wire []instanceWire
	if err := json.Unmarshal(out, &wire); err != nil {
		return nil, fmt.Errorf("spawn list: parse json: %w", err)
	}
	all := make([]Instance, 0, len(wire))
	for _, w := range wire {
		all = append(all, w.instance())
	}
	return all, nil
}

func (s spawnAdapter) Terminate(ctx context.Context, instanceID string) error {
	if _, err := s.run.Run(ctx, "spawn", "terminate", instanceID, "-o", "json"); err != nil {
		return fmt.Errorf("spawn terminate %s: %w", instanceID, err)
	}
	return nil
}

// KeepWarm records that a session is active so a model-holding-HBM worker is not
// reaped between two traces (ARCHITECTURE.md §6.1). The seam is unchanged and
// still load-bearing; what changed is the mechanism underneath it.
//
// This used to shell out to `spawn extend <id> <grace>`, which was the wrong
// lever twice over:
//
//  1. `spawn extend` moves the **hard TTL**, not the idle timer — it rewrites the
//     `spawn:ttl` tag and the termination deadline (spawn cmd/extend.go). It has
//     no effect whatsoever on idle reaping, so it never did the job this seam
//     exists to do.
//  2. Worse, TTL is cumulative from launch, so extending on *every trace* walked
//     the hard terminate deadline outward indefinitely. That silently dissolves
//     the guardrail that makes cost per-session rather than per-hour — a chatty
//     session could outlive its own ceiling.
//
// The correct mechanism is in-instance and set at launch: LaunchSpec.ActivePorts
// puts the worker's port under spawn's idle daemon, which counts ESTABLISHED
// connections on it as activity and resets the idle timer itself
// (spawn pkg/agent countActivePortConnections). A trace in flight therefore keeps
// its own instance alive with no control-plane round-trip, and the idle window
// after the last trace is exactly --idle-timeout.
//
// So there is deliberately no spawn call here. The durable record of activity is
// the per-session last_request_time the gateway writes (gateway.Store.Touch),
// which is what ARCHITECTURE.md §6.1 calls the contract — "the timestamp, not the
// mechanism" — and what a spawn-side consumer reads. Keeping the method on the
// interface keeps that contract explicit and leaves one place to hang a real
// remote poke if spawn ever grows one.
func (s spawnAdapter) KeepWarm(ctx context.Context, instanceID string, lastRequest time.Time) error {
	_, _, _ = ctx, instanceID, lastRequest
	return nil
}

// defaultKeepWarmGrace is the post-trace warmth window: keep the worker alive a
// few minutes after the last request so the next trace doesn't re-stream
// weights. Since re-cold-start is seconds (GDS), the grace-vs-restream tradeoff
// is near-free either way (ARCHITECTURE.md §6.1). It is what callers pass as
// LaunchSpec.IdleGrace, and the fake uses it to model the idle deadline.
const defaultKeepWarmGrace = 5 * time.Minute
