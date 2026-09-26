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
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
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
// So the ARN is resolved at deploy time from the layer's own published versions.
// `--lwa-layer-arn` still pins it explicitly for an air-gapped account, a private
// mirror of the layer, or to hold a known-good version.
const (
	// lwaPublisherAccount is the AWS-owned account that publishes the public LWA
	// layers. Stable and documented at
	// https://github.com/awslabs/aws-lambda-web-adapter.
	lwaPublisherAccount = "753240598075"
	// lwaLayerNameArm64 matches the architecture the Lambdas are built for
	// (provided.al2023/arm64). An x86 layer on an arm64 function fails at runtime,
	// not at deploy time, so the two must not drift apart.
	lwaLayerNameArm64 = "LambdaAdapterLayerArm64"
)

// lwaLayerBase is the versionless layer ARN — what ListLayerVersions takes as its
// LayerName (the API accepts a full ARN there, which is how a layer in another
// account can be enumerated at all).
func lwaLayerBase(region string) string {
	return fmt.Sprintf("arn:%s:lambda:%s:%s:layer:%s", partition, region, lwaPublisherAccount, lwaLayerNameArm64)
}

// layerLister is the one Lambda call layer resolution needs.
type layerLister interface {
	ListLayerVersions(ctx context.Context, in *lambda.ListLayerVersionsInput, opts ...func(*lambda.Options)) (*lambda.ListLayerVersionsOutput, error)
}

// resolveLWALayerARN returns the newest published LWA layer version for the region.
//
// Newest rather than pinned-by-default is the right trade here: the layer is a thin
// adapter maintained by AWS, the alternative is a stale hardcoded version that
// breaks silently when a region lags, and `--lwa-layer-arn` exists for anyone who
// wants to hold a specific one. The error says what to do, because this is the
// failure a deployer in an unusual account will hit.
func resolveLWALayerARN(ctx context.Context, api layerLister, region string) (string, error) {
	base := lwaLayerBase(region)
	out, err := api.ListLayerVersions(ctx, &lambda.ListLayerVersionsInput{
		LayerName: aws.String(base),
		MaxItems:  aws.Int32(1), // versions come back newest-first
	})
	if err != nil {
		return "", fmt.Errorf("resolve the Lambda Web Adapter layer in %s (%s): %w\n"+
			"  pass --lwa-layer-arn to pin it yourself; see https://github.com/awslabs/aws-lambda-web-adapter",
			region, base, err)
	}
	if len(out.LayerVersions) == 0 {
		return "", fmt.Errorf("no Lambda Web Adapter layer versions published in %s (%s)\n"+
			"  pass --lwa-layer-arn to point at a layer you control",
			region, base)
	}
	arn := aws.ToString(out.LayerVersions[0].LayerVersionArn)
	if arn == "" {
		return "", fmt.Errorf("the Lambda Web Adapter layer in %s reported no version ARN", region)
	}
	return arn, nil
}
