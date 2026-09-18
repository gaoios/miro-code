# Linux Server Deployment Guide

Deploy soul-cli server on a fresh Ubuntu/Debian Linux machine. Tested on Ubuntu 24.04 LTS.

## Prerequisites

| Component | Version | Purpose |
|-----------|---------|---------|
| Go | 1.25+ | Build soul-cli from source |
| Node.js | 22 LTS | Claude Code runtime |
| Claude Code | latest | AI backend (`npm install -g @anthropic-ai/claude-code`) |
| git | any | Clone source repo |

## Quick Start (copy-paste)

```bash
# 1. Install Go 1.25
wget -q "https://go.dev/dl/go1.25.0.linux-amd64.tar.gz" -O /tmp/go.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf /tmp/go.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin:$HOME/.local/bin' >> ~/.bashrc
export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin:$HOME/.local/bin

# 2. Install Node.js 22 + Claude Code
curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -
sudo apt-get install -y nodejs git build-essential
sudo npm install -g @anthropic-ai/claude-code

# 3. Set Anthropic auth (OAuth token or API key)
export ANTHROPIC_API_KEY="sk-ant-oat01-your-token-here"

# 4. Verify
go version        # go1.25.0 linux/amd64
claude --version   # 2.x.x (Claude Code)
claude auth status # loggedIn: true

# 5. Clone and build soul-cli
git clone https://github.com/kiyor/soul-cli.git ~/soul-cli
cd ~/soul-cli
VERSION=$(cat VERSION)
COMMIT=$(git rev-parse --short HEAD)
go build -ldflags "-X github.com/kiyor/soul-cli/internal/app.buildVersion=${VERSION} -X github.com/kiyor/soul-cli/internal/app.buildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ) -X github.com/kiyor/soul-cli/internal/app.buildCommit=${COMMIT}" -o soul .
mkdir -p ~/.local/bin && cp soul ~/.local/bin/soul

# 6. Initialize workspace
export MIRO_HOME=$HOME/.soul  # or any directory you want
soul init --archetype engineer --name my-ai --owner yourname --tz America/Los_Angeles

# 7. Start server
MIRO_HOME=$HOME/.soul CLAUDE_CODE_OAUTH_TOKEN="sk-ant-oat01-..." \
  soul server --token your-secret-token --host 0.0.0.0 --port 9847
```

## Step-by-Step

### 1. Install Go

soul-cli requires Go 1.25+. The Ubuntu apt version is too old; install from golang.org:

```bash
wget -q "https://go.dev/dl/go1.25.0.linux-amd64.tar.gz" -O /tmp/go.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf /tmp/go.tar.gz
rm /tmp/go.tar.gz

# Add to PATH permanently
echo 'export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin:$HOME/.local/bin' >> ~/.bashrc
source ~/.bashrc

# Verify
go version
```

### 2. Install Node.js and Claude Code

Claude Code is an npm package that requires Node.js 22+:

```bash
curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -
sudo apt-get install -y nodejs git build-essential

# Install Claude Code globally
sudo npm install -g @anthropic-ai/claude-code

# Verify
node --version     # v22.x.x
claude --version   # 2.x.x (Claude Code)

# Symlink claude to expected path (soul-cli checks ~/.local/bin/claude first)
mkdir -p ~/.local/bin
ln -sf $(which claude) ~/.local/bin/claude
```

### 3. Authenticate Claude Code

Claude Code needs an Anthropic credential. On Linux (no macOS Keychain), use environment variables:

```bash
# Option A: OAuth token (preferred)
export CLAUDE_CODE_OAUTH_TOKEN="sk-ant-oat01-..."

# Option B: Standard API key
export ANTHROPIC_API_KEY="sk-ant-api03-..."

# Verify authentication
claude auth status
# Should show: loggedIn: true, authMethod: oauth_token (or api_key)
```

> **Important**: The server process must have the credential in its environment. Set it in the systemd unit file or your shell profile.

### 4. Build soul-cli

```bash
git clone https://github.com/kiyor/soul-cli.git ~/soul-cli
cd ~/soul-cli

# Build with version info (never use bare `go build`)
VERSION=$(cat VERSION)
COMMIT=$(git rev-parse --short HEAD)
DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)

go build -ldflags "-X github.com/kiyor/soul-cli/internal/app.buildVersion=${VERSION} -X github.com/kiyor/soul-cli/internal/app.buildDate=${DATE} -X github.com/kiyor/soul-cli/internal/app.buildCommit=${COMMIT}" -o soul .

# Install
mkdir -p ~/.local/bin
cp soul ~/.local/bin/soul

# Verify
soul --version
```

#### Custom binary name

The binary name determines the app identity. Name it whatever you want:

```bash
# Build as "aria"
go build -ldflags "... -X github.com/kiyor/soul-cli/internal/app.defaultAppName=aria" -o aria .
cp aria ~/.local/bin/aria

# Now use: aria init, aria server, etc.
```

### 5. Initialize Workspace

```bash
# Set home directory (default: ~/.openclaw)
export MIRO_HOME=$HOME/.soul

# Non-interactive init
soul init --archetype engineer --name my-ai --owner yourname --tz America/Los_Angeles

# Or interactive:
soul init
```

