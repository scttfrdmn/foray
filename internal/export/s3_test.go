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

package export

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// fakeS3 records calls and serves canned object bodies/listings (no AWS).
type fakeS3 struct {
	objects map[string][]byte // key -> body

	listCalls, getCalls, putCalls int
	puts                          map[string][]byte
	getKeys                       []string
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string][]byte{}, puts: map[string][]byte{}}
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.listCalls++
	prefix := aws.ToString(in.Prefix)
	var contents []s3types.Object
	for k, b := range f.objects {
		if strings.HasPrefix(k, prefix) {
			contents = append(contents, s3types.Object{Key: aws.String(k), Size: aws.Int64(int64(len(b)))})
		}
	}
	return &s3.ListObjectsV2Output{Contents: contents, IsTruncated: aws.Bool(false)}, nil
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.getCalls++
	key := aws.ToString(in.Key)
	f.getKeys = append(f.getKeys, key)
	body, ok := f.objects[key]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.putCalls++
	body, _ := io.ReadAll(in.Body)
	f.puts[aws.ToString(in.Key)] = body
	return &s3.PutObjectOutput{}, nil
}

// fakePresign records the key it was asked to sign and returns a canned URL.
type fakePresign struct {
	calls    int
	lastKey  string
	lastTTL  time.Duration
	gotInput *s3.GetObjectInput
}

func (f *fakePresign) PresignGetObject(_ context.Context, in *s3.GetObjectInput, opts ...func(*s3.PresignOptions)) (*presignedRequest, error) {
	f.calls++
	f.lastKey = aws.ToString(in.Key)
	f.gotInput = in
	o := &s3.PresignOptions{}
	for _, fn := range opts {
		fn(o)
	}
	f.lastTTL = o.Expires
	return &presignedRequest{URL: "https://example-bucket.s3.amazonaws.com/" + f.lastKey + "?X-Amz-Signature=fake"}, nil
}

func newTestPresigner(api s3API, presign s3PresignAPI) *S3Presigner {
	return &S3Presigner{
		api:      api,
		presign:  presign,
		bucket:   "my-foray-data",
		maxBytes: DefaultMaxBundleBytes,
		log:      slog.New(slog.DiscardHandler),
		now:      func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	}
}

func TestS3PresignerSingleObject(t *testing.T) {
	fp := &fakePresign{}
	fs := newFakeS3()
	p := newTestPresigner(fs, fp)

	link, err := p.Presign(context.Background(), "sess-1", KindOutputs, 0)
	if err != nil {
		t.Fatalf("Presign: %v", err)
	}
	// Single-object export is a pure presign: exactly one PresignGetObject, no
	// listing, no get/put (no bytes transit this process).
	if fp.calls != 1 {
		t.Fatalf("PresignGetObject calls = %d, want 1", fp.calls)
	}
	if fs.listCalls != 0 || fs.getCalls != 0 || fs.putCalls != 0 {
		t.Fatalf("single-object export touched the data path: list=%d get=%d put=%d", fs.listCalls, fs.getCalls, fs.putCalls)
	}
	if want := "sessions/sess-1/outputs.json"; fp.lastKey != want {
		t.Fatalf("presigned key = %q, want %q", fp.lastKey, want)
	}
	if aws.ToString(fp.gotInput.Bucket) != "my-foray-data" {
		t.Fatalf("presigned against the wrong bucket: %q", aws.ToString(fp.gotInput.Bucket))
	}
	if fp.lastTTL != DefaultTTL {
		t.Fatalf("ttl = %v, want default %v", fp.lastTTL, DefaultTTL)
	}
	if link.URL == "" || link.Kind != KindOutputs {
		t.Fatalf("bad link: %+v", link)
	}
}

func TestS3PresignerBundleZipsAndPresignsSingleKey(t *testing.T) {
	fp := &fakePresign{}
	fs := newFakeS3()
	fs.objects["sessions/sess-2/activations.safetensors"] = bytes.Repeat([]byte("a"), 100)
	fs.objects["sessions/sess-2/outputs.json"] = []byte(`{"logits":[1,2,3]}`)
	fs.objects["sessions/sess-2/nnsight.py"] = []byte("with model.trace(x): pass")
	p := newTestPresigner(fs, fp)

	link, err := p.Presign(context.Background(), "sess-2", KindBundle, 0)
	if err != nil {
		t.Fatalf("Presign bundle: %v", err)
	}
	if fs.listCalls != 1 {
		t.Fatalf("list calls = %d, want 1", fs.listCalls)
	}
	if fs.putCalls != 1 {
		t.Fatalf("put calls = %d, want 1 (the zip)", fs.putCalls)
	}
	// Exactly one object presigned: the bundle, under exports/.
	if fp.calls != 1 || !strings.Contains(fp.lastKey, "/exports/bundle-") {
		t.Fatalf("bundle presign key = %q (calls=%d), want a single exports/bundle key", fp.lastKey, fp.calls)
	}
	if !strings.HasPrefix(fp.lastKey, "sessions/sess-2/exports/") {
		t.Fatalf("bundle key %q not under the session's exports prefix", fp.lastKey)
	}

	// The uploaded zip carries the three objects + a manifest.
	zipped := mustOnlyPut(t, fs)
	names := zipNames(t, zipped)
	for _, want := range []string{"activations.safetensors", "outputs.json", "nnsight.py", "manifest.json"} {
		if !names[want] {
			t.Fatalf("bundle missing %q; has %v", want, names)
		}
	}
	if link.Bytes == 0 {
		t.Fatal("bundle link should report a nonzero byte size")
	}
}

func TestS3PresignerBundleCapDropsOversized(t *testing.T) {
	fp := &fakePresign{}
	fs := newFakeS3()
	fs.objects["sessions/sess-3/small.json"] = []byte("tiny")
	fs.objects["sessions/sess-3/huge.bin"] = bytes.Repeat([]byte("x"), 5000)
	p := newTestPresigner(fs, fp)
	p.maxBytes = 1000 // forces huge.bin to be dropped

	if _, err := p.Presign(context.Background(), "sess-3", KindBundle, 0); err != nil {
		t.Fatalf("Presign: %v", err)
	}
	zipped := mustOnlyPut(t, fs)
	names := zipNames(t, zipped)
	if names["huge.bin"] {
		t.Fatal("oversized object should have been dropped from the bundle")
	}
	if !names["small.json"] {
		t.Fatal("under-cap object should be in the bundle")
	}
	// The manifest must record the drop — never silently truncate.
	man := readManifest(t, zipped)
	if len(man.Dropped) != 1 || man.Dropped[0].Key != "sessions/sess-3/huge.bin" {
		t.Fatalf("dropped list = %+v, want huge.bin recorded", man.Dropped)
	}
}

// --- helpers -----------------------------------------------------------------

func mustOnlyPut(t *testing.T, fs *fakeS3) []byte {
	t.Helper()
	if len(fs.puts) != 1 {
		t.Fatalf("want exactly one PutObject, got %d", len(fs.puts))
	}
	for _, b := range fs.puts {
		return b
	}
	return nil
}

func zipNames(t *testing.T, b []byte) map[string]bool {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	return names
}

func readManifest(t *testing.T, b []byte) manifest {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	for _, f := range zr.File {
		if f.Name != "manifest.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open manifest: %v", err)
		}
		defer func() { _ = rc.Close() }()
		var m manifest
		if err := json.NewDecoder(rc).Decode(&m); err != nil {
			t.Fatalf("decode manifest: %v", err)
		}
		return m
	}
	t.Fatal("no manifest.json in bundle")
	return manifest{}
}
