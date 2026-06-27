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

// Package catalog resolves a model *source* — a HuggingFace id, an s3:// URI, or
// a reference to an uploaded object — into a normalized, validated descriptor of
// a HF-format checkpoint (config + safetensors + tokenizer) the worker can load.
// Source is irrelevant to the worker; only format matters (ARCHITECTURE.md §6.5).
//
// This package is pure parsing and validation: AWS-free, no network, no fetching.
// It produces a *validated plan to fetch*, not a fetch — the actual download
// lands with the spore adapters (step 3) and the worker (step 5).
//
// The three accepted kinds (hf/s3/upload) mirror the modelSource values in
// internal/brain/policy/foray.cedar exactly: a source this package accepts is
// precisely one Cedar can permit. Keep them in lockstep — drift breaks the gate.
package catalog

import (
	"errors"
	"fmt"
	"strings"
)

// Kind is the model-source family. The string values are load-bearing: they are
// the modelSource literals in foray.cedar. See TestKindsMatchCedar.
type Kind string

const (
	KindHF     Kind = "hf"     // a HuggingFace repo id, e.g. "gpt2" or "meta-llama/Llama-3-8B"
	KindS3     Kind = "s3"     // an s3:// URI to a checkpoint the user already holds
	KindUpload Kind = "upload" // an opaque "upload:<id>" ref the control plane staged
)

// ErrUnsupportedSource is the sentinel for a source string that is neither an
// s3:// URI, an upload: ref, nor a valid HF id. Callers (brain/CLI) match it
// with errors.Is and surface the wrapped message verbatim, the way Cedar deny
// reasons surface.
var ErrUnsupportedSource = errors.New("unsupported model source")

// Source is the resolved descriptor. Which fields are populated depends on Kind;
// Raw always preserves the original input for receipts and error messages.
type Source struct {
	Kind     Kind   // hf | s3 | upload
	Ref      string // canonical reference (HF repo id, upload id, or s3 key)
	Bucket   string // s3 only: bucket name
	Key      string // s3 only: object key / prefix
	Revision string // hf only: optional @revision (branch / tag / commit)
	Raw      string // original input, preserved for receipts and errors
}

// String renders a Source compactly for CLI output and receipts.
func (s Source) String() string {
	switch s.Kind {
	case KindS3:
		return fmt.Sprintf("s3://%s/%s", s.Bucket, s.Key)
	case KindUpload:
		return "upload:" + s.Ref
	case KindHF:
		if s.Revision != "" {
			return s.Ref + "@" + s.Revision
		}
		return s.Ref
	default:
		return s.Raw
	}
}

// Parse classifies and validates a model-source string. Detection is by prefix:
// "s3://" → S3, "upload:" → upload, otherwise a bare/qualified HF repo id. Any
// other URI scheme (gs://, http://, file://) is rejected rather than guessed at
// as a HF id, so the error is honest.
func Parse(raw string) (Source, error) {
	in := strings.TrimSpace(raw)
	if in == "" {
		return Source{}, fmt.Errorf("%w: empty source", ErrUnsupportedSource)
	}

	switch {
	case strings.HasPrefix(in, "s3://"):
		return parseS3(in)
	case strings.HasPrefix(in, "upload:"):
		return parseUpload(in)
	default:
		return parseHF(in)
	}
}

// parseS3 splits "s3://bucket/key..." and validates the bucket name and a
// non-empty key. The key may itself be a prefix ending in "/" (a checkpoint
// directory), so we don't constrain its shape beyond non-empty.
func parseS3(in string) (Source, error) {
	rest := strings.TrimPrefix(in, "s3://")
	bucket, key, found := strings.Cut(rest, "/")
	if !found || key == "" {
		return Source{}, fmt.Errorf("%w: s3 uri %q is missing an object key", ErrUnsupportedSource, in)
	}
	if err := validateBucket(bucket); err != nil {
		return Source{}, fmt.Errorf("%w: s3 uri %q: %v", ErrUnsupportedSource, in, err)
	}
	return Source{Kind: KindS3, Bucket: bucket, Key: key, Ref: key, Raw: in}, nil
}

// parseUpload validates an "upload:<id>" ref. The id is opaque to this package;
// the control plane mints it when staging an upload to a foray-owned bucket and
// resolves it to a concrete object later (a later AWS step).
func parseUpload(in string) (Source, error) {
	id := strings.TrimPrefix(in, "upload:")
	if id == "" {
		return Source{}, fmt.Errorf("%w: upload ref %q is missing an id", ErrUnsupportedSource, in)
	}
	if !isUploadID(id) {
		return Source{}, fmt.Errorf("%w: upload id %q has illegal characters (allowed: letters, digits, '-', '_')", ErrUnsupportedSource, id)
	}
	return Source{Kind: KindUpload, Ref: id, Raw: in}, nil
}

// parseHF validates a HuggingFace repo id with an optional "@revision" suffix.
// The id is either a bare canonical name ("gpt2") or "org/name"; at most one
// slash, HF's charset on each segment.
func parseHF(in string) (Source, error) {
	id, rev, _ := strings.Cut(in, "@")
	if rev != "" && !isHFSegment(rev) {
		return Source{}, fmt.Errorf("%w: hf revision %q has illegal characters", ErrUnsupportedSource, rev)
	}

	segs := strings.Split(id, "/")
	if len(segs) > 2 {
		return Source{}, fmt.Errorf("%w: hf id %q has too many path segments (want \"name\" or \"org/name\")", ErrUnsupportedSource, id)
	}
	for _, seg := range segs {
		if !isHFSegment(seg) {
			return Source{}, fmt.Errorf("%w: %q is not a valid hf id, s3:// uri, or upload: ref (allowed sources: hf, s3, upload)", ErrUnsupportedSource, in)
		}
	}
	return Source{Kind: KindHF, Ref: id, Revision: rev, Raw: in}, nil
}

// validateBucket applies the subset of AWS S3 bucket-naming rules that catch the
// inputs we'd actually see: 3–63 chars, lowercase letters/digits/'.'/'-', and no
// leading or trailing punctuation. It is deliberately not the full ruleset (no
// IP-address or "xn--" checks) — that belongs to the SDK at fetch time.
func validateBucket(b string) error {
	if len(b) < 3 || len(b) > 63 {
		return fmt.Errorf("bucket %q must be 3-63 characters", b)
	}
	for _, r := range b {
		lowerAlnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if !lowerAlnum && r != '.' && r != '-' {
			return fmt.Errorf("bucket %q has illegal character %q", b, r)
		}
	}
	if c := b[0]; c == '.' || c == '-' {
		return fmt.Errorf("bucket %q must not start with '.' or '-'", b)
	}
	if c := b[len(b)-1]; c == '.' || c == '-' {
		return fmt.Errorf("bucket %q must not end with '.' or '-'", b)
	}
	return nil
}

// isHFSegment reports whether s is a non-empty HuggingFace name segment: letters,
// digits, '-', '_', '.'.
func isHFSegment(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !isAlnum(r) && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

// isUploadID reports whether s is a non-empty opaque upload id: letters, digits,
// '-', '_'.
func isUploadID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !isAlnum(r) && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func isAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}
