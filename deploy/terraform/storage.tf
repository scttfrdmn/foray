# deploy/terraform/storage.tf — the two S3 buckets.
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
#  - web:  the static SPA. Private; reachable only through CloudFront (OAC).
#  - data: the user's OWN in-region saves/outputs/exports, laid out under
#          sessions/<id>/. The worker writes here; export presigns reads from
#          here. "The user's own bucket" in their own account (#53).
#
# Both block all public access. No automatic egress: nothing here is world-
# readable, and export hands out short-TTL presigned URLs, never open objects.

# ─── web bucket (static SPA) ─────────────────────────────────────────────────

resource "aws_s3_bucket" "web" {
  bucket = var.web_bucket_name
  tags   = { Name = "foray-web" }
}

resource "aws_s3_bucket_public_access_block" "web" {
  bucket                  = aws_s3_bucket.web.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "web" {
  bucket = aws_s3_bucket.web.id
  rule {
    apply_server_side_encryption_by_default { sse_algorithm = "AES256" }
  }
}

# Reachable only by this distribution (OAC). The condition pins it to our own
# CloudFront ARN so no other principal can read the bucket.
resource "aws_s3_bucket_policy" "web" {
  bucket = aws_s3_bucket.web.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "AllowCloudFrontOACRead"
      Effect    = "Allow"
      Principal = { Service = "cloudfront.amazonaws.com" }
      Action    = "s3:GetObject"
      Resource  = "${aws_s3_bucket.web.arn}/*"
      Condition = {
        StringEquals = { "AWS:SourceArn" = aws_cloudfront_distribution.web.arn }
      }
    }]
  })
}

# ─── data bucket (saves / outputs / exports) ─────────────────────────────────

resource "aws_s3_bucket" "data" {
  bucket = var.data_bucket_name
  tags   = { Name = "foray-data" }
}

resource "aws_s3_bucket_public_access_block" "data" {
  bucket                  = aws_s3_bucket.data.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "data" {
  bucket = aws_s3_bucket.data.id
  rule {
    apply_server_side_encryption_by_default { sse_algorithm = "AES256" }
  }
}

# The browser downloads a presigned bundle directly from S3; allow GET from the
# page's origin only. Presigned PUT is not used (the worker writes server-side).
resource "aws_s3_bucket_cors_configuration" "data" {
  bucket = aws_s3_bucket.data.id
  cors_rule {
    allowed_methods = ["GET"]
    allowed_origins = ["https://${aws_cloudfront_distribution.web.domain_name}"]
    allowed_headers = ["*"]
    max_age_seconds = 3000
  }
}

# Expire generated export bundles so exports/ doesn't accumulate cost. Scoped to
# the */exports/ artifacts ONLY — the user's saved activations under
# sessions/<id>/ are theirs to keep or discard and are never auto-deleted. A
# presigned URL is short-lived, so a generated zip need not outlive a day.
resource "aws_s3_bucket_lifecycle_configuration" "data" {
  bucket = aws_s3_bucket.data.id
  rule {
    id     = "expire-export-bundles"
    status = "Enabled"
    # Object tag set at PutObject time marks the bundle zips; only tagged objects
    # expire, so saved activations are untouched regardless of prefix.
    filter {
      tag {
        key   = "foray-export-bundle"
        value = "true"
      }
    }
    expiration { days = 1 }
  }
}
