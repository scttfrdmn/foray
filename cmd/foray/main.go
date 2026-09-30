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

// Command foray is the CLI on-ramp (ARCHITECTURE.md §5): the whole
// question → propose → Go → run → assess → climb loop as a pipeable command,
// plus the expert path (skip the dialog, name model/technique/engine/hardware
// directly) and the export / models / sessions / stop verbs. Results are fetched
// through the gateway (forayd's library, hosted in-process here exactly as the
// future Lambda hosts it), so the human climbs the ladder rung by rung — each
// climb a fresh Go, never auto-climbed.
//
// Under FORAY_FAKE=1 the whole loop walks with no AWS calls (the CI gate,
// make demo-fake): a fake brain, a fake spawn, and the gateway's canned worker.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/scttfrdmn/foray/internal/brain"
	"github.com/scttfrdmn/foray/internal/catalog"
	"github.com/scttfrdmn/foray/internal/device"
	"github.com/scttfrdmn/foray/internal/export"
	"github.com/scttfrdmn/foray/internal/gateway"
	"github.com/scttfrdmn/foray/internal/sizing"
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
		modelsCmd(os.Args[2:])
	case "sessions":
		sessionsCmd(ctx, os.Args[2:])
	case "stop":
		stopCmd(ctx, os.Args[2:])
	case "deploy":
		deployCmd(ctx, os.Args[2:])
	case "teardown":
		teardownCmd(ctx, os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "foray: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

// parseWithPositionals parses flags that may appear *after* the positional
// argument, and returns the first positional.
//
// stdlib flag stops parsing at the first non-flag argument, so `foray run "why
// does it refuse X?" --yes` silently dropped every flag after the question — which
// is how a person naturally types it, how the README shows it, and how
// `make demo-fake` invokes it. The demo-fake gate was passing only because an
// unreadable stdin happened to read as approval, not because --yes took effect.
//
// The loop below is the canonical stdlib idiom: parse, take the positional it
// stopped on, parse the remainder, repeat. Cheaper than a dependency and it keeps
// flag's own error handling.
func parseWithPositionals(fs *flag.FlagSet, args []string) string {
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return "" // ExitOnError already reported and exited
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(positional) == 0 {
		return ""
	}
	return positional[0]
}

// runCmd plans (or builds, on the expert path) the ladder and walks the loop. The
// same loop serves the fake and real paths — only how the collaborators are wired
// differs (buildDeps).
func runCmd(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var (
		model     = fs.String("model", "", "model source: HF id, s3:// URI, or upload ref (expert path; skips the dialog)")
		technique = fs.String("technique", "", "logit-lens | attribution | steering | sae | generate")
		engine    = fs.String("engine", "", "eager | vllm (auto if empty)")
		hardware  = fs.String("hardware", "", "override instance type, e.g. g7e.xlarge (else the smallest tier)")
		budget    = fs.Float64("budget", 0, "per-question budget envelope in USD (the ladder is capped here)")
		yes       = fs.Bool("yes", false, "approve every rung without prompting (pre-authorizes the whole climb)")
		keep      = fs.Bool("keep", false, "leave each rung's instance stopped instead of terminated, so its disk can be inspected (its EBS bills until TTL terminates it)")
	)
	question := parseWithPositionals(fs, args)

	d, err := buildDeps(depsOpts{budgetUSD: *budget, keep: *keep, dataPlane: true})
	if err != nil {
		die(err)
	}

	var (
		ladder *brain.Ladder
		prop   *brain.Proposal
	)
	if *model != "" {
		// Expert on-ramp (§5 on-ramp 3): the user named the knobs; skip the brain's
		// planning dialog and build exactly the one rung they asked for.
		ladder, err = buildExpertLadder(ctx, d, expertFlags{
			model: *model, technique: *technique, engine: *engine, hardware: *hardware, question: question,
		}, *budget)
		if err != nil {
			die(err)
		}
		prop = &brain.Proposal{Rung: &ladder.Rungs[0]}
	} else {
		if strings.TrimSpace(question) == "" {
			die(errors.New(`run needs a question, e.g. foray run "why does it refuse X?" (or use --model for the expert path)`))
		}
		ladder, prop, err = d.brain.Propose(ctx, question)
		if err != nil {
			die(err)
		}
		// A clarifying question short-circuits: naming a model is the wrong first move.
		if prop != nil && prop.Clarify != "" {
			fmt.Printf("\n  foray needs to know first: %s\n\n", prop.Clarify)
			return
		}
	}
	runLoop(ctx, d, ladder, prop, *yes, *keep)
}

// runLoop is the result-gated ladder, shared by the fake and real paths:
// propose → (human Go) → Approve (Cedar gate) → register+trace through the
// gateway → Interpret → Assess → climb only on a fresh Go. The brain proposes
// and interprets; only Approve launches; climbing is never automatic and stops
// on an honest negative (CLAUDE.md invariants).
func runLoop(ctx context.Context, d *deps, ladder *brain.Ladder, prop *brain.Proposal, yes, keep bool) {
	fmt.Printf("\n  question: %s\n", ladder.Question.Text)
	fmt.Printf("  budget for this question: $%.2f\n", ladder.Question.BudgetUSD)

	for prop != nil {
		printProposal(prop)
		if !approve(yes) {
			fmt.Println("  stopped.")
			break
		}

		// The session's identity comes first: the graph is staged into the task from
		// sessions/<id>/, so the id has to exist before the instance does (#103).
		sid := brain.NewSessionID(prop.Rung)
		if err := d.collector.handOff(ctx, sid, prop.Rung); err != nil {
			die(err)
		}

		// Approve is the sole acceptance node: it runs Cedar, then launches.
		if err := d.brain.Approve(ctx, ladder, prop, sid); err != nil {
			die(err) // Cedar denials surface here with the policy reason verbatim.
		}
		fmt.Printf("  Go — launched session %s on %s\n", sid, prop.Rung.Chosen.InstanceType)

		tr, err := runRung(ctx, d, sid, keep)
		if err != nil {
			die(err)
		}

		res, err := d.brain.Interpret(ctx, ladder, prop.Rung, brain.RawResult{
			SaveRef: tr.SaveRef, VizRef: tr.VizRef, NNSight: tr.NNSight,
		})
		if err != nil {
			die(err)
		}
		fmt.Printf("  ↳ %s\n", res.Finding)
		fmt.Printf("    saves: %s   (download: foray export %s)\n", tr.SaveRef, sid)

		rec, err := d.brain.Assess(ctx, ladder, res)
		if err != nil {
			die(err)
		}
		fmt.Printf("  assessment: %s — %s\n", rec.Decision, rec.Reason)
		if rec.Decision != brain.Climb {
			break
		}
		// The brain recommends climbing — but the next rung is a fresh proposal
		// awaiting its own Go. NextProposal never launches; only the next Approve does.
		prop = d.brain.NextProposal(ctx, ladder)
		if prop != nil {
			fmt.Printf("\n  the brain recommends climbing; the next rung needs a fresh Go.\n")
		}
	}

	fmt.Printf("\n  receipt: %d rung(s) run · $%.2f of $%.2f spent on this question\n\n",
		ladder.Cursor, ladder.Spent, ladder.Question.BudgetUSD)
}

// approve is the HITL gate. --yes pre-authorizes the whole climb (the demo-fake
// path); otherwise every rung — first or climbed — prompts for its own Go.
func approve(yes bool) bool {
	if yes {
		fmt.Println("  Go (auto)")
		return true
	}
	return confirm("  Go?")
}

// expertFlags carries the parsed expert knobs into the ladder builder.
type expertFlags struct {
	model, technique, engine, hardware, question string
}

// buildExpertLadder turns the expert flags into a one-rung ladder: resolve the
// model source (verbatim ErrUnsupportedSource on bad input), resolve the hardware
// (or default to the smallest enabled tier), price it via truffle, and hand it to
// brain.ExpertLadder. No Bedrock — the user already decided.
func buildExpertLadder(ctx context.Context, d *deps, f expertFlags, budgetUSD float64) (*brain.Ladder, error) {
	src, err := catalog.Parse(f.model)
	if err != nil {
		return nil, err // wraps catalog.ErrUnsupportedSource with a verbatim reason
	}

	var hw device.Option
	if f.hardware != "" {
		opt, ok := device.ByInstanceType(f.hardware)
		if !ok {
			return nil, fmt.Errorf("unknown hardware %q (not an enabled tier; see the device menu)", f.hardware)
		}
		hw = opt
	} else {
		// No override: default to the smallest enabled tier. Precise sizing of an
		// arbitrary model is the planner's job, so say so rather than guess big.
		opts := device.Options(1)
		if len(opts) == 0 {
			return nil, errors.New("no enabled hardware tiers available")
		}
		hw = opts[0]
		fmt.Printf("  note: no --hardware given; defaulting to %s (%s). Precise auto-sizing of an\n", hw.InstanceType, hw.GPU)
		fmt.Printf("        arbitrary model is the planner's job — drop --model to use the dialog, or pass --hardware.\n")
	}

	pricer := brain.NewTrufflePricer(d.truffle, d.regionScope()...)
	return brain.ExpertLadder(ctx, brain.ExpertSpec{
		Question:    f.question,
		ModelSource: string(src.Kind),
		ModelRef:    src.String(),
		Technique:   f.technique,
		Engine:      sizing.Engine(f.engine),
		Instance:    hw,
	}, pricer, budgetUSD)
}

// modelsCmd lists the resolvable model-source kinds, or resolves and prints one.
// AWS-free: it is pure catalog parsing.
func modelsCmd(args []string) {
	if len(args) == 0 {
		fmt.Print(`
  foray resolves three model-source kinds (only format matters to the worker):

    hf      a HuggingFace repo id            gpt2  ·  meta-llama/Llama-3.1-8B@main
    s3      an s3:// URI you already hold    s3://my-bucket/checkpoints/model/
    upload  an opaque ref to a staged upload upload:ab12cd34

  resolve one:  foray models <source>

`)
		return
	}
	src, err := catalog.Parse(args[0])
	if err != nil {
		die(err) // ErrUnsupportedSource reason, verbatim
	}
	fmt.Printf("\n  %s\n    kind: %s\n    resolved: %s\n\n", args[0], src.Kind, src.String())
}

// sessionsCmd lists running foray sessions with age, TTL, and $-so-far. In fake
// mode each invocation is a fresh process with no launched instances, so it
// honestly reports none.
func sessionsCmd(ctx context.Context, args []string) {
	_ = args
	d, err := buildDeps(depsOpts{})
	if err != nil {
		die(err)
	}
	insts, err := d.spawn.List(ctx)
	if err != nil {
		die(err)
	}
	if len(insts) == 0 {
		fmt.Print("\n  no running sessions.\n\n")
		return
	}
	fmt.Printf("\n  %-18s %-14s %-8s %-12s %s\n", "SESSION", "INSTANCE", "AGE", "TTL", "$-SO-FAR")
	for _, inst := range insts {
		age := time.Since(inst.LaunchedAt)
		ttl := "—"
		if d := inst.TTLDeadline(); !d.IsZero() {
			ttl = humaneDur(time.Until(d)) + " left"
		}
		fmt.Printf("  %-18s %-14s %-8s %-12s $%.2f\n",
			inst.ID, inst.InstanceType, humaneDur(age), ttl, sessionCostSoFar(ctx, d.truffle, inst))
	}
	fmt.Println()
}

// stopCmd terminates a session. The explicit invocation is the human's approval;
// we still confirm (unless --force) and echo what is being stopped.
func stopCmd(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	force := fs.Bool("force", false, "stop without confirming")
	_ = fs.Parse(args)
	sid := fs.Arg(0)
	if sid == "" {
		fmt.Fprintln(os.Stderr, "usage: foray stop <session> [--force]")
		os.Exit(2)
	}
	d, err := buildDeps(depsOpts{})
	if err != nil {
		die(err)
	}
	if !*force && !confirm(fmt.Sprintf("  stop session %s?", sid)) {
		// Be precise about which deadline does what: idle only *stops* the
		// instance (EBS keeps billing), TTL is what terminates it. Saying "idle
		// will reap it" would promise $0 that does not arrive until TTL.
		fmt.Println("  left running (idle will stop it; TTL terminates it).")
		return
	}
	if err := d.spawn.Terminate(ctx, sid); err != nil {
		die(err)
	}
	fmt.Printf("  stopped %s.\n", sid)
}

// exportCmd mints a presigned download of the user's own saved values. Export is
// opt-in egress of one's own data (ARCHITECTURE.md §6.9): the Cedar export gate
// runs for real (residency / ownership denials surface verbatim), and the S3
// presigner itself is a clearly-labeled stub until the deploy step (#25).
func exportCmd(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	kind := fs.String("kind", "bundle", "activations | outputs | bundle")
	_ = fs.Parse(args)
	session := fs.Arg(0)
	if session == "" {
		fmt.Fprintln(os.Stderr, "usage: foray export <session> [--kind bundle|activations|outputs]")
		os.Exit(2)
	}

	ex, err := buildExporter(ctx, session)
	if err != nil {
		die(err)
	}
	link, err := ex.Export(ctx, export.Request{SessionID: session, Kind: export.Kind(*kind)})
	if err != nil {
		die(err) // Cedar deny reason, or the stub presigner's "not wired" note, verbatim
	}
	fmt.Printf("\n  download (%s), expires %s:\n  %s\n\n",
		link.Kind, link.ExpiresAt.Format("15:04 MST"), link.URL)
}

// buildExporter wires the export path. Fake: the canned exporter. Real: the Cedar
// export policy (ownership resolved through spawn.Status) plus the stub presigner.
func buildExporter(ctx context.Context, session string) (*export.Exporter, error) {
	if spore.Enabled() {
		return export.NewFake(), nil
	}
	d, err := buildRealDeps(depsOpts{})
	if err != nil {
		return nil, err
	}
	bucket := os.Getenv("FORAY_DATA_BUCKET")
	if bucket == "" {
		return nil, errors.New("set FORAY_DATA_BUCKET to your in-region saves bucket to export")
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS config (set AWS_PROFILE / credentials): %w", err)
	}
	// Ownership comes from the session's saves being in the user's own bucket, not
	// from its instance still running. A finished session is exactly the one a user
	// wants to export from, and the run output tells them to (issue #80).
	s3c := s3.NewFromConfig(cfg)
	owner := export.NewSessionOwner(s3c, bucket, d.principal.Subject)
	// Distinguish "nothing saved here" from "not yours" — Cedar's deny reason can
	// only speak to the second, and the two need different things from the user.
	if ok, err := owner.Exists(ctx, session); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("no saved values found for session %s in s3://%s/%s", session, bucket, "sessions/"+session+"/")
	}
	pol, err := brain.NewCedarExportPolicy(d.principal, owner.OwnerFunc(ctx))
	if err != nil {
		return nil, err
	}
	presigner := export.NewS3Presigner(s3c, bucket, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	return &export.Exporter{Policy: pol, Presigner: presigner}, nil
}

// --- collaborators ----------------------------------------------------------

// deps bundles the collaborators a command needs, wired once per mode.
type deps struct {
	brain     *brain.Brain
	spawn     spore.Spawn
	truffle   spore.Truffle
	principal brain.Principal
	region    string
	collector *collector
}

// regionScope returns the truffle/pricing region scope, or nil to let truffle
// pick by availability/price.
func (d *deps) regionScope() []string {
	if d.region == "" {
		return nil
	}
	return []string{d.region}
}

// runRung waits for the approved rung's result, then makes sure nothing is left
// billing.
//
// It exists as a function rather than inline in runLoop so cleanup can be deferred.
// Cleanup is the whole point: the instance exists from the moment Approve returns, so
// *every* exit path from here — including a failed trace — has to reap it. runLoop
// reports errors through die(), which calls os.Exit and therefore runs no defers, so the
// cleanup has to finish before the error gets there.
func runRung(ctx context.Context, d *deps, sid string, keep bool) (gateway.TraceResult, error) {
	defer reap(ctx, d, sid, keep)
	return d.collector.await(ctx, sid)
}

// reap terminates the session's instance. It is now a backstop rather than the
// mechanism: the task's own `on_complete: terminate` fires on the completion signal —
// including for a failed task — so by the time a rung's result is in hand, spawn has
// usually already reaped the instance. This covers what escapes that: a task that never
// wrote a completion signal at all, and the caller who gave up waiting.
//
// It stays because spawn's *idle* timeout only stops an instance, and a stopped instance
// keeps billing its EBS volumes until TTL (issue #80). Terminating when the control plane
// knows the rung is done is still the only thing that reaches $0 promptly.
//
// A failed termination is reported, not fatal: the rung's result is already in hand, and
// TTL still bounds the instance. Telling the user which session to clean up by hand beats
// discarding their finding. An already-terminated instance lands here too, which is the
// common case — hence "could not" rather than an alarm.
func reap(ctx context.Context, d *deps, sid string, keep bool) {
	if keep {
		fmt.Printf("    session %s left stopped (--keep); its disk bills until TTL terminates it.\n", sid)
		fmt.Printf("    terminate it now with: foray stop %s\n", sid)
		return
	}
	if err := d.spawn.Terminate(ctx, sid); err != nil {
		fmt.Fprintf(os.Stderr,
			"  note: could not terminate %s (%v) — the task's on_complete and TTL both still reap it; `foray stop %s` to be sure\n",
			sid, err, sid)
	}
}

// collector is the CLI's half of the launch-time handoff: write the graph where the task
// will stage it from, then wait for the result the worker stages back (#66, #103).
//
// It is the same gateway code the deployed control plane runs — HandOff and Collect — so
// the CLI and the web app reach the worker the same way. The CLI used to be different: it
// held an SSH forward through `spawn service` and POSTed the graph to a loopback listener.
// That stopped being possible when the data plane moved to `spawn task run`, which starts
// the container itself from the spec's command; there is no longer a worker sitting there
// waiting to be handed a graph over HTTP.
type collector struct {
	gw *gateway.Gateway

	// poll is how often to ask for the result, and wait is how long to keep asking.
	// Both are generous: a cold GPU instance boots, pulls the worker image, and streams
	// model weights before the trace even starts.
	poll, wait time.Duration
}

// handOff records the session and writes its graph, in that order — Touch needs a row.
//
// The graph goes over *before* the launch, which is the inverse of the pre-#103 order and
// the reason it is now safe: `spawn task run` stages inputs before the container execs,
// so the object must already be there. It also means the worker never waits for its own
// input, and a graph that fails to write costs nothing because no instance exists yet.
func (c *collector) handOff(ctx context.Context, sid string, r *brain.Rung) error {
	if err := c.gw.Store.Put(ctx, gateway.Session{ID: sid, InstanceID: sid}); err != nil {
		return fmt.Errorf("register session %s: %w", sid, err)
	}
	if err := c.gw.HandOff(ctx, sid, gateway.Graph{
		Engine:  string(r.Engine),
		Payload: []byte(r.NNSight),
	}); err != nil {
		return fmt.Errorf("hand off the graph for %s: %w", sid, err)
	}
	return nil
}

// await polls until the worker's result appears, the task dies, or the wait runs out.
//
// Polling an object rather than waiting on a callback: the instance has no inbound path,
// and the result object appearing *is* the completion signal. Collect also consults the
// task's completion record, so a container that died before writing anything ends the
// wait with a reason instead of running it out.
//
// Giving up does not leave the instance running — runRung's deferred reap still fires, and
// the task's own TTL and on_complete bound it regardless.
func (c *collector) await(ctx context.Context, sid string) (gateway.TraceResult, error) {
	interval, deadline := c.poll, c.wait
	if interval <= 0 {
		interval = defaultPollInterval
	}
	if deadline <= 0 {
		deadline = defaultTraceWait
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		res, pending, err := c.gw.Collect(ctx, sid)
		if err != nil {
			return gateway.TraceResult{}, fmt.Errorf("trace session %s: %w", sid, err)
		}
		if !pending {
			return res, nil
		}
		select {
		case <-ctx.Done():
			return gateway.TraceResult{}, fmt.Errorf(
				"trace session %s: no result after %s (the instance is being reaped; "+
					"`foray export %s` if the worker wrote saves before it stopped)", sid, deadline, sid)
		case <-t.C:
		}
	}
}

// Poll cadence. Seconds of latency are irrelevant next to a GPU boot plus weight
// streaming, and each poll is one small GetObject.
const (
	defaultPollInterval = 10 * time.Second
	defaultTraceWait    = 45 * time.Minute
)

// buildDeps wires the fake or real collaborators depending on FORAY_FAKE.
func buildDeps(o depsOpts) (*deps, error) {
	if spore.Enabled() {
		return buildFakeDeps(o)
	}
	return buildRealDeps(o)
}

// depsOpts is what a command wants from its collaborators. dataPlane separates the
// commands that launch and collect (run) from the ones that only read or stop what is
// already there (sessions, stop, export) — the latter must not demand a bucket or a
// worker image they never touch.
type depsOpts struct {
	budgetUSD float64
	keep      bool // --keep: stop rather than terminate at end of rung
	dataPlane bool // this command runs a trace, so the handoff must be wired
}

// buildFakeDeps wires the offline path: a fake brain, a shared fake spawn (so the
// brain's executor and the gateway's idle bridge see the same instance table),
// and the gateway's canned worker. Zero AWS — the dev/rehearse path and CI gate.
func buildFakeDeps(_ depsOpts) (*deps, error) {
	f := spore.NewFake()
	b := brain.NewFakeWith(f.Task, f.Spawn)
	gw := &gateway.Gateway{
		Store: gateway.NewMemStore(),
		Spawn: f.Spawn,
		// The same launch-time handoff the deployed path uses, so the rehearsal exercises
		// the flow that actually runs. The fake reports pending once before answering,
		// which keeps the polling branch from rotting.
		Handoff: gateway.NewFakeHandoff(),
		Task:    f.Task,
	}
	return &deps{
		brain:     b,
		spawn:     f.Spawn,
		truffle:   f.Truffle,
		principal: brain.Principal{Subject: envOr("FORAY_USER", "foray-user"), AllowExport: true},
		// Poll immediately: the fake's one pending answer is the point, not the wait.
		// make demo-fake is a CI gate and must not spend 10s asleep to prove it polls.
		collector: &collector{gw: gw, poll: time.Millisecond, wait: 10 * time.Second},
	}, nil
}

// buildRealDeps wires the real brain (Bedrock plan + Cedar + spawn), the spore
// CLIs, and a gateway over the stdlib HTTP worker. Credentials and region resolve
// via the standard AWS chain; the planning model is a US inference profile id.
func buildRealDeps(o depsOpts) (*deps, error) {
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS config (set AWS_PROFILE / credentials): %w", err)
	}
	modelID := envOr("FORAY_PLAN_MODEL", "us.anthropic.claude-sonnet-4-6")
	invoker := brain.NewBedrockInvoker(bedrockruntime.NewFromConfig(cfg), modelID)

	bucket := os.Getenv("FORAY_DATA_BUCKET")
	if bucket == "" && o.dataPlane {
		return nil, errors.New("FORAY_DATA_BUCKET is required to run a trace: the graph is staged " +
			"from it and the worker writes its result and saves back to it (foray deploy creates it)")
	}

	runner := spore.NewExecRunner()
	truffle := spore.NewTruffle(runner)
	spawn := spore.NewSpawn(runner)
	task := spore.NewTask(runner)
	principal := buildPrincipal()

	b, err := brain.NewReal(brain.Config{
		Invoker:     invoker,
		Truffle:     truffle,
		Task:        task,
		Spawn:       spawn,
		Principal:   principal,
		BudgetUSD:   o.budgetUSD,
		Region:      cfg.Region,
		Spot:        true,
		WorkerImage: os.Getenv("FORAY_WORKER_IMAGE"),
		DataBucket:  bucket,
		Device:      envOr("FORAY_DEVICE", "cuda"),
		OnComplete:  onComplete(o.keep),
	})
	if err != nil {
		return nil, err
	}
	gw := &gateway.Gateway{
		Store:   gateway.NewMemStore(),
		Spawn:   spawn,
		Handoff: gateway.NewS3Handoff(s3.NewFromConfig(cfg), bucket),
		Task:    task,
	}
	return &deps{
		brain:     b,
		spawn:     spawn,
		truffle:   truffle,
		principal: principal,
		region:    cfg.Region,
		collector: &collector{gw: gw},
	}, nil
}

