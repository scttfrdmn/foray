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

	"github.com/aws/aws-sdk-go-v2/aws"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// --- S3 ---------------------------------------------------------------------

// CreateBucket must carry a LocationConstraint everywhere EXCEPT us-east-1: it is
// an InvalidLocationConstraint error there, and omitting it elsewhere silently
// creates the bucket in us-east-1 — which for foray would put the data bucket out
// of region from the worker that writes to it.
func TestBucketRegionConstraint(t *testing.T) {
	tests := []struct {
		name       string
		region     string
		wantRegion string
	}{
		{"us-east-1 sends no constraint", "us-east-1", "us-east-1"},
		{"us-west-2 sends its constraint", "us-west-2", "us-west-2"},
		{"eu-central-1 sends its constraint", "eu-central-1", "eu-central-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeS3()
			b := &bucket{api: f, bucketName: "foray-x", region: tt.region}
			if _, err := b.ensure(context.Background()); err != nil {
				t.Fatalf("ensure: %v", err)
			}
			got := f.buckets["foray-x"].region
			if got != tt.wantRegion {
				t.Errorf("bucket created in %q, want %q", got, tt.wantRegion)
			}
		})
	}
}

// A name we already own is idempotent success — a concurrent or earlier run of the
// same deploy got there first.
func TestBucketAlreadyOwnedByYouIsSuccess(t *testing.T) {
	f := newFakeS3()
	f.failCreate["foray-x"] = &s3types.BucketAlreadyOwnedByYou{Message: aws.String("yours")}
	b := &bucket{api: f, bucketName: "foray-x", region: "us-west-2"}
	if _, err := b.ensure(context.Background()); err != nil {
		t.Fatalf("ensure should treat BucketAlreadyOwnedByYou as success: %v", err)
	}
}

// A name owned by someone else can never be fixed by retrying, so the error has to
// say what the deployer must actually do: pick a different name.
func TestBucketAlreadyExistsElsewhereExplainsItself(t *testing.T) {
	f := newFakeS3()
	f.failCreate["foray-x"] = &s3types.BucketAlreadyExists{Message: aws.String("taken")}
	b := &bucket{api: f, bucketName: "foray-x", region: "us-west-2"}
	_, err := b.ensure(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "global") {
		t.Errorf("error %q should explain that S3 bucket names are global", err)
	}
}

// HeadBucket answers 403 (not 404) for a bucket in another account, and that
// distinction is what lets the message name the real problem.
func TestBucketForbiddenMeansNameTaken(t *testing.T) {
	f := newFakeS3()
	f.forbidden["foray-x"] = true
	b := &bucket{api: f, bucketName: "foray-x", region: "us-west-2"}
	_, err := b.ensure(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "not accessible") {
		t.Errorf("error %q should say the name exists in another account", err)
	}
}

// The safety posture is re-applied to a bucket that already exists. An Apply that
// skipped it would leave "is this bucket private?" unverified — which is the whole
// question for a bucket holding the user's activations.
func TestBucketPostureConvergesOnExistingBucket(t *testing.T) {
	f := newFakeS3()
	// Pre-existing, wide-open bucket.
	f.buckets["foray-x"] = &fakeBucket{region: "us-west-2", objects: map[string]struct{}{}, tags: map[string]string{}}

	b := &bucket{api: f, bucketName: "foray-x", region: "us-west-2", lifecycleBundles: true}
	act, err := b.ensure(context.Background())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if act.Op != OpExists {
		t.Errorf("op = %s, want exists", act.Op)
	}
	got := f.buckets["foray-x"]
	if !got.publicBlock {
		t.Error("public access block not applied to the pre-existing bucket")
	}
	if !got.encrypted {
		t.Error("encryption not applied to the pre-existing bucket")
	}
	if got.tags[TagProject] != TagProjectValue {
		t.Error("Project tag not applied — teardown-verify would not see this bucket")
	}
}

