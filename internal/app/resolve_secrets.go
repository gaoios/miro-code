package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

var secretRefPattern = regexp.MustCompile(`(vault|env)://[^\s\)\]]+`)

// resolveSecretRefs replaces vault:// and env:// references in text with resolved values.
// Unresolved references are left as-is (no data loss).
func resolveSecretRefs(content string) string {
	return secretRefPattern.ReplaceAllStringFunc(content, func(ref string) string {
		switch {
		case strings.HasPrefix(ref, "env://"):
			varName := strings.TrimPrefix(ref, "env://")
			if val := os.Getenv(varName); val != "" {
				return val
			}
			fmt.Fprintf(os.Stderr, "[%s] warning: unresolved secret ref: %s\n", appName, ref)
			return ref
		case strings.HasPrefix(ref, "vault://"):
			path, key := parseVaultRef(ref)
			if val, err := resolveVaultSecret(path, key); err == nil {
				return val
			} else {
				fmt.Fprintf(os.Stderr, "[%s] warning: vault resolve failed for %s: %v\n", appName, ref, err)
				return ref
			}
		}
		return ref
	})
}

func parseVaultRef(ref string) (path, key string) {
	s := strings.TrimPrefix(ref, "vault://")
	if idx := strings.LastIndex(s, "#"); idx >= 0 {
		return s[:idx], s[idx+1:]
	}
	return s, ""
}

func resolveVaultSecret(path, key string) (string, error) {
	addr := os.Getenv("VAULT_ADDR")
	if addr == "" {
		addr = "http://127.0.0.1:8200"
	}
	token := os.Getenv("VAULT_TOKEN")
	if token == "" {
		if data, err := os.ReadFile(os.ExpandEnv("$HOME/.vault-token")); err == nil {
			token = strings.TrimSpace(string(data))
		}
	}
	if token == "" {
		return "", fmt.Errorf("no vault token available")
	}

	client := &http.Client{Timeout: 2 * time.Second}
	req, _ := http.NewRequest("GET", addr+"/v1/"+path, nil)
	req.Header.Set("X-Vault-Token", token)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// Try KV v2 first (data nested under data.data)
	var v2 struct {
		Data struct {
			Data map[string]interface{} `json:"data"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &v2) == nil {
		if val, ok := v2.Data.Data[key]; ok {
			return fmt.Sprint(val), nil
		}
	}

	// KV v1 fallback (data directly under data)
	var v1 struct {
		Data map[string]interface{} `json:"data"`
	}
	if json.Unmarshal(body, &v1) == nil {
		if val, ok := v1.Data[key]; ok {
			return fmt.Sprint(val), nil
		}
	}

	return "", fmt.Errorf("key %q not found in vault path %s", key, path)
}
