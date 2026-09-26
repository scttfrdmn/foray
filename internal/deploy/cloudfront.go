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
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// cloudfrontAPI is the slice of CloudFront this package uses. The disable/wait/delete
// trio is as load-bearing as creation — see distribution.remove.
type cloudfrontAPI interface {
	ListDistributions(ctx context.Context, in *cloudfront.ListDistributionsInput, opts ...func(*cloudfront.Options)) (*cloudfront.ListDistributionsOutput, error)
	CreateDistributionWithTags(ctx context.Context, in *cloudfront.CreateDistributionWithTagsInput, opts ...func(*cloudfront.Options)) (*cloudfront.CreateDistributionWithTagsOutput, error)
	GetDistribution(ctx context.Context, in *cloudfront.GetDistributionInput, opts ...func(*cloudfront.Options)) (*cloudfront.GetDistributionOutput, error)
	GetDistributionConfig(ctx context.Context, in *cloudfront.GetDistributionConfigInput, opts ...func(*cloudfront.Options)) (*cloudfront.GetDistributionConfigOutput, error)
	UpdateDistribution(ctx context.Context, in *cloudfront.UpdateDistributionInput, opts ...func(*cloudfront.Options)) (*cloudfront.UpdateDistributionOutput, error)
	DeleteDistribution(ctx context.Context, in *cloudfront.DeleteDistributionInput, opts ...func(*cloudfront.Options)) (*cloudfront.DeleteDistributionOutput, error)

	ListOriginAccessControls(ctx context.Context, in *cloudfront.ListOriginAccessControlsInput, opts ...func(*cloudfront.Options)) (*cloudfront.ListOriginAccessControlsOutput, error)
	CreateOriginAccessControl(ctx context.Context, in *cloudfront.CreateOriginAccessControlInput, opts ...func(*cloudfront.Options)) (*cloudfront.CreateOriginAccessControlOutput, error)
	DeleteOriginAccessControl(ctx context.Context, in *cloudfront.DeleteOriginAccessControlInput, opts ...func(*cloudfront.Options)) (*cloudfront.DeleteOriginAccessControlOutput, error)
	GetOriginAccessControl(ctx context.Context, in *cloudfront.GetOriginAccessControlInput, opts ...func(*cloudfront.Options)) (*cloudfront.GetOriginAccessControlOutput, error)
}

// s3PolicyAPI is the bucket-level S3 slice CloudFront needs: the OAC read policy on
// the web bucket and the CORS rule on the data bucket both name the distribution, so
// they can only be written once it exists (which is why storage.go deliberately left
// them out).
type s3PolicyAPI interface {
	PutBucketPolicy(ctx context.Context, in *s3.PutBucketPolicyInput, opts ...func(*s3.Options)) (*s3.PutBucketPolicyOutput, error)
	PutBucketCors(ctx context.Context, in *s3.PutBucketCorsInput, opts ...func(*s3.Options)) (*s3.PutBucketCorsOutput, error)
}

const (
	// OACName identifies the origin access control. CloudFront has no name-based
	// lookup for distributions, but OACs do carry a name.
	OACName = "foray-web-oac"
	// distributionComment is the distribution's identity as far as this package is
	// concerned: distributions have no name, and the comment is the only
	// caller-chosen field that survives round-tripping.
	distributionComment = "foray control plane"

	originWeb = "web-s3"
	originAPI = "api-gw"

	// AWS-managed policy ids. Using the managed policies rather than authoring our
	// own keeps them correct as AWS evolves them, and they are the same ids
	// deploy/terraform/cdn.tf uses.
	//
	//	CachingOptimized          — the SPA: cache it
	//	CachingDisabled           — /api/* and /sessions/*: never cache a trace or a plan
	//	AllViewerExceptHostHeader — forward everything but Host, which must stay the
	//	                            origin's or API Gateway rejects the request
	cachePolicyCachingOptimized        = "658327ea-f89d-4fab-a63d-7e88639e58f6"
	cachePolicyCachingDisabled         = "4135ea2d-6df8-44a3-9df3-4b5a84be39ad"
	originReqAllViewerExceptHostHeader = "b689b0a8-53d0-40ab-baf2-68738e2966ac"

	// DefaultDistributionWait bounds the wait for a distribution to finish deploying.
	// CloudFront propagates to every edge location, which genuinely takes minutes —
	// this is not a slow API, it is a global rollout.
	DefaultDistributionWait = 20 * time.Minute
)

