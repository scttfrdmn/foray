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

	"github.com/scttfrdmn/foray/internal/spore"
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

// The keys are the whole of the handoff contract, and two places spell them: this
// package writes and reads them, and internal/brain names them as the task spec's staging
// manifests. If they drift, stage-in silently fetches nothing and the control plane polls
// an object that never appears — with no error anywhere to explain it.
//
// brain does not import this package on purpose (it keeps no gateway dependency), so the
// guard is a source-level one. It used to point at worker/batch.py, which built the keys
// itself; under `spawn task run` the worker only sees staged local files and the S3 layout
// is entirely the control plane's business (#103).
func TestHandoffKeysMatchTheTaskSpec(t *testing.T) {
	b, err := os.ReadFile("../brain/real.go")
	if err != nil {
		t.Fatalf("read internal/brain/real.go: %v", err)
	}
	src := string(b)

	for _, want := range []string{
		`"s3://%s/sessions/%s/` + graphKeyName + `"`,
		`"s3://%s/sessions/%s/` + resultKeyName + `"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("internal/brain/real.go does not stage %s — the task and the gateway "+
				"would use different keys", want)
		}
	}

}

// The other half of the contract, and still genuinely cross-language: the worker must
// record a *failure* as its result rather than leaving one absent. An absent result is
// indistinguishable from a trace still running, and while spawn's completion record now
// catches a container that died outright (Collect's death detection), a trace that raises
// inside a healthy container exits 0 in spawn's eyes — only batch.py can report that.
func TestWorkerRecordsFailuresAsResults(t *testing.T) {
	b, err := os.ReadFile("../../worker/batch.py")
	if err != nil {
		t.Fatalf("read worker/batch.py: %v", err)
	}
	src := string(b)

	var r handoffResult
	fields := reflect.TypeOf(r)
	for i := 0; i < fields.NumField(); i++ {
		name := strings.Split(fields.Field(i).Tag.Get("json"), ",")[0]
		if name != "error" {
			continue
		}
		if !strings.Contains(src, `"`+name+`"`) {
			t.Errorf("worker/batch.py never writes %q — a failed trace would look pending forever", name)
		}
	}
}

// --- death detection ---------------------------------------------------------

// stubTask reports a fixed completion, standing in for `spawn task status`.
type stubTask struct {
	completion spore.TaskCompletion
	err        error
	calls      int
}

func (s *stubTask) Run(context.Context, spore.TaskSpec) (spore.TaskRun, error) {
	return spore.TaskRun{}, nil
}

func (s *stubTask) Status(context.Context, string) (spore.TaskCompletion, error) {
	s.calls++
	return s.completion, s.err
}

// The hole this closes: worker/batch.py writes its own error result for a trace that
// *raises*, but nothing writes anything when the container never reaches Python — a bad
// image, a failed ECR pull, an OOM during model load. Before #103 those polled an object
// that would never appear, forever.
//
// It is race-free because spawn's wrapper stages outputs out BEFORE writing the
// completion record, so a record plus a missing result means the result is not coming.
func TestCollectReportsATaskThatDiedWithoutAResult(t *testing.T) {
	tests := []struct {
		name       string
		completion spore.TaskCompletion
		wantInErr  string
	}{
		{
			name: "the command failed",
			completion: spore.TaskCompletion{
				Complete: true, State: "failed", ExitCode: 137, RetryClass: "app_error",
			},
			wantInErr: "app_error",
		},
		{
			// spawn's own priority order: stage-in failing means nothing ran at all.
			name: "stage-in failed so the graph never arrived",
			completion: spore.TaskCompletion{
				Complete: true, State: "failed", ExitCode: 1, RetryClass: "staging_error",
			},
			wantInErr: "staging_error",
		},
		{
			// The cookbook's hard-won lesson (spore-host/spawn#561): an exit code does not
			// prove the output is real. A task can record completed/0 with its declared
			// output never staged, so a clean record with no result is still a dead end.
			name:       "exit 0 but the result never staged",
			completion: spore.TaskCompletion{Complete: true, State: "completed"},
			wantInErr:  "without writing a result",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, _, _ := newHandoffGateway(t, time.Now())
			g.Task = &stubTask{completion: tt.completion}

			_, pending, err := g.Collect(context.Background(), "sess-1")
			if pending {
				t.Fatal("a finished task with no result must not read as pending — the caller would poll forever")
			}
			if !errors.Is(err, ErrTraceFailed) {
				t.Fatalf("err = %v, want ErrTraceFailed", err)
			}
			if !strings.Contains(err.Error(), tt.wantInErr) {
				t.Errorf("err = %q, should explain %q", err, tt.wantInErr)
			}
		})
	}
}

// A task still running keeps the wait going, and the session's activity keeps being
// stamped. This is the common case on every poll before the trace finishes, so reading it
// as death would fail every single trace.
func TestCollectKeepsWaitingWhileTheTaskRuns(t *testing.T) {
	g, _, _ := newHandoffGateway(t, time.Now())
	task := &stubTask{completion: spore.TaskCompletion{Complete: false}}
	g.Task = task

	_, pending, err := g.Collect(context.Background(), "sess-1")
	if err != nil || !pending {
		t.Fatalf("pending=%v err=%v, want pending with no error", pending, err)
	}
	if task.calls != 1 {
		t.Errorf("Status calls = %d, want 1", task.calls)
	}
}

// A Status call that itself fails must not end the wait: the trace may well be running,
// and one failed `spawn task status` is cheap to repeat. Only a positive completion
// signal is allowed to declare death.
func TestCollectIgnoresAFailingStatusCall(t *testing.T) {
	g, _, _ := newHandoffGateway(t, time.Now())
	g.Task = &stubTask{err: errors.New("spawn: throttled")}

	_, pending, err := g.Collect(context.Background(), "sess-1")
	if err != nil || !pending {
		t.Fatalf("pending=%v err=%v, want the wait to continue", pending, err)
	}
}

// With no task wired, Collect behaves as it did before #103 — polling the artifact alone.
// Worth pinning: the CLI and the deployed path both wire one, and a regression that
// dropped it would silently restore the endless poll.
func TestCollectWithoutATaskStillPolls(t *testing.T) {
	g, _, _ := newHandoffGateway(t, time.Now())
	if _, pending, err := g.Collect(context.Background(), "sess-1"); err != nil || !pending {
		t.Fatalf("pending=%v err=%v, want pending", pending, err)
	}
}
