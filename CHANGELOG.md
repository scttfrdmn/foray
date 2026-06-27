# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html).

While the project is pre-1.0.0, the public API and behavior may change in any
release. Breaking changes will be called out under **Changed** with a `BREAKING:`
prefix.

## [Unreleased]

### Added

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

- The core packages (`device`, `sizing`, `catalog`, `brain` real path,
  `gateway`/`forayd`, `worker`, `spore` adapters, `deploy`) are not yet
  implemented; their work is tracked in GitHub milestones and issues. As a
  result `go build ./...` and `go test ./...` are intentionally not green at
  this stage — only `internal/export` builds standalone; `brain` and
  `cmd/foray` await `internal/sizing`. The static page (`web/`) runs the full
  loop today.

[Unreleased]: https://github.com/scttfrdmn/foray/commits/main
