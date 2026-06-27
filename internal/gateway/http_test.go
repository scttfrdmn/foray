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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleTraceOK: a POSTed graph routes through and returns the worker's
// result reference as JSON.
func TestHandleTraceOK(t *testing.T) {
	g, _ := NewFake()
	srv := httptest.NewServer(g.Handler(nil))
	defer srv.Close()

	body := `{"engine":"eager","payload":"Zm9v"}` // payload base64 "foo"
	resp, err := http.Post(srv.URL+"/sessions/"+FakeSessionID+"/trace", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var res TraceResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.SessionID != FakeSessionID || res.SaveRef == "" {
		t.Errorf("result = %+v, want session + save ref", res)
	}
	// No-automatic-egress guard: the result reference carries no tensor bytes,
	// only s3:// and viz:// references.
	if !strings.HasPrefix(res.SaveRef, "s3://") {
		t.Errorf("save ref %q is not an in-region s3:// reference", res.SaveRef)
	}
}

// TestHandleTraceUnknownSession: an unmapped session is a 404, not a 502.
func TestHandleTraceUnknownSession(t *testing.T) {
	g, _ := NewFake()
	srv := httptest.NewServer(g.Handler(nil))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/sessions/nope/trace", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestHandleTraceBadBody: malformed JSON is a 400.
func TestHandleTraceBadBody(t *testing.T) {
	g, _ := NewFake()
	srv := httptest.NewServer(g.Handler(nil))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/sessions/"+FakeSessionID+"/trace", "application/json", strings.NewReader(`{not json`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestHandleHealthz: liveness plus the idle-bridge metric — after a trace,
// last_seen reflects the recorded last_request_time so an operator can confirm
// the bridge is firing (issue #46).
func TestHandleHealthz(t *testing.T) {
	g, _ := NewFake()
	srv := httptest.NewServer(g.Handler(nil))
	defer srv.Close()

	// Route one trace so last_request_time advances.
	if _, err := g.Route(context.Background(), FakeSessionID, Graph{Payload: []byte("g")}); err != nil {
		t.Fatalf("Route: %v", err)
	}

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var h healthz
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if h.Status != "ok" || h.Sessions != 1 {
		t.Errorf("healthz = %+v, want ok with 1 session", h)
	}
	if !h.LastSeen.Equal(fakeNow) {
		t.Errorf("last_seen = %v, want the routed request time %v", h.LastSeen, fakeNow)
	}
}