// distribution provisions the CDN and the two bucket settings that can only be
// written once it exists.
type distribution struct {
	cf  cloudfrontAPI
	s3  s3PolicyAPI
	cfg Config
	// apiHost is the API Gateway endpoint host (no scheme) used as the API origin.
	// Discovered at apply time, because the API's id is generated.
	apiHost func(ctx context.Context) (string, error)
	// NoWait skips waiting for propagation. Create returns as soon as CloudFront
	// accepts the distribution; teardown still waits, because deletion *requires* the
	// disabled state to have propagated.
	NoWait bool
	wait   time.Duration
	sleep  func(time.Duration)
}

// setNoWait lets the Deployer pass --no-wait down without the resource interface
// knowing about it — only the CDN has anything to wait for.
func (d *distribution) setNoWait(v bool) { d.NoWait = v }

func (d *distribution) kind() string { return "cloudfront" }
func (d *distribution) name() string { return d.cfg.WebBucket }

func (d *distribution) ensure(ctx context.Context) (Action, error) {
	act := Action{Kind: d.kind(), Name: distributionComment}

	oacID, err := d.ensureOAC(ctx)
	if err != nil {
		return act, err
	}

	id, domain, err := d.find(ctx)
	if err != nil {
		return act, err
	}
	if id == "" {
		host, err := d.apiHost(ctx)
		if err != nil {
			return act, err
		}
		out, err := d.cf.CreateDistributionWithTags(ctx, &cloudfront.CreateDistributionWithTagsInput{
			DistributionConfigWithTags: &cftypes.DistributionConfigWithTags{
				DistributionConfig: d.config(oacID, host),
				Tags:               &cftypes.Tags{Items: cfTags(Tags(distributionComment))},
			},
		})
		if err != nil {
			return act, fmt.Errorf("create distribution: %w", err)
		}
		id = aws.ToString(out.Distribution.Id)
		domain = aws.ToString(out.Distribution.DomainName)
		act.Op = OpCreate
	} else {
		act.Op = OpExists
		changed, err := d.converge(ctx, id, oacID)
		if err != nil {
			return act, err
		}
		if changed {
			act.Detail = "config updated"
		}
	}

	// The web bucket's read policy and the data bucket's CORS rule both name the
	// distribution, so they land here. Applied on every run: the SPA is unreachable
	// without the policy (403 from S3), and a stale CORS origin breaks the page's
	// export download with a browser error that names neither cause.
	if err := d.allowOACRead(ctx, id); err != nil {
		return act, err
	}
	if err := d.allowCORSFrom(ctx, domain); err != nil {
		return act, err
	}

	if !d.NoWait && act.Op == OpCreate {
		if err := d.waitDeployed(ctx, id); err != nil {
			return act, err
		}
	}
	act.Detail = "https://" + domain
	if d.NoWait && act.Op == OpCreate {
		act.Detail += " (still deploying — edges take a few minutes)"
	}
	return act, nil
}

