package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsConfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/kiyor/soul-cli/pkg/provider/gemini"
	"github.com/kiyor/soul-cli/pkg/provider/ollama"
	"github.com/kiyor/soul-cli/pkg/provider/openai"
)

//go:embed web/index.html
var indexHTML []byte

//go:embed web/manifest.json
var manifestJSON []byte

//go:embed web/service-worker.js
var serviceWorkerJS []byte

//go:embed web/icons
var iconsFS embed.FS

//go:embed web/css
var cssFS embed.FS

//go:embed web/js
var jsFS embed.FS

// ── Server Config ──

type serverConfig struct {
	Token                   string             `json:"token"`
	Host                    string             `json:"host"`
	Port                    int                `json:"port"`
	MaxSessions             int                `json:"maxSessions"`
	IdleTimeoutMin          int                `json:"idleTimeoutMin"`
	MaxLifetimeHrs          int                `json:"maxLifetimeHours"`
	RateLimitPerMin         int                `json:"rateLimitPerMin"`
	MaxInteractionRounds    int                `json:"maxInteractionRounds"`
	Telegram                telegramBotConfig  `json:"telegram"`
	Proxy                   proxyConfig        `json:"proxy"`
	SessionReset            sessionResetConfig `json:"sessionReset"`
	S3                      s3Config           `json:"s3"`
	DBExplorer              dbExplorerConfig   `json:"dbExplorer"`
	DefaultReplaceSoul      bool               `json:"defaultReplaceSoul"`      // 本我模式 default for new sessions
	DefaultInteractiveModel string             `json:"defaultInteractiveModel"` // default model for new interactive sessions; fallback: opus[1m]
	DisableTmuxDrawer       bool               `json:"disableTmuxDrawer"`       // hide the tmux drawer/tab in the web UI
}

// s3Config holds Wasabi/S3 upload settings for image hosting.
type s3Config struct {
	Endpoint string `json:"endpoint"` // e.g. "https://s3.us-west-1.wasabisys.com"
	Bucket   string `json:"bucket"`   // e.g. "my-agent-images"
	Region   string `json:"region"`   // e.g. "us-west-1"
	Prefix   string `json:"prefix"`   // e.g. "webui/"
	Profile  string `json:"profile"`  // AWS credentials profile, e.g. "wasabi"
}

// Cached S3 client to avoid re-loading AWS config on every upload.
var (
	s3ClientOnce   sync.Once
	s3ClientCached *s3.Client
)

func getS3Client(cfg s3Config) *s3.Client {
	s3ClientOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		var awsCfg aws.Config
		var err error
		if cfg.Profile != "" {
			awsCfg, err = awsConfig.LoadDefaultConfig(ctx,
				awsConfig.WithSharedConfigProfile(cfg.Profile),
				awsConfig.WithRegion(cfg.Region),
			)
		} else {
			awsCfg, err = awsConfig.LoadDefaultConfig(ctx,
				awsConfig.WithRegion(cfg.Region),
			)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] s3: load config failed: %v\n", appName, err)
			return
		}
		s3ClientCached = s3.NewFromConfig(awsCfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			o.UsePathStyle = true // Wasabi requires path-style
		})
	})
	return s3ClientCached
}

// sessionResetConfig mirrors SessionResetPolicy for JSON config loading.
// Fields default to zero-values; loadResetPolicyFromConfig fills in sane
// defaults for anything unset.
type sessionResetConfig struct {
	Mode          string `json:"mode"`          // "idle" | "daily" | "both" | "none"
	IdleMinutes   int    `json:"idleMinutes"`   // 0 → default 1440
	DailyAtHour   int    `json:"dailyAtHour"`   // 0-23; default 4
	NotifyOnReset bool   `json:"notifyOnReset"` // send Telegram on reset
}

// telegramBotConfig holds Telegram bot settings.
type telegramBotConfig struct {
	Enabled        bool     `json:"enabled"`
	AllowedChatIDs []string `json:"allowedChatIDs"` // if empty, uses global tgChatID
}

func defaultServerConfig() serverConfig {
	return serverConfig{
		Host:                 "0.0.0.0",
		Port:                 9847,
		MaxSessions:          5,
		IdleTimeoutMin:       30,
		MaxLifetimeHrs:       4,
		RateLimitPerMin:      60,
		MaxInteractionRounds: defaultMaxInteractionRounds,
		Proxy:                defaultProxyConfig(),
		DefaultReplaceSoul:   true, // 本我模式 default on
	}
}

// loadServerConfig reads server config from config.json's "server" field.
func loadServerConfig() serverConfig {
	cfg := defaultServerConfig()

	configPaths := []string{
		appDir + "/config.json",
	}
	for _, p := range configPaths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var root map[string]json.RawMessage
		if json.Unmarshal(data, &root) != nil {
			break
		}
		serverRaw, ok := root["server"]
		if !ok {
			break
		}
		var serverFields map[string]json.RawMessage
		_ = json.Unmarshal(serverRaw, &serverFields)
		var srv serverConfig
		if json.Unmarshal(serverRaw, &srv) == nil {
			if srv.Token != "" {
				cfg.Token = srv.Token
			}
			if srv.Host != "" {
				cfg.Host = srv.Host
			}
			if srv.Port != 0 {
				cfg.Port = srv.Port
			}
			if srv.MaxSessions > 0 {
				cfg.MaxSessions = srv.MaxSessions
			}
			if srv.IdleTimeoutMin > 0 {
				cfg.IdleTimeoutMin = srv.IdleTimeoutMin
			}
			if srv.MaxLifetimeHrs > 0 {
				cfg.MaxLifetimeHrs = srv.MaxLifetimeHrs
			}
			if srv.RateLimitPerMin > 0 {
				cfg.RateLimitPerMin = srv.RateLimitPerMin
			}
			if srv.MaxInteractionRounds > 0 {
				cfg.MaxInteractionRounds = srv.MaxInteractionRounds
			}
			cfg.Telegram = srv.Telegram
			// Session reset policy (always copy; defaults are filled in by loadResetPolicyFromConfig)
			cfg.SessionReset = srv.SessionReset
			cfg.S3 = srv.S3
			if _, ok := serverFields["dbExplorer"]; ok {
				cfg.DBExplorer = srv.DBExplorer
				if dbRaw, ok := serverFields["dbExplorer"]; ok {
					var dbFields map[string]json.RawMessage
					if json.Unmarshal(dbRaw, &dbFields) == nil {
						_, cfg.DBExplorer.enabledSet = dbFields["enabled"]
					}
				}
			}
			if _, ok := serverFields["defaultReplaceSoul"]; ok {
				cfg.DefaultReplaceSoul = srv.DefaultReplaceSoul
			}
			cfg.DisableTmuxDrawer = srv.DisableTmuxDrawer
			if srv.Proxy.Port != 0 || srv.Proxy.Upstream != "" || srv.Proxy.Enabled {
				if srv.Proxy.Enabled {
					cfg.Proxy.Enabled = true
				}
				if srv.Proxy.Port != 0 {
					cfg.Proxy.Port = srv.Proxy.Port
				}
				if srv.Proxy.Upstream != "" {
					cfg.Proxy.Upstream = srv.Proxy.Upstream
				}
			}
			// Default interactive model
			if srv.DefaultInteractiveModel != "" {
				cfg.DefaultInteractiveModel = srv.DefaultInteractiveModel
			}
		}
		break
	}

	// Validate defaultInteractiveModel; fallback to opus[1m] if unset or invalid.
	// Use nativeModelAliases for validation first (no provider dependency),
	// then try resolveFuzzyModel for provider models.
	const fallbackInteractiveModel = "opus[1m]"
	if cfg.DefaultInteractiveModel == "" {
		cfg.DefaultInteractiveModel = fallbackInteractiveModel
	} else if _, ok := nativeModelAliases[cfg.DefaultInteractiveModel]; !ok {
		// Not a native alias — try full resolution (may depend on providers)
		if resolved, err := resolveFuzzyModel(cfg.DefaultInteractiveModel); err != nil || resolved == "" {
			fmt.Fprintf(os.Stderr, "[%s] server: invalid defaultInteractiveModel %q, falling back to %s\n", appName, cfg.DefaultInteractiveModel, fallbackInteractiveModel)
			cfg.DefaultInteractiveModel = fallbackInteractiveModel
		}
	}

	return cfg
}

// ── Rate Limiter (per-token, sliding window) ──

type rateLimiter struct {
	mu       sync.Mutex
	requests []time.Time
	limit    int
	window   time.Duration
}

func newRateLimiter(limit int) *rateLimiter {
	return &rateLimiter{
		limit:  limit,
		window: time.Minute,
	}
}

func (rl *rateLimiter) allow() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.window)

	// Trim old entries
	valid := 0
	for _, t := range rl.requests {
		if t.After(cutoff) {
			rl.requests[valid] = t
			valid++
		}
	}
	rl.requests = rl.requests[:valid]

	if len(rl.requests) >= rl.limit {
		return false
	}
	rl.requests = append(rl.requests, now)
	return true
}

// ── HTTP Server ──

