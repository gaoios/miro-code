package app

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// ── Haiku Worker Pool ──
// Persistent Haiku session for lightweight one-shot queries (auto-rename, etc).
// Modeled after JMS's haiku pool pattern: lazy init, idle reaper, token-based recycling.

const (
	haikuPoolSystemPrompt = "You are a concise analysis assistant. For each user message, respond with ONLY the requested output — no preamble, no explanation, no markdown fences unless asked."
	haikuPoolIdleTimeout  = 10 * time.Minute
	haikuPoolMaxTokens    = 100000 // recycle session after this many tokens
	haikuPoolInitTimeout  = 30 * time.Second
	haikuPoolQueryTimeout = 30 * time.Second
)

type haikuPool struct {
	mu          sync.Mutex
	proc        *claudeBackend
	scanner     *lineScanner
	totalTokens int
	lastUsed    time.Time
	sessionID   string
}

// lineScanner wraps readLines into a channel-based pull model for query().
type lineScanner struct {
	lines chan parsedLine
	done  chan struct{}
}

type parsedLine struct {
	MsgType string
	Raw     json.RawMessage
}

var (
	globalHaikuPool     *haikuPool
	globalHaikuPoolOnce sync.Once
)

func getHaikuPool() *haikuPool {
	globalHaikuPoolOnce.Do(func() {
		globalHaikuPool = &haikuPool{}
		// Idle reaper
		go func() {
			for {
				time.Sleep(1 * time.Minute)
				globalHaikuPool.mu.Lock()
				if globalHaikuPool.proc != nil && time.Since(globalHaikuPool.lastUsed) > haikuPoolIdleTimeout {
					fmt.Fprintf(os.Stderr, "[%s] haiku-pool: idle reaper shutting down session\n", appName)
					globalHaikuPool.proc.shutdown()
					globalHaikuPool.proc = nil
					globalHaikuPool.scanner = nil
					globalHaikuPool.totalTokens = 0
					globalHaikuPool.sessionID = ""
				}
				globalHaikuPool.mu.Unlock()
			}
		}()
	})
	return globalHaikuPool
}

// ensureSession starts or restarts the haiku session. Must hold hp.mu.
func (hp *haikuPool) ensureSession() error {
	needRestart := hp.proc == nil || !hp.proc.alive() || hp.totalTokens > haikuPoolMaxTokens
	if !needRestart {
		return nil
	}

	// Shutdown existing
	if hp.proc != nil {
		hp.proc.shutdown()
		hp.proc = nil
		hp.scanner = nil
	}

	proc, err := spawnClaude(sessionOpts{
		WorkDir:          workspace,
		SystemPromptFile: "", // no soul prompt for haiku pool
		Model:            "haiku",
		MaxTurns:         0, // unlimited turns (persistent session)
	})
	if err != nil {
		return fmt.Errorf("spawn haiku pool: %w", err)
	}
	hp.proc = proc
	hp.totalTokens = 0
	hp.sessionID = ""

	// Start line scanner goroutine
	ls := &lineScanner{
		lines: make(chan parsedLine, 64),
		done:  make(chan struct{}),
	}
	go func() {
		proc.readLines(func(msgType string, raw json.RawMessage) {
			select {
			case ls.lines <- parsedLine{msgType, raw}:
			case <-ls.done:
			}
		})
		close(ls.lines)
	}()
	hp.scanner = ls

	// Wait for init message
	deadline := time.After(haikuPoolInitTimeout)
	for {
		select {
		case <-deadline:
			proc.shutdown()
			hp.proc = nil
			hp.scanner = nil
			return fmt.Errorf("haiku pool init timeout")
		case line, ok := <-ls.lines:
			if !ok {
				hp.proc = nil
				hp.scanner = nil
				return fmt.Errorf("haiku pool stdout closed during init")
			}
			if line.MsgType == "system" {
				var init InitMessage
				if json.Unmarshal(line.Raw, &init) == nil && init.SessionID != "" {
					hp.sessionID = init.SessionID
				}
				fmt.Fprintf(os.Stderr, "[%s] haiku-pool: session ready (sid=%s)\n", appName, shortID(hp.sessionID))
				return nil
			}
			// skip non-system messages during init
		}
	}
}

// query sends a prompt and returns the text result.
func (hp *haikuPool) query(prompt string, timeout time.Duration) (string, error) {
	hp.mu.Lock()
	defer hp.mu.Unlock()

	if err := hp.ensureSession(); err != nil {
		return "", err
	}
	hp.lastUsed = time.Now()

	if err := hp.proc.sendMessage(prompt); err != nil {
		// Process died mid-query — reset and retry once
		hp.proc = nil
		hp.scanner = nil
		if err := hp.ensureSession(); err != nil {
			return "", err
		}
		if err := hp.proc.sendMessage(prompt); err != nil {
			return "", fmt.Errorf("send to haiku pool: %w", err)
		}
	}

	// Read lines until result
	deadline := time.After(timeout)
	for {
		select {
		case <-deadline:
			return "", fmt.Errorf("haiku pool query timeout after %v", timeout)
		case <-hp.proc.done:
			return "", fmt.Errorf("haiku pool process exited")
		case line, ok := <-hp.scanner.lines:
			if !ok {
				return "", fmt.Errorf("haiku pool stdout closed")
			}

			switch line.MsgType {
			case "system":
				// Re-init (session recycled) — parse session ID
				var init InitMessage
				if json.Unmarshal(line.Raw, &init) == nil && init.SessionID != "" {
					hp.sessionID = init.SessionID
				}

			case "assistant":
				// Track token usage
				var msg AssistantMsg
				if json.Unmarshal(line.Raw, &msg) == nil && msg.Message.Usage != nil {
					hp.totalTokens += msg.Message.Usage.InputTokens + msg.Message.Usage.OutputTokens
				}

			case "result":
				var result ResultMessage
				if json.Unmarshal(line.Raw, &result) == nil {
					return strings.TrimSpace(result.Result), nil
				}
				return "", fmt.Errorf("failed to parse result")
			}
		}
	}
}

// shutdown cleanly stops the pool (call on server shutdown).
func (hp *haikuPool) shutdown() {
	hp.mu.Lock()
	defer hp.mu.Unlock()
	if hp.proc != nil {
		hp.proc.shutdown()
		hp.proc = nil
		hp.scanner = nil
	}
}