// converge updates an existing distribution when its config no longer matches what
// this package wants, and reports whether it changed anything.
//
// Without this, a configuration fix could not be rolled out by re-running deploy —
// the operator would have to tear the distribution down and recreate it, which for
// CloudFront is two multi-minute propagations. That is how the custom-error-response
// bug above was found *and* how it would have had to be fixed.
//
// It compares only the fields this package sets, explicitly, rather than diffing the
// whole struct: CloudFront returns many defaulted fields we never specify, so a
// deep comparison would report a difference on every run and trigger a pointless
// multi-minute redeployment each time.
func (d *distribution) converge(ctx context.Context, id, oacID string) (bool, error) {
	cur, err := d.cf.GetDistributionConfig(ctx, &cloudfront.GetDistributionConfigInput{Id: aws.String(id)})
	if err != nil {
		return false, fmt.Errorf("get distribution config: %w", err)
	}
	host, err := d.apiHost(ctx)
	if err != nil {
		return false, err
	}
	want := d.config(oacID, host)
	if !distributionConfigDiffers(cur.DistributionConfig, want) {
		return false, nil
	}

	// Read-modify-write, NOT a fresh config.
	//
	// UpdateDistribution replaces the entire configuration and requires every field
	// CloudFront considers part of it — including ones this package never sets. Sending
	// a config built from scratch is rejected with
	// `IllegalUpdate: Aliases are missing for the resource`, which is how this was
	// found on a real deploy.
	//
	// It is also the right behavior beyond satisfying the API: an operator may have
	// attached a custom domain and certificate, a WAF, or access logging. Those are
	// theirs, so only the fields foray manages are overwritten and everything else is
	// carried through untouched.
	updated := *cur.DistributionConfig
	updated.Origins = want.Origins
	updated.DefaultCacheBehavior = want.DefaultCacheBehavior
	updated.CacheBehaviors = want.CacheBehaviors
	updated.CustomErrorResponses = want.CustomErrorResponses
	updated.DefaultRootObject = want.DefaultRootObject
	updated.PriceClass = want.PriceClass

	if _, err := d.cf.UpdateDistribution(ctx, &cloudfront.UpdateDistributionInput{
		Id:                 aws.String(id),
		IfMatch:            cur.ETag,
		DistributionConfig: &updated,
	}); err != nil {
		return false, fmt.Errorf("update distribution config: %w", err)
	}
	return true, nil
}

// distributionConfigDiffers compares the fields this package manages. Each one is
// something a wrong value breaks in a way that is hard to diagnose from the outside,
// which is why they are worth converging.
func distributionConfigDiffers(cur, want *cftypes.DistributionConfig) bool {
	if cur == nil || want == nil {
		return true
	}
	if aws.ToString(cur.DefaultRootObject) != aws.ToString(want.DefaultRootObject) ||
		cur.PriceClass != want.PriceClass ||
		aws.ToBool(cur.Enabled) != aws.ToBool(want.Enabled) {
		return true
	}
	// Custom error responses: the count is what matters, because the bug this guards
	// against is their *presence* (they rewrite API errors distribution-wide).
	if errorResponseCount(cur) != errorResponseCount(want) {
		return true
	}
	if originSignature(cur.Origins) != originSignature(want.Origins) {
		return true
	}
	if defaultBehaviorSignature(cur.DefaultCacheBehavior) != defaultBehaviorSignature(want.DefaultCacheBehavior) {
		return true
	}
	return behaviorSignature(cur.CacheBehaviors) != behaviorSignature(want.CacheBehaviors)
}

func errorResponseCount(c *cftypes.DistributionConfig) int32 {
	if c.CustomErrorResponses == nil {
		return 0
	}
	return aws.ToInt32(c.CustomErrorResponses.Quantity)
}

