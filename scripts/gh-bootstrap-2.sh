#!/usr/bin/env bash
# Second batch of foray issues: tests, docs, observability, hardening, and the
# bootstrap follow-ups. Idempotent; safe to re-run. Run after gh-bootstrap.sh.
#
# Copyright 2026 Scott Friedman. Apache License 2.0.
set -euo pipefail
REPO="${REPO:-scttfrdmn/foray}"

issue() {
  local title="$1" ms="$2" labels="$3" body="$4"
  local existing
  existing=$(gh issue list --repo "$REPO" --state all --search "\"$title\" in:title" --json title --jq ".[] | select(.title==\"$title\") | .title" 2>/dev/null | head -1)
  if [ -n "$existing" ]; then echo "    skip (exists): $title"; return; fi
  gh issue create --repo "$REPO" --title "$title" --milestone "$ms" --label "$labels" --body "$body" >/dev/null
  echo "    created: $title"
}

echo "--> issues (batch 2)"

# Bootstrap follow-ups
issue "ci: flip demo-fake + test from tracking to hard gate" "0 — Bootstrap" "type:ci,prio:p0" \
"CI currently runs \`go test ./...\` and \`make demo-fake\` with continue-on-error because the core packages are unimplemented. Once steps 1 + 6 land, remove continue-on-error so these become hard gates.

Tracks the honesty note in CHANGELOG."

issue "ci: build the full tree (drop the export-only vet scope)" "0 — Bootstrap" "type:ci,prio:p1" \
"\`vet-build\` only vets \`internal/export\` today because the rest doesn't compile. Once \`internal/sizing\` + brain types exist, switch to \`go vet ./...\` and \`go build ./...\`."

issue "docs: add a docs/ directory with a divergence-from-NDIF note" "0 — Bootstrap" "type:docs,good-first-issue,prio:p2" \
"Mirror the sibling Gauss project's divergence tracking: a short doc listing where foray deliberately differs from NDIF and why."

issue "chore: add CODEOWNERS" "0 — Bootstrap" "type:chore,good-first-issue,prio:p2" \
"Add .github/CODEOWNERS routing reviews to the maintainer."

issue "chore: publish spore.host dependencies (truffle/spawn/lagotto) or vendor interfaces" "3 — spore adapters" "type:chore,area:spore,prio:p1" \
"The spore.host modules are not yet importable under this account. Decide: publish them as Go modules, or keep \`internal/spore\` as adapter interfaces with a fake until they exist. go.mod currently has no spore require."

# Testing depth
issue "test: table-driven tests for catalog URI parsing" "2 — catalog" "type:test,area:catalog,prio:p1" \
"Cover hf id / s3:// / upload ref / invalid, with no AWS. Mirrors device/sizing test discipline (CLAUDE.md)."

issue "test: brain budget envelope vs Cedar ceiling are distinct and both hold" "6 — brain" "type:test,area:brain,prio:p1" \
"Extend plan_test.go: the per-question envelope (brain) and the per-session ceiling (Cedar) are separate and both enforced."

issue "test: cedar policy unit tests (allow/deny matrix)" "6 — brain" "type:test,area:brain,type:security,prio:p1" \
"Evaluate foray.cedar against a matrix: budget over/under, tier allowed/large, gradients with/without allowLargeSaves, neuron engine, export owner vs non-owner. Deny reasons asserted verbatim."

issue "test: export Denied surfaces the Cedar reason verbatim" "6 — brain" "type:test,area:export,prio:p2" \
"Assert \`Exporter.Export\` returns \`*Denied{Reason}\` with the policy reason unchanged when export is forbidden."

# Observability / ops
issue "forayd: structured request/session logging and a /healthz" "4 — forayd gateway" "type:feature,area:gateway,prio:p2" \
"Minimal observability for the gateway: structured logs keyed by session, a health endpoint, and last_request_time metrics. stdlib-first."

issue "ops: cost receipt persisted per question (DynamoDB)" "6 — brain" "type:feature,area:brain,prio:p2" \
"Persist the per-question receipt (rungs run, \$ spent vs envelope) so 'foray sessions' and the page can show \$-so-far accurately."

issue "deploy: teardown leaves nothing running or billing (verified)" "9 — deploy" "type:test,area:deploy,invariant,prio:p0" \
"\`make teardown\` must remove every data-plane resource and assert nothing is left billing. Enforces 'ephemeral by default' at the IaC layer."

# Worker / engine details
issue "worker: VLLM path rejects gradient requests with a clear error" "5 — worker" "type:feature,area:worker,prio:p1" \
"Paged-attention retains no autograd graph; a gradient request routed to VLLM must fail loudly, not silently differ. See ARCHITECTURE.md §3 routing rule."

issue "worker: container image + make worker target" "5 — worker" "type:feature,area:worker,prio:p1" \
"Build the single worker image (LanguageModel + VLLM) and wire \`make worker\`. Device target injected by the control plane."

# Web polish (deferred-adjacent but concrete)
issue "web: accessibility pass (reduced-motion, aria, keyboard)" "8 — web" "type:feature,area:web,good-first-issue,prio:p2" \
"app.js already honors prefers-reduced-motion; complete an a11y pass on the strata viz, the Go buttons, and the cost meter."

issue "web: wire the live cost meter to real estimates" "8 — web" "type:feature,area:web,prio:p2" \
"The header meter sums canned rung costs. Bind it to truffle-backed estimates from the brain once available."

# Security
issue "security: presigned URLs are single-object, short-TTL, least-privilege" "6 — brain" "type:security,area:export,prio:p1" \
"Ensure export presigning grants GET on exactly the session's objects, default 15-min TTL, from the user's own bucket only. No bucket-wide grants."

issue "security: document the IAM trust + permission boundary for spawn" "9 — deploy" "type:docs,type:security,area:deploy,prio:p2" \
"Document the least-privilege role spawn assumes to launch/terminate instances, scoped to foray-tagged resources."

echo "==> batch 2 done."
