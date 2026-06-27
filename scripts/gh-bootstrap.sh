#!/usr/bin/env bash
# Idempotently populate the foray GitHub repo with labels, milestones, and
# issues that mirror ARCHITECTURE.md §10 (build order), the in-code TODOs, and
# the CLAUDE.md invariants (as CI-gate issues). Safe to re-run.
#
# Copyright 2026 Scott Friedman. Apache License 2.0.
set -euo pipefail

REPO="${REPO:-scttfrdmn/foray}"
echo "==> bootstrapping $REPO"

# ---------------------------------------------------------------------------
# Labels — full taxonomy (type / area / priority + specials).
# ---------------------------------------------------------------------------
label() { gh label create "$1" --repo "$REPO" --color "$2" --description "$3" --force >/dev/null; }

echo "--> labels"
# type
label "type:feature"   "1d76db" "New capability or implementation task"
label "type:bug"       "d73a4a" "Something behaves incorrectly"
label "type:chore"     "fef2c0" "Maintenance, deps, tooling"
label "type:docs"      "0075ca" "Documentation"
label "type:test"      "bfd4f2" "Tests and fixtures"
label "type:ci"        "5319e7" "CI/CD and automation"
label "type:security"  "b60205" "Security-relevant work"
# area (per ARCHITECTURE.md components)
label "area:device"    "c2e0c6" "Accelerator/instance abstraction"
label "area:sizing"    "c2e0c6" "Footprint -> hardware options"
label "area:catalog"   "c2e0c6" "Model source resolver"
label "area:spore"     "c2e0c6" "truffle/spawn/lagotto adapters"
label "area:gateway"   "c2e0c6" "forayd — the load-bearing contract"
label "area:worker"    "c2e0c6" "nnsight worker (Python)"
label "area:brain"     "c2e0c6" "AgentCore plan/execute + Cedar + ladder"
label "area:export"    "c2e0c6" "Opt-in presigned download"
label "area:cli"       "c2e0c6" "foray CLI"
label "area:web"       "c2e0c6" "Static SPA"
label "area:deploy"    "c2e0c6" "IaC / deployment"
label "area:deps"      "ededed" "Dependencies"
# priority
label "prio:p0"        "b60205" "Blocker / must-have for MVP"
label "prio:p1"        "d93f0b" "Important"
label "prio:p2"        "fbca04" "Nice to have"
# specials
label "invariant"               "5319e7" "Enforces a CLAUDE.md invariant"
label "deferred"                "cccccc" "Explicitly not now (ARCHITECTURE.md §9)"
label "licensed-workload-stub"  "e99695" "Blocked on a license agreement"
label "good-first-issue"        "7057ff" "Good entry point for newcomers"
label "blocked"                 "000000" "Blocked on another issue"

# ---------------------------------------------------------------------------
# Milestones — build steps (ARCHITECTURE.md §10) + bootstrap + MVP gate.
# ---------------------------------------------------------------------------
milestone() {
  local title="$1" desc="$2"
  local num
  num=$(gh api "repos/$REPO/milestones?state=all" --jq ".[] | select(.title==\"$title\") | .number" 2>/dev/null | head -1)
  if [ -z "$num" ]; then
    gh api "repos/$REPO/milestones" -f title="$title" -f description="$desc" --jq '.number'
  else
    echo "$num"
  fi
}

echo "--> milestones"
declare -A MS
MS[bootstrap]=$(milestone "0 — Bootstrap"        "Repo, license, layout, CI, tracking. (this commit)")
MS[device]=$(milestone    "1 — device + sizing"  "Pure logic, table-driven tests, no AWS.")
MS[catalog]=$(milestone   "2 — catalog"          "Model source resolver; unit-test URI parsing.")
MS[spore]=$(milestone     "3 — spore adapters"   "Wrap truffle/spawn/lagotto behind a FORAY_FAKE fake.")
MS[gateway]=$(milestone   "4 — forayd gateway"   "Graph routing + idle bridge. The load-bearing contract.")
MS[worker]=$(milestone    "5 — worker"           "nnsight server + GDS loader; cuda only.")
MS[brain]=$(milestone     "6 — brain"            "AgentCore plan/execute + Cedar + HITL ladder.")
MS[cli]=$(milestone       "7 — CLI"              "foray over the gateway + brain.")
MS[web]=$(milestone       "8 — web"              "Static SPA in the demo style.")
MS[deploy]=$(milestone    "9 — deploy"           "IaC: S3+CloudFront, API GW+Lambda, IAM, Cedar, DDB.")
MS[mvp]=$(milestone       "MVP — demo-fake green" "Full intent->plan->Go->fake-spawn->receipt, offline.")

