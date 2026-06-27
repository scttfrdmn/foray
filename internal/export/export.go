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

// Package export is foray's opt-in download path. The default posture is that
// saved values never leave the region (ARCHITECTURE.md §8); export is the
// deliberate, user-initiated exception — the researcher's own data, in their own
// account, handed back as a time-limited presigned URL when they ask for it.
//
// This is NOT the architecture egressing tensors on every trace (the eDIF
// anti-pattern). It is a person choosing to download their results. Governed by
// a Cedar `export` action so an org can disable it where data must stay in-region.
package export

import (
	"context"
	"time"
)

// Kind selects what gets bundled.
type Kind string

const (
	KindActivations Kind = "activations" // the saved tensors for a session
	KindOutputs     Kind = "outputs"     // generated text / logits / viz data
	KindBundle      Kind = "bundle"      // activations + outputs + nnsight + manifest, zipped
)

// Request is a user-initiated export of one session's saves.
type Request struct {
	SessionID string
	Kind      Kind
	TTL       time.Duration // presigned-URL lifetime; default short (see DefaultTTL)
}

// DefaultTTL keeps download links short-lived.
const DefaultTTL = 15 * time.Minute

// Link is the result handed to the user: a presigned GET they can curl/click.
type Link struct {
	URL       string
	Kind      Kind
	Bytes     int64
	ExpiresAt time.Time
}

// Policy gates export. An org may forbid it where saved values must remain
// in-region; the deny reason surfaces verbatim (same pattern as the brain's
// Cedar gate).
type Policy interface {
	PermitExport(ctx context.Context, sessionID string) (ok bool, reason string)
}

// Presigner produces a presigned S3 GET for an object/bundle in the user's
// bucket. Implemented with the AWS SDK in prod; a fake under FORAY_FAKE=1.
type Presigner interface {
	// Presign returns a time-limited URL for the session's saves of the given
	// kind. For KindBundle it zips saves + outputs + nnsight + manifest first.
	Presign(ctx context.Context, sessionID string, kind Kind, ttl time.Duration) (Link, error)
}

// Exporter ties policy + presigner together.
type Exporter struct {
	Policy    Policy
	Presigner Presigner
}

// Export checks policy, then mints a presigned link. Returns *Denied when policy
// forbids it.
func (e *Exporter) Export(ctx context.Context, req Request) (Link, error) {
	if req.TTL <= 0 {
		req.TTL = DefaultTTL
	}
	if ok, reason := e.Policy.PermitExport(ctx, req.SessionID); !ok {
		return Link{}, &Denied{Reason: reason}
	}
	return e.Presigner.Presign(ctx, req.SessionID, req.Kind, req.TTL)
}

// Denied is returned when Cedar forbids export (e.g. data-residency policy).
type Denied struct{ Reason string }

func (e *Denied) Error() string { return "export denied: " + e.Reason }

// The real Presigner (S3Presigner) lives in s3.go: a single-object presigned GET
// for KindActivations/KindOutputs, and zip-on-demand for KindBundle (saves +
// outputs + nnsight + manifest.json), all against the user's own bucket. The
// Cedar export policy (CedarExportPolicy) lives in internal/brain/cedar.go; the
// FORAY_FAKE=1 stubs live in fake.go.
