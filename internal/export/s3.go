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
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// s3API is the minimal slice of the S3 client the presigner uses for the bundle
// path — the seam a fake replaces in tests (no AWS). Single-object exports need
// none of this; only presigning.
type s3API interface {
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// s3PresignAPI mints a presigned GET. Prod: *s3.PresignClient; a fake in tests.
type s3PresignAPI interface {
	PresignGetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.PresignOptions)) (*presignedRequest, error)
}

// presignedRequest mirrors the field of v4.PresignedHTTPRequest the presigner
// needs (the URL), so the seam doesn't leak the SDK signer type into tests.
type presignedRequest struct{ URL string }

// DefaultMaxBundleBytes caps a zip-on-demand bundle. Activations can be large and
// a Lambda has bounded time/memory; objects over the running total are skipped,
// logged, and listed in the manifest's `dropped` — never silently truncated. The
// CLI hosts the same presigner with no Lambda ceiling for very large exports.
const DefaultMaxBundleBytes int64 = 2 << 30 // 2 GiB

// S3Presigner is the real Presigner (ARCHITECTURE.md §6.9). It presigns a GET on
// the user's OWN bucket — the data is already theirs, in their account; export
// just hands back a time-limited URL. Single-object kinds presign that object
// directly (no ListBucket, no bucket-wide grant); KindBundle zips the session's
// saves + outputs + nnsight + a manifest to one object, then presigns that.
//
// The bytes never transit this process's response: a single-object export is a
// pure presign, and a bundle is assembled S3-side (read → zip → PutObject) and
// the user is handed a presigned GET on the result. This keeps the
// no-automatic-egress invariant intact (CLAUDE.md): we hand out a URL, not tensors.
type S3Presigner struct {
	api      s3API
	presign  s3PresignAPI
	bucket   string // the user's own data bucket (FORAY_DATA_BUCKET)
	maxBytes int64
	log      *slog.Logger
	// now lets tests pin the bundle-object timestamp; nil → time.Now.
	now func() time.Time
}

// realPresignClient adapts *s3.PresignClient to s3PresignAPI.
type realPresignClient struct{ c *s3.PresignClient }

