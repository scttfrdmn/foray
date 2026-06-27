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

package catalog

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
		want    Source // checked only when wantErr is false
	}{
		// HuggingFace ids
		{name: "hf bare canonical", in: "gpt2",
			want: Source{Kind: KindHF, Ref: "gpt2", Raw: "gpt2"}},
		{name: "hf org/name", in: "meta-llama/Llama-3-8B",
			want: Source{Kind: KindHF, Ref: "meta-llama/Llama-3-8B", Raw: "meta-llama/Llama-3-8B"}},
		{name: "hf with revision", in: "gpt2@main",
			want: Source{Kind: KindHF, Ref: "gpt2", Revision: "main", Raw: "gpt2@main"}},
		{name: "hf org/name with dotted name", in: "EleutherAI/gpt-neo-1.3B",
			want: Source{Kind: KindHF, Ref: "EleutherAI/gpt-neo-1.3B", Raw: "EleutherAI/gpt-neo-1.3B"}},
		{name: "hf trims surrounding space", in: "  gpt2  ",
			want: Source{Kind: KindHF, Ref: "gpt2", Raw: "gpt2"}},

		// s3:// URIs
		{name: "s3 object", in: "s3://my-bucket/llama-8b/model.safetensors",
			want: Source{Kind: KindS3, Bucket: "my-bucket", Key: "llama-8b/model.safetensors", Ref: "llama-8b/model.safetensors", Raw: "s3://my-bucket/llama-8b/model.safetensors"}},
		{name: "s3 prefix dir", in: "s3://my-bucket/llama-8b/",
			want: Source{Kind: KindS3, Bucket: "my-bucket", Key: "llama-8b/", Ref: "llama-8b/", Raw: "s3://my-bucket/llama-8b/"}},

		// upload: refs
		{name: "upload id", in: "upload:a1b2c3d4",
			want: Source{Kind: KindUpload, Ref: "a1b2c3d4", Raw: "upload:a1b2c3d4"}},
		{name: "upload id with dashes/underscores", in: "upload:sess-01_ckpt",
			want: Source{Kind: KindUpload, Ref: "sess-01_ckpt", Raw: "upload:sess-01_ckpt"}},

		// invalids
		{name: "empty", in: "", wantErr: true},
		{name: "whitespace only", in: "   ", wantErr: true},
		{name: "gs scheme", in: "gs://bucket/obj", wantErr: true},
		{name: "http scheme", in: "http://example.com/model", wantErr: true},
		{name: "file scheme", in: "file:///tmp/model", wantErr: true},
		{name: "s3 missing key", in: "s3://my-bucket", wantErr: true},
		{name: "s3 empty key", in: "s3://my-bucket/", wantErr: true},
		{name: "s3 bucket too short", in: "s3://ab/key", wantErr: true},
		{name: "s3 bucket uppercase", in: "s3://My-Bucket/key", wantErr: true},
		{name: "s3 bucket leading dash", in: "s3://-bucket/key", wantErr: true},
		{name: "upload empty id", in: "upload:", wantErr: true},
		{name: "upload illegal char", in: "upload:a/b", wantErr: true},
		{name: "hf too many segments", in: "a/b/c", wantErr: true},
		{name: "hf illegal char", in: "org/name!", wantErr: true},
		{name: "hf empty segment", in: "org/", wantErr: true},
		{name: "hf bad revision", in: "gpt2@bad/rev", wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Parse(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) = %+v, want error", c.in, got)
				}
				if !errors.Is(err, ErrUnsupportedSource) {
					t.Fatalf("Parse(%q) error %v, want it to wrap ErrUnsupportedSource", c.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) unexpected error: %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("Parse(%q) = %+v, want %+v", c.in, got, c.want)
			}
		})
	}
}

// TestSourceString checks the round-trip rendering used by the CLI and receipts.
func TestSourceString(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"gpt2", "gpt2"},
		{"gpt2@main", "gpt2@main"},
		{"meta-llama/Llama-3-8B", "meta-llama/Llama-3-8B"},
		{"s3://my-bucket/llama-8b/", "s3://my-bucket/llama-8b/"},
		{"upload:a1b2c3d4", "upload:a1b2c3d4"},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.in, err)
		}
		if s := got.String(); s != c.want {
			t.Errorf("Parse(%q).String() = %q, want %q", c.in, s, c.want)
		}
	}
}

// TestKindsMatchCedar guards against drift between the catalog Kind literals and
// the modelSource values the Cedar policy permits (internal/brain/policy/
// foray.cedar). If a kind string changes here without changing the policy, a
// source the resolver accepts could be one Cedar can never permit.
func TestKindsMatchCedar(t *testing.T) {
	const policyPath = "../brain/policy/foray.cedar"
	b, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatalf("read %s: %v", policyPath, err)
	}
	policy := string(b)
	for _, k := range []Kind{KindHF, KindS3, KindUpload} {
		// The policy expresses each allowed source as `resource.modelSource == "<k>"`.
		needle := `resource.modelSource == "` + string(k) + `"`
		if !strings.Contains(policy, needle) {
			t.Errorf("foray.cedar does not permit modelSource %q (looked for %q)", k, needle)
		}
	}
}
