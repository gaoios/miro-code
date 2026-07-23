#!/usr/bin/env bash
#
# grok-smoke.sh — Grok Build headless worker smoke test.
#
# This validates the raw headless command used by BackendGrok. Go tests cover
# the Backend and SSE bridge; this probe confirms the installed CLI can return
# machine-readable output.

set -euo pipefail

GREEN=$'\033[32m'; RED=$'\033[31m'; YELLOW=$'\033[33m'; BOLD=$'\033[1m'; CLR=$'\033[0m'

ok()    { printf "${GREEN}✓${CLR} %s\n" "$*"; }
fail()  { printf "${RED}✗${CLR} %s\n" "$*" >&2; }
note()  { printf "${YELLOW}!${CLR} %s\n" "$*"; }
hdr()   { printf "\n${BOLD}=== %s ===${CLR}\n" "$*"; }

if [[ -z "${GROK_BIN:-}" ]]; then
  GROK_BIN="$(command -v grok || true)"
fi
GROK_PROMPT="${GROK_PROMPT:-Reply with exactly: GROK_SMOKE_OK}"
GROK_EXPECTED="${GROK_EXPECTED:-GROK_SMOKE_OK}"
GROK_CWD="${GROK_CWD:-$(pwd)}"
GROK_TIMEOUT_SECONDS="${GROK_TIMEOUT_SECONDS:-120}"

hdr "preflight"

if [[ -z "$GROK_BIN" || ! -x "$GROK_BIN" ]]; then
  fail "grok binary not found"
  cat <<EOF >&2

  Install or expose Grok Build first, then rerun:
    GROK_BIN=/path/to/grok $0

EOF
  exit 10
fi
ok "grok binary: $GROK_BIN ($("$GROK_BIN" --version 2>/dev/null | head -1))"

if [[ ! -d "$GROK_CWD" ]]; then
  fail "GROK_CWD is not a directory: $GROK_CWD"
  exit 10
fi
ok "working directory: $GROK_CWD"

if ! command -v python3 >/dev/null 2>&1; then
  fail "python3 required for JSON output validation"
  exit 10
fi
ok "python3: $(python3 --version 2>&1)"

hdr "stage 1 — headless single-turn probe"

args=(
  -p "$GROK_PROMPT"
  --cwd "$GROK_CWD"
  --output-format json
  --max-turns 1
  --no-subagents
  --disable-web-search
  --no-memory
)

if [[ -n "${GROK_MODEL:-}" ]]; then
  args=(-m "$GROK_MODEL" "${args[@]}")
fi

set +e
out="$(python3 - "$GROK_TIMEOUT_SECONDS" "$GROK_BIN" "${args[@]}" <<'PY'
import subprocess
import sys

timeout = float(sys.argv[1])
command = sys.argv[2:]
try:
    result = subprocess.run(
        command,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        timeout=timeout,
    )
except subprocess.TimeoutExpired as exc:
    if exc.stdout:
        output = exc.stdout.decode() if isinstance(exc.stdout, bytes) else exc.stdout
        print(output, end="")
    print(f"grok smoke timed out after {timeout:g}s", file=sys.stderr)
    raise SystemExit(124)
print(result.stdout, end="")
raise SystemExit(result.returncode)
PY
)"
code=$?
set -e

if [[ "$code" -ne 0 ]]; then
  fail "grok exited with code $code"
  printf "%s\n" "$out" >&2
  exit 1
fi

text="$(
  printf "%s" "$out" | python3 -c '
import json, sys
raw = sys.stdin.read()
idx = raw.find("{")
while idx >= 0:
    try:
        obj, _ = json.JSONDecoder().raw_decode(raw[idx:])
        print(obj.get("text", ""))
        raise SystemExit(0)
    except SystemExit:
        raise
    except Exception:
        idx = raw.find("{", idx + 1)
raise SystemExit(2)
'
)"

if [[ "$text" != "$GROK_EXPECTED" ]]; then
  fail "unexpected Grok response: $text"
  printf "%s\n" "$out" >&2
  exit 1
fi

ok "stage 1 PASSED — Grok Build headless output matched: $text"

hdr "result"
ok "grok smoke complete"
