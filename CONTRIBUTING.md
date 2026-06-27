# Contributing to foray

Thanks for your interest. Read [ARCHITECTURE.md](ARCHITECTURE.md) first, then
[CLAUDE.md](CLAUDE.md) — the latter is the working contract and its **invariants
are hard rules**, not preferences.

## Development setup

```bash
# Go 1.26+
go version

make build        # build foray + forayd
make lint         # gofmt + go vet + staticcheck/golangci-lint
make test         # go test ./...   (no AWS)
make demo-fake    # full intent -> plan -> Go -> fake-spawn -> receipt, no AWS
```

`make demo-fake` is the CI gate and the definition of done for the MVP. It must
walk the whole loop offline, with `FORAY_FAKE=1` and zero AWS calls.

## Invariants (do not violate)

These come from [CLAUDE.md](CLAUDE.md). A PR that breaks one will not be merged:

- **Ephemeral by default.** Nothing is kept warm — no hot/warm tiers, no
  scheduler, no resident catalog.
- **Control plane rests at ~$0.** No always-on server, queue broker, or
  Ray/K8s cluster.
- **Cost is per-session, not per-hour.** Every surface shows $/session; the
  brain refuses experiments over the Cedar budget ceiling.
- **No *automatic* egress.** Saved values stay in S3 in-region; only pixels reach
  the browser. User-initiated export (presigned, opt-in) is a feature;
  auto-egress on every trace is forbidden.
- **Single-tenant self-install.** No untrusted-code sandbox, no shared hosted tier.
- **Device-agnostic worker.** Keep the device target a parameter; `neuron` stays
  registered-but-disabled until TorchNeuron GAs.
- **The question is the load-bearing invariant.** Every experiment serves the
  user's question; nothing mutates it.
- **The brain proposes and interprets; it never accepts.** The human at "Go" is
  the acceptance node. Never auto-climb, never auto-declare success.

## Style

- **Go for everything except the worker** (Python hot path). Go 1.26+.
- **stdlib-first.** No web frameworks or DI containers. AWS SDK for Go v2 for AWS.
  `flag` for the CLI unless Cobra earns its place.
- **Reuse, don't reimplement** the spore.host suite (truffle / spawn / lagotto)
  behind `internal/spore` adapters.
- Errors wrapped with context (`fmt.Errorf("...: %w", err)`); no panics in library
  code.
- **Table-driven tests.** `device`, `sizing`, `catalog` must be fully unit-tested
  with no AWS calls.
- Apache 2.0 header on every file. Terse comments that explain *why*.

## Branches, commits, and PRs

- Branch off `main`; do not commit directly to `main`.
- [Conventional Commits](https://www.conventionalcommits.org/) are encouraged
  (`feat:`, `fix:`, `docs:`, `chore:`, `test:`, `ci:`).
- Every user-facing change updates `CHANGELOG.md` under `[Unreleased]`
  (Keep a Changelog format). CI checks this.
- Fill out the PR template, including the invariant checklist.

## Versioning & releases

[Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html). Pre-1.0.0,
anything may change between minor versions; breaking changes are flagged
`BREAKING:` in the changelog. Releases are cut by tagging `vX.Y.Z`, which must
have a matching `CHANGELOG.md` section; `release.yml` enforces this.

## Licensing

By contributing you agree your contributions are licensed under the Apache
License 2.0. Add the standard header to new files.
