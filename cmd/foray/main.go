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

// Command foray is the CLI on-ramp: the whole question -> propose -> Go -> run ->
// assess -> climb loop as a pipeable command, plus expert flags and an export
// verb. Under FORAY_FAKE=1 it walks the full loop with no AWS calls (the CI gate).
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"

	"github.com/scttfrdmn/foray/internal/brain"
	"github.com/scttfrdmn/foray/internal/export"
	"github.com/scttfrdmn/foray/internal/spore"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx := context.Background()
	switch os.Args[1] {
	case "run":
		runCmd(ctx, os.Args[2:])
	case "export":
		exportCmd(ctx, os.Args[2:])
	case "models":
		fmt.Println("TODO(claude-code): list resolvable sources (hf / s3:// / upload)")
	case "sessions":
		fmt.Println("TODO(claude-code): list running sessions: age, TTL, $-so-far")
	case "stop":
		fmt.Println("TODO(claude-code): stop a session (or let idle reap it)")
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "foray: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func runCmd(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var (
		model     = fs.String("model", "", "model source: HF id, s3:// URI, or upload ref (expert path)")
		technique = fs.String("technique", "", "logit-lens | patchscope | steering | sae")
		engine    = fs.String("engine", "", "eager | vllm (auto if empty)")
		hardware  = fs.String("hardware", "", "override instance type (else brain picks)")
		budget    = fs.Float64("budget", 0, "per-session budget ceiling in USD")
		yes       = fs.Bool("yes", false, "approve every rung without prompting")
	)
	_ = fs.Parse(args)
	question := fs.Arg(0)
	_, _, _, _ = model, technique, engine, hardware // expert flags: full wiring is step 7

	fake := os.Getenv("FORAY_FAKE") == "1"
	if !fake {
		runReal(ctx, question, *budget, *yes)
		return
	}

	b := brain.NewFake()
	ladder, prop, err := b.Propose(ctx, question)
	if err != nil {
		die(err)
	}
	if prop != nil && prop.Clarify != "" {
		fmt.Printf("\n  foray needs to know first: %s\n\n", prop.Clarify)
		return
	}

	fmt.Printf("\n  question: %s\n", question)
	fmt.Printf("  budget for this question: $%.2f\n", ladder.Question.BudgetUSD)

	// In the fake, every rung is auto-approved so the CI gate walks the whole
	// loop unattended. The real path always asks for an explicit Go.
	for prop != nil {
		printProposal(prop)
		fmt.Println("  Go (auto)")

		sid, err := b.Approve(ctx, ladder, prop)
		if err != nil {
			die(err)
		}
		res := brain.FakeResult(sid, prop.Rung.Index)
		fmt.Printf("  ↳ %s\n", res.Finding)
		fmt.Printf("    saves: %s   (download: foray export %s)\n", res.VizRef, sid)

		rec, err := b.Assess(ctx, ladder, res)
		if err != nil {
			die(err)
		}
		fmt.Printf("  assessment: %s — %s\n", rec.Decision, rec.Reason)
		if rec.Decision != brain.Climb {
			break
		}
		prop = b.NextProposal(ctx, ladder)
	}

	fmt.Printf("\n  receipt: %d rung(s) run · $%.2f of $%.2f spent on this question\n\n",
		ladder.Cursor, ladder.Spent, ladder.Question.BudgetUSD)
}

// runReal walks the real brain: Bedrock plans the ladder, Cedar gates each rung,
// the human's Go launches it via spawn. Streaming the trace and fetching results
// runs through forayd + the worker; wiring that into the CLI is step 7, so this
// path plans, gates, and launches the first rung, then reports the live session.
func runReal(ctx context.Context, question string, budgetUSD float64, yes bool) {
	b, err := buildRealBrain(budgetUSD)
	if err != nil {
		die(err)
	}
	ladder, prop, err := b.Propose(ctx, question)
	if err != nil {
		die(err)
	}
	if prop != nil && prop.Clarify != "" {
		fmt.Printf("\n  foray needs to know first: %s\n\n", prop.Clarify)
		return
	}

	fmt.Printf("\n  question: %s\n", question)
	fmt.Printf("  budget for this question: $%.2f\n", ladder.Question.BudgetUSD)

	printProposal(prop)
	if !yes && !confirm("  Go?") {
		fmt.Println("  stopped.")
		return
	}
	sid, err := b.Approve(ctx, ladder, prop)
	if err != nil {
		die(err) // Cedar denials surface here with the policy reason verbatim.
	}
	fmt.Printf("  Go — launched session %s on %s\n", sid, prop.Rung.Chosen.InstanceType)
	fmt.Printf("  the worker streams the trace via forayd; result-driven climbing lands with the gateway-wired CLI (step 7).\n")
	fmt.Printf("\n  receipt: 1 rung launched · ~$%.2f of $%.2f budgeted for this question\n\n",
		prop.Rung.EstCostUSD, ladder.Question.BudgetUSD)
}

