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
	"net/url"
	"strings"
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
	traceURL, token, err := workerEndpoint(workerURL)
	if err != nil {
		return TraceResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, traceURL, bytes.NewReader(body))
	if err != nil {
		return TraceResult{}, fmt.Errorf("build worker request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		// Present the credential as a header rather than echoing the query
		// parameter: tokens in URLs end up in logs and error strings, and this one
		// appears in both (the error paths below include the endpoint).
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return TraceResult{}, fmt.Errorf("call worker %s: %w", traceURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Surface the worker's own error body (capped) the way the spore adapters
		// fold a tool's stderr into the error — the diagnostic reaches the user.
		// traceURL is token-free, so the credential stays out of error text.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return TraceResult{}, fmt.Errorf("worker %s: status %d: %s", traceURL, resp.StatusCode, bytes.TrimSpace(msg))
	}
	var res TraceResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return TraceResult{}, fmt.Errorf("decode worker result: %w", err)
	}
	return res, nil
}

// workerEndpoint splits a session's worker base URL into the /trace endpoint and
// the access token, if it carries one.
//
// Two shapes arrive here. A plain base — "http://10.0.1.7:8000" — is the direct
// form. A tunneled worker reached through `spawn service` (issue #66) arrives as
// spawn reports it, with the credential in the query:
//
//	http://127.0.0.1:54321/?token=abc123
//
// Concatenating "/trace" onto the second form would produce a nonsense URL, so
// the query is lifted off and the path is joined properly. The returned URL is
// always token-free, which is what makes it safe to name in error messages.
func workerEndpoint(workerURL string) (traceURL, token string, err error) {
	u, err := url.Parse(workerURL)
	if err != nil {
		return "", "", fmt.Errorf("parse worker url: %w", err)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("worker url %q has no host", workerURL)
	}
	token = u.Query().Get("token")
	u.RawQuery = ""
	u.Fragment = ""
	u.Path = strings.TrimSuffix(u.Path, "/") + "/trace"
	return u.String(), token, nil
}

// The prod session<->instance mapping (DynamoDBStore implementing Store) lives
// in dynamo.go: Get/Put/Touch by session_id, Touch a single UpdateItem on
// last_request so the hot path is one write. It deliberately omits the
// enumerator capability (a Scan per /healthz would be wasteful), so /healthz
// degrades to plain liveness when List is absent.
