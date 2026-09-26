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
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
)

// --- teardown: the part CloudFront makes hard --------------------------------

// A distribution cannot be deleted while enabled, and the disable must finish
// propagating first. The fake refuses both the way the real API does, so this test
// fails if the dance is skipped — which is the single most likely way teardown leaves
// a distribution (and its bucket, which cannot be deleted under it) behind.
func TestDistributionTeardownDisablesWaitsThenDeletes(t *testing.T) {
	d, f := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if f.cf.distByComment(distributionComment) == nil {
		t.Fatal("distribution not created")
	}

	if _, err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if f.cf.deleteWhileEnabled {
		t.Error("attempted to delete an enabled distribution — CloudFront refuses that")
	}
	if f.cf.staleIfMatch {
		t.Error("used a stale ETag — every CloudFront mutation rolls it, so If-Match must be re-read")
	}
	if len(f.cf.dists) != 0 {
		t.Errorf("%d distribution(s) left", len(f.cf.dists))
	}
}

// The OAC outlives the distribution and has its own ETag, so it needs its own
// deletion — and it cannot go first, since the distribution references it.
func TestOACRemovedAfterDistribution(t *testing.T) {
	d, f := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(f.cf.oacs) != 1 {
		t.Fatalf("got %d origin access controls, want 1", len(f.cf.oacs))
	}
	if _, err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(f.cf.oacs) != 0 {
		t.Errorf("%d origin access control(s) left behind", len(f.cf.oacs))
	}
}

// Teardown must wait for propagation even when the deploy was told not to: skipping
// the wait does not make deletion faster, it makes it fail.
func TestTeardownWaitsEvenWithNoWait(t *testing.T) {
	d, f := newTestDeployer(t, testConfig())
	for _, r := range d.resources {
		if dist, ok := r.(*distribution); ok {
			dist.NoWait = true
		}
	}
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown with NoWait must still succeed: %v", err)
	}
	if f.cf.deleteWhileEnabled {
		t.Error("NoWait skipped the disable/propagate wait on teardown — the delete would fail")
	}
}

// The CDN comes off before the buckets it reads: S3 refuses to delete a bucket an
// enabled distribution is serving from.
func TestDistributionRemovedBeforeBuckets(t *testing.T) {
	d, _ := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	actions, err := d.Teardown(context.Background())
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	cdnAt, firstBucketAt := -1, -1
	for i, a := range actions {
		if a.Kind == "cloudfront" {
			cdnAt = i
		}
		if a.Kind == "s3 bucket" && firstBucketAt < 0 {
			firstBucketAt = i
		}
	}
	if cdnAt < 0 || firstBucketAt < 0 {
		t.Fatalf("missing actions: cdn=%d bucket=%d", cdnAt, firstBucketAt)
	}
	if cdnAt > firstBucketAt {
		t.Errorf("CDN removed at %d, after a bucket at %d", cdnAt, firstBucketAt)
	}
}

// --- the two bucket settings the CDN owns -----------------------------------

// The web bucket's read policy must be scoped to THIS distribution. Without the
// AWS:SourceArn condition it grants every CloudFront distribution in every account
// read access to the bucket.
func TestOACPolicyScopedToThisDistribution(t *testing.T) {
	cfg := testConfig()
	d, f := newTestDeployer(t, cfg)
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	dist := f.cf.distByComment(distributionComment)
	policy := f.s3.buckets[cfg.WebBucket].policy
	if policy == "" {
		t.Fatal("web bucket has no policy — CloudFront would get 403 from S3 and the SPA would never load")
	}

	doc := mustParse(t, policy)
	st := doc.Statement[0]
	if st.Condition == nil {
		t.Fatal("no AWS:SourceArn condition — this grants EVERY CloudFront distribution read access")
	}
	cond, _ := json.Marshal(st.Condition)
	want := distributionARN(cfg.AccountID, dist.id)
	if !strings.Contains(string(cond), want) {
		t.Errorf("condition = %s, want it scoped to %s", cond, want)
	}
	// Read-only, and only the objects.
	if st.Action != "s3:GetObject" {
		t.Errorf("action = %v, want only s3:GetObject", st.Action)
	}
	if got, want := st.Resource, bucketARN(cfg.WebBucket)+"/*"; got != want {
		t.Errorf("resource = %v, want %v", got, want)
	}
}

