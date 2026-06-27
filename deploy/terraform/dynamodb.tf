# deploy/terraform/dynamodb.tf — the session<->instance map (and cost receipts).
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
# On-demand (PAY_PER_REQUEST): zero resting cost — the control-plane invariant.
# Composite key (pk, sk) holds two item shapes in one table:
#   pk=SESSION#<id>, sk=META           the Session row (instance_id, worker_url, last_request)
#   pk=SESSION#<id>, sk=RECEIPT#<rung> per-question cost receipt (issue #47)
# A spawn-side consumer reads last_request (the durable idle signal) instead of
# the gateway shelling out to `spawn extend` from a Lambda (ARCHITECTURE.md §6.1).
# TTL on `expires` self-cleans stale rows so the table never accumulates cost.

resource "aws_dynamodb_table" "sessions" {
  name         = var.sessions_table_name
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "pk"
  range_key    = "sk"

  attribute {
    name = "pk"
    type = "S"
  }
  attribute {
    name = "sk"
    type = "S"
  }

  ttl {
    attribute_name = "expires"
    enabled        = true
  }

  point_in_time_recovery {
    enabled = false # ephemeral session state; PITR would add resting cost
  }

  tags = { Name = "foray-sessions" }
}
