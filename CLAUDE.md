# CLAUDE.md — foray

Guidance for Claude Code working in this repo. Read `ARCHITECTURE.md` first; this
file is the working contract.

## What this is

`foray` is **ADI — AWS Deep Inference**: ephemeral remote access to model
internals (`nnsight`) on right-sized EC2 GPUs, per session, in the user's own AWS
account. NDIF with the scarcity machinery deleted. Tagline: *all you need is an
AWS account.*

## Invariants (do not violate)

- **Ephemeral by default.** Nothing is kept warm. No hot/warm tiers, no
  scheduler, no resident catalog. An instance exists only while a session runs;
  idle + TTL terminate it.
- **Control plane rests at ~$0.** Static SPA + cold Lambdas + Bedrock per-token.
  Never introduce an always-on server, queue broker, or Ray/K8s cluster.
- **Cost is per-session, not per-hour.** Every surface shows $/session. The brain
  refuses experiments over the Cedar budget ceiling.
- **No *automatic* egress.** By default saved values stay in S3 in-region and
  only pixels reach the browser — never auto-stream tensors on every trace (the
  eDIF anti-pattern). The user may *explicitly* export their own saved
  activations/outputs: a presigned, opt-in download from their own bucket. Their
  data, their call. Auto-egress is forbidden; user-initiated export is a feature.
- **Single-tenant self-install.** The user runs their own code on their own GPU
  in their own account. Do **not** build an untrusted-code sandbox or a shared
  hosted tier — that reintroduces NDIF's hardest problem.
- **Device-agnostic worker.** `nnsight` needs eager PyTorch + live module
  boundaries + autograd, not CUDA specifically. Keep the device target a
  parameter so `neuron` can slot in later (see Deferred).
- **The question is the load-bearing invariant.** Users write a question, not a
  structure. Every experiment serves that question; results are framed against
  it; nothing mutates it. (After Telos, at foray's scale.)
- **The brain proposes and interprets; it never accepts.** The human at "Go" is
  the acceptance node — no node settles its own acceptance. Cheap before
  expensive: only the first ladder rung runs on Go; climbing is result-gated,
  HITL-approved, and **stops on honest negatives**. Never auto-climb, never
  auto-declare success.
- **Budget has two scopes.** Cedar enforces the per-session ceiling; the brain
  enforces the per-question envelope across the whole ladder. Both hold.

## Language & style

- **Go for everything except the worker.** Go control plane, Python hot path
  (the `nnsight` worker), after umami. Go 1.26+.
- **Apache 2.0**, every file.
- **stdlib-first.** No web frameworks, no DI containers, no codegen unless it
  earns its place. AWS SDK for Go v2 for AWS calls. Cobra is acceptable for the
  CLI; prefer flag if it suffices.
- **Unix philosophy.** Single-purpose binaries (`foray`, `forayd`), composable,
  pipeable, quiet on success. Mirror obol/obold and the spore.host tools.
- **Errors wrapped with context** (`fmt.Errorf("...: %w", err)`); no panics in
  library code; no naked returns hiding errors.
- **Table-driven tests.** `device`, `sizing`, `catalog` must be fully unit-tested
  with no AWS calls.
- Terse, precise comments. Explain *why*, not *what*.

## Reuse — do not reimplement

The spore.host suite is a dependency, not a thing to rebuild:

- **truffle** — instance discovery, Spot pricing, quota. Backs cost numbers.
- **spawn** — launch + TTL + idle (idle fed by `forayd`) + hibernation.
- **lagotto** — capacity watcher (Lambda) for the scarce multi-GPU case.

`internal/spore/` holds thin adapters. If you find yourself writing an instance
launcher or a price fetcher, stop — call the tool.

## The one load-bearing new contract

`internal/gateway` (`forayd`): route the serialized `nnsight` intervention graph
to the live worker, and **bridge per-session `last_request_time` into spawn's
idle signal** so a model-holding-HBM worker isn't reaped as "idle" between two
traces. Get this right; everything else is plumbing.

## AWS shape (after aws-agentcore-demo)

- **Bedrock AgentCore** is the brain: plan → propose → (HITL Go) → execute.
- **Cedar** governs model sources, instance tiers, budgets — policy in
  `internal/brain/policy/foray.cedar`, not in Go.
- US inference profile IDs for models (`us.anthropic.claude-...`).
- A **fake mode** (`FORAY_FAKE=1`) runs the whole loop with canned responses and
  zero AWS calls — the dev/rehearse path and the CI gate, exactly like the demo's
  `make demo-fake`.

## Make targets (keep these working)

```
make build         # build foray + forayd
make lint          # gofmt + go vet + staticcheck
make test          # go test ./...  (no AWS)
make demo-fake     # full intent→plan→Go→fake-spawn→receipt, no AWS  (CI gate)
make worker        # build the nnsight worker image
make deploy        # IaC up (S3+CloudFront, API GW+Lambda, IAM, Cedar, DDB)
make teardown      # IaC down — leave nothing running, nothing billing
```

## Build order

Follow `ARCHITECTURE.md` §10: device+sizing → catalog → spore adapters → forayd →
worker → brain → CLI → web → deploy. Steps 1–3 have no AWS dependency; land them
with tests first.

## Deferred — do not build yet

- **Trainium / `neuron` device.** Keep it registered-but-disabled in
  `internal/device/neuron.go`. The abstraction must accept it; the menu must not
  show it until TorchNeuron GAs. NVIDIA-only ships.
- **Shared hosted multi-tenant tier** (and therefore any sandbox).
- **Polished Workbench UI.** Skeleton + style only for now.

## Definition of done (MVP)

`make demo-fake` walks intent → proposed experiment (model+technique+hardware+$)
→ Go → fake spawn → trace → receipt, entirely offline. Then the same loop against
a real account launches a G7/G7e instance, streams a small model, runs a logit
lens, returns the viz and the generated `nnsight`, and self-terminates on idle.
