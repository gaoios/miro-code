#!/usr/bin/env bash
set -euo pipefail

KIMI_BIN="${KIMI_BIN:-kimi}"
KIMI_CWD="${KIMI_CWD:-$(pwd)}"
KIMI_PROMPT="${KIMI_PROMPT:-Reply with exactly: KIMI_SMOKE_OK}"

args=(
  --print
  --output-format text
  --final-message-only
  --work-dir "$KIMI_CWD"
  --prompt "$KIMI_PROMPT"
)
if [[ -n "${KIMI_MODEL:-}" ]]; then
  args+=(--model "$KIMI_MODEL")
fi

output="$($KIMI_BIN "${args[@]}")"
if [[ "$output" != *"KIMI_SMOKE_OK"* ]]; then
  printf 'unexpected Kimi output: %s\n' "$output" >&2
  exit 1
fi
printf '%s\n' "$output"
