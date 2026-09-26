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

package deploy

import (
	"context"
	"strings"
	"testing"
)

// newTestSync builds a sync over a fake bucket with a canned file set.
func newTestSync(t *testing.T, files map[string]string) (*webSync, *fakeS3) {
	t.Helper()
	f := newFakeS3()
	f.buckets["foray-web"] = &fakeBucket{
		region:     "us-west-2",
		objects:    map[string]struct{}{},
		tags:       map[string]string{},
		objectData: map[string]fakeObject{},
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	return &webSync{
		api:      f,
		bucket:   "foray-web",
		dir:      "web",
		readDir:  func(string) ([]string, error) { return names, nil },
		readFile: func(p string) ([]byte, error) { return []byte(files[strings.TrimPrefix(p, "web/")]), nil },
	}, f
}

// Without a Content-Type, S3 serves everything as application/octet-stream and the
// browser downloads the page instead of rendering it.
func TestWebSyncSetsContentTypes(t *testing.T) {
	sync, f := newTestSync(t, map[string]string{
		"index.html": "<html>",
		"app.js":     "const x=1",
		"styles.css": "body{}",
	})
	if _, err := sync.ensure(context.Background()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	want := map[string]string{
		"index.html": "text/html; charset=utf-8",
		"app.js":     "text/javascript; charset=utf-8",
		"styles.css": "text/css; charset=utf-8",
	}
	for key, ct := range want {
		got := f.buckets["foray-web"].objectData[key].contentType
		if got != ct {
			t.Errorf("%s content type = %q, want %q", key, got, ct)
		}
	}
}

func TestContentTypeOf(t *testing.T) {
	tests := []struct{ key, want string }{
		{"index.html", "text/html; charset=utf-8"},
		{"a/b.HTML", "text/html; charset=utf-8"},
		{"app.js", "text/javascript; charset=utf-8"},
		{"s.css", "text/css; charset=utf-8"},
		{"d.json", "application/json"},
		{"i.svg", "image/svg+xml"},
		{"f.woff2", "font/woff2"},
		{"noext", "application/octet-stream"},
	}
	for _, tt := range tests {
		if got := contentTypeOf(tt.key); got != tt.want {
			t.Errorf("contentTypeOf(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}

// An unchanged file must not be re-uploaded: S3's ETag is the content MD5 for these
// single-part uploads, so the comparison is exact.
func TestWebSyncSkipsUnchangedFiles(t *testing.T) {
	sync, f := newTestSync(t, map[string]string{"index.html": "<html>", "app.js": "x"})

	if _, err := sync.ensure(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if f.puts != 2 {
		t.Fatalf("first sync uploaded %d objects, want 2", f.puts)
	}
	if _, err := sync.ensure(context.Background()); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if f.puts != 2 {
		t.Errorf("second sync uploaded again (%d total puts), want no re-upload of unchanged files", f.puts)
	}
}

// A changed file must be re-uploaded, or CloudFront keeps serving the old one.
func TestWebSyncUploadsChangedFiles(t *testing.T) {
	files := map[string]string{"app.js": "v1"}
	sync, f := newTestSync(t, files)
	if _, err := sync.ensure(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	files["app.js"] = "v2"
	if _, err := sync.ensure(context.Background()); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if f.puts != 2 {
		t.Errorf("puts = %d, want 2 (the changed file re-uploaded)", f.puts)
	}
}

// A file removed from web/ must be removed from the bucket. A stale asset is worse
// than a missing one: CloudFront will happily serve last month's app.js.
func TestWebSyncRemovesStaleObjects(t *testing.T) {
	files := map[string]string{"index.html": "<html>", "old.js": "gone soon"}
	sync, f := newTestSync(t, files)
	if _, err := sync.ensure(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if _, ok := f.buckets["foray-web"].objects["old.js"]; !ok {
		t.Fatal("old.js was not uploaded")
	}

	delete(files, "old.js")
	sync.readDir = func(string) ([]string, error) { return []string{"index.html"}, nil }

	act, err := sync.ensure(context.Background())
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if _, ok := f.buckets["foray-web"].objects["old.js"]; ok {
		t.Error("old.js survived — CloudFront would keep serving a stale asset")
	}
	if !strings.Contains(act.Detail, "stale removed") {
		t.Errorf("detail = %q, want it to report the removal", act.Detail)
	}
}

// DeleteObjects reports per-key failures inside a 200 body, so a "successful" call can
// leave the stale asset in place.
func TestWebSyncSurfacesStaleDeleteFailures(t *testing.T) {
	files := map[string]string{"index.html": "<html>", "old.js": "x"}
	sync, f := newTestSync(t, files)
	if _, err := sync.ensure(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	sync.readDir = func(string) ([]string, error) { return []string{"index.html"}, nil }
	f.deleteObjectErrors["foray-web"] = true

	if _, err := sync.ensure(context.Background()); err == nil {
		t.Fatal("want the per-key delete failure surfaced, not a silent success")
	}
}

// A missing web/ directory is not a failure: deploying only the API is a legitimate
// thing to do, and failing here would block it.
func TestWebSyncWithNoFilesIsNotAnError(t *testing.T) {
	sync, f := newTestSync(t, nil)
	sync.readDir = func(string) ([]string, error) { return nil, nil }

	act, err := sync.ensure(context.Background())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if act.Op != OpExists {
		t.Errorf("op = %s, want exists", act.Op)
	}
	if f.puts != 0 {
		t.Errorf("uploaded %d objects from an empty directory", f.puts)
	}
}

// The sync has no lifecycle of its own — its objects go with the bucket, so removing
// them here would duplicate that and report a misleading count.
func TestWebSyncRemoveIsDeferredToTheBucket(t *testing.T) {
	sync, _ := newTestSync(t, map[string]string{"index.html": "x"})
	act, err := sync.remove(context.Background())
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if act.Op != OpAbsent {
		t.Errorf("op = %s, want absent", act.Op)
	}
	if !strings.Contains(act.Detail, "bucket") {
		t.Errorf("detail = %q, want it to say the bucket handles it", act.Detail)
	}
}