// onComplete maps --keep onto the task's instance fate. Stopping is the most the task
// path can offer: there is no on_complete that leaves an instance running, so --keep now
// means "leave the root volume around to look at", with EBS billing until TTL as the
// price. Not keeping means terminate, which is the ephemerality invariant.
func onComplete(keep bool) string {
	if keep {
		return spore.TaskOnCompleteStop
	}
	return spore.TaskOnCompleteTerminate
}

// buildPrincipal reads the Cedar principal's budget/tier opt-ins from the
// environment. "large" requires an explicit opt-in; export is allowed unless the
// org denies it (data-residency).
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

// sessionCostSoFar estimates spend on a running session: the Spot $/hr × age.
func sessionCostSoFar(ctx context.Context, t spore.Truffle, inst spore.Instance) float64 {
	quotes, err := t.Price(ctx, inst.InstanceType, inst.Region)
	if err != nil || len(quotes) == 0 {
		return 0
	}
	hrs := time.Since(inst.LaunchedAt).Hours()
	if hrs < 0 {
		hrs = 0
	}
	c := quotes[0].PriceUSDHr * hrs
	return float64(int(c*100+0.5)) / 100
}

// humaneDur renders a duration compactly (e.g. "12m", "3h4m"), flooring negatives.
func humaneDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Minute)
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
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

