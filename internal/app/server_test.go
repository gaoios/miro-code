package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestAuthMiddleware_NoToken(t *testing.T) {
	handler := authMiddleware("secret", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestLoadServerConfigReadsBooleanOnlyServerConfig(t *testing.T) {
	origAppDir := appDir
	appDir = t.TempDir()
	t.Cleanup(func() { appDir = origAppDir })

	cfgPath := filepath.Join(appDir, "config.json")
	err := os.WriteFile(cfgPath, []byte(`{
		"server": {
			"defaultReplaceSoul": false,
			"disableTmuxDrawer": true,
			"dbExplorer": {"enabled": false}
		}
	}`), 0o600)
	if err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg := loadServerConfig()
	if cfg.DefaultReplaceSoul {
		t.Fatalf("defaultReplaceSoul=true, want false")
	}
	if !cfg.DisableTmuxDrawer {
		t.Fatalf("disableTmuxDrawer=false, want true")
	}
	if dbExplorerEnabled(cfg.DBExplorer) {
		t.Fatalf("dbExplorer enabled, want disabled")
	}
}

func TestIsActiveUploadExt(t *testing.T) {
	for _, ext := range []string{".html", ".HTM", ".js", ".svg", ".xml"} {
		if !isActiveUploadExt(ext) {
			t.Fatalf("isActiveUploadExt(%q)=false, want true", ext)
		}
	}
	for _, ext := range []string{".png", ".jpg", ".pdf", ".txt", ""} {
		if isActiveUploadExt(ext) {
			t.Fatalf("isActiveUploadExt(%q)=true, want false", ext)
		}
	}
}

func TestAuthMiddleware_InvalidToken(t *testing.T) {
	handler := authMiddleware("secret", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestAuthMiddleware_ValidToken(t *testing.T) {
	handler := authMiddleware("secret", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestAuthMiddleware_QueryToken(t *testing.T) {
	handler := authMiddleware("secret", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/test?token=secret", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with query token, got %d", w.Code)
	}
}

func TestRateLimiter(t *testing.T) {
	rl := newRateLimiter(3)

	for i := 0; i < 3; i++ {
		if !rl.allow() {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}

	if rl.allow() {
		t.Fatal("4th request should be rate limited")
	}
}

func TestWriteJSON(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusOK, map[string]string{"hello": "world"})

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json, got %s", ct)
	}

	var result map[string]string
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result["hello"] != "world" {
		t.Errorf("expected world, got %s", result["hello"])
	}
}

func TestDefaultServerConfig(t *testing.T) {
	cfg := defaultServerConfig()
	if cfg.Host != "127.0.0.1" {
		t.Errorf("expected 127.0.0.1, got %s", cfg.Host)
	}
	if cfg.Port != 9847 {
		t.Errorf("expected 9847, got %d", cfg.Port)
	}
	if cfg.MaxSessions != 5 {
		t.Errorf("expected 5, got %d", cfg.MaxSessions)
	}
}

// ── Broadcaster tests ──

func TestBroadcaster_SubscribeUnsubscribe(t *testing.T) {
	b := newBroadcaster()

	sub := b.subscribe()
	if b.count() != 1 {
		t.Errorf("expected 1 subscriber, got %d", b.count())
	}

	b.unsubscribe(sub)
	if b.count() != 0 {
		t.Errorf("expected 0 subscribers, got %d", b.count())
	}
}

func TestBroadcaster_Broadcast(t *testing.T) {
	b := newBroadcaster()
	sub := b.subscribe()
	defer b.unsubscribe(sub)

	ev := sseEvent{Event: "test", Data: []byte(`{"msg":"hello"}`)}
	b.broadcast(ev)

	select {
	case got := <-sub.ch:
		if got.Event != "test" {
			t.Errorf("expected event 'test', got '%s'", got.Event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for broadcast")
	}
}

func TestBroadcaster_SlowConsumerDropped(t *testing.T) {
	b := newBroadcaster()
	sub := b.subscribe()

	// Fill the buffer
	for i := 0; i < subscriberBufSize+10; i++ {
		b.broadcast(sseEvent{Event: "flood", Data: []byte(fmt.Sprintf(`{"i":%d}`, i))})
	}

	// Slow consumer should be dropped
	select {
	case <-sub.closed:
		// expected
	case <-time.After(time.Second):
		t.Fatal("slow consumer was not dropped")
	}

	if b.count() != 0 {
		t.Errorf("expected 0 subscribers after drop, got %d", b.count())
	}
}

func TestBroadcaster_HistoryCatchup(t *testing.T) {
	b := newBroadcaster()

	// Broadcast before subscribing
	b.broadcast(sseEvent{Event: "early", Data: []byte(`{"msg":"early"}`)})

	sub := b.subscribe()
	defer b.unsubscribe(sub)

	select {
	case got := <-sub.ch:
		if got.Event != "early" {
			t.Errorf("expected 'early' event from history, got '%s'", got.Event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for history catchup")
	}
}

// ── MapEventType tests ──

func TestMapEventType(t *testing.T) {
	tests := []struct {
		msgType  string
		raw      string
		expected string
	}{
		{"system", `{"type":"system","subtype":"init"}`, "init"},
		{"system", `{"type":"system","subtype":"task_started"}`, "system"},
		{"assistant", `{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`, "assistant"},
		{"assistant", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}]}}`, "tool_use"},
		{"user", `{"type":"user"}`, "tool_result"},
		{"result", `{"type":"result","subtype":"success"}`, "result"},
		{"unknown", `{"type":"unknown"}`, "unknown"},
	}

	for _, tt := range tests {
		got := mapEventType(tt.msgType, json.RawMessage(tt.raw))
		if got != tt.expected {
			t.Errorf("mapEventType(%q) = %q, want %q", tt.msgType, got, tt.expected)
		}
	}
}

// ── Session Manager tests (without Claude process) ──

func TestSessionManager_ReaperStops(t *testing.T) {
	sm := newSessionManager(5, time.Hour, time.Hour)
	sm.shutdownAll() // should not panic
}

// ── FilterEnv tests ──

func TestFilterEnv(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"CLAUDECODE=something",
		"CLAUDECODE_STUFF=other",
		"HOME=/root",
	}
	got := filterEnv(env, "CLAUDECODE")
	if len(got) != 2 {
		t.Errorf("expected 2 entries, got %d: %v", len(got), got)
	}
}

// ── RandHex tests ──

func TestRandHex(t *testing.T) {
	h := randHex(8)
	if len(h) != 8 {
		t.Errorf("expected 8 chars, got %d", len(h))
	}
	for _, c := range h {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Errorf("unexpected char: %c", c)
		}
	}
}

// ── Health endpoint integration test ──

func TestHealthEndpoint(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "ok",
			"app":     appName,
			"version": buildVersion,
		})
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	if result["status"] != "ok" {
		t.Errorf("expected ok, got %v", result["status"])
	}
}

func TestSpawnAndWakeRequireAuthAndRateLimit(t *testing.T) {
	token := "test-secret"
	rl := newRateLimiter(1)

	wakeHandler := authMiddleware(token, func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow() {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
	})

	spawnHandler := authMiddleware(token, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
	})

	// 1. POST /api/spawn without token -> 401
	req := httptest.NewRequest("POST", "/api/spawn", nil)
	w := httptest.NewRecorder()
	spawnHandler(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("spawn without token: expected 401, got %d", w.Code)
	}

	// 2. POST /api/spawn with token -> 200
	req = httptest.NewRequest("POST", "/api/spawn", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	spawnHandler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("spawn with token: expected 200, got %d", w.Code)
	}

	// 3. POST /api/wake without token -> 401
	req = httptest.NewRequest("POST", "/api/wake", nil)
	w = httptest.NewRecorder()
	wakeHandler(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wake without token: expected 401, got %d", w.Code)
	}

	// 4. POST /api/wake with token -> 200 on 1st request
	req = httptest.NewRequest("POST", "/api/wake", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	wakeHandler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("wake with token (1st): expected 200, got %d", w.Code)
	}

	// 5. POST /api/wake with token -> 429 on 2nd request (rate limit)
	req = httptest.NewRequest("POST", "/api/wake", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	wakeHandler(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("wake rate limit: expected 429, got %d", w.Code)
	}
}

func TestUploadsPathTraversal(t *testing.T) {
	tempDir := t.TempDir()
	uploadsDir := filepath.Join(tempDir, "uploads")
	if err := os.MkdirAll(uploadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	testFile := filepath.Join(uploadsDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("hello upload"), 0644); err != nil {
		t.Fatal(err)
	}

	handler := func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/uploads/")
		if name == "" || filepath.IsAbs(name) || strings.Contains(name, "..") {
			http.NotFound(w, r)
			return
		}
		cleanUploadsDir := filepath.Clean(uploadsDir)
		resolved := filepath.Clean(filepath.Join(cleanUploadsDir, filepath.Clean(name)))
		if !strings.HasPrefix(resolved, cleanUploadsDir+string(filepath.Separator)) {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, resolved)
	}

	// Valid request
	req := httptest.NewRequest("GET", "/uploads/test.txt", nil)
	w := httptest.NewRecorder()
	handler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("valid upload request: expected 200, got %d", w.Code)
	}

	// Traversal with ..
	req = httptest.NewRequest("GET", "/uploads/../secret.txt", nil)
	w = httptest.NewRecorder()
	handler(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("traversal with ..: expected 404, got %d", w.Code)
	}

	// Traversal with absolute path
	req = httptest.NewRequest("GET", "/uploads//etc/passwd", nil)
	w = httptest.NewRecorder()
	handler(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("traversal with abs path: expected 404, got %d", w.Code)
	}
}

func TestFilesReadEndpointSecurity(t *testing.T) {
	token := "test-secret"
	handler := authMiddleware(token, func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.Query().Get("path")
		if raw == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is required"})
			return
		}
		clean := filepath.Clean(raw)
		allowed := false
		for _, prefix := range []string{"/tmp/claude-", "/private/tmp/claude-"} {
			if strings.HasPrefix(clean, prefix) {
				allowed = true
				break
			}
		}
		if !allowed {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "path not in whitelist"})
			return
		}
		resolved, err := filepath.EvalSymlinks(clean)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		resolvedAllowed := false
		for _, prefix := range []string{"/tmp/claude-", "/private/tmp/claude-"} {
			if strings.HasPrefix(resolved, prefix) {
				resolvedAllowed = true
				break
			}
		}
		if !resolvedAllowed {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "resolved path not in whitelist"})
			return
		}
		f, err := os.OpenFile(clean, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			if os.IsNotExist(err) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			} else {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid file or symlink: " + err.Error()})
			}
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if fi.IsDir() || !fi.Mode().IsRegular() {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is not a regular file"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	// 1. /var/folders/ is rejected
	req := httptest.NewRequest("GET", "/api/files/read?path=/var/folders/xx/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("/var/folders path: expected 403, got %d", w.Code)
	}

	// 2. /etc/passwd is rejected
	req = httptest.NewRequest("GET", "/api/files/read?path=/etc/passwd", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	handler(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("/etc/passwd path: expected 403, got %d", w.Code)
	}

	// 3. Valid file under /tmp/claude-test-xxx
	tmpFile, err := os.CreateTemp("/tmp", "claude-test-*.txt")
	if err != nil {
		t.Skipf("cannot create temp file in /tmp: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.WriteString("test content")
	tmpFile.Close()

	req = httptest.NewRequest("GET", "/api/files/read?path="+tmpFile.Name(), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	handler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("valid /tmp/claude file: expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	// 4. Symlink under /tmp/claude-* pointing to /etc/passwd is rejected
	symlinkPath := filepath.Join("/tmp", fmt.Sprintf("claude-link-%d", time.Now().UnixNano()))
	if err := os.Symlink("/etc/passwd", symlinkPath); err == nil {
		defer os.Remove(symlinkPath)
		req = httptest.NewRequest("GET", "/api/files/read?path="+symlinkPath, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w = httptest.NewRecorder()
		handler(w, req)
		if w.Code != http.StatusForbidden && w.Code != http.StatusBadRequest {
			t.Fatalf("symlink escape to /etc/passwd: expected 403 or 400, got %d", w.Code)
		}
	}
}
