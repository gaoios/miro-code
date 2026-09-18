package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// handleNew resets all Telegram direct sessions (chat + system channels)
// fixes the issue where OpenClaw /new only clears system channel but not chat channel
func handleNew() {
	sessionsFile := filepath.Join(appDir, "telegram", "sessions.json")
	sessDir := filepath.Join(appDir, "telegram")

	data, err := os.ReadFile(sessionsFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] failed to read sessions.json: %v\n", err)
		os.Exit(1)
	}

	// parse as map[string]json.RawMessage to preserve original structure
	var sessions map[string]json.RawMessage
	if err := json.Unmarshal(data, &sessions); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] failed to parse sessions.json: %v\n", err)
		os.Exit(1)
	}

	// find all telegram direct sessions (exclude slash/group)
	var resetKeys []string
	for k := range sessions {
		if strings.Contains(k, "telegram") && strings.Contains(k, "direct") && !strings.Contains(k, "slash") {
			resetKeys = append(resetKeys, k)
		}
	}

	if len(resetKeys) == 0 {
		fmt.Println("[" + appName + "] no Telegram direct sessions found")
		return
	}

	// reset each one
	var resetSummary []string
	for _, key := range resetKeys {
		// parse current entry
		var entry map[string]interface{}
		if err := json.Unmarshal(sessions[key], &entry); err != nil {
			fmt.Fprintf(os.Stderr, "["+appName+"] failed to parse session entry %s: %v\n", key, err)
			continue
		}

		oldID, _ := entry["sessionId"].(string)

		// generate new session
		newID := uuid.New().String()
		newJSONL := filepath.Join(sessDir, newID+".jsonl")

		// create empty JSONL file
		if err := os.WriteFile(newJSONL, []byte{}, 0600); err != nil {
			fmt.Fprintf(os.Stderr, "["+appName+"] failed to create %s: %v\n", newJSONL, err)
			continue
		}

		// update entry
		entry["sessionId"] = newID
		entry["updatedAt"] = time.Now().UnixMilli()
		entry["systemSent"] = false
		entry["compactionCount"] = 0
		entry["sessionFile"] = newJSONL

		// delete skillsSnapshot to save space (gateway will regenerate)
		delete(entry, "skillsSnapshot")

		updated, err := json.Marshal(entry)
		if err != nil {
			fmt.Fprintf(os.Stderr, "["+appName+"] failed to serialize session entry: %v\n", err)
			continue
		}
		sessions[key] = json.RawMessage(updated)

		shortOld := oldID
		if len(shortOld) > 8 {
			shortOld = shortOld[:8]
		}
		resetSummary = append(resetSummary, fmt.Sprintf("  %s: %s → %s", key, shortOld, shortID(newID)))
		fmt.Printf("["+appName+"] reset %s: %s → %s\n", key, shortOld, shortID(newID))
	}

	// write back sessions.json
	out, err := json.MarshalIndent(sessions, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] failed to serialize sessions.json: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(sessionsFile, out, 0600); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] failed to write sessions.json: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("["+appName+"] ✅ reset %d Telegram direct sessions\n", len(resetSummary))

	// notify Telegram
	msg := fmt.Sprintf("🔄 Session reset (%d channels)\n%s", len(resetSummary), strings.Join(resetSummary, "\n"))
	sendTelegram(msg)
}