// handleServer is the main entry point for `weiran server`.
func handleServer(args []string) {
	isServerMode = true
	cfg := loadServerConfig()

	// Override from CLI flags
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &cfg.Port)
				i++
			}
		case "--host":
			if i+1 < len(args) {
				cfg.Host = args[i+1]
				i++
			}
		case "--token":
			if i+1 < len(args) {
				cfg.Token = args[i+1]
				i++
			}
		}
	}

	// Token from env overrides
	if envToken := os.Getenv("WEIRAN_SERVER_TOKEN"); envToken != "" {
		cfg.Token = envToken
	}

	// Stash server config globals for IPC env injection
	serverPort = cfg.Port
	serverAuthToken = cfg.Token

	// Initialize the webhook framework's compiled-plugin cache dir
	// (workspace/webhook/_bin/). Idempotent and best-effort — domain
	// directories (download roots, output trees, etc.) are NOT created
	// here; that's the plugin's responsibility.
	initWebhookDirs()

	// Refuse to start without auth token
	if cfg.Token == "" {
		fmt.Fprintf(os.Stderr, "[%s] server: refusing to start without auth token\n", appName)
		fmt.Fprintf(os.Stderr, "  set via: --token, WEIRAN_SERVER_TOKEN env, or config.json server.token\n")
		os.Exit(1)
	}

	// Info if binding to non-localhost
	if cfg.Host != "127.0.0.1" && cfg.Host != "localhost" && cfg.Host != "::1" {
		fmt.Fprintf(os.Stderr, "[%s] server: binding to %s (non-localhost)\n", appName, cfg.Host)
	}

	sm := newSessionManager(
		cfg.MaxSessions,
		time.Duration(cfg.IdleTimeoutMin)*time.Minute,
		time.Duration(cfg.MaxLifetimeHrs)*time.Hour,
	)

	rl := newRateLimiter(cfg.RateLimitPerMin)

	// WebSocket hub for bidirectional real-time sync
	hub := newWSHub(cfg.Token, rl, cfg.DefaultReplaceSoul, cfg.DefaultInteractiveModel)
	hub.sm = sm
	sm.hub = hub

	mux := http.NewServeMux()

	// Health (no auth required)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"status":        "ok",
			"app":           appName,
			"agent_name":    agentName,
			"version":       buildVersion,
			"sessions":      len(sm.listSessions()),
			"uptime":        time.Since(serverStartTime).String(),
			"sniff_enabled": activeProxyPort > 0,
			"sniff_port":    activeProxyPort,
		}
		if agentAvatarURL != "" {
			resp["avatar_url"] = agentAvatarURL
		}
		if userAvatarURL != "" {
			resp["user_avatar_url"] = userAvatarURL
		}
		if agentWelcomeImage != "" {
			resp["welcome_image"] = agentWelcomeImage
		}
		writeJSON(w, http.StatusOK, resp)
	})

	// Codex backend metrics — authed because the snapshot exposes internal
	// architecture details (queue depth, session counts, JSON-RPC stats).
	// See codex_metrics.go for the snapshot shape.
	mux.HandleFunc("GET /api/codex/metrics", authMiddleware(cfg.Token, handleCodexMetrics))

	// Skill-driven webhook endpoint. Receives JSON bodies (typically from
	// iPhone Shortcuts share-sheets), routes by `action` field to plugin
	// files under workspace/webhook/, and falls back to AI-spawned sessions
	// on plugin miss/failure. See webhook.go for the full protocol.
	mux.HandleFunc("POST /webhook", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow() {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
			return
		}
		handleWebhook(sm, w, r)
	}))

	// Config info (authed)
	mux.HandleFunc("GET /api/config", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"max_sessions":              cfg.MaxSessions,
			"idle_timeout_min":          cfg.IdleTimeoutMin,
			"max_lifetime_hrs":          cfg.MaxLifetimeHrs,
			"rate_limit":                cfg.RateLimitPerMin,
			"default_replace_soul":      cfg.DefaultReplaceSoul,
			"default_interactive_model": cfg.DefaultInteractiveModel,
			"disable_tmux_drawer":       cfg.DisableTmuxDrawer,
			"workspace":                 workspace,
			"claude_bin":                claudeBin,
		})
	}))

	// FTS5 full-text search over daily notes + session summaries
	// GET /api/search?q=<query>&scope=<daily|session|content|both>&limit=<N>
	mux.HandleFunc("GET /api/search", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if q == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "q required"})
			return
		}
		scope := r.URL.Query().Get("scope")
		if scope == "" {
			scope = "both"
		}
		limit := 20
		if l := r.URL.Query().Get("limit"); l != "" {
			fmt.Sscanf(l, "%d", &limit)
			if limit < 1 || limit > 200 {
				limit = 20
			}
		}
		hits, err := searchFTS(q, scope, limit)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"query": q,
			"scope": scope,
			"hits":  hits,
			"total": len(hits),
		})
	}))

	// Memory audit log
	// GET /api/memory/audit?days=7&op=recall&limit=100
	mux.HandleFunc("GET /api/memory/audit", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		days := 7
		if d := r.URL.Query().Get("days"); d != "" {
			fmt.Sscanf(d, "%d", &days)
		}
		limit := 100
		if l := r.URL.Query().Get("limit"); l != "" {
			fmt.Sscanf(l, "%d", &limit)
		}
		op := r.URL.Query().Get("op")
		db := openAuditDB()
		if db == nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "audit DB not available"})
			return
		}
		entries, err := queryMemoryAudit(db, days, op, limit)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "total": len(entries)})
	}))

	// Memory audit aggregated stats
	// GET /api/memory/stats?days=7
	mux.HandleFunc("GET /api/memory/stats", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		days := 7
		if d := r.URL.Query().Get("days"); d != "" {
			fmt.Sscanf(d, "%d", &days)
		}
		db := openAuditDB()
		if db == nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "audit DB not available"})
			return
		}
		stats, err := queryMemoryAuditStats(db, days)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, stats)
	}))

	// Link preview (OG tags)
	mux.HandleFunc("GET /api/link-preview", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		url := r.URL.Query().Get("url")
		if url == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url required"})
			return
		}
		data := fetchOGTags(url)
		if data == nil {
			writeJSON(w, http.StatusNoContent, nil)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=86400")
		writeJSON(w, http.StatusOK, data)
	}))

	// Tmux sessions (read-only drawer). When the user doesn't use tmux directly,
	// everything visible here is the agent (or its spawned tasks) running long jobs.
	mux.HandleFunc("GET /api/tmux/sessions", authMiddleware(cfg.Token, handleTmuxSessions))
	mux.HandleFunc("GET /api/tmux/capture", authMiddleware(cfg.Token, handleTmuxCapture))

	// List configured providers (for UI model dropdown, apiKey redacted)
	mux.HandleFunc("GET /api/providers", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		type providerInfo struct {
			Name   string   `json:"name"`
			Models []string `json:"models,omitempty"`
		}
		providers, _ := loadAllProviders()
		var infos []providerInfo
		for name, prov := range providers {
			if prov.BaseURL == "" && prov.Type != "openai" && prov.Type != "ollama" && prov.Type != "gemini" {
				continue
			}
			infos = append(infos, providerInfo{Name: name, Models: prov.Models})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"providers":    infos,
			"defaultModel": defaultModel,
		})
	}))

	// OpenAI/Codex proxy management: start proxy in server process and return port.
	// CLI uses this to avoid starting its own embedded proxy (which dies on syscall.Exec).
	// GET /api/proxy/openai?provider=codex
	mux.HandleFunc("GET /api/proxy/openai", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		providerName := r.URL.Query().Get("provider")
		if providerName == "" {
			http.Error(w, "provider query param required", http.StatusBadRequest)
			return
		}
		pcfg := resolveProvider(providerName)
		if pcfg == nil || pcfg.Type != "openai" {
			http.Error(w, "provider not found or not openai type", http.StatusNotFound)
			return
		}
		// Singleton per provider: reuse if already started
		if cached, ok := serverOpenAIProxies.Load(providerName); ok {
			writeJSON(w, http.StatusOK, map[string]int{"port": cached.(int)})
			return
		}
		port, err := openai.StartProxy(*pcfg)
		if err != nil {
			http.Error(w, "start proxy: "+err.Error(), http.StatusInternalServerError)
			return
		}
		serverOpenAIProxies.Store(providerName, port)
		writeJSON(w, http.StatusOK, map[string]int{"port": port})
	}))

	// Ollama proxy management: start ollama proxy in server process and return port.
	// GET /api/proxy/ollama?provider=ollama
	mux.HandleFunc("GET /api/proxy/ollama", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		providerName := r.URL.Query().Get("provider")
		if providerName == "" {
			http.Error(w, "provider query param required", http.StatusBadRequest)
			return
		}
		pcfg := resolveProvider(providerName)
		if pcfg == nil || pcfg.Type != "ollama" {
			http.Error(w, "provider not found or not ollama type", http.StatusNotFound)
			return
		}
		if cached, ok := serverOllamaProxies.Load(providerName); ok {
			writeJSON(w, http.StatusOK, map[string]int{"port": cached.(int)})
			return
		}
		port, err := ollama.StartProxy(*pcfg)
		if err != nil {
			http.Error(w, "start proxy: "+err.Error(), http.StatusInternalServerError)
			return
		}
		serverOllamaProxies.Store(providerName, port)
		writeJSON(w, http.StatusOK, map[string]int{"port": port})
	}))

	// Gemini (Code Assist) proxy management: start gemini proxy in server process and return port.
	// GET /api/proxy/gemini?provider=gemini
	mux.HandleFunc("GET /api/proxy/gemini", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		providerName := r.URL.Query().Get("provider")
		if providerName == "" {
			http.Error(w, "provider query param required", http.StatusBadRequest)
			return
		}
		pcfg := resolveProvider(providerName)
		if pcfg == nil || pcfg.Type != "gemini" {
			http.Error(w, "provider not found or not gemini type", http.StatusNotFound)
			return
		}
		if cached, ok := serverGeminiProxies.Load(providerName); ok {
			writeJSON(w, http.StatusOK, map[string]int{"port": cached.(int)})
			return
		}
		port, err := gemini.StartProxy(*pcfg)
		if err != nil {
			http.Error(w, "start proxy: "+err.Error(), http.StatusInternalServerError)
			return
		}
		serverGeminiProxies.Store(providerName, port)
		writeJSON(w, http.StatusOK, map[string]int{"port": port})
	}))

	// List available skills
	mux.HandleFunc("GET /api/skills", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		var skills []map[string]string
		for _, dir := range skillDirs {
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				skillMd := dir + "/" + e.Name() + "/SKILL.md"
				if _, err := os.Stat(skillMd); err != nil {
					continue
				}
				name, desc, _ := parseSkillFrontmatter(skillMd)
				if name == "" {
					name = e.Name()
				}
				// truncate description
				for _, cut := range []string{"Trigger", "trigger", "触发"} {
					if idx := strings.Index(desc, cut); idx > 0 {
						desc = strings.TrimSpace(desc[:idx])
						desc = strings.TrimRight(desc, "。.\n :：,，")
						break
					}
				}
				if len(desc) > 80 {
					desc = desc[:80] + "…"
				}
				skills = append(skills, map[string]string{"name": name, "description": desc})
			}
		}
		writeJSON(w, http.StatusOK, skills)
	}))

	// Path completion for project directory input
	mux.HandleFunc("GET /api/complete-path", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		prefix := r.URL.Query().Get("prefix")
		home, _ := os.UserHomeDir()

		// expand ~ to home dir
		if strings.HasPrefix(prefix, "~/") {
			prefix = filepath.Join(home, prefix[2:])
		} else if prefix == "~" {
			prefix = home
		}

		// split into directory and partial filename
		dir := filepath.Dir(prefix)
		partial := filepath.Base(prefix)
		// if prefix ends with /, list that directory
		if strings.HasSuffix(r.URL.Query().Get("prefix"), "/") || prefix == home {
			dir = prefix
			partial = ""
		}

		entries, err := os.ReadDir(dir)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"prefix":      prefix,
				"completions": []string{},
				"common":      "",
				"base":        dir,
			})
			return
		}

		var matches []string
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if partial == "" || strings.HasPrefix(e.Name(), partial) {
				matches = append(matches, e.Name())
			}
		}
		// cap results (default 20 for tab-complete, higher for browse mode)
		limit := 20
		if lq := r.URL.Query().Get("limit"); lq != "" {
			if n, err := strconv.Atoi(lq); err == nil && n > 0 && n <= 200 {
				limit = n
			}
		}
		if len(matches) > limit {
			matches = matches[:limit]
		}

		// compute longest common prefix
		common := ""
		if len(matches) == 1 {
			common = matches[0]
		} else if len(matches) > 1 {
			common = matches[0]
			for _, m := range matches[1:] {
				for !strings.HasPrefix(m, common) {
					common = common[:len(common)-1]
				}
			}
		}

		// convert dir back to ~/ for display
		base := dir
		if strings.HasPrefix(base, home) {
			base = "~" + base[len(home):]
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"prefix":      prefix,
			"completions": matches,
			"common":      common,
			"base":        base,
		})
	}))

	// List sessions (optional ?category=interactive to filter)
	mux.HandleFunc("GET /api/sessions", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		all := sm.listSessions()
		cat := r.URL.Query().Get("category")
		if cat == "" {
			writeJSON(w, http.StatusOK, all)
			return
		}
		filtered := make([]map[string]any, 0)
		for _, s := range all {
			if s["category"] == cat {
				filtered = append(filtered, s)
			}
		}
		writeJSON(w, http.StatusOK, filtered)
	}))

	// Create session
	mux.HandleFunc("POST /api/sessions", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow() {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
			return
		}

		var req struct {
			Name           string   `json:"name"`
			Project        string   `json:"project"`
			Model          string   `json:"model"`
			InitialMessage string   `json:"initial_message"`
			Message        string   `json:"message"` // alias for initial_message
			SoulFiles      *bool    `json:"soul_files"`
			MCPConfig      string   `json:"mcp_config"`
			GalID          string   `json:"gal_id"`
			Category       string   `json:"category"`
			Tags           []string `json:"tags"`
			ReplaceSoul    *bool    `json:"replace_soul"` // nil → use config default
			Mode           string   `json:"mode"`         // "weiran"|"benwo"|"cc"; overrides legacy bools when set
			SpawnedBy      string   `json:"spawned_by"`   // parent session ID
			// Backend selects the harness implementation: "cc" (default,
			// Claude Code stream-json), "codex" (OpenAI codex JSON-RPC), or
			// "grok" (Grok Build headless). Empty triggers resolveBackendKind
			// which auto-routes based on model prefix / backend model_map.
			Backend string `json:"backend"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		// Support "message" as alias for "initial_message"
		if req.InitialMessage == "" && req.Message != "" {
			req.InitialMessage = req.Message
		}

		if req.Name == "" {
			req.Name = fmt.Sprintf("session-%s", time.Now().Format("0102-1504"))
		}
		if req.Project == "" {
			req.Project = workspace
		}

		// Resolve soul/replace_soul from mode enum if provided, else fall back
		// to legacy bool fields (default soul=true, replace=cfg default).
		soulFiles := true
		if req.SoulFiles != nil {
			soulFiles = *req.SoulFiles
		}
		replaceSoul := cfg.DefaultReplaceSoul
		if req.ReplaceSoul != nil {
			replaceSoul = *req.ReplaceSoul
		}
		if req.Mode != "" {
			if s, rp, ok := modeToFlags(req.Mode); ok {
				soulFiles = s
				replaceSoul = rp
			} else {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("unknown mode %q", req.Mode)})
				return
			}
		}

		// Backend kind: explicit body field wins; empty → auto-route by model.
		// Validate the explicit value so a typo doesn't silently fall back.
		var backendKind BackendKind
		switch strings.ToLower(strings.TrimSpace(req.Backend)) {
		case "", "auto":
			// Empty / "auto" → resolveBackendKind decides.
			backendKind = ""
		case string(BackendCC), "claude", "claude-code":
			backendKind = BackendCC
		case string(BackendCodex), "openai-codex":
			if !codexEnabled {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": "backend=codex requested but agents.codex.enabled=false in config.json",
				})
				return
			}
			backendKind = BackendCodex
		case string(BackendGrok), "grok-build":
			if !grokEnabled {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": "backend=grok requested but agents.grok.enabled=false in config.json",
				})
				return
			}
			backendKind = BackendGrok
		case string(BackendKimi), "kimi-cli", "moonshot-kimi":
			if !kimiEnabled {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": "backend=kimi requested but agents.kimi.enabled=false in config.json",
				})
				return
			}
			backendKind = BackendKimi
		case string(BackendAgy), "antigravity", "antigravity-cli":
			if !agyEnabled {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": "backend=agy requested but agents.agy.enabled=false in config.json",
				})
				return
			}
			backendKind = BackendAgy
		case string(BackendOpencode), "opencode-cli":
			if !opencodeEnabled {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": "backend=opencode requested but agents.opencode.enabled=false in config.json",
				})
				return
			}
			backendKind = BackendOpencode
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("unknown backend %q (expected cc|codex|grok|kimi|agy|opencode|auto)", req.Backend),
			})
			return
		}

		model := resolveCreateModel(req.Model, req.Category, backendKind, cfg.DefaultInteractiveModel)

		sess, err := sm.createSessionWithOpts(sessionCreateOpts{
			Name:           req.Name,
			Project:        req.Project,
			Model:          model,
			Soul:           soulFiles,
			MCP:            req.MCPConfig,
			GalID:          req.GalID,
			Category:       req.Category,
			Tags:           req.Tags,
			ReplaceSoul:    replaceSoul,
			SpawnedBy:      req.SpawnedBy,
			Backend:        backendKind,
			InitialMessage: req.InitialMessage,
		})
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}

		fmt.Fprintf(os.Stderr, "[%s] server: session created: %s (%s)\n", appName, shortID(sess.ID), sess.Name)

		// Capture first user message for hint display
		if req.InitialMessage != "" {
			// Phase D — sanitize on intake so any literal soul-patch markers
			// in the create-time InitialMessage are disarmed before we cache
			// them as FirstMsg or feed them to the model.
			//
			// Note: prepareSoulPatch() below will sanitize again, but that's
			// a fast path (strings.Contains short-circuit when no marker is
			// present, which is the overwhelmingly common case). We keep the
			// explicit sanitize here so FirstMsg — visible synchronously in
			// the create-response snapshot, before the goroutine runs — is
			// guaranteed clean.
			cleanInitial, _ := sanitizeSoulPatchInput(req.InitialMessage)
			sess.mu.Lock()
			if sess.FirstMsg == "" {
				sess.FirstMsg = cleanInitial
			}
			sess.mu.Unlock()

			// Send initial message if provided — wait for Claude Code to
			// emit init before writing to stdin (500ms hardcoded sleep was
			// unreliable for slow providers like GPT/MiniMax that need
			// proxy startup time).
			//
			// We route the initial message through prepareSoulPatch for
			// consistency with the regular /message path: the create-time
			// build already loaded fragments for the detected mode, so the
			// diff here is normally empty (no patch) — but if the user's
			// first message routes to a *different* mode than the cwd /
			// source did, the missing fragments get patched in immediately.
			go func() {
				if !sess.awaitBackendReady(30 * time.Second) {
					// Backend died during its handshake — the session is now
					// terminal/"error" and injects nothing.
					return
				}
				injection := sess.prepareSoulPatch(cleanInitial)
				userEvent, _ := json.Marshal(map[string]any{
					"type":    "user",
					"message": map[string]any{"role": "user", "content": injection.DisplayMessage},
				})
				sess.broadcaster.broadcast(sseEvent{Event: "user", Data: userEvent})
				if err := sess.process.sendMessage(injection.Outbound); err != nil {
					fmt.Fprintf(os.Stderr, "[%s] server: failed to send initial message to %s: %v\n", appName, shortID(sess.ID), err)
					return
				}
				sess.commitSoulPatchInjection(injection)
			}()
		}

		writeJSON(w, http.StatusCreated, sess.snapshot())
	}))

	// Get session
	mux.HandleFunc("GET /api/sessions/{id}", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		writeJSON(w, http.StatusOK, sess.snapshot())
	}))

	// Rename session
	mux.HandleFunc("PATCH /api/sessions/{id}/rename", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
			return
		}
		sess.mu.Lock()
		sess.Name = req.Name
		sess.mu.Unlock()
		markRenamed(sess.ID, req.Name)
		if hub != nil {
			hub.notifySessions()
		}
		fmt.Fprintf(os.Stderr, "[%s] server: session %s renamed to %q\n", appName, shortID(sess.ID), req.Name)
		writeJSON(w, http.StatusOK, map[string]string{"status": "renamed", "name": req.Name})
	}))

	// Auto-rename session (calls Haiku to generate title from conversation)
	mux.HandleFunc("POST /api/sessions/{id}/auto-rename", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		// Collect snippets from broadcaster history (same logic as tryAutoRename)
		sess.broadcaster.mu.RLock()
		var snippets []string
		for _, ev := range sess.broadcaster.history {
			if ev.Event == "assistant" {
				var peek struct {
					Message struct {
						Content []struct {
							Type string `json:"type"`
							Text string `json:"text"`
						} `json:"content"`
					} `json:"message"`
				}
				if json.Unmarshal(ev.Data, &peek) == nil {
					for _, c := range peek.Message.Content {
						if c.Type == "text" && c.Text != "" {
							text := c.Text
							if len(text) > 200 {
								text = text[:200]
							}
							snippets = append(snippets, text)
						}
					}
				}
			}
		}
		sess.broadcaster.mu.RUnlock()

		if len(snippets) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no conversation context for auto-rename"})
			return
		}
		context := strings.Join(snippets, "\n---\n")
		if len(context) > 1500 {
			context = context[:1500]
		}
		title := callHaikuForTitle(context)
		if title == "" {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "no title generated"})
			return
		}
		sess.mu.Lock()
		sess.Name = title
		sess.mu.Unlock()
		markAutoNamed(sess.ID, title)
		if hub != nil {
			hub.notifySessions()
		}
		fmt.Fprintf(os.Stderr, "[%s] server: auto-renamed %s → %q\n", appName, shortID(sess.ID), title)
		writeJSON(w, http.StatusOK, map[string]string{"status": "renamed", "name": title})
	}))

	// Delete session
	mux.HandleFunc("DELETE /api/sessions/{id}", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := sm.destroySession(id); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		fmt.Fprintf(os.Stderr, "[%s] server: session destroyed: %s\n", appName, shortID(id))
		writeJSON(w, http.StatusOK, map[string]string{"status": "destroyed"})
	}))

	// Send message to session
	mux.HandleFunc("POST /api/sessions/{id}/message", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow() {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
			return
		}

		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}

		var req struct {
			Message string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Message == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message is required"})
			return
		}

		if !sess.process.alive() {
			writeJSON(w, http.StatusGone, map[string]string{"error": "session process has exited"})
			return
		}

		sess.touch()
		sess.setStatus("running")

		// User sent a new message — implicitly dismiss any pending
		// AskUserQuestion. We reply behavior=deny so claude reads the new
		// message instead of continuing to block on stdin.
		sess.dismissAllPendingAUQ("user_message")

		// Phase D — Runtime Soul Patch Protocol: sanitize literal markers,
		// then compute the soul-mode for this turn and produce a possibly
		// patched outbound message. DisplayMessage is what the UI sees
		// (sanitized real text only), Outbound is what the model sees
		// (patches + real text).
		injection := sess.prepareSoulPatch(req.Message)

		// Capture first user message for hint display (display version, not
		// the patched outbound — the hint is for humans)
		sess.mu.Lock()
		firstMsgCaptured := sess.FirstMsg == ""
		if firstMsgCaptured {
			sess.FirstMsg = injection.DisplayMessage
		}
		sess.mu.Unlock()
		if firstMsgCaptured && hub != nil {
			hub.notifySessions()
		}

		// Broadcast user message to SSE/WS so it persists in history
		// (without this, switching sessions and back loses user messages).
		// We broadcast the display version so soul-patch payloads never
		// surface in the Web UI history pane.
		userEvent, _ := json.Marshal(map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": injection.DisplayMessage},
		})
		sess.broadcaster.broadcast(sseEvent{Event: "user", Data: userEvent})

		if err := sess.process.sendMessage(injection.Outbound); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		// Backend accepted the message → commit the new fragments onto the
		// session's persistent loaded set so we don't re-patch them next turn.
		// On send failure above we deliberately *don't* commit, so the patch
		// re-fires on the next attempt.
		sess.commitSoulPatchInjection(injection)

		writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
	}))

	// Phase D — Soul Fragment Lazy-Load: notification endpoint called from
	// the tool-hook subprocess after the agent Reads a soul fragment file.
	// Body: {"path": "/abs/path/to/soul/.../X.md"}
	//
	// Effect: appends path to LoadedFragments so server-side prepareSoulPatch
	// (the topic-based fallback detector) doesn't re-patch a fragment the
	// agent already has in conversation history. Idempotent — duplicate paths
	// are deduped inside commitLoadedFragments.
	//
	// Tolerant of unknown sessions (idle/exited/never-existed) — returns 200
	// even on miss so the hook's best-effort caller never raises a noisy
	// error path. Agent Read still happened; only the server-side dedupe
	// optimisation is missed.
	mux.HandleFunc("POST /api/sessions/{id}/loaded-fragment", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow() {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		id := r.PathValue("id")
		var req struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing path"})
			return
		}
		sess := sm.getSession(id)
		if sess == nil {
			// Unknown session — best-effort hook should not see 404 noise.
			writeJSON(w, http.StatusOK, map[string]string{"status": "session_unknown"})
			return
		}
		// Empty SoulMode passed because this endpoint records a fragment
		// load without changing the routing decision (which was set by the
		// last user message).
		sess.commitLoadedFragments([]string{req.Path}, "")
		writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
	}))

	// Receive failure reports from `weiran tool-hook` subprocesses so the web
	// UI can surface hook errors that would otherwise only land in stderr.
	// Successful hook runs are deliberately silent — only error/warn paths
	// hit this endpoint. Best-effort: if the session is gone, drop quietly.
	mux.HandleFunc("POST /api/sessions/{id}/hook-error", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		// Loopback-only: this endpoint exists for the local tool-hook
		// subprocess to ship error toasts to the UI. Server may bind
		// 0.0.0.0 (LAN access for the web UI), and a leaked bearer token
		// would otherwise let any remote holder inject fake hook_error
		// events into any session. Reject anything that didn't originate
		// from this host.
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			http.Error(w, "loopback only", http.StatusForbidden)
			return
		}
		// No rate limiting for hook-error: it's loopback-only diagnostic traffic.
		// Sharing the main rl would suppress error reports during legitimate bursts.
		id := r.PathValue("id")
		var req struct {
			RuleID   string `json:"rule_id"`
			Event    string `json:"event"`
			ToolName string `json:"tool_name"`
			Level    string `json:"level"`
			Msg      string `json:"msg"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Msg == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing msg"})
			return
		}
		if req.Level != "warn" && req.Level != "info" {
			req.Level = "error"
		}
		sess := sm.getSession(id)
		if sess == nil {
			// Race with session GC / unknown weiran ID — already in stderr.
			writeJSON(w, http.StatusOK, map[string]string{"status": "session_unknown"})
			return
		}
		payload, _ := json.Marshal(map[string]any{
			"rule_id":   req.RuleID,
			"event":     req.Event,
			"tool_name": req.ToolName,
			"level":     req.Level,
			"msg":       req.Msg,
			"ts":        time.Now().Unix(),
		})
		// broadcast → SSE subscribers + auto-fanout to WS hub via server_stream.go:110
		sess.broadcaster.broadcast(sseEvent{Event: "hook_error", Data: payload})
		writeJSON(w, http.StatusOK, map[string]string{"status": "delivered"})
	}))

	// Answer an in-flight AskUserQuestion permission request from the Web UI.
	// Body: {"request_id": "req_...", "answers": [{"question": "...", "answer": "A. label"}, ...]}
	// The server merges the user's answers into the original updatedInput and
	// sends a control_response with behavior=allow, which unblocks the claude
	// subprocess (it was waiting on stdin for a can_use_tool decision).
	//
	// For cancellation, set "cancelled": true — server replies with
	// behavior=deny so claude marks the tool_use as user-rejected.
	mux.HandleFunc("POST /api/sessions/{id}/answer-question", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow() {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
			return
		}
		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		var req struct {
			RequestID string `json:"request_id"`
			Answers   []struct {
				Question string `json:"question"`
				Answer   string `json:"answer"`
			} `json:"answers"`
			Cancelled bool `json:"cancelled,omitempty"`
			// Plan, when set, overrides tool_input.plan for plan_approval kind.
			// Lets the Web UI edit the plan content before approving (parity
			// with Claude Code TUI's edit-then-approve flow).
			Plan string `json:"plan,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
			return
		}
		if req.RequestID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request_id is required"})
			return
		}
		// ExitPlanMode plan body has no business being multi-megabyte.
		// Cap defensively before persisting / forwarding to claude. 64KB is
		// >> any reasonable plan and < the JSONL line size we want to keep
		// the transcript readable. Anything over this is almost certainly a
		// bug or abuse.
		const maxPlanBytes = 64 * 1024
		if len(req.Plan) > maxPlanBytes {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "plan body too large (max 64KB)"})
			return
		}
		if !sess.process.alive() {
			writeJSON(w, http.StatusGone, map[string]string{"error": "session process has exited"})
			return
		}

		entry := sess.takePendingAUQ(req.RequestID)
		if entry == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no pending question for that request_id (already answered or expired)"})
			return
		}

		sess.touch()
		sess.setStatus("running")

		var decision map[string]any
		switch entry.Kind {
		case "plan_approval":
			// ExitPlanMode plan approval. The frontend sends:
			//   answers[0].answer = "approve" | "reject"
			//   answers[0].question = feedback text (optional)
			isDeny := req.Cancelled
			feedback := ""
			if len(req.Answers) > 0 {
				ans := strings.TrimSpace(req.Answers[0].Answer)
				feedback = strings.TrimSpace(req.Answers[0].Question) // repurposed as feedback
				if ans == "reject" {
					isDeny = true
				}
			}
			if isDeny {
				msg := "User rejected the plan from the Web UI."
				if feedback != "" {
					msg = "User rejected the plan with feedback: " + feedback
				}
				decision = map[string]any{
					"behavior": "deny",
					"message":  msg,
				}
			} else {
				// Pass original tool_input through (contains plan injected by
				// normalizeToolInput). If the Web UI submitted an edited plan
				// body, override input.plan so Claude Code's ExitPlanMode
				// call() sees the user's revisions instead of the model's
				// original draft. Mirrors TUI's edit-then-approve flow.
				var orig any = map[string]any{}
				if len(entry.Input) > 0 {
					if err := json.Unmarshal(entry.Input, &orig); err != nil {
						writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "corrupt pending input: " + err.Error()})
						return
					}
				}
				if req.Plan != "" {
					if m, ok := orig.(map[string]any); ok {
						m["plan"] = req.Plan
						orig = m
					}
				}
				decision = map[string]any{
					"behavior":     "allow",
					"updatedInput": orig,
				}
			}
		case "permission":
			// Generic tool permission prompt (Write, Edit, …). The Web UI
			// synthesized a Yes/No AUQ — we only care which answer came
			// back, not the question text. Option A is always Yes, option
			// B is always No (see makeOnCanUseTool).
			isDeny := req.Cancelled
			if !isDeny && len(req.Answers) > 0 {
				ans := strings.TrimSpace(req.Answers[0].Answer)
				// Frontend sends "A. <label>" / "B. <label>". Treat B or
				// anything starting with "No" as deny; everything else
				// (A, empty, unexpected) as allow — biased toward letting
				// the turn continue.
				if strings.HasPrefix(ans, "B.") || strings.HasPrefix(strings.ToLower(ans), "no") {
					isDeny = true
				}
			}
			if isDeny {
				decision = map[string]any{
					"behavior": "deny",
					"message":  "User declined this tool call from the Web UI.",
				}
			} else {
				// Pass original tool_input through unchanged. Use RawMessage
				// so we don't silently reshape the model's arguments.
				var orig any = map[string]any{}
				if len(entry.Input) > 0 {
					if err := json.Unmarshal(entry.Input, &orig); err != nil {
						writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "corrupt pending input: " + err.Error()})
						return
					}
				}
				decision = map[string]any{
					"behavior":     "allow",
					"updatedInput": orig,
				}
			}
		default:
			// AskUserQuestion tool (Kind="auq" or legacy empty).
			if req.Cancelled {
				decision = map[string]any{
					"behavior": "deny",
					"message":  "User cancelled the questions.",
				}
			} else {
				// Convert the answers list into AskUserQuestion's expected shape:
				// `answers` is a Record<questionText, answerString>. Multi-select
				// answers are already comma-joined by the frontend.
				answersMap := make(map[string]string, len(req.Answers))
				for _, a := range req.Answers {
					if a.Question == "" {
						continue
					}
					answersMap[a.Question] = a.Answer
				}
				// Merge answers into the original input (preserve questions + any
				// other fields the model supplied).
				merged := map[string]any{}
				if len(entry.Input) > 0 {
					if err := json.Unmarshal(entry.Input, &merged); err != nil {
						writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "corrupt pending input: " + err.Error()})
						return
					}
				}
				merged["answers"] = answersMap
				decision = map[string]any{
					"behavior":     "allow",
					"updatedInput": merged,
				}
			}
		}

		if err := sess.process.sendPermissionDecision(req.RequestID, decision); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
	}))

	// Aggregate UI state for a session: pending AskUserQuestion, running tasks,
	// todos. The Web UI fetches this on page load to make refresh idempotent —
	// SSE replays the message stream, this endpoint replays the panel state.
	mux.HandleFunc("GET /api/sessions/{id}/state", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		sess.mu.Lock()
		var todos json.RawMessage
		if len(sess.Todos) > 0 {
			todos = make(json.RawMessage, len(sess.Todos))
			copy(todos, sess.Todos)
		}
		sess.mu.Unlock()
		state := map[string]any{
			"pending_auq":   sess.snapshotPendingAUQ(),
			"running_tasks": sess.tasks.snapshot(),
		}
		if todos != nil {
			state["todos"] = todos
		}
		writeJSON(w, http.StatusOK, state)
	}))

	// Read a tool output file (Bash run_in_background, Agent transcripts, Monitor
	// captures). Whitelisted to /tmp/claude-* and /private/tmp/claude-* with no
	// path traversal. Returns the last 64 KiB of the file as plain text.
	mux.HandleFunc("GET /api/files/read", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.Query().Get("path")
		if raw == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is required"})
			return
		}
		clean := filepath.Clean(raw)
		// Whitelist: only files under Claude Code's per-pid temp roots.
		allowed := false
		for _, prefix := range []string{"/tmp/claude-", "/private/tmp/claude-", "/var/folders/"} {
			if strings.HasPrefix(clean, prefix) {
				allowed = true
				break
			}
		}
		if !allowed {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "path not in whitelist"})
			return
		}
		fi, err := os.Stat(clean)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		if fi.IsDir() {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is a directory"})
			return
		}
		f, err := os.Open(clean)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		defer f.Close()
		const maxRead = 64 * 1024
		size := fi.Size()
		var offset int64
		if size > maxRead {
			offset = size - maxRead
			_, _ = f.Seek(offset, 0)
		}
		buf := make([]byte, maxRead)
		n, _ := f.Read(buf)
		writeJSON(w, http.StatusOK, map[string]any{
			"path":      clean,
			"size":      size,
			"offset":    offset,
			"truncated": size > maxRead,
			"content":   string(buf[:n]),
		})
	}))

	// IPC: send message from one session to another
	mux.HandleFunc("POST /api/sessions/{id}/message-from", authMiddleware(cfg.Token,
		handleIPCMessageFrom(sm, hub, cfg.MaxInteractionRounds)))

	// IPC: query interaction count between two sessions
	mux.HandleFunc("GET /api/sessions/{id}/interaction-count", authMiddleware(cfg.Token,
		handleIPCInteractionCount()))

	// Wait for session to reach idle/stopped/error state
	mux.HandleFunc("GET /api/sessions/{id}/wait", authMiddleware(cfg.Token,
		handleSessionWait(sm)))

	// Upload file to session (multipart)
	mux.HandleFunc("POST /api/sessions/{id}/upload", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow() {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
			return
		}

		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}

		// Parse multipart form — 32 MB max
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to parse multipart form: " + err.Error()})
			return
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file field is required"})
			return
		}
		defer file.Close()

		// All file types allowed — single-user authenticated upload, files are
		// served back from /uploads/ behind the same auth middleware. The
		// 32 MB ParseMultipartForm cap above is the only size guard.
		ext := strings.ToLower(filepath.Ext(header.Filename))

		// Ensure content-type matches
		ct := header.Header.Get("Content-Type")
		if ct == "" {
			ct = mime.TypeByExtension(ext)
		}

		// Generate unique filename: timestamp-random.ext
		randBytes := make([]byte, 8)
		rand.Read(randBytes)
		filename := fmt.Sprintf("%d-%s%s", time.Now().UnixMilli(), hex.EncodeToString(randBytes), ext)

		// Save to workspace/uploads/
		uploadsDir := filepath.Join(workspace, "uploads")
		if err := os.MkdirAll(uploadsDir, 0755); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create uploads dir: " + err.Error()})
			return
		}

		destPath := filepath.Join(uploadsDir, filename)
		dst, err := os.Create(destPath)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create file: " + err.Error()})
			return
		}
		defer dst.Close()

		written, err := io.Copy(dst, file)
		if err != nil {
			os.Remove(destPath)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save file: " + err.Error()})
			return
		}

		// Build URL path for the uploaded file
		urlPath := "/uploads/" + filename

		// Best-effort S3 upload: if configured, upload to S3 and return public URL
		s3URL := uploadToS3(cfg.S3, destPath, filename, ct)
		if s3URL != "" {
			urlPath = s3URL
		}

		fmt.Fprintf(os.Stderr, "[%s] server: uploaded %s (%d bytes) for session %s\n",
			appName, filename, written, sess.ID)

		writeJSON(w, http.StatusOK, map[string]any{
			"url":           urlPath,
			"filename":      filename,
			"original_name": header.Filename,
			"size":          written,
			"content_type":  ct,
		})
	}))

	// Voice transcription (upload audio → whisper-cpp → transcript)
	mux.HandleFunc("POST /api/sessions/{id}/voice", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow() {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
			return
		}

		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}

		// Parse multipart form — 32 MB max
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to parse form: " + err.Error()})
			return
		}

		file, _, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file field is required"})
			return
		}
		defer file.Close()

		// Save to temp file
		ts := time.Now().UnixMilli()
		tmpInput := fmt.Sprintf("/tmp/webui-voice-%d.webm", ts)
		dst, err := os.Create(tmpInput)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create temp file"})
			return
		}
		if _, err := io.Copy(dst, file); err != nil {
			dst.Close()
			os.Remove(tmpInput)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save audio"})
			return
		}
		dst.Close()
		defer os.Remove(tmpInput)

		// Convert to WAV via ffmpeg
		tmpWAV := fmt.Sprintf("/tmp/webui-voice-%d.wav", ts)
		ffCtx, ffCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer ffCancel()
		if out, err := exec.CommandContext(ffCtx, "ffmpeg", "-y", "-i", tmpInput, "-ar", "16000", "-ac", "1", "-f", "wav", tmpWAV).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] voice: ffmpeg failed: %v\n%s\n", appName, err, out)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "audio conversion failed"})
			return
		}
		defer os.Remove(tmpWAV)

		// Transcribe via whisper-cpp (reuse existing function from server_telegram.go)
		transcript := transcribeAudio(tmpWAV, 0) // duration 0 = use default timeout
		if transcript == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "transcription failed or audio was empty"})
			return
		}

		fmt.Fprintf(os.Stderr, "[%s] voice: transcribed %d chars for session %s\n", appName, len(transcript), sess.ID)
		writeJSON(w, http.StatusOK, map[string]string{"transcript": transcript})
	}))

	// SSE stream
	mux.HandleFunc("GET /api/sessions/{id}/stream", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		serveSSE(w, r, sess.broadcaster)
	}))

	// Control request (interrupt, set_model, etc.)
	mux.HandleFunc("POST /api/sessions/{id}/control", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}

		var req struct {
			Subtype string         `json:"subtype"`
			Extra   map[string]any `json:"extra"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Subtype == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "subtype is required"})
			return
		}

		if err := sess.process.controlRequest(req.Subtype, req.Extra); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		// Persist apply_flag_settings.effort so it survives CC reloads
		// (resume / rehydrate / model-switch). Without persistence the next
		// CC subprocess would fall back to its built-in default and the
		// user-picked tier silently disappears.
		if req.Subtype == "apply_flag_settings" {
			if settings, ok := req.Extra["settings"].(map[string]any); ok {
				if eff, ok := settings["effort"].(string); ok {
					sess.mu.Lock()
					sess.Effort = eff
					sess.mu.Unlock()
					setEffortDB(sess.ID, eff)
				}
			}
		}

		writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
	}))

	// Toggle chrome (--chrome flag) — reloads the underlying claude proc.
	mux.HandleFunc("POST /api/sessions/{id}/chrome", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		if err := sm.setChrome(sess.ID, req.Enabled); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "chrome_enabled": req.Enabled})
	}))

	// Toggle 本我模式 (replace-soul) — reloads the underlying claude proc
	// with --system-prompt-file instead of --append-system-prompt-file.
	// Strips CC's native system prompt, leaving only the soul. Server-side
	// Anthropic safety blocks (injection defense, privacy, copyright) are
	// NOT affected — they're baked into the API. Mid-session toggling
	// causes the model's "identity" to shift for the remainder of the
	// conversation; the UI warns about this.
	mux.HandleFunc("POST /api/sessions/{id}/replace-soul", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		if err := sm.setReplaceSoul(sess.ID, req.Enabled); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "replace_soul": req.Enabled})
	}))

	// Switch session mode — reloads the underlying claude proc in one of
	//   weiran (default): --append-system-prompt-file <soul> (CC harness + SOUL)
	//   benwo (本我):     --system-prompt-file <soul>         (SOUL replaces CC)
	//   cc (bare):        no soul prompt at all               (pure Claude Code)
	// Session state (conversation history via resume, name, chrome, todos) is
	// preserved across the reload. Switching to/from cc shifts identity much
	// more than append↔replace, so the UI should warn.
	mux.HandleFunc("POST /api/sessions/{id}/mode", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		var req struct {
			Mode string `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Mode == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "mode is required (weiran|benwo|cc)"})
			return
		}
		if err := sm.setMode(sess.ID, req.Mode); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "mode": req.Mode})
	}))

	// Switch model — respawns the claude process with a new model while
	// preserving session state (conversation history, replace_soul, chrome).
	mux.HandleFunc("POST /api/sessions/{id}/set-model", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model is required"})
			return
		}
		if err := sm.setModel(sess.ID, req.Model); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "model": req.Model})
	}))

	// Context usage — fire control request, response comes via SSE
	mux.HandleFunc("POST /api/sessions/{id}/usage", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		sess := sm.getSession(r.PathValue("id"))
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}

		if err := sess.process.controlRequest("get_context_usage", nil); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
	}))

	// Historical sessions (from JSONL files, for resume)
	// ?category=interactive — filter by category (looks up server_sessions DB)
	// ?category=all — show all including heartbeat/cron (default: interactive only)
	mux.HandleFunc("GET /api/history", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		limit := 30
		if l := r.URL.Query().Get("limit"); l != "" {
			if v, err := strconv.Atoi(l); err == nil && v > 0 && v <= 100 {
				limit = v
			}
		}
		catFilter := r.URL.Query().Get("category")
		if catFilter == "" {
			catFilter = CategoryInteractive // default: hide ephemeral
		}

		all := scanAllSessions()
		sort.Slice(all, func(i, j int) bool { return all[i].ModTime.After(all[j].ModTime) })

		// Build spawn agent lookup: session_name → agent_id
		spawnAgentMap := loadSpawnAgentMap()

		// Collect IDs for batch DB query
		sessionIDs := make([]string, len(all))
		for i, s := range all {
			sessionIDs[i] = s.ID
		}
		// Batch-load session meta from DB (category, model, cost) — single WHERE IN query
		metaMap := batchLoadSessionMeta(sessionIDs)

		// Convert to JSON-friendly format, applying category filter
		items := make([]map[string]any, 0, len(all))
		for _, s := range all {
			// Look up meta from batch (by claude_session_id which == JSONL s.ID)
			meta := metaMap[s.ID]

			cat := meta.Category
			if cat == "" {
				cat = inferCategoryFromName(s.Name, s.FirstMsg)
			}

			// Filter by category (unless "all" is requested)
			if catFilter != "all" && cat != catFilter {
				continue
			}

			if len(items) >= limit {
				break
			}

			// Determine agent: DB record > spawn name match > default "main" for server sessions
			agent := getSessionAgent(s.ID)
			if agent == "" {
				if a, ok := spawnAgentMap[s.Name]; ok {
					agent = a
				}
			}

			// Prefer DB model (contains [1m] suffix) over JSONL-scanned model
			model := meta.Model
			if model == "" {
				model = s.Model
			}

			// Cost: try live proxy first, then persisted from batch
			cost := float64(0)
			if meta.WeiranSID != "" {
				cost = getSessionProxyCost(meta.WeiranSID)
			}
			if cost <= 0 {
				cost = meta.Cost
			}

			item := map[string]any{
				"id":             s.ID,
				"name":           s.Name,
				"title":          s.Title,
				"project":        s.Project,
				"model":          model,
				"category":       cat,
				"agent":          agent,
				"first_msg":      s.FirstMsg,
				"summary":        s.Summary,
				"size":           s.Size,
				"messages":       s.Messages,
				"mod_time":       s.ModTime.Format(time.RFC3339),
				"proxy_cost_usd": cost,
			}
			// Enrich with DB fields: user_turns, created_at, updated_at
			if meta.UserTurns > 0 {
				item["user_turns"] = meta.UserTurns
			}
			if meta.CreatedAt != "" {
				item["created_at"] = meta.CreatedAt
			}
			if meta.UpdatedAt != "" {
				item["updated_at"] = meta.UpdatedAt
			}
			items = append(items, item)
		}
		writeJSON(w, http.StatusOK, items)
	}))

	// Get messages from a historical session JSONL file
	mux.HandleFunc("GET /api/history/{id}/messages", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.PathValue("id")

		// Find the JSONL file — try direct ID first, then check active sessions
		// for ClaudeSID or ResumedFrom mapping
		path := findSessionJSONL(sessionID)
		if path == "" {
			if sess := sm.getSession(sessionID); sess != nil {
				if sess.ClaudeSID != "" {
					path = findSessionJSONL(sess.ClaudeSID)
				}
				if path == "" && sess.ResumedFrom != "" {
					path = findSessionJSONL(sess.ResumedFrom)
				}
			}
		}
		if path == "" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session file not found"})
			return
		}

		limit := 200
		if l := r.URL.Query().Get("limit"); l != "" {
			if v, err := strconv.Atoi(l); err == nil && v > 0 && v <= 1000 {
				limit = v
			}
		}

		messages := parseSessionMessages(path, limit)
		writeJSON(w, http.StatusOK, messages)
	}))

	// Native Claude Code Agent subagent backfill: scan the full JSONL and
	// rebuild lifecycle of every Agent (Task) tool call, so the Web UI's
	// subagent drawer can restore state after reload / session switch.
	mux.HandleFunc("GET /api/sessions/{id}/subagents", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.PathValue("id")
		path := findSessionJSONL(sessionID)
		if path == "" {
			if sess := sm.getSession(sessionID); sess != nil {
				if sess.ClaudeSID != "" {
					path = findSessionJSONL(sess.ClaudeSID)
				}
				if path == "" && sess.ResumedFrom != "" {
					path = findSessionJSONL(sess.ResumedFrom)
				}
			}
		}
		if path == "" {
			writeJSON(w, http.StatusOK, []subagentRecord{})
			return
		}
		writeJSON(w, http.StatusOK, parseSessionSubagents(path))
	}))

	// Resume a historical session (creates a server session wrapping --resume)
	mux.HandleFunc("POST /api/sessions/resume", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow() {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
			return
		}

		var req struct {
			SessionID   string `json:"session_id"`
			Message     string `json:"message"`
			Name        string `json:"name"`
			Category    string `json:"category"`
			Model       string `json:"model"`
			ReplaceSoul *bool  `json:"replace_soul"` // nil → inherit persisted DB flag
			SoulFiles   *bool  `json:"soul_files"`   // nil → inherit persisted DB flag
			Mode        string `json:"mode"`         // optional: "weiran"/"benwo"/"cc" — overrides above
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SessionID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session_id is required"})
			return
		}
		// Mode takes precedence if provided
		if req.Mode != "" {
			if soulFlag, replaceFlag, ok := modeToFlags(req.Mode); ok {
				req.SoulFiles = &soulFlag
				req.ReplaceSoul = &replaceFlag
			}
		}

		sess, err := sm.resumeSession(req.SessionID, req.Message, req.Name, req.Category, req.Model, req.ReplaceSoul, req.SoulFiles)
		if err != nil {
			status := http.StatusInternalServerError
			if strings.Contains(err.Error(), "max sessions") {
				status = http.StatusServiceUnavailable
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}

		writeJSON(w, http.StatusCreated, sess.snapshot())
	}))

	// GAL save endpoints
	mux.HandleFunc("GET /api/gal", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		galDir := filepath.Join(workspace, "memory", "gal")
		entries, err := os.ReadDir(galDir)
		if err != nil {
			writeJSON(w, http.StatusOK, []any{})
			return
		}
		var saves []map[string]any
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(galDir, e.Name()))
			if err != nil {
				continue
			}
			var save map[string]any
			if json.Unmarshal(data, &save) == nil {
				saves = append(saves, save)
			}
		}
		if saves == nil {
			saves = []map[string]any{}
		}
		writeJSON(w, http.StatusOK, saves)
	}))

	mux.HandleFunc("GET /api/gal/{id}", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		galPath := filepath.Join(workspace, "memory", "gal", id+".json")
		data, err := os.ReadFile(galPath)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "save not found"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	}))

	// Wake API — trigger a heartbeat session (replaces OpenClaw /hooks/wake)
	// Body: {"text": "reason", "soul": true/false}
	// soul=true (default) → full soul prompt; soul=false → bare claude, lighter context
	// No auth required — Jira calls this from Docker container
	mux.HandleFunc("POST /api/wake", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Text  string `json:"text"`
			Soul  *bool  `json:"soul"`  // pointer to distinguish missing from false
			Model string `json:"model"` // override model (e.g. from defaultModel via delegateToServer)
		}
		json.NewDecoder(r.Body).Decode(&req) // optional body, ignore errors

		reason := req.Text
		if reason == "" {
			reason = "jira-wake"
		}

		// Dedup: reject if a heartbeat/cron/evolve session is already running
		sm.mu.RLock()
		for _, s := range sm.sessions {
			if (s.Category == CategoryHeartbeat || s.Category == CategoryCron || s.Category == CategoryEvolve) &&
				s.process != nil && s.process.alive() {
				sm.mu.RUnlock()
				fmt.Fprintf(os.Stderr, "[%s] server: wake rejected — %s session %s already running\n",
					appName, s.Category, shortID(s.ID))
				writeJSON(w, http.StatusConflict, map[string]string{
					"error":    fmt.Sprintf("%s session already running", s.Category),
					"session":  s.ID,
					"category": s.Category,
				})
				return
			}
		}
		sm.mu.RUnlock()

		// Default: soul enabled. Caller can explicitly disable.
		soulEnabled := true
		if req.Soul != nil {
			soulEnabled = *req.Soul
		}

		// Build heartbeat task
		hb := heartbeatTask()
		taskMsg := hb
		if reason != "jira-wake" {
			taskMsg = fmt.Sprintf("Context: %s\n\n%s", reason, hb)
		}

		// Soul session lifecycle: compact check → resume or create
		endStaleSoulSessions()

		// Auto-compact: if rounds exceeded, end old session and start fresh
		if shouldCompactSoulSession(agentName, soulSessionMaxRounds) {
			fmt.Fprintf(os.Stderr, "[%s] server: soul session hit %d rounds, compacting (daily notes carry context forward)\n",
				appName, soulSessionMaxRounds)
			endSoulSession(agentName, fmt.Sprintf("context compaction (rounds >= %d)", soulSessionMaxRounds))
		}

		claudeResumeID := getActiveSoulSession(agentName)
		soulSessionID := getActiveSoulSessionID(agentName)
		if claudeResumeID != "" {
			fmt.Fprintf(os.Stderr, "[%s] server: wake resuming soul session (claude %s)\n", appName, shortID(claudeResumeID))
		} else {
			// No active soul session — create a new daily one
			soulSessionID = createSoulSession(agentName, "daily", 2.0)
		}

		// Apply model: request > defaultModel > empty (upstream default)
		wakeModel := req.Model
		if wakeModel == "" && defaultModel != "" {
			wakeModel = defaultModel
		}

		sessName := fmt.Sprintf("heartbeat-%s", time.Now().Format("0102-1504"))
		sess, err := sm.createSessionWithOpts(sessionCreateOpts{
			Name:     sessName,
			Project:  workspace,
			Model:    wakeModel,
			Soul:     soulEnabled,
			Category: CategoryHeartbeat,
			Tags:     []string{"auto"},
			ResumeID: claudeResumeID,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] server: wake failed to create session: %v\n", appName, err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}

		// Set model fallback chain for rate_limit auto-retry (non-interactive only)
		if len(defaultModelFallbacks) > 0 {
			sess.mu.Lock()
			sess.fallbackModels = append([]string{}, defaultModelFallbacks...)
			sess.taskMessage = taskMsg
			sess.sessionMgr = sm
			sess.mu.Unlock()
		}

		// Link or touch soul session once Claude session ID is available
		if soulSessionID > 0 {
			go func(soulID int64, s *serverSession) {
				// Wait for init (which sets ClaudeSID) instead of polling
				if !s.process.waitInit(30 * time.Second) {
					return
				}
				s.mu.Lock()
				cid := s.ClaudeSID
				s.mu.Unlock()
				if cid != "" {
					linkSoulSession(soulID, cid)
				}
			}(soulSessionID, sess)
		}

		// Send heartbeat task as initial message — wait for init before writing to stdin
		go func() {
			if !sess.awaitBackendReady(30 * time.Second) {
				return
			}
			if err := sess.process.sendMessage(taskMsg); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] server: wake failed to send task: %v\n", appName, err)
			}
		}()

		// Track rounds for auto-compact
		incrementSoulSessionRounds(agentName)

		soulLabel := "with soul"
		if !soulEnabled {
			soulLabel = "no soul"
		}
		resumeLabel := ""
		if claudeResumeID != "" {
			resumeLabel = fmt.Sprintf(" [resume %s]", shortID(claudeResumeID))
		}
		fmt.Fprintf(os.Stderr, "[%s] server: wake triggered: %s (%s, session %s%s)\n", appName, reason, soulLabel, shortID(sess.ID), resumeLabel)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"session_id": sess.ID,
			"reason":     reason,
			"soul":       soulEnabled,
			"resumed":    claudeResumeID != "",
		})
	})

	// Spawn API — dispatch a task to any agent
	// Body: {"agent": "intern", "task": "处理 #815", "wait": false}
	// No auth required — external task systems and the agent itself both call this
	mux.HandleFunc("POST /api/spawn", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Agent string `json:"agent"`
			Task  string `json:"task"`
			Wait  bool   `json:"wait"`
			Self  bool   `json:"self"`
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Agent == "" || req.Task == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent and task are required"})
			return
		}

		agents, err := loadAgents()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "load agents: " + err.Error()})
			return
		}

		agent := findAgent(agents, req.Agent)
		if agent == nil {
			available := []string{}
			for _, a := range agents {
				available = append(available, a.ID)
			}
			writeJSON(w, http.StatusNotFound, map[string]any{
				"error":     "agent not found: " + req.Agent,
				"available": available,
			})
			return
		}

		if agent.ID == "main" && !req.Self {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "spawning main agent requires self:true"})
			return
		}

		// --self --model override
		if req.Self && req.Model != "" {
			if resolved, resolveErr := resolveFuzzyModel(req.Model); resolveErr == nil && resolved != "" {
				agent.Model = resolved
			} else {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("model %q could not be resolved", req.Model)})
				return
			}
		}

		// Per-agent mutual exclusion
		if running := agentRunningSpawn(agent.ID); running != nil {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":      fmt.Sprintf("agent %s already has a running spawn", agent.ID),
				"spawn_id":   running.id,
				"task":       running.task,
				"started_at": running.started,
			})
			return
		}

		// Build prompt and spawn
		promptContent := buildAgentPrompt(agent)
		tmpDir, err := os.MkdirTemp("", fmt.Sprintf("%s-spawn-%s-", appName, agent.ID))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "create temp dir: " + err.Error()})
			return
		}
		promptFile := filepath.Join(tmpDir, "prompt.md")
		os.WriteFile(promptFile, []byte(promptContent), 0600)

		sessionName := fmt.Sprintf("spawn-%s-%s", agent.ID, time.Now().Format("0102-1504"))
		spawnID := recordSpawnStart(agent, req.Task, sessionName, tmpDir)

		claudeArgs := []string{
			"--append-system-prompt-file", promptFile,
			"--dangerously-skip-permissions",
			"--name", sessionName,
			"-p", req.Task,
		}
		if agent.Model != "" {
			claudeArgs = append(claudeArgs, "--model", agent.Model)
		}

		cmd := exec.Command(claudeBin, claudeArgs...)
		cmd.Dir = agent.Workspace
		cmd.Env = injectProxyEnv(os.Environ())

		// Set JIRA_TOKEN
		jiraTokenFile := filepath.Join(agent.Workspace, ".jira-token")
		if data, err := os.ReadFile(jiraTokenFile); err == nil {
			if token := strings.TrimSpace(string(data)); token != "" {
				cmd.Env = append(cmd.Env, "JIRA_TOKEN="+token)
			}
		}

		// Async: double-fork via wrapper script
		logFile := filepath.Join(tmpDir, "output.log")
		pidFile := filepath.Join(tmpDir, "pid")
		exitFile := filepath.Join(tmpDir, "exit")
		notifyBin, _ := exec.LookPath(os.Args[0])
		if notifyBin == "" {
			notifyBin = os.Args[0]
		}

		// Shell-escape values interpolated into the wrapper script's double-quoted
		// notify arguments to prevent injection via agent.Name / sessionName.
		safeAgentName := shellEscapeForDoubleQuote(agent.Name)
		safeSessionName := shellEscapeForDoubleQuote(sessionName)
		safeLogFile := shellEscapeForDoubleQuote(logFile)
		wrapperScript := fmt.Sprintf("#!/bin/sh\necho $$ > %q\n%s > %q 2>&1\nEXIT_CODE=$?\necho \"$EXIT_CODE\" > %q\nDURATION=$SECONDS\n%s spawn finish %d $EXIT_CODE $DURATION %q 2>/dev/null\nif [ \"$EXIT_CODE\" -eq 0 ]; then\n  %s notify \"✅ spawn %s 完成 (${DURATION}s)\\nsession: %s\"\nelse\n  %s notify \"❌ spawn %s 失败 (exit=$EXIT_CODE, ${DURATION}s)\\nsession: %s\\nlog: %s\"\nfi\n",
			pidFile,
			shellQuoteArgs(cmd.Path, cmd.Args[1:]...), logFile,
			exitFile,
			notifyBin, spawnID, logFile,
			notifyBin, safeAgentName, safeSessionName,
			notifyBin, safeAgentName, safeSessionName, safeLogFile,
		)

		wrapperPath := filepath.Join(tmpDir, "run.sh")
		os.WriteFile(wrapperPath, []byte(wrapperScript), 0700)

		bgCmd := exec.Command("/bin/sh", wrapperPath)
		bgCmd.Dir = agent.Workspace
		bgCmd.Env = cmd.Env
		bgCmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		bgCmd.Stdin = nil
		bgCmd.Stdout = nil
		bgCmd.Stderr = nil

		if err := bgCmd.Start(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "spawn failed: " + err.Error()})
			return
		}
		bgCmd.Process.Release()

		fmt.Fprintf(os.Stderr, "[%s] server: spawned %s (%s) for task: %s\n",
			appName, agent.Name, agent.ID, truncate(req.Task, 80))

		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"spawn_id": spawnID,
			"agent":    agent.ID,
			"session":  sessionName,
			"log":      logFile,
		})
	})

	// ── Prepare Restart (rehydration marker) ──
	// Called by a session before triggering `make server-restart`.
	// Marks itself as the restart initiator so it gets woken after rehydration.
	mux.HandleFunc("POST /api/server/prepare-restart", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SessionID string `json:"session_id"` // weiran session ID of the caller
			Message   string `json:"message"`    // message to send after rehydration
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SessionID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session_id is required"})
			return
		}
		if req.Message == "" {
			req.Message = "Server restarted successfully. Continue from where you left off."
		}

		// Verify session exists
		sess := sm.getSession(req.SessionID)
		if sess == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}

		// Ensure claude_session_id is persisted (needed for resume after restart)
		sess.mu.Lock()
		claudeSID := sess.ClaudeSID
		sess.mu.Unlock()
		if claudeSID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session has no claude_session_id yet"})
			return
		}
		setClaudeSessionID(req.SessionID, claudeSID)
		setRehydrateMessage(req.SessionID, req.Message)

		fmt.Fprintf(os.Stderr, "[%s] server: prepare-restart: session %s marked for rehydration\n",
			appName, shortID(req.SessionID))

		writeJSON(w, http.StatusOK, map[string]string{
			"status":            "prepared",
			"session_id":        req.SessionID,
			"claude_session_id": claudeSID,
		})
	}))

	// ── OAuth Token Bridge ──
	// GET /api/oauth/token returns the current Claude Code OAuth access_token,
	// refreshing proactively if near expiry. Intended for SSH-launched CLI
	// clients (`weiran token`, shell-aliased `claude`) that cannot reach the
	// macOS login keychain directly. The server process itself runs under
	// launchd and *does* have keychain access, so it acts as a bridge.
	//
	// Response: {"accessToken":"sk-ant-oat01-...","expiresAt":<unix-ms>}
	// 503 if keychain is unreachable or empty (user hasn't run `claude login`).
	mux.HandleFunc("GET /api/oauth/token", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		creds := readClaudeKeychainCreds()
		if creds == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "keychain unreachable or empty; run `claude login` in a GUI session",
			})
			return
		}
		// Warm proactively if we're within the refresh threshold. ensureFreshClaudeCreds
		// is idempotent / debounced and takes ~3-5s max. Re-read keychain after warm
		// so we return the post-refresh token rather than the stale one.
		remaining := time.UnixMilli(creds.ExpiresAt).Sub(time.Now())
		if remaining < warmThreshold {
			ensureFreshClaudeCreds()
			if fresh := readClaudeKeychainCreds(); fresh != nil {
				creds = fresh
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"accessToken": creds.AccessToken,
			"expiresAt":   creds.ExpiresAt,
		})
	}))

	// WebSocket endpoint for bidirectional real-time sync
	mux.HandleFunc("GET /api/ws", hub.serveWS)

	// Serve uploaded files (authed)
	uploadsDir := filepath.Join(workspace, "uploads")
	mux.HandleFunc("GET /uploads/", authMiddleware(cfg.Token, func(w http.ResponseWriter, r *http.Request) {
		// Strip /uploads/ prefix and serve from uploadsDir
		name := strings.TrimPrefix(r.URL.Path, "/uploads/")
		if name == "" || strings.Contains(name, "..") {
			http.NotFound(w, r)
			return
		}
		filePath := filepath.Join(uploadsDir, name)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if isActiveUploadExt(filepath.Ext(name)) {
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(name)))
		}
		http.ServeFile(w, r, filePath)
	}))

	// Dynamic branding: derive UI title from binary name (e.g. "weiran" → "Weiran", "soul" → "Soul")
	appTitle := strings.ToUpper(appName[:1]) + appName[1:]
	appInitial := strings.ToUpper(appName[:1])

	// PWA assets (no auth — browser needs direct access during install)
	renderedManifest := strings.ReplaceAll(string(manifestJSON), "{{.AppTitle}}", appTitle)
	renderedManifestBytes := []byte(renderedManifest)
	mux.HandleFunc("GET /manifest.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/manifest+json")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(renderedManifestBytes)
	})
	mux.HandleFunc("GET /service-worker.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Cache-Control", "no-cache") // SW must not be cached
		w.Write(serviceWorkerJS)
	})
	iconsSubFS, _ := fs.Sub(iconsFS, "web/icons")
	mux.HandleFunc("GET /icons/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=604800")
		http.StripPrefix("/icons/", http.FileServerFS(iconsSubFS)).ServeHTTP(w, r)
	})

	// Static assets split out of index.html for modularity (see web/README or
	// commit log). CSS is large-ish (~1.6K LOC) and JS grows per feature, so
	// keep them cacheable and separately editable.
	cssSubFS, _ := fs.Sub(cssFS, "web/css")
	mux.HandleFunc("GET /css/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.StripPrefix("/css/", http.FileServerFS(cssSubFS)).ServeHTTP(w, r)
	})
	jsSubFS, _ := fs.Sub(jsFS, "web/js")
	mux.HandleFunc("GET /js/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.StripPrefix("/js/", http.FileServerFS(jsSubFS)).ServeHTTP(w, r)
	})

	// Serve UI — inject server config via string replacement (not text/template,
	// because the HTML contains JS/CSS with {{ }} that would break template parsing).
	// Sanitize config values to prevent JS/HTML injection from malformed config.
	safeModel := strings.NewReplacer(
		`\`, `\\`, `"`, `\"`, `'`, `\'`, `<`, `\x3c`, `>`, `\x3e`, `&`, `\x26`,
	).Replace(cfg.DefaultInteractiveModel)
	renderedIndex := string(indexHTML)
	replaceSoulChecked := ""
	if cfg.DefaultReplaceSoul {
		replaceSoulChecked = "checked"
	}
	renderedIndex = strings.ReplaceAll(renderedIndex, "{{.AppTitle}}", appTitle)
	renderedIndex = strings.ReplaceAll(renderedIndex, "{{.AppInitial}}", appInitial)
	renderedIndex = strings.ReplaceAll(renderedIndex, "{{.DefaultReplaceSoulChecked}}", replaceSoulChecked)
	renderedIndex = strings.ReplaceAll(renderedIndex, "{{.DefaultReplaceSoul}}", fmt.Sprintf("%t", cfg.DefaultReplaceSoul))
	renderedIndex = strings.ReplaceAll(renderedIndex, "{{.DefaultInteractiveModel}}", safeModel)
	// Tmux drawer hide: when disabled, inject a CSS rule that removes the
	// tab + drawer from the layout (display:none). Also exposes the flag
	// to JS via SERVER_CONFIG so drawer-tmux.js can short-circuit polling.
	tmuxDrawerHideCSS := ""
	if cfg.DisableTmuxDrawer {
		tmuxDrawerHideCSS = ".tmux-tab,#tmux-drawer{display:none!important}"
	}
	renderedIndex = strings.ReplaceAll(renderedIndex, "{{.TmuxDrawerHideCSS}}", tmuxDrawerHideCSS)
	renderedIndex = strings.ReplaceAll(renderedIndex, "{{.DisableTmuxDrawer}}", fmt.Sprintf("%t", cfg.DisableTmuxDrawer))
	// Alternate backend availability — drives the New Session sheet's Backend
	// radio group. The selector appears when at least one alternate backend is
	// enabled; individual backend choices are hidden independently.
	renderedIndex = strings.ReplaceAll(renderedIndex, "{{.CodexEnabled}}", fmt.Sprintf("%t", codexEnabled))
	renderedIndex = strings.ReplaceAll(renderedIndex, "{{.GrokEnabled}}", fmt.Sprintf("%t", grokEnabled))
	renderedIndex = strings.ReplaceAll(renderedIndex, "{{.KimiEnabled}}", fmt.Sprintf("%t", kimiEnabled))
	renderedIndex = strings.ReplaceAll(renderedIndex, "{{.AgyEnabled}}", fmt.Sprintf("%t", agyEnabled))
	renderedIndexBytes := []byte(renderedIndex)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(renderedIndexBytes)
	})

	// Start Telegram bot if enabled
	var tgBridge *telegramBridge
	if cfg.Telegram.Enabled {
		tgToken := getTelegramToken()
		if tgToken == "" {
			fmt.Fprintf(os.Stderr, "[%s] server: telegram enabled but no bot token found\n", appName)
		} else {
			allowedIDs := cfg.Telegram.AllowedChatIDs
			if len(allowedIDs) == 0 && tgChatID != "" {
				allowedIDs = []string{tgChatID} // fallback to global chat ID
			}
			tgBridge = newTelegramBridge(tgToken, allowedIDs, sm, hub)
			tgBridge.start()
		}
	}

	// Register proxy usage API endpoint
	registerProxyAPI(mux, cfg.Token)

	// Register read-only SQLite explorer (/api/db/*)
	// Build the source registry from config. Always includes the primary
	// sessions.db source even if the user didn't configure dbExplorer.sources.
	if dbExplorerEnabled(cfg.DBExplorer) {
		dbRegistry = buildDBRegistry(cfg.DBExplorer)
		registerDBUIAPI(mux, cfg.Token)
	}

	// Start Anthropic API proxy if enabled
	if cfg.Proxy.Enabled {
		startProxyServer(cfg.Proxy)
	}

	// Start server
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	server := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // SSE needs no write timeout
		IdleTimeout:  120 * time.Second,
	}

	// Parent context for background goroutines (session lifecycle watcher etc).
	// Cancelled in the graceful shutdown handler below so watchers exit cleanly.
	bgCtx, bgCancel := context.WithCancel(context.Background())

	// Session lifecycle watcher: expires idle soul sessions + daily reset.
	// Runs as a background goroutine inside the server process.
	resetPolicy := loadResetPolicyFromConfig(cfg)
	go sessionLifecycleWatcher(bgCtx, resetPolicy, 5*time.Minute)

	// Rehydrate sessions from previous server instance.
	// Runs after lifecycle watcher is up, with a delay to ensure HTTP server is ready.
	go func() {
		time.Sleep(3 * time.Second)
		sm.rehydrateSessions()
	}()

	// Graceful shutdown on SIGTERM/SIGINT
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "\n[%s] server: received %v, shutting down...\n", appName, sig)

		// Stop background goroutines (lifecycle watcher etc)
		bgCancel()

		// Stop Telegram bot first (stops receiving new messages)
		if tgBridge != nil {
			tgBridge.shutdown()
		}

		// Notify via Telegram
		if tgChatID != "" {
			trySendTelegram(fmt.Sprintf("🔴 %s server shutting down (signal: %v)", appName, sig))
		}

		sm.shutdownAll()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	}()

	serverStartTime = time.Now()
	fmt.Fprintf(os.Stderr, "[%s] server: listening on %s\n", appName, addr)
	fmt.Fprintf(os.Stderr, "[%s] server: max_sessions=%d idle_timeout=%dm max_lifetime=%dh\n",
		appName, cfg.MaxSessions, cfg.IdleTimeoutMin, cfg.MaxLifetimeHrs)

	// Notify via Telegram
	if tgChatID != "" {
		trySendTelegram(fmt.Sprintf("🟢 %s server started on %s", appName, addr))
	}

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "[%s] server: fatal: %v\n", appName, err)
		os.Exit(1)
	}
}

