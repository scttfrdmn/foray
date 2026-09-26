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
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Launch-time handoff: how the *deployed* control plane gets a trace to the worker
// (issue #66).
//
// The CLI reaches the worker through `spawn service`, which holds an SSH forward to a
// loopback-only listener. A Lambda cannot hold a forward, and the alternatives were
// worse: VPC-attaching the Lambda pulls in interface endpoints and NAT that bill
// hourly and break the ~$0 control plane, and opening the worker's port to the
// internet would reverse the loopback-only posture that made the tunnel worth using.
//
// So the deployed path does not reach the worker at all. It cannot need to: a session
// runs **exactly one trace** — each rung launches a fresh instance, routes one graph,
// and the instance is terminated — so the work is fully known before the instance
// exists. The graph is written to the user's own bucket, the instance is launched with
// a command that reads it, and the worker writes its result reference back to the same
// bucket. No inbound path, no open port, no VPC.
//
// What crosses is unchanged: a graph in, a *reference* out. Nothing tensor-shaped
// (CLAUDE.md, no automatic egress).

// Handoff keys under the session's own prefix, alongside its saves.
const (
	graphKeyName  = "graph.json"
	resultKeyName = "result.json"
)

func graphKey(sessionID string) string  { return "sessions/" + sessionID + "/" + graphKeyName }
func resultKey(sessionID string) string { return "sessions/" + sessionID + "/" + resultKeyName }

// Handoff hands a graph to a session's instance and collects the result it writes.
//
// Deliberately not the Worker interface: Worker.Run is request/response against a live
// endpoint, and the point of this path is that there is no endpoint.
type Handoff interface {
	// PutGraph writes the graph where the worker will look for it. Called before the
	// instance is launched, so the work is present when the worker boots.
	PutGraph(ctx context.Context, sessionID string, g Graph) error
	// GetResult reports the worker's result. pending is true while the worker has not
	// written one yet, which is the normal state for as long as the trace takes.
	GetResult(ctx context.Context, sessionID string) (res TraceResult, pending bool, err error)
}

// handoffResult is what the worker writes back. Error is set instead of the references
// when the trace failed, so a failure is reported rather than looking like a trace
// that never finishes.
//
// It carries no tensor-bearing field, and must never grow one — the reflective guard
// in internal/webapi covers the boundary structs for exactly this reason.
type handoffResult struct {
	SessionID string `json:"session_id"`
	SaveRef   string `json:"save_ref"`
	VizRef    string `json:"viz_ref"`
	NNSight   string `json:"nnsight"`
	Error     string `json:"error,omitempty"`
}

// ErrTraceFailed marks a trace the worker reported as failed. Wrapping lets the HTTP
// layer tell "the worker said no" from "the worker is still working".
var ErrTraceFailed = errors.New("gateway: trace failed on the worker")

// s3HandoffAPI is the slice of S3 the handoff uses.
type s3HandoffAPI interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// S3Handoff passes graphs and results through the user's own in-region data bucket.
//
// The same bucket holds the session's saves, and the spawn instance role already has
// GetObject/PutObject scoped to `sessions/*` — so this needs no new permission, no new
// resource, and nothing leaves the region.
type S3Handoff struct {
	api    s3HandoffAPI
	bucket string
}

// NewS3Handoff returns a Handoff over the in-region data bucket.
func NewS3Handoff(c *s3.Client, bucket string) *S3Handoff {
	return &S3Handoff{api: c, bucket: bucket}
}

func (h *S3Handoff) PutGraph(ctx context.Context, sessionID string, g Graph) error {
	if sessionID == "" {
		return errors.New("gateway: PutGraph needs a session id")
	}
	// Graph marshals to exactly the envelope the worker expects —
	// {"engine": string, "payload": base64} — because encoding/json renders []byte as
	// base64. That is the same encoding the HTTP path sends, so worker/graph.py's
	// decoder is reached identically whichever way the work arrived.
	body, err := json.Marshal(g)
	if err != nil {
		return fmt.Errorf("marshal graph for %s: %w", sessionID, err)
	}
	if _, err := h.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(h.bucket),
		Key:         aws.String(graphKey(sessionID)),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("application/json"),
	}); err != nil {
		return fmt.Errorf("hand off graph for %s: %w", sessionID, err)
	}
	return nil
}

