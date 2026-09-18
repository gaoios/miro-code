package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// setupLifecycleTest creates a temp DB with an isolated workspace for
// session lifecycle tests. Returns a cleanup function.
func setupLifecycleTest(t *testing.T) func() {
	t.Helper()
	origDB := dbPath
	origWS := workspace
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	workspace = dir
	return func() {
		dbPath = origDB
		workspace = origWS
		// Reset watcher singleton for next test
		watcherMu.Lock()
		watcherRunning = false
		watcherMu.Unlock()
	}
}

// insertTestSoulSession inserts a soul session with an explicit updated_at.
// Used to simulate stale sessions without waiting real wall-clock time.
func insertTestSoulSession(t *testing.T, agent, status string, updatedAt time.Time) int64 {
	t.Helper()
	db, err := openDB()
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()
	ts := updatedAt.Format(time.RFC3339)
	res, err := db.Exec(`INSERT INTO soul_sessions(agent_id, type, status, started_at, updated_at) VALUES(?,?,?,?,?)`,
		agent, "daily", status, ts, ts)
	if err != nil {
		t.Fatalf("insert soul_session: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func countActiveSoulSessions(t *testing.T, agent string) int {
	t.Helper()
	db, _ := openDB()
	defer db.Close()
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM soul_sessions WHERE agent_id=? AND status IN ('running','suspended')`, agent).Scan(&n)
	return n
}

func TestDefaultResetPolicy(t *testing.T) {
	p := defaultResetPolicy()
	if p.Mode != "both" {
		t.Errorf("Mode = %q, want both", p.Mode)
	}
	if p.IdleMinutes != 1440 {
		t.Errorf("IdleMinutes = %d, want 1440", p.IdleMinutes)
	}
	if p.DailyAtHour != 4 {
		t.Errorf("DailyAtHour = %d, want 4", p.DailyAtHour)
	}
	if !p.NotifyOnReset {
		t.Errorf("NotifyOnReset should default true")
	}
}

func TestLoadResetPolicyFromConfig_Empty(t *testing.T) {
	// Empty config → pure defaults
	cfg := serverConfig{}
	p := loadResetPolicyFromConfig(cfg)
	if p.Mode != "both" || p.IdleMinutes != 1440 || p.DailyAtHour != 4 {
		t.Errorf("empty cfg → %+v, want defaults", p)
	}
	if !p.NotifyOnReset {
		t.Errorf("empty cfg → NotifyOnReset=false, want true")
	}
}

func TestLoadResetPolicyFromConfig_Overrides(t *testing.T) {
	cfg := serverConfig{
		SessionReset: sessionResetConfig{
			Mode:          "idle",
			IdleMinutes:   60,
			DailyAtHour:   3,
			NotifyOnReset: false,
		},
	}
	p := loadResetPolicyFromConfig(cfg)
	if p.Mode != "idle" {
		t.Errorf("Mode = %q, want idle", p.Mode)
	}
	if p.IdleMinutes != 60 {
		t.Errorf("IdleMinutes = %d, want 60", p.IdleMinutes)
	}
	if p.DailyAtHour != 3 {
		t.Errorf("DailyAtHour = %d, want 3", p.DailyAtHour)
	}
	if p.NotifyOnReset {
		t.Errorf("NotifyOnReset should be false")
	}
}

func TestLoadResetPolicyFromConfig_Midnight(t *testing.T) {
	// Mode set + DailyAtHour=0 should honor midnight (0 is valid, not default)
	cfg := serverConfig{
		SessionReset: sessionResetConfig{
			Mode:        "daily",
			DailyAtHour: 0,
		},
	}
	p := loadResetPolicyFromConfig(cfg)
	if p.DailyAtHour != 0 {
		t.Errorf("midnight: got %d, want 0", p.DailyAtHour)
	}
}

func TestExpireIdleSoulSessions_StaleExpired(t *testing.T) {
	defer setupLifecycleTest(t)()

	// Fresh session — should survive
	insertTestSoulSession(t, "main", "running", time.Now())
	// Stale (2 hours old)
	insertTestSoulSession(t, "main", "running", time.Now().Add(-2*time.Hour))
	// Very stale (3 days old)
	insertTestSoulSession(t, "main", "suspended", time.Now().Add(-72*time.Hour))

	// 1-hour threshold should kill 2 sessions (2h and 3d ones)
	n, err := expireIdleSoulSessions(60)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 2 {
		t.Errorf("expired=%d, want 2", n)
	}
	if active := countActiveSoulSessions(t, "main"); active != 1 {
		t.Errorf("active after expiry = %d, want 1", active)
	}
}

func TestExpireIdleSoulSessions_ZeroIdleNoop(t *testing.T) {
	defer setupLifecycleTest(t)()
	insertTestSoulSession(t, "main", "running", time.Now().Add(-48*time.Hour))

	n, _ := expireIdleSoulSessions(0)
	if n != 0 {
		t.Errorf("0 idle should be no-op, got n=%d", n)
	}
	if countActiveSoulSessions(t, "main") != 1 {
		t.Errorf("session should survive zero-idle sweep")
	}
}

func TestMaybeDailyReset_BeforeHour(t *testing.T) {
	defer setupLifecycleTest(t)()
	insertTestSoulSession(t, "main", "running", time.Now())

	// Use an hour that hasn't happened yet today (23 = 11pm).
	// If the test happens to run after 11pm, skip gracefully.
	if time.Now().Hour() >= 23 {
		t.Skip("skipping: test cannot run after 23:00 local time")
	}
	n, err := maybeDailyReset(23, false)
	if err != nil {
		t.Fatalf("daily reset: %v", err)
	}
	if n != 0 {
		t.Errorf("before-hour should not fire, got n=%d", n)
	}
	if countActiveSoulSessions(t, "main") != 1 {
		t.Errorf("session should survive pre-hour reset")
	}
}

func TestMaybeDailyReset_AfterHourAndIdempotent(t *testing.T) {
	defer setupLifecycleTest(t)()
	insertTestSoulSession(t, "main", "running", time.Now())
	insertTestSoulSession(t, "main", "suspended", time.Now())

	// Use hour 0 (midnight) — we're guaranteed to be past it.
	n, err := maybeDailyReset(0, false)
	if err != nil {
		t.Fatalf("first reset: %v", err)
	}
	if n != 2 {
		t.Errorf("first reset n=%d, want 2", n)
	}
	if countActiveSoulSessions(t, "main") != 0 {
		t.Errorf("all sessions should be ended")
	}

	// Insert a new session after the reset
	insertTestSoulSession(t, "main", "running", time.Now())

	// Second call same day should be a no-op (idempotent)
	n2, err := maybeDailyReset(0, false)
	if err != nil {
		t.Fatalf("second reset: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second call should be idempotent, got n=%d", n2)
	}
	if countActiveSoulSessions(t, "main") != 1 {
		t.Errorf("new session should survive idempotent second call")
	}
}

func TestRunLifecycleSweep_Modes(t *testing.T) {
	defer setupLifecycleTest(t)()

	// Mode "none" should be a pure no-op
	insertTestSoulSession(t, "main", "running", time.Now().Add(-48*time.Hour))
	runLifecycleSweep(SessionResetPolicy{Mode: "none", IdleMinutes: 60, DailyAtHour: 0})
	if countActiveSoulSessions(t, "main") != 1 {
		t.Errorf("mode=none should skip expiry")
	}

	// Mode "idle" should expire the stale one
	runLifecycleSweep(SessionResetPolicy{Mode: "idle", IdleMinutes: 60, DailyAtHour: 0})
	if countActiveSoulSessions(t, "main") != 0 {
		t.Errorf("mode=idle should expire stale session")
	}
}

func TestSessionLifecycleWatcher_CancelsCleanly(t *testing.T) {
	defer setupLifecycleTest(t)()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sessionLifecycleWatcher(ctx, SessionResetPolicy{Mode: "none"}, 50*time.Millisecond)
		close(done)
	}()

	// Let it run a couple of ticks
	time.Sleep(120 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// exited cleanly
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not stop within timeout")
	}
}

func TestSessionLifecycleWatcher_Singleton(t *testing.T) {
	defer setupLifecycleTest(t)()

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	started1 := make(chan struct{})
	started2 := make(chan struct{})
	exited2 := make(chan struct{})

	go func() {
		close(started1)
		sessionLifecycleWatcher(ctx1, SessionResetPolicy{Mode: "none"}, 50*time.Millisecond)
	}()
	<-started1
	time.Sleep(20 * time.Millisecond) // let first one claim the flag

	go func() {
		close(started2)
		sessionLifecycleWatcher(ctx2, SessionResetPolicy{Mode: "none"}, 50*time.Millisecond)
		close(exited2)
	}()
	<-started2

	// Second watcher should return immediately because the first holds the singleton
	select {
	case <-exited2:
		// expected: second watcher refused to start
	case <-time.After(300 * time.Millisecond):
		t.Fatal("second watcher did not return; singleton guard failed")
	}
}

func TestLifecycleKV_SetGet(t *testing.T) {
	defer setupLifecycleTest(t)()

	db, _ := openDB()
	defer db.Close()
	ensureLifecycleKVTable(db)

	if v := kvGet(db, "nonexistent"); v != "" {
		t.Errorf("missing key should return empty, got %q", v)
	}
	kvSet(db, "test", "value1")
	if v := kvGet(db, "test"); v != "value1" {
		t.Errorf("got %q, want value1", v)
	}
	kvSet(db, "test", "value2")
	if v := kvGet(db, "test"); v != "value2" {
		t.Errorf("upsert failed, got %q", v)
	}
}
