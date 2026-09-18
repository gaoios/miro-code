package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ── `weiran token` subcommand ──
//
// Prints a fresh Claude Code OAuth access_token to stdout. Intended for use
// in shell functions / aliases:
//
//	claude() {
//	    local tok=$(weiran token 2>/dev/null)
//	    [[ -n $tok ]] && export CLAUDE_CODE_OAUTH_TOKEN="$tok"
//	    command claude "$@"
//	}
//
// Resolution priority (each step stops on first success):
//
//  1. Env var CLAUDE_CODE_OAUTH_TOKEN (trust caller; no validation)
//  2. Local weiran server via HTTP (the common SSH path — server has keychain
//     access, we don't). Uses config.json server.host/port/token.
//  3. macOS keychain via `security` (works only in GUI sessions; SSH can't).
//  4. ~/.claude/.credentials.json (CC's own fallback file — may be stale
//     because CC deletes it after successful keychain writes, but worth trying).
//
// Exit 0 on success (token on stdout), exit 1 on failure (no output).
// Errors go to stderr so shell substitution `$(weiran token)` gets clean output.

func handleToken(args []string) {
	verbose := false
	for _, a := range args {
		if a == "-v" || a == "--verbose" {
			verbose = true
		}
	}
	logf := func(format string, a ...any) {
		if verbose {
			fmt.Fprintf(os.Stderr, "[%s token] "+format+"\n", append([]any{appName}, a...)...)
		}
	}

	// 1. Env var — trust caller
	if tok := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); tok != "" {
		logf("from env")
		fmt.Println(tok)
		return
	}

	// 2. Local server IPC (uses the same helper that oauthToken() calls)
	if tok := fetchTokenFromLocalServer(); tok != "" {
		logf("from server")
		fmt.Println(tok)
		return
	}

	// 3. Keychain directly (GUI session only)
	if creds := readClaudeKeychainCreds(); creds != nil && creds.AccessToken != "" {
		logf("from keychain")
		fmt.Println(creds.AccessToken)
		return
	}

	// 4. ~/.claude/.credentials.json
	if tok := readClaudeCredentialsFile(logf); tok != "" {
		logf("from ~/.claude/.credentials.json")
		fmt.Println(tok)
		return
	}

	fmt.Fprintf(os.Stderr, "[%s token] no token available (tried env, server, keychain, credentials file)\n", appName)
	os.Exit(1)
}

// readClaudeCredentialsFile reads CC's fallback credentials file.
// CC actively deletes this after successful keychain writes, so it may be
// absent/stale. Still worth trying as a last resort.
func readClaudeCredentialsFile(logf func(string, ...any)) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	path := filepath.Join(home, ".claude", ".credentials.json")
	data, err := os.ReadFile(path)
	if err != nil {
		logf("credentials file unreadable: %v", err)
		return ""
	}
	var p claudeKeychainPayload
	if err := json.Unmarshal(data, &p); err != nil {
		logf("credentials file parse failed: %v", err)
		return ""
	}
	tok := p.ClaudeAIOauth.AccessToken
	if tok == "" {
		return ""
	}
	// Best-effort freshness check. If we know the expiry and it's already past,
	// don't return the stale value — callers should fall through (but since
	// we're already the last step, they'll just fail).
	if p.ClaudeAIOauth.ExpiresAt > 0 {
		if time.UnixMilli(p.ClaudeAIOauth.ExpiresAt).Before(time.Now()) {
			logf("credentials file token expired")
			return ""
		}
	}
	return tok
}