func (r realPresignClient) PresignGetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.PresignOptions)) (*presignedRequest, error) {
	req, err := r.c.PresignGetObject(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return &presignedRequest{URL: req.URL}, nil
}

// NewS3Presigner wires the real presigner over an S3 client for the user's data
// bucket. Build the client once per cold start and hand it in; this package stays
// free of SDK/credential construction. log may be nil.
func NewS3Presigner(c *s3.Client, bucket string, log *slog.Logger) *S3Presigner {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &S3Presigner{
		api:      c,
		presign:  realPresignClient{c: s3.NewPresignClient(c)},
		bucket:   bucket,
		maxBytes: DefaultMaxBundleBytes,
		log:      log,
		now:      time.Now,
	}
}

// sessionPrefix is the in-region home of a session's saved values.
func sessionPrefix(sessionID string) string { return "sessions/" + sessionID + "/" }

// objectKey is the canonical single object for a non-bundle kind.
func objectKey(sessionID string, kind Kind) string {
	switch kind {
	case KindActivations:
		return sessionPrefix(sessionID) + "activations.safetensors"
	case KindOutputs:
		return sessionPrefix(sessionID) + "outputs.json"
	default:
		return sessionPrefix(sessionID) + string(kind)
	}
}

// Presign implements Presigner. Single-object kinds presign directly; KindBundle
// zips first. The Cedar export gate has already run by the time we are called
// (export.go), so a request that reaches here is permitted.
func (p *S3Presigner) Presign(ctx context.Context, sessionID string, kind Kind, ttl time.Duration) (Link, error) {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if kind == KindBundle {
		return p.presignBundle(ctx, sessionID, ttl)
	}
	return p.presignObject(ctx, sessionID, objectKey(sessionID, kind), kind, ttl)
}

// presignObject mints a single-object presigned GET — least privilege, one key,
// the user's own bucket (issue #53). No ListBucket and no bucket-wide grant.
func (p *S3Presigner) presignObject(ctx context.Context, _ string, key string, kind Kind, ttl time.Duration) (Link, error) {
	req, err := p.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(p.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return Link{}, fmt.Errorf("presign %s: %w", key, err)
	}
	return Link{
		URL:       req.URL,
		Kind:      kind,
		ExpiresAt: p.clock().Add(ttl),
	}, nil
}

// manifest is synthesized at bundle time so the archive is reproducible from
// bucket contents alone.
type manifest struct {
	SessionID string         `json:"session_id"`
	Kind      Kind           `json:"kind"`
	Created   string         `json:"created"`
	Objects   []manifestItem `json:"objects"`
	Dropped   []manifestItem `json:"dropped,omitempty"`
}

type manifestItem struct {
	Key   string `json:"key"`
	Bytes int64  `json:"bytes"`
}

// presignBundle lists the session's objects, zips them (capped, dropping +
// logging oversized objects), uploads the zip to the session's exports/ prefix,
// and presigns a GET on that one object. The final URL is single-object, so #53
// holds for bundles too.
func (p *S3Presigner) presignBundle(ctx context.Context, sessionID string, ttl time.Duration) (Link, error) {
	prefix := sessionPrefix(sessionID)
	keys, err := p.listSession(ctx, prefix)
	if err != nil {
		return Link{}, err
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	man := manifest{SessionID: sessionID, Kind: KindBundle, Created: p.clock().UTC().Format(time.RFC3339)}

	var total int64
	for _, k := range keys {
		// Never recurse into a prior bundle under exports/.
		if strings.HasPrefix(k.key, prefix+"exports/") {
			continue
		}
		if total+k.size > p.maxBytes {
			p.log.Warn("export: object dropped from bundle (size cap)",
				"session", sessionID, "key", k.key, "bytes", k.size, "cap", p.maxBytes)
			man.Dropped = append(man.Dropped, manifestItem{Key: k.key, Bytes: k.size})
			continue
		}
		body, err := p.getBody(ctx, k.key)
		if err != nil {
			return Link{}, err
		}
		w, err := zw.Create(path.Base(k.key))
		if err != nil {
			return Link{}, fmt.Errorf("zip create %s: %w", k.key, err)
		}
		if _, err := w.Write(body); err != nil {
			return Link{}, fmt.Errorf("zip write %s: %w", k.key, err)
		}
		total += k.size
		man.Objects = append(man.Objects, manifestItem{Key: k.key, Bytes: k.size})
	}

	mw, err := zw.Create("manifest.json")
	if err != nil {
		return Link{}, fmt.Errorf("zip create manifest: %w", err)
	}
	if err := json.NewEncoder(mw).Encode(man); err != nil {
		return Link{}, fmt.Errorf("encode manifest: %w", err)
	}
	if err := zw.Close(); err != nil {
		return Link{}, fmt.Errorf("close zip: %w", err)
	}

	bundleKey := fmt.Sprintf("%sexports/bundle-%d.zip", prefix, p.clock().Unix())
	if _, err := p.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(p.bucket),
		Key:         aws.String(bundleKey),
		Body:        bytes.NewReader(buf.Bytes()),
		ContentType: aws.String("application/zip"),
		// Tag the generated zip so the bucket lifecycle rule expires only these
		// bundles, never the user's saved activations (deploy/terraform/storage.tf).
		Tagging: aws.String("foray-export-bundle=true"),
	}); err != nil {
		return Link{}, fmt.Errorf("upload bundle %s: %w", bundleKey, err)
	}

	link, err := p.presignObject(ctx, sessionID, bundleKey, KindBundle, ttl)
	if err != nil {
		return Link{}, err
	}
	link.Bytes = int64(buf.Len())
	return link, nil
}

// listedObject is a key + its size from a ListObjectsV2 page.
type listedObject struct {
	key  string
	size int64
}

// listSession enumerates a session's objects, following pagination. ListObjectsV2
// is scoped to the session prefix — never a bucket-wide listing.
func (p *S3Presigner) listSession(ctx context.Context, prefix string) ([]listedObject, error) {
	var out []listedObject
	var token *string
	for {
		page, err := p.api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(p.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", prefix, err)
		}
		for _, o := range page.Contents {
			out = append(out, listedObject{key: aws.ToString(o.Key), size: aws.ToInt64(o.Size)})
		}
		if page.IsTruncated == nil || !*page.IsTruncated {
			break
		}
		token = page.NextContinuationToken
	}
	return out, nil
}

// getBody reads an object fully into memory (bounded by the per-object size that
// already passed the cap check).
func (p *S3Presigner) getBody(ctx context.Context, key string) ([]byte, error) {
	obj, err := p.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(p.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	defer func() { _ = obj.Body.Close() }()
	body, err := io.ReadAll(obj.Body)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", key, err)
	}
	return body, nil
}

func (p *S3Presigner) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}
