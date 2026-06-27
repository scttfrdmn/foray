#!/usr/bin/env bash
# Post-teardown verification (issue #48): assert nothing tagged Project=foray is
# left running or billing after `make teardown`. Enforces "ephemeral by default"
# and "nothing left billing" at the IaC layer.
#
# Relies on every Terraform resource carrying default_tags{Project=foray}. Uses
# the Resource Groups Tagging API, which spans services, so one query catches a
# stray bucket, table, Lambda, log group, or — most importantly — a data-plane
# EC2 instance that escaped spawn's TTL.
# Copyright 2026 Scott Friedman. Apache License 2.0.
set -euo pipefail

PROJECT_TAG="${FORAY_PROJECT_TAG:-foray}"
REGION_ARG=()
if [ -n "${AWS_REGION:-}" ]; then
  REGION_ARG=(--region "$AWS_REGION")
fi

if ! command -v aws >/dev/null 2>&1; then
  echo "teardown-verify: aws CLI not found (install it or run this from a machine with creds)" >&2
  exit 2
fi

echo "==> teardown-verify: looking for any resource tagged Project=$PROJECT_TAG"
arns=$(aws resourcegroupstaggingapi get-resources \
  --tag-filters "Key=Project,Values=$PROJECT_TAG" \
  --query 'ResourceTagMappingList[].ResourceARN' \
  --output text "${REGION_ARG[@]}" 2>/dev/null || true)

if [ -n "$arns" ]; then
  echo "teardown-verify: FAILED — these Project=$PROJECT_TAG resources still exist:" >&2
  echo "$arns" | tr '\t' '\n' | sed 's/^/  /' >&2
  echo "  (re-run 'make teardown', or remove them manually — the control plane must rest at \$0)" >&2
  exit 1
fi

echo "teardown-verify: OK — no Project=$PROJECT_TAG resources remain (nothing running, nothing billing)"
