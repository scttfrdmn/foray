# foray — Architecture

**ADI: AWS Deep Inference.** `foray` is ephemeral deep inference — remote access to
the internals of any open model, on the right hardware, for the duration of one
experiment, in your own AWS account.

> NDIF gives researchers free remote access to model internals by keeping a
> fixed catalog of large models resident on a small, fixed national cluster.
> That architecture is a response to scarcity: two GPU types, a shared
> allocation, queue fairness, hot/warm rationing. `foray` is the same capability
> with the scarcity assumption removed. There is no standing fabric. Nothing is
> kept warm. You name an experiment, the right instance is summoned, it runs,
> and it disappears. All you need is an AWS account.

---

## 1. The inversion

NDIF's complexity is scarcity-management, not capability. Ray, Redis, the
controller/processor split, hot-swap eviction, the hot/warm catalog — all of it
exists to share a fixed pool of two GPU types among untrusted strangers with
queue fairness. None of it touches instrumentation.

`foray` keeps the capability (model internals via `nnsight`) and deletes the
scarcity machinery:

| NDIF (scarcity) | foray (elasticity) |
| --- | --- |
| Fixed 4-model catalog | Any model: HF id, S3 URI, or upload |
| Two GPU types (NCSA Delta) | The full EC2 NVIDIA menu, right-sized per model |
| Hot/warm tiers (rationing) | Per-session provisioning; nothing kept warm |
| Ray scheduler + eviction | `spawn` TTL + idle; no scheduler |
| Shared multi-tenant sandbox | Single-tenant self-install; no sandbox needed |
| Activations downloaded to client | Activations stay in-region; only pixels leave |
| Free-but-queued (weeks to allocation) | Cheap-and-instant (seconds to first token) |

The enabling facts are recent and real: **GPUDirect Storage** streams an 800 GB
checkpoint from S3 into HBM in tens of seconds, so cold-start is no longer a
reason to keep models warm; the **EC2 NVIDIA menu** now spans 32 GB (G7 /
RTX PRO 4500) → 96 GB (G7e / RTX PRO 6000, MIG-sliceable) → 141 GB (H200) and up,
so right-sizing is a catalog lookup, not a compromise.

---

## 2. Two planes

The whole system is a control plane that rests at ~$0 and a data plane that
exists only while an experiment runs.

### Control plane — always up, costs ~nothing

| Concern | Service | Cost model |
| --- | --- | --- |
| Web UI ("the page") | Static SPA in **S3 + CloudFront** | fractions of a cent |
| Brain (plan the experiment, write the `nnsight`) | **Bedrock AgentCore** | per-token, zero idle |
| Glue / session state | **API Gateway + Lambda + DynamoDB** | per-invocation |
| Price / quota / availability | **truffle** (spore.host) | local CLI |

Resting cost of the entire platform is a static bucket and some cold Lambdas.
Every dollar above that is per-use (Bedrock tokens) or per-session (the GPU).

### Data plane — ephemeral, per session

| Concern | Component | Lifetime |
| --- | --- | --- |
| Launch the right instance | **spawn** (spore.host) | summoned on Go |
| Hold the model + run interventions | **worker image** (`nnsight`) | one session |
| Stream weights S3 → HBM | GDS loader in the worker | once, on boot |
| Scarce multi-GPU capacity wait | **lagotto** (spore.host) | as needed |
| Saved activations | **S3, in-region** | until the user discards |
| Self-terminate | spawn TTL + request-level idle | end of session |

The new code in all of this is small: **`forayd`** (the gateway) and the
**worker image**. Everything else is reuse (spore.host suite, Bedrock,
standard serverless).

---

## 3. Instrumentation

The capability is `nnsight`, which is PyTorch forward/backward hooks over a proxy
tree mirroring the module graph. A `with model.trace(x):` block registers a
deferred intervention graph; hooks fire at module boundaries during an eager
forward pass to capture (`.save()`), overwrite, or compute against activations.
Two serving engines, routed per request:

- **`LanguageModel` (eager, transformers).** Full transparency: arbitrary module
  access, activation edits, **gradients**. Loads essentially any architecture
  (incl. custom via `trust_remote_code`). Bare-PyTorch throughput. **This is the
  universal path and the reason "any model" is true.**
