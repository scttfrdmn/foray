#!/usr/bin/env bash
# Pre-deploy guard (issue #30): fail if any unresolved PLACEHOLDER_* or
# LICENSED_WORKLOAD_STUB token remains in the deployable IaC artifacts. Mirrors
# the sibling Gauss project's discipline — Gauss scopes its grep to the concrete
# deploy inputs (gauss/cluster/gauss.yaml, gauss-asbx/terraform/), not the whole
# tree, so that docs and issue templates that *name* the tokens don't trip it.
# Here the deployable artifact is deploy/terraform/.
# Copyright 2026 Scott Friedman. Apache License 2.0.
set -euo pipefail

# What actually gets deployed. The operator copies example.tfvars → a gitignored
# prod.tfvars and fills it; we scan the .tf files plus any *.tfvars that is NOT
# the template (a filled tfvars must have no PLACEHOLDERs left).
scan_root="deploy/terraform"

if [ ! -d "$scan_root" ]; then
  echo "deploy-check: $scan_root not present (nothing to check)"
  exit 0
fi

# Tokens that block a deploy. PLACEHOLDER_* is the unresolved-value marker;
# LICENSED_WORKLOAD_STUB marks software needing a license agreement first.
pattern='PLACEHOLDER_[A-Z0-9_]+|LICENSED_WORKLOAD_STUB'

fail=0
while IFS= read -r f; do
  [ -z "$f" ] && continue
  # example.tfvars is the committed template; it legitimately carries the tokens.
  case "$f" in
    */example.tfvars) continue ;;
  esac
  if grep -nE "$pattern" "$f" >/dev/null 2>&1; then
    echo "unresolved deploy blocker in $f:"
    grep -nE "$pattern" "$f" | sed 's/^/  /'
    fail=1
  fi
done < <(find "$scan_root" -type f \( -name '*.tf' -o -name '*.tfvars' \))

if [ "$fail" -eq 0 ]; then
  echo "deploy-check: OK (no unresolved PLACEHOLDER_* / LICENSED_WORKLOAD_STUB in $scan_root)"
fi
exit "$fail"