// buildRealBrain wires the real brain from AWS config + the spore CLIs. The
// planning model is a US inference profile id (FORAY_PLAN_MODEL, defaulting to a
// current Claude profile); the Cedar principal's budget/tier opt-ins come from
// the environment. Credentials and region resolve via the standard AWS chain.
func buildRealBrain(budgetUSD float64) (*brain.Brain, error) {
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS config (set AWS_PROFILE / credentials): %w", err)
	}
	modelID := envOr("FORAY_PLAN_MODEL", "us.anthropic.claude-sonnet-4-6")
	invoker := brain.NewBedrockInvoker(bedrockruntime.NewFromConfig(cfg), modelID)

	runner := spore.NewExecRunner()
	principal := brain.Principal{
		Subject:          envOr("FORAY_USER", "foray-user"),
		BudgetCeilingUSD: envFloat("FORAY_BUDGET_CEILING", 5.00),
		AllowedTiers:     []string{"slice", "small", "mid"}, // "large" requires explicit opt-in
		AllowLargeSaves:  os.Getenv("FORAY_ALLOW_LARGE_SAVES") == "1",
		AllowExport:      os.Getenv("FORAY_DENY_EXPORT") != "1",
	}
	if os.Getenv("FORAY_ALLOW_LARGE_TIER") == "1" {
		principal.AllowedTiers = append(principal.AllowedTiers, "large")
	}
	return brain.NewReal(brain.Config{
		Invoker:   invoker,
		Truffle:   spore.NewTruffle(runner),
		Spawn:     spore.NewSpawn(runner),
		Principal: principal,
		BudgetUSD: budgetUSD,
		Region:    cfg.Region,
		Spot:      true,
	})
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

func printProposal(p *brain.Proposal) {
	r := p.Rung
	hw := "—"
	if r.Chosen.InstanceType != "" {
		hw = fmt.Sprintf("%s (%s, %dGB)", r.Chosen.InstanceType, r.Chosen.GPU, r.Chosen.GPUMemGB)
	}
	fmt.Printf("\n  ── rung %d ─────────────────────────────────\n", r.Index)
	fmt.Printf("  model:     %s\n", r.Model.Name)
	fmt.Printf("  technique: %s   engine: %s\n", r.Technique, r.Engine)
	fmt.Printf("  hardware:  %s\n", hw)
	fmt.Printf("  cost:      ~$%.2f / session\n", r.EstCostUSD)
	fmt.Printf("  why:       %s\n", r.Rationale)
	if r.NNSight != "" {
		fmt.Printf("  nnsight:\n")
		for _, line := range strings.Split(r.NNSight, "\n") {
			fmt.Printf("    %s\n", line)
		}
	}
}

func exportCmd(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	kind := fs.String("kind", "bundle", "activations | outputs | bundle")
	_ = fs.Parse(args)
	session := fs.Arg(0)
	if session == "" {
		fmt.Fprintln(os.Stderr, "usage: foray export <session> [--kind bundle|activations|outputs]")
		os.Exit(2)
	}
	if os.Getenv("FORAY_FAKE") != "1" {
		fmt.Println("foray: export presigner not wired yet — run with FORAY_FAKE=1 for a stub.")
		return
	}
	ex := export.NewFake()
	link, err := ex.Export(ctx, export.Request{SessionID: session, Kind: export.Kind(*kind)})
	if err != nil {
		die(err)
	}
	fmt.Printf("\n  download (%s), expires %s:\n  %s\n\n",
		link.Kind, link.ExpiresAt.Format("15:04 MST"), link.URL)
}

func confirm(prompt string) bool {
	fmt.Printf("%s [Y/n] ", prompt)
	s, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	s = strings.TrimSpace(strings.ToLower(s))
	return s == "" || s == "y" || s == "yes"
}

func die(err error) {
	fmt.Fprintf(os.Stderr, "foray: %v\n", err)
	os.Exit(1)
}

func usage() {
	fmt.Fprint(os.Stderr, `foray — ephemeral deep inference (ADI)

usage:
  foray run "<question>"          propose a ladder, approve, run, climb
  foray run --model ... ...       expert path: skip the dialog, every knob
  foray export <session>          download your own saved activations/outputs
  foray models                    resolvable model sources
  foray sessions                  running sessions: age, TTL, $-so-far
  foray stop <session>            stop a session

env:
  FORAY_FAKE=1                    walk the whole loop with no AWS calls
`)
}
