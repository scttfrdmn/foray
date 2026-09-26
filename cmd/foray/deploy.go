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

package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/config"

	"github.com/scttfrdmn/foray/internal/deploy"
	"github.com/scttfrdmn/foray/internal/spore"
)

// deployCmd provisions the control plane (issue #85). This is the primary
// deployment path; deploy/terraform/ remains the declarative alternate.
func deployCmd(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	var (
		region     = fs.String("region", os.Getenv("AWS_REGION"), "region for the control plane (default $AWS_REGION, else the AWS config chain)")
		webBucket  = fs.String("web-bucket", os.Getenv("FORAY_WEB_BUCKET"), "globally-unique S3 bucket for the SPA (default $FORAY_WEB_BUCKET)")
		dataBucket = fs.String("data-bucket", os.Getenv("FORAY_DATA_BUCKET"), "globally-unique S3 bucket for in-region saves (default $FORAY_DATA_BUCKET)")
		table      = fs.String("table", deploy.DefaultSessionsTable, "DynamoDB table for sessions + cost receipts")
		planModel  = fs.String("plan-model", envOr("FORAY_PLAN_MODEL", deploy.DefaultPlanModelID), "Bedrock inference profile the brain plans with (scopes the web API's IAM policy)")
		dryRun     = fs.Bool("dry-run", false, "print what would be created, call no mutating API")
	)
	_ = fs.Parse(args)

	d, cfg, err := buildDeployer(ctx, deploy.Config{
		Region:        *region,
		WebBucket:     *webBucket,
		DataBucket:    *dataBucket,
		SessionsTable: *table,
		PlanModelID:   *planModel,
	})
	if err != nil {
		die(err)
	}
	d.DryRun = *dryRun

	verb := "deploying"
	if *dryRun {
		verb = "planning"
	}
	fmt.Printf("\n  %s the foray control plane in %s\n", verb, cfg.Region)
	fmt.Printf("  web bucket: %s\n  data bucket: %s\n  sessions table: %s\n\n", cfg.WebBucket, cfg.DataBucket, cfg.SessionsTable)

	actions, err := d.Apply(ctx)
	printActions(actions)
	if err != nil {
		// Apply returns what it managed to do alongside the error. Say so: there is
		// no state file, so what already exists is the user's next problem, and
		// re-running is how it gets fixed.
		fmt.Fprintf(os.Stderr, "\n  deploy stopped: %v\n", err)
		fmt.Fprintf(os.Stderr, "  the resources listed above already exist; `foray deploy` again to continue.\n\n")
		os.Exit(1)
	}
	if *dryRun {
		fmt.Printf("\n  plan only — nothing was created. Drop --dry-run to apply.\n\n")
		return
	}
	fmt.Printf("\n  control plane up. `foray teardown` removes it.\n\n")
}

// teardownCmd removes the control plane. Destructive and confirmed, because the
// data bucket holds the user's saved activations.
func teardownCmd(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("teardown", flag.ExitOnError)
	var (
		region     = fs.String("region", os.Getenv("AWS_REGION"), "region the control plane is in")
		webBucket  = fs.String("web-bucket", os.Getenv("FORAY_WEB_BUCKET"), "the SPA bucket to remove")
		dataBucket = fs.String("data-bucket", os.Getenv("FORAY_DATA_BUCKET"), "the saves bucket to remove")
		table      = fs.String("table", deploy.DefaultSessionsTable, "the sessions table to remove")
		force      = fs.Bool("force", false, "skip the confirmation prompt")
		dryRun     = fs.Bool("dry-run", false, "print what would be removed, call no mutating API")
	)
	_ = fs.Parse(args)

	d, cfg, err := buildDeployer(ctx, deploy.Config{
		Region:        *region,
		WebBucket:     *webBucket,
		DataBucket:    *dataBucket,
		SessionsTable: *table,
	})
	if err != nil {
		die(err)
	}
	d.DryRun = *dryRun

	if !*dryRun && !*force {
		// Name what is irreversible. Emptying the data bucket destroys saved
		// activations the user cannot regenerate without re-running an experiment —
		// that is the one deletion here worth a deliberate yes.
		fmt.Printf("\n  this removes the foray control plane in %s:\n", cfg.Region)
		fmt.Printf("    - DynamoDB table %s (sessions + cost receipts)\n", cfg.SessionsTable)
		fmt.Printf("    - S3 bucket %s (the SPA — re-synced by the next deploy)\n", cfg.WebBucket)
		fmt.Printf("    - S3 bucket %s — INCLUDING every saved activation and export in it\n", cfg.DataBucket)
		fmt.Printf("\n  export anything you want to keep first (foray export <session>).\n")
		if !confirm("  proceed?") {
			fmt.Println("  left alone.")
			return
		}
	}

	actions, err := d.Teardown(ctx)
	printActions(actions)
	if err != nil {
		// Teardown does not stop at the first failure, so this is a summary of
		// everything that would not go. Point at the independent check.
		fmt.Fprintf(os.Stderr, "\n  teardown incomplete: %v\n", err)
		fmt.Fprintf(os.Stderr, "  run `make teardown-verify` to see what is still billing.\n\n")
		os.Exit(1)
	}
	if *dryRun {
		fmt.Printf("\n  plan only — nothing was removed.\n\n")
		return
	}
	fmt.Printf("\n  control plane down. `make teardown-verify` confirms nothing is left billing.\n\n")
}

// buildDeployer wires the fake or real deployer depending on FORAY_FAKE, mirroring
// buildDeps. The fake path walks the real resource list and ordering with no AWS
// account, which is what makes the deploy verb rehearsable offline.
func buildDeployer(ctx context.Context, cfg deploy.Config) (*deploy.Deployer, deploy.Config, error) {
	if spore.Enabled() {
		// Keep the rehearsal turnkey: no env, no flags, still a coherent walk.
		if cfg.Region == "" {
			cfg.Region = "us-east-1"
		}
		if cfg.WebBucket == "" {
			cfg.WebBucket = "foray-web-fake"
		}
		if cfg.DataBucket == "" {
			cfg.DataBucket = "foray-data-fake"
		}
		d, err := deploy.NewFake(cfg)
		if err != nil {
			return nil, cfg, err
		}
		return d, d.Config(), nil
	}

	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, cfg, fmt.Errorf("load AWS config (set AWS_PROFILE / credentials): %w", err)
	}
	// Fall back to the region the AWS config chain resolved, so --region is an
	// override rather than a requirement.
	if cfg.Region == "" {
		cfg.Region = awsCfg.Region
	}
	d, err := deploy.New(ctx, cfg, awsCfg)
	if err != nil {
		return nil, cfg, err
	}
	return d, d.Config(), nil
}

// printActions renders the deploy report.
func printActions(actions []deploy.Action) {
	for _, a := range actions {
		fmt.Printf("  %s\n", a)
	}
}
