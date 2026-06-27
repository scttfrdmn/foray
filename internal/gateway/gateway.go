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

// Package gateway is forayd — the one genuinely new contract in foray
// (ARCHITECTURE.md §6.1). It does two things and nothing else:
//
//  1. Routes a serialized nnsight intervention graph to the live worker for a
//     session, maintaining the session<->instance mapping. The graph goes in;
//     a *reference* to the saved values comes back (S3 in-region + a viz ref) —
//     never the tensors themselves. No automatic egress (CLAUDE.md invariant).
//
//  2. Bridges per-session last_request_time into spawn's idle signal. spawn's
//     native idle detection (CPU/network/process) reads a model-holding-HBM
//     worker as idle even when it is exactly what we want alive between two
//     traces. forayd records request activity and drives spore.Spawn.KeepWarm
//     so the deadline rolls forward from *requests*, not OS heuristics. This is
//     the load-bearing seam — get it right; everything else is plumbing.
//
// forayd is per-invocation, not a daemon: the gateway logic holds no long-lived
// goroutines, timers, or background loops, so it drops onto a cold Lambda and
// the control plane rests at ~$0 (CLAUDE.md invariant "Control plane rests at
// ~$0"). cmd/forayd wraps Handler() in an http.Server for local/dev; the future
// Lambda adapter wraps the same Handler.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/scttfrdmn/foray/internal/spore"
)

// ErrUnknownSession is returned when a request names a session with no mapping.
// It wraps cleanly so the HTTP layer can render a 404 rather than a 500.
var ErrUnknownSession = errors.New("gateway: unknown session")

// Session maps a foray session to the spawn instance holding its worker. The
// worker holds the model in HBM for the session's lifetime; LastRequest is the
// timestamp the idle bridge feeds to spawn so the worker isn't reaped between
// traces. This is the row persisted to DynamoDB in prod (in-memory in the fake).
type Session struct {
	ID          string    `json:"session_id"`
	InstanceID  string    `json:"instance_id"`  // spawn handle (for KeepWarm/Terminate)
	WorkerURL   string    `json:"worker_url"`   // where the live worker accepts graphs
	LastRequest time.Time `json:"last_request"` // most recent trace; drives the idle bridge
}

// Graph is a serialized nnsight intervention graph plus the routing metadata the
// worker needs. The payload is opaque to forayd — the gateway routes bytes, it
// does not deserialize the graph (that is the worker's job, §6.7). Engine
// selects the serving path (eager LanguageModel vs. VLLM, §3).
type Graph struct {
	Engine  string `json:"engine"`  // "eager" | "vllm"; empty → worker default
	Payload []byte `json:"payload"` // serialized intervention graph, opaque here
}

// TraceResult is what a trace yields: references to saved values, never the
// values themselves. SaveRef is an s3:// URI in-region; VizRef points at the
// rendered viz (only pixels reach the browser). NNSight is the generated code
// returned alongside results so the GUI teaches the library (§5). Honoring the
// no-automatic-egress invariant means this struct must never grow a tensor field
// — export of one's own data is the separate, opt-in path (internal/export).
type TraceResult struct {
	SessionID string `json:"session_id"`
	SaveRef   string `json:"save_ref"` // s3:// in-region; the saved activations/outputs
	VizRef    string `json:"viz_ref"`  // rendered viz reference (pixels, not tensors)
	NNSight   string `json:"nnsight"`  // the generated code that produced this trace
}

// Store maintains the session<->instance mapping. DynamoDB in prod; an in-memory
// map in the fake. Touch records the latest request time without rewriting the
// whole row — the hot path on every trace.
type Store interface {
	Get(ctx context.Context, sessionID string) (Session, error)
	Put(ctx context.Context, s Session) error
	Touch(ctx context.Context, sessionID string, at time.Time) error
}

// Worker routes a serialized graph to the live worker and returns the result
// reference. Real impl POSTs to the worker's FastAPI endpoint over the VPC
// (net/http, stdlib); the fake returns canned data. The worker — not the
// gateway — deserializes and runs the graph (§6.7).
type Worker interface {
	Run(ctx context.Context, workerURL string, g Graph) (TraceResult, error)
}

// Gateway ties the store, the worker, and the idle bridge together. It is built
// per invocation (cold Lambda) and holds no background state.
type Gateway struct {
	Store  Store
	Worker Worker
	Spawn  spore.Spawn // the idle bridge: KeepWarm rolls the deadline forward

	// Now lets tests pin the clock; nil → time.Now. The gateway never sleeps or
	// schedules on it — it only stamps last_request_time.
	Now func() time.Time
}

// now returns the current time, honoring an injected clock for tests.
func (g *Gateway) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// Route is the load-bearing path. For one trace it:
//
//  1. resolves the session -> instance/worker mapping,
//  2. stamps last_request_time and bridges it into spawn's idle signal
//     (KeepWarm) so the model-holding worker survives to the next trace,
//  3. forwards the serialized graph to the live worker,
//  4. returns the worker's result *reference* (no tensors cross this boundary).
//
// The idle bridge runs before the worker call so a slow trace still counts as
// activity from the moment it arrives — the window we must not be reaped in is
// the whole request, not just its tail.
func (g *Gateway) Route(ctx context.Context, sessionID string, graph Graph) (TraceResult, error) {
	sess, err := g.Store.Get(ctx, sessionID)
	if err != nil {
		return TraceResult{}, fmt.Errorf("route %s: %w", sessionID, err)
	}

	at := g.now()
	if err := g.bridgeIdle(ctx, sess, at); err != nil {
		// A failed keep-warm is not fatal to the trace itself — the worker is
		// still up right now — but it risks an early reap, so surface it.
		return TraceResult{}, fmt.Errorf("route %s: idle bridge: %w", sessionID, err)
	}

	res, err := g.Worker.Run(ctx, sess.WorkerURL, graph)
	if err != nil {
		return TraceResult{}, fmt.Errorf("route %s: worker: %w", sessionID, err)
	}
	res.SessionID = sessionID
	return res, nil
}

// bridgeIdle records the request time and feeds it to spawn's idle signal. This
// is the seam ARCHITECTURE.md §6.1 calls load-bearing: spawn consumes *this*
// timestamp via KeepWarm instead of OS idle heuristics, so a worker holding a
// model in HBM between two traces is kept alive a short grace rather than reaped.
func (g *Gateway) bridgeIdle(ctx context.Context, sess Session, at time.Time) error {
	if err := g.Store.Touch(ctx, sess.ID, at); err != nil {
		return fmt.Errorf("record last_request_time: %w", err)
	}
	if err := g.Spawn.KeepWarm(ctx, sess.InstanceID, at); err != nil {
		return fmt.Errorf("keep-warm %s: %w", sess.InstanceID, err)
	}
	return nil
}
