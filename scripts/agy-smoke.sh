#!/usr/bin/env bash
set -euo pipefail

AGY_BIN="${AGY_BIN:-agy}"
AGY_CWD="${AGY_CWD:-$(pwd)}"
AGY_MODEL="${AGY_MODEL:-gemini-3.1-pro-low}"
AGY_PROMPT="${AGY_PROMPT:-Reply with exactly: AGY_SMOKE_OK}"

output="$(cd "$AGY_CWD" && "$AGY_BIN" \
  --print "$AGY_PROMPT" \
  --model "$AGY_MODEL" \
  --effort low \
  --mode plan \
  --print-timeout 5m)"
if [[ "$output" != *"AGY_SMOKE_OK"* ]]; then
  printf 'unexpected Antigravity output: %s\n' "$output" >&2
  exit 1
fi
printf '%s\n' "$output"
