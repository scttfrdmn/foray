# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html).

While the project is pre-1.0.0, the public API and behavior may change in any
release. Breaking changes will be called out under **Changed** with a `BREAKING:`
prefix.

## [Unreleased]

### Added

- `internal/device`: accelerator/instance registry — `Tier` (slice/small/mid/
  large), `Backend`, `Option`, `Provider`, `Lookup`, and `Options(minHBM)` which
  returns enabled NVIDIA tiers that fit, sorted ascending. `neuron` is
  registered but GA-gated (disabled, never surfaced).
- `internal/sizing`: `Size(model, intervention)` → footprint + ranked hardware
  options. Engine routing (gradients/per-layer saves → eager; large prompt sweep
  → vLLM), residual-stream and KV-pool memory math.
- `internal/catalog`: model-source resolver — `Parse(raw)` classifies and
  validates a HuggingFace id, an `s3://` URI, or an `upload:<id>` ref into a
  `Source` descriptor; unsupported sources wrap `ErrUnsupportedSource` with a
  verbatim reason. Allowed kinds (`hf`/`s3`/`upload`) mirror the `modelSource`
  values in `internal/brain/policy/foray.cedar`. AWS-free, table-driven tests.
- `internal/brain`: the result-gated ladder core — `Brain`, `Ladder`, `Rung`,
  `Question`, `Proposal`, `Result`, `Recommendation`, the Planner/Policy/Executor
  seams, and `Propose`/`Approve`/`Assess`/`NextProposal`. The human "Go"
  (`Approve`) is the only place a rung runs.
- **`make demo-fake` now walks the full loop end-to-end offline** (intent → plan
  → Go → run → assess → climb → receipt), zero AWS — the MVP definition of done.
