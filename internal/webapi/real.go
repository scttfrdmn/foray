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

package webapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/scttfrdmn/foray/internal/brain"
	"github.com/scttfrdmn/foray/internal/export"
	"github.com/scttfrdmn/foray/internal/gateway"
	"github.com/scttfrdmn/foray/internal/spore"
)

// NewRealDeps wires the deployed collaborators (the API Gateway + Lambda surface):
// the real Bedrock+Cedar+spawn brain, a DynamoDB-backed gateway over the stdlib
// HTTP worker, the exec-backed spawn (so the brain's executor can launch and the
// gateway can resolve session→worker), and the real S3 export presigner. It is
// the cmd/foray buildRealDeps + buildExporter shape, projected onto webapi.Deps.
//
// Credentials and region resolve via the standard AWS chain; configuration comes
// from the environment Terraform injects (table, bucket, plan model). log may be
// nil.
//
// Note (documented follow-on): in a Lambda runtime the `spawn` binary the spore
// adapters shell out to is not present, so the launch path here assumes spawn is
// reachable (bundled into the image, or the brain runs where spawn exists). The
// gateway's trace-only sibling (cmd/forayd) avoids this entirely via the
// DynamoDB idle-bridge; wiring the full loop's launch path for Lambda is the
// VPC/worker-reachability follow-on (ARCHITECTURE.md §6.1, plan step 9 risks).
func NewRealDeps(ctx context.Context, log *slog.Logger) (Deps, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return Deps{}, fmt.Errorf("load AWS config (set AWS_PROFILE / credentials): %w", err)
	}

	modelID := envOr("FORAY_PLAN_MODEL", "us.anthropic.claude-sonnet-4-6")
	invoker := brain.NewBedrockInvoker(bedrockruntime.NewFromConfig(cfg), modelID)

	runner := spore.NewExecRunner()
	truffle := spore.NewTruffle(runner)
	spawn := spore.NewSpawn(runner)
	principal := buildPrincipal()

	b, err := brain.NewReal(brain.Config{
		Invoker:   invoker,
		Truffle:   truffle,
		Spawn:     spawn,
		Principal: principal,
		Region:    cfg.Region,
		Spot:      true,
	})
	if err != nil {
		return Deps{}, err
	}

	table := envOr("FORAY_SESSIONS_TABLE", "foray-sessions")
	gw := &gateway.Gateway{
		Store:  gateway.NewDynamoStore(dynamodb.NewFromConfig(cfg), table),
		Worker: gateway.HTTPWorker{Client: &http.Client{Timeout: 10 * time.Minute}},
		Spawn:  spawn,
	}

	bucket := os.Getenv("FORAY_DATA_BUCKET")
	if bucket == "" {
		return Deps{}, fmt.Errorf("set FORAY_DATA_BUCKET to the in-region saves bucket")
	}
	// A session is owned by this principal iff spawn knows it. The DynamoDB store
	// holds the durable mapping; ownership for export resolves through spawn's
	// view today, consistent with the CLI's buildExporter.
	owners := func(sid string) (string, bool) {
		if _, err := spawn.Status(ctx, sid); err != nil {
			return "", false
		}
		return principal.Subject, true
	}
	pol, err := brain.NewCedarExportPolicy(principal, owners)
	if err != nil {
		return Deps{}, err
	}
	exporter := &export.Exporter{
		Policy:    pol,
		Presigner: export.NewS3Presigner(s3.NewFromConfig(cfg), bucket, log),
	}

	return Deps{Brain: b, Gateway: gw, Spawn: spawn, Exporter: exporter}, nil
}

// buildPrincipal reads the Cedar principal's budget/tier opt-ins from the
// environment, mirroring cmd/foray.buildPrincipal so the CLI and the page enforce
// the same policy posture.
func buildPrincipal() brain.Principal {
	p := brain.Principal{
		Subject:          envOr("FORAY_USER", "foray-user"),
		BudgetCeilingUSD: envFloat("FORAY_BUDGET_CEILING", 5.00),
		AllowedTiers:     []string{"slice", "small", "mid"},
		AllowLargeSaves:  os.Getenv("FORAY_ALLOW_LARGE_SAVES") == "1",
		AllowExport:      os.Getenv("FORAY_DENY_EXPORT") != "1",
	}
	if os.Getenv("FORAY_ALLOW_LARGE_TIER") == "1" {
		p.AllowedTiers = append(p.AllowedTiers, "large")
	}
	return p
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
