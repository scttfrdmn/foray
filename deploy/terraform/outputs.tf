# deploy/terraform/outputs.tf
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

output "cloudfront_domain" {
  description = "The page URL (the SPA + the /api and /sessions behaviors)."
  value       = "https://${aws_cloudfront_distribution.web.domain_name}"
}

output "api_endpoint" {
  description = "Direct API Gateway endpoint (for the CLI / raw nnsight backend)."
  value       = aws_apigatewayv2_api.foray.api_endpoint
}

output "web_bucket" {
  description = "Static SPA bucket — make deploy syncs web/ here."
  value       = aws_s3_bucket.web.bucket
}

output "data_bucket" {
  description = "In-region saves/exports bucket (FORAY_DATA_BUCKET)."
  value       = aws_s3_bucket.data.bucket
}

output "sessions_table" {
  description = "DynamoDB session<->instance table (FORAY_SESSIONS_TABLE)."
  value       = aws_dynamodb_table.sessions.name
}

output "spawn_role_arn" {
  description = "Least-privilege role the data-plane GPU instances assume (issue #54)."
  value       = aws_iam_role.spawn.arn
}

output "spawn_instance_profile" {
  description = "Instance profile wrapping the spawn role — pass to spawn launches."
  value       = aws_iam_instance_profile.spawn.name
}
