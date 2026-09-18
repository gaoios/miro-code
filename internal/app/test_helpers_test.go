package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	// Prevent tests from sending real Telegram messages
	disableTelegram = true
	os.Exit(m.Run())
}

// requireWorkspace skips the test if the workspace doesn't contain the
// soul files needed by buildPrompt / buildSkillIndex integration tests.
func requireWorkspace(t *testing.T) {
	t.Helper()
	for _, f := range []string{"SOUL.md", "IDENTITY.md"} {
		if _, err := os.Stat(filepath.Join(workspace, f)); err != nil {
			t.Skipf("workspace %s missing %s — skip integration test", workspace, f)
		}
	}
}