- **`VLLM` (paged-attention, continuous batching).** High throughput over many
  prompts. **No gradients** (paged-attention kernels retain no autograd graph),
  text-gen only, gated to vLLM-supported architectures, numerically ≠
  transformers.

**Routing rule:** gradients or exotic arch → `LanguageModel`; throughput over
many prompts on a mainstream arch → `VLLM`. Both bake into one worker image.

The engine is device-agnostic — `nnsight` cares about *eager PyTorch with live
module boundaries and working autograd*, not about CUDA specifically. This is why
the device abstraction (§6.3) can later accept Trainium unchanged.

---

## 4. Request lifecycle

```
intent ──▶ brain (AgentCore) ──▶ proposed experiment ──▶ [user: Go] ──▶ spawn
  │            plan: model +          model+technique+         HITL          │
  │            technique +            hardware + $/session      seam         │
  │            hardware + cost                                               ▼
  │                                                              instance boots
  ▼                                                              GDS streams weights
"why does it refuse X?"                                          worker ready
  └──────────────────────────────────────────────────────────────────┐
                                                                       ▼
                          forayd routes the serialized intervention graph
                                                                       ▼
                          worker runs trace, saves activations → S3 (in-region)
                                                                       ▼
                          viz rendered; only pixels reach the browser
                                                                       ▼
                          idle (no requests N min) → spawn terminates → $0
```

The **plan/execute split with a human-in-the-loop seam** is the clAWS pattern:
the brain plans, the user approves ("Go"), `spawn` executes. The elicitation
conversation also doubles as cover for the seconds-long cold start — by the time
the user hits Go, the instance is warming.

This loop is **one rung of a ladder** (§6.2): the first Go runs the cheapest
experiment that could answer the question; after the result the brain recommends
climbing or stopping, and each climb is a fresh Go. The question carried at the
top is the invariant every rung serves.

---

## 5. Progressive disclosure

One capability, four on-ramps. Experts skip the conversation entirely.

1. **Intent** — a box, not a dropdown. "Why does it store France→Paris?" The
   brain kicks back a question (naming a model is the wrong first move) and
   proposes the smallest experiment that answers it (often GPT-2 for cents,
   8B to confirm it scales).
2. **Intermediate** — pick model + technique directly, skip the dialog.
3. **Expert** — every knob: any model URI, engine, layer set, batch, hardware
   override, budget.
4. **CLI** — the whole loop as a pipeable command (`foray run ...`), plus raw
   `nnsight` against the `forayd` endpoint for people who want only the remote
   backend.

The generated `nnsight` code is returned alongside results: the GUI is the
on-ramp, the code is the escape hatch, and watching it get written teaches the
library. This collapses NDIF's Workbench-vs-nnsight split into one surface.

---

## 6. Components

### 6.1 `forayd` — the gateway (Go)

The one genuinely new piece. Responsibilities:

- Accept a serialized `nnsight` intervention graph over HTTP; route it to the
  live worker for the session.
- Maintain session ↔ instance mapping (DynamoDB).
- **Bridge request-activity into spawn's idle signal.** spawn's native idle
  detection (CPU/network/process/session) reads a model-holding-HBM worker as
  idle even when it is exactly what you want alive. `forayd` emits
  `last_request_time` per session; spawn consumes *that* instead of OS
  heuristics. A short idle-grace keeps the worker warm a few minutes post-trace
  so the next trace doesn't re-stream weights; since re-cold-start is seconds,
  the grace-vs-restream tradeoff is near-free either way.

This is the single load-bearing contract — see `internal/gateway/gateway.go`.

### 6.2 brain — Bedrock AgentCore (Go orchestration)

Plan/execute with Cedar and HITL, after clAWS / aws-agentcore-demo, organized
around a **result-gated experiment ladder** (the idea borrowed from Telos, at
foray's scale — Telos is autonomous; foray is human-driven, the human climbs the
ladder). Three disciplines:

1. **The question is the load-bearing invariant.** The user writes a question,
   not a structure. Every rung serves that question; nothing drifts from it, and
   results are framed against it ("here's what we learned about your question,"
   not "here's a logit lens").