- `internal/spore`: thin adapters over the spore.host binaries — `Truffle`
  (`Price`/`Quota`/`Discover`), `Spawn` (`Launch`/`Status`/`Terminate`/
  `KeepWarm`), and `Lagotto` (`Watch`/`List`/`Status`). Adapters shell out to the
  installed CLIs via a `Runner` seam and parse their `-o json` output; a `Fake`
  trio (`NewFake`/`FromEnv`) returns canned data with zero AWS for `FORAY_FAKE=1`.
  `Spawn.KeepWarm(id, lastRequest)` is the idle-bridge surface the forayd gateway
  (step 4) will drive so a model-holding worker isn't reaped between traces.
  Resolves how foray depends on spore.host (issue #41): shell out, no Go-module
  dependency — documented in the package doc.
- `internal/gateway` (`forayd`): the one load-bearing new contract. `Gateway.Route`
  resolves a session, bridges per-session `last_request_time` into spawn's idle
  signal (via `spore.Spawn.KeepWarm`) so a model-holding-HBM worker isn't reaped
  between traces, then forwards the serialized nnsight graph to the live worker —
  returning only references (`s3://` save ref + viz ref), never tensors (no
  automatic egress). `Store`/`Worker` seams (in-memory + `HTTPWorker` now;
  DynamoDB-backed store deferred to deploy). `Handler` serves
  `POST /sessions/{id}/trace` and `GET /healthz` (liveness + freshest
  `last_request_time`) on a stdlib `ServeMux`. `NewFake` runs it all with zero
  AWS for `FORAY_FAKE=1`. Closes #11, #12, #46.
- `cmd/forayd`: thin entrypoint wrapping `Gateway.Handler` in an `http.Server`
  for local/dev and rehearsal; per-invocation gateway logic (no daemon state) so
  it drops onto a cold Lambda and the control plane rests at ~$0.
- `worker/`: the nnsight worker — the one Python boundary (ARCHITECTURE.md §6.7).
  FastAPI server speaking the wire contract fixed by step 4: `POST /trace`
  (`Graph{engine, payload}` → `TraceResult{session_id, save_ref, viz_ref,
  nnsight}`, references only — never tensors) and `GET /healthz`. Engine routing
  per §3 — `eager` (nnsight `LanguageModel`, full transparency + gradients, the
  universal path) vs. `vllm` (paged-attention throughput, no gradients); a
  gradient request on `vllm` is rejected with a clear `400` (#49). GDS loader
  streams weights S3→HBM on boot with a plain-download fallback (#14). Device
  target is a parameter (`FORAY_DEVICE`, default `cuda`); `neuron` is
  registered-but-disabled and refused until TorchNeuron GAs, mirroring the Go
  registry's three-layer gate (#15). Heavy deps (`torch`/`nnsight`/`vllm`/`boto3`)
  are imported lazily inside the real paths, so `FORAY_FAKE=1` and the unit tests
  run with no GPU, no AWS, and no torch. Closes #13.
- `worker/Dockerfile` + `make worker`: a single image holding both engines; the
  device target is injected by the control plane at run time, not baked in (#50).
- `make worker-test` (pytest under `FORAY_FAKE=1`, the new CI job), `make
  worker-fake` (local uvicorn), and `make worker-smoke` (manual real GPU/AWS
  smoke, opt-in via `FORAY_GPU_SMOKE=1`, never run in CI — with a reproducible EC2
  recipe in `worker/README.md`).
- `internal/brain` real path — AgentCore plan/execute + Cedar + the result-gated
  ladder, behind the same `Planner`/`Policy`/`Executor` seams the fake uses, so
  `FORAY_FAKE=1` and `make demo-fake` stay green offline (the CI gate is
  untouched). `AgentCorePlanner` asks Bedrock (`Converse`, via the `Invoker` seam)
  for a cheapest-first ladder — or a clarifying question when the ask
  underdetermines the experiment — then sizes and prices each rung locally so the
  LLM never touches the money path; rungs are ordered by `$/session` (smaller
  model breaks a tie). `CedarPolicy` evaluates `foray.cedar` per rung via the
  cedar-go SDK and surfaces deny reasons verbatim from each `forbid`'s `@reason`
  annotation (budget ceiling, allowed tiers, the `large`-tier and gradient/
  large-save opt-ins, the `neuron` GA gate). `CedarExportPolicy` does the same for
  the `export` action (owner-only, org data-residency). `NewTrufflePricer` turns a
  Spot `$/hour` quote into a `$/session` estimate; `SpawnExecutor` launches an
  approved rung via spawn with TTL + idle guardrails. `NewReal(Config)` wires it
  all; `BedrockInvoker` isolates the AWS SDK behind the `Invoker` seam. Closes
  #17, #18, #34, #35, #43, #44, #45.
- `cmd/foray`: the real `run` path is wired — it loads AWS config, builds the real
  brain (planning model via `FORAY_PLAN_MODEL`, Cedar principal from the
  environment), and plans → asks for an explicit Go → Cedar-gates → launches the
  rung via spawn (result-driven climbing over forayd lands with the gateway-wired
  CLI in step 7). `FORAY_FAKE=1` still walks the whole loop unattended.
- `cmd/foray`: the full CLI on-ramp (ARCHITECTURE.md §5) — one result-gated loop
  shared by the fake and real paths that walks
  propose → Go → `Approve` → trace → interpret → assess → climb. Results are
  fetched through the gateway library hosted **in-process** (the CLI plays the
  role forayd plays as a Lambda: `gateway.Route` + the idle bridge, no running
  daemon and no DynamoDB needed), so the human climbs rung by rung — **each climb
  a fresh Go, never auto-climbed**, and the loop **stops on an honest negative**.
  The expert on-ramp (`--model/--technique/--engine/--hardware/--budget`) skips
  the dialog and builds one priced rung via `brain.ExpertLadder` (Cedar still
  gates it; `--budget` is the per-question envelope). New verbs: `models` (list /
  resolve sources via `catalog.Parse`), `sessions` (age, TTL, `$-so-far` via
  `spore.Spawn.List` + truffle), `stop <session>` (confirm → `spawn.Terminate`),
  and a real `export` path that runs the Cedar export gate (`CedarExportPolicy`,
  ownership via `spawn.Status`) ahead of a clearly-labeled presigner stub (the
  real S3 presigner is deferred to the deploy step, #25). `make demo-fake` walks
  the whole loop — including the gateway trace and the climb — offline. Closes
  #20, #21, #22, #23, #24.
- `internal/brain`: an `Interpreter` seam (`AgentCoreInterpreter` real,
  `fakeInterpreter` offline) so the brain frames a rung's result against the
  question and reports the honest-negative signal — the LLM interprets, never
  touches the money path or acceptance. `RawResult` is the brain-local view of a
  trace (refs only, never tensors). `ExpertLadder`/`ExpertSpec` build a one-rung
  ladder from explicit knobs with no Bedrock.
- `internal/spore`: `Spawn.List` enumerates foray-launched instances (backs
  `foray sessions`); `Instance` gains `LaunchedAt` for session age + `$-so-far`.
- `internal/gateway`: `NewFakeWorker` exposes the canned worker so a caller can
  build a `Gateway` over its own store + spawn.
- `internal/device`: `ByInstanceType` resolves an EC2 instance type to its
  enabled tier option (the `--hardware` override and the `sessions` view); a
  GA-gated backend never matches.

### Changed

- `internal/brain`: `Result` gains `EffectPresent`; `Assess` now stops the climb
  on an honest negative (`!EffectPresent`) before the budget gate — a null result
  stops regardless of remaining envelope ("don't pay to confirm nothing"). `Brain`
  gains the `Interp` seam and an `Interpret` method; the fake's executor is now the
  real `SpawnExecutor` over a fake spawn (the offline loop exercises real executor
  and gateway code).

- `internal/brain`: `Rung` gains `ModelSource` and `Gradients` so the Cedar
  Experiment entity is faithful (additive; the fake sets them).
- `internal/brain/policy/foray.cedar`: removed an unconditional `forbid` that
  would have denied every request (Cedar is deny-by-default); added `@id`/
  `@reason` annotations so deny messages are authored in policy, an explicit
  over-budget `forbid`, and the two `export` forbids (non-owner, data-residency).
  Decimal comparisons use `.lessThanOrEqual`/`.greaterThan` (Cedar decimals are
  not `<=`-comparable).
- First real third-party dependencies: `github.com/cedar-policy/cedar-go` and
  `github.com/aws/aws-sdk-go-v2` (`config`, `service/bedrockruntime`). `go test
  ./...` and `make demo-fake` remain fully offline.

- CI: `go test ./...`, `go build ./...`/`go vet ./...`, and `make demo-fake` are
  now hard gates (dropped `continue-on-error`); `.golangci.yml` migrated to the
  v2 schema. New `worker-test` job runs the worker's pytest suite under
  `FORAY_FAKE=1` (Python 3.12, base deps only — no GPU, no AWS) and `ruff check`.
  `scripts/license-check.sh` now covers `*.py` and `Dockerfile`.

### Project bootstrap

- Project bootstrap: Apache 2.0 `LICENSE` + `NOTICE`, license headers on every
  source file.
- Go module `github.com/scttfrdmn/foray` (Go 1.26) and the
  `cmd/` + `internal/` layout from `ARCHITECTURE.md`.
- `ARCHITECTURE.md` (full design) and `CLAUDE.md` (working contract / invariants).
- `internal/brain` fake planner and a 2-rung GPT-2 → 8B ladder for `FORAY_FAKE=1`.
- `internal/export` opt-in presigned-download interfaces and a fake presigner.
- `internal/brain/policy/foray.cedar` — the Cedar policy spine.
- Test specifications for `internal/device`, `internal/sizing`, and
  `internal/brain` (these define behavior ahead of implementation).
- Static web demo (`web/`) — the page, runs the loop client-side with canned data.
- `Makefile` (`build`, `lint`, `test`, `demo-fake`, `worker`, `deploy`,
  `teardown`), `golangci-lint` config.
- CI/CD: GitHub Actions `ci.yml` (format, vet, build, demo-fake, license-header
  check) and `release.yml` (tag-driven, CHANGELOG-validated), Dependabot.
- Contributor docs: `README.md`, `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`,
  `SECURITY.md`, `AUTHORS`, issue/PR templates.

### Notes

- The whole tree now builds and `go test ./...` passes. The remaining AWS-touching
  pieces (the real non-fake `brain` path, the DynamoDB `gateway.Store`, the GPU/AWS
  `worker` real path, `deploy`) are not yet exercised in CI; their work is tracked
  in GitHub milestones and issues. The worker's real GPU/AWS path is validated by
  hand via `make worker-smoke` (see `worker/README.md`), never in CI.

[Unreleased]: https://github.com/scttfrdmn/foray/commits/main
