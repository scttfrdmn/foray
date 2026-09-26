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
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// s3DeployAPI is the slice of S3 this package uses.
type s3DeployAPI interface {
	HeadBucket(ctx context.Context, in *s3.HeadBucketInput, opts ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	CreateBucket(ctx context.Context, in *s3.CreateBucketInput, opts ...func(*s3.Options)) (*s3.CreateBucketOutput, error)
	DeleteBucket(ctx context.Context, in *s3.DeleteBucketInput, opts ...func(*s3.Options)) (*s3.DeleteBucketOutput, error)
	PutBucketTagging(ctx context.Context, in *s3.PutBucketTaggingInput, opts ...func(*s3.Options)) (*s3.PutBucketTaggingOutput, error)
	PutPublicAccessBlock(ctx context.Context, in *s3.PutPublicAccessBlockInput, opts ...func(*s3.Options)) (*s3.PutPublicAccessBlockOutput, error)
	PutBucketEncryption(ctx context.Context, in *s3.PutBucketEncryptionInput, opts ...func(*s3.Options)) (*s3.PutBucketEncryptionOutput, error)
	PutBucketLifecycleConfiguration(ctx context.Context, in *s3.PutBucketLifecycleConfigurationInput, opts ...func(*s3.Options)) (*s3.PutBucketLifecycleConfigurationOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput, opts ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

// ExportBundleTag marks a zipped export so the data bucket's lifecycle rule can
// expire bundles *only*. Must match what export.S3Presigner puts on the object —
// a mismatch would either leave bundles accumulating or, far worse, expire the
// user's saved activations. Mirrors deploy/terraform/storage.tf.
const (
	ExportBundleTagKey   = "foray-export-bundle"
	ExportBundleTagValue = "true"
	// ExportBundleExpiryDays is short: a bundle is a convenience copy of data that
	// still exists under sessions/<id>/, so keeping it costs storage for nothing.
	ExportBundleExpiryDays = 1
)

// bucket provisions one S3 bucket with foray's baseline posture: private,
// encrypted, tagged. `lifecycleBundles` adds the export-bundle expiry rule (the
// data bucket only).
//
// Deliberately NOT here: the web bucket's OAC read policy and the data bucket's
// CORS rule. Both reference the CloudFront distribution's ARN/domain, so they are
// applied once CloudFront exists rather than guessed at now.
type bucket struct {
	api s3DeployAPI
	// bucketName is a field, so the resource interface's name() is a method below.
	bucketName string
	region     string
	// lifecycleBundles applies the expire-export-bundles rule.
	lifecycleBundles bool
	// emptyOnRemove allows Teardown to delete objects before the bucket. S3 refuses
	// to delete a non-empty bucket, and "nothing left billing" means the objects
	// have to go too.
	emptyOnRemove bool
}

func (b *bucket) kind() string { return "s3 bucket" }
func (b *bucket) name() string { return b.bucketName }

func (b *bucket) ensure(ctx context.Context) (Action, error) {
	act := Action{Kind: b.kind(), Name: b.bucketName}

	exists, err := b.exists(ctx)
	if err != nil {
		return act, err
	}
	if exists {
		act.Op = OpExists
	} else {
		if err := b.create(ctx); err != nil {
			return act, err
		}
		act.Op = OpCreate
	}

	// Converge the posture on every run, existing bucket or not: these settings are
	// the difference between a private bucket and a public one, and an Apply that
	// skipped them on a pre-existing bucket would leave that unverified.
	if err := b.applyPosture(ctx); err != nil {
		return act, err
	}
	return act, nil
}

// create makes the bucket, handling the two S3 shapes that bite here.
func (b *bucket) create(ctx context.Context) error {
	in := &s3.CreateBucketInput{Bucket: aws.String(b.bucketName)}
	// us-east-1 must NOT carry a LocationConstraint, and every other region must.
	// Sending it for us-east-1 is an InvalidLocationConstraint error; omitting it
	// elsewhere silently creates the bucket in us-east-1.
	if b.region != "" && b.region != "us-east-1" {
		in.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraint(b.region),
		}
	}
	if _, err := b.api.CreateBucket(ctx, in); err != nil {
		// Bucket names are global. "Owned by you" means a concurrent or earlier run
		// of this same deploy got there first — idempotent success. "Already exists"
		// means someone else owns the name, which no amount of retrying fixes, so
		// say so plainly.
		var owned *s3types.BucketAlreadyOwnedByYou
		if errors.As(err, &owned) {
			return nil
		}
		var taken *s3types.BucketAlreadyExists
		if errors.As(err, &taken) {
			return fmt.Errorf("bucket name %q is taken by another AWS account (S3 bucket names are global) — choose a different name: %w", b.bucketName, err)
		}
		return fmt.Errorf("create bucket: %w", err)
	}
	return nil
}

// applyPosture sets the things that make the bucket safe, and is idempotent — each
// call is a Put of the desired state.
func (b *bucket) applyPosture(ctx context.Context) error {
	// Block all public access. The web bucket is read by CloudFront through an
	// origin access control, never by the internet directly; the data bucket holds
	// the user's activations and is reached only by presigned URL.
	_, err := b.api.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{
		Bucket: aws.String(b.bucketName),
		PublicAccessBlockConfiguration: &s3types.PublicAccessBlockConfiguration{
			BlockPublicAcls:       aws.Bool(true),
			BlockPublicPolicy:     aws.Bool(true),
			IgnorePublicAcls:      aws.Bool(true),
			RestrictPublicBuckets: aws.Bool(true),
		},
	})
	if err != nil {
		return fmt.Errorf("block public access: %w", err)
	}

	_, err = b.api.PutBucketEncryption(ctx, &s3.PutBucketEncryptionInput{
		Bucket: aws.String(b.bucketName),
		ServerSideEncryptionConfiguration: &s3types.ServerSideEncryptionConfiguration{
			Rules: []s3types.ServerSideEncryptionRule{{
				ApplyServerSideEncryptionByDefault: &s3types.ServerSideEncryptionByDefault{
					SSEAlgorithm: s3types.ServerSideEncryptionAes256,
				},
			}},
		},
	})
	if err != nil {
		return fmt.Errorf("enable encryption: %w", err)
	}

	if _, err := b.api.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{
		Bucket:  aws.String(b.bucketName),
		Tagging: &s3types.Tagging{TagSet: s3Tags(Tags(b.bucketName))},
	}); err != nil {
		return fmt.Errorf("tag bucket: %w", err)
	}

	if b.lifecycleBundles {
		if err := b.applyBundleLifecycle(ctx); err != nil {
			return err
		}
	}
	return nil
}