2. **Cheap before expensive, result-gated.** The brain plans a ladder of
   experiments ordered cheapest-first (GPT-2 → 8B → 70B). Only the first rung
   runs on Go. The brain proposes climbing *only* when the lower rung's result
   warrants it, and recommends **stopping on an honest negative** ("no effect at
   GPT-2; likely absent — don't pay for 8B to confirm nothing"). A per-question
   budget envelope caps the whole ladder (brain-enforced; distinct from Cedar's
   per-session ceiling — both hold).
3. **The brain never settles its own acceptance.** It proposes and interprets;
   the human accepts. The HITL "Go" *is* the separate acceptance envelope — foray
   gets Telos's "no node settles its own acceptance" property for free from the
   loop, as long as the brain never auto-declares success or auto-climbs.

Mechanics:

- **Plan** (AgentCore Runtime): question → an ordered `Ladder` of `Rung`s
  `{model, technique, engine, hardware, est_cost, nnsight}`, or a clarifying
  question when the ask underdetermines the experiment.
- **Cedar policy**, evaluated per rung: allowed model sources, instance tiers,
  per-session budget ceiling, gradient/large-save toggles. Mirrors the demo's
  AgentCore Gateway Cedar policy (`web_fetch is not permitted`) — here, e.g.,
  `instance tier large denied: exceeds session budget`.
- **HITL seam**: each rung's proposed experiment + cost render in the page; "Go"
  approves *that rung*. No GPU launches before approval.
- **Execute**: hand the approved rung's instance spec to `spawn`; accumulate
  spend against the question's envelope.
- **Assess**: after a rung's result, recommend climb or stop (with a reason tied
  to the finding and the question). A recommendation, never an action — the human
  decides. See `internal/brain/plan.go`.

### 6.3 device — accelerator/instance abstraction (Go)

Maps an abstract accelerator target to concrete EC2 tiers. **NVIDIA is the only
enabled provider now; `neuron` is registered but GA-gated** so the worker image's
device path and the sizing logic are built to accept it from day one without it
appearing in the public menu. See `internal/device/`.

NVIDIA tiers (enabled):

| Tier | Instance | Per-GPU | Use |
| --- | --- | --- | --- |
| slice | G7e RTX PRO 6000 MIG (~24 GB) | 24 GB | GPT-2, 8B, packed concurrent sessions |
| small | G7 RTX PRO 4500 (whole) | 32 GB | 8B, single session, simpler than MIG |
| mid | G7e RTX PRO 6000 (whole) | 96 GB | up to 70B FP8 single-GPU |
| large | H200 / multi-GPU G7e over P2P/EFA | 141 GB+ | 70B bf16, 405B |

Trainium (gated, deferred): TorchNeuron's native PyTorch backend (PrivateUse1
device, eager dispatch, working autograd) makes `nnsight` portable to Neuron with
little or no change once it GAs. Until then `neuron` stays disabled in the
registry. See `internal/device/neuron.go` and `CLAUDE.md` §Deferred.

### 6.4 sizing — footprint → options (Go)

Sizes to **model + intervention shape**, not model alone. Same 8B is ~16 GB for a
logit lens with light saves, but balloons if the user captures the full residual
stream across every layer and token, and forces the eager path with a retained
autograd graph for gradient work. Output: a ranked list of `{tier, instance,
utilization, $/session}` rows for the page and the brain. See
`internal/sizing/footprint.go`.

### 6.5 catalog — model source resolver (Go)

Model source is irrelevant to the worker; only format matters. Resolves HF id /
`s3://` URI / uploaded object → a HF-format checkpoint (config + safetensors +
tokenizer) the worker can load. Uploads are just S3 objects.

### 6.6 spore — adapters (Go)

Thin adapters over the spore.host binaries. **Do not reimplement these.**

- **truffle**: instance discovery, Spot pricing across regions/AZs, quota check.
  Backs every cost number.
- **spawn**: launch < 2 min, TTL auto-termination, idle (fed by `forayd`),
  hibernation, GPU/MPI clusters.
- **lagotto**: capacity watcher (Lambda) for scarce multi-GPU — the former
  "warm" tier, reframed as watch-and-grab.

### 6.7 worker — the nnsight server (Python, containerized)

The one Python boundary (Go control plane + Python hot path, after umami).

- FastAPI: receive serialized graph → deserialize → run interleaved with the
  forward pass → return saved values (to S3, in-region).
