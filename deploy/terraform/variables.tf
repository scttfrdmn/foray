# deploy/terraform/variables.tf — foray control-plane inputs.
# Copyright 2026 Scott Friedman
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

variable "aws_region" {
  description = "Region for the control plane (the data plane is launched per-session by spawn)."
  type        = string
  default     = "us-east-1"
}

variable "web_bucket_name" {
  description = "Globally-unique S3 bucket name for the static SPA. Served only via CloudFront (OAC); never public."
  type        = string
}

variable "data_bucket_name" {
  description = "Globally-unique S3 bucket name for the user's in-region saves/outputs/exports (sessions/<id>/...)."
  type        = string
}

variable "sessions_table_name" {
  description = "DynamoDB table for the session<->instance mapping (and per-question cost receipts)."
  type        = string
  default     = "foray-sessions"
}

variable "lwa_layer_arn" {
  description = <<-EOT
    AWS Lambda Web Adapter layer ARN, REGION-SPECIFIC and versioned. The control
    plane wraps the existing http.Handler binaries verbatim via this layer, so it
    must match var.aws_region. Pin a specific version — an unpinned/mismatched ARN
    is a silent deploy failure. See README.md for the current public ARN per region.
  EOT
  type        = string
}

variable "plan_model_id" {
  description = "Bedrock planning model — a US inference profile id (e.g. us.anthropic.claude-sonnet-4-6)."
  type        = string
  default     = "us.anthropic.claude-sonnet-4-6"
}

variable "budget_ceiling_usd" {
  description = "Per-session Cedar budget ceiling injected into the Lambdas (FORAY_BUDGET_CEILING)."
  type        = number
  default     = 5.00
}

variable "forayd_zip" {
  description = "Path to the built gateway Lambda zip (provided.al2023/arm64; binary named bootstrap). make deploy builds this."
  type        = string
  default     = "../../build/forayd.zip"
}

variable "webapi_zip" {
  description = "Path to the built web-API Lambda zip (provided.al2023/arm64; binary named bootstrap)."
  type        = string
  default     = "../../build/foray-web.zip"
}

# Documented for the deployer; consumed by the Makefile, not Terraform. The
# web-API Lambda prices via the truffle binary (the spore "call the tool" rule),
# which isn't on the stock Lambda PATH — `make lambdas TRUFFLE_SRC=<path>`
# cross-compiles it from a spore.host/truffle checkout into the zip under bin/.
# See README.md and the Makefile's bundle-truffle target.
variable "truffle_src" {
  description = "Local spore.host/truffle checkout the Makefile cross-compiles into the web-API Lambda zip (Makefile-consumed; not read by Terraform). Default ../spore-host/truffle."
  type        = string
  default     = "../spore-host/truffle"
}

variable "log_retention_days" {
  description = "CloudWatch log retention for the Lambdas (short — the control plane is cheap to observe)."
  type        = number
  default     = 14
}
