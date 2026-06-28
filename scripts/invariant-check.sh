#!/usr/bin/env bash
# Invariant gate (issue #31): the control plane rests at ~$0 — no always-on
# server, queue broker, or Ray/K8s cluster. This is a static guard: it scans the
# two places an always-on dependency could enter — the Go module graph and the
# deployable IaC — and fails the build on a violation. Mirrors deploy-check.sh:
# scoped to concrete artifacts (go.mod, deploy/terraform/) so docs that merely
# *name* a forbidden resource don't trip it.
#
# What this does NOT cover: the no-auto-egress invariant (#32), which is enforced
# by a reflective Go test over the egress-boundary structs (internal/webapi).
# Copyright 2026 Scott Friedman. Apache License 2.0.
set -euo pipefail

fail=0

# --- 1. Go module graph: no always-on infra clients --------------------------
# foray talks to AWS via the SDK and shells out to the spore.host CLIs; it never
# embeds a broker/queue/cluster client. These module-path fragments would each
# pull in an always-on dependency (a broker to run, a cluster to join).
#   ray / k8s / kubernetes  — a cluster to keep alive
#   nats / amqp / rabbitmq / streadway  — a message broker
#   segmentio/kafka / confluent / sarama / franz-go  — Kafka
#   temporal / cadence  — a workflow server
#   redis / go-redis  — a resident cache/queue
#   nsq / asynq / machinery / gocraft/work  — job/queue brokers
if [ -f go.mod ]; then
  deny_mod='(^|[[:space:]/])(ray|k8s\.io|kubernetes|nats-io|streadway/amqp|rabbitmq|segmentio/kafka-go|confluentinc|Shopify/sarama|IBM/sarama|twmb/franz-go|go\.temporal\.io|cadence|go-redis|redis/go-redis|nsqio|hibiken/asynq|RichardKnop/machinery|gocraft/work)(/|[[:space:]]|$)'
  if hits=$(grep -nEi "$deny_mod" go.mod 2>/dev/null); then
    echo "invariant #31 violation: always-on infra dependency in go.mod:"
    echo "$hits" | sed 's/^/  /'
    echo "  (control plane must rest at ~\$0 — no broker/cluster/queue client)"
    fail=1
  fi
fi

# --- 2. IaC: no always-on resource types -------------------------------------
# The control plane is static SPA + cold Lambdas + per-token Bedrock + on-demand
# DynamoDB. GPU EC2 is launched at *runtime* by spore's spawn tool, never by this
# Terraform — so an aws_instance (or any of the below) in deploy/terraform/ means
# something always-on slipped into the control plane.
scan_root="deploy/terraform"
if [ -d "$scan_root" ]; then
  # Resource types that bill by the hour / keep something resident.
  deny_tf='aws_instance|aws_spot_instance_request|aws_ecs_(service|cluster|task_definition)|aws_eks_|aws_autoscaling|aws_launch_(template|configuration)|aws_mq_|aws_msk_|aws_rds_|aws_db_instance|aws_elasticache_|aws_lb([^a-z]|$)|aws_alb([^a-z]|$)|aws_elb([^a-z]|$)|aws_lb_target_group|aws_nat_gateway|aws_redshift_|aws_emr_|aws_kinesis_'
  while IFS= read -r f; do
    [ -z "$f" ] && continue
    if hits=$(grep -nE "^[[:space:]]*resource[[:space:]]+\"($deny_tf)" "$f" 2>/dev/null); then
      echo "invariant #31 violation: always-on resource in $f:"
      echo "$hits" | sed 's/^/  /'
      fail=1
    fi
  done < <(find "$scan_root" -type f -name '*.tf')

  # DynamoDB must be on-demand: PROVISIONED throughput is always-on billing.
  while IFS= read -r f; do
    [ -z "$f" ] && continue
    if grep -nE '^[[:space:]]*resource[[:space:]]+"aws_dynamodb_table"' "$f" >/dev/null 2>&1; then
      if ! grep -nE 'billing_mode[[:space:]]*=[[:space:]]*"PAY_PER_REQUEST"' "$f" >/dev/null 2>&1; then
        echo "invariant #31 violation: $f declares a DynamoDB table without"
        echo "  billing_mode = \"PAY_PER_REQUEST\" (provisioned capacity is always-on billing)"
        fail=1
      fi
    fi
  done < <(find "$scan_root" -type f -name '*.tf')
fi

if [ "$fail" -eq 0 ]; then
  echo "invariant-check: OK (no always-on server/broker/cluster in go.mod or $scan_root)"
fi
exit "$fail"
