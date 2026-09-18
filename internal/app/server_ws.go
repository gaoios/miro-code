package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var wsUpgrader = websocket.Upgrader{
	CheckOrigin:      func(r *http.Request) bool { return true },
	ReadBufferSize:   4096,
	WriteBufferSize:  16384,
	HandshakeTimeout: 10 * time.Second,
}

const (
	wsWriteWait  = 10 * time.Second
	wsPongWait   = 60 * time.Second
	wsPingPeriod = (wsPongWait * 9) / 10
	wsMaxMsgSize = 64 * 1024
)

// ── WebSocket Hub ──

// wsHub manages all WebSocket client connections and broadcasts events.
type wsHub struct {
	mu                      sync.RWMutex
	clients                 map[*wsClient]struct{}
	sm                      *sessionManager
	rl                      *rateLimiter
	token                   string
	defaultReplaceSoul      bool   // from serverConfig
	defaultInteractiveModel string // from serverConfig; fallback model for new sessions
}

func newWSHub(token string, rl *rateLimiter, defaultReplaceSoul bool, defaultInteractiveModel string) *wsHub {
	return &wsHub{
		clients:                 make(map[*wsClient]struct{}),
		rl:                      rl,
		token:                   token,
		defaultReplaceSoul:      defaultReplaceSoul,
		defaultInteractiveModel: defaultInteractiveModel,
	}
}

// wsClient is a single WebSocket connection.
type wsClient struct {
	hub    *wsHub
	conn   *websocket.Conn
	send   chan []byte
	subSID string // subscribed session ID for detailed events
	mu     sync.Mutex
}

func (h *wsHub) register(c *wsClient) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *wsHub) unregister(c *wsClient) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.send)
	}
	h.mu.Unlock()
}

func (h *wsHub) clientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// broadcastAll sends raw JSON to every connected client.
func (h *wsHub) broadcastAll(msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		select {
		case c.send <- msg:
		default:
			// slow consumer — will be cleaned up by writePump
		}
	}
}

// broadcastSessionEvent sends a session stream event to clients subscribed to that session.
func (h *wsHub) broadcastSessionEvent(sessionID, event string, data json.RawMessage) {
	msg, _ := json.Marshal(map[string]any{
		"type":       "event",
		"session_id": sessionID,
		"event":      event,
		"data":       data,
	})
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		c.mu.Lock()
		sub := c.subSID
		c.mu.Unlock()
		if sub == sessionID {
			select {
			case c.send <- msg:
			default:
			}
		}
	}
}

// notifySessions broadcasts updated session list to all connected clients.
func (h *wsHub) notifySessions() {
	if h.sm == nil {
		return
	}
	sessions := h.sm.listSessions()
	msg, _ := json.Marshal(map[string]any{
		"type":     "sessions",
		"sessions": sessions,
	})
	h.broadcastAll(msg)
}

// ── HTTP Handler ──

func (h *wsHub) serveWS(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token != h.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] ws: upgrade failed: %v\n", appName, err)
		return
	}

	client := &wsClient{
		hub:  h,
		conn: conn,
		send: make(chan []byte, 256),
	}

	h.register(client)
	n := h.clientCount()
	fmt.Fprintf(os.Stderr, "[%s] ws: client connected (%d total)\n", appName, n)

	// Send initial session list
	if h.sm != nil {
		sessions := h.sm.listSessions()
		initMsg, _ := json.Marshal(map[string]any{
			"type":     "sessions",
			"sessions": sessions,
		})
		select {
		case client.send <- initMsg:
		default:
		}
	}

	go client.writePump()
	go client.readPump()
}

// ── Read / Write Pumps ──

