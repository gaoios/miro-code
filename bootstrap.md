# soul-cli Linux Bootstrap

You are deploying **soul-cli** on a fresh Linux server. Follow every step below. Do not skip anything. Do not ask questions or wait for user input — use the defaults specified below for all choices. Execute every command directly.

## Context

soul-cli is a Go CLI that gives Claude Code a persistent soul (identity, memory, skills). It compiles to a single binary, runs an HTTP server with a Web UI, and manages Claude Code sessions with soul prompt injection.

Repo: https://github.com/kiyor/soul-cli

## Environment Detection

First, detect the environment:

```bash
uname -a
cat /etc/os-release 2>/dev/null
go version 2>/dev/null || echo "go: not installed"
node --version 2>/dev/null || echo "node: not installed"
claude --version 2>/dev/null || echo "claude: not installed"
echo "ANTHROPIC_API_KEY=${ANTHROPIC_API_KEY:+set}" 
echo "HOME=$HOME"
echo "ARCH=$(uname -m)"
```

Use the output to skip already-installed components.

## Step 1 — Install Go 1.25+

Skip if `go version` shows 1.25+.

For x86_64:
```bash
wget -q "https://go.dev/dl/go1.25.0.linux-amd64.tar.gz" -O /tmp/go.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf /tmp/go.tar.gz && rm /tmp/go.tar.gz
```

For aarch64/arm64:
```bash
wget -q "https://go.dev/dl/go1.25.0.linux-arm64.tar.gz" -O /tmp/go.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf /tmp/go.tar.gz && rm /tmp/go.tar.gz
```

Then add to PATH:
```bash
grep -q '/usr/local/go/bin' ~/.bashrc || echo 'export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin:$HOME/.local/bin' >> ~/.bashrc
export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin:$HOME/.local/bin
```

Verify: `go version` must show `go1.25`.

## Step 2 — Install Node.js 22 + Claude Code

Skip Node.js install if `node --version` shows v22+.
Skip Claude Code install if `claude --version` works.

For Ubuntu/Debian:
```bash
curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -
sudo apt-get install -y nodejs git build-essential
```

For RHEL/CentOS/Fedora:
```bash
curl -fsSL https://rpm.nodesource.com/setup_22.x | sudo bash -
sudo yum install -y nodejs git gcc gcc-c++ make
```

Then install Claude Code:
```bash
sudo npm install -g @anthropic-ai/claude-code
```

Verify: `claude --version` must output a version string.

Ensure claude is discoverable at `~/.local/bin/claude` (soul-cli checks this path first):
```bash
mkdir -p ~/.local/bin
if [ ! -f ~/.local/bin/claude ]; then
  ln -sf $(which claude) ~/.local/bin/claude
fi
```

## Step 3 — Authenticate

Claude Code needs a credential. Check which is available:

- `CLAUDE_CODE_OAUTH_TOKEN` — OAuth token (`<YOUR_ANTHROPIC_TOKEN>`), authenticates as `oauth_token`
- `ANTHROPIC_API_KEY` — standard API key (`sk-ant-api03-...`), authenticates as `api_key`

If neither is set, stop and tell the user:

> No Anthropic credential found. Export one before running this bootstrap:
> ```
> export CLAUDE_CODE_OAUTH_TOKEN="<YOUR_ANTHROPIC_TOKEN>"   # OAuth token (preferred)
> # or
> export ANTHROPIC_API_KEY="sk-ant-api03-..."          # standard API key
> ```

Verify: `claude auth status` must show `loggedIn: true`.

## Step 4 — Build soul-cli

Use `soul` as the binary name:

```bash
BINARY_NAME=soul

git clone https://github.com/kiyor/soul-cli.git ~/soul-cli
cd ~/soul-cli
mkdir -p ~/.local/bin

VERSION=$(cat VERSION)
COMMIT=$(git rev-parse --short HEAD)
DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)

go build -ldflags "-X github.com/kiyor/soul-cli/internal/app.buildVersion=${VERSION} -X github.com/kiyor/soul-cli/internal/app.buildDate=${DATE} -X github.com/kiyor/soul-cli/internal/app.buildCommit=${COMMIT} -X github.com/kiyor/soul-cli/internal/app.defaultAppName=${BINARY_NAME}" -o ~/.local/bin/${BINARY_NAME} .
```

**Important**: Never use bare `go build .` — the ldflags inject version info that the binary needs.

Verify: `~/.local/bin/${BINARY_NAME} --version` must print a version.

Use `${BINARY_NAME}` in place of `soul` for all subsequent commands.

## Step 5 — Initialize workspace

Use defaults — the user can customize later by editing SOUL.md and IDENTITY.md.

```bash
ENV_PREFIX=$(echo ${BINARY_NAME} | tr '[:lower:]-' '[:upper:]_')
export ${ENV_PREFIX}_HOME=$HOME/.${BINARY_NAME}

OWNER=$(whoami)
TZ=$(cat /etc/timezone 2>/dev/null || echo UTC)

${BINARY_NAME} init --archetype engineer --name ${BINARY_NAME} --owner ${OWNER} --tz ${TZ}
```

