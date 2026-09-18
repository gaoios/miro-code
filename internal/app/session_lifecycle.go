package app

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"time"
)

// Session lifecycle management for soul sessions.
//
// Complements the pre-existing soul_session.go by adding:
//   1. Parameterized idle-expiry (not hardcoded 24h)
//   2. Daily reset at a configurable hour with idempotency tracking
//   3. Background watcher goroutine that runs inside weiran server
//
// Borrowed design ideas from Hermes Agent's _session_expiry_watcher,
// adapted to weiran's simpler single-agent-on-localhost topology.

// SessionResetPolicy controls when active soul sessions are terminated.
//   Mode "idle"   — expire sessions whose updated_at is older than IdleMinutes
//   Mode "daily"  — end all active sessions once per day at DailyAtHour local time
//   Mode "both"   — apply both rules (default)
//   Mode "none"   — watcher runs but no automatic lifecycle actions
type SessionResetPolicy struct {
	Mode          string `json:"mode"`
	IdleMinutes   int    `json:"idleMinutes"`
	DailyAtHour   int    `json:"dailyAtHour"`
	NotifyOnReset bool   `json:"notifyOnReset"`
}

// defaultResetPolicy returns the conservative defaults:
//   both rules active, 24h idle, daily at 04:00 local, notifications on.
func defaultResetPolicy() SessionResetPolicy {
	return SessionResetPolicy{
		Mode:          "both",
		IdleMinutes:   1440, // 24 hours
		DailyAtHour:   4,    // 04:00 local time
		NotifyOnReset: true,
	}
}

// loadResetPolicyFromConfig extracts the session-reset block from
// serverConfig, falling back to defaults.
//
// Convention: an empty Mode string means "no config present" → return pure
// defaults (including NotifyOnReset=true). When Mode IS set, the caller
// has opted in to configure the policy; zero-valued numeric fields fall
// back to defaults individually, and NotifyOnReset is honored verbatim.
func loadResetPolicyFromConfig(cfg serverConfig) SessionResetPolicy {
	p := defaultResetPolicy()
	c := cfg.SessionReset
	if c.Mode == "" {
		return p
	}
	p.Mode = c.Mode
	if c.IdleMinutes > 0 {
		p.IdleMinutes = c.IdleMinutes
	}
	if c.DailyAtHour >= 0 && c.DailyAtHour <= 23 {
		// 0 is valid (midnight); user opted in by setting Mode, so honor it.
		p.DailyAtHour = c.DailyAtHour
	}
	p.NotifyOnReset = c.NotifyOnReset
	return p
}

// ─── kv table for idempotency state ────────────────────────────────────────

const lifecycleKVSchema = `CREATE TABLE IF NOT EXISTS lifecycle_kv (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL
)`

// ensureLifecycleKVTable creates the tiny kv table used to track
// "last_daily_reset" date (and any future lifecycle state we need).
func ensureLifecycleKVTable(db *sql.DB) error {
	_, err := db.Exec(lifecycleKVSchema)
	return err
}

func kvGet(db *sql.DB, key string) string {
	var v string
	db.QueryRow(`SELECT value FROM lifecycle_kv WHERE key = ?`, key).Scan(&v)
	return v
}

