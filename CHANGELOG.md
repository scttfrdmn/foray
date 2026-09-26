# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html).

While the project is pre-1.0.0, the public API and behavior may change in any
release. Breaking changes will be called out under **Changed** with a `BREAKING:`
prefix.

## [Unreleased]

### Added

- **`foray deploy`: the HTTP API — routes, `$default` stage and Lambda invoke
  permissions (#85, increment 3b).** Completes the request path: CloudFront aside,
  the deployed control plane can now serve.
  - Three routes matching `api.tf`: `ANY /sessions/{proxy+}` → the gateway,
    `ANY /api/{proxy+}` and `GET /healthz` → the web API. One integration per
    *function* rather than per route, so `/api/*` and `/healthz` share the web API's.
    A route pointing at a stale integration is retargeted rather than left — that
    failure deploys cleanly and then 404s every request from a function that has
    never heard of the path.
  - **The API is one resource, not several.** Its id is *generated*, unlike every
    other resource here whose identity is constructed from the config — so the unit
    that owns the id owns everything needing it (integrations, routes, stage,
    permissions) rather than threading discovered state between resources.
    Integrations are matched by their target function ARN and routes by route key,
    since those are the only stable identities they have.
  - **Invoke permissions are scoped to this API's execution ARN** (`/*/*`).
    Granting `apigateway.amazonaws.com` without a `SourceArn` would let any API in
    any account invoke the functions. The statement id is fixed so a re-apply
    converges one statement instead of accumulating them, and teardown removes them
    explicitly — they live on the *functions*, so deleting the API alone would
    strand them.
  - `$default` stage with `AutoDeploy`: the stage-less path is what lets CloudFront
    forward `/api/*` straight through, and without auto-deploy a newly added route
    404s with nothing explaining why. Access logging captures
    `$context.integrationErrorMessage` — the field that reports "the Lambda never
    became ready", which was invisible in #64.
  - API Gateway does not enforce unique names, so two APIs named `foray` are
    *reported* rather than guessed between; silently picking one would have
    successive deploys converge different APIs.
  - Adds `service/apigatewayv2`. The drift guard now covers `api.tf` (route keys,
    stage, payload format, integration type, log group, the access-log field) and the
    invoke permission's statement id and principal.

- **`foray deploy`: the two Lambda functions, their log groups, and automatic
  Lambda Web Adapter layer resolution (#85, third increment).**
  - **The LWA layer ARN is now resolved at deploy time** instead of pasted into a
    tfvars file. Keeping the adapter is deliberate: it is why `cmd/forayd` and
    `cmd/foray-web` run on Lambda as *unmodified* `http.Server` binaries with no
    `aws-lambda-go` and no proxy shim. What it cost was a region-specific, versioned
    ARN whose mismatch is a **silent** failure — the readiness check never passes and
    API Gateway returns 503 with nothing in the application log (PR #64). Resolution
    removes that friction without giving up the property; `--lwa-layer-arn` still
    pins it for an air-gapped account, a private mirror, or a known-good version.
    Resolution happens *before* anything is created, so an unresolvable layer fails a
    deploy that has not yet touched the account.
  - **`AWS_LWA_PORT` is set per function** (gateway 8080, web API 8090) with the
    constants adjacent and a test asserting they differ — this is the #64 bug, and
    the `foray deploy` output now shows both ports so a shared value is visible at a
    glance.
  - **`CreateFunction` retries IAM propagation.** IAM is eventually consistent and
    this package creates the roles itself, so a first deploy almost always hits the
    window where Lambda rejects a role as not-yet-assumable. That specific error is
    retried; anything else fails fast, so a typo does not look like a hang.
  - Code is republished only when the package's sha256 differs from the deployed
    one, and configuration (env, layer, timeout, memory) converges on every run —
    the LWA env in particular is what makes the function respond at all.
  - **Log groups are created explicitly**, before their functions. A group Lambda
    creates implicitly on first invocation retains **forever**, and log storage is
    one of the few things here that bills by the GB-month with no TTL of its own;
    creating them also gives teardown something to delete. Retention converges on
    existing groups for exactly the implicit-creation case. Tagging a log group is
    best-effort, since some accounts restrict it and nothing that bills is tracked
    only by that tag.
  - Adds `service/lambda` and `service/cloudwatchlogs`.
  - The drift guard now covers `lambda.tf` too — function names, **both LWA ports**,
    the exec wrapper and readiness path, runtime, architecture, the bundled-truffle
    `PATH`, and the log group names. A port fixed in one path and not the other would
    otherwise bring the silent 503 back on the next deploy the other way.

- **`foray deploy`: the three IAM roles and the spawn instance profile (#85,
  second increment).** Mirrors `deploy/terraform/iam.tf` statement for statement:
  - `foray-gateway-lambda` — forayd: the sessions table and nothing else. Four
    operations, no `Scan` (nothing enumerates — `/healthz` deliberately omits the
    enumerator capability for this reason) and no `DeleteItem` (rows expire by TTL).
  - `foray-webapi-lambda` — the page's API: the table, Bedrock to plan, read-only
    Spot pricing for truffle, the data bucket's `sessions/` prefix to presign
    exports, and `PassRole` for the spawn role.
  - `foray-spawn-instance` (+ instance profile) — the GPU: write its saves under
    `sessions/`, and terminate or stop *itself*.
  - **Teardown order is load-bearing.** IAM refuses to delete a role that still
    carries inline or attached policies, and refuses to delete an instance profile
    still holding a role — so the profile comes off before its role, and both
    policy sets are *enumerated* (not assumed from our own list) so a role that
    drifted still tears down cleanly. The fakes model both refusals, and the
    ordering is asserted on the action sequence rather than the end state.
  - Adds `service/iam` and `service/sts` (the latter resolves the account id for
    the policy ARNs, and doubles as an early credential check — a deploy with bad
    credentials now fails before creating anything).
  - ARNs are constructed rather than read back, which removes both an API
    round-trip and a dependency edge. Partition is hardcoded `aws`, matching what
    `iam.tf` already does.

- **A drift guard between the two deployment paths.** They are maintained in
  parallel, so `internal/deploy/drift_test.go` compares the *permission surface*
  against `deploy/terraform/iam.tf` in both directions: an action granted only in
  Go means the alternate path deploys a control plane that cannot do its job, and
  one granted only in Terraform means `foray deploy` is under-permissioned (an
  `AccessDenied` at run time). Role names are checked too — a mismatch would have
  the two paths create different roles, so a teardown by one leaves the other's
  behind. Verified to fail in both directions.

- **`foray deploy` / `foray teardown` — the primary deployment path (#85, first
  increment).** `internal/deploy` provisions the control plane directly through
  the AWS SDK, so Terraform stops being a prerequisite for "all you need is an AWS
  account". `deploy/terraform/` remains the documented alternate and is still the
  complete path; the verb is taking over incrementally and covers **storage and
  session state** so far (DynamoDB sessions table, both S3 buckets). Lambda +
  API Gateway, IAM, and CloudFront + web sync follow. Shape borrowed from lagotto,
  the spore.host tool in the same position.
  - **No state file, so everything is discovered before it is created.** `Apply` is
    re-runnable by construction — which matters more here than for Terraform, since
    without state a half-finished deploy can only be fixed by running it again.
    `Apply` stops at the first failure (later resources depend on earlier ones) and
    returns what it completed, because there is nothing else for the caller to
    consult.
  - **`Teardown` does not stop at the first failure**, since the requirement is
    that nothing is left billing — one stubborn resource must not shield the rest.
    It empties buckets before deleting them (S3 refuses otherwise, and stored bytes
    bill), and surfaces `DeleteObjects` per-key failures, which the API reports in
    a 200 body.
  - Every resource carries `Project=foray`, which `scripts/teardown-verify.sh`
    asserts on. Terraform got this from `default_tags`; here it is per-call, and an
    untagged resource is the one way this can fail silently — so a test pins it.
  - `--dry-run` reports the plan and calls no mutating API. `FORAY_FAKE=1` walks the
    real resource list and ordering over in-memory AWS stand-ins, so `make
    deploy-fake` (new, and a new CI gate) rehearses deploy *and* teardown offline.
  - `foray teardown` names what is irreversible before asking — emptying the data
    bucket destroys saved activations the user cannot regenerate — and inherits
    #87's refusal to read an unanswerable prompt as consent, so a non-tty teardown
    declines rather than wiping the bucket.
  - Handles the S3 edges that bite: the `LocationConstraint` must be sent for every
    region *except* us-east-1 (an error there, and silently wrong elsewhere);
    `BucketAlreadyOwnedByYou` is idempotent success while `BucketAlreadyExists` and
    a 403 from `HeadBucket` mean the global name is taken and say so. The
    export-bundle lifecycle rule filters on the **tag**, never a prefix — a prefix
    rule over `sessions/` would expire the user's saves.

- CI: **`scripts/invariant-check.sh` now also scans `internal/deploy/`** (#31).
  The always-on gate previously looked only at `deploy/terraform/`, so moving
  provisioning into Go would have made it half-blind. It now denies hourly-billing
  service clients (RDS, ECS/EKS, ElastiCache, MSK, load balancers, …) and calls
  (`RunInstances`, `CreateNatGateway`, `CreateDBInstance`, …) in the deploy package,
  and asserts `CreateTable` uses `BillingModePayPerRequest` — the same rule the
  `.tf` files carry. Each of the three new checks was verified to fail on an
  injected violation.

### Changed

- **foray now terminates a session's instance when the rung ends** (#80), which is
  what actually reaches $0. spawn's idle action only *stops* an instance and a
  stopped instance keeps billing its EBS volumes, so the previous behavior — leave
  it to idle, then TTL — billed storage for the remainder of the TTL window
  (~1h55m with the shipped 5m idle / 2h TTL defaults). The control plane knows
  precisely when a session is done, so it says so.
  - Termination happens on **every** exit path, including a failed trace: the
    per-rung work moved into `runRung`, whose `defer`s close the tunnel and then
    reap the instance. This matters because `runLoop` reports errors through
    `die()` → `os.Exit`, which runs no defers — previously a failed trace would
    have leaked a GPU until TTL. Pinned by tests, verified to fail without the
    cleanup.
  - New `foray run --keep` leaves each rung's instance up for a session you want
    to poke at. Idle and TTL remain the backstops for anything that escapes the
    normal path (a crashed CLI, a lost network), which is why every launch still
    carries both.
  - A failed termination is reported, not fatal — the rung's finding is already in
    hand and TTL still bounds the instance, so the message names the session to
    clean up rather than discarding the result.

- **Export ownership no longer depends on the instance being alive** (#80).
  `export.SessionOwner` resolves ownership from the presence of the session's
  saves under `sessions/<id>/` in the user's own data bucket, replacing a resolver
  that asked spawn whether the instance still existed. That coupling was backwards:
  it would have denied export the moment a session ended — and the run output
  prints `download: foray export <session>` — while liveness was never what
  ownership meant. Object presence is durable, survives termination, and directly
  answers the question export asks. Fails closed on a listing error, scopes the
  listing to the one session prefix with `MaxKeys=1`, and `Exists` is split out so
  the CLI can say "no saved values found for session X" instead of Cedar's
  misleading "only the session owner may export". Wired into both `cmd/foray` and
  `internal/webapi`; the Cedar gate itself is unchanged and still governs the
  residency case (`allowExport == false`).

### Fixed

- `cmd/foray`: **an unattended run no longer approves its own rungs** (#87). The
  Go prompt read an unreadable stdin as approval: a closed pipe or absent tty
  returns `io.EOF` with an empty string, which is the *same value* a bare Enter
  produces, and the error was discarded — so "nobody is there" meant "yes".
  `foray run "q" < /dev/null` with **no `--yes` at all** approved every rung and
  launched a GPU per rung, with no human at the acceptance node. That is the
  invariant CLAUDE.md calls load-bearing ("the human at Go is the acceptance
  node"; "no GPU launches before approval"), so it is a refusal now: an absent
  human is not an approving one, and the prompt says to pass `--yes` (or
  `--force`) if pre-authorization was the intent. A person at a terminal is
  unaffected — bare Enter still defaults to yes, because interactive Enter returns
  `"\n"` with a nil error. `confirmFrom` takes an injectable reader so the
  behavior is testable without a tty; the table covers EOF, a read error, and
  every interactive answer, and was verified to fail with the guard made inert.
  Same prompt backs `foray stop`, which gains the same protection.

- `cmd/foray`: **flags written after the question no longer get silently dropped.**
  stdlib `flag` stops parsing at the first positional argument, so
  `foray run "why does it refuse X?" --yes` discarded `--yes` — which is how a
  person types it, how the README shows it, and how `make demo-fake` invokes it.
  The gate was passing only because an unreadable stdin happened to read as
  approval, not because `--yes` took effect (`--yes` now prints `Go (auto)` there,
  as it always should have). Found while adding `--keep`, which would have been
  unusable in the position everyone writes flags. `parseWithPositionals` uses the
  canonical stdlib parse/positional/re-parse loop; table-driven test covers flags
  before, after, and on both sides of the question.

### Documentation

- Corrected a claim that was simply false: **spawn's idle action stops an
  instance, it never terminates one** (`--on-idle` explicitly refuses
  `terminate`, and only TTL destroys an instance). ARCHITECTURE.md §2/§4, the
  CLAUDE.md "ephemeral by default" invariant and the README all said idle
  terminates and reached $0; a stopped instance costs no compute but **keeps
  billing its EBS volumes**, so $0 arrives at TTL. Idle is the fast brake, TTL is
  the guarantee. `foray stop`'s decline message said "idle + TTL will still reap
  it", which promised a $0 that does not arrive until TTL. Issue #80 tracks the
  behavior change (terminating deliberately at end-of-session); this is the
  documentation half, correct regardless of which fix lands.

### Changed

- The worker's Python is managed with **uv**. `worker/pyproject.toml` is now the
  single source of truth for dependencies — base deps in `[project]`, the GPU stack
  as a `gpu` extra, pytest/ruff as a `dev` dependency-group — and a committed
  `worker/uv.lock` pins the resolution so CI, the image and a laptop install the
  same thing. `worker/requirements.txt` and `worker/requirements-gpu.txt` are
  removed.
  - Make targets run through uv: `worker-sync` (new), `worker-test`, `worker-lint`
    (new), `worker-fake`, `worker-smoke`, plus `worker-serve-fake` (new) which runs
    the worker the way `spawn service` does so the readiness contract can be
    eyeballed offline. uv runs from the repo root with `--project worker`, because
    the package imports as `worker.*`.
  - CI installs uv via the SHA-pinned `astral-sh/setup-uv` (the spore.host house
    convention) and runs `uv sync --project worker --locked`, which **fails on a
    stale lock** — a dependency change can no longer land without its lock update.
  - `worker/Dockerfile` installs with `uv sync --locked --extra gpu --no-dev`
    (pinned uv copied from its official image rather than fetched by a shell
    script), so the image and CI cannot silently diverge and test tooling stays out
    of a production image.
  - New repo-root `.dockerignore`. The image builds from the repo root, so without
    it a developer's `worker/.venv` would be copied over the environment `uv sync`
    just built inside the image — a wrong-platform venv that fails at run time
    rather than build time. It also keeps the Go artifacts and IaC state out of the
    build context.

### Added

- `internal/spore` + `worker/serve.py`: the CLI now reaches the GPU worker through
  **`spawn service`** — spawn's verb for running a long-lived HTTP service on an
  instance and tunneling to it (issue #66, the deploy gap's second half). The
  worker binds the instance's **loopback** and is reachable only through an SSH
  forward, so nothing is exposed to the internet, no security-group port opens,
  and no VPC/interface endpoint/NAT is involved — the control plane stays at ~$0.
  This supersedes the "public IP + bearer token" shape the issue originally
  proposed, which would have been a weaker posture for the same money and would
  have reimplemented what spawn already does.
  - `spore.Server`/`ServeSpec`/`Service` wrap the verb, with a `Starter`/`Proc`
    seam for the long-lived child process (`Runner` waits for exit, so it could
    not host a held-open tunnel). `Serve` reads spawn's one-line JSON result for
    `local_url` and bounds the wait with `--boot-timeout`
    (`DefaultServeBootTimeout` 10m, since streaming weights dominates boot).
    spawn's flags are terminated with `--` before the service command: spawn does
    not disable flag interspersion, so `python3 -m worker.serve` would otherwise
    be rejected for the `-m`.
  - `worker/serve.py` implements spawn's readiness contract — bind, then announce
    `{"event":"ready","addr":…,"token":…}` on one flushed line of stdout. It
    **refuses to bind anything but loopback**, since the tunnel is the only
    intended way in.
  - `worker/app.py` requires that per-session token on `/trace`, accepting
    `Authorization: Bearer …` (what foray sends) or `?token=…` (what the URL spawn
    prints carries, so a pasted URL works), compared with `secrets.compare_digest`.
    `/healthz` stays open.
  - `gateway.HTTPWorker` splits the token out of the session's worker URL and
    sends it as a header, so the credential never appears in the endpoint strings
    that errors quote. The plain `http://host:8000` shape still works unchanged.
  - `cmd/foray`'s `tracer` owns the tunnel: `register` opens it per rung and
    `close` tears it down before the next rung launches its own instance, so a
    forward never outlives its session. The instance lifecycle is untouched — its
    TTL and idle timeout remain the guarantee, since stopping a tunnel is only a
    request.

### Documentation

- `README.md`: refreshed the project-status section, which still described steps
  2–5 and 9 as unbuilt and 6–8 as partial. All nine build-order steps are on
  `main`; the table now says so, notes the post-build-order work (cost receipts,
  bundled truffle, the two invariant CI gates), and names the one real remaining
  gap (#66 gateway→worker reachability). Also fixed a stale Quickstart: the page
  is now a thin client over `/api`, so `open web/index.html` leaves it with no
  API to call — `make web-fake` is the offline browser path. Repository-layout
  block completed (it omitted `catalog`, `gateway`, `spore`, `webapi`, `worker`,
  `deploy`, and both new `cmd/` entrypoints).

### Fixed

- `internal/spore`: the adapters' JSON contracts now match what the spore.host
  CLIs actually emit. The tags were hand-inferred and marked
  `TODO(verify-json)`, with the fake as the test source of truth — so four wrong
  guesses decoded to zero values against real output while CI stayed green:
  - **`Instance.PublicDNS` → `PublicIP`.** `spawn list -o json` reports
    `public_ip` and has no `public_dns` key at all, so the worker URL was built
    from an empty host on *every* real trace. The `host == "" → instance ID`
    fallback in `cmd/foray` and `internal/webapi` turned that into an
    unresolvable `http://i-0abc:8000` instead of an error; `workerURL` now
    returns an error when there is no address.
  - **`ttl_deadline`/`idle_deadline` → `ttl`/`idle_timeout`.** spawn reports
    durations from launch, not absolute deadlines, so `foray sessions` silently
    dropped its TTL column. `Instance.TTLDeadline()` now derives it from
    `LaunchedAt + TTL`.
  - **`Quota{limit, in_use}` → `{quota_vcpus, usage_vcpus, available_vcpus}`**
    (truffle `quotaRow`). Every quota check read 0/0.
  - **`Discover` parsed `[]string`.** `truffle find -o json` emits instance-type
    *objects*, so the call failed outright; names are now extracted and
    de-duplicated in truffle's ranking order.
  - **`Status` now answers from `spawn list`, not `spawn status`.** The latter
    proxies the in-instance `spored` daemon's own document over SSH/SSM — a
    different shape, and unreachable from a cold Lambda. `spawn list` uses the
    EC2 API and works wherever the control plane runs.

  A new `internal/spore/wire_test.go` pins each contract against fixtures taken
  from the *tools'* own emitting code, so the fake can no longer mask drift.

- `internal/spore`: `KeepWarm` no longer shells out to `spawn extend`. That verb
  moves the **hard TTL** (it rewrites `spawn:ttl` and the termination deadline),
  not the idle timer, so it never did the idle bridge's job — and because TTL
  accumulates from launch, calling it once per trace walked the hard terminate
  deadline outward indefinitely, dissolving the guardrail that makes cost
  per-session rather than per-hour. The correct mechanism is set at launch:
  `LaunchSpec.ActivePorts` (new, wired to the worker's port by
  `brain.SpawnExecutor`) puts that port under spawn's in-instance idle daemon,
  which counts ESTABLISHED connections on it as activity and resets the idle
  timer itself — so an in-flight trace keeps its own instance alive with no
  control-plane round-trip. The load-bearing contract is unchanged: the durable
  signal remains the per-session `last_request_time` the gateway writes
  (ARCHITECTURE.md §6.1, "the timestamp, not the mechanism").

- `internal/brain`: `extractJSON` now repairs literal control characters (raw
  newlines/tabs/CRs) inside JSON string values returned by the planning model.
  Live Bedrock emits the generated `nnsight` as a multi-line value with
  unescaped newlines, which `encoding/json` rejects (`invalid character '\n' in
  string literal`) — surfaced only on the real deploy path; the canned fakes are
  pre-escaped. Shared by the planner and interpreter.
- `deploy/terraform/lambda.tf`: set `AWS_LWA_PORT` per-function (forayd `8080`,
  foray-web `8090`) instead of one shared value. The two binaries bind different
  default ports, so a single shared port left the web-API Lambda failing its LWA
  readiness check — a silent `503 Service Unavailable` with no app-level error.

### Added

- CI: two invariant gates now fail the build on a violation (issues #31, #32).
  `scripts/invariant-check.sh` (`make invariant-check`) is a static guard for
  the *control plane rests at ~$0* invariant — it scans `go.mod` for always-on
  infra clients (broker/cluster/queue) and `deploy/terraform/` for always-on
  resource types (`aws_instance`, ECS/EKS, RDS, load balancers, NAT gateways,
  …) and requires the DynamoDB table to be `PAY_PER_REQUEST`. A new reflective
  test `internal/webapi.TestNoTensorEgressBoundaries` enforces *no automatic
  egress* — it reflects over every trace-result boundary struct
  (`gateway.TraceResult`, `brain.RawResult`, `brain.Result`, `webapi.resultView`)
  and fails if any field has a tensor-bearing type (`[]byte`, a numeric
  slice/array, a map, or `interface{}`). Both run in a new `invariant` CI job.
- `deploy`: the `foray-web` Lambda now bundles the **truffle** binary so
  `/api/propose` can price in the deployed control plane. `make lambdas`
  cross-compiles truffle (`linux/arm64`, `CGO_ENABLED=0`, stripped) from a
  spore.host/truffle checkout (`TRUFFLE_SRC`, default `../spore-host/truffle`)
  into the zip under `bin/`; `lambda.tf` prepends `/var/task/bin` to `PATH` so
  the spore exec runner's `LookPath` resolves it (no Go-adapter change — it
  honors the "call the tool, don't reimplement" rule). The webapi IAM role gains
  read-only Spot pricing actions (`ec2:DescribeSpotPriceHistory`,
  `DescribeInstanceTypes`, `DescribeInstanceTypeOfferings`, `DescribeRegions`,
  `pricing:GetProducts`). Stays ~$0: a non-VPC Lambda keeps default internet
  egress, so no VPC/endpoints/NAT are needed. This closes the pricing half of
  the deploy gap documented in PR #64; gateway→worker reachability remains a
  filed follow-on.

- `internal/gateway`: per-question cost receipts persisted to DynamoDB (#47).
  A `ReceiptStore` capability (optional, like `enumerator`) on `DynamoStore` and
  `MemStore` writes one row per approved rung under the reserved
  `pk=QUESTION#<id>` / `sk=RECEIPT#<rung>` layout (same `expires` TTL, so it
  self-cleans → $0); `QuestionID` derives a stable, whitespace/case-normalized
  partition id from the question text so the stateless web client and the server
  agree without carrying an id; `SummarizeReceipts` folds the rows into
  `$-so-far vs envelope`. `RecordReceipt`/`LoadReceipts` are best-effort seams
  (a lost receipt never fails a trace; a store without the capability no-ops).
- `internal/webapi`: `/api/approve` now persists the receipt after each approved
  rung, and a new `GET /api/receipt?question=<text>|id=<id>` returns the
  authoritative persisted `$-so-far` so the page's cost meter survives a reload.
  The SPA seeds its meter from this receipt on (re-)propose.
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
- `internal/webapi`: the brain-over-HTTP surface (ARCHITECTURE.md §6.8) — the
  same result-gated loop the CLI walks in-process, exposed as a small JSON API so
  the static page becomes a thin client. `Handler(deps)` is a stdlib `ServeMux`:
  `POST /api/propose` (question → a clarifying question or the planned ladder +
  first rung), `POST /api/approve` (one loop iteration — Cedar-gated launch →
  trace through the gateway → interpret → assess → the next rung, if any, awaiting
  a fresh Go), `POST /api/export` (the opt-in presigned download), and
  `GET /healthz`. It holds no daemon state — the client carries the `Ladder` JSON
  on each call and Cedar still gates server-side at `Approve` — so it drops onto a
  cold Lambda unchanged at the deploy step and the control plane rests at ~$0.
  Responses carry references only (`s3://` save ref + viz ref), never tensors — a
  test guards against tensor egress. `NewFakeDeps` runs the whole loop with zero
  AWS for `FORAY_FAKE=1`. Closes #27, #52.
- `cmd/foray-web`: thin dev server that serves the static `web/` SPA alongside
  `webapi.Handler` so local rehearsal is one command (`make web-fake`); per-
  invocation, no daemon state. Real path is refused here (the production surface
  is the deploy step's Lambda), mirroring `cmd/forayd`.
- `web/`: the page is now a thin client over the real loop — it `fetch`es
  `/api/propose` and `/api/approve` instead of replaying a canned `RUNGS` array,
  binds the live cost meter and per-rung cost to the brain's own estimates (#52),
  renders the brain's findings and climb/stop recommendations, climbs only on a
  fresh Go, and offers the opt-in export. The strata panel is now an explicitly
  illustrative logit lens (seeded from the rung's real layer count; the rendered
  viz arrives via `vizRef` at deploy). Accessibility pass (#51): a skip link,
  `role="img"`/`aria-label` on the lens, `aria-busy` Go buttons, an `aria-live`
  finding/meter, visible focus rings, and `prefers-reduced-motion` honored
  throughout. Closes #51.
- **deploy (step 9) — IaC for the ~$0 control plane.** `deploy/terraform/`
  stands up S3 (a private OAC-only SPA bucket + an in-region saves/exports
  bucket), CloudFront (SPA + `/api/*` and `/sessions/*` behaviors → API Gateway,
  one origin), an API Gateway HTTP API, two cold Lambdas wrapping
  `gateway.Handler` and `webapi.Handler` **verbatim** via the AWS Lambda Web
  Adapter (no `aws-lambda-go` dependency; binary is `bootstrap` on
  `provided.al2023`/`arm64`), an on-demand DynamoDB sessions table with TTL, and
  three least-privilege IAM roles (the two Lambda execs + the `spawn` instance
  role, #54). Cedar deploys nothing — it stays embedded and in-process.
  `make deploy`/`make teardown` build, apply, sync, and verify; new
  `make deploy-check` (#30) and `make teardown-verify` (#48) guard the
  invariants. Closes #29, #30, #48, #54.
- `internal/gateway`: `DynamoStore` — the prod `Store` (DynamoDB, AWS SDK v2).
  `Touch` is a single `UpdateItem` on `last_request` (the idle-bridge hot path);
  `Get` maps a missing item to `ErrUnknownSession`; the composite key reserves
  `RECEIPT#<rung>` rows for per-question cost receipts. It deliberately does not
  implement `enumerator`, so `/healthz` degrades to liveness (no Scan). The
  deployed gateway's idle bridge is `spore.DynamoIdleBridge`: a KeepWarm-no-op
  shim, since `Touch` already writes the durable `last_request` to DynamoDB that
  a spawn-side consumer reads — no `spawn` exec in a Lambda runtime.
- `internal/export`: `S3Presigner` — the real opt-in download (#25, #53). A
  single-object, short-TTL presigned GET against the user's own bucket; for
  `KindBundle`, a zip-on-demand of the session's saves + outputs + `nnsight` +
  a synthesized `manifest.json` to one `exports/` object, then presigned —
  oversized objects are dropped, logged, and recorded in the manifest (never
  silently truncated). The CLI and web real paths now use it instead of the
  stub presigner.
- `cmd/forayd` and `cmd/foray-web`: real paths wired (previously refused). forayd
  builds the `DynamoStore`-backed gateway over the HTTP worker; foray-web's
  `webapi.NewRealDeps` wires the real Bedrock+Cedar+spawn brain, the DynamoDB
  gateway, and the S3 export presigner from the environment Terraform injects.

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
