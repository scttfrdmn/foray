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

package webapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scttfrdmn/foray/internal/brain"
)

// newTestServer stands up the API over the offline fake deps — the same wiring
// the dev server's rehearsal mode and make demo-fake use. Zero AWS.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(Handler(NewFakeDeps(), nil))
	t.Cleanup(srv.Close)
	return srv
}

// postJSON POSTs v as JSON and decodes the response into out (when non-nil).
func postJSON(t *testing.T, url string, v any, out any) *http.Response {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
	}
	return resp
}

// propose returns the planned ladder and its first rung; nothing launches.
func TestProposeReturnsLadder(t *testing.T) {
	srv := newTestServer(t)

	var got proposeResp
	resp := postJSON(t, srv.URL+"/api/propose", proposeReq{Question: "why does it store France as Paris?"}, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got.Clarify != "" {
		t.Fatalf("clarify = %q, want a ladder", got.Clarify)
	}
	if got.Ladder == nil || len(got.Ladder.Rungs) != 2 {
		t.Fatalf("ladder = %+v, want the 2-rung GPT-2→8B ladder", got.Ladder)
	}
	if got.Proposal == nil || got.Proposal.Index != 0 {
		t.Fatalf("proposal = %+v, want rung 0", got.Proposal)
	}
	// The cost meter (#52) binds to a real, positive estimate, not a canned sum.
	if got.Proposal.EstCostUSD <= 0 {
		t.Errorf("rung 0 est cost = %v, want > 0", got.Proposal.EstCostUSD)
	}
	// The escape hatch: the generated nnsight rides along (§5).
	if !strings.Contains(got.Proposal.NNSight, "model.trace") {
		t.Errorf("nnsight = %q, want the generated trace code", got.Proposal.NNSight)
	}
	// The question is the load-bearing invariant — it survives planning verbatim.
	if got.Ladder.Question.Text != "why does it store France as Paris?" {
		t.Errorf("question = %q, want it carried verbatim", got.Ladder.Question.Text)
	}
}

// An empty question is the wrong first move: 400, not a launch.
func TestProposeEmptyQuestion(t *testing.T) {
	srv := newTestServer(t)
	resp := postJSON(t, srv.URL+"/api/propose", proposeReq{Question: "  "}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// Malformed JSON is a 400, not a 500.
func TestProposeBadBody(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Post(srv.URL+"/api/propose", "application/json", strings.NewReader(`{not json`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// The full result-gated loop over HTTP: propose → approve rung 0 (launch, trace,
// interpret, assess) → climb recommendation + a fresh next proposal → approve
// rung 1 → stop, no next. Mirrors the CLI's runLoop and make demo-fake.
func TestApproveWalksTheLadder(t *testing.T) {
	srv := newTestServer(t)

	var planned proposeResp
	postJSON(t, srv.URL+"/api/propose", proposeReq{Question: "why does it store France as Paris?"}, &planned)

	// Rung 0: the cheap GPT-2 rung shows the effect; the brain recommends climbing.
	var r0 approveResp
	resp := postJSON(t, srv.URL+"/api/approve", approveReq{Ladder: planned.Ladder, RungIndex: 0}, &r0)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve rung 0 status = %d, want 200", resp.StatusCode)
	}
	if r0.SessionID == "" {
		t.Error("approve rung 0: no session id — nothing launched")
	}
	if r0.Result == nil || r0.Result.Finding == "" {
		t.Fatalf("approve rung 0 result = %+v, want a finding", r0.Result)
	}
	if r0.Recommendation.Decision != string(brain.Climb) {
		t.Errorf("rung 0 decision = %q, want climb", r0.Recommendation.Decision)
	}
	if r0.NextProposal == nil || r0.NextProposal.Index != 1 {
		t.Fatalf("next proposal = %+v, want rung 1 awaiting a fresh Go", r0.NextProposal)
	}
	if r0.SpentUSD <= 0 || r0.BudgetUSD <= 0 {
		t.Errorf("spent/budget = %v/%v, want positive (the receipt + meter)", r0.SpentUSD, r0.BudgetUSD)
	}

	// Rung 1: climb on a FRESH approve (never auto). The carried ladder advanced.
	var r1 approveResp
	resp = postJSON(t, srv.URL+"/api/approve", approveReq{Ladder: r0.Ladder, RungIndex: 1}, &r1)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve rung 1 status = %d, want 200", resp.StatusCode)
	}
	if r1.Recommendation.Decision != string(brain.Stop) {
		t.Errorf("rung 1 decision = %q, want stop (top of the ladder)", r1.Recommendation.Decision)
	}
	if r1.NextProposal != nil {
		t.Errorf("next proposal = %+v, want nil at the top of the ladder", r1.NextProposal)
	}
	if r1.SpentUSD <= r0.SpentUSD {
		t.Errorf("spend did not accumulate: %v then %v", r0.SpentUSD, r1.SpentUSD)
	}
}

// No-automatic-egress guard: no approve response — at any rung — may carry tensor
// bytes. Only s3:// save refs and viz refs cross this boundary (CLAUDE.md
// invariant; mirrors the gateway's guard). We assert on the raw JSON so a future
// field addition can't smuggle tensors past the typed view.
func TestApproveNoTensorEgress(t *testing.T) {
	srv := newTestServer(t)

	var planned proposeResp
	postJSON(t, srv.URL+"/api/propose", proposeReq{Question: "why?"}, &planned)

	body, _ := json.Marshal(approveReq{Ladder: planned.Ladder, RungIndex: 0})
	resp, err := http.Post(srv.URL+"/api/approve", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	res, ok := raw["result"]
	if !ok {
		t.Fatal("approve response has no result")
	}
	var rv resultView
	if err := json.Unmarshal(res, &rv); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !strings.HasPrefix(rv.SaveRef, "s3://") {
		t.Errorf("save ref %q is not an in-region s3:// reference", rv.SaveRef)
	}
	// The result view has exactly the reference-bearing fields; a tensor field
	// would show up here as an unexpected key.
	var fields map[string]any
	if err := json.Unmarshal(res, &fields); err != nil {
		t.Fatalf("decode result fields: %v", err)
	}
	for k := range fields {
		switch k {
		case "rung", "finding", "effectPresent", "saveRef", "vizRef":
		default:
			t.Errorf("unexpected result field %q — guard against tensor egress", k)
		}
	}
}

// A missing/invalid ladder is a 400, not a launch.
func TestApproveInvalidLadder(t *testing.T) {
	srv := newTestServer(t)
	resp := postJSON(t, srv.URL+"/api/approve", approveReq{Ladder: nil, RungIndex: 0}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// Export is the opt-in egress path: the fake exporter returns a presigned stub.
func TestExportFake(t *testing.T) {
	srv := newTestServer(t)
	var got exportResp
	resp := postJSON(t, srv.URL+"/api/export", exportReq{SessionID: "sess-fake000001", Kind: "bundle"}, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(got.URL, "presigned") || got.Kind != "bundle" {
		t.Errorf("export = %+v, want a presigned bundle link", got)
	}
}

// Healthz is liveness.
func TestHealthz(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}
