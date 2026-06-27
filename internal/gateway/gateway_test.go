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

package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/scttfrdmn/foray/internal/spore"
)

// spyWorker records the workerURL and graph it was handed so routing can be
// asserted without a live worker.
type spyWorker struct {
	gotURL   string
	gotGraph Graph
	res      TraceResult
	err      error
}

func (w *spyWorker) Run(_ context.Context, url string, g Graph) (TraceResult, error) {
	w.gotURL, w.gotGraph = url, g
	if w.err != nil {
		return TraceResult{}, w.err
	}
	return w.res, nil
}

// newTestGateway wires a fake spawn (with one launched instance) to an in-memory
// store and a spy worker, pinned to a fixed clock. Returns the gateway, the spy,
// and the spawn so tests can read the idle deadline the bridge rolls forward.
func newTestGateway(t *testing.T, w *spyWorker, now time.Time) (*Gateway, spore.Spawn, string) {
	t.Helper()
	sp := spore.NewFake().Spawn
	inst, err := sp.Launch(context.Background(), spore.LaunchSpec{Name: "sess-1", InstanceType: "g7e.2xlarge"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	store := NewMemStore()
	if err := store.Put(context.Background(), Session{ID: "sess-1", InstanceID: inst.ID, WorkerURL: "http://w:8000"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	g := &Gateway{Store: store, Worker: w, Spawn: sp, Now: func() time.Time { return now }}
	return g, sp, inst.ID
}

// TestRouteForwardsGraph: the gateway hands the worker the session's URL and the
// caller's graph, and stamps the result with the session id.
func TestRouteForwardsGraph(t *testing.T) {
	w := &spyWorker{res: TraceResult{SaveRef: "s3://b/x", VizRef: "viz://x"}}
	g, _, _ := newTestGateway(t, w, time.Now())

	graph := Graph{Engine: "eager", Payload: []byte("graph-bytes")}
	res, err := g.Route(context.Background(), "sess-1", graph)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if w.gotURL != "http://w:8000" {
		t.Errorf("worker url = %q, want session's url", w.gotURL)
	}
	if string(w.gotGraph.Payload) != "graph-bytes" || w.gotGraph.Engine != "eager" {
		t.Errorf("graph not forwarded verbatim: %+v", w.gotGraph)
	}
	if res.SessionID != "sess-1" {
		t.Errorf("result session = %q, want sess-1", res.SessionID)
	}
	if res.SaveRef != "s3://b/x" {
		t.Errorf("result = %+v, want worker's reference", res)
	}
}

// TestRouteBridgesIdle is the load-bearing assertion (ARCHITECTURE.md §6.1): a
// trace must roll the worker's idle deadline forward from request activity, so a
// model-holding-HBM worker isn't reaped between two traces. We assert both the
// stored last_request_time and that spawn's idle deadline advanced past the
// request time.
func TestRouteBridgesIdle(t *testing.T) {
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	w := &spyWorker{res: TraceResult{SaveRef: "s3://b/x"}}
	g, sp, instID := newTestGateway(t, w, now)

	before, _ := sp.Status(context.Background(), instID)

	if _, err := g.Route(context.Background(), "sess-1", Graph{Payload: []byte("g")}); err != nil {
		t.Fatalf("Route: %v", err)
	}

	// last_request_time recorded on the session row.
	sess, _ := g.Store.Get(context.Background(), "sess-1")
	if !sess.LastRequest.Equal(now) {
		t.Errorf("last_request = %v, want %v", sess.LastRequest, now)
	}

	// Idle deadline rolled forward past the request time — the worker survives.
	after, _ := sp.Status(context.Background(), instID)
	if !after.IdleDeadline.After(now) {
		t.Errorf("idle deadline %v not after request %v — worker would be reaped", after.IdleDeadline, now)
	}
	if !after.IdleDeadline.After(before.IdleDeadline) {
		t.Errorf("idle deadline did not advance: before %v, after %v", before.IdleDeadline, after.IdleDeadline)
	}
}

// TestRouteUnknownSession: an unmapped session surfaces ErrUnknownSession so the
// HTTP layer can render a 404, and the worker is never called.
func TestRouteUnknownSession(t *testing.T) {
	w := &spyWorker{}
	g, _, _ := newTestGateway(t, w, time.Now())

	_, err := g.Route(context.Background(), "nope", Graph{})
	if !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("err = %v, want ErrUnknownSession", err)
	}
	if w.gotURL != "" {
		t.Errorf("worker called for unknown session: %q", w.gotURL)
	}
}

// TestRouteWorkerErrorWraps: a worker failure is wrapped with the session for
// context and does not masquerade as success.
func TestRouteWorkerErrorWraps(t *testing.T) {
	w := &spyWorker{err: errors.New("boom")}
	g, _, _ := newTestGateway(t, w, time.Now())

	_, err := g.Route(context.Background(), "sess-1", Graph{})
	if err == nil {
		t.Fatal("want error from failing worker")
	}
	if got := err.Error(); !contains(got, "sess-1") || !contains(got, "boom") {
		t.Errorf("error %q lacks session/cause context", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
