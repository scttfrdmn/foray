#!/usr/bin/env bash
# Create the foray roadmap project board (Projects v2) and add all open issues.
# Requires a token with the `project` scope:  gh auth refresh -s project,read:project
#
# Copyright 2026 Scott Friedman. Apache License 2.0.
set -euo pipefail
OWNER="${OWNER:-scttfrdmn}"
REPO="${REPO:-scttfrdmn/foray}"
TITLE="${TITLE:-foray roadmap}"

# Create the board if it doesn't already exist.
num=$(gh project list --owner "$OWNER" --format json --jq ".projects[] | select(.title==\"$TITLE\") | .number" 2>/dev/null | head -1)
if [ -z "$num" ]; then
  num=$(gh project create --owner "$OWNER" --title "$TITLE" --format json --jq '.number')
  echo "created project #$num"
else
  echo "project #$num exists"
fi

# Add every issue in the repo to the board.
gh issue list --repo "$REPO" --state all --limit 200 --json url --jq '.[].url' | while read -r url; do
  gh project item-add "$num" --owner "$OWNER" --url "$url" >/dev/null && echo "  added $url"
done
echo "==> board ready: https://github.com/users/$OWNER/projects/$num"
