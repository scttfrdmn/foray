# deploy/terraform/api.tf — API Gateway HTTP API v2 in front of the two Lambdas.
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
# HTTP API (not REST/v1): cheaper, no minimum, AWS_PROXY payload v2 that LWA
# translates into a localhost HTTP call. Routes mirror the handlers' own muxes:
#   ANY /api/{proxy+}        -> webapi  (POST /api/propose|approve|export)
#   ANY /sessions/{proxy+}   -> forayd  (POST /sessions/{id}/trace)
#   GET /healthz             -> webapi  (liveness)
# CloudFront fronts this (cdn.tf) so the page and API share one origin and dodge
# broad CORS; the API is reachable directly too for the CLI/raw nnsight backend.

resource "aws_apigatewayv2_api" "foray" {
  name          = "foray"
  protocol_type = "HTTP"
  tags          = { Name = "foray" }
}

resource "aws_apigatewayv2_integration" "forayd" {
  api_id                 = aws_apigatewayv2_api.foray.id
  integration_type       = "AWS_PROXY"
  integration_uri        = aws_lambda_function.forayd.invoke_arn
  integration_method     = "POST"
  payload_format_version = "2.0"
}

resource "aws_apigatewayv2_integration" "webapi" {
  api_id                 = aws_apigatewayv2_api.foray.id
  integration_type       = "AWS_PROXY"
  integration_uri        = aws_lambda_function.webapi.invoke_arn
  integration_method     = "POST"
  payload_format_version = "2.0"
}

resource "aws_apigatewayv2_route" "trace" {
  api_id    = aws_apigatewayv2_api.foray.id
  route_key = "ANY /sessions/{proxy+}"
  target    = "integrations/${aws_apigatewayv2_integration.forayd.id}"
}

resource "aws_apigatewayv2_route" "api" {
  api_id    = aws_apigatewayv2_api.foray.id
  route_key = "ANY /api/{proxy+}"
  target    = "integrations/${aws_apigatewayv2_integration.webapi.id}"
}

resource "aws_apigatewayv2_route" "healthz" {
  api_id    = aws_apigatewayv2_api.foray.id
  route_key = "GET /healthz"
  target    = "integrations/${aws_apigatewayv2_integration.webapi.id}"
}

# Auto-deploy stage with access logging. $default has no path prefix, so the
# CloudFront origin path and the handler routes line up exactly.
resource "aws_cloudwatch_log_group" "apigw" {
  name              = "/aws/apigateway/foray"
  retention_in_days = var.log_retention_days
  tags              = { Name = "foray-apigw" }
}

resource "aws_apigatewayv2_stage" "default" {
  api_id      = aws_apigatewayv2_api.foray.id
  name        = "$default"
  auto_deploy = true

  access_log_settings {
    destination_arn = aws_cloudwatch_log_group.apigw.arn
    format = jsonencode({
      requestId    = "$context.requestId"
      ip           = "$context.identity.sourceIp"
      routeKey     = "$context.routeKey"
      status       = "$context.status"
      responseLen  = "$context.responseLength"
      integrationE = "$context.integrationErrorMessage"
    })
  }

  tags = { Name = "foray" }
}
