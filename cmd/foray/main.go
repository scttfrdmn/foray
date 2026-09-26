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
	"net/http"
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
		keep      = fs.Bool("keep", false, "leave each rung's instance running instead of terminating it when the rung ends (idle still stops it, TTL still terminates it)")
	)
	question := parseWithPositionals(fs, args)

	d, err := buildDeps(*budget)
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

		// Approve is the sole acceptance node: it runs Cedar, then launches.
		sid, err := d.brain.Approve(ctx, ladder, prop)
		if err != nil {
			die(err) // Cedar denials surface here with the policy reason verbatim.
		}
		fmt.Printf("  Go — launched session %s on %s\n", sid, prop.Rung.Chosen.InstanceType)

		tr, err := runRung(ctx, d, sid, prop.Rung, keep)
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
	d, err := buildDeps(0)
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
	d, err := buildDeps(0)
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
	d, err := buildRealDeps(0)
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
	tracer    *tracer
}

// regionScope returns the truffle/pricing region scope, or nil to let truffle
// pick by availability/price.
func (d *deps) regionScope() []string {
	if d.region == "" {
		return nil
	}
	return []string{d.region}
}

// runRung does one approved rung's data-plane work: open a tunnel to the worker,
// trace, then tear the session down.
//
// It exists as a function rather than inline in runLoop so cleanup can be
// deferred. Cleanup is the whole point: the instance exists from the moment
// Approve returns, so *every* exit path from here — including a failed trace —
// has to close the tunnel and reap the instance. runLoop reports errors through
// die(), which calls os.Exit and therefore runs no defers, so the cleanup has to
// finish before the error gets there.
//
// Order is LIFO and deliberate: close the tunnel first, then terminate the
// instance it pointed at.
func runRung(ctx context.Context, d *deps, sid string, r *brain.Rung, keep bool) (gateway.TraceResult, error) {
	defer reap(ctx, d, sid, keep)

	// register maps the session→worker (opening the spawn service tunnel); trace
	// routes the graph and bridges the idle signal; only references come back,
	// never tensors.
	if err := d.tracer.register(ctx, sid, r); err != nil {
		return gateway.TraceResult{}, fmt.Errorf("register session: %w", err)
	}
	defer func() {
		if cerr := d.tracer.close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "  note: closing the worker tunnel for %s: %v\n", sid, cerr)
		}
	}()

	tr, err := d.tracer.trace(ctx, sid, r)
	if err != nil {
		return gateway.TraceResult{}, fmt.Errorf("trace session %s: %w", sid, err)
	}
	return tr, nil
}

// reap terminates the session's instance, which is what actually reaches $0.
//
// spawn's idle timeout only *stops* an instance — it never terminates one, and a
// stopped instance keeps billing its EBS volumes until TTL (issue #80). Since the
// control plane knows precisely when a rung is done, it terminates then rather
// than leaving an idle box billing storage for the rest of its TTL. Idle and TTL
// remain the backstops for anything that escapes this path.
//
// A failed termination is reported, not fatal: the rung's result is already in
// hand, and TTL still bounds the instance. Telling the user which session to clean
// up by hand beats discarding their finding.
func reap(ctx context.Context, d *deps, sid string, keep bool) {
	if keep {
		fmt.Printf("    session %s left running (--keep); idle stops it, TTL terminates it.\n", sid)
		fmt.Printf("    terminate it now with: foray stop %s\n", sid)
		return
	}
	if err := d.spawn.Terminate(ctx, sid); err != nil {
		fmt.Fprintf(os.Stderr,
			"  note: could not terminate %s (%v) — TTL will still reap it; `foray stop %s` to be sure\n",
			sid, err, sid)
	}
}

// tracer fetches a rung's result through the gateway library, in-process. It is
// the CLI playing the role forayd plays as a Lambda: same Route, same idle bridge.
type tracer struct {
	gw     *gateway.Gateway
	spawn  spore.Spawn
	server spore.Server
	device string

	// svc is the current session's tunnel, held between register and close.
	svc *spore.Service
}

// register starts the worker on the session's instance and opens a tunnel to it,
// then maps the session so Route can resolve it (issue #66).
//
// The worker is reached through `spawn service`, not over a public IP: it binds
// the instance's loopback and only the SSH forward reaches it, so nothing is
// exposed to the internet and no VPC/endpoint/NAT is needed to talk to it. The
// URL spawn hands back is a localhost address carrying the worker's session
// token; gateway.HTTPWorker splits the two apart.
func (t *tracer) register(ctx context.Context, sid string, r *brain.Rung) error {
	svc, err := t.server.Serve(ctx, spore.ServeSpec{
		InstanceID: sid,
		Command:    workerCommand(sid, r, t.device),
	})
	if err != nil {
		return fmt.Errorf("open a tunnel to the worker on %s: %w", sid, err)
	}
	t.svc = svc
	return t.gw.Store.Put(ctx, gateway.Session{
		ID:         sid,
		InstanceID: sid,
		WorkerURL:  svc.URL,
	})
}

