package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ── Claude Code OAuth refresh warmer ──
//
// Claude Code stores its OAuth credentials on macOS in the login keychain.
// Priority order (from CC src/utils/secureStorage/index.ts + fallbackStorage.ts):
//   1. macOS keychain  (primary)
//   2. ~/.claude/.credentials.json  (fallback, auto-deleted on keychain write)
// So weiran must read from the keychain — the plaintext file is a migration
// relic and can be stale or absent at any moment.
//
// Short-lived access_tokens (~1h) are rotated via refresh_token at
// https://platform.claude.com/v1/oauth/token (CC src/constants/oauth.ts:91,
// src/services/oauth/client.ts:146). CC itself serializes refresh across
// processes via proper-lockfile on claudeDir (src/utils/auth.ts:1491), retries
// ELOCKED up to 5 times with 1–2s jitter.
//
// Why weiran still sees 401 in practice: when the server spawns N concurrent
// CC processes at the moment the access_token is within minutes of expiry,
// all of them queue on the lockfile; if the burst takes longer than
// ~10s of retry budget they fall through, use the stale token, and the
// Anthropic API returns 401 (the first successful refresh has already
// rotated refresh_token, invalidating any in-flight attempts that read the
// cache before the write landed).
//
// Solution: **before** spawning any CC, proactively warm the keychain by
// running one throwaway `claude -p "."` synchronously when expiry is close.
// That single process goes through CC's normal lockfile-protected refresh
// path and writes the new {access_token, refresh_token, expiresAt} back to
// keychain. All subsequent concurrent spawns see a fresh token and never
// trigger their own refresh — race eliminated. Cost: ~1 token of API usage
// per ~55min of active use.

const (
	// keychainServiceBase is the stable portion of CC's keychain service name
	// (CC src/utils/secureStorage/macOsKeychainHelpers.ts:29). Production
	// OAUTH_FILE_SUFFIX is empty, so the prefix is literally "Claude Code".
	// The "-credentials" suffix distinguishes the OAuth entry from the legacy
	// API-key entry under the same base name.
	keychainServiceBase = "Claude Code-credentials"

	// warmThreshold is how close to expiry we run the warmer. Must exceed CC's
	// own isOAuthTokenExpired() margin (a few minutes) — otherwise weiran and
	// CC would both decide to refresh at the same time, defeating the whole
	// purpose. 10min is comfortable.
	warmThreshold = 10 * time.Minute

	// warmCooldown suppresses re-entry after a successful warm. One warm is
	// good for ~55min, but we use 60s here as a lightweight dedupe between
	// rapid concurrent callers — the expiry check itself will gate anything
	// past the cooldown.
	warmCooldown = 60 * time.Second

	// warmTimeout caps how long the warmer subprocess may run. A normal
	// refresh + tiny prompt + exit completes in 3–5s; anything longer
	// suggests network trouble or a hanging CC and we move on.
	warmTimeout = 20 * time.Second
)

// claudeKeychainCreds mirrors the CC credential payload stored in the keychain.
// The keychain entry's password field is a JSON object whose sole top-level
// key is "claudeAiOauth" (see the jq output from ~/.claude/.credentials.json,
// which CC uses the same shape for).
type claudeKeychainCreds struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"` // unix ms
}

type claudeKeychainPayload struct {
	ClaudeAIOauth claudeKeychainCreds `json:"claudeAiOauth"`
}

var (
	warmerMu     sync.Mutex
	lastWarmedAt time.Time
)

// claudeKeychainServiceName computes the keychain service string CC uses for
// the current CLAUDE_CONFIG_DIR. Matches src/utils/secureStorage/macOsKeychainHelpers.ts:29:
//
//	`Claude Code${OAUTH_FILE_SUFFIX}-credentials${dirHash}`
//
// where dirHash is empty for the default config dir and "-<sha256(path)[:8]>"
// otherwise. Production builds have an empty OAUTH_FILE_SUFFIX.
func claudeKeychainServiceName() string {
	name := keychainServiceBase
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		sum := sha256.Sum256([]byte(dir))
		name = name + "-" + hex.EncodeToString(sum[:])[:8]
	}
	return name
}