func (c *wsClient) readPump() {
	defer func() {
		c.hub.unregister(c)
		c.conn.Close()
		n := c.hub.clientCount()
		fmt.Fprintf(os.Stderr, "[%s] ws: client disconnected (%d remain)\n", appName, n)
	}()

	c.conn.SetReadLimit(wsMaxMsgSize)
	c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
		return nil
	})

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				fmt.Fprintf(os.Stderr, "[%s] ws: read error: %v\n", appName, err)
			}
			return
		}
		c.handleMessage(message)
	}
}

func (c *wsClient) writePump() {
	ticker := time.NewTicker(wsPingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// ── Message Handler ──

func (c *wsClient) handleMessage(raw []byte) {
	var msg struct {
		Type        string `json:"type"`
		SID         string `json:"sid"`
		Message     string `json:"message"`
		Name        string `json:"name"`
		Project     string `json:"project"`
		Model       string `json:"model"`
		SoulFiles   *bool  `json:"soul_files"`
		InitMsg     string `json:"initial_message"`
		SkipReplay  bool   `json:"skip_replay"`
		GalID       string `json:"gal_id"`
		ReplaceSoul *bool  `json:"replace_soul"` // 本我模式; nil → default true for create, inherit-from-DB for resume
		Mode        string `json:"mode"`         // "weiran"/"benwo"/"cc"; overrides SoulFiles+ReplaceSoul when set
		// Backend selects the harness driving this session: "cc" (Claude Code
		// stream-json), "codex" (OpenAI codex JSON-RPC), "grok" (Grok Build
		// headless), or "" / "auto" to let resolveBackendKind decide based on
		// model name. Mirrors the REST /api/sessions field of the same name.
		Backend string `json:"backend"`
		// RequestID is an opaque token the client mints per request and
		// expects echoed in the response. Used by `resume` to discriminate
		// stale responses (A→B→A re-resume races): the client tracks the
		// latest pending request_id and ignores any echo that doesn't match.
		// Optional; empty value disables the check.
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		c.sendJSON(map[string]string{"type": "error", "error": "invalid JSON"})
		return
	}

	switch msg.Type {
	case "ping":
		c.sendJSON(map[string]string{"type": "pong"})

	case "subscribe":
		c.mu.Lock()
		c.subSID = msg.SID
		c.mu.Unlock()
		// Replay history events for catch-up (skip if client already loaded via HTTP)
		if !msg.SkipReplay {
			if sess := c.hub.sm.getSession(msg.SID); sess != nil {
				sess.broadcaster.mu.RLock()
				for _, ev := range sess.broadcaster.history {
					evMsg, _ := json.Marshal(map[string]any{
						"type":       "event",
						"session_id": msg.SID,
						"event":      ev.Event,
						"data":       json.RawMessage(ev.Data),
					})
					select {
					case c.send <- evMsg:
					default:
					}
				}
				sess.broadcaster.mu.RUnlock()
			}
		}
		c.sendJSON(map[string]string{"type": "subscribed", "sid": msg.SID})

	case "unsubscribe":
		c.mu.Lock()
		c.subSID = ""
		c.mu.Unlock()

	case "send":
		if msg.SID == "" || msg.Message == "" {
			c.sendJSON(map[string]string{"type": "error", "error": "sid and message required"})
			return
		}
		if !c.hub.rl.allow() {
			c.sendJSON(map[string]string{"type": "error", "error": "rate limit exceeded"})
			return
		}
		sess := c.hub.sm.getSession(msg.SID)
		if sess == nil {
			c.sendJSON(map[string]string{"type": "error", "error": "session not found"})
			return
		}
		if !sess.process.alive() {
			c.sendJSON(map[string]string{"type": "error", "error": "session process has exited"})
			return
		}
		sess.touch()
		sess.setStatus("running")

		// Mirror the REST POST /api/sessions/{id}/message path (server.go ~L1043).
		// The previous WS handler skipped both of the steps below, which is why
		// messages sent from the Web UI during a long-running turn or while an
		// AskUserQuestion was pending appeared to be silently dropped:
		//
		//  1. dismissAllPendingAUQ: when claude is blocked on a control_request
		//     for AskUserQuestion, raw stdin writes don't unblock it — the
		//     subprocess only resumes when it gets the matching control_response.
		//     Sending behavior=deny tells claude to read the new user message
		//     instead of waiting on the question.
		//
		//  2. prepareSoulPatch: routes the turn through the lazy soul-patch
		//     injector so newly-relevant fragments are prepended before the user
		//     text. Without this, soul-mode turns coming through the WS path
		//     diverge from REST/Telegram behavior.
		//
		// We deliberately do NOT add the Telegram-style busy-wait queue here —
		// the Web UI is allowed to send mid-turn, and stream-json's stdin pipe
		// already serializes writes via claudeBackend.mu.
		sess.dismissAllPendingAUQ("user_message")
		injection := sess.prepareSoulPatch(msg.Message)

		// Capture first user message for hint display (sanitized display copy,
		// not the patched outbound — the hint is for humans).
		sess.mu.Lock()
		firstMsgCaptured := sess.FirstMsg == ""
		if firstMsgCaptured {
			sess.FirstMsg = injection.DisplayMessage
		}
		sess.mu.Unlock()
		if firstMsgCaptured && sess.hub != nil {
			go sess.hub.notifySessions()
		}

		// Broadcast user message so it persists in history for session switching.
		// Display version only — soul-patch payloads must never surface in UI.
		userEvent, _ := json.Marshal(map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": injection.DisplayMessage},
		})
		sess.broadcaster.broadcast(sseEvent{Event: "user", Data: userEvent})

		if err := sess.process.sendMessage(injection.Outbound); err != nil {
			c.sendJSON(map[string]string{"type": "error", "error": "send failed: " + err.Error()})
			return
		}

		// Backend accepted → commit new fragments. Same failure semantics as
		// the REST/Telegram paths: on send failure we deliberately don't commit
		// so the patch re-fires next attempt.
		sess.commitSoulPatchInjection(injection)

		c.sendJSON(map[string]string{"type": "sent", "sid": msg.SID})
		c.hub.notifySessions()

	case "create":
		if !c.hub.rl.allow() {
			c.sendJSON(map[string]string{"type": "error", "error": "rate limit exceeded"})
			return
		}
		name := msg.Name
		if name == "" {
			name = fmt.Sprintf("session-%s", time.Now().Format("0102-1504"))
		}
		project := msg.Project
		if project == "" {
			project = workspace
		}
		soul := true
		if msg.SoulFiles != nil {
			soul = *msg.SoulFiles
		}
		replaceSoul := c.hub.defaultReplaceSoul
		if msg.ReplaceSoul != nil {
			replaceSoul = *msg.ReplaceSoul
		}
		// Mode enum overrides the legacy bools when provided
		if msg.Mode != "" {
			if s, r, ok := modeToFlags(msg.Mode); ok {
				soul = s
				replaceSoul = r
			}
		}
		// Backend kind: same parsing rules as the REST POST /api/sessions
		// path (server.go ~L797). Empty / "auto" → resolveBackendKind picks
		// based on model. Explicit "cc"/"codex"/"grok" wins. Anything else
		// is a client bug — surface it instead of silently defaulting.
		var backendKind BackendKind
		switch strings.ToLower(strings.TrimSpace(msg.Backend)) {
		case "", "auto":
			backendKind = ""
		case string(BackendCC), "claude", "claude-code":
			backendKind = BackendCC
		case string(BackendCodex), "openai-codex":
			if !codexEnabled {
				c.sendJSON(map[string]string{"type": "error", "error": "backend=codex requested but agents.codex.enabled=false in config.json"})
				return
			}
			backendKind = BackendCodex
		case string(BackendGrok), "grok-build":
			if !grokEnabled {
				c.sendJSON(map[string]string{"type": "error", "error": "backend=grok requested but agents.grok.enabled=false in config.json"})
				return
			}
			backendKind = BackendGrok
		case string(BackendKimi), "kimi-cli", "moonshot-kimi":
			if !kimiEnabled {
				c.sendJSON(map[string]string{"type": "error", "error": "backend=kimi requested but agents.kimi.enabled=false in config.json"})
				return
			}
			backendKind = BackendKimi
		case string(BackendAgy), "antigravity", "antigravity-cli":
			if !agyEnabled {
				c.sendJSON(map[string]string{"type": "error", "error": "backend=agy requested but agents.agy.enabled=false in config.json"})
				return
			}
			backendKind = BackendAgy
		case string(BackendOpencode), "opencode-cli":
			if !opencodeEnabled {
				c.sendJSON(map[string]string{"type": "error", "error": "backend=opencode requested but agents.opencode.enabled=false in config.json"})
				return
			}
			backendKind = BackendOpencode
		default:
			c.sendJSON(map[string]string{"type": "error", "error": fmt.Sprintf("unknown backend %q (expected cc|codex|grok|kimi|agy|opencode|auto)", msg.Backend)})
			return
		}
		model := resolveCreateModel(msg.Model, CategoryInteractive, backendKind, c.hub.defaultInteractiveModel)
		sess, err := c.hub.sm.createSessionWithOpts(sessionCreateOpts{
			Name:        name,
			Project:     project,
			Model:       model,
			Soul:        soul,
			GalID:       msg.GalID,
			Category:    CategoryInteractive,
			ReplaceSoul: replaceSoul,
			Backend:     backendKind,
		})
		if err != nil {
			c.sendJSON(map[string]string{"type": "error", "error": err.Error()})
			return
		}
		if msg.InitMsg != "" {
			// Sanitize literal soul-patch markers up front so FirstMsg (visible
			// in the create snapshot below) is guaranteed clean; prepareSoulPatch
			// in the goroutine will sanitize again as a fast no-op for the
			// common case where the input has no markers. Mirrors server.go
			// POST /api/sessions create path (~L858).
			cleanInitial, _ := sanitizeSoulPatchInput(msg.InitMsg)
			sess.mu.Lock()
			if sess.FirstMsg == "" {
				sess.FirstMsg = cleanInitial
			}
			sess.mu.Unlock()
			go func() {
				if !sess.awaitBackendReady(30 * time.Second) {
					return
				}
				// Route through prepareSoulPatch for consistency with REST.
				// The create-time prompt build already has fragments for the
				// detected mode loaded, so the per-message diff is normally
				// empty — but if the user's first message routes to a
				// different mode, missing fragments are patched in here.
				injection := sess.prepareSoulPatch(cleanInitial)
				userEvent, _ := json.Marshal(map[string]any{
					"type":    "user",
					"message": map[string]any{"role": "user", "content": injection.DisplayMessage},
				})
				sess.broadcaster.broadcast(sseEvent{Event: "user", Data: userEvent})
				if err := sess.process.sendMessage(injection.Outbound); err != nil {
					fmt.Fprintf(os.Stderr, "[%s] ws: create: failed to send initial message: %v\n", appName, err)
					return
				}
				sess.commitSoulPatchInjection(injection)
			}()
		}
		// Auto-subscribe client to the new session
		c.mu.Lock()
		c.subSID = sess.ID
		c.mu.Unlock()
		c.sendJSON(map[string]any{"type": "created", "session": sess.snapshot()})
		c.hub.notifySessions()

	case "rename":
		if msg.SID == "" || msg.Name == "" {
			c.sendJSON(map[string]string{"type": "error", "error": "sid and name required"})
			return
		}
		sess := c.hub.sm.getSession(msg.SID)
		if sess == nil {
			c.sendJSON(map[string]string{"type": "error", "error": "session not found"})
			return
		}
		sess.mu.Lock()
		sess.Name = msg.Name
		sess.mu.Unlock()
		markRenamed(sess.ID, msg.Name)
		c.sendJSON(map[string]string{"type": "renamed", "sid": msg.SID, "name": msg.Name})
		c.hub.notifySessions()

	case "destroy":
		if msg.SID == "" {
			c.sendJSON(map[string]string{"type": "error", "error": "sid required"})
			return
		}
		if err := c.hub.sm.destroySession(msg.SID); err != nil {
			c.sendJSON(map[string]string{"type": "error", "error": err.Error()})
			return
		}
		c.sendJSON(map[string]string{"type": "destroyed", "session_id": msg.SID})
		c.hub.notifySessions()

	case "resume":
		if msg.SID == "" {
			c.sendJSON(map[string]string{"type": "error", "error": "sid required"})
			return
		}
		if !c.hub.rl.allow() {
			c.sendJSON(map[string]string{"type": "error", "error": "rate limit exceeded"})
			return
		}
		// Guard against concurrent resume for the same session — a second resume
		// while the first is still spawning CC would create an orphan process.
		if _, loaded := c.hub.sm.resuming.LoadOrStore(msg.SID, true); loaded {
			c.sendJSON(map[string]any{"type": "resume_failed", "session_id": msg.SID, "request_id": msg.RequestID, "error": "resume already in progress"})
			return
		}
		// Acknowledge synchronously so the WS read loop is never blocked by
		// resumeSession (which can take up to 30s on init timeout). The actual
		// resume runs in a goroutine and posts back resumed/resume_failed.
		c.sendJSON(map[string]any{"type": "resuming", "session_id": msg.SID, "request_id": msg.RequestID})
		// Snapshot fields needed inside the goroutine (msg is reused by caller).
		sid := msg.SID
		message := msg.Message
		name := msg.Name
		replaceSoul := msg.ReplaceSoul
		soulFiles := msg.SoulFiles
		reqID := msg.RequestID
		go func() {
			defer c.hub.sm.resuming.Delete(sid)
			// Recover from any panic inside resumeSession / spawnClaude so a
			// crash on one resume request doesn't take the whole server down.
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintf(os.Stderr, "[%s] ws resume goroutine panic sid=%s: %v\n", appName, shortID(sid), r)
					c.sendJSON(map[string]any{
						"type":        "resume_failed",
						"session_id":  sid,
						"request_sid": sid,
						"request_id":  reqID,
						"error":       fmt.Sprintf("server panic during resume: %v", r),
					})
				}
			}()
			// Don't pass frontend model on resume — it may be stale/stripped (missing [1m] suffix).
			// Let resumeSession resolve model from DB which preserves the correct value.
			sess, err := c.hub.sm.resumeSession(sid, message, name, "", "", replaceSoul, soulFiles)
			if err != nil {
				c.sendJSON(map[string]any{
					"type":        "resume_failed",
					"session_id":  sid,
					"request_sid": sid,
					"request_id":  reqID,
					"error":       err.Error(),
				})
				return
			}
			// Auto-subscribe client to the new session before sending resumed
			c.mu.Lock()
			c.subSID = sess.ID
			c.mu.Unlock()
			// request_id echoes the client-minted opaque token so the
			// frontend can ignore stale resumed events even in A→B→A
			// re-resume races where request_sid alone isn't unique.
			c.sendJSON(map[string]any{
				"type":        "resumed",
				"session":     sess.snapshot(),
				"request_sid": sid,
				"request_id":  reqID,
			})
			c.hub.notifySessions()
		}()

	default:
		c.sendJSON(map[string]string{"type": "error", "error": "unknown type: " + msg.Type})
	}
}

func (c *wsClient) sendJSON(v any) {
	// Recover from "send on closed channel" — happens when the client
	// disconnected while a long-running goroutine (e.g. resume) is still
	// trying to deliver a response. Drop silently rather than crash the server.
	defer func() {
		if r := recover(); r != nil {
			// no-op: client gone
		}
	}()
	data, _ := json.Marshal(v)
	select {
	case c.send <- data:
	default:
	}
}