This creates the workspace with SOUL.md, IDENTITY.md, USER.md, AGENTS.md, MEMORY.md.

## Step 6 — Create systemd service

```bash
ENV_PREFIX=$(echo ${BINARY_NAME} | tr '[:lower:]-' '[:upper:]_')
SERVICE_NAME="${BINARY_NAME}-server"
HOME_DIR="$HOME/.${BINARY_NAME}"

sudo tee /etc/systemd/system/${SERVICE_NAME}.service > /dev/null << EOF
[Unit]
Description=${BINARY_NAME} server
After=network.target

[Service]
Type=simple
User=$(whoami)
Environment=${ENV_PREFIX}_HOME=${HOME_DIR}
Environment=CLAUDE_CODE_OAUTH_TOKEN=${CLAUDE_CODE_OAUTH_TOKEN:-}
Environment=ANTHROPIC_API_KEY=${ANTHROPIC_API_KEY:-}
Environment=PATH=/usr/local/go/bin:$HOME/.local/bin:/usr/local/bin:/usr/bin:/bin
ExecStart=$HOME/.local/bin/${BINARY_NAME} server --token $(openssl rand -hex 16) --host 0.0.0.0 --port 9847
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable ${SERVICE_NAME}
sudo systemctl start ${SERVICE_NAME}
```

Wait 2 seconds, then verify:
```bash
sudo systemctl is-active ${SERVICE_NAME}
curl -sf http://localhost:9847/api/health && echo " OK" || echo " FAILED"
```

## Step 7 — Smoke test

Extract the auth token from the service file:
```bash
TOKEN=$(grep -oP '(?<=--token )\S+' /etc/systemd/system/${SERVICE_NAME}.service)
```

Create a session and verify soul injection:
```bash
# Create session with soul enabled
SESS=$(curl -sf -X POST http://localhost:9847/api/sessions \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"bootstrap-test","soul_files":true,"replace_soul":true}')
SID=$(echo "$SESS" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)

sleep 3

# Send test message
curl -sf -X POST "http://localhost:9847/api/sessions/$SID/message" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"message":"Who are you? Reply in one sentence."}'

# Wait for response
sleep 15

# Read response
curl -sf "http://localhost:9847/api/history/$SID/messages" \
  -H "Authorization: Bearer $TOKEN"
```

If the response mentions the soul name from IDENTITY.md (not just "Claude"), soul injection is working.

## Step 8 — Report

Print a summary:

```
=== soul-cli deployed ===
Binary:    ~/.local/bin/${BINARY_NAME}
Workspace: ~/.<binary-name>/workspace/
Service:   ${SERVICE_NAME}.service (systemd)
URL:       http://<this-machine-ip>:9847
Token:     $TOKEN
Web UI:    http://<this-machine-ip>:9847

Next steps:
  - Edit SOUL.md to customize personality
  - Edit IDENTITY.md to set name and role
  - Open the Web UI in a browser to chat
```

## Step 9 — Soul enrichment (optional but recommended)

If there are existing Claude Code sessions on this machine (in `~/.claude/projects/`), scan them to learn about the user and enrich the soul files.

```bash
# Check for existing session history
find ~/.claude/projects -name "*.jsonl" 2>/dev/null | head -5
```

If JSONL files exist, read a sample (the most recent 2-3 files, first 200 lines each) to understand:
- What kind of work the user does (SRE, frontend, data science, etc.)
- Their communication style (terse? verbose? casual? formal?)
- Technologies they use frequently
- Patterns in how they interact with AI (direct commands? collaborative discussion?)

Then update `SOUL.md` and `USER.md` based on what you learned:
- Add specific speaking examples that match their style
- Add domain knowledge relevant to their work
- Adjust personality traits to complement their communication patterns
- Fill in `USER.md` with observed preferences, tools, and work patterns

This is the "day-0 soul enrichment" — making the AI feel like it already knows the user from the first interaction.

Do NOT copy any sensitive data (tokens, passwords, PII) from the session history into soul files.

## Gotchas you may hit

1. **`go build` fails with "go 1.25 required"** — The apt Go is too old. Must install from golang.org (Step 1).
2. **`claude auth status` shows `loggedIn: false`** — `ANTHROPIC_API_KEY` not set. Linux has no macOS Keychain; the env var is required.
3. **Session creates but `soul_enabled: false`** — Must pass `"soul_files": true, "replace_soul": true` in the create request.
4. **`fork/exec ~/.local/bin/claude: no such file or directory`** — Claude Code is at `/usr/bin/claude` (npm global). Fix: `ln -sf $(which claude) ~/.local/bin/claude`
5. **Port 9847 not reachable** — Check firewall: `sudo ufw allow 9847/tcp` or `sudo iptables -I INPUT -p tcp --dport 9847 -j ACCEPT`.