// The export-bundle lifecycle rule MUST filter on the tag. A prefix rule over
// sessions/ would expire the user's saved activations after a day, which is data
// they cannot regenerate without re-running the experiment.
func TestBundleLifecycleFiltersOnTagNotPrefix(t *testing.T) {
	f := newFakeS3()
	b := &bucket{api: f, bucketName: "foray-data", region: "us-west-2", lifecycleBundles: true}
	if _, err := b.ensure(context.Background()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	cfgIn := f.buckets["foray-data"].lifecycleInput
	if cfgIn == nil || len(cfgIn.Rules) != 1 {
		t.Fatalf("lifecycle config = %+v, want one rule", cfgIn)
	}
	rule := cfgIn.Rules[0]
	if rule.Filter == nil || rule.Filter.Tag == nil {
		t.Fatalf("rule filter = %+v, want a TAG filter", rule.Filter)
	}
	if k := aws.ToString(rule.Filter.Tag.Key); k != ExportBundleTagKey {
		t.Errorf("filter tag key = %q, want %q", k, ExportBundleTagKey)
	}
	if rule.Filter.Prefix != nil && *rule.Filter.Prefix != "" {
		t.Errorf("rule has a prefix filter (%q) — that would expire the user's saves", *rule.Filter.Prefix)
	}
}

// The web bucket gets no lifecycle rule; only the data bucket expires bundles.
func TestWebBucketHasNoLifecycleRule(t *testing.T) {
	f := newFakeS3()
	b := &bucket{api: f, bucketName: "foray-web", region: "us-west-2"}
	if _, err := b.ensure(context.Background()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if f.buckets["foray-web"].lifecycle {
		t.Error("web bucket should carry no lifecycle configuration")
	}
}

// S3 refuses to delete a non-empty bucket, so teardown empties first. Without this
// the bucket survives and its stored bytes keep billing.
func TestBucketTeardownEmptiesFirst(t *testing.T) {
	f := newFakeS3()
	b := &bucket{api: f, bucketName: "foray-data", region: "us-west-2", emptyOnRemove: true}
	if _, err := b.ensure(context.Background()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	f.seedObject("foray-data", "sessions/i-0abc/activations/layer0.safetensors")
	f.seedObject("foray-data", "sessions/i-0abc/outputs.json")

	act, err := b.remove(context.Background())
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if act.Op != OpDelete {
		t.Errorf("op = %s, want delete", act.Op)
	}
	if _, ok := f.buckets["foray-data"]; ok {
		t.Error("bucket survived teardown")
	}
	if !strings.Contains(act.Detail, "2 object") {
		t.Errorf("detail = %q, want a note about the objects deleted", act.Detail)
	}
}

// DeleteObjects reports per-key failures in the response body with a 200 status, so
// a "successful" call can leave the bucket non-empty. That must surface, or
// teardown reports success while bytes keep billing.
func TestBucketTeardownSurfacesPerKeyDeleteFailures(t *testing.T) {
	f := newFakeS3()
	b := &bucket{api: f, bucketName: "foray-data", region: "us-west-2", emptyOnRemove: true}
	if _, err := b.ensure(context.Background()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	f.seedObject("foray-data", "stuck-object")
	f.deleteObjectErrors["foray-data"] = true

	if _, err := b.remove(context.Background()); err == nil {
		t.Fatal("want the per-key delete failure surfaced, not a silent success")
	}
}

// --- DynamoDB ---------------------------------------------------------------

// The table is on-demand with TTL on `expires`. Both are resting-cost decisions:
// provisioned capacity bills around the clock, and without TTL the rows never
// expire, so a $0-at-rest table grows a bill.
func TestSessionsTableCreatedOnDemandWithTTL(t *testing.T) {
	f := newFakeDynamo()
	tbl := &sessionsTable{api: f, table: "foray-sessions"}
	act, err := tbl.ensure(context.Background())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if act.Op != OpCreate {
		t.Errorf("op = %s, want create", act.Op)
	}
	got := f.tables["foray-sessions"]
	if got.billingMode != ddbtypes.BillingModePayPerRequest {
		t.Errorf("billing mode = %s, want PAY_PER_REQUEST (invariant #31)", got.billingMode)
	}
	if !got.ttlEnabled {
		t.Error("TTL not enabled — rows would never expire")
	}
	if got.tags[TagProject] != TagProjectValue {
		t.Error("Project tag missing — teardown-verify would not see this table")
	}
}

// UpdateTimeToLive errors when asked to enable TTL that is already on, so a
// re-Apply must not call it again.
func TestSessionsTableDoesNotReEnableTTL(t *testing.T) {
	f := newFakeDynamo()
	tbl := &sessionsTable{api: f, table: "foray-sessions"}
	if _, err := tbl.ensure(context.Background()); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if f.ttlUpdates != 1 {
		t.Fatalf("ttl updates after create = %d, want 1", f.ttlUpdates)
	}
	if _, err := tbl.ensure(context.Background()); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if f.ttlUpdates != 1 {
		t.Errorf("ttl updates after re-apply = %d, want still 1 — re-enabling is an error", f.ttlUpdates)
	}
}

// A pre-existing provisioned table is reported, not silently converted: switching
// billing mode is a cost decision the deployer should make knowingly, and an
// on-demand table is invariant #31 — so saying nothing is the worst option.
func TestSessionsTableWarnsOnProvisionedBillingMode(t *testing.T) {
	f := newFakeDynamo()
	f.tables["foray-sessions"] = &fakeTable{
		billingMode: ddbtypes.BillingModeProvisioned,
		status:      ddbtypes.TableStatusActive,
		tags:        map[string]string{},
	}
	tbl := &sessionsTable{api: f, table: "foray-sessions"}
	act, err := tbl.ensure(context.Background())
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if act.Op != OpExists {
		t.Errorf("op = %s, want exists", act.Op)
	}
	if !strings.Contains(act.Detail, "PAY_PER_REQUEST") {
		t.Errorf("detail = %q, want a warning about provisioned capacity billing at rest", act.Detail)
	}
}

// An older provisioned table omits the billing-mode summary entirely; reading that
// absence as on-demand would hide exactly the case worth warning about.
func TestBillingModeOfDefaultsToProvisioned(t *testing.T) {
	if got := billingModeOf(nil); got != string(ddbtypes.BillingModeProvisioned) {
		t.Errorf("billingModeOf(nil) = %q, want PROVISIONED", got)
	}
	if got := billingModeOf(&ddbtypes.TableDescription{}); got != string(ddbtypes.BillingModeProvisioned) {
		t.Errorf("billingModeOf(no summary) = %q, want PROVISIONED", got)
	}
}

// Removing a table that is not there is not an error — Teardown must be re-runnable.
func TestSessionsTableRemoveAbsent(t *testing.T) {
	f := newFakeDynamo()
	tbl := &sessionsTable{api: f, table: "foray-sessions"}
	act, err := tbl.remove(context.Background())
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if act.Op != OpAbsent {
		t.Errorf("op = %s, want absent", act.Op)
	}
}
