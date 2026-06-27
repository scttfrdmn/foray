# deploy/terraform/iam.tf — least-privilege roles.
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
# Three roles, each scoped to exactly what it needs (issue #53, #54):
#   - gateway Lambda:  CloudWatch logs + DynamoDB on the sessions table.
#   - web-API Lambda:  logs + DynamoDB + Bedrock invoke (plan model) + S3 on
#                      sessions/* (NO bucket-wide grant) + PassRole(spawn role).
#   - spawn instance:  the data-plane EC2 role spawn launches with — S3 on
#                      sessions/*, self-terminate scoped to Project=foray tags.
# Cedar is in-process (no AWS resource); see main.tf / README.md.

data "aws_iam_policy_document" "lambda_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

# ─── gateway (forayd) Lambda role ────────────────────────────────────────────

resource "aws_iam_role" "forayd" {
  name               = "foray-gateway-lambda"
  assume_role_policy = data.aws_iam_policy_document.lambda_assume.json
  tags               = { Name = "foray-gateway-lambda" }
}

resource "aws_iam_role_policy_attachment" "forayd_logs" {
  role       = aws_iam_role.forayd.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
}

# Gateway touches only the sessions table: Get/Put on registration, UpdateItem on
# every trace (the idle-bridge Touch), Query for receipts. No Scan.
resource "aws_iam_role_policy" "forayd_dynamo" {
  name = "sessions-table"
  role = aws_iam_role.forayd.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "dynamodb:GetItem",
        "dynamodb:PutItem",
        "dynamodb:UpdateItem",
        "dynamodb:Query",
      ]
      Resource = aws_dynamodb_table.sessions.arn
    }]
  })
}

# ─── web-API (foray-web) Lambda role ─────────────────────────────────────────

resource "aws_iam_role" "webapi" {
  name               = "foray-webapi-lambda"
  assume_role_policy = data.aws_iam_policy_document.lambda_assume.json
  tags               = { Name = "foray-webapi-lambda" }
}

resource "aws_iam_role_policy_attachment" "webapi_logs" {
  role       = aws_iam_role.webapi.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
}

resource "aws_iam_role_policy" "webapi_dynamo" {
  name = "sessions-table"
  role = aws_iam_role.webapi.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "dynamodb:GetItem",
        "dynamodb:PutItem",
        "dynamodb:UpdateItem",
        "dynamodb:Query",
      ]
      Resource = aws_dynamodb_table.sessions.arn
    }]
  })
}

# Bedrock invoke, scoped to the plan model's inference-profile + the foundation
# models it fronts. The LLM never touches the money path; this just lets the
# brain plan/interpret.
resource "aws_iam_role_policy" "webapi_bedrock" {
  name = "bedrock-invoke"
  role = aws_iam_role.webapi.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "bedrock:InvokeModel",
        "bedrock:InvokeModelWithResponseStream",
      ]
      # Inference-profile invoke spans the profile ARN and the underlying
      # foundation-model ARNs in-region; scope to this account's Bedrock surface.
      Resource = [
        "arn:aws:bedrock:*:${local.account_id}:inference-profile/${var.plan_model_id}",
        "arn:aws:bedrock:*::foundation-model/*",
      ]
    }]
  })
}

# S3 on the session prefix ONLY — presign reads, bundle read/write. No
# bucket-wide grant; the resource is sessions/* (issue #53). ListBucket is
# conditioned to the sessions/ prefix so a presign can enumerate one session's
# objects without listing the whole bucket.
resource "aws_iam_role_policy" "webapi_s3" {
  name = "data-bucket-sessions"
  role = aws_iam_role.webapi.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = ["s3:GetObject", "s3:PutObject"]
        Resource = "${aws_s3_bucket.data.arn}/sessions/*"
      },
      {
        Effect   = "Allow"
        Action   = ["s3:ListBucket"]
        Resource = aws_s3_bucket.data.arn
        Condition = {
          StringLike = { "s3:prefix" = ["sessions/*"] }
        }
      },
    ]
  })
}

# Let the web API hand the spawn role to instances it launches (the brain's
# executor → spawn). Scoped to exactly the spawn role ARN.
resource "aws_iam_role_policy" "webapi_passrole" {
  name = "pass-spawn-role"
  role = aws_iam_role.webapi.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["iam:PassRole"]
      Resource = aws_iam_role.spawn.arn
      Condition = {
        StringEquals = { "iam:PassedToService" = "ec2.amazonaws.com" }
      }
    }]
  })
}

# ─── spawn instance role (issue #54) ─────────────────────────────────────────
#
# The least-privilege role a data-plane GPU instance assumes. Documented in
# README.md — that doc is as much the #54 deliverable as this HCL. Scoped to:
#   - read/write the session's OWN saves under sessions/* (no bucket-wide grant),
#   - self-terminate on TTL, restricted to instances tagged Project=foray.

data "aws_iam_policy_document" "ec2_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "spawn" {
  name               = "foray-spawn-instance"
  assume_role_policy = data.aws_iam_policy_document.ec2_assume.json
  tags               = { Name = "foray-spawn-instance" }
}

resource "aws_iam_instance_profile" "spawn" {
  name = "foray-spawn-instance"
  role = aws_iam_role.spawn.name
}

# The worker streams a checkpoint in and writes saves out, all under sessions/*.
resource "aws_iam_role_policy" "spawn_s3" {
  name = "session-saves"
  role = aws_iam_role.spawn.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["s3:GetObject", "s3:PutObject"]
      Resource = "${aws_s3_bucket.data.arn}/sessions/*"
    }]
  })
}

# Self-terminate on TTL/idle — but only foray's own instances. The tag condition
# means this role cannot touch any instance not tagged Project=foray.
resource "aws_iam_role_policy" "spawn_self_terminate" {
  name = "self-terminate-foray-tagged"
  role = aws_iam_role.spawn.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["ec2:TerminateInstances", "ec2:StopInstances"]
      Resource = "arn:aws:ec2:*:${local.account_id}:instance/*"
      Condition = {
        StringEquals = { "aws:ResourceTag/Project" = "foray" }
      }
    }]
  })
}
