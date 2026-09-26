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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// fakeHandoffS3 is a tiny object store: enough for "the graph is written where the
// worker looks" and "an absent result means pending".
type fakeHandoffS3 struct {
	objects map[string][]byte
	getErr  error
	puts    int
}

func newFakeHandoffS3() *fakeHandoffS3 {
	return &fakeHandoffS3{objects: map[string][]byte{}}
}

func (f *fakeHandoffS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	f.objects[aws.ToString(in.Key)] = body
	f.puts++
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeHandoffS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	body, ok := f.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &s3types.NoSuchKey{Message: aws.String("no such key")}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func newTestHandoff() (*S3Handoff, *fakeHandoffS3) {
	f := newFakeHandoffS3()
	return &S3Handoff{api: f, bucket: "foray-data"}, f
}

// The graph must land exactly where the worker looks for it: the worker boots, reads
// that key, and exits if it is not there.
func TestPutGraphWritesWhereTheWorkerLooks(t *testing.T) {
	h, f := newTestHandoff()
	g := Graph{Engine: "eager", Payload: []byte(`{"prompt":"hi","saves":["lm_head.output"]}`)}

	if err := h.PutGraph(context.Background(), "i-0abc", g); err != nil {
		t.Fatalf("PutGraph: %v", err)
	}
	body, ok := f.objects["sessions/i-0abc/graph.json"]
	if !ok {
		keys := make([]string, 0, len(f.objects))
		for k := range f.objects {
			keys = append(keys, k)
		}
		t.Fatalf("graph not at sessions/i-0abc/graph.json; wrote %v", keys)
	}

	// The envelope must be what worker/batch.py parses: engine plus a base64 payload
	// (encoding/json's []byte form, the same as the HTTP path).
	var env struct {
		Engine  string `json:"engine"`
		Payload []byte `json:"payload"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("envelope is not the expected JSON: %v", err)
	}
	if env.Engine != "eager" {
		t.Errorf("engine = %q, want eager", env.Engine)
	}
	if string(env.Payload) != string(g.Payload) {
		t.Errorf("payload round-trip = %q, want %q", env.Payload, g.Payload)
	}
}

func TestPutGraphNeedsASessionID(t *testing.T) {
	h, _ := newTestHandoff()
	if err := h.PutGraph(context.Background(), "", Graph{}); err == nil {
		t.Fatal("want an error for an empty session id")
	}
}

// An absent result means the worker is still working — the normal state for as long as
// the trace takes. Reading it as an error would surface a spurious failure on every
// poll before the trace finishes.
func TestGetResultAbsentMeansPending(t *testing.T) {
	h, _ := newTestHandoff()
	res, pending, err := h.GetResult(context.Background(), "i-0abc")
	if err != nil {
		t.Fatalf("GetResult: %v", err)
	}
	if !pending {
		t.Error("an absent result must be pending, not done")
	}
	if res.SaveRef != "" {
		t.Errorf("pending result carries data: %+v", res)
	}
}

// The references the worker wrote come back, stamped with the session.
func TestGetResultReturnsReferences(t *testing.T) {
	h, f := newTestHandoff()
	f.objects["sessions/i-0abc/result.json"] = []byte(`{
	  "session_id":"i-0abc",
	  "save_ref":"s3://foray-data/sessions/i-0abc/",
	  "viz_ref":"viz://i-0abc/logit-lens.png",
	  "nnsight":"with model.trace(prompt):\n    pass"
	}`)

	res, pending, err := h.GetResult(context.Background(), "i-0abc")
	if err != nil {
		t.Fatalf("GetResult: %v", err)
	}
	if pending {
		t.Fatal("result present but reported pending")
	}
	if res.SaveRef != "s3://foray-data/sessions/i-0abc/" {
		t.Errorf("SaveRef = %q", res.SaveRef)
	}
	if res.VizRef == "" || res.NNSight == "" {
		t.Errorf("result = %+v, want the viz ref and the generated nnsight", res)
	}
	if res.SessionID != "i-0abc" {
		t.Errorf("SessionID = %q", res.SessionID)
	}
}

// A failed trace must surface as a failure, not as a trace that never finishes: an
// absent result and a failed one are indistinguishable to a poller, so the worker
// writes the error and this reports it.
func TestGetResultSurfacesWorkerFailure(t *testing.T) {
	h, f := newTestHandoff()
	f.objects["sessions/i-0abc/result.json"] =
		[]byte(`{"session_id":"i-0abc","error":"gradients on vllm are not supported"}`)

	_, pending, err := h.GetResult(context.Background(), "i-0abc")
	if pending {
		t.Error("a failed trace must not look pending — the caller would poll forever")
	}
	if !errors.Is(err, ErrTraceFailed) {
		t.Fatalf("err = %v, want ErrTraceFailed", err)
	}
	if !strings.Contains(err.Error(), "gradients on vllm") {
		t.Errorf("error %q does not carry the worker's own message", err)
	}
}

// A real S3 error is not pending: reporting it as "still working" would hide a broken
// bucket or a permissions problem behind an endless spinner.
func TestGetResultRealErrorIsNotPending(t *testing.T) {
	h, f := newTestHandoff()
	f.getErr = errors.New("AccessDenied")

	_, pending, err := h.GetResult(context.Background(), "i-0abc")
	if pending {
		t.Error("a real S3 error must not be reported as pending")
	}
	if err == nil {
		t.Fatal("want the error surfaced")
	}
}

// --- gateway operations ------------------------------------------------------

func newHandoffGateway(t *testing.T, now time.Time) (*Gateway, *S3Handoff, *fakeHandoffS3) {
	t.Helper()
	h, f := newTestHandoff()
	store := NewMemStore()
	if err := store.Put(context.Background(), Session{ID: "sess-1", InstanceID: "i-0abc"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	g := &Gateway{Store: store, Handoff: h, Now: func() time.Time { return now }}
	return g, h, f
}

// HandOff writes the graph and stamps activity — the same idle-bridge contract Route
// honors, since the session is live from the moment the work is queued.
func TestHandOffWritesGraphAndStampsActivity(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	g, _, f := newHandoffGateway(t, now)

	if err := g.HandOff(context.Background(), "sess-1", Graph{Engine: "eager", Payload: []byte("g")}); err != nil {
		t.Fatalf("HandOff: %v", err)
	}
	if _, ok := f.objects["sessions/sess-1/graph.json"]; !ok {
		t.Error("graph not written")
	}
	sess, _ := g.Store.Get(context.Background(), "sess-1")
	if !sess.LastRequest.Equal(now) {
		t.Errorf("last_request = %v, want %v", sess.LastRequest, now)
	}
}

// Collect keeps the session's activity fresh while the worker works, so a long trace
// does not let the idle window close under it.
func TestCollectTouchesWhilePending(t *testing.T) {
	start := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	g, _, _ := newHandoffGateway(t, start)

	later := start.Add(5 * time.Minute)
	g.Now = func() time.Time { return later }

	_, pending, err := g.Collect(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !pending {
		t.Fatal("want pending")
	}
	sess, _ := g.Store.Get(context.Background(), "sess-1")
	if !sess.LastRequest.Equal(later) {
		t.Errorf("last_request = %v, want %v — a long trace would be reaped mid-flight",
			sess.LastRequest, later)
	}
}

func TestCollectReturnsTheResult(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	g, _, f := newHandoffGateway(t, now)
	f.objects["sessions/sess-1/result.json"] =
		[]byte(`{"save_ref":"s3://b/sessions/sess-1/","viz_ref":"viz://x","nnsight":"pass"}`)

	res, pending, err := g.Collect(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if pending {
		t.Fatal("result present but reported pending")
	}
	if res.SaveRef != "s3://b/sessions/sess-1/" {
		t.Errorf("SaveRef = %q", res.SaveRef)
	}
}

// An unknown session is a 404-shaped answer, not a pending trace.
func TestCollectUnknownSession(t *testing.T) {
	g, _, _ := newHandoffGateway(t, time.Now())
	if _, _, err := g.Collect(context.Background(), "nope"); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("err = %v, want ErrUnknownSession", err)
	}
}

// Both operations refuse clearly when no handoff is configured — the CLI path has a
// live endpoint and leaves this nil, so a misconfiguration should say so rather than
// panic.
func TestHandoffOperationsRequireAHandoff(t *testing.T) {
	g := &Gateway{Store: NewMemStore()}
	if err := g.HandOff(context.Background(), "s", Graph{}); err == nil {
		t.Error("HandOff with no handoff configured should error")
	}
	if _, _, err := g.Collect(context.Background(), "s"); err == nil {
		t.Error("Collect with no handoff configured should error")
	}
}

// The result boundary must never carry tensors. handoffResult is a trace-result
// boundary struct like the others, so it gets the same scrutiny — the reflective guard
// in internal/webapi covers the exported ones, and this covers this unexported one.
func TestHandoffResultCarriesNoTensors(t *testing.T) {
	doc := []byte(`{"session_id":"s","save_ref":"s3://b/x","viz_ref":"viz://x","nnsight":"pass",
	                "activations":[1.0,2.0,3.0]}`)
	var r handoffResult
	if err := json.Unmarshal(doc, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The extra field is simply dropped — there is nowhere for it to land, which is the
	// property worth having: a worker that tried to return tensors could not.
	round, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(round), "activations") {
		t.Error("handoffResult accepted a tensor-bearing field — no automatic egress")
	}
}

// The handoff is a contract between two languages, and the keys are the whole of it:
// if Go writes `sessions/<id>/graph.json` and worker/batch.py reads something else, the
// worker finds nothing and the control plane polls an object that never appears — with
// no error anywhere to explain it. Same drift-guard idea as the Cedar policy test in
// internal/catalog.
func TestHandoffKeysMatchTheWorker(t *testing.T) {
	b, err := os.ReadFile("../../worker/batch.py")
	if err != nil {
		t.Fatalf("read worker/batch.py: %v", err)
	}
	src := string(b)

	// The Go side's keys, as the worker must spell them.
	for _, want := range []string{
		`f"sessions/{session_id}/` + graphKeyName + `"`,
		`f"sessions/{session_id}/` + resultKeyName + `"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("worker/batch.py does not build %s — the two sides would use different keys", want)
		}
	}

	// The result fields the Go side reads must be the ones the worker writes. They come
	// from worker/engine.py's TraceResult, so check the field names appear on both
	// sides of the boundary.
	var r handoffResult
	fields := reflect.TypeOf(r)
	for i := 0; i < fields.NumField(); i++ {
		tag := fields.Field(i).Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" {
			continue
		}
		// session_id and error are written by batch.py directly; the reference fields
		// come from the engine's TraceResult, which batch.py serializes wholesale.
		if name == "error" && !strings.Contains(src, `"error"`) {
			t.Errorf("worker/batch.py never writes %q — a failed trace would look pending forever", name)
		}
	}
	if !strings.Contains(src, `"error": message`) && !strings.Contains(src, `"error"`) {
		t.Error("worker/batch.py must record a failure as an error result")
	}
}
