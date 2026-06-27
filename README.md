# foray

**ADI — AWS Deep Inference.** Ephemeral remote access to the internals of any
open model, on right-sized EC2 GPUs, for the length of one experiment, in your
own AWS account. Then it's gone.

> [!NOTE]
> **Status: scaffolding (v0.x, pre-release).** This repository currently holds
> the architecture, the policy spine, the static web demo, the fake-mode
> plumbing, and the test specifications for the core packages. The
> implementation of those packages is tracked as issues and milestones — see
> [Project status](#project-status). `go build ./...` and `go test ./...` are
> **not** green yet by design: the test files specify packages
> (`device`, `sizing`) that are not implemented, and `brain`/`cmd/foray` depend
> on them. The **static web page** (`web/index.html`) runs the full loop today
> with canned data; the Go `make demo-fake` path turns green once steps 1 and 6
> land (the brain's fake ladder is already written and waiting on `sizing`).

[![CI](https://github.com/scttfrdmn/foray/actions/workflows/ci.yml/badge.svg)](https://github.com/scttfrdmn/foray/actions/workflows/ci.yml)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/scttfrdmn/foray.svg)](https://pkg.go.dev/github.com/scttfrdmn/foray)

---

## The idea

NDIF gives researchers free remote access to model internals by keeping a fixed
catalog of large models resident on a small, fixed national cluster. That
architecture is a response to **scarcity**: two GPU types, a shared allocation,
queue fairness, hot/warm rationing. `foray` is the same capability with the
scarcity assumption removed. There is no standing fabric. Nothing is kept warm.
You name an experiment, the right instance is summoned, it runs, and it
disappears.

**All you need is an AWS account.**

| NDIF (scarcity) | foray (elasticity) |
| --- | --- |
| Fixed 4-model catalog | Any model: HF id, S3 URI, or upload |
| Two GPU types | The full EC2 NVIDIA menu, right-sized per model |
| Hot/warm tiers (rationing) | Per-session provisioning; nothing kept warm |
| Ray scheduler + eviction | TTL + idle; no scheduler |
| Shared multi-tenant sandbox | Single-tenant self-install; no sandbox needed |
| Activations downloaded to client | Activations stay in-region; only pixels leave |
| Free-but-queued (weeks) | Cheap-and-instant (seconds to first token) |

See **[ARCHITECTURE.md](ARCHITECTURE.md)** for the full design and
**[CLAUDE.md](CLAUDE.md)** for the working contract / invariants.

## Two planes

- **Control plane — always up, costs ~nothing.** Static SPA (S3 + CloudFront),
  Bedrock AgentCore (the brain), API Gateway + Lambda + DynamoDB (glue). Resting
  cost is a static bucket and some cold Lambdas.
- **Data plane — ephemeral, per session.** `spawn` launches the right GPU, the
  `nnsight` worker holds the model and runs interventions, saved values land in
  S3 in-region, and the instance self-terminates on idle.

The number that matters is **$/session, not $/hour**.

## Quickstart

```bash
git clone https://github.com/scttfrdmn/foray
cd foray

# Today: the static page runs the whole loop client-side with canned data.
open web/index.html        # or: python3 -m http.server -d web

# Once steps 1 + 6 land (device/sizing + brain), the Go offline loop:
make demo-fake             # intent -> plan -> Go -> run -> receipt, zero AWS (CI gate)
```

## Project status

Build order (see [ARCHITECTURE.md §10](ARCHITECTURE.md)). Each step is an issue
milestone; earlier steps have no AWS dependency.

| Step | Component | State |
| --- | --- | --- |
| 0 | Bootstrap (repo, license, CI, layout) | ✅ this commit |
| 1 | `device` + `sizing` | 🔲 tests written, impl tracked |
| 2 | `catalog` | 🔲 tracked |
| 3 | `spore` adapters (truffle / spawn / lagotto) | 🔲 tracked |
| 4 | `forayd` gateway (the load-bearing contract) | 🔲 tracked |
| 5 | `worker` (nnsight, Python) | 🔲 tracked |
| 6 | `brain` (AgentCore + Cedar + HITL ladder) | 🟡 fake path present |
| 7 | `foray` CLI | 🟡 fake path present |
| 8 | `web` static SPA | 🟡 skeleton + style contract |
| 9 | `deploy` (IaC) | 🔲 tracked |

Track everything in
[Issues](https://github.com/scttfrdmn/foray/issues) and
[Milestones](https://github.com/scttfrdmn/foray/milestones).

## Repository layout

```
cmd/foray/              the CLI (run / export / models / sessions / stop)
internal/brain/         AgentCore plan/execute + the result-gated ladder
internal/brain/policy/  foray.cedar — the policy spine
internal/device/        accelerator/instance abstraction (NVIDIA now, neuron gated)
internal/sizing/        footprint -> ranked hardware options
internal/export/        opt-in presigned download of your own saved values
web/                    the static SPA (S3 + CloudFront)
ARCHITECTURE.md         the full design
CLAUDE.md               the working contract / invariants
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). The invariants in
[CLAUDE.md](CLAUDE.md) are hard rules. This project follows
[Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html) and
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) — see
[CHANGELOG.md](CHANGELOG.md).

## Security

See [SECURITY.md](SECURITY.md). foray is single-tenant by design: you run your
own code on your own ephemeral GPU in your own account. There is no
untrusted-code sandbox and no automatic egress of activations.

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
Copyright 2026 Scott Friedman.
