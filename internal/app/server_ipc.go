package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"context"
)

// ── IPC: Inter-Session Communication ──

// Default max interaction rounds per session pair (bidirectional).
const defaultMaxInteractionRounds = 10

// recordInteraction inserts an interaction record for anti-loop tracking.
func recordInteraction(fromID, toID, snippet string) error {
	db, err := openServerDB()
	if err != nil {
		return err
	}
	serverDBMu.Lock()
	defer serverDBMu.Unlock()
	_, err = db.Exec(`INSERT INTO session_interactions (from_id, to_id, snippet, created_at) VALUES (?, ?, ?, ?)`,
		fromID, toID, snippet, time.Now().UTC().Format(time.RFC3339))
	return err
}

// countInteractionRounds returns the total bidirectional interaction count
// between two sessions (A→B and B→A both counted).
func countInteractionRounds(idA, idB string) int {
	db, err := openServerDB()
	if err != nil {
		return 0
	}
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM session_interactions WHERE (from_id=? AND to_id=?) OR (from_id=? AND to_id=?)`,
		idA, idB, idB, idA).Scan(&count)
	return count
}

// checkInteractionLimit returns true if the pair has exceeded the max rounds.
func checkInteractionLimit(fromID, toID string, maxRounds int) bool {
	return countInteractionRounds(fromID, toID) >= maxRounds
}

// updateParticipants adds a sender session ID to the target session's participants list.
func updateParticipants(targetSessionID, senderSessionID string) {
	db, err := openServerDB()
	if err != nil {
		return
	}
	serverDBMu.Lock()
	defer serverDBMu.Unlock()

	var raw string
	err = db.QueryRow(`SELECT participants FROM server_sessions WHERE session_id = ?`, targetSessionID).Scan(&raw)
	if err != nil {
		return
	}

	var participants []string
	json.Unmarshal([]byte(raw), &participants)

	// Dedup: don't add if already present
	for _, p := range participants {
		if p == senderSessionID {
			return
		}
	}
	participants = append(participants, senderSessionID)
	data, _ := json.Marshal(participants)
	db.Exec(`UPDATE server_sessions SET participants = ? WHERE session_id = ?`, string(data), targetSessionID)
}

// getParticipants returns the list of session IDs that have sent IPC messages to this session.
func getParticipants(sessionID string) []string {
	db, err := openServerDB()
	if err != nil {
		return nil
	}
	var raw string
	err = db.QueryRow(`SELECT participants FROM server_sessions WHERE session_id = ?`, sessionID).Scan(&raw)
	if err != nil {
		return nil
	}
	var participants []string
	json.Unmarshal([]byte(raw), &participants)
	return participants
}

// updateSpawnedBy persists the parent session ID for a child session.
func updateSpawnedBy(childSessionID, parentSessionID string) {
	db, err := openServerDB()
	if err != nil {
		return
	}
	serverDBMu.Lock()
	defer serverDBMu.Unlock()
	result, err := db.Exec(`UPDATE server_sessions SET spawned_by = ? WHERE session_id = ?`, parentSessionID, childSessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] ipc: updateSpawnedBy failed: %v\n", appName, err)
		return
	}
	if n, _ := result.RowsAffected(); n == 0 {
		fmt.Fprintf(os.Stderr, "[%s] ipc: updateSpawnedBy: no row matched session_id=%s\n", appName, shortID(childSessionID))
	}
}

// ── HTTP Handlers ──

// handleIPCMessageFrom handles POST /api/sessions/{id}/message-from
// Sends a message from one session to another, with anti-loop enforcement.
func handleIPCMessageFrom(sm *sessionManager, hub *wsHub, maxRounds int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		targetID := r.PathValue("id")

		var req struct {
			FromSessionID string `json:"from_session_id"`
			Message       string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.FromSessionID == "" || req.Message == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "from_session_id and message are required"})
			return
		}

		// Validate: target session exists and is alive
		targetSess := sm.getSession(targetID)
		if targetSess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "target session not found"})
			return
		}
		if !targetSess.process.alive() {
			writeJSON(w, http.StatusGone, map[string]string{"error": "target session process has exited"})
			return
		}

		// Validate: source session exists
		fromSess := sm.getSession(req.FromSessionID)
		if fromSess == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source session not found"})
			return
		}

		// Anti-loop check
		round := countInteractionRounds(req.FromSessionID, targetID)
		if round >= maxRounds {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"error":      "interaction limit reached",
				"round":      round,
				"max_rounds": maxRounds,
			})
			return
		}

		// Record interaction
		snippet := req.Message
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		if err := recordInteraction(req.FromSessionID, targetID, snippet); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] ipc: failed to record interaction: %v\n", appName, err)
		}
		round++ // reflect the just-recorded one

		// Update participants on target
		updateParticipants(targetID, req.FromSessionID)

		// Note: spawned_by is set explicitly via opts.SpawnedBy at session creation,
		// not inferred from IPC messages (first sender ≠ parent).

		// Build prefixed message
		fromName := fromSess.Name
		if fromName == "" {
			fromName = shortID(req.FromSessionID)
		}
		prefixedMsg := fmt.Sprintf("[From session %s (%s)] %s", shortID(req.FromSessionID), fromName, req.Message)

		// Send to target session's stdin
		targetSess.touch()
		targetSess.setStatus("running")

		// Phase D — Runtime Soul Patch Protocol: peer-session IPC messages
		// route through the same lazy injector as Web UI / TG. The sanitizer
		// strips any patch markers a misbehaving peer might smuggle, and the
		// mode router uses the prefixed message body to decide what fragments
		// the target persona needs for this incoming turn.
		injection := targetSess.prepareSoulPatch(prefixedMsg)

		// Broadcast user event for UI — display the sanitized prefixed
		// message, never the patched outbound.
		userEvent, _ := json.Marshal(map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": injection.DisplayMessage},
			"ipc":     true,
			"from":    req.FromSessionID,
		})
		targetSess.broadcaster.broadcast(sseEvent{Event: "user", Data: userEvent})

		if err := targetSess.process.sendMessage(injection.Outbound); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		targetSess.commitSoulPatchInjection(injection)

		// Broadcast IPC event to WebSocket
		if hub != nil {
			ipcEvent, _ := json.Marshal(map[string]any{
				"type":      "ipc_message",
				"sid":       targetID,
				"from":      req.FromSessionID,
				"from_name": fromName,
				"round":     round,
			})
			hub.broadcastAll(ipcEvent)
		}

		fmt.Fprintf(os.Stderr, "[%s] ipc: %s → %s (round %d/%d)\n",
			appName, shortID(req.FromSessionID), shortID(targetID), round, maxRounds)

		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"round":      round,
			"max_rounds": maxRounds,
		})
	}
}

// handleIPCInteractionCount handles GET /api/sessions/{id}/interaction-count?peer={other_id}
func handleIPCInteractionCount() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.PathValue("id")
		peerID := r.URL.Query().Get("peer")
		if peerID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "peer query param required"})
			return
		}
		count := countInteractionRounds(sessionID, peerID)
		writeJSON(w, http.StatusOK, map[string]any{
			"session_id": sessionID,
			"peer_id":    peerID,
			"rounds":     count,
		})
	}
}

// handleSessionWait handles GET /api/sessions/{id}/wait
// Blocks until the session reaches idle/stopped/error, or times out (10 min).
func handleSessionWait(sm *sessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.PathValue("id")
		sess := sm.getSession(sessionID)
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}

		// Check if already in a terminal state
		sess.mu.Lock()
		status := sess.Status
		numTurns := sess.NumTurns
		if status == "idle" || status == "stopped" || status == "error" {
			sess.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{
				"status":    status,
				"num_turns": numTurns,
				"timeout":   false,
			})
			return
		}

		// Register waiter channel
		ch := make(chan string, 1)
		sess.waiters = append(sess.waiters, ch)
		sess.mu.Unlock()

		// Wait with timeout (10 minutes), respecting client disconnect
		timeout := 10 * time.Minute
		if t := r.URL.Query().Get("timeout"); t != "" {
			if d, err := time.ParseDuration(t); err == nil && d > 0 && d <= 30*time.Minute {
				timeout = d
			}
		}

		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		select {
		case finalStatus := <-ch:
			sess.mu.Lock()
			turns := sess.NumTurns
			sess.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{
				"status":    finalStatus,
				"num_turns": turns,
				"timeout":   false,
			})
		case <-ctx.Done():
			// Timeout or client disconnect — remove our waiter
			sess.mu.Lock()
			for i, c := range sess.waiters {
				if c == ch {
					sess.waiters = append(sess.waiters[:i], sess.waiters[i+1:]...)
					break
				}
			}
			sess.mu.Unlock()
			if r.Context().Err() != nil {
				return
			}
			sess.mu.Lock()
			currentStatus := sess.Status
			turns := sess.NumTurns
			sess.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{
				"status":    currentStatus,
				"num_turns": turns,
				"timeout":   true,
			})
		}
	}
}
