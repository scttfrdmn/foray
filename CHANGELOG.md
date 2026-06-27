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

### Changed

- CI: `go test ./...`, `go build ./...`/`go vet ./...`, and `make demo-fake` are
  now hard gates (dropped `continue-on-error`); `.golangci.yml` migrated to the
  v2 schema.

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

- The whole tree now builds and `go test ./...` passes. The AWS-touching pieces
  (`catalog`, the real non-fake `brain` path, `gateway`/`forayd`, `worker`,
  `spore` adapters, `deploy`) are not yet implemented; their work is tracked in
  GitHub milestones and issues.

[Unreleased]: https://github.com/scttfrdmn/foray/commits/main
