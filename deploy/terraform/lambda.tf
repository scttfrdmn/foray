# deploy/terraform/lambda.tf — the two cold Lambdas.
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
# Each Lambda wraps an existing http.Handler VERBATIM via the AWS Lambda Web
# Adapter (LWA) layer — the binary is an ordinary http.Server (cmd/forayd,
# cmd/foray-web) and LWA proxies the API Gateway event into a localhost HTTP call.
# Zero Lambda-specific Go code, no aws-lambda-go dependency. The binary inside the
# zip is named `bootstrap` for the provided.al2023 runtime (make deploy builds it).
#
# Both are per-invocation with no daemon state, so they rest at ~$0.

# LWA proxies the API Gateway event to the binary over localhost. AWS_LWA_PORT
# must match the port the binary actually listens on — and the two binaries pick
# different defaults (forayd :8080, foray-web :8090), so the port is set
# per-function below, not shared. A mismatch is a silent 503 ("app is not
# ready") with no app-level error — readiness just never passes.
locals {
  lwa_env = {
    AWS_LAMBDA_EXEC_WRAPPER      = "/opt/bootstrap"
    AWS_LWA_READINESS_CHECK_PATH = "/healthz"
  }
}

# ─── gateway (forayd) ────────────────────────────────────────────────────────

resource "aws_cloudwatch_log_group" "forayd" {
  name              = "/aws/lambda/foray-gateway"
  retention_in_days = var.log_retention_days
  tags              = { Name = "foray-gateway" }
}

resource "aws_lambda_function" "forayd" {
  function_name    = "foray-gateway"
  role             = aws_iam_role.forayd.arn
  filename         = var.forayd_zip
  source_code_hash = filebase64sha256(var.forayd_zip)
  handler          = "bootstrap"
  runtime          = "provided.al2023"
  architectures    = ["arm64"]
  timeout          = 600 # a trace can be slow (matches the 10-min worker HTTP client)
  memory_size      = 256
  layers           = [var.lwa_layer_arn]

  environment {
    variables = merge(local.lwa_env, {
      AWS_LWA_PORT         = "8080" # cmd/forayd binds :8080
      FORAY_SESSIONS_TABLE = aws_dynamodb_table.sessions.name
    })
  }

  depends_on = [aws_cloudwatch_log_group.forayd]
  tags       = { Name = "foray-gateway" }
}

# ─── web API (foray-web) ─────────────────────────────────────────────────────

resource "aws_cloudwatch_log_group" "webapi" {
  name              = "/aws/lambda/foray-webapi"
  retention_in_days = var.log_retention_days
  tags              = { Name = "foray-webapi" }
}

resource "aws_lambda_function" "webapi" {
  function_name    = "foray-webapi"
  role             = aws_iam_role.webapi.arn
  filename         = var.webapi_zip
  source_code_hash = filebase64sha256(var.webapi_zip)
  handler          = "bootstrap"
  runtime          = "provided.al2023"
  architectures    = ["arm64"]
  timeout          = 600 # Bedrock planning + a trace round-trip
  memory_size      = 512
  layers           = [var.lwa_layer_arn]

  environment {
    variables = merge(local.lwa_env, {
      AWS_LWA_PORT         = "8090" # cmd/foray-web binds :8090
      FORAY_SESSIONS_TABLE = aws_dynamodb_table.sessions.name
      FORAY_DATA_BUCKET    = aws_s3_bucket.data.bucket
      FORAY_PLAN_MODEL     = var.plan_model_id
      FORAY_BUDGET_CEILING = tostring(var.budget_ceiling_usd)
      # The brain prices via truffle (the spore "call the tool" rule). truffle
      # isn't on the stock Lambda PATH, so `make lambdas` bundles it under bin/
      # in this zip and we prepend /var/task/bin (the unzipped code root) so
      # spore's exec.LookPath finds it. No VPC needed: a non-VPC Lambda keeps
      # default internet egress for truffle's read-only EC2/pricing calls, so
      # /api/propose prices while the control plane stays ~$0.
      PATH = "/var/task/bin:/usr/local/bin:/usr/bin:/bin"
    })
  }

  depends_on = [aws_cloudwatch_log_group.webapi]
  tags       = { Name = "foray-webapi" }
}

# API Gateway invoke permissions.
resource "aws_lambda_permission" "forayd_apigw" {
  statement_id  = "AllowAPIGatewayInvoke"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.forayd.function_name
  principal     = "apigateway.amazonaws.com"
  source_arn    = "${aws_apigatewayv2_api.foray.execution_arn}/*/*"
}

resource "aws_lambda_permission" "webapi_apigw" {
  statement_id  = "AllowAPIGatewayInvoke"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.webapi.function_name
  principal     = "apigateway.amazonaws.com"
  source_arn    = "${aws_apigatewayv2_api.foray.execution_arn}/*/*"
}
