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
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// A manual, opt-in integration test of the launch-time handoff contract against real
// S3 and the real worker (#66). Never run in CI — same precedent as
// `make worker-smoke`.
//
//	FORAY_LIVE_HANDOFF=1 FORAY_DATA_BUCKET=<bucket> AWS_PROFILE=aws \
//	  go test ./internal/gateway/ -run TestLiveHandoffRoundTrip -v
//
// Why this is worth having despite the offline tests: the handoff is a contract between
// two languages passing through a third system. The unit tests prove each side agrees
// with my *model* of the other — the S3 key layout, the base64 envelope, the result
// document. Only a real round trip proves the model is right, and the two previous
// real-AWS validations in this repo each found bugs that offline tests could not
// (PRs #97, #99).
//
// It needs no GPU: the worker runs under FORAY_FAKE=1, which exercises the whole
// handoff path — fetch the graph, route it, write the result — with the nnsight/CUDA
// step short-circuited. The GPU half is worker-smoke's job.
func TestLiveHandoffRoundTrip(t *testing.T) {
	if os.Getenv("FORAY_LIVE_HANDOFF") != "1" {
		t.Skip("manual: set FORAY_LIVE_HANDOFF=1 with FORAY_DATA_BUCKET and AWS credentials")
	}
	bucket := os.Getenv("FORAY_DATA_BUCKET")
	if bucket == "" {
		t.Fatal("FORAY_DATA_BUCKET is required")
	}

	ctx := context.Background()
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		t.Fatalf("load AWS config: %v", err)
	}
	h := NewS3Handoff(s3.NewFromConfig(awsCfg), bucket)

	// A session id that cannot collide with a real one, so a stray object is obviously
	// from this test.
	session := "live-handoff-" + time.Now().UTC().Format("20060102T150405")
	t.Logf("session %s in s3://%s", session, bucket)

	// 1. Nothing handed over yet: an absent result must read as pending, not as an
	//    error. This is the state the control plane sees for the whole trace.
	if _, pending, err := h.GetResult(ctx, session); err != nil || !pending {
		t.Fatalf("before handoff: pending=%v err=%v, want pending with no error", pending, err)
	}

	// 2. Hand over a graph, exactly as /api/approve does.
	graph := Graph{
		Engine:  "eager",
		Payload: []byte(`{"prompt":"The Eiffel Tower is in the city of","saves":["lm_head.output"],"layers":[0,1]}`),
	}
	if err := h.PutGraph(ctx, session, graph); err != nil {
		t.Fatalf("PutGraph: %v", err)
	}
	t.Logf("wrote %s", graphKey(session))

	// 3. Run the real worker against the real bucket. FORAY_FAKE=1 keeps the GPU out of
	//    it; everything about the handoff — reading the envelope, routing, writing the
	//    result — is the production path.
	cmd := exec.CommandContext(ctx, "uv", "run", "--project", "worker", "python", "-m", "worker.batch")
	cmd.Dir = "../.."
	cmd.Env = append(os.Environ(),
		"FORAY_FAKE=1",
		"FORAY_SESSION_ID="+session,
		"FORAY_SAVE_BUCKET="+bucket,
		"FORAY_SAVE_REGION="+awsCfg.Region,
	)
	out, err := cmd.CombinedOutput()
	t.Logf("worker output:\n%s", out)
	if err != nil {
		t.Fatalf("worker.batch failed: %v", err)
	}

	// 4. Collect it, exactly as /api/result does.
	res, pending, err := h.GetResult(ctx, session)
	if err != nil {
		t.Fatalf("GetResult after the worker ran: %v", err)
	}
	if pending {
		t.Fatal("still pending after the worker wrote its result — the two sides disagree about the key")
	}
	if !strings.HasPrefix(res.SaveRef, "s3://") {
		t.Errorf("SaveRef = %q, want an in-region s3:// reference", res.SaveRef)
	}
	if res.VizRef == "" || res.NNSight == "" {
		t.Errorf("result = %+v, want a viz ref and the generated nnsight", res)
	}
	if res.SessionID != session {
		t.Errorf("SessionID = %q, want %q", res.SessionID, session)
	}
	t.Logf("collected: save_ref=%s viz_ref=%s", res.SaveRef, res.VizRef)
}

// The failure path, live: a worker that cannot run must leave a result saying why,
// because an absent result is indistinguishable from a trace still going — the control
// plane would poll forever and the user would see a spinner with no reason.
func TestLiveHandoffRecordsFailure(t *testing.T) {
	if os.Getenv("FORAY_LIVE_HANDOFF") != "1" {
		t.Skip("manual: set FORAY_LIVE_HANDOFF=1 with FORAY_DATA_BUCKET and AWS credentials")
	}
	bucket := os.Getenv("FORAY_DATA_BUCKET")
	if bucket == "" {
		t.Fatal("FORAY_DATA_BUCKET is required")
	}

	ctx := context.Background()
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		t.Fatalf("load AWS config: %v", err)
	}
	h := NewS3Handoff(s3.NewFromConfig(awsCfg), bucket)
	session := "live-handoff-fail-" + time.Now().UTC().Format("20060102T150405")

	// An empty payload is a graph the worker must refuse (graph.GraphError).
	if err := h.PutGraph(ctx, session, Graph{Engine: "eager", Payload: []byte(`{}`)}); err != nil {
		t.Fatalf("PutGraph: %v", err)
	}

	cmd := exec.CommandContext(ctx, "uv", "run", "--project", "worker", "python", "-m", "worker.batch")
	cmd.Dir = "../.."
	cmd.Env = append(os.Environ(),
		"FORAY_FAKE=1",
		"FORAY_SESSION_ID="+session,
		"FORAY_SAVE_BUCKET="+bucket,
		"FORAY_SAVE_REGION="+awsCfg.Region,
	)
	out, _ := cmd.CombinedOutput() // a refused graph exits non-zero, which is correct
	t.Logf("worker output:\n%s", out)

	_, pending, err := h.GetResult(ctx, session)
	if pending {
		t.Fatal("a refused graph left no result — the control plane would poll forever")
	}
	if !errors.Is(err, ErrTraceFailed) {
		t.Fatalf("err = %v, want ErrTraceFailed carrying the worker's reason", err)
	}
	t.Logf("failure surfaced: %v", err)
}
