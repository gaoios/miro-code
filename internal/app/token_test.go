package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestReadClaudeCredentialsFile_Valid verifies we can read a well-formed
// credentials.json with a not-yet-expired token.
func TestReadClaudeCredentialsFile_Valid(t *testing.T) {
	tmpHome := t.TempDir()
	claudeDir := filepath.Join(tmpHome, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := claudeKeychainPayload{
		ClaudeAIOauth: claudeKeychainCreds{
			AccessToken:  "sk-ant-oat01-testtoken",
			RefreshToken: "sk-ant-ort01-testrefresh",
			ExpiresAt:    time.Now().Add(1 * time.Hour).UnixMilli(),
		},
	}
	data, _ := json.Marshal(payload)
	if err := os.WriteFile(filepath.Join(claudeDir, ".credentials.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	oldHome := os.Getenv("HOME")
	t.Cleanup(func() { os.Setenv("HOME", oldHome) })
	os.Setenv("HOME", tmpHome)

	got := readClaudeCredentialsFile(func(string, ...any) {})
	if got != "sk-ant-oat01-testtoken" {
		t.Errorf("expected token, got %q", got)
	}
}

// TestReadClaudeCredentialsFile_Expired verifies we skip tokens that are
// already past expiry — returning stale tokens just shifts the login prompt
// from `claude` to the next API call, which is worse UX than failing here.
func TestReadClaudeCredentialsFile_Expired(t *testing.T) {
	tmpHome := t.TempDir()
	claudeDir := filepath.Join(tmpHome, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := claudeKeychainPayload{
		ClaudeAIOauth: claudeKeychainCreds{
			AccessToken: "sk-ant-oat01-staletoken",
			ExpiresAt:   time.Now().Add(-1 * time.Hour).UnixMilli(),
		},
	}
	data, _ := json.Marshal(payload)
	if err := os.WriteFile(filepath.Join(claudeDir, ".credentials.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	oldHome := os.Getenv("HOME")
	t.Cleanup(func() { os.Setenv("HOME", oldHome) })
	os.Setenv("HOME", tmpHome)

	if got := readClaudeCredentialsFile(func(string, ...any) {}); got != "" {
		t.Errorf("expected empty (expired), got %q", got)
	}
}

// TestReadClaudeCredentialsFile_Missing verifies we return "" for a missing
// file rather than crashing or leaking the error.
func TestReadClaudeCredentialsFile_Missing(t *testing.T) {
	tmpHome := t.TempDir()
	oldHome := os.Getenv("HOME")
	t.Cleanup(func() { os.Setenv("HOME", oldHome) })
	os.Setenv("HOME", tmpHome)

	if got := readClaudeCredentialsFile(func(string, ...any) {}); got != "" {
		t.Errorf("expected empty for missing file, got %q", got)
	}
}

// TestReadClaudeCredentialsFile_NoExpiry verifies we accept tokens without
// an expiry field (shouldn't happen in practice, but don't crash).
func TestReadClaudeCredentialsFile_NoExpiry(t *testing.T) {
	tmpHome := t.TempDir()
	claudeDir := filepath.Join(tmpHome, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// ExpiresAt = 0 → treat as "unknown", not "expired"
	data := []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat01-noexpiry"}}`)
	if err := os.WriteFile(filepath.Join(claudeDir, ".credentials.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	oldHome := os.Getenv("HOME")
	t.Cleanup(func() { os.Setenv("HOME", oldHome) })
	os.Setenv("HOME", tmpHome)

	if got := readClaudeCredentialsFile(func(string, ...any) {}); got != "sk-ant-oat01-noexpiry" {
		t.Errorf("expected token, got %q", got)
	}
}

// TestFetchTokenFromLocalServer_NoConfig verifies we bail out cleanly when
// there's no server config to IPC to. Important: don't block, don't panic.
func TestFetchTokenFromLocalServer_NoConfig(t *testing.T) {
	// Point appDir at a temp dir with no config.json — loadServerConfig
	// returns defaults (empty Token), which fetchTokenFromLocalServer
	// should treat as "no server configured".
	tmpDir := t.TempDir()
	origAppDir := appDir
	t.Cleanup(func() { appDir = origAppDir })
	appDir = tmpDir

	if got := fetchTokenFromLocalServer(); got != "" {
		t.Errorf("expected empty with no config, got %q", got)
	}
}