var serverStartTime time.Time

// ── Middleware ──

// authMiddleware verifies Bearer token.
func authMiddleware(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" {
			// Also check query parameter (for SSE clients that can't set headers)
			if q := r.URL.Query().Get("token"); q == token {
				next(w, r)
				return
			}
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing Authorization header"})
			return
		}

		parts := strings.SplitN(auth, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") || parts[1] != token {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
			return
		}

		next(w, r)
	}
}

// ── Helpers ──

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func isActiveUploadExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".html", ".htm", ".js", ".mjs", ".css", ".svg", ".xml", ".xhtml":
		return true
	default:
		return false
	}
}

// ── S3 Upload ──

// uploadToS3 uploads a file to S3 (Wasabi) using a cached client.
// Returns the public URL on success, or empty string on failure.
func uploadToS3(cfg s3Config, localPath string, s3Key string, contentType string) string {
	if cfg.Bucket == "" || cfg.Endpoint == "" {
		return ""
	}

	client := getS3Client(cfg)
	if client == nil {
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	file, err := os.Open(localPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] s3: open file failed: %v\n", appName, err)
		return ""
	}
	defer file.Close()

	// Normalize prefix: ensure trailing slash, no double slashes
	prefix := strings.TrimRight(cfg.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}

	stat, err := file.Stat()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] s3: stat failed: %v\n", appName, err)
		return ""
	}
	fullKey := prefix + strings.TrimLeft(s3Key, "/")
	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(cfg.Bucket),
		Key:           aws.String(fullKey),
		Body:          file,
		ContentLength: aws.Int64(stat.Size()),
		ContentType:   aws.String(contentType),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] s3: upload failed: %v\n", appName, err)
		return ""
	}

	// Build public URL with normalized path
	endpoint := strings.TrimRight(cfg.Endpoint, "/")
	return fmt.Sprintf("%s/%s/%s", endpoint, cfg.Bucket, fullKey)
}
