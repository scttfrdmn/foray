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
	"fmt"
	"io"
	"net/http"
)

// HTTPWorker routes graphs to a live worker's FastAPI endpoint over the VPC
// (§6.7). stdlib net/http only — no SDK, no framework (CLAUDE.md §"stdlib-first").
// The worker deserializes and runs the graph; the gateway only forwards bytes
// and returns the result reference, so no tensors cross this boundary.
type HTTPWorker struct {
	// Client is the HTTP client; nil → http.DefaultClient. Inject one with a
	// timeout in prod (a trace can be slow, but not unbounded).
	Client *http.Client
}

// Run POSTs the serialized graph to workerURL/trace and decodes the result
// reference. The worker is responsible for saving activations in-region and
// returning only references (SaveRef/VizRef), never the tensors.
func (h HTTPWorker) Run(ctx context.Context, workerURL string, g Graph) (TraceResult, error) {
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	body, err := json.Marshal(g)
	if err != nil {
		return TraceResult{}, fmt.Errorf("marshal graph: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, workerURL+"/trace", bytes.NewReader(body))
	if err != nil {
		return TraceResult{}, fmt.Errorf("build worker request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return TraceResult{}, fmt.Errorf("call worker %s: %w", workerURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Surface the worker's own error body (capped) the way the spore adapters
		// fold a tool's stderr into the error — the diagnostic reaches the user.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return TraceResult{}, fmt.Errorf("worker %s: status %d: %s", workerURL, resp.StatusCode, bytes.TrimSpace(msg))
	}
	var res TraceResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return TraceResult{}, fmt.Errorf("decode worker result: %w", err)
	}
	return res, nil
}

// TODO(claude-code): DynamoDBStore implementing Store — the prod session<->
// instance mapping (AWS SDK for Go v2). Get/Put/Touch by session_id; Touch is a
// cheap UpdateItem on last_request so the hot path is one write. It need not
// implement the enumerator capability (a Scan per /healthz would be wasteful);
// /healthz degrades to plain liveness when List is absent. Lands with the deploy
// step (§10 step 9) alongside the rest of the IaC.
