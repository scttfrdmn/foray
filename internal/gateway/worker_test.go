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