// applyBundleLifecycle expires export bundles and nothing else.
//
// The tag filter is load-bearing: a rule scoped to the sessions/ prefix instead
// would delete the user's saved activations after a day. The presigner tags the
// zip it writes; only those objects match.
func (b *bucket) applyBundleLifecycle(ctx context.Context) error {
	_, err := b.api.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket: aws.String(b.bucketName),
		LifecycleConfiguration: &s3types.BucketLifecycleConfiguration{
			Rules: []s3types.LifecycleRule{{
				ID:     aws.String("expire-export-bundles"),
				Status: s3types.ExpirationStatusEnabled,
				Filter: &s3types.LifecycleRuleFilter{
					Tag: &s3types.Tag{
						Key:   aws.String(ExportBundleTagKey),
						Value: aws.String(ExportBundleTagValue),
					},
				},
				Expiration: &s3types.LifecycleExpiration{Days: aws.Int32(ExportBundleExpiryDays)},
			}},
		},
	})
	if err != nil {
		return fmt.Errorf("set export-bundle lifecycle: %w", err)
	}
	return nil
}

func (b *bucket) remove(ctx context.Context) (Action, error) {
	act := Action{Kind: b.kind(), Name: b.bucketName}
	exists, err := b.exists(ctx)
	if err != nil {
		return act, err
	}
	if !exists {
		act.Op = OpAbsent
		return act, nil
	}
	if b.emptyOnRemove {
		n, err := b.empty(ctx)
		if err != nil {
			return act, err
		}
		if n > 0 {
			act.Detail = fmt.Sprintf("deleted %d object(s) first", n)
		}
	}
	if _, err := b.api.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(b.bucketName)}); err != nil {
		return act, fmt.Errorf("delete bucket: %w", err)
	}
	act.Op = OpDelete
	return act, nil
}