# ---------------------------------------------------------------------------
# Issues — created only if an open/closed issue with the same title is absent.
# ---------------------------------------------------------------------------
issue() {
  local title="$1" ms="$2" labels="$3" body="$4"
  local existing
  existing=$(gh issue list --repo "$REPO" --state all --search "\"$title\" in:title" --json title --jq ".[] | select(.title==\"$title\") | .title" 2>/dev/null | head -1)
  if [ -n "$existing" ]; then
    echo "    skip (exists): $title"
    return
  fi
  gh issue create --repo "$REPO" --title "$title" --milestone "$ms" --label "$labels" --body "$body" >/dev/null
  echo "    created: $title"
}

echo "--> issues"

# --- Step 1: device + sizing ---
issue "device: implement accelerator/instance registry and Options()" "1 — device + sizing" "type:feature,area:device,prio:p0" \
"Implement \`internal/device\` so \`device_test.go\` passes.

Acceptance:
- [ ] \`Lookup(BackendNeuron)\` returns a registered-but-disabled provider
- [ ] \`Options(minHBM)\` returns tier-sorted \`Option\`s; neuron never surfaces
- [ ] Tier fit matches the table test (slice/small/mid/large by HBM)
- [ ] No AWS calls

Spec: \`internal/device/device_test.go\`. See ARCHITECTURE.md §6.3."

issue "device: register neuron backend, GA-gated and disabled" "1 — device + sizing" "type:feature,area:device,deferred,prio:p1" \
"Add \`internal/device/neuron.go\`: Trainium backend present in the registry but \`Enabled()==false\` until TorchNeuron GAs, and never returned by \`Options()\`.

See CLAUDE.md §Deferred and ARCHITECTURE.md §6.3. Verified by \`TestNeuronGatedByDefault\`."

issue "sizing: implement Size() footprint + ranked options" "1 — device + sizing" "type:feature,area:sizing,prio:p0" \
"Implement \`internal/sizing\` so \`footprint_test.go\` passes.

Acceptance:
- [ ] Logit lens with per-layer saves forces the eager engine and exceeds bare weights
- [ ] Gradients force eager and grow activation memory
- [ ] Many-prompt sweep chooses vLLM and sizes a KV pool
- [ ] \`residualStreamGB(m, allTokens)\` grows with all-tokens capture
- [ ] Returns ranked \`{tier, instance, utilization, \$/session}\` options

Spec: \`internal/sizing/footprint_test.go\`. See ARCHITECTURE.md §6.4."

# --- Step 2: catalog ---
issue "catalog: model source resolver (hf / s3:// / upload)" "2 — catalog" "type:feature,area:catalog,prio:p1" \
"Resolve a HF id, \`s3://\` URI, or uploaded object to a HF-format checkpoint the worker can load. Uploads are S3 objects.

Acceptance:
- [ ] URI parsing unit-tested (hf id, s3 uri, upload ref), no AWS
- [ ] Clear error on unsupported sources (mirrors Cedar's allowed sources)

See ARCHITECTURE.md §6.5."

# --- Step 3: spore adapters ---
issue "spore: truffle adapter (pricing/quota/discovery)" "3 — spore adapters" "type:feature,area:spore,prio:p1" \
"Thin adapter over the \`truffle\` binary for Spot pricing, quota, instance discovery. Backs every cost number. Fake under FORAY_FAKE=1. Do not reimplement truffle.

See ARCHITECTURE.md §6.6 and CLAUDE.md §Reuse."

issue "spore: spawn adapter (launch/TTL/idle/hibernate)" "3 — spore adapters" "type:feature,area:spore,prio:p0" \
"Adapter over \`spawn\`: launch <2min, TTL auto-termination, idle (fed by forayd), hibernation. Fake under FORAY_FAKE=1.

See ARCHITECTURE.md §6.6."

issue "spore: lagotto adapter (capacity watcher)" "3 — spore adapters" "type:feature,area:spore,prio:p2" \
"Adapter over \`lagotto\` for the scarce multi-GPU case (watch-and-grab). Fake under FORAY_FAKE=1.

See ARCHITECTURE.md §6.6."

# --- Step 4: forayd gateway ---
issue "forayd: route serialized nnsight graph to the live worker" "4 — forayd gateway" "type:feature,area:gateway,prio:p0" \
"Accept a serialized nnsight intervention graph over HTTP and route it to the session's worker. Maintain session<->instance mapping (DynamoDB).

This is the single load-bearing new contract. See ARCHITECTURE.md §6.1."

issue "forayd: bridge per-session last_request_time into spawn idle" "4 — forayd gateway" "type:feature,area:gateway,invariant,prio:p0" \
"Emit \`last_request_time\` per session so spawn consumes request-activity instead of OS idle heuristics — a model-holding-HBM worker must not be reaped between traces. Short idle-grace post-trace.

The load-bearing contract (ARCHITECTURE.md §6.1). Enforces the ephemeral-but-not-prematurely-reaped behavior."

# --- Step 5: worker ---
issue "worker: nnsight FastAPI server (LanguageModel + VLLM routing)" "5 — worker" "type:feature,area:worker,prio:p0" \
"Python worker: deserialize graph, run interleaved with the forward pass, return saved values to S3 in-region. Route per §3: gradients/exotic -> LanguageModel; throughput -> VLLM. One image, device target passed in.

See ARCHITECTURE.md §3, §6.7."

issue "worker: GDS loader streams weights S3 -> HBM on boot" "5 — worker" "type:feature,area:worker,prio:p1" \
"GPUDirect Storage loader (sharded for multi-GPU) so cold-start is seconds, not a reason to keep models warm.

See ARCHITECTURE.md §6.7."

issue "worker: device target parameterized (cuda now, neuron later)" "5 — worker" "type:feature,area:worker,deferred,prio:p2" \
"Keep the device target a parameter so neuron slots in unchanged when TorchNeuron GAs. cuda only ships.

See CLAUDE.md invariant 'Device-agnostic worker'."

# --- Step 6: brain ---
issue "brain: define core types (Brain, Ladder, Rung, Proposal, Result)" "6 — brain" "type:feature,area:brain,prio:p0" \
"Define the brain's types and the Plan/Policy/Exec seams that \`fake.go\` and \`plan_test.go\` already use. Makes \`internal/brain\` and \`cmd/foray\` compile.

Acceptance:
- [ ] \`plan_test.go\` passes (ladder walk: climb then stop; budget envelope forces stop)
- [ ] \`Propose\`/\`Approve\`/\`Assess\`/\`NextProposal\` behave per the test

See ARCHITECTURE.md §6.2."

issue "brain: real AgentCore PlanLadder (question -> cheapest-first rungs)" "6 — brain" "type:feature,area:brain,prio:p1" \
"Real Bedrock AgentCore planner: question -> ordered Ladder of Rungs, or a clarifying question when underdetermined. Cheapest-first (GPT-2 -> 8B -> 70B).

See ARCHITECTURE.md §6.2."

issue "brain: Cedar policy evaluation per rung (CedarPolicy)" "6 — brain" "type:feature,area:brain,type:security,prio:p0" \
"Wire the Cedar Go SDK to evaluate \`internal/brain/policy/foray.cedar\` per rung: allowed sources, instance tiers, budget ceiling, gradient/large-save and neuron gates. Deny reasons surface verbatim.

Finalize the entity schema (TODO in foray.cedar). See ARCHITECTURE.md §6.2, §8."

issue "brain: Assess recommends climb/stop with honest negatives" "6 — brain" "type:feature,area:brain,invariant,prio:p0" \
"After a rung, recommend climb or stop tied to the finding and the question. Stop on honest negatives. A recommendation, never an action — the human decides. Cap the whole ladder by the per-question envelope (distinct from Cedar's per-session ceiling).

Enforces 'the brain never settles its own acceptance' and 'cheap before expensive'. See ARCHITECTURE.md §6.2."

# --- Step 7: CLI ---
issue "cli: wire expert flags (--model/--technique/--engine/--hardware/--budget)" "7 — CLI" "type:feature,area:cli,prio:p1" \
"\`cmd/foray/main.go\` parses these flags but discards them (\`_ = ...\`). Wire them into the brain's expert path so users can skip the dialog.

See ARCHITECTURE.md §5 (progressive disclosure)."

issue "cli: implement 'models' (list resolvable sources)" "7 — CLI" "type:feature,area:cli,good-first-issue,prio:p2" \
"Replace the TODO stub: list resolvable model sources (hf / s3:// / upload) via the catalog."

issue "cli: implement 'sessions' (age, TTL, \$-so-far)" "7 — CLI" "type:feature,area:cli,prio:p2" \
"Replace the TODO stub: list running sessions with age, TTL, and \$-so-far (from forayd/spawn)."

issue "cli: implement 'stop <session>'" "7 — CLI" "type:feature,area:cli,prio:p2" \
"Replace the TODO stub: stop a session (or let idle reap it) via the spawn adapter."

issue "cli: real (non-fake) path wiring for 'run'" "7 — CLI" "type:feature,area:cli,prio:p1" \
"Today \`foray run\` without FORAY_FAKE=1 prints 'not wired'. Wire AgentCore/Cedar/spawn for the real loop.

See cmd/foray/main.go runCmd."

# --- export ---
issue "export: S3Presigner with zip-on-demand for KindBundle" "6 — brain" "type:feature,area:export,prio:p1" \
"Implement the real presigner (TODO in export.go): presigned S3 GET for single objects; zip saves + outputs + nnsight + manifest.json for KindBundle, in the user's own bucket.

See ARCHITECTURE.md §6.9."

issue "export: CedarExportPolicy evaluates the 'export' action" "6 — brain" "type:feature,area:export,type:security,prio:p1" \
"Evaluate the Cedar \`export\` action against session + user so an org can disable export where data must stay in-region. Permit-by-default for the owner; deny via \`allowExport==false\`.

See foray.cedar and ARCHITECTURE.md §6.9."

# --- Step 8: web ---
issue "web: serve the real loop against forayd/brain (replace canned data)" "8 — web" "type:feature,area:web,prio:p2" \
"The page runs the loop client-side with canned data (mirrors demo-fake). Wire it to the real API Gateway endpoints once the brain/gateway land; keep the rehearsal mode as the offline path.

See ARCHITECTURE.md §6.8."

issue "web: polished Workbench UI" "8 — web" "type:feature,area:web,deferred,prio:p2" \
"Skeleton + style contract ships now; the polished build is a follow-on.

See ARCHITECTURE.md §9 (deferred)."

# --- Step 9: deploy ---
issue "deploy: IaC for the control plane (S3+CloudFront, API GW+Lambda, IAM, Cedar, DDB)" "9 — deploy" "type:feature,area:deploy,prio:p1" \
"Stand up the ~\$0 control plane and the make deploy/teardown targets. Resting cost must be a static bucket + cold Lambdas.

See ARCHITECTURE.md §2, §10."

issue "deploy: guard against unresolved PLACEHOLDER / LICENSED_WORKLOAD_STUB" "9 — deploy" "type:ci,area:deploy,invariant,prio:p0" \
"A pre-deploy check that fails if any PLACEHOLDER_* token or unresolved LICENSED_WORKLOAD_STUB remains. Mirrors the sibling Gauss project's discipline.

Enforces a deploy-time invariant."

# --- Invariant CI gates ---
issue "invariant gate: control plane has no always-on server/broker/cluster" "MVP — demo-fake green" "type:ci,invariant,prio:p1" \
"CI check that no always-on server, queue broker, or Ray/K8s cluster is introduced. Enforces 'Control plane rests at ~\$0'. (e.g. denylist of dependencies / IaC resource types.)"

issue "invariant gate: no automatic egress of activations" "MVP — demo-fake green" "type:ci,invariant,type:security,prio:p0" \
"CI/test check that the only egress path is the user-initiated, presigned export — never auto-streamed tensors on a trace (the eDIF anti-pattern). Enforces the no-auto-egress invariant."

issue "invariant gate: neuron stays out of the public menu" "1 — device + sizing" "type:test,invariant,prio:p1" \
"Keep \`TestNeuronGatedByDefault\` green in CI as a standing gate; add a Cedar test that \`engine == neuron\` is denied. Enforces 'device-agnostic but neuron GA-gated'."

issue "invariant gate: brain never auto-climbs or auto-declares success" "6 — brain" "type:test,invariant,prio:p0" \
"Test that no code path advances a rung or accepts a result without an explicit human Go. Enforces 'the brain proposes and interprets; it never accepts'."

issue "invariant gate: every proposal carries a \$/session estimate <= ceiling" "6 — brain" "type:test,invariant,prio:p1" \
"Test that a proposal over the Cedar budget ceiling is refused and that every surface shows \$/session. Enforces 'cost is per-session' + budget ceilings."

# --- Bootstrap / MVP cross-cutting ---
issue "make demo-fake green end-to-end in Go" "MVP — demo-fake green" "type:test,area:cli,prio:p0" \
"Once device+sizing (step 1) and brain types (step 6) land, \`make demo-fake\` must walk intent->plan->Go->fake-spawn->receipt with zero AWS and exit 0. This is the CI gate and MVP definition of done.

Depends on the step 1 and step 6 issues."

echo "==> done. Review: https://github.com/$REPO/issues"
