<!-- Thanks for contributing to foray. Keep the diff focused. -->

## What & why

<!-- What does this change do, and which issue/milestone does it serve? -->

Closes #

## Checklist

- [ ] `make lint` and `make test` pass (or failures are pre-existing and noted)
- [ ] `CHANGELOG.md` updated under `[Unreleased]` (Keep a Changelog)
- [ ] New files carry the Apache 2.0 header
- [ ] Tests added/updated (table-driven for `device`/`sizing`/`catalog`)

## Invariant check (see CLAUDE.md)

Confirm this PR does not violate any invariant:

- [ ] Ephemeral by default — nothing kept warm; no scheduler/resident catalog
- [ ] Control plane rests at ~$0 — no always-on server/broker/cluster
- [ ] Cost is per-session; budgets/ceilings respected
- [ ] No automatic egress — export stays opt-in, user-initiated, presigned
- [ ] Single-tenant — no untrusted-code sandbox, no shared hosted tier
- [ ] Device-agnostic — `neuron` stays registered-but-disabled
- [ ] The question stays the load-bearing invariant; brain never auto-accepts
