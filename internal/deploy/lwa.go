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
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
)

// The AWS Lambda Web Adapter lets `cmd/forayd` and `cmd/foray-web` run on Lambda as
// **unmodified** `http.Server` binaries — no `aws-lambda-go`, no proxy shim, zero new
// Go dependencies. That property is worth keeping, so this package keeps the layer.
//
// The cost of keeping it is that the layer ARN is region-specific *and* versioned,
// and a wrong one is a **silent** failure: the readiness check never passes and API
// Gateway returns 503 with nothing in the application logs (the bug PR #64 spent
// real time on). Terraform made the deployer paste that ARN into a tfvars file,
// which is exactly the kind of friction `foray deploy` exists to remove.
//
// So the version is discovered at deploy time. `--lwa-layer-arn` still pins it
// explicitly for an air-gapped account, a private mirror of the layer, or to hold a
// known-good version.
const (
	// lwaPublisherAccount is the AWS-owned account that publishes the public LWA
	// layers. Stable and documented at
	// https://github.com/awslabs/aws-lambda-web-adapter.
	lwaPublisherAccount = "753240598075"
	// lwaLayerNameArm64 matches the architecture the Lambdas are built for
	// (provided.al2023/arm64). An x86 layer on an arm64 function fails at runtime,
	// not at deploy time, so the two must not drift apart.
	lwaLayerNameArm64 = "LambdaAdapterLayerArm64"

	// lwaProbeCeiling bounds the search. Layer versions are small integers that grow
	// slowly (30 in us-west-2 as of this writing), so this is generous; it exists so
	// a pathological account cannot turn resolution into an unbounded call loop.
	lwaProbeCeiling = 1024
)

// lwaLayerBase is the versionless layer ARN.
func lwaLayerBase(region string) string {
	return fmt.Sprintf("arn:%s:lambda:%s:%s:layer:%s", partition, region, lwaPublisherAccount, lwaLayerNameArm64)
}

// layerProbe is the one Lambda call layer resolution needs.
//
// Note it is GetLayerVersion, not ListLayerVersions. Enumerating another account's
// layer is **not permitted** even when the layer is public: the LWA layer's
// resource-based policy grants `lambda:GetLayerVersion` to everyone and nothing
// else, so ListLayerVersions returns AccessDenied no matter what the caller's own
// IAM allows. That was only discoverable against a real account — the first
// implementation here used ListLayerVersions and failed immediately on a live
// deploy.
type layerProbe interface {
	GetLayerVersion(ctx context.Context, in *lambda.GetLayerVersionInput, opts ...func(*lambda.Options)) (*lambda.GetLayerVersionOutput, error)
}

// resolveLWALayerARN finds the newest available LWA layer version for the region.
//
// Because versions cannot be enumerated (see layerProbe), it searches: probe upward
// by doubling until a version is unavailable, then binary-search the boundary. That
// is ~2·log₂(n) calls — around ten for a layer at version 30 — and is bounded by
// lwaProbeCeiling.
func resolveLWALayerARN(ctx context.Context, api layerProbe, region string) (string, error) {
	base := lwaLayerBase(region)

	// Version 1 has existed in every region the layer is published to, so its
	// absence means the layer is not available here at all — a different problem
	// from "which version", and worth a different message.
	ok, err := lwaVersionAvailable(ctx, api, base, 1)
	if err != nil {
		return "", fmt.Errorf("probe the Lambda Web Adapter layer in %s (%s): %w\n"+
			"  pass --lwa-layer-arn to pin it yourself; see https://github.com/awslabs/aws-lambda-web-adapter",
			region, base, err)
	}
	if !ok {
		return "", fmt.Errorf("the Lambda Web Adapter layer is not available in %s (%s)\n"+
			"  pass --lwa-layer-arn to point at a layer you control, or deploy to a region where it is published",
			region, base)
	}

	// Double until a version is unavailable: lo stays known-available, hi becomes
	// known-unavailable (or the ceiling).
	lo, hi := int32(1), int32(2)
	for hi <= lwaProbeCeiling {
		ok, err := lwaVersionAvailable(ctx, api, base, hi)
		if err != nil {
			return "", fmt.Errorf("probe %s:%d: %w", base, hi, err)
		}
		if !ok {
			break
		}
		lo = hi
		hi *= 2
	}
	if hi > lwaProbeCeiling {
		// Implausible in practice; refuse rather than return a version we never
		// confirmed is the newest.
		return "", fmt.Errorf("the Lambda Web Adapter layer in %s has more than %d versions — pass --lwa-layer-arn to pin one",
			region, lwaProbeCeiling)
	}

	// Binary-search the boundary between lo (available) and hi (not).
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		ok, err := lwaVersionAvailable(ctx, api, base, mid)
		if err != nil {
			return "", fmt.Errorf("probe %s:%d: %w", base, mid, err)
		}
		if ok {
			lo = mid
		} else {
			hi = mid
		}
	}
	return fmt.Sprintf("%s:%d", base, lo), nil
}

// lwaVersionAvailable reports whether this account can attach that layer version.
//
// "Available" and "exists" are the same question here, and both an absent version
// and a genuinely forbidden one answer **AccessDenied** — the resource-based policy
// is per-version, so a version that was never published has no policy granting
// anything, and AWS reports that as authorization failure rather than 404. Treating
// the two alike is therefore correct, not a shortcut.
//
// Every other error is returned: a throttle or network failure read as "absent"
// would silently resolve an older version, or none at all.
func lwaVersionAvailable(ctx context.Context, api layerProbe, base string, version int32) (bool, error) {
	_, err := api.GetLayerVersion(ctx, &lambda.GetLayerVersionInput{
		LayerName:     aws.String(base),
		VersionNumber: aws.Int64(int64(version)),
	})
	if err == nil {
		return true, nil
	}
	var notFound *lambdatypes.ResourceNotFoundException
	if errors.As(err, &notFound) {
		return false, nil
	}
	if isAccessDenied(err) {
		return false, nil
	}
	return false, err
}

// isAccessDenied reports whether an error is an authorization failure. Matched on the
// error code rather than a typed exception because Lambda returns
// AccessDeniedException, which the SDK does not model as a service-specific type.
func isAccessDenied(err error) bool {
	var apiErr interface{ ErrorCode() string }
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "AccessDeniedException", "AccessDenied":
			return true
		}
	}
	return false
}
