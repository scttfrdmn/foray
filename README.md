# foray

**ADI — AWS Deep Inference.** Ephemeral remote access to the internals of any
open model, on right-sized EC2 GPUs, for the length of one experiment, in your
own AWS account. Then it's gone.

> [!NOTE]
> **Status: early (v0.x, pre-release).** The
> [ARCHITECTURE.md §10 build order is complete](#project-status) — all nine
> steps are on `main`, and **`make demo-fake` walks the full
> intent→plan→Go→run→assess→climb→receipt loop offline** (the MVP definition of
> done). The control plane has been deployed to a real account and torn down
> clean, with live Bedrock planning, a DynamoDB session round-trip proving the
> idle bridge fires, and a real presigned export.
>
> **The remaining gap is the GPU data plane end-to-end:** the gateway Lambda has
> no network path to the worker yet
> ([#66](https://github.com/scttfrdmn/foray/issues/66)), so a real trace against
> a live GPU is not validated. The worker's real path is exercised by hand via
> `make worker-smoke`, never in CI.

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

make demo-fake             # intent -> plan -> Go -> run -> assess -> climb -> receipt
                           # entirely offline, zero AWS calls (the CI gate)

make web-fake              # the same loop in the browser, still offline:
                           # serves web/ + the /api brain loop on localhost:8090
```

The page is a thin client over the real brain loop (`POST /api/propose` →
Go → `POST /api/approve`), so it needs that server — opening `web/index.html`
straight from disk leaves it with no API to call.

## Project status

Build order (see [ARCHITECTURE.md §10](ARCHITECTURE.md)). Each step is an issue
milestone; earlier steps have no AWS dependency. **All nine steps are on `main`.**

| Step | Component | State |
| --- | --- | --- |
| 0 | Bootstrap (repo, license, CI, layout) | ✅ done |
| 1 | `device` + `sizing` | ✅ done |
| 2 | `catalog` (hf / `s3://` / `upload:` resolver) | ✅ done |
| 3 | `spore` adapters (truffle / spawn / lagotto) | ✅ done |
| 4 | `forayd` gateway (the load-bearing contract) | ✅ done |
| 5 | `worker` (nnsight, Python) | ✅ done — fake path in CI; real GPU by hand (`make worker-smoke`) |
| 6 | `brain` (AgentCore + Cedar + HITL ladder) | ✅ done — live Bedrock verified |
| 7 | `foray` CLI (run / export / models / sessions / stop) | ✅ done |
| 8 | `web` static SPA | ✅ skeleton + live brain loop; polished UI deferred ([#28](https://github.com/scttfrdmn/foray/issues/28)) |
| 9 | `deploy` (IaC) | ✅ done — real apply/destroy hand-validated |

Since the build order closed: per-question cost receipts persisted to DynamoDB,
`truffle` bundled into the web-API Lambda so the deployed control plane can
price, and two invariants turned into build-failing CI gates — a static scan for
always-on infra, and a reflective test that fails if any trace-result boundary
struct grows a tensor-bearing field.

**Known gap:** gateway→worker reachability in the deployed control plane
([#66](https://github.com/scttfrdmn/foray/issues/66)) — VPC-attaching the Lambda
to reach a private worker would add hourly-billing endpoints/NAT and break the
~$0 control plane, so the fix has to preserve that invariant. Needs a real GPU to
validate.

Track everything in
[Issues](https://github.com/scttfrdmn/foray/issues) and
[Milestones](https://github.com/scttfrdmn/foray/milestones).

## Repository layout

```
cmd/foray/              the CLI (run / export / models / sessions / stop)
cmd/forayd/             the gateway daemon (also the Lambda entrypoint)
cmd/foray-web/          dev server: the SPA + the /api brain loop
internal/brain/         AgentCore plan/execute + the result-gated ladder
internal/brain/policy/  foray.cedar — the policy spine
internal/catalog/       model-source resolver (hf id / s3:// / upload:)
internal/device/        accelerator/instance abstraction (NVIDIA now, neuron gated)
internal/sizing/        footprint -> ranked hardware options
internal/gateway/       forayd: graph routing + the spawn idle bridge
internal/spore/         thin adapters over truffle / spawn / lagotto
internal/webapi/        the HTTP surface the page talks to
internal/export/        opt-in presigned download of your own saved values
worker/                 the nnsight worker (the one Python boundary)
web/                    the static SPA (S3 + CloudFront)
deploy/terraform/       IaC for the ~$0 control plane
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