func kvSet(db *sql.DB, key, value string) error {
	now := time.Now().Format(time.RFC3339)
	_, err := db.Exec(`INSERT INTO lifecycle_kv(key, value, updated_at) VALUES(?,?,?)
	         ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, value, now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] lifecycle kvSet(%s): %v\n", appName, key, err)
	}
	return err
}

// ─── expiry actions ────────────────────────────────────────────────────────

// expireIdleSoulSessions marks any running/suspended soul session as ended
// if its updated_at is older than idleMinutes.
// Generalisation of the hardcoded 24h logic in endStaleSoulSessions().
func expireIdleSoulSessions(idleMinutes int) (int, error) {
	if idleMinutes <= 0 {
		return 0, nil
	}
	db, err := openDB()
	if err != nil {
		return 0, err
	}
	defer db.Close()

	cutoff := time.Now().Add(-time.Duration(idleMinutes) * time.Minute).Format(time.RFC3339)
	reason := fmt.Sprintf("auto-expired: %dmin inactive", idleMinutes)
	res, err := db.Exec(`UPDATE soul_sessions
		SET status='ended', context=?, ended_at=datetime('now'), updated_at=datetime('now')
		WHERE status IN ('running','suspended') AND updated_at < ?`, reason, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		fmt.Fprintf(os.Stderr, "[%s] expired %d stale soul sessions (idle > %dmin)\n", appName, n, idleMinutes)
	}
	return int(n), nil
}

// maybeDailyReset ends all active soul sessions exactly once per calendar
// day after the clock crosses atHour:00 local time. Uses lifecycle_kv to
// ensure idempotency (key = "last_daily_reset", value = "YYYY-MM-DD").
// Returns the number of sessions reset (0 if no reset happened).
func maybeDailyReset(atHour int, notify bool) (int, error) {
	if atHour < 0 || atHour > 23 {
		return 0, nil
	}
	db, err := openDB()
	if err != nil {
		return 0, err
	}
	defer db.Close()
	if err := ensureLifecycleKVTable(db); err != nil {
		return 0, err
	}

	now := time.Now()
	// Only proceed if local clock is at or past the daily hour
	if now.Hour() < atHour {
		return 0, nil
	}

	today := now.Format("2006-01-02")
	if last := kvGet(db, "last_daily_reset"); last == today {
		return 0, nil
	}

	res, err := db.Exec(`UPDATE soul_sessions
		SET status='ended', context='auto-expired: daily reset', ended_at=datetime('now'), updated_at=datetime('now')
		WHERE status IN ('running','suspended')`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()

	// Always stamp today to prevent re-firing later in the same day, even
	// if there were no rows to reset this time around.
	if err := kvSet(db, "last_daily_reset", today); err != nil {
		// If we can't persist the key, the reset will re-fire next tick.
		// Log the error but still return the count of sessions we did reset.
		fmt.Fprintf(os.Stderr, "[%s] WARNING: daily reset idempotency key failed to persist: %v\n", appName, err)
	}

	if n > 0 {
		fmt.Fprintf(os.Stderr, "[%s] daily reset at %02d:00 → ended %d active soul sessions\n", appName, atHour, n)
		if notify && tgChatID != "" {
			trySendTelegram(fmt.Sprintf("🌅 daily reset — ended %d active soul session(s) at %02d:00", n, atHour))
		}
	}
	return int(n), nil
}

// ─── background watcher ───────────────────────────────────────────────────

// watcherMu guards the singleton watcher goroutine so callers never spawn
// more than one (defensive; only used in tests and server startup).
var watcherMu sync.Mutex
var watcherRunning bool

// sessionLifecycleWatcher runs until ctx is cancelled. It wakes every
// `interval` (default 5min) and applies the policy.
//
// Safe to call exactly once from server.go startup, passing the server's
// shutdown context so the goroutine exits cleanly on SIGTERM.
func sessionLifecycleWatcher(ctx context.Context, policy SessionResetPolicy, interval time.Duration) {
	watcherMu.Lock()
	if watcherRunning {
		watcherMu.Unlock()
		return
	}
	watcherRunning = true
	watcherMu.Unlock()

	defer func() {
		watcherMu.Lock()
		watcherRunning = false
		watcherMu.Unlock()
	}()

	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if policy.Mode == "" {
		policy.Mode = "both"
	}

	fmt.Fprintf(os.Stderr, "[%s] session lifecycle watcher: mode=%s idle=%dmin daily=%02d:00 interval=%s\n",
		appName, policy.Mode, policy.IdleMinutes, policy.DailyAtHour, interval)

	// Run a quick sweep immediately so startup cleans up any stale
	// sessions left by a previous crash.
	runLifecycleSweep(policy)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "[%s] session lifecycle watcher: stopping\n", appName)
			return
		case <-ticker.C:
			runLifecycleSweep(policy)
		}
	}
}

// runLifecycleSweep applies the policy exactly once.
func runLifecycleSweep(policy SessionResetPolicy) {
	if policy.Mode == "none" {
		return
	}
	if policy.Mode == "idle" || policy.Mode == "both" {
		if _, err := expireIdleSoulSessions(policy.IdleMinutes); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] idle expiry: %v\n", appName, err)
		}
	}
	if policy.Mode == "daily" || policy.Mode == "both" {
		if _, err := maybeDailyReset(policy.DailyAtHour, policy.NotifyOnReset); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] daily reset: %v\n", appName, err)
		}
	}
}

// ─── backward compat wrapper ──────────────────────────────────────────────

// Note: the hardcoded endStaleSoulSessions() in soul_session.go is kept as-is
// for callers (heartbeat/cron entry points). It is equivalent to
// expireIdleSoulSessions(24 * 60). The watcher operates in parallel without
// conflicting: both take the same UPDATE path.
