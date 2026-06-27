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
	"strings"

	"github.com/scttfrdmn/foray/internal/brain"
	"github.com/scttfrdmn/foray/internal/export"
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
	_, _, _, _, _ = model, technique, engine, hardware, budget // expert flags: TODO real wiring

	fake := os.Getenv("FORAY_FAKE") == "1"
	if !fake {
		fmt.Println("foray: real collaborators (AgentCore / Cedar / spawn) not wired yet.")
		fmt.Println("       run with FORAY_FAKE=1 to walk the loop offline. See CLAUDE.md.")
		return
	}
	auto := fake || *yes
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

	for prop != nil {
		printProposal(prop)
		if !auto && !confirm("  Go?") {
			fmt.Println("  stopped.")
			break
		}
		if auto {
			fmt.Println("  Go (auto)")
		}

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
