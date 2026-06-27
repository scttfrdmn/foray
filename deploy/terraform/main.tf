# deploy/terraform/main.tf — foray control plane (ARCHITECTURE.md §2).
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
#
# The whole control plane rests at ~$0: a static SPA in S3 + CloudFront, two cold
# Lambdas (the gateway and the web API, each wrapping an existing http.Handler
# verbatim via the AWS Lambda Web Adapter), an on-demand DynamoDB table, and an
# HTTP API in front. Nothing here is always-on; every dollar above pennies is
# per-use (Bedrock tokens) or per-session (the GPU, launched by spawn outside
# this stack).
#
# NOTE on state: local state is fine for a single-account pilot. To migrate to an
# S3 backend for shared use, see the "Remote state" section in README.md.
#
# NOTE on Cedar: foray's Cedar policy is NOT an AWS resource. It is embedded into
# the Lambda binaries (internal/brain/policy/foray.cedar via go:embed) and
# evaluated in-process by cedar-go. There is nothing to provision here for it —
# do not go looking for an AWS Verified Permissions policy store. See README.md.

terraform {
  required_version = ">= 1.7"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.40"
    }
  }
}

provider "aws" {
  region = var.aws_region

  # Every resource carries Project=foray so teardown-verify (scripts/
  # teardown-verify.sh, issue #48) can assert nothing is left billing with one
  # Resource Groups Tagging API query.
  default_tags {
    tags = {
      Project   = "foray"
      ManagedBy = "terraform"
    }
  }
}

data "aws_caller_identity" "current" {}
data "aws_region" "current" {}

locals {
  account_id = data.aws_caller_identity.current.account_id
  region     = data.aws_region.current.name
}
