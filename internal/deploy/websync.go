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
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // content addressing, not security: S3 ETags are MD5
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// webSyncAPI is the S3 slice the SPA upload needs.
type webSyncAPI interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput, opts ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

// DefaultWebDir is where the SPA lives in the repo.
const DefaultWebDir = "web"

// webSync uploads the static SPA to the web bucket — the `aws s3 sync web/` step the
// Makefile did, in-process.
//
// It mirrors rather than merely uploads: a file removed from web/ is removed from the
// bucket, because a stale asset left behind is served by CloudFront and is far more
// confusing than a missing one.
type webSync struct {
	api    webSyncAPI
	bucket string
	dir    string
	// readDir and readFile are seams so the FORAY_FAKE rehearsal and the tests need no
	// web/ directory on disk.
	readDir  func(string) ([]string, error)
	readFile func(string) ([]byte, error)
}

func (w *webSync) kind() string { return "web sync" }
func (w *webSync) name() string { return w.bucket }

func (w *webSync) ensure(ctx context.Context) (Action, error) {
	act := Action{Kind: w.kind(), Name: w.bucket}

	files, err := w.list()
	if err != nil {
		return act, err
	}
	if len(files) == 0 {
		act.Op = OpExists
		act.Detail = "nothing to upload from " + w.dir
		return act, nil
	}

	// Existing objects, so an unchanged file is not re-uploaded and a deleted one is
	// removed. S3's ETag is the content MD5 for a single-part upload, which is what
	// these all are.
	remote, err := w.remoteETags(ctx)
	if err != nil {
		return act, err
	}

	var uploaded int
	keep := map[string]bool{}
	for _, rel := range files {
		key := filepath.ToSlash(rel)
		keep[key] = true

		body, err := w.read(filepath.Join(w.dir, rel))
		if err != nil {
			return act, err
		}
		if etag, ok := remote[key]; ok && etag == contentMD5(body) {
			continue
		}
		if _, err := w.api.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(w.bucket),
			Key:    aws.String(key),
			Body:   bytes.NewReader(body),
			// Without this, S3 serves everything as application/octet-stream and the
			// browser downloads the page instead of rendering it.
			ContentType: aws.String(contentTypeOf(key)),
		}); err != nil {
			return act, fmt.Errorf("upload %s: %w", key, err)
		}
		uploaded++
	}

	removed, err := w.removeStale(ctx, remote, keep)
	if err != nil {
		return act, err
	}

	act.Op = OpExists
	if uploaded > 0 || removed > 0 {
		act.Op = OpCreate
	}
	act.Detail = fmt.Sprintf("%d uploaded, %d unchanged", uploaded, len(files)-uploaded)
	if removed > 0 {
		act.Detail += fmt.Sprintf(", %d stale removed", removed)
	}
	return act, nil
}

// remove is a no-op: the bucket itself is emptied and deleted by the bucket resource,
// so deleting the objects here would duplicate that and report a misleading count.
func (w *webSync) remove(context.Context) (Action, error) {
	return Action{Kind: w.kind(), Name: w.bucket, Op: OpAbsent, Detail: "removed with the bucket"}, nil
}

// removeStale deletes objects the local directory no longer has. A stale asset is
// worse than a missing one: CloudFront will happily serve last month's app.js.
func (w *webSync) removeStale(ctx context.Context, remote map[string]string, keep map[string]bool) (int, error) {
	var stale []string
	for key := range remote {
		if !keep[key] {
			stale = append(stale, key)
		}
	}
	if len(stale) == 0 {
		return 0, nil
	}
	ids := make([]s3types.ObjectIdentifier, 0, len(stale))
	for _, k := range stale {
		ids = append(ids, s3types.ObjectIdentifier{Key: aws.String(k)})
	}
	out, err := w.api.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String(w.bucket),
		Delete: &s3types.Delete{Objects: ids, Quiet: aws.Bool(true)},
	})
	if err != nil {
		return 0, fmt.Errorf("remove stale objects: %w", err)
	}
	// DeleteObjects reports per-key failures in a 200 body, so a "successful" call can
	// leave the stale asset in place — and CloudFront would keep serving it.
	if len(out.Errors) > 0 {
		return 0, fmt.Errorf("remove stale objects: %d key(s) failed, first: %s %s",
			len(out.Errors), aws.ToString(out.Errors[0].Key), aws.ToString(out.Errors[0].Message))
	}
	return len(stale), nil
}

func (w *webSync) remoteETags(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	var token *string
	for {
		page, err := w.api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(w.bucket),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", w.bucket, err)
		}
		for _, o := range page.Contents {
			// S3 quotes ETags.
			out[aws.ToString(o.Key)] = strings.Trim(aws.ToString(o.ETag), `"`)
		}
		if page.IsTruncated == nil || !*page.IsTruncated {
			return out, nil
		}
		token = page.NextContinuationToken
	}
}

// list returns the files to upload, relative to dir, sorted for determinism.
func (w *webSync) list() ([]string, error) {
	if w.readDir != nil {
		return w.readDir(w.dir)
	}
	var out []string
	err := filepath.WalkDir(w.dir, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(w.dir, p)
		if err != nil {
			return err
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			// No web/ directory is not a failure: someone deploying only the API has
			// nothing to upload, and failing here would block that.
			return nil, nil
		}
		return nil, fmt.Errorf("walk %s: %w", w.dir, err)
	}
	return out, nil
}

func (w *webSync) read(p string) ([]byte, error) {
	if w.readFile != nil {
		return w.readFile(p)
	}
	b, err := os.ReadFile(p) //nolint:gosec // path built from a walk of the configured dir
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	return b, nil
}

// contentMD5 is the hex MD5 S3 reports as a single-part object's ETag.
func contentMD5(b []byte) string {
	sum := md5.Sum(b) //nolint:gosec // content addressing, not security
	return hex.EncodeToString(sum[:])
}

// contentTypeOf maps the SPA's file types. Explicit rather than mime.TypeByExtension
// because that consults the OS's mime database, which differs between a laptop and a
// CI container — and a wrong Content-Type makes the browser download the page.
func contentTypeOf(key string) string {
	switch strings.ToLower(path.Ext(key)) {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".json":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".ico":
		return "image/x-icon"
	case ".woff2":
		return "font/woff2"
	case ".map":
		return "application/json"
	default:
		return "application/octet-stream"
	}
}