// readClaudeKeychainCreds spawns `security find-generic-password -w` to read
// the raw JSON payload from the login keychain. Returns nil on any error:
// - keychain locked / user denied access
// - entry not present (user hasn't run `claude login` yet)
// - `security` binary missing (non-macOS or hardened env)
// In all of those cases, the caller silently skips warming and CC falls back
// to its own auth path — the warmer is an optimization, never a dependency.
func readClaudeKeychainCreds() *claudeKeychainCreds {
	user := os.Getenv("USER")
	if user == "" {
		return nil
	}
	cmd := exec.Command("security", "find-generic-password",
		"-s", claudeKeychainServiceName(),
		"-a", user,
		"-w",
	)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var p claudeKeychainPayload
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &p); err != nil {
		return nil
	}
	if p.ClaudeAIOauth.AccessToken == "" || p.ClaudeAIOauth.ExpiresAt == 0 {
		return nil
	}
	return &p.ClaudeAIOauth
}

// ensureFreshClaudeCreds warms Claude Code's OAuth access_token if it's close
// to expiry. Safe to call on every spawn — the fast path (fresh token or
// recent warm) is a mutex acquire plus one `security` spawn, typically <80ms.
//
// Must only be called when weiran is letting CC manage its own auth (i.e.
// oauthToken() == ""). If weiran is injecting CLAUDE_CODE_OAUTH_TOKEN itself,
// CC never touches the keychain, so warming is pointless.
func ensureFreshClaudeCreds() {
	warmerMu.Lock()
	defer warmerMu.Unlock()

	// Dedupe bursts: if we just warmed, trust it until cooldown elapses.
	// This matters when the server rehydrates many sessions at once —
	// without dedup, we'd spawn the warmer N times before the first finishes.
	if time.Since(lastWarmedAt) < warmCooldown {
		return
	}

	creds := readClaudeKeychainCreds()
	if creds == nil {
		// Keychain empty or unreachable. Let CC handle auth — if user hasn't
		// run `claude login` yet, CC will prompt; no amount of warming helps.
		return
	}

	remaining := time.UnixMilli(creds.ExpiresAt).Sub(time.Now())
	if remaining > warmThreshold {
		return
	}

	// Build a clean env for the warmer:
	// - Strip CLAUDE_CODE_OAUTH_TOKEN so CC actually exercises its keychain/
	//   refresh path instead of just using the injected static token.
	// - Strip ANTHROPIC_BASE_URL so the warmer hits api.anthropic.com directly,
	//   not our own 9091 proxy (which would be circular and pointless).
	env := make([]string, 0, len(os.Environ()))
	for _, v := range os.Environ() {
		if strings.HasPrefix(v, "CLAUDE_CODE_OAUTH_TOKEN=") ||
			strings.HasPrefix(v, "ANTHROPIC_BASE_URL=") {
			continue
		}
		env = append(env, v)
	}

	fmt.Fprintf(os.Stderr, "[%s] oauth-warmer: access_token expires in %s → refreshing\n",
		appName, remaining.Round(time.Second))

	// `claude -p "."` forces CC's startup path to run checkAndRefreshOAuthTokenIfNeeded
	// (src/utils/auth.ts), then sends a minimal prompt. The API roundtrip costs
	// ~$0.0002 but guarantees the refresh actually happened (if it didn't, the
	// API call itself would 401 and we'd see it in CC's output).
	//
	// We bypass the shell and don't tie stdin/stdout to weiran's terminal —
	// the warmer must be invisible to the user.
	cmd := exec.Command(claudeBin, "-p", ".")
	cmd.Env = env
	cmd.Stdin = strings.NewReader("")
	cmd.Stdout = nil
	cmd.Stderr = nil
	// Keep the working dir stable; CC reads per-project config otherwise
	// and spamming "no project" noise into logs is annoying.
	if wd, err := os.Getwd(); err == nil {
		cmd.Dir = wd
	} else {
		cmd.Dir = filepath.Dir(claudeBin)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	select {
	case err := <-done:
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] oauth-warmer: warm failed: %v\n", appName, err)
		}
	case <-time.After(warmTimeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		fmt.Fprintf(os.Stderr, "[%s] oauth-warmer: timed out after %s\n", appName, warmTimeout)
	}
	// Record warm time even on failure — retrying immediately is unlikely to
	// help and would spam the user with subprocess overhead. Cooldown expires
	// in 60s for a fresh attempt if the spawn path asks for another warm.
	lastWarmedAt = time.Now()
}
