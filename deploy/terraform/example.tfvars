# deploy/terraform/example.tfvars — copy to prod.tfvars (gitignored) and fill.
# Copyright 2026 Scott Friedman. Apache License 2.0.
#
# Every PLACEHOLDER_* below must be resolved before deploy. `make deploy-check`
# (scripts/deploy-check.sh, issue #30) fails on any PLACEHOLDER_* or
# LICENSED_WORKLOAD_STUB left in deploy/terraform/ — this template file is the
# one exception it skips.

aws_region = "us-east-1"

# S3 bucket names are globally unique — pick your own.
web_bucket_name  = "PLACEHOLDER_WEB_BUCKET"
data_bucket_name = "PLACEHOLDER_DATA_BUCKET"

# DynamoDB table for the session<->instance map (default foray-sessions is fine).
sessions_table_name = "foray-sessions"

# AWS Lambda Web Adapter layer — REGION-SPECIFIC and versioned. Find the current
# public ARN for your region and architecture (arm64) at:
#   https://github.com/awslabs/aws-lambda-web-adapter#aws-lambda-web-adapter
# e.g. arn:aws:lambda:us-east-1:753240598075:layer:LambdaAdapterLayerArm64:24
lwa_layer_arn = "PLACEHOLDER_LWA_LAYER_ARN"

# Bedrock planning model — a US inference profile id.
plan_model_id = "us.anthropic.claude-sonnet-4-6"

# Per-session Cedar budget ceiling (USD).
budget_ceiling_usd = 5.00
