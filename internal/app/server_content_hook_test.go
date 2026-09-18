package app

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func setupContentHookTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	origDB := dbPath
	dbPath = filepath.Join(dir, "test.db")
	t.Cleanup(func() { dbPath = origDB })

	db, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ensureContentHookTable(db)
	return db
}

func TestEvalPostAssistantMsg_Match(t *testing.T) {
	db := setupContentHookTestDB(t)

	// Write a minimal config with a PostAssistantMsg rule
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "tool-hooks.yaml")
	os.WriteFile(cfgPath, []byte(`
budget: 1500
rules:
  - id: test_vault_miss
    events: [PostAssistantMsg]
    match_prompt:
      - '没有.*[Vv]ault'
    priority: 60
    dedupe: per_session
    budget: 300
    inject: "go check ~/.cred/"
`), 0644)

	// Point config cache to our test file
	chConfigCache.mu.Lock()
	chConfigCache.path = cfgPath
	chConfigCache.config = nil
	chConfigCache.mu.Unlock()
	defer func() {
		chConfigCache.mu.Lock()
		chConfigCache.path = ""
		chConfigCache.config = nil
		chConfigCache.mu.Unlock()
	}()

	// Eval with matching text (没有 before Vault)
	evalPostAssistantMsg(db, "test-session-123", "这个 secret 没有存在 Vault 里")

	// Check pending was written
	pending := drainContentHookPending(db, "test-session-123")
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending injection, got %d", len(pending))
	}
	if pending[0] != "[rule:test_vault_miss] go check ~/.cred/" {
		t.Errorf("unexpected injection text: %q", pending[0])
	}

	// After drain, should be empty
	pending2 := drainContentHookPending(db, "test-session-123")
	if len(pending2) != 0 {
		t.Errorf("expected 0 pending after drain, got %d", len(pending2))
	}
}

func TestEvalPostAssistantMsg_NoMatch(t *testing.T) {
	db := setupContentHookTestDB(t)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "tool-hooks.yaml")
	os.WriteFile(cfgPath, []byte(`
budget: 1500
rules:
  - id: test_vault_miss
    events: [PostAssistantMsg]
    match_prompt:
      - '没有.*[Vv]ault'
    priority: 60
    dedupe: per_session
    budget: 300
    inject: "go check ~/.cred/"
`), 0644)

	chConfigCache.mu.Lock()
	chConfigCache.path = cfgPath
	chConfigCache.config = nil
	chConfigCache.mu.Unlock()
	defer func() {
		chConfigCache.mu.Lock()
		chConfigCache.path = ""
		chConfigCache.config = nil
		chConfigCache.mu.Unlock()
	}()

	// Eval with non-matching text
	evalPostAssistantMsg(db, "test-session-123", "Everything is fine with the deployment")

	pending := drainContentHookPending(db, "test-session-123")
	if len(pending) != 0 {
		t.Errorf("expected 0 pending (no match), got %d", len(pending))
	}
}

func TestEvalPostAssistantMsg_Dedupe(t *testing.T) {
	db := setupContentHookTestDB(t)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "tool-hooks.yaml")
	os.WriteFile(cfgPath, []byte(`
budget: 1500
rules:
  - id: test_vault_miss
    events: [PostAssistantMsg]
    match_prompt:
      - '没有.*[Vv]ault'
    priority: 60
    dedupe: per_session
    budget: 300
    inject: "go check ~/.cred/"
`), 0644)

	chConfigCache.mu.Lock()
	chConfigCache.path = cfgPath
	chConfigCache.config = nil
	chConfigCache.mu.Unlock()
	defer func() {
		chConfigCache.mu.Lock()
		chConfigCache.path = ""
		chConfigCache.config = nil
		chConfigCache.mu.Unlock()
	}()

	// First eval — should match (没有 before Vault)
	evalPostAssistantMsg(db, "test-session-456", "这个 token 没有在 Vault 中")
	pending := drainContentHookPending(db, "test-session-456")
	if len(pending) != 1 {
		t.Fatalf("first eval: expected 1, got %d", len(pending))
	}

	// Second eval same session — should be deduped
	evalPostAssistantMsg(db, "test-session-456", "另一个 key 也没有在 Vault 里")
	pending2 := drainContentHookPending(db, "test-session-456")
	if len(pending2) != 0 {
		t.Errorf("second eval (dedupe): expected 0, got %d", len(pending2))
	}
}

func TestEvalPostAssistantMsg_SkipInput(t *testing.T) {
	db := setupContentHookTestDB(t)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "tool-hooks.yaml")
	os.WriteFile(cfgPath, []byte(`
budget: 1500
rules:
  - id: test_vault_miss
    events: [PostAssistantMsg]
    match_prompt:
      - '没有.*[Vv]ault'
    skip_input:
      - '已经.*存入'
    priority: 60
    dedupe: per_session
    budget: 300
    inject: "go check ~/.cred/"
`), 0644)

	chConfigCache.mu.Lock()
	chConfigCache.path = cfgPath
	chConfigCache.config = nil
	chConfigCache.mu.Unlock()
	defer func() {
		chConfigCache.mu.Lock()
		chConfigCache.path = ""
		chConfigCache.config = nil
		chConfigCache.mu.Unlock()
	}()

	// Text matches the rule but also matches skip_input
	evalPostAssistantMsg(db, "test-session-789", "之前没有在 Vault 里，但已经存入了")
	pending := drainContentHookPending(db, "test-session-789")
	if len(pending) != 0 {
		t.Errorf("expected 0 (skip_input matched), got %d", len(pending))
	}
}

func TestExtractAssistantText(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "single text block",
			raw:  `{"type":"assistant","message":{"content":[{"type":"text","text":"hello world"}]}}`,
			want: "hello world",
		},
		{
			name: "multiple text blocks",
			raw:  `{"type":"assistant","message":{"content":[{"type":"text","text":"first "},{"type":"text","text":"second"}]}}`,
			want: "first second",
		},
		{
			name: "tool_use only",
			raw:  `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Read"}]}}`,
			want: "",
		},
		{
			name: "mixed content",
			raw:  `{"type":"assistant","message":{"content":[{"type":"text","text":"let me check"},{"type":"tool_use","id":"t1","name":"Read"}]}}`,
			want: "let me check",
		},
		{
			name: "empty content",
			raw:  `{"type":"assistant","message":{"content":[]}}`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractAssistantText([]byte(tt.raw))
			if got != tt.want {
				t.Errorf("extractAssistantText() = %q, want %q", got, tt.want)
			}
		})
	}
}