// The data bucket's CORS rule must allow only the distribution's own origin: the
// export link is a presigned URL the browser follows from the page.
func TestDataBucketCORSAllowsOnlyTheDistribution(t *testing.T) {
	cfg := testConfig()
	d, f := newTestDeployer(t, cfg)
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	dist := f.cf.distByComment(distributionComment)
	cors := f.s3.buckets[cfg.DataBucket].cors
	if cors == nil || len(cors.CORSRules) != 1 {
		t.Fatalf("data bucket CORS = %+v, want one rule", cors)
	}
	rule := cors.CORSRules[0]
	if len(rule.AllowedOrigins) != 1 || rule.AllowedOrigins[0] != "https://"+dist.domain {
		t.Errorf("allowed origins = %v, want just https://%s", rule.AllowedOrigins, dist.domain)
	}
	for _, m := range rule.AllowedMethods {
		if m != "GET" {
			t.Errorf("allowed method %q — export is a download, GET is all it needs", m)
		}
	}
	// The web bucket is served through CloudFront, not fetched cross-origin.
	if f.s3.buckets[cfg.WebBucket].cors != nil {
		t.Error("web bucket has a CORS rule it does not need")
	}
}

// --- distribution config -----------------------------------------------------

// /api/* and /sessions/* must reach API Gateway uncached, and must NOT forward the
// viewer's Host header — API Gateway rejects a request whose Host is not its own,
// which is the classic 403 for an API behind CloudFront.
func TestAPIBehaviorsAreUncachedAndPreserveHost(t *testing.T) {
	d, f := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	cfgOut := f.cf.distByComment(distributionComment).config
	if cfgOut.CacheBehaviors == nil || len(cfgOut.CacheBehaviors.Items) != 2 {
		t.Fatalf("want 2 ordered behaviors, got %+v", cfgOut.CacheBehaviors)
	}
	seen := map[string]cftypes.CacheBehavior{}
	for _, b := range cfgOut.CacheBehaviors.Items {
		seen[aws.ToString(b.PathPattern)] = b
	}
	for _, pattern := range []string{"/api/*", "/sessions/*"} {
		b, ok := seen[pattern]
		if !ok {
			t.Fatalf("no behavior for %s", pattern)
		}
		if aws.ToString(b.TargetOriginId) != originAPI {
			t.Errorf("%s targets %q, want %q", pattern, aws.ToString(b.TargetOriginId), originAPI)
		}
		if aws.ToString(b.CachePolicyId) != cachePolicyCachingDisabled {
			t.Errorf("%s is cached — a cached trace or plan would be a correctness bug", pattern)
		}
		if aws.ToString(b.OriginRequestPolicyId) != originReqAllViewerExceptHostHeader {
			t.Errorf("%s origin request policy = %q, want AllViewerExceptHostHeader (API Gateway rejects a foreign Host)",
				pattern, aws.ToString(b.OriginRequestPolicyId))
		}
		// The page POSTs to /api/approve and /sessions/{id}/trace.
		var hasPost bool
		for _, m := range b.AllowedMethods.Items {
			if m == cftypes.MethodPost {
				hasPost = true
			}
		}
		if !hasPost {
			t.Errorf("%s does not allow POST — the brain loop and traces would fail", pattern)
		}
	}
}

