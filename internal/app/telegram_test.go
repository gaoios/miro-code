package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGetTelegramToken_FromConfig(t *testing.T) {
	token := getTelegramToken()
	if token == "" {
		t.Skip("no telegram token configured (not on Kiyor's machine)")
	}
	if len(token) < 20 {
		t.Errorf("token too short: %d chars", len(token))
	}
}

// setTestHome overrides home, appHome, and appDir for test isolation
func setTestHome(t *testing.T, dir string) {
	t.Helper()
	origHome := home
	origAppHome := appHome
	origAppDir := appDir
	origCachedToken := cachedTelegramToken
	home = dir
	appHome = filepath.Join(dir, ".weiran")
	appDir = filepath.Join(dir, ".weiran", "data")
	cachedTelegramToken = "" // clear cache for test isolation
	t.Cleanup(func() {
		home = origHome
		appHome = origAppHome
		appDir = origAppDir
		cachedTelegramToken = origCachedToken
	})
}

func TestGetTelegramToken_MissingConfig(t *testing.T) {
	setTestHome(t, "/nonexistent")

	token := getTelegramToken()
	if token != "" {
		t.Errorf("expected empty token for missing config, got %q", token)
	}
}

func TestGetTelegramToken_BadJSON(t *testing.T) {
	dir := t.TempDir()
	setTestHome(t, dir)

	dataDir := filepath.Join(dir, ".weiran", "data")
	os.MkdirAll(dataDir, 0755)
	os.WriteFile(filepath.Join(dataDir, "config.json"), []byte("not json"), 0644)

	token := getTelegramToken()
	if token != "" {
		t.Errorf("expected empty token for bad JSON, got %q", token)
	}
}

func TestGetTelegramToken_EmptyConfig(t *testing.T) {
	dir := t.TempDir()
	setTestHome(t, dir)

	dataDir := filepath.Join(dir, ".weiran", "data")
	os.MkdirAll(dataDir, 0755)
	os.WriteFile(filepath.Join(dataDir, "config.json"), []byte(`{}`), 0644)

	token := getTelegramToken()
	if token != "" {
		t.Errorf("expected empty token for empty config, got %q", token)
	}
}

func TestGetTelegramToken_ConfigJSON(t *testing.T) {
	dir := t.TempDir()
	setTestHome(t, dir)

	dataDir := filepath.Join(dir, ".weiran", "data")
	os.MkdirAll(dataDir, 0755)
	config := `{"server":{"telegram":{"botToken":"test-token-12345678901234567890"}}}`
	os.WriteFile(filepath.Join(dataDir, "config.json"), []byte(config), 0644)

	token := getTelegramToken()
	if token != "test-token-12345678901234567890" {
		t.Errorf("expected config.json token, got %q", token)
	}
}

func TestTrySendTelegram_NoToken(t *testing.T) {
	setTestHome(t, "/nonexistent")
	// Temporarily re-enable telegram so we actually test the no-token path
	disableTelegram = false
	t.Cleanup(func() { disableTelegram = true })

	err := trySendTelegram("test message")
	if err == nil {
		t.Error("expected error when no token")
	}
}

func TestTrySendTelegramPhoto_NoToken(t *testing.T) {
	setTestHome(t, "/nonexistent")
	disableTelegram = false
	t.Cleanup(func() { disableTelegram = true })

	err := trySendTelegramPhoto("https://example.com/photo.jpg", "test")
	if err == nil {
		t.Error("expected error when no token")
	}
}
