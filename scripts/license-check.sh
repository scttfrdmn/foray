#!/usr/bin/env bash
# Verify every source file carries an Apache 2.0 license header.
# Copyright 2026 Scott Friedman. Apache License 2.0.
set -euo pipefail

needle="Licensed under the Apache License, Version 2.0"
fail=0

# Source extensions that must carry a header.
mapfile -t files < <(git ls-files '*.go' '*.js' '*.css' '*.html' '*.cedar' 2>/dev/null || \
  find . -type f \( -name '*.go' -o -name '*.js' -o -name '*.css' -o -name '*.html' -o -name '*.cedar' \) -not -path './.git/*')

for f in "${files[@]}"; do
  [ -z "$f" ] && continue
  if ! head -n 20 "$f" | grep -qF "$needle"; then
    echo "missing license header: $f"
    fail=1
  fi
done

if [ "$fail" -eq 0 ]; then
  echo "license-check: OK (${#files[@]} files)"
fi
exit "$fail"