// The SPA is cached and served from S3; an SPA owns its own routing, so a path S3 does
// not have is a client route rather than a missing page.
func TestDefaultBehaviorServesTheSPA(t *testing.T) {
	d, f := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	cfgOut := f.cf.distByComment(distributionComment).config
	if got := aws.ToString(cfgOut.DefaultCacheBehavior.TargetOriginId); got != originWeb {
		t.Errorf("default origin = %q, want %q", got, originWeb)
	}
	if got := aws.ToString(cfgOut.DefaultCacheBehavior.CachePolicyId); got != cachePolicyCachingOptimized {
		t.Errorf("default cache policy = %q, want CachingOptimized", got)
	}
	if got := aws.ToString(cfgOut.DefaultRootObject); got != "index.html" {
		t.Errorf("default root object = %q", got)
	}
	if cfgOut.CustomErrorResponses == nil || len(cfgOut.CustomErrorResponses.Items) != 2 {
		t.Fatalf("want 403 and 404 rewritten to index.html, got %+v", cfgOut.CustomErrorResponses)
	}
	for _, e := range cfgOut.CustomErrorResponses.Items {
		if aws.ToString(e.ResponseCode) != "200" || aws.ToString(e.ResponsePagePath) != "/index.html" {
			t.Errorf("error response %d → %s %s, want 200 /index.html",
				aws.ToInt32(e.ErrorCode), aws.ToString(e.ResponseCode), aws.ToString(e.ResponsePagePath))
		}
	}
}

// The API origin must be the API's own endpoint host, and the S3 origin the REGIONAL
// domain — the global form can 307 to the regional endpoint on a fresh bucket, which
// CloudFront surfaces as a signature failure.
func TestOriginsPointAtTheRightHosts(t *testing.T) {
	cfg := testConfig()
	d, f := newTestDeployer(t, cfg)
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	apiID := f.apigw.apiByName(APIName).id
	cfgOut := f.cf.distByComment(distributionComment).config

	byID := map[string]cftypes.Origin{}
	for _, o := range cfgOut.Origins.Items {
		byID[aws.ToString(o.Id)] = o
	}
	if got, want := aws.ToString(byID[originAPI].DomainName), apiEndpointHost(cfg.Region, apiID); got != want {
		t.Errorf("api origin = %q, want %q", got, want)
	}
	webDomain := aws.ToString(byID[originWeb].DomainName)
	if !strings.Contains(webDomain, "."+cfg.Region+".") {
		t.Errorf("web origin = %q, want the REGIONAL s3 domain", webDomain)
	}
	if aws.ToString(byID[originWeb].OriginAccessControlId) == "" {
		t.Error("web origin has no OAC — the private bucket would be unreadable")
	}
}

// Re-applying converges: one distribution, one OAC, same id.
func TestCDNApplyIsIdempotent(t *testing.T) {
	d, f := newTestDeployer(t, testConfig())
	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	firstID := f.cf.distByComment(distributionComment).id

	if _, err := d.Apply(context.Background()); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if len(f.cf.dists) != 1 {
		t.Errorf("%d distributions after re-apply, want 1", len(f.cf.dists))
	}
	if len(f.cf.oacs) != 1 {
		t.Errorf("%d origin access controls after re-apply, want 1", len(f.cf.oacs))
	}
	if got := f.cf.distByComment(distributionComment).id; got != firstID {
		t.Errorf("distribution id changed on re-apply (%s → %s)", firstID, got)
	}
}

// Distributions have no name, so identity is the comment — and a duplicate must be
// reported rather than guessed between, or a deploy would repoint the bucket policy at
// a different distribution than the last run did.
func TestDuplicateDistributionsAreReported(t *testing.T) {
	f := newFakeCloudFront()
	for i := 0; i < 2; i++ {
		f.dists[f.nextID("E")] = &fakeDistribution{
			id:      fmt.Sprintf("Edup%d", i),
			comment: distributionComment,
			enabled: true,
			status:  "Deployed",
		}
	}
	d := &distribution{cf: f, s3: newFakeS3(), cfg: mustValidate(t, testConfig()), sleep: func(time.Duration) {}}
	if _, err := d.ensure(context.Background()); err == nil {
		t.Fatal("want an error naming the ambiguity")
	} else if !strings.Contains(err.Error(), "delete the extras") {
		t.Errorf("error %q should say what to do about it", err)
	}
}
