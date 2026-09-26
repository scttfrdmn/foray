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
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// New builds a Deployer over real AWS clients.
//
// Resource order is dependency order: Apply walks it forward, Teardown backward.
// Storage and state come first because nothing else needs them to exist —
// the Lambdas, API and CDN that follow in later increments all do.
func New(cfg Config, awsCfg aws.Config) (*Deployer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	ddb := dynamodb.NewFromConfig(awsCfg)
	s3c := s3.NewFromConfig(awsCfg)
	return newWith(cfg, ddb, s3c), nil
}

// newWith assembles the resource list over injected clients. Separate from New so
// tests build the same Deployer — same ordering, same idempotency rules — with no
// AWS and no credentials.
func newWith(cfg Config, ddb dynamoAPI, s3c s3DeployAPI) *Deployer {
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
		},
	}
}