// close tears down the session's tunnel. The instance itself is not touched —
// its TTL and idle timeout own that, as they must: stopping a tunnel is a
// request, and only the instance's own deadlines are a guarantee.
func (t *tracer) close() error {
	if t.svc == nil {
		return nil
	}
	err := t.svc.Stop()
	t.svc = nil
	return err
}

// workerCommand is the argv `spawn service` runs on the instance. spawn
// shell-quotes it and appends "--addr 127.0.0.1:0", so worker.serve binds a free
// loopback port and announces it (see worker/serve.py).
//
// The session's configuration rides in front of the interpreter via env(1)
// because `spawn service` has no --env flag — and it should not grow one for our
// sake when the command is already a command.
func workerCommand(sid string, r *brain.Rung, deviceTarget string) []string {
	return []string{
		"env",
		"FORAY_SESSION_ID=" + sid,
		"FORAY_MODEL_URI=" + r.Model.Name,
		"FORAY_DEFAULT_ENGINE=" + string(r.Engine),
		"FORAY_DEVICE=" + deviceTarget,
		"FORAY_SAVE_BUCKET=" + os.Getenv("FORAY_DATA_BUCKET"),
		"FORAY_SAVE_REGION=" + os.Getenv("AWS_REGION"),
		"python3", "-m", "worker.serve",
	}
}

// trace routes the rung's generated nnsight to the worker and returns the result
// reference. The payload is the nnsight as bytes — the seam where real graph
// serialization plugs in; the gateway treats it as opaque either way.
func (t *tracer) trace(ctx context.Context, sid string, r *brain.Rung) (gateway.TraceResult, error) {
	return t.gw.Route(ctx, sid, gateway.Graph{
		Engine:  string(r.Engine),
		Payload: []byte(r.NNSight),
	})
}

// buildDeps wires the fake or real collaborators depending on FORAY_FAKE.
func buildDeps(budgetUSD float64) (*deps, error) {
	if spore.Enabled() {
		return buildFakeDeps(budgetUSD)
	}
	return buildRealDeps(budgetUSD)
}

// buildFakeDeps wires the offline path: a fake brain, a shared fake spawn (so the
// brain's executor and the gateway's idle bridge see the same instance table),
// and the gateway's canned worker. Zero AWS — the dev/rehearse path and CI gate.
func buildFakeDeps(_ float64) (*deps, error) {
	f := spore.NewFake()
	b := brain.NewFakeWith(f.Spawn)
	gw := &gateway.Gateway{
		Store:  gateway.NewMemStore(),
		Worker: gateway.NewFakeWorker(),
		Spawn:  f.Spawn,
	}
	return &deps{
		brain:     b,
		spawn:     f.Spawn,
		truffle:   f.Truffle,
		principal: brain.Principal{Subject: envOr("FORAY_USER", "foray-user"), AllowExport: true},
		tracer:    &tracer{gw: gw, spawn: f.Spawn, server: f.Server, device: envOr("FORAY_DEVICE", "cuda")},
	}, nil
}

// buildRealDeps wires the real brain (Bedrock plan + Cedar + spawn), the spore
// CLIs, and a gateway over the stdlib HTTP worker. Credentials and region resolve
// via the standard AWS chain; the planning model is a US inference profile id.
func buildRealDeps(budgetUSD float64) (*deps, error) {
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS config (set AWS_PROFILE / credentials): %w", err)
	}
	modelID := envOr("FORAY_PLAN_MODEL", "us.anthropic.claude-sonnet-4-6")
	invoker := brain.NewBedrockInvoker(bedrockruntime.NewFromConfig(cfg), modelID)

	runner := spore.NewExecRunner()
	truffle := spore.NewTruffle(runner)
	spawn := spore.NewSpawn(runner)
	// The worker is reached through `spawn service`, which is a long-lived child
	// process rather than a one-shot command, so it needs the Starter seam.
	server := spore.NewServer(spore.NewExecStarter())
	principal := buildPrincipal()

	b, err := brain.NewReal(brain.Config{
		Invoker:   invoker,
		Truffle:   truffle,
		Spawn:     spawn,
		Principal: principal,
		BudgetUSD: budgetUSD,
		Region:    cfg.Region,
		Spot:      true,
	})
	if err != nil {
		return nil, err
	}
	gw := &gateway.Gateway{
		Store:  gateway.NewMemStore(),
		Worker: gateway.HTTPWorker{Client: &http.Client{Timeout: 10 * time.Minute}},
		Spawn:  spawn,
	}
	return &deps{
		brain:     b,
		spawn:     spawn,
		truffle:   truffle,
		principal: principal,
		region:    cfg.Region,
		tracer:    &tracer{gw: gw, spawn: spawn, server: server, device: envOr("FORAY_DEVICE", "cuda")},
	}, nil
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
