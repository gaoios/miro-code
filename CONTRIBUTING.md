# Contributing to miro

## Setup

1. Clone the repo and build:
   ```bash
   git clone https://github.com/kiyor/soul-cli.git
   cd miro
   go build -o miro .
   ```

2. Create your workspace (if you don't have OpenClaw):
   ```bash
   mkdir -p ~/.openclaw/workspace/memory
   ```

3. Write your soul files — see [examples/](examples/) for templates:
   - `SOUL.md` — personality & values
   - `IDENTITY.md` — name & role
   - `USER.md` — your profile
   - `AGENTS.md` — behavioral rules

4. Copy and edit config:
   ```bash
   cp config.example.json config.json
   cp services.example.json services.json
   ```

## Running Tests

```bash
go test ./...
```

Most tests use temp dirs and in-memory SQLite — no external services needed.

Some tests (`TestBuildSkillIndex`, `TestBuildPrompt`) read from `~/.openclaw/` and only pass on a configured machine. They'll be skipped or fail gracefully on a fresh setup.

## Code Style

- Application code lives in `internal/app` (single package); root `main.go` is a thin shim; shared providers in `pkg/`
- Comments in English
- Test names follow `TestFunctionName_Scenario` convention
- Chinese text in test fixtures is intentional (testing CJK tokenization)

## Before Submitting

1. `go test ./...` passes
2. `go vet ./...` clean
3. No secrets or personal paths in code
4. Config files (`config.json`, `services.json`) are gitignored — don't commit them
