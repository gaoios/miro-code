package app

import (
	"os"
	"testing"
)

func TestResolveSecretRefs_EnvRef(t *testing.T) {
	os.Setenv("TEST_SOUL_TOKEN", "abc123")
	defer os.Unsetenv("TEST_SOUL_TOKEN")

	got := resolveSecretRefs("Token: env://TEST_SOUL_TOKEN end")
	want := "Token: abc123 end"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveSecretRefs_EnvMissing(t *testing.T) {
	got := resolveSecretRefs("Token: env://NONEXISTENT_VAR_XYZ_12345 end")
	if got != "Token: env://NONEXISTENT_VAR_XYZ_12345 end" {
		t.Errorf("missing env ref should be preserved, got %q", got)
	}
}

func TestResolveSecretRefs_NoRefs(t *testing.T) {
	input := "plain text without any refs"
	got := resolveSecretRefs(input)
	if got != input {
		t.Errorf("no-ref text changed: %q", got)
	}
}

func TestResolveSecretRefs_MultipleRefs(t *testing.T) {
	os.Setenv("A_TOKEN_TEST", "aaa")
	os.Setenv("B_TOKEN_TEST", "bbb")
	defer os.Unsetenv("A_TOKEN_TEST")
	defer os.Unsetenv("B_TOKEN_TEST")

	got := resolveSecretRefs("a=env://A_TOKEN_TEST b=env://B_TOKEN_TEST")
	want := "a=aaa b=bbb"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveSecretRefs_MixedContent(t *testing.T) {
	os.Setenv("MY_KEY_TEST", "secret")
	defer os.Unsetenv("MY_KEY_TEST")

	input := "- API Key: env://MY_KEY_TEST\n- URL: https://example.com\n- Vault: vault://secret/data/test#key"
	got := resolveSecretRefs(input)
	if got == input {
		t.Error("expected at least env ref to be resolved")
	}
	// env ref should be resolved
	if !contains(got, "secret") {
		t.Error("env://MY_KEY_TEST should be resolved to 'secret'")
	}
	// https:// should NOT be touched (not a vault/env ref)
	if !contains(got, "https://example.com") {
		t.Error("https:// URLs should not be modified")
	}
}

func TestResolveVaultSecret_Unavailable(t *testing.T) {
	os.Setenv("VAULT_ADDR", "http://127.0.0.1:1") // unreachable port
	os.Setenv("VAULT_TOKEN", "fake")
	defer os.Unsetenv("VAULT_ADDR")
	defer os.Unsetenv("VAULT_TOKEN")

	_, err := resolveVaultSecret("secret/test", "key")
	if err == nil {
		t.Error("expected error for unreachable vault")
	}
}

func TestResolveVaultSecret_NoToken(t *testing.T) {
	origToken := os.Getenv("VAULT_TOKEN")
	os.Unsetenv("VAULT_TOKEN")
	defer func() {
		if origToken != "" {
			os.Setenv("VAULT_TOKEN", origToken)
		}
	}()

	// Also ensure no ~/.vault-token
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", t.TempDir())
	defer os.Setenv("HOME", origHome)

	_, err := resolveVaultSecret("secret/test", "key")
	if err == nil {
		t.Error("expected error when no vault token available")
	}
}

func TestParseVaultRef(t *testing.T) {
	tests := []struct {
		input    string
		wantPath string
		wantKey  string
	}{
		{"vault://secret/data/jira#token", "secret/data/jira", "token"},
		{"vault://secret/data/civitai#api_key", "secret/data/civitai", "api_key"},
		{"vault://secret/data/test", "secret/data/test", ""},
		{"vault://kv/data/multi#a#b", "kv/data/multi#a", "b"}, // LastIndex of #
	}
	for _, tt := range tests {
		path, key := parseVaultRef(tt.input)
		if path != tt.wantPath || key != tt.wantKey {
			t.Errorf("parseVaultRef(%q) = (%q, %q), want (%q, %q)",
				tt.input, path, key, tt.wantPath, tt.wantKey)
		}
	}
}

func TestSecretRefPattern(t *testing.T) {
	// Should match vault:// and env:// but NOT https:// or http://
	tests := []struct {
		input string
		want  []string
	}{
		{"env://MY_VAR", []string{"env://MY_VAR"}},
		{"vault://secret/path#key", []string{"vault://secret/path#key"}},
		{"https://example.com", nil},
		{"http://localhost:8080", nil},
		{"ftp://files.example.com", nil},
		{"mixed env://A and vault://B#c text", []string{"env://A", "vault://B#c"}},
	}
	for _, tt := range tests {
		got := secretRefPattern.FindAllString(tt.input, -1)
		if len(got) != len(tt.want) {
			t.Errorf("pattern on %q: got %v, want %v", tt.input, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("pattern on %q[%d]: got %q, want %q", tt.input, i, got[i], tt.want[i])
			}
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