func (h *S3Handoff) GetResult(ctx context.Context, sessionID string) (TraceResult, bool, error) {
	out, err := h.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(h.bucket),
		Key:    aws.String(resultKey(sessionID)),
	})
	if err != nil {
		// Absent means the worker has not finished — the normal state for as long as
		// the trace runs, so it is "pending", not an error.
		var noKey *s3types.NoSuchKey
		if errors.As(err, &noKey) || isNotFound(err) {
			return TraceResult{}, true, nil
		}
		return TraceResult{}, false, fmt.Errorf("read result for %s: %w", sessionID, err)
	}
	defer func() { _ = out.Body.Close() }()

	// Cap the read: this is a small JSON document of references, and a result object
	// large enough to matter would mean something is writing tensors where references
	// belong.
	body, err := io.ReadAll(io.LimitReader(out.Body, 1<<20))
	if err != nil {
		return TraceResult{}, false, fmt.Errorf("read result body for %s: %w", sessionID, err)
	}
	var r handoffResult
	if err := json.Unmarshal(body, &r); err != nil {
		return TraceResult{}, false, fmt.Errorf("parse result for %s: %w", sessionID, err)
	}
	if r.Error != "" {
		return TraceResult{}, false, fmt.Errorf("%w: %s", ErrTraceFailed, r.Error)
	}
	return TraceResult{
		SessionID: sessionID,
		SaveRef:   r.SaveRef,
		VizRef:    r.VizRef,
		NNSight:   r.NNSight,
	}, false, nil
}

// isNotFound covers S3's 404 shapes. GetObject on a missing key answers NoSuchKey, but
// a caller without ListBucket can see NotFound instead — and here the two mean the
// same thing: the worker has not written a result yet.
func isNotFound(err error) bool {
	var nf *s3types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var apiErr interface{ ErrorCode() string }
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		}
	}
	return false
}

// --- gateway operations ------------------------------------------------------

// HandOff writes the session's graph and stamps request activity, so the instance has
// its work waiting when it boots.
//
// It is the launch-time counterpart to Route: same graph in, same idle bridge, but no
// live endpoint. Call it *before* launching the instance — a worker that boots and
// finds no graph has nothing to do and would exit.
func (g *Gateway) HandOff(ctx context.Context, sessionID string, graph Graph) error {
	if g.Handoff == nil {
		return errors.New("gateway: no handoff configured (the deployed path needs one)")
	}
	if err := g.Handoff.PutGraph(ctx, sessionID, graph); err != nil {
		return err
	}
	// The same activity stamp Route makes. The trace has not started yet, but the
	// session is live from here, and the idle bridge's contract is the timestamp.
	if err := g.Store.Touch(ctx, sessionID, g.now()); err != nil {
		return fmt.Errorf("record last_request_time for %s: %w", sessionID, err)
	}
	return nil
}

// Collect reports the worker's result, or pending while it is still working.
//
// Polling rather than a callback: the worker is on an instance with no inbound path,
// the control plane is cold Lambdas with nowhere to receive a webhook, and the object
// appearing in the bucket is already the completion signal. Nothing extra to run, and
// nothing that bills while idle.
func (g *Gateway) Collect(ctx context.Context, sessionID string) (TraceResult, bool, error) {
	if g.Handoff == nil {
		return TraceResult{}, false, errors.New("gateway: no handoff configured")
	}
	sess, err := g.Store.Get(ctx, sessionID)
	if err != nil {
		return TraceResult{}, false, fmt.Errorf("collect %s: %w", sessionID, err)
	}
	res, pending, err := g.Handoff.GetResult(ctx, sess.ID)
	if err != nil {
		return TraceResult{}, false, err
	}
	if pending {
		// Still working: keep the session's activity fresh so the idle window does not
		// close under a long trace.
		if err := g.Store.Touch(ctx, sessionID, g.now()); err != nil {
			return TraceResult{}, false, fmt.Errorf("collect %s: %w", sessionID, err)
		}
		return TraceResult{}, true, nil
	}
	return res, false, nil
}
