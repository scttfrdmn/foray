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
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// SessionOwner resolves "who owns this session" for the Cedar export gate, from
// the presence of the session's saved values in the user's own data bucket.
//
// It replaces an earlier resolver that asked spawn whether the session's instance
// was still alive. That coupled the right to download your results to the GPU
// still running, which is backwards twice over: it blocked export the moment a
// session ended (issue #80 — the run output prints
// "download: foray export <session>", which a terminated instance would have
// denied), and liveness was never what ownership meant anyway.
//
// Object presence is the better signal. It is durable, it survives termination,
// and it is a direct answer to the question export actually asks: are these
// saves, in your bucket, in your account, yours to download? foray is
// single-tenant self-install (CLAUDE.md), so "it is in your bucket" and "it is
// yours" are the same statement — the Cedar gate remains meaningful for the
// residency case (`allowExport == false`), which is the part an org really sets.
type SessionOwner struct {
	api     ownerListAPI
	bucket  string
	subject string
}

// ownerListAPI is the single S3 call ownership needs. Narrow on purpose: a
// resolver that could read objects would be a resolver that could leak them.
type ownerListAPI interface {
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// NewSessionOwner returns a resolver over the in-region data bucket. subject is
// the principal that owns whatever is in it — for a single-tenant install, the
// caller.
func NewSessionOwner(c *s3.Client, bucket, subject string) *SessionOwner {
	return &SessionOwner{api: c, bucket: bucket, subject: subject}
}

// Owner reports the session's owner, or ok=false when the session has no saved
// values in the bucket.
//
// A listing error is reported as "not owned" rather than swallowed as "owned":
// failing closed is the only safe direction for an authorization input, and the
// caller's Exists check surfaces the underlying error to the user separately.
func (o *SessionOwner) Owner(ctx context.Context, sessionID string) (string, bool) {
	ok, err := o.Exists(ctx, sessionID)
	if err != nil || !ok {
		return "", false
	}
	return o.subject, true
}

// OwnerFunc adapts Owner to the resolver signature brain.NewCedarExportPolicy
// takes, binding ctx (that signature has no ctx of its own).
func (o *SessionOwner) OwnerFunc(ctx context.Context) func(string) (string, bool) {
	return func(sessionID string) (string, bool) { return o.Owner(ctx, sessionID) }
}

// Exists reports whether a session has any saved values under its prefix.
//
// Separate from Owner so a caller can tell "nothing was ever saved here" from
// "you may not have this", and say so. Those produce very different next steps
// for the user, and Cedar's deny reason can only speak to the second.
func (o *SessionOwner) Exists(ctx context.Context, sessionID string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	// MaxKeys=1: presence is the whole question, so do not pay to enumerate a
	// session that may hold thousands of activation shards.
	page, err := o.api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(o.bucket),
		Prefix:  aws.String(sessionPrefix(sessionID)),
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return false, fmt.Errorf("look up saves for session %s: %w", sessionID, err)
	}
	return len(page.Contents) > 0, nil
}
