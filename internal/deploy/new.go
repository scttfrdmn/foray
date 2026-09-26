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
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// New builds a Deployer over real AWS clients.
//
// It resolves the account id up front because the IAM policies are written from it
// (Bedrock profile, EC2 instance and PassRole ARNs). That is also a useful early
// credential check: a deploy with no valid credentials fails here, before creating
// anything, rather than halfway through.
func New(ctx context.Context, cfg Config, awsCfg aws.Config) (*Deployer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	ident, err := sts.NewFromConfig(awsCfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, fmt.Errorf("resolve AWS account (check credentials): %w", err)
	}
	cfg.AccountID = aws.ToString(ident.Account)
	return newWith(cfg,
		dynamodb.NewFromConfig(awsCfg),
		s3.NewFromConfig(awsCfg),
		iam.NewFromConfig(awsCfg),
	), nil
}

// newWith assembles the resource list over injected clients. Separate from New so
// tests build the same Deployer — same ordering, same idempotency rules — with no
// AWS and no credentials.
//
// Order is dependency order: Apply walks it forward, Teardown backward.
//
//  1. Storage and session state: nothing else needs them to exist, and the IAM
//     policies below are written against their ARNs.
//  2. IAM roles, then the instance profile that wraps one of them. The Lambdas and
//     API of later increments assume these roles, so they come first.
//
// The IAM policies reference the table and bucket by *constructed* ARN rather than
// by reading the created resources, so ordering here is about what must exist
// before something can be used — not about what must exist before a policy can be
// written.
func newWith(cfg Config, ddb dynamoAPI, s3c s3DeployAPI, iamc iamAPI) *Deployer {
	tableARN := sessionsTableARN(cfg.Region, cfg.AccountID, cfg.SessionsTable)
	return &Deployer{
		cfg: cfg,
		resources: []resource{
			&sessionsTable{api: ddb, table: cfg.SessionsTable},
			&bucket{
				api:        s3c,
				bucketName: cfg.WebBucket,
				region:     cfg.Region,
				// The SPA is re-synced on every deploy, so nothing here is the user's
				// only copy — emptying it on teardown is safe.
				emptyOnRemove: true,
			},
			&bucket{
				api:        s3c,
				bucketName: cfg.DataBucket,
				region:     cfg.Region,
				// Only the data bucket expires export bundles.
				lifecycleBundles: true,
				// This bucket holds the user's saved activations. Emptying it on
				// teardown is still correct — `foray teardown` is an explicit request
				// to leave nothing billing, and the CLI confirms before calling it —
				// but it is the one deletion in this package that destroys data the
				// user cannot regenerate without re-running an experiment.
				emptyOnRemove: true,
			},
			gatewayRole(iamc, tableARN),
			webAPIRole(iamc, tableARN, cfg.DataBucket, cfg.AccountID, cfg.PlanModelID),
			spawnRole(iamc, cfg.DataBucket, cfg.AccountID),
			// Last of the IAM set, so Teardown removes it first: an instance profile
			// holding a role blocks that role's deletion.
			&instanceProfile{api: iamc, profileName: RoleSpawnInstance, roleName: RoleSpawnInstance},
		},
	}
}
