package app

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveProvider_AllowsOllamaWithoutBaseURL covers a soul-cli config
// shape that used to fail early validation: an ollama provider that omits
// baseUrl (the subpackage defaults to http://localhost:11434). The test lives
// here, not in pkg/provider/ollama, because resolveProvider + appHome are
// main-package implementation details.
func TestResolveProvider_AllowsOllamaWithoutBaseURL(t *testing.T) {
	dir := t.TempDir()
	cfg := `{
		"providers": {
			"ollama": {
				"type": "ollama",
				"models": ["llama3.2"]
			}
		}
	}`
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data", "config.json"), []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}

	origAppHome := appHome
	appHome = dir
	defer func() { appHome = origAppHome }()

	prov := resolveProvider("ollama")
	if prov == nil {
		t.Fatal("resolveProvider(ollama) = nil, want provider")
	}
	if prov.Type != "ollama" {
		t.Fatalf("resolveProvider(ollama) type = %q, want ollama", prov.Type)
	}
}