Available archetypes: `companion`, `engineer`, `steward`, `mentor`, `custom`.

This creates:
```
~/.soul/workspace/
├── SOUL.md       — personality & values
├── IDENTITY.md   — name & role
├── USER.md       — your preferences
├── AGENTS.md     — behavioral rules
├── MEMORY.md     — memory index
├── memory/       — daily notes
└── skills/       — skill definitions
```

Edit `SOUL.md` and `IDENTITY.md` to customize personality.

### 6. Start Server

```bash
# Foreground (for testing)
MIRO_HOME=$HOME/.soul CLAUDE_CODE_OAUTH_TOKEN="sk-ant-oat01-..." \
  soul server --token my-secret --host 0.0.0.0 --port 9847

# Background
nohup env MIRO_HOME=$HOME/.soul CLAUDE_CODE_OAUTH_TOKEN="sk-ant-oat01-..." \
  soul server --token my-secret --host 0.0.0.0 --port 9847 \
  > /var/log/soul-server.log 2>&1 &
```

### 7. Verify

```bash
# Health check
curl http://localhost:9847/api/health

# Create a session with soul injection
curl -X POST http://localhost:9847/api/sessions \
  -H "Authorization: Bearer my-secret" \
  -H "Content-Type: application/json" \
  -d '{"name":"test","soul_files":true,"replace_soul":true}'

# Send a message (replace SESSION_ID)
curl -X POST http://localhost:9847/api/sessions/SESSION_ID/message \
  -H "Authorization: Bearer my-secret" \
  -H "Content-Type: application/json" \
  -d '{"message":"Who are you? Reply in one sentence."}'

# Read response (wait ~10s for Claude to respond)
curl http://localhost:9847/api/history/SESSION_ID/messages \
  -H "Authorization: Bearer my-secret"
```

## Systemd Service

For production, run as a systemd service:

```bash
sudo tee /etc/systemd/system/soul-server.service << 'EOF'
[Unit]
Description=soul-cli server
After=network.target

[Service]
Type=simple
User=youruser
Environment=MIRO_HOME=/home/youruser/.soul
Environment=CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-your-token
Environment=PATH=/usr/local/go/bin:/home/youruser/.local/bin:/usr/bin:/bin
ExecStart=/home/youruser/.local/bin/soul server --token your-secret --host 0.0.0.0 --port 9847
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable soul-server
sudo systemctl start soul-server

# Check status
sudo systemctl status soul-server
journalctl -u soul-server -f
```

## Configuration

### config.json

Optional config file at `$MIRO_HOME/workspace/config.json`:

```json
{
  "server": {
    "token": "your-secret-token",
    "host": "0.0.0.0",
    "port": 9847
  },
  "defaultReplaceSoul": true,
  "defaultInteractiveModel": "claude-opus-4-7[1m]"
}
```

### Environment Variables

| Variable | Description |
|----------|-------------|
| `MIRO_HOME` | Base directory (default: `~/.openclaw`) |
| `ANTHROPIC_API_KEY` | Anthropic OAuth token or API key |
| `MIRO_SERVER_TOKEN` | Server auth token (alt to `--token`) |

> Note: The env var prefix matches the binary name. If your binary is `aria`, use `ARIA_HOME`, `ARIA_SERVER_TOKEN`, etc.

## Gotchas

### Claude binary not found

If you get `fork/exec ~/.local/bin/claude: no such file or directory`:
- Claude Code installed via npm goes to `/usr/bin/claude` or `/usr/local/bin/claude`
- soul-cli checks `~/.local/bin/claude` first, then falls back to PATH search
- Quick fix: `ln -sf $(which claude) ~/.local/bin/claude`

### soul_enabled: false

When creating sessions via API, pass `"soul_files": true` and `"replace_soul": true` to enable soul injection:

```json
{"name": "my-session", "soul_files": true, "replace_soul": true}
```

Without these flags, Claude runs without your soul prompt.

### OAuth token on Linux

macOS Claude Code uses Keychain for auth. Linux has no Keychain — set `ANTHROPIC_API_KEY` environment variable instead. Both OAuth tokens (`sk-ant-oat01-...`) and standard API keys (`sk-ant-api03-...`) work.

### Build without ldflags

Never use bare `go build .` — always include `-ldflags` for version injection:

```bash
go build -ldflags "-X github.com/kiyor/soul-cli/internal/app.buildVersion=$(cat VERSION) -X github.com/kiyor/soul-cli/internal/app.buildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ) -X github.com/kiyor/soul-cli/internal/app.buildCommit=$(git rev-parse --short HEAD)" -o soul .
```

## Updating

```bash
cd ~/soul-cli
git pull
VERSION=$(cat VERSION)
COMMIT=$(git rev-parse --short HEAD)
go build -ldflags "-X github.com/kiyor/soul-cli/internal/app.buildVersion=${VERSION} -X github.com/kiyor/soul-cli/internal/app.buildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ) -X github.com/kiyor/soul-cli/internal/app.buildCommit=${COMMIT}" -o soul .
cp soul ~/.local/bin/soul
sudo systemctl restart soul-server
```