// confirm asks a yes/no question on stdin, defaulting to yes on a bare Enter.
//
// It refuses when stdin has nothing to give. An unreadable stdin — a closed pipe,
// no tty — returns io.EOF with an empty string, which is the *same value* a bare
// Enter produces; discarding the error therefore made "nobody is there" mean
// "yes". For the Go gate that meant an unattended `foray run` approved every rung
// and launched GPUs with no human at the acceptance node, which is the invariant
// CLAUDE.md calls load-bearing (issue #87). An absent human is not an approving
// one, and this prompt spends money, so the safe direction is the only direction.
//
// Interactive Enter still returns "\n" with a nil error, so the [Y/n] default is
// unchanged for a person at a terminal.
func confirm(prompt string) bool { return confirmFrom(os.Stdin, prompt) }

// confirmFrom is confirm over an injectable reader, so the refusal-on-EOF behavior
// is testable without a terminal.
func confirmFrom(r io.Reader, prompt string) bool {
	fmt.Printf("%s [Y/n] ", prompt)
	s, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && strings.TrimSpace(s) == "" {
		fmt.Println("\n  no answer available on stdin — declining.")
		fmt.Println("  pass --yes to pre-authorize a run, or --force to skip this prompt.")
		return false
	}
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
  foray run "<question>"          propose a ladder, approve, run, climb rung by rung
  foray run --model ... ...       expert path: skip the dialog, name every knob
  foray export <session>          download your own saved activations/outputs
  foray deploy                    provision the ~$0 control plane (primary path)
  foray teardown                  remove it — leave nothing billing
  foray models [<source>]         resolvable model sources (or resolve one)
  foray sessions                  running sessions: age, TTL, $-so-far
  foray stop <session>            stop a session (or let idle reap it)

run flags:
  --model       HF id / s3:// URI / upload:<id>   (expert path; skips the dialog)
  --technique   logit-lens | attribution | steering | sae | generate
  --engine      eager | vllm                       (auto if empty)
  --hardware    instance type, e.g. g7e.xlarge     (else the smallest tier)
  --budget      per-question envelope in USD        (the ladder is capped here)
  --yes         approve every rung without prompting

env:
  FORAY_FAKE=1            walk the whole loop with no AWS calls (the CI gate)
  FORAY_BUDGET_CEILING   per-session Cedar ceiling (USD; default 5.00)
  FORAY_PLAN_MODEL       Bedrock planning model id (US inference profile)
  FORAY_DENY_EXPORT=1    org policy: forbid export (data must stay in-region)
`)
}