// empty deletes every object in the bucket, paging through the listing. S3 will
// not delete a non-empty bucket, so this is what makes teardown actually reach $0
// rather than leaving stored bytes billing behind a deleted stack.
func (b *bucket) empty(ctx context.Context) (int, error) {
	var deleted int
	var token *string
	for {
		page, err := b.api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(b.bucketName),
			ContinuationToken: token,
		})
		if err != nil {
			return deleted, fmt.Errorf("list objects: %w", err)
		}
		if len(page.Contents) > 0 {
			ids := make([]s3types.ObjectIdentifier, 0, len(page.Contents))
			for _, o := range page.Contents {
				ids = append(ids, s3types.ObjectIdentifier{Key: o.Key})
			}
			out, err := b.api.DeleteObjects(ctx, &s3.DeleteObjectsInput{
				Bucket: aws.String(b.bucketName),
				Delete: &s3types.Delete{Objects: ids, Quiet: aws.Bool(true)},
			})
			if err != nil {
				return deleted, fmt.Errorf("delete objects: %w", err)
			}
			// DeleteObjects reports per-key failures in the body with a 200 status, so
			// a bucket can stay non-empty while the call "succeeds". Surface it.
			if len(out.Errors) > 0 {
				return deleted, fmt.Errorf("delete objects: %d key(s) failed, first: %s %s",
					len(out.Errors), aws.ToString(out.Errors[0].Key), aws.ToString(out.Errors[0].Message))
			}
			deleted += len(ids)
		}
		if page.IsTruncated == nil || !*page.IsTruncated {
			return deleted, nil
		}
		token = page.NextContinuationToken
	}
}

// exists reports whether the bucket is present and ours. A 404 means absent; a 403
// means the name exists in another account, which is worth distinguishing because
// the fix is to pick a different name, not to retry.
func (b *bucket) exists(ctx context.Context) (bool, error) {
	_, err := b.api.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(b.bucketName)})
	if err == nil {
		return true, nil
	}
	var notFound *s3types.NotFound
	if errors.As(err, &notFound) {
		return false, nil
	}
	if isS3Forbidden(err) {
		return false, fmt.Errorf("bucket %q exists but is not accessible from this account (S3 names are global) — choose a different name: %w", b.bucketName, err)
	}
	return false, fmt.Errorf("head bucket: %w", err)
}

// s3Tags converts the common tag map to S3's tag shape.
func s3Tags(m map[string]string) []s3types.Tag {
	out := make([]s3types.Tag, 0, len(m))
	for k, v := range m {
		out = append(out, s3types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out
}

// isS3Forbidden reports whether an error is S3's 403. HeadBucket on a bucket owned
// by another account returns 403, not 404, so telling them apart is what lets the
// error message name the actual problem (the name is taken) instead of a generic
// permissions complaint.
func isS3Forbidden(err error) bool {
	var apiErr interface{ ErrorCode() string }
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "Forbidden", "AccessDenied", "AllAccessDisabled":
			return true
		}
	}
	var respErr interface{ HTTPStatusCode() int }
	if errors.As(err, &respErr) {
		return respErr.HTTPStatusCode() == 403
	}
	return false
}