// originSignature renders the origins as a comparable string: id, host and OAC.
func originSignature(o *cftypes.Origins) string {
	if o == nil {
		return ""
	}
	parts := make([]string, 0, len(o.Items))
	for _, it := range o.Items {
		parts = append(parts, fmt.Sprintf("%s|%s|%s",
			aws.ToString(it.Id), aws.ToString(it.DomainName), aws.ToString(it.OriginAccessControlId)))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func defaultBehaviorSignature(b *cftypes.DefaultCacheBehavior) string {
	if b == nil {
		return ""
	}
	return fmt.Sprintf("%s|%s|%s",
		aws.ToString(b.TargetOriginId), aws.ToString(b.CachePolicyId), b.ViewerProtocolPolicy)
}

// behaviorSignature renders the ordered behaviors: pattern, origin and both policies.
// A wrong cache policy on /api/* silently caches a trace; a wrong origin request
// policy gets the request rejected by API Gateway.
func behaviorSignature(b *cftypes.CacheBehaviors) string {
	if b == nil {
		return ""
	}
	parts := make([]string, 0, len(b.Items))
	for _, it := range b.Items {
		parts = append(parts, fmt.Sprintf("%s|%s|%s|%s",
			aws.ToString(it.PathPattern), aws.ToString(it.TargetOriginId),
			aws.ToString(it.CachePolicyId), aws.ToString(it.OriginRequestPolicyId)))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// CloudFront's UpdateDistribution requires a FULLY populated configuration, including
// a set of legacy fields that CreateDistribution happily defaults. Omitting any one of
// them is a 400 naming that single field — `The parameter SmoothStreaming flag is
// missing`, `The 'OriginCustomHeaders' field is missing`, and so on, one round trip at
// a time.
//
// So the desired config sets them explicitly rather than relying on create-time
// defaults: it has to be complete enough to send *back*, not just complete enough to
// create. These are the documented defaults, not preferences — except Compress, which
// is on because every asset here is text.
func legacyBehaviorDefaults() (smooth *bool, compress *bool, fle *string, lambdaAssoc *cftypes.LambdaFunctionAssociations, funcAssoc *cftypes.FunctionAssociations, signers *cftypes.TrustedSigners, keyGroups *cftypes.TrustedKeyGroups) {
	return aws.Bool(false),
		aws.Bool(true),
		aws.String(""),
		&cftypes.LambdaFunctionAssociations{Quantity: aws.Int32(0)},
		&cftypes.FunctionAssociations{Quantity: aws.Int32(0)},
		&cftypes.TrustedSigners{Enabled: aws.Bool(false), Quantity: aws.Int32(0)},
		&cftypes.TrustedKeyGroups{Enabled: aws.Bool(false), Quantity: aws.Int32(0)}
}

// config is the distribution's desired state: the SPA from S3, and /api/* plus
// /sessions/* passed through to API Gateway uncached.
func (d *distribution) config(oacID, apiHost string) *cftypes.DistributionConfig {
	viewerMethods := []cftypes.Method{
		cftypes.MethodGet, cftypes.MethodHead, cftypes.MethodOptions,
		cftypes.MethodPut, cftypes.MethodPost, cftypes.MethodPatch, cftypes.MethodDelete,
	}
	cachedMethods := []cftypes.Method{cftypes.MethodGet, cftypes.MethodHead}

	smooth, compress, fle, lambdaAssoc, funcAssoc, signers, keyGroups := legacyBehaviorDefaults()

	apiBehavior := func(pattern string) cftypes.CacheBehavior {
		return cftypes.CacheBehavior{
			SmoothStreaming:            smooth,
			Compress:                   compress,
			FieldLevelEncryptionId:     fle,
			LambdaFunctionAssociations: lambdaAssoc,
			FunctionAssociations:       funcAssoc,
			TrustedSigners:             signers,
			TrustedKeyGroups:           keyGroups,
			PathPattern:                aws.String(pattern),
			TargetOriginId:             aws.String(originAPI),
			ViewerProtocolPolicy:       cftypes.ViewerProtocolPolicyHttpsOnly,
			AllowedMethods: &cftypes.AllowedMethods{
				Quantity: aws.Int32(int32(len(viewerMethods))),
				Items:    viewerMethods,
				CachedMethods: &cftypes.CachedMethods{
					Quantity: aws.Int32(int32(len(cachedMethods))),
					Items:    cachedMethods,
				},
			},
			// Never cache: a trace result or a plan is per-request, and a cached POST
			// response would be a correctness bug, not a performance win.
			CachePolicyId: aws.String(cachePolicyCachingDisabled),
			// Forward everything except Host. Host must remain API Gateway's own or it
			// rejects the request — forwarding the viewer's Host is the classic way to
			// get a 403 from an API behind CloudFront.
			OriginRequestPolicyId: aws.String(originReqAllViewerExceptHostHeader),
		}
	}

	return &cftypes.DistributionConfig{
		CallerReference:   aws.String(distributionComment),
		Comment:           aws.String(distributionComment),
		Enabled:           aws.Bool(true),
		DefaultRootObject: aws.String("index.html"),
		// PriceClass_100 is the cheapest edge set (North America + Europe). The
		// control plane is a static page and a JSON API; paying for every edge
		// location worldwide would be resting cost for no benefit.
		PriceClass: cftypes.PriceClassPriceClass100,
		Origins: &cftypes.Origins{
			Quantity: aws.Int32(2),
			Items: []cftypes.Origin{
				{
					Id: aws.String(originWeb),
					// CloudFront defaults these on create but REQUIRES them on update
					// (`IllegalUpdate: The 'OriginCustomHeaders' field is missing`), so they
					// are set explicitly — the desired config has to be complete enough to
					// send back, not just complete enough to create.
					OriginPath:         aws.String(""),
					CustomHeaders:      &cftypes.CustomHeaders{Quantity: aws.Int32(0)},
					ConnectionAttempts: aws.Int32(3),
					ConnectionTimeout:  aws.Int32(10),
					// The regional domain, not the global one: the global form can 307 to
					// the regional endpoint on a fresh bucket, which CloudFront surfaces
					// as a signature failure.
					DomainName:            aws.String(fmt.Sprintf("%s.s3.%s.amazonaws.com", d.cfg.WebBucket, d.cfg.Region)),
					OriginAccessControlId: aws.String(oacID),
					S3OriginConfig:        &cftypes.S3OriginConfig{OriginAccessIdentity: aws.String("")},
				},
				{
					Id:                 aws.String(originAPI),
					DomainName:         aws.String(apiHost),
					OriginPath:         aws.String(""),
					CustomHeaders:      &cftypes.CustomHeaders{Quantity: aws.Int32(0)},
					ConnectionAttempts: aws.Int32(3),
					ConnectionTimeout:  aws.Int32(10),
					CustomOriginConfig: &cftypes.CustomOriginConfig{
						HTTPPort:             aws.Int32(80),
						HTTPSPort:            aws.Int32(443),
						OriginProtocolPolicy: cftypes.OriginProtocolPolicyHttpsOnly,
						OriginSslProtocols: &cftypes.OriginSslProtocols{
							Quantity: aws.Int32(1),
							Items:    []cftypes.SslProtocol{cftypes.SslProtocolTLSv12},
						},
						OriginReadTimeout:      aws.Int32(30),
						OriginKeepaliveTimeout: aws.Int32(5),
					},
				},
			},
		},
		DefaultCacheBehavior: &cftypes.DefaultCacheBehavior{
			SmoothStreaming:            smooth,
			Compress:                   compress,
			FieldLevelEncryptionId:     fle,
			LambdaFunctionAssociations: lambdaAssoc,
			FunctionAssociations:       funcAssoc,
			TrustedSigners:             signers,
			TrustedKeyGroups:           keyGroups,
			TargetOriginId:             aws.String(originWeb),
			ViewerProtocolPolicy:       cftypes.ViewerProtocolPolicyRedirectToHttps,
			AllowedMethods: &cftypes.AllowedMethods{
				Quantity: aws.Int32(3),
				Items:    []cftypes.Method{cftypes.MethodGet, cftypes.MethodHead, cftypes.MethodOptions},
				CachedMethods: &cftypes.CachedMethods{
					Quantity: aws.Int32(int32(len(cachedMethods))),
					Items:    cachedMethods,
				},
			},
			CachePolicyId: aws.String(cachePolicyCachingOptimized),
		},
		CacheBehaviors: &cftypes.CacheBehaviors{
			Quantity: aws.Int32(2),
			Items: []cftypes.CacheBehavior{
				apiBehavior("/api/*"),
				apiBehavior("/sessions/*"),
			},
		},
		// Deliberately NO custom error responses.
		//
		// The obvious SPA move is to rewrite 403/404 to index.html with a 200 so client
		// routes resolve. It is wrong here, and validating against a real deploy is how
		// that surfaced: **CloudFront applies custom error responses across the whole
		// distribution, not per behavior.** They therefore rewrite errors from the API
		// origin too — `POST /sessions/i-nope/trace` came back as 200 with the page's
		// HTML instead of forayd's `404 {"error":"…unknown session"}`.
		//
		// The severe case is 403: a Cedar denial would be rewritten to a 200 HTML page,
		// so the policy reason foray goes to some trouble to surface verbatim would
		// never reach the user, and the client would be parsing HTML as JSON.
		//
		// The page needs none of it — web/app.js has no client-side routing (no
		// pushState, no hashchange, no pathname reads); it is one document that fetches
		// /api/*. If routing is added later, the right mechanism is a CloudFront
		// Function attached to the default behavior only, which cannot touch the API.
		CustomErrorResponses: &cftypes.CustomErrorResponses{Quantity: aws.Int32(0)},
		Restrictions: &cftypes.Restrictions{
			GeoRestriction: &cftypes.GeoRestriction{
				RestrictionType: cftypes.GeoRestrictionTypeNone,
				Quantity:        aws.Int32(0),
			},
		},
		ViewerCertificate: &cftypes.ViewerCertificate{CloudFrontDefaultCertificate: aws.Bool(true)},
	}
}

// ensureOAC returns the origin access control's id, creating it if absent.
//
// OAC replaces the legacy origin access identity: the bucket stays private and
// CloudFront signs its origin requests with SigV4, so the only reader is this
// distribution.
func (d *distribution) ensureOAC(ctx context.Context) (string, error) {
	list, err := d.cf.ListOriginAccessControls(ctx, &cloudfront.ListOriginAccessControlsInput{})
	if err != nil {
		return "", fmt.Errorf("list origin access controls: %w", err)
	}
	if list.OriginAccessControlList != nil {
		for _, it := range list.OriginAccessControlList.Items {
			if aws.ToString(it.Name) == OACName {
				return aws.ToString(it.Id), nil
			}
		}
	}
	out, err := d.cf.CreateOriginAccessControl(ctx, &cloudfront.CreateOriginAccessControlInput{
		OriginAccessControlConfig: &cftypes.OriginAccessControlConfig{
			Name:                          aws.String(OACName),
			OriginAccessControlOriginType: cftypes.OriginAccessControlOriginTypesS3,
			SigningBehavior:               cftypes.OriginAccessControlSigningBehaviorsAlways,
			SigningProtocol:               cftypes.OriginAccessControlSigningProtocolsSigv4,
		},
	})
	if err != nil {
		return "", fmt.Errorf("create origin access control: %w", err)
	}
	return aws.ToString(out.OriginAccessControl.Id), nil
}

// allowOACRead lets this distribution — and nothing else — read the web bucket.
//
// The condition on AWS:SourceArn is what makes that true: without it, the policy
// grants every CloudFront distribution in every account read access to the bucket.
func (d *distribution) allowOACRead(ctx context.Context, distributionID string) error {
	doc := mustJSON(policyDoc{
		Version: policyVersion,
		Statement: []statement{{
			Sid:       "AllowCloudFrontOACRead",
			Effect:    "Allow",
			Principal: map[string]string{"Service": "cloudfront.amazonaws.com"},
			Action:    "s3:GetObject",
			Resource:  bucketARN(d.cfg.WebBucket) + "/*",
			Condition: map[string]any{
				"StringEquals": map[string]any{
					"AWS:SourceArn": distributionARN(d.cfg.AccountID, distributionID),
				},
			},
		}},
	})
	if _, err := d.s3.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: aws.String(d.cfg.WebBucket),
		Policy: aws.String(doc),
	}); err != nil {
		return fmt.Errorf("allow OAC read on %s: %w", d.cfg.WebBucket, err)
	}
	return nil
}

// allowCORSFrom lets the page fetch its own exports from the data bucket.
//
// GET only, and only from the distribution's own origin: the export link is a
// presigned URL the browser follows, so the bucket has to permit that one origin and
// no other.
func (d *distribution) allowCORSFrom(ctx context.Context, domain string) error {
	if domain == "" {
		return nil
	}
	if _, err := d.s3.PutBucketCors(ctx, &s3.PutBucketCorsInput{
		Bucket: aws.String(d.cfg.DataBucket),
		CORSConfiguration: &s3types.CORSConfiguration{
			CORSRules: []s3types.CORSRule{{
				AllowedMethods: []string{"GET"},
				AllowedOrigins: []string{"https://" + domain},
				AllowedHeaders: []string{"*"},
				MaxAgeSeconds:  aws.Int32(3000),
			}},
		},
	}); err != nil {
		return fmt.Errorf("set CORS on %s: %w", d.cfg.DataBucket, err)
	}
	return nil
}

// remove tears the distribution down, which CloudFront makes a three-step dance.
//
// A distribution cannot be deleted while it is enabled. It must be disabled, that
// change must *finish propagating to every edge*, and only then can it be deleted —
// each mutation carrying the current ETag as If-Match. There is no way to shortcut
// the wait, which is why teardown ignores NoWait: skipping it does not make the
// deletion faster, it makes it fail.
func (d *distribution) remove(ctx context.Context) (Action, error) {
	act := Action{Kind: d.kind(), Name: distributionComment}

	id, _, err := d.find(ctx)
	if err != nil {
		return act, err
	}
	if id == "" {
		// Still clean up the OAC, which outlives the distribution.
		if err := d.removeOAC(ctx); err != nil {
			return act, err
		}
		act.Op = OpAbsent
		return act, nil
	}

	cfgOut, err := d.cf.GetDistributionConfig(ctx, &cloudfront.GetDistributionConfigInput{Id: aws.String(id)})
	if err != nil {
		return act, fmt.Errorf("get distribution config: %w", err)
	}
	etag := aws.ToString(cfgOut.ETag)

	if aws.ToBool(cfgOut.DistributionConfig.Enabled) {
		disabled := cfgOut.DistributionConfig
		disabled.Enabled = aws.Bool(false)
		// The ETag returned here is not kept: the delete below re-reads it, so
		// tracking it would be dead state.
		if _, err := d.cf.UpdateDistribution(ctx, &cloudfront.UpdateDistributionInput{
			Id:                 aws.String(id),
			IfMatch:            aws.String(etag),
			DistributionConfig: disabled,
		}); err != nil {
			return act, fmt.Errorf("disable distribution: %w", err)
		}
	}

	// Wait for the disable to reach Deployed. This is the slow part — minutes.
	if err := d.waitDeployed(ctx, id); err != nil {
		return act, err
	}

	// Re-read the ETag for the delete. The disable rolled it, and re-reading also
	// covers a concurrent deploy from elsewhere having bumped it since — one call, and
	// a stale If-Match would be a PreconditionFailed at the very last step of a
	// teardown.
	fresh, err := d.cf.GetDistributionConfig(ctx, &cloudfront.GetDistributionConfigInput{Id: aws.String(id)})
	if err != nil {
		return act, fmt.Errorf("re-read distribution config: %w", err)
	}
	if _, err := d.cf.DeleteDistribution(ctx, &cloudfront.DeleteDistributionInput{
		Id:      aws.String(id),
		IfMatch: fresh.ETag,
	}); err != nil {
		return act, fmt.Errorf("delete distribution: %w", err)
	}
	if err := d.removeOAC(ctx); err != nil {
		return act, err
	}
	act.Op = OpDelete
	return act, nil
}

// removeOAC deletes the origin access control. It has its own ETag, and it cannot be
// deleted while a distribution still references it — hence after the distribution.
func (d *distribution) removeOAC(ctx context.Context) error {
	list, err := d.cf.ListOriginAccessControls(ctx, &cloudfront.ListOriginAccessControlsInput{})
	if err != nil {
		return fmt.Errorf("list origin access controls: %w", err)
	}
	var id string
	if list.OriginAccessControlList != nil {
		for _, it := range list.OriginAccessControlList.Items {
			if aws.ToString(it.Name) == OACName {
				id = aws.ToString(it.Id)
				break
			}
		}
	}
	if id == "" {
		return nil
	}
	cur, err := d.cf.GetOriginAccessControl(ctx, &cloudfront.GetOriginAccessControlInput{Id: aws.String(id)})
	if err != nil {
		return fmt.Errorf("get origin access control: %w", err)
	}
	if _, err := d.cf.DeleteOriginAccessControl(ctx, &cloudfront.DeleteOriginAccessControlInput{
		Id:      aws.String(id),
		IfMatch: cur.ETag,
	}); err != nil {
		return fmt.Errorf("delete origin access control: %w", err)
	}
	return nil
}

// waitDeployed polls until the distribution's status leaves InProgress.
func (d *distribution) waitDeployed(ctx context.Context, id string) error {
	limit := d.wait
	if limit <= 0 {
		limit = DefaultDistributionWait
	}
	sleep := d.sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	const interval = 15 * time.Second
	deadline := time.Now().Add(limit)
	for {
		out, err := d.cf.GetDistribution(ctx, &cloudfront.GetDistributionInput{Id: aws.String(id)})
		if err != nil {
			return fmt.Errorf("get distribution: %w", err)
		}
		if !strings.EqualFold(aws.ToString(out.Distribution.Status), "InProgress") {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("distribution %s still deploying after %s (CloudFront propagates to every edge; "+
				"re-run to continue, or pass --no-wait on deploy)", id, limit)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		sleep(interval)
	}
}

// find locates the distribution by its comment, returning id and domain.
//
// Distributions have no name; the comment is the only caller-chosen field that comes
// back on a list. As with the HTTP API, a duplicate is reported rather than guessed
// between — converging a different distribution than the last run did would silently
// repoint the bucket policy.
func (d *distribution) find(ctx context.Context) (id, domain string, err error) {
	var (
		marker  *string
		matches []cftypes.DistributionSummary
	)
	for {
		out, err := d.cf.ListDistributions(ctx, &cloudfront.ListDistributionsInput{Marker: marker})
		if err != nil {
			return "", "", fmt.Errorf("list distributions: %w", err)
		}
		if out.DistributionList == nil {
			return "", "", nil
		}
		for _, s := range out.DistributionList.Items {
			if aws.ToString(s.Comment) == distributionComment {
				matches = append(matches, s)
			}
		}
		if !aws.ToBool(out.DistributionList.IsTruncated) {
			break
		}
		marker = out.DistributionList.NextMarker
	}
	switch len(matches) {
	case 0:
		return "", "", nil
	case 1:
		return aws.ToString(matches[0].Id), aws.ToString(matches[0].DomainName), nil
	default:
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, aws.ToString(m.Id))
		}
		return "", "", fmt.Errorf("%d CloudFront distributions carry the comment %q (%v) — "+
			"delete the extras so a deploy converges a single distribution", len(matches), distributionComment, ids)
	}
}

func cfTags(m map[string]string) []cftypes.Tag {
	out := make([]cftypes.Tag, 0, len(m))
	for k, v := range m {
		out = append(out, cftypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out
}

// forayDistribution builds the CDN resource. apiHost is resolved lazily because the
// API's id — and therefore its endpoint — is generated at apply time.
func forayDistribution(cf cloudfrontAPI, s3c s3PolicyAPI, apigw apigwAPI, cfg Config) *distribution {
	return &distribution{
		cf:  cf,
		s3:  s3c,
		cfg: cfg,
		apiHost: func(ctx context.Context) (string, error) {
			h := &httpAPI{api: apigw, apiName: APIName}
			id, err := h.find(ctx)
			if err != nil {
				return "", err
			}
			if id == "" {
				return "", errors.New("the HTTP API does not exist yet — deploy creates it before the distribution")
			}
			return apiEndpointHost(cfg.Region, id), nil
		},
	}
}

// apiEndpointHost is the API's default endpoint host, without a scheme. Constructed
// rather than read back, matching the rest of this package.
func apiEndpointHost(region, apiID string) string {
	return fmt.Sprintf("%s.execute-api.%s.amazonaws.com", apiID, region)
}