- Engines: `LanguageModel` (eager) and `VLLM`, routed per §3.
- GDS loader: stream weights S3 → HBM on boot (sharded GDS for multi-GPU).
- Device target passed in from the control plane (`cuda` now; `neuron` later).

### 6.8 web — the page (static SPA)

S3 + CloudFront, plain HTML/JS in the aws-agentcore-demo style (badges, live cost
meter, receipt, `demo-fake` rehearsal mode). Intent box → proposed-experiment
card → live cost → Go, with the expert/CLI escape hatches visible. The
"workbench, but a lot better." Polished build is a follow-on; this repo ships the
skeleton and style contract.

---

### 6.9 export — opt-in download (Go)

The default is that saved values never leave the region (§8). Export is the
deliberate, user-initiated exception: a researcher who wants to keep, publish, or
analyze their results locally can download them. It is a **presigned S3 GET**
against the user's own bucket — the data is already theirs, in their account; the
control just hands them a time-limited URL (single object, or a zipped bundle of
a session's saves + the generated `nnsight` + a manifest). Surfaced as
`foray export <session>` and a "Download" control on the result card. Governed by
a Cedar `export` action so an org can disable it where policy requires data stay
in-region. This is opt-in egress of one's own data, not the architecture
streaming tensors on every trace. See `internal/export/`.



The number that matters is **$/session, not $/hour**, because TTL + idle bound
the session. The page shows a live meter and a receipt; the brain refuses to
propose an experiment whose estimate exceeds the Cedar budget ceiling. NDIF
spends real money keeping 405B warm to advertise "free"; foray rests at pennies
and bills honestly per trace.

---

## 8. Security posture

- **No untrusted-code sandbox.** Single-tenant self-install: the user runs their
  own intervention code on their own ephemeral GPU in their own account. NDIF's
  hardest problem — isolating strangers on shared GPUs — simply isn't present. It
  returns only if a shared hosted tier is ever offered (deferred).
- **No automatic egress of activations.** Saved values land in S3 in-region; the
  analysis loop (notebook / Workbench backend) is co-resident in the VPC. Only
  rendered pixels reach the browser. The "30-minute activation download" is an
  artifact of off-cloud clients and does not exist here. **Export is the
  deliberate exception:** a user can download their own saved values via a
  presigned URL when they choose to — opt-in and user-initiated, not the
  architecture egressing on every trace. See §6.9.
- **Cedar governs the agent.** Model sources, instance tiers, and budgets are
  policy, not code. The brain plans within policy; the HITL seam is the final
  gate.

---

## 9. Deferred (explicitly not now)

- **Trainium / `neuron` device.** Validated and waiting, GA-gated. NVIDIA-only
  ships; the abstraction accepts `neuron` the day TorchNeuron GAs.
- **Shared hosted multi-tenant tier.** Would reintroduce the sandbox problem.
  Single-tenant self-install only.
- **Polished Workbench frontend.** Skeleton + style contract here; full build
  follows.

---

## 10. Build order

For Claude Code. Each step is independently testable; earlier steps have no AWS
dependency.

1. **device + sizing** — pure logic, table-driven tests, no AWS.
2. **catalog** — source resolver; unit-test URI parsing.
3. **spore adapters** — wrap truffle/spawn/lagotto; integration-test behind a
   fake (`FORAY_FAKE=1`) like aws-agentcore-demo's `demo-fake`.
4. **forayd** — gateway: graph routing + idle bridge. The load-bearing contract.
5. **worker** — nnsight server + GDS loader; `cuda` only.
6. **brain** — AgentCore plan/execute + Cedar + HITL as a result-gated ladder:
   `PlanLadder` (question → cheapest-first rungs, or a clarifying question),
   per-rung `Permit`, `Approve` one rung per Go, `Assess` (climb/stop with honest
   negatives, capped by the per-question envelope). Fake: a 2-rung GPT-2→8B ladder.
7. **CLI** — `foray` over the gateway + brain.
8. **web** — static SPA in the demo style.
9. **deploy** — IaC: S3+CloudFront, API GW+Lambda, IAM, Cedar, DynamoDB.

Ship gate: `make demo-fake` exercises the full intent → plan → Go → (fake)
spawn → receipt loop with no AWS calls.
