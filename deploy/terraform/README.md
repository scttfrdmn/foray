<!--
Copyright 2026 Scott Friedman. Apache License 2.0.
-->
# foray control plane — Terraform

The ~$0 control plane (ARCHITECTURE.md §2): a static SPA in S3 + CloudFront, two
cold Lambdas wrapping the existing `http.Handler`s, an on-demand DynamoDB table,
and an HTTP API. Nothing here is always-on. The GPU data plane is launched
per-session by `spawn`, outside this stack.

## Deploy

```bash
cp deploy/terraform/example.tfvars deploy/terraform/prod.tfvars   # gitignored
$EDITOR deploy/terraform/prod.tfvars                              # fill PLACEHOLDER_*
make deploy                                                       # build Lambdas, tf apply, sync web/
```

`make deploy` runs `make deploy-check` first (fails on any unresolved
`PLACEHOLDER_*` / `LICENSED_WORKLOAD_STUB`), cross-compiles `cmd/forayd` and
`cmd/foray-web` to `provided.al2023`/`arm64` (binary named `bootstrap`), zips
each, `terraform apply`s, then `aws s3 sync web/` to the SPA bucket.

```bash
make teardown   # terraform destroy, then teardown-verify (asserts nothing Project=foray remains)
```

## What this provisions

| Resource | Why | Resting cost |
| --- | --- | --- |
| S3 `web` (OAC-only) | the static SPA | pennies |
| S3 `data` | the user's in-region saves/outputs/exports (`sessions/<id>/…`) | per-GB stored |
| CloudFront | serves the SPA; `/api/*` + `/sessions/*` behaviors → API GW | per-request |
| API Gateway HTTP API | routes to the two Lambdas | per-request |
| Lambda `foray-gateway` | `gateway.Handler` (trace + idle bridge) via LWA | $0 idle |
| Lambda `foray-webapi` | `webapi.Handler` (propose/approve/export) via LWA | $0 idle |
| DynamoDB `foray-sessions` | session↔instance map + cost receipts; on-demand, TTL | $0 idle |
| IAM (3 roles) | least-privilege Lambda execs + the spawn instance role | $0 |

## Cedar deploys nothing

foray's authorization policy (`internal/brain/policy/foray.cedar`) is **embedded
into the Lambda binaries** (`go:embed`) and evaluated **in-process** by
`cedar-go`. It is **not** AWS Verified Permissions and **not** an AgentCore
Gateway policy — there is no AWS resource to provision for it. The policy ships
inside the binary; updating it means rebuilding and redeploying the Lambdas.

## AWS Lambda Web Adapter (LWA)

The Lambdas run the existing `http.Server` binaries verbatim — no
`aws-lambda-go`, no proxy shim. LWA is attached as a layer
(`var.lwa_layer_arn`) with `AWS_LAMBDA_EXEC_WRAPPER=/opt/bootstrap`, and proxies
each API Gateway event into a localhost HTTP call on `AWS_LWA_PORT` (8080), with
`AWS_LWA_READINESS_CHECK_PATH=/healthz`.

The layer ARN is **region-specific and versioned** — pin it in `prod.tfvars` for
`var.aws_region`. A mismatched or unpinned ARN is a silent deploy failure. The
current public ARNs are listed at
<https://github.com/awslabs/aws-lambda-web-adapter>.

## IAM (least privilege)

Three roles, each scoped to exactly what it needs.

### `foray-gateway-lambda`
- `AWSLambdaBasicExecutionRole` (CloudWatch logs).
- DynamoDB `GetItem`/`PutItem`/`UpdateItem`/`Query` on the **sessions table ARN
  only**. No `Scan`. The hot path (`Touch` on every trace) is a single
  `UpdateItem`.

### `foray-webapi-lambda`
- Logs.
- DynamoDB on the sessions table ARN only.
- `bedrock:InvokeModel[WithResponseStream]` scoped to the plan model's
  inference-profile ARN and the in-region foundation-model ARNs. The LLM never
  touches the money path; this only lets the brain plan/interpret.
- S3 `GetObject`/`PutObject` on **`<data-bucket>/sessions/*` only** (no
  bucket-wide grant), plus `ListBucket` **conditioned to the `sessions/*`
  prefix** so a presign can enumerate one session without listing the bucket.
- `iam:PassRole` for the spawn role only, conditioned to `ec2.amazonaws.com`.

### `foray-spawn-instance` (issue #54)
The least-privilege role a data-plane GPU instance assumes (via an instance
profile passed to `spawn`):
- `s3:GetObject`/`PutObject` on **`<data-bucket>/sessions/*` only** — stream a
  checkpoint in, write saves out. No bucket-wide grant.
- `ec2:TerminateInstances`/`StopInstances` for self-reaping on TTL/idle,
  **conditioned on `aws:ResourceTag/Project = foray`** — the role cannot touch
  any instance not tagged as foray's.
- Trusts `ec2.amazonaws.com` only.

The `Project=foray` default tag on every resource is also what lets
`scripts/teardown-verify.sh` (issue #48) prove, with one Resource Groups Tagging
API query, that teardown left nothing billing.

## Presigned exports (issues #25, #53)

Export is opt-in egress of the user's own data (ARCHITECTURE.md §6.9). The
presigner (`internal/export/s3.go`) mints a **single-object, short-TTL** (15 min
default) presigned GET against the user's own `data` bucket. `KindBundle` zips a
session's saves + outputs + `nnsight` + a synthesized `manifest.json` to one
object under `sessions/<id>/exports/`, then presigns **that one key** — so the
final URL is always single-object, never a bucket-wide grant. Oversized objects
are skipped, logged, and recorded in the manifest's `dropped` list (never
silently truncated). The generated zips carry a `foray-export-bundle=true` object
tag so the bucket lifecycle rule expires them after a day without touching the
user's saved activations.

## Remote state

Local state is fine for a single-account pilot. For shared use, add an S3 backend
to `main.tf`'s `terraform {}` block and re-`init`:

```hcl
backend "s3" {
  bucket = "your-tfstate-bucket"
  key    = "foray/control-plane/terraform.tfstate"
  region = "us-east-1"
}
```

## Worker reachability (documented follow-on)

The gateway Lambda POSTs traces to the worker at `http://<public_dns>:8000`. In
production the gateway Lambda is expected to be **VPC-attached** (with VPC
endpoints for DynamoDB and S3) so it reaches the GPU worker privately. That VPC
plumbing is intentionally not in this first IaC pass — the real deploy is
hand-validated (like `make worker-smoke`), and the trace path is exercised there.
