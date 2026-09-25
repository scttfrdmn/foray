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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHTTPWorkerRun: the real worker adapter POSTs the graph to /trace and
// decodes the result reference. Exercised against an httptest server — stdlib
// only, no AWS, no live worker.
func TestHTTPWorkerRun(t *testing.T) {
	var gotPath string
	var gotGraph Graph
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotGraph)
		writeJSON(w, http.StatusOK, TraceResult{SaveRef: "s3://b/sess/act", VizRef: "viz://x"})
	}))
	defer srv.Close()

	res, err := HTTPWorker{}.Run(context.Background(), srv.URL, Graph{Engine: "vllm", Payload: []byte("bytes")})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotPath != "/trace" {
		t.Errorf("path = %q, want /trace", gotPath)
	}
	if gotGraph.Engine != "vllm" || string(gotGraph.Payload) != "bytes" {
		t.Errorf("graph not forwarded verbatim: %+v", gotGraph)
	}
	if res.SaveRef != "s3://b/sess/act" {
		t.Errorf("result = %+v", res)
	}
}

// TestHTTPWorkerRunError: a non-200 from the worker folds the worker's own error
// body into the returned error (the way spore folds a tool's stderr).
func TestHTTPWorkerRunError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "graph rejected: gradients on vllm", http.StatusBadRequest)
	}))
	defer srv.Close()

	_, err := HTTPWorker{}.Run(context.Background(), srv.URL, Graph{})
	if err == nil {
		t.Fatal("want error on non-200")
	}
	if !strings.Contains(err.Error(), "gradients on vllm") {
		t.Errorf("error %q lacks worker diagnostic", err.Error())
	}
}

// workerEndpoint has to cope with both shapes a session's worker URL arrives in:
// a plain base for a directly-reachable worker, and the URL `spawn service`
// reports for a tunneled one, which carries the access token in its query
// (issue #66). Naive concatenation would turn the latter into nonsense.
func TestWorkerEndpoint(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantURL   string
		wantToken string
		wantErr   bool
	}{
		{
			name:    "plain base url",
			in:      "http://10.0.1.7:8000",
			wantURL: "http://10.0.1.7:8000/trace",
		},
		{
			name:    "base url with trailing slash",
			in:      "http://10.0.1.7:8000/",
			wantURL: "http://10.0.1.7:8000/trace",
		},
		{
			name:      "spawn service tunnel url",
			in:        "http://127.0.0.1:54321/?token=abc123",
			wantURL:   "http://127.0.0.1:54321/trace",
			wantToken: "abc123",
		},
		{
			name:      "tunnel url without trailing slash",
			in:        "http://127.0.0.1:54321?token=abc123",
			wantURL:   "http://127.0.0.1:54321/trace",
			wantToken: "abc123",
		},
		{
			name:    "no host",
			in:      "not-a-url",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotURL, gotToken, err := workerEndpoint(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error for %q", tt.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("workerEndpoint(%q): %v", tt.in, err)
			}
			if gotURL != tt.wantURL {
				t.Errorf("url = %q, want %q", gotURL, tt.wantURL)
			}
			if gotToken != tt.wantToken {
				t.Errorf("token = %q, want %q", gotToken, tt.wantToken)
			}
		})
	}
}

// A tunneled worker's token travels as a bearer header, not as a query
// parameter: the endpoint string appears in error messages, and a credential
// should not.
func TestHTTPWorkerSendsBearerToken(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.RawQuery
		gotPath = r.URL.Path
		writeJSON(w, http.StatusOK, TraceResult{SaveRef: "s3://b/x"})
	}))
	defer srv.Close()

	if _, err := (HTTPWorker{}).Run(context.Background(), srv.URL+"/?token=secret-token", Graph{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer secret-token")
	}
	if gotQuery != "" {
		t.Errorf("query = %q, want the token stripped from the URL", gotQuery)
	}
	if gotPath != "/trace" {
		t.Errorf("path = %q, want /trace", gotPath)
	}
}

// No token in the URL means no Authorization header — the direct path is
// unchanged by the tunnel work.
func TestHTTPWorkerNoTokenNoHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		writeJSON(w, http.StatusOK, TraceResult{})
	}))
	defer srv.Close()

	if _, err := (HTTPWorker{}).Run(context.Background(), srv.URL, Graph{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want none", gotAuth)
	}
}

// The token must not leak into the error text a user sees.
func TestHTTPWorkerErrorOmitsToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := (HTTPWorker{}).Run(context.Background(), srv.URL+"/?token=secret-token", Graph{})
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Errorf("error leaks the session token: %q", err.Error())
	}
}
