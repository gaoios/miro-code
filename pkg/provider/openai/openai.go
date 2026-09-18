// Package openai is the embedded Anthropic→Codex protocol proxy.
//
// When a provider has Type=="openai", the soul-cli runtime asks this package
// for a local HTTP server that translates Anthropic /v1/messages into the
// Codex /codex/responses API. Claude Code talks Anthropic; this proxy
// silently speaks ChatGPT Plus (Codex) on the way out.
//
// Auth source: ~/.codex/auth.json (written by `codex login`).
package openai

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/kiyor/soul-cli/pkg/provider"
)

// codexSeenEventTypes tracks unique SSE event types observed across all
// requests. Diagnostic only; concurrent-safe via sync.Map so multiple in-flight
// proxy requests can race on first-sighting without panicking the process.
// Cleared on process restart.
var codexSeenEventTypes sync.Map // map[string]struct{}

// codexSeenToolTypes tracks unique Anthropic tool `type` values observed on
// incoming requests. Same contract as codexSeenEventTypes — diagnostic only,
// concurrent-safe via sync.Map. Lets us learn what CC actually sends (e.g.
// web_search_20250305, bash_20250124, text_editor_*, custom function tools
// with no `type` field).
var codexSeenToolTypes sync.Map // map[string]struct{}

// Endpoint is the Codex Responses API URL this package targets by default
// when a provider does not set ChatURL explicitly.
const Endpoint = "https://chatgpt.com/backend-api/codex/responses"

const (
	codexEndpoint = Endpoint
	codexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexTokenURL = "https://auth.openai.com/oauth/token"
	codexAuthFile = "~/.codex/auth.json"
)

// Session is the exported alias for the internal codex OAuth session. Exposed
// so server endpoints that need AccessToken / AccountID / mu can hold a *Session
// without the subpackage leaking its internal type name.
type Session = codexSession

// LoadAuth reads the Codex OAuth auth.json (defaults to ~/.codex/auth.json when
// authFile is empty) and returns a Session ready for use with EnsureTokenFresh.
// Wraps the internal loadCodexAuth so main-package code doesn't reach into the
// lowercase name.
func LoadAuth(authFile string) (*Session, error) { return loadCodexAuth(authFile) }

// EnsureTokenFresh refreshes the access token on sess if it's expired, writing
// the rotated token back to disk when needed. Wraps ensureCodexTokenFresh.
func EnsureTokenFresh(sess *Session, authFile string) error {
	return ensureCodexTokenFresh(sess, authFile)
}

// UserAgent returns the User-Agent string this package sends on upstream
// Codex requests. Exported so server endpoints that mint their own HTTP
// requests (e.g. /api/codex/usage) match the signature of the embedded proxy.
func UserAgent() string { return codexUserAgent() }

// ── Auth ──────────────────────────────────────────────────────────────────────

type codexAuthJSON struct {
	AuthMode string `json:"auth_mode"`
	Tokens   struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
}

type codexSession struct {
	AccessToken  string
	RefreshToken string
	AccountID    string
}

func loadCodexAuth(authFile string) (*codexSession, error) {
	if authFile == "" {
		authFile = codexAuthFile
	}
	path := provider.ExpandPath(authFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w (run: codex login)", authFile, err)
	}
	var f codexAuthJSON
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("decode codex auth: %w", err)
	}
	if f.Tokens.AccessToken == "" {
		return nil, fmt.Errorf("no access_token in %s", authFile)
	}
	return &codexSession{
		AccessToken:  f.Tokens.AccessToken,
		RefreshToken: f.Tokens.RefreshToken,
		AccountID:    f.Tokens.AccountID,
	}, nil
}

// decodeJWTExp extracts the exp claim from a JWT without signature validation.
func decodeJWTExp(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	pad := (4 - len(parts[1])%4) % 4
	decoded, err := base64.URLEncoding.DecodeString(parts[1] + strings.Repeat("=", pad))
	if err != nil {
		return time.Time{}, false
	}
	var payload struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(decoded, &payload); err != nil || payload.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(payload.Exp, 0), true
}

type codexTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// reloadCodexAuthFromDisk re-reads auth.json and updates the in-memory session
// if disk has a newer token. This handles the case where codex-cli itself
// refreshed the token out-of-band (OAuth refresh_token rotation makes our
// cached refresh_token unusable once codex-cli has consumed it).
//
// Returns true if the in-memory token was replaced with a fresher one.
func reloadCodexAuthFromDisk(sess *codexSession, authFile string) bool {
	fresh, err := loadCodexAuth(authFile)
	if err != nil {
		return false
	}
	if fresh.AccessToken == sess.AccessToken {
		return false
	}
	sess.AccessToken = fresh.AccessToken
	sess.RefreshToken = fresh.RefreshToken
	if fresh.AccountID != "" {
		sess.AccountID = fresh.AccountID
	}
	return true
}

// writeCodexAuthToDisk persists a refreshed token pair back to auth.json so
// codex-cli and subsequent proxy restarts see the latest credentials. Failure
// is non-fatal (in-memory session still works for this proxy).
func writeCodexAuthToDisk(sess *codexSession, authFile string) {
	if authFile == "" {
		authFile = codexAuthFile
	}
	path := provider.ExpandPath(authFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return
	}
	tokens, _ := raw["tokens"].(map[string]any)
	if tokens == nil {
		tokens = make(map[string]any)
		raw["tokens"] = tokens
	}
	tokens["access_token"] = sess.AccessToken
	if sess.RefreshToken != "" {
		tokens["refresh_token"] = sess.RefreshToken
	}
	if sess.AccountID != "" {
		tokens["account_id"] = sess.AccountID
	}
	raw["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return
	}
	// Atomic write: tmp file then rename
	tmp := path + ".weiran.tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func refreshCodexToken(sess *codexSession, authFile string) error {
	values := url.Values{}
	values.Set("grant_type", "refresh_token")
	values.Set("client_id", codexClientID)
	values.Set("refresh_token", sess.RefreshToken)

	req, err := http.NewRequest(http.MethodPost, codexTokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read refresh response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// OAuth refresh_token_reused: disk likely has a newer token (codex-cli
		// rotated it). Try reloading from disk as a fallback.
		if bytes.Contains(body, []byte("refresh_token_reused")) || bytes.Contains(body, []byte("already been used")) {
			if reloadCodexAuthFromDisk(sess, authFile) {
				fmt.Fprintf(os.Stderr, "[%s] codex: refresh_token_reused → reloaded newer token from disk\n", provider.AppName)
				return nil
			}
		}
		return fmt.Errorf("refresh failed %s: %s", resp.Status, body)
	}
	var tr codexTokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return fmt.Errorf("decode refresh: %w", err)
	}
	if tr.AccessToken == "" {
		return fmt.Errorf("no access_token in refresh response")
	}
	sess.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		sess.RefreshToken = tr.RefreshToken
	}
	// Persist so codex-cli and future proxy restarts see the rotated pair.
	writeCodexAuthToDisk(sess, authFile)
	return nil
}

func ensureCodexTokenFresh(sess *codexSession, authFile string) error {
	// Always peek at disk first — codex-cli may have refreshed out-of-band,
	// in which case our in-memory refresh_token is already burned.
	reloadCodexAuthFromDisk(sess, authFile)

	exp, ok := decodeJWTExp(sess.AccessToken)
	if ok && time.Now().Before(exp.Add(-30*time.Second)) {
		return nil
	}
	fmt.Fprintf(os.Stderr, "[%s] codex token expired, refreshing...\n", provider.AppName)
	return refreshCodexToken(sess, authFile)
}

func codexUserAgent() string {
	return fmt.Sprintf("pi (%s; %s)", runtime.GOOS, runtime.GOARCH)
}

// ── Anthropic request/response types ─────────────────────────────────────────

type codexAnthropicRequest struct {
	Model     string                  `json:"model"`
	MaxTokens int                     `json:"max_tokens"`
	System    any                     `json:"system,omitempty"`
	Messages  []codexAnthropicMessage `json:"messages"`
	Stream    bool                    `json:"stream"`
	Thinking  *codexAnthropicThinking `json:"thinking,omitempty"`
}

// codexAnthropicThinking is the top-level `thinking` field on /v1/messages.
// Claude Code sends one of:
//   - {type: "adaptive"}                               (Opus/Sonnet 4.6+ default)
//   - {type: "enabled", budget_tokens: N}              (older models or explicit)
//   - {type: "disabled"}                               (or field absent)
type codexAnthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type codexAnthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type codexAnthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`    // for tool_use blocks
	Name      string          `json:"name,omitempty"`  // for tool_use blocks
	Input     json.RawMessage `json:"input,omitempty"` // for tool_use blocks
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
}

type codexAnthropicResponse struct {
	ID      string                `json:"id"`
	Type    string                `json:"type"`
	Role    string                `json:"role"`
	Content []codexAnthropicBlock `json:"content"`
	Model   string                `json:"model"`
	// StopReason/StopSequence as pointers → serialize as JSON null (Anthropic
	// canonical) on message_start, not "" which some SDK paths treat as
	// already-set and never update from the subsequent message_delta.
	StopReason   *string             `json:"stop_reason"`
	StopSequence *string             `json:"stop_sequence"`
	Usage        codexAnthropicUsage `json:"usage"`
}

type codexAnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// SSE event types
type codexMsgStart struct {
	Type    string                 `json:"type"`
	Message codexAnthropicResponse `json:"message"`
}

type codexBlockStart struct {
	Type         string               `json:"type"`
	Index        int                  `json:"index"`
	ContentBlock codexAnthropicBlock  `json:"content_block"`
}

type codexBlockDelta struct {
	Type  string         `json:"type"`
	Index int            `json:"index"`
	Delta map[string]any `json:"delta"`
}

type codexBlockStop struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type codexMsgDelta struct {
	Type  string `json:"type"`
	Delta struct {
		StopReason   string  `json:"stop_reason"`
		StopSequence *string `json:"stop_sequence"`
	} `json:"delta"`
	Usage codexAnthropicUsage `json:"usage"`
}

type codexMsgStop struct {
	Type string `json:"type"`
}

// ── Codex request ─────────────────────────────────────────────────────────────

type codexAPIRequest struct {
	Model             string                `json:"model"`
	Store             bool                  `json:"store"`
	Stream            bool                  `json:"stream"`
	Instructions      string                `json:"instructions"`
	Input             []any                 `json:"input"`
	Text              map[string]any        `json:"text"`
	Tools             []codexToolSpec       `json:"tools,omitempty"`
	ToolChoice        string                `json:"tool_choice"`
	ParallelToolCalls bool                  `json:"parallel_tool_calls"`
	Reasoning         *codexReasoningConfig `json:"reasoning,omitempty"`
	// Include controls which optional response artefacts the upstream returns.
	// We always request `reasoning.encrypted_content` because Store=false leaves
	// no server-side state, so the only way to keep multi-turn reasoning
	// context alive is to round-trip the encrypted reasoning blob back to the
	// upstream on the next turn. Without this, GPT-5 / o-series effectively
	// "forget" their prior chain of thought between Anthropic /v1/messages
	// turns, even though Claude Code preserves the thinking blocks locally.
	// cc-switch sets this under is_codex_oauth=true; mirror that behaviour.
	Include []string `json:"include,omitempty"`
	// ServiceTier asks the ChatGPT backend to route this request through the
	// priority queue. ChatGPT Plus/Pro subscriptions are entitled to it; free
	// accounts gracefully fall back to the standard queue. codex-cli itself
	// sends "priority" by default, so matching its fingerprint here keeps the
	// proxy indistinguishable from an authentic codex-cli session.
	ServiceTier string `json:"service_tier,omitempty"`
}

// codexReasoningConfig requests reasoning summary emission. Without
// summary="auto", GPT-5 / o-series still reason internally but do NOT emit
// any response.reasoning_summary_text.delta events, so CC sees zero thinking
// blocks. Effort controls how much capacity the upstream spends reasoning.
type codexReasoningConfig struct {
	Summary string `json:"summary,omitempty"`
	Effort  string `json:"effort,omitempty"`
}

// anthropicThinkingToCodexReasoning maps Anthropic's top-level `thinking`
// field to Codex's reasoning config.
//
// Anthropic has three thinking types:
//   - "adaptive":  let the model decide (Opus/Sonnet 4.6+ default) → effort:"high"
//   - "enabled":   explicit budget N in tokens → bucket into low/medium/high/xhigh
//   - "disabled":  or field absent → effort:"minimal" with no summary
//
// Rationale for summary="auto" even at minimal effort: CC's JSONL still
// records thinking blocks when upstream emits them; losing them silently is
// worse than a few extra tokens of summary. Only when Anthropic explicitly
// says "disabled" do we fully suppress.
//
// Budget bucket thresholds match Anthropic's own internal guidance: <2048 is
// a light reasoning pass (formatting, short plans); 2048-9999 is medium
// (multi-step reasoning); 10000-19999 is deep thinking; 20000+ is
// ultrathink territory and maps to xhigh on gpt-5.4+ (older models without
// an xhigh tier are normalized back to high by normalizeCodexEffortForModel).
//
// Adaptive stays at "high" intentionally: Claude Code's default thinking is
// adaptive, and bumping that to xhigh would silently overbuy reasoning on
// every request. xhigh only unlocks when the caller sets an explicit budget.
func anthropicThinkingToCodexReasoning(th *codexAnthropicThinking) *codexReasoningConfig {
	if th == nil || th.Type == "" || th.Type == "disabled" {
		return &codexReasoningConfig{Effort: "minimal"}
	}
	if th.Type == "adaptive" {
		return &codexReasoningConfig{Summary: "auto", Effort: "high"}
	}
	// type == "enabled" (or anything else we don't recognize → treat as enabled)
	effort := "medium"
	switch {
	case th.BudgetTokens <= 0:
		// No budget specified on "enabled" — keep prior behaviour (upstream default).
		effort = ""
	case th.BudgetTokens < 2048:
		effort = "low"
	case th.BudgetTokens < 10000:
		effort = "medium"
	case th.BudgetTokens < 20000:
		effort = "high"
	default:
		// 20000+ → ultrathink. Only gpt-5.4+ honours xhigh; older models get
		// normalized back to high by normalizeCodexEffortForModel.
		effort = "xhigh"
	}
	return &codexReasoningConfig{Summary: "auto", Effort: effort}
}

// codexModelSupportsMinimalEffort reports whether the target codex model
// accepts reasoning.effort="minimal". gpt-5.4 and its siblings renamed the
// zero-reasoning tier to "none" and now reject "minimal" with HTTP 400:
//
//	Unsupported value: 'minimal' is not supported with the 'gpt-5.4' model.
//	Supported values are: 'none', 'low', 'medium', 'high', and 'xhigh'.
//
// The same constraint applies to gpt-5.5 (and presumably any future
// gpt-5.N with N>=4) — the tier rename happened once and sticks.
//
// This matters most for subagent (Agent tool) dispatch: Claude Code spawns
// subagents without a `thinking` field, which falls through to
// anthropicThinkingToCodexReasoning → Effort:"minimal" → 400 on gpt-5.4+.
// Subagents cannot configure their own effort, so the proxy has to normalize.
func codexModelSupportsMinimalEffort(model string) bool {
	// Parse the minor version for gpt-5.N models; 5.4+ rejects minimal.
	// Older families (gpt-5.3, gpt-5.2, o-series) still accept it.
	if rest, ok := strings.CutPrefix(model, "gpt-5."); ok {
		minor := 0
		for i := 0; i < len(rest); i++ {
			if rest[i] < '0' || rest[i] > '9' {
				break
			}
			minor = minor*10 + int(rest[i]-'0')
		}
		if minor >= 4 {
			return false
		}
	}
	return true
}

// codexModelSupportsXHighEffort reports whether the target codex model
// accepts reasoning.effort="xhigh". Only gpt-5.4+ exposes this tier;
// older gpt-5.N and o-series cap out at "high".
func codexModelSupportsXHighEffort(model string) bool {
	if rest, ok := strings.CutPrefix(model, "gpt-5."); ok {
		minor := 0
		for i := 0; i < len(rest); i++ {
			if rest[i] < '0' || rest[i] > '9' {
				break
			}
			minor = minor*10 + int(rest[i]-'0')
		}
		if minor >= 4 {
			return true
		}
	}
	return false
}

// normalizeCodexEffortForModel rewrites effort values that the target model
// does not accept. Two rewrites are needed today:
//   - "minimal" → "none" for gpt-5.4+ (the zero-reasoning tier was renamed)
//   - "xhigh"   → "high" for anything other than gpt-5.4+ (tier doesn't exist)
//
// The function is nil-safe and idempotent.
func normalizeCodexEffortForModel(cfg *codexReasoningConfig, model string) {
	if cfg == nil {
		return
	}
	if cfg.Effort == "minimal" && !codexModelSupportsMinimalEffort(model) {
		cfg.Effort = "none"
	}
	if cfg.Effort == "xhigh" && !codexModelSupportsXHighEffort(model) {
		cfg.Effort = "high"
	}
}

// codexToolSpec is a Codex function tool definition (OpenAI Responses API format).
type codexToolSpec struct {
	Type        string         `json:"type"` // "function"
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Strict      bool           `json:"strict"`
}

// codexInputMessage is a Codex input message item.
type codexInputMessage struct {
	Type    string           `json:"type"` // "message"
	Role    string           `json:"role"`
	Content []codexInputItem `json:"content"`
}

// codexInputItem is a content item inside a Codex input message.
type codexInputItem struct {
	Type string `json:"type"` // "input_text" or "output_text"
	Text string `json:"text"`
}

// codexFunctionCall is a Codex function_call input item (tool invocation in history).
type codexFunctionCall struct {
	Type      string `json:"type"` // "function_call"
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// codexFunctionCallOutput is a Codex function_call_output input item (tool result in history).
type codexFunctionCallOutput struct {
	Type   string `json:"type"` // "function_call_output"
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

type codexMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ── Protocol translation helpers ──────────────────────────────────────────────

func codexExtractSystemText(system any) string {
	switch v := system.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := obj["text"].(string); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func codexExtractContentText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := obj["type"].(string)
			if typ == "text" {
				if t, _ := obj["text"].(string); t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// codexTranslateTools converts Anthropic tool definitions to Codex function tool specs.
//
// Anthropic distinguishes two tool classes:
//   - Client tools: {type: "custom", name, input_schema} — CC executes locally.
//     These translate cleanly to OpenAI function tools.
//   - Server tools: {type: "web_search_20250305", ...} / {type: "bash_20250124", ...} /
//     {type: "text_editor_*", ...} / {type: "computer_*", ...} — executed by
//     Anthropic's backend, not by the client. When proxying to OpenAI Codex
//     these have no equivalent; upstream cannot execute them. We strip them
//     to avoid malformed function tools with empty parameter schemas.
func codexTranslateTools(tools []any) []codexToolSpec {
	var out []codexToolSpec
	for _, t := range tools {
		obj, ok := t.(map[string]any)
		if !ok {
			continue
		}
		name, _ := obj["name"].(string)
		desc, _ := obj["description"].(string)
		typ, _ := obj["type"].(string)
		params, _ := obj["input_schema"].(map[string]any)

		// Diagnostic: log each distinct tool type once per proxy lifetime.
		// Key is type|name so we see both server tools (type=X) and client
		// tools (type="" or type="custom" + unique name).
		// LoadOrStore is atomic — multiple in-flight requests on the same key
		// will only emit the log line once.
		diagKey := typ + "|" + name
		if _, loaded := codexSeenToolTypes.LoadOrStore(diagKey, struct{}{}); !loaded {
			fmt.Fprintf(os.Stderr, "[%s] codex tool seen: type=%q name=%q has_schema=%v\n",
				provider.AppName, typ, name, params != nil)
		}

		if name == "" {
			continue
		}
		// Strip Anthropic server tools — upstream OpenAI can't execute them.
		// Heuristic: any `type` that isn't "custom" or empty is a server tool.
		// Common patterns: web_search_YYYYMMDD, bash_YYYYMMDD, text_editor_*,
		// computer_*. If Anthropic adds more, they'll match here too.
		if typ != "" && typ != "custom" && typ != "function" {
			fmt.Fprintf(os.Stderr, "[%s] codex: stripping Anthropic server tool type=%q name=%q (no upstream equivalent)\n",
				provider.AppName, typ, name)
			continue
		}
		// Client tools must have a schema — Codex requires `parameters` to
		// be a valid JSON schema object. Empty `{}` is acceptable (zero-arg
		// function); nil is not.
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, codexToolSpec{
			Type:        "function",
			Name:        name,
			Description: desc,
			Parameters:  params,
			Strict:      false,
		})
	}
	return out
}

// codexTranslateMessages converts Anthropic messages (with tool_use/tool_result blocks)
// to Codex input items for the Responses API.
func codexTranslateMessages(messages []codexAnthropicMessage) []any {
	var out []any
	// Map tool_use_id (Anthropic) → call_id (Codex) for accurate reverse mapping.
	// Avoids TrimPrefix assumption that all IDs were generated by us.
	toolUseToCallID := make(map[string]string)
	for _, m := range messages {
		role := m.Role
		switch content := m.Content.(type) {
		case string:
			itemType := "input_text"
			if role == "assistant" {
				itemType = "output_text"
			}
			out = append(out, codexInputMessage{
				Type: "message",
				Role: role,
				Content: []codexInputItem{{Type: itemType, Text: content}},
			})
		case []any:
			// Collect text blocks into a message, emit tool blocks as separate items
			var textParts []codexInputItem
			itemType := "input_text"
			if role == "assistant" {
				itemType = "output_text"
			}
			for _, item := range content {
				block, ok := item.(map[string]any)
				if !ok {
					continue
				}
				typ, _ := block["type"].(string)
				switch typ {
				case "text":
					if t, _ := block["text"].(string); t != "" {
						textParts = append(textParts, codexInputItem{Type: itemType, Text: t})
					}
				case "tool_use":
					// Flush pending text first
					if len(textParts) > 0 {
						out = append(out, codexInputMessage{
							Type: "message", Role: role, Content: textParts,
						})
						textParts = nil
					}
					id, _ := block["id"].(string)
					name, _ := block["name"].(string)
					inputVal := block["input"]
					argBytes, _ := json.Marshal(inputVal)
					// Use the raw Anthropic ID as the Codex CallID, and record
					// the mapping so tool_result can look it up accurately.
					toolUseToCallID[id] = id
					out = append(out, codexFunctionCall{
						Type:      "function_call",
						CallID:    id,
						Name:      name,
						Arguments: string(argBytes),
					})
				case "tool_result":
					// Flush pending text first
					if len(textParts) > 0 {
						out = append(out, codexInputMessage{
							Type: "message", Role: role, Content: textParts,
						})
						textParts = nil
					}
					toolUseID, _ := block["tool_use_id"].(string)
					// Look up the original call_id from our mapping.
					// Falls back to TrimPrefix for IDs generated during
					// Codex→Anthropic streaming (where we prefixed "toolu_").
					callID, ok := toolUseToCallID[toolUseID]
					if !ok {
						callID = strings.TrimPrefix(toolUseID, "toolu_")
					}
					outputText := ""
					switch c := block["content"].(type) {
					case string:
						outputText = c
					case []any:
						for _, ci := range c {
							if co, ok := ci.(map[string]any); ok {
								if t, _ := co["text"].(string); t != "" {
									outputText += t
								}
							}
						}
					}
					out = append(out, codexFunctionCallOutput{
						Type:   "function_call_output",
						CallID: callID,
						Output: outputText,
					})
				}
			}
			// Flush remaining text
			if len(textParts) > 0 {
				out = append(out, codexInputMessage{
					Type: "message", Role: role, Content: textParts,
				})
			}
		}
	}
	return out
}

func codexWriteSSE(w http.ResponseWriter, event string, data any) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(b))
}

func codexRandomID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// codexValidatedToolArgs returns validated JSON for a function_call's
// accumulated arguments. Codex normally emits well-formed JSON, but malformed
// output has been observed when `incomplete` or `content_filter` terminates the
// stream mid-tool-call; wrap as {"raw": "..."} so the Anthropic client's JSON
// parser does not choke.
func codexValidatedToolArgs(arguments string) string {
	if arguments == "" {
		return "{}"
	}
	if json.Valid([]byte(arguments)) {
		return arguments
	}
	b, _ := json.Marshal(map[string]string{"raw": arguments})
	return string(b)
}

// mapCodexStopReason translates Codex completion state to Anthropic stop_reason.
// Parameters:
//
//	finishReason     — legacy `choice.finish_reason` (rarely set on Responses API)
//	incompleteReason — `response.incomplete.reason` (e.g. "max_output_tokens", "content_filter")
//	hasToolUse       — whether any function_call was emitted
//
// Priority: explicit incomplete reason > finish_reason > tool_use heuristic > end_turn.
func mapCodexStopReason(finishReason, incompleteReason string, hasToolUse bool) string {
	switch incompleteReason {
	case "max_output_tokens":
		return "max_tokens"
	case "content_filter":
		return "stop_sequence"
	}
	switch finishReason {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "stop_sequence"
	}
	if hasToolUse {
		return "tool_use"
	}
	return "end_turn"
}

// codexVerbosityFor picks a Responses API verbosity setting based on the
// request's max_tokens. Previously hardcoded to "medium", which made GPT
// verbose for short questions and terse for long ones. Mapping:
//
//	< 512       → low
//	512 – 4096  → medium
//	> 4096      → high
func codexVerbosityFor(maxTokens int) string {
	if maxTokens <= 0 {
		return "medium"
	}
	if maxTokens < 512 {
		return "low"
	}
	if maxTokens > 4096 {
		return "high"
	}
	return "medium"
}

// ── Server proxy delegation ───────────────────────────────────────────────────

// DetectServerProxy checks if the weiran server is running and asks it to
// start (or reuse) an OpenAI proxy for the given provider. Returns the proxy port,
// or 0 if the server is not available or does not support the provider.
//
// When the server manages the proxy, CLI can use syscall.Exec normally (the proxy
// goroutine lives in the long-running server process, not the ephemeral CLI process).
func DetectServerProxy(providerName string) int {
	cfg := provider.LoadServerAddr()
	if cfg.Token == "" {
		return 0
	}

	serverURL := fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)

	// Quick health check (200ms timeout)
	client := &http.Client{Timeout: 200 * time.Millisecond}
	resp, err := client.Get(serverURL + "/api/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return 0
	}
	resp.Body.Close()

	// Ask server to start/get proxy for this provider
	req, err := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/api/proxy/openai?provider=%s", serverURL, providerName), nil)
	if err != nil {
		return 0
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	resp, err = client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return 0
	}
	defer resp.Body.Close()

	var result struct {
		Port int `json:"port"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || result.Port == 0 {
		return 0
	}

	fmt.Fprintf(os.Stderr, "[%s] reusing server openai proxy on port %d\n", provider.AppName, result.Port)
	return result.Port
}

// ── Embedded proxy ────────────────────────────────────────────────────────────

// StartProxy starts a local HTTP server that translates Anthropic /v1/messages
// to Codex /codex/responses. Returns the port the server is listening on.
//
// After a successful start, provider.ActivePort is set so the main package
// knows to avoid syscall.Exec (which would kill the proxy goroutine).
func StartProxy(cfg provider.Config) (int, error) {
	authFile := cfg.AuthFile
	sess, err := loadCodexAuth(authFile)
	if err != nil {
		return 0, fmt.Errorf("codex auth: %w", err)
	}

	chatURL := cfg.ChatURL
	if chatURL == "" {
		chatURL = codexEndpoint
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("listen: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleCodexMessages(w, r, sess, chatURL, authFile)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"status":"ok","proxy":"weiran/codex"}`)
	})

	go func() {
		if err := http.Serve(ln, mux); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] codex proxy stopped: %v\n", provider.AppName, err)
		}
	}()

	provider.ActivePort = port
	fmt.Fprintf(os.Stderr, "[%s] codex proxy started on http://127.0.0.1:%d\n", provider.AppName, port)
	return port, nil
}

// handleCodexMessages translates Anthropic /v1/messages → Codex /codex/responses
// with full tool use support (function_call ↔ tool_use).
func handleCodexMessages(w http.ResponseWriter, r *http.Request, sess *codexSession, chatURL, authFile string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Parse as generic map first to extract tools (not in typed struct)
	var rawReq map[string]any
	if err := json.Unmarshal(body, &rawReq); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}

	var areq codexAnthropicRequest
	if err := json.Unmarshal(body, &areq); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}

	if err := ensureCodexTokenFresh(sess, authFile); err != nil {
		http.Error(w, "token refresh: "+err.Error(), http.StatusUnauthorized)
		return
	}

	instructions := codexExtractSystemText(areq.System)
	if instructions == "" {
		instructions = "You are a helpful assistant."
	}

	// Translate Anthropic tools → Codex function tools
	var codexTools []codexToolSpec
	if toolsRaw, ok := rawReq["tools"].([]any); ok && len(toolsRaw) > 0 {
		codexTools = codexTranslateTools(toolsRaw)
	}

	// Translate messages with tool_use/tool_result support
	inputItems := codexTranslateMessages(areq.Messages)

	// Pass the requested model through as-is. Previously a non-"gpt-" prefix
	// silently rewrote to "gpt-4.1", which meant callers thought they were
	// using (say) gpt-5.1-codex but were actually served by gpt-4.1 — the
	// single biggest "GPT is a fool" symptom. Now the endpoint will reject
	// unsupported model names and the error surfaces to the caller.
	targetModel := areq.Model
	if !strings.HasPrefix(targetModel, "gpt-") {
		fmt.Fprintf(os.Stderr,
			"[%s] codex proxy: model %q does not have gpt- prefix; codex endpoint may reject\n",
			provider.AppName, targetModel)
	}

	// Map Anthropic's thinking config onto Codex's reasoning config so
	// user intent (adaptive / enabled:N / disabled) actually reaches the
	// upstream. Previously every request was hardcoded to summary:"auto"
	// with empty effort, which overbought reasoning for small budgets and
	// under-instrumented for large ones.
	//
	// Then normalize per target model — gpt-5.4 rejects "minimal" with 400,
	// which breaks subagent (Agent tool) spawns because subagents send no
	// thinking field and fall through to Effort:"minimal".
	reasoning := anthropicThinkingToCodexReasoning(areq.Thinking)
	normalizeCodexEffortForModel(reasoning, targetModel)

	creq := codexAPIRequest{
		Model:             targetModel,
		Store:             false,
		Stream:            true,
		Instructions:      instructions,
		Input:             inputItems,
		Text:              map[string]any{"verbosity": codexVerbosityFor(areq.MaxTokens)},
		Tools:             codexTools,
		ToolChoice:        "auto",
		ParallelToolCalls: true,
		Reasoning:         reasoning,
		// Preserve multi-turn reasoning context under Store=false (Codex OAuth
		// path requires Store=false). Without this, GPT-5 reasoning chains do
		// not survive across turns even though CC keeps the thinking blocks.
		Include: []string{"reasoning.encrypted_content"},
		// Match codex-cli's default fingerprint; Plus/Pro accounts get the
		// priority queue, free accounts gracefully degrade.
		ServiceTier: "priority",
	}

	reqBody, err := json.Marshal(creq)
	if err != nil {
		http.Error(w, "marshal request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	req, err := http.NewRequest(http.MethodPost, chatURL, bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, "build request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Authorization", "Bearer "+sess.AccessToken)
	req.Header.Set("chatgpt-account-id", sess.AccountID)
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("originator", "pi")
	req.Header.Set("User-Agent", codexUserAgent())
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		upBody, _ := io.ReadAll(resp.Body)
		snippet := string(upBody)
		if len(snippet) > 2000 {
			snippet = snippet[:2000] + "...[truncated]"
		}
		fmt.Fprintf(os.Stderr, "[%s] codex upstream %d for model=%s: %s\n",
			provider.AppName, resp.StatusCode, targetModel, snippet)

		// Map upstream HTTP status to Anthropic-shaped error type. CC/SDK
		// clients branch on `error.type`: rate_limit_error triggers exponential
		// backoff, while api_error gets a fast retry that compounds upstream
		// throttle. Lumping everything into api_error (the previous behaviour)
		// breaks 429 backoff and disguises 4xx config errors as transient
		// server faults.
		errType := "api_error"
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			errType = "rate_limit_error"
		case resp.StatusCode == http.StatusUnauthorized:
			errType = "authentication_error"
		case resp.StatusCode == http.StatusForbidden:
			errType = "permission_error"
		case resp.StatusCode == http.StatusNotFound:
			errType = "not_found_error"
		case resp.StatusCode == http.StatusRequestEntityTooLarge:
			errType = "request_too_large"
		case resp.StatusCode >= 400 && resp.StatusCode < 500:
			errType = "invalid_request_error"
		case resp.StatusCode >= 500:
			errType = "api_error"
		}

		// On 429, propagate Retry-After so CC backs off the right amount.
		// Prefer upstream's value; fall back to a 1s floor if missing so CC
		// doesn't immediately stampede.
		if resp.StatusCode == http.StatusTooManyRequests {
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				w.Header().Set("Retry-After", ra)
			} else {
				w.Header().Set("Retry-After", "1")
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		errMsg, _ := json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]string{"type": errType, "message": string(upBody)},
		})
		w.Write(errMsg)
		return
	}

	// Stream Codex SSE → Anthropic SSE with tool support
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	msgID := "msg_" + codexRandomID()

	codexWriteSSE(w, "message_start", codexMsgStart{
		Type: "message_start",
		Message: codexAnthropicResponse{
			ID:      msgID,
			Type:    "message",
			Role:    "assistant",
			Content: []codexAnthropicBlock{},
			Model:   targetModel,
			Usage:   codexAnthropicUsage{},
		},
	})
	codexWriteSSE(w, "ping", map[string]string{"type": "ping"})
	if flusher != nil {
		flusher.Flush()
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var outputTokens int
	blockIndex := 0
	textBlockStarted := false

	// item_id (Codex) → block index (Anthropic) for function_call streaming.
	// Built on `response.output_item.added`, consumed by
	// `response.function_call_arguments.delta` / `.done`.
	toolItemBlockIdx := make(map[string]int)
	toolItemArgs := make(map[string]string) // accumulated args per item_id
	toolItemOrder := make([]string, 0)       // emit order for cleanup
	toolItemClosed := make(map[string]bool)  // track which items already had block_stop

	// Reasoning item state. Codex emits a "reasoning" output_item containing
	// summary parts; we materialise each reasoning item as one Anthropic
	// thinking block. summary_part.added → maybe-open block; summary_text.delta
	// → emit thinking_delta; output_item.done(reasoning) → close block.
	reasoningItemBlockIdx := make(map[string]int)
	reasoningItemOpen := make(map[string]bool)
	reasoningItemClosed := make(map[string]bool)

	// Terminal signals from upstream (Codex only emits one of completed / failed / incomplete).
	finishReason := ""
	incompleteReason := ""
	upstreamErrMsg := ""
	upstreamFailed := false

	// closeAllOpenBlocks flushes any open content blocks so message_delta/stop
	// can be emitted cleanly. Called when the stream terminates (normally, or
	// due to a failed/incomplete event).
	closeAllOpenBlocks := func() {
		if textBlockStarted {
			codexWriteSSE(w, "content_block_stop", codexBlockStop{
				Type: "content_block_stop", Index: blockIndex,
			})
			textBlockStarted = false
			blockIndex++
		}
		for itemID, open := range reasoningItemOpen {
			if !open || reasoningItemClosed[itemID] {
				continue
			}
			codexWriteSSE(w, "content_block_stop", codexBlockStop{
				Type: "content_block_stop", Index: reasoningItemBlockIdx[itemID],
			})
			reasoningItemClosed[itemID] = true
			blockIndex++
		}
		for _, itemID := range toolItemOrder {
			if toolItemClosed[itemID] {
				continue
			}
			bIdx := toolItemBlockIdx[itemID]
			args := codexValidatedToolArgs(toolItemArgs[itemID])
			codexWriteSSE(w, "content_block_delta", codexBlockDelta{
				Type:  "content_block_delta",
				Index: bIdx,
				Delta: map[string]any{
					"type":         "input_json_delta",
					"partial_json": args,
				},
			})
			codexWriteSSE(w, "content_block_stop", codexBlockStop{
				Type: "content_block_stop", Index: bIdx,
			})
			toolItemClosed[itemID] = true
		}
	}

	// closeTextIfOpen / openReasoningBlock helpers for reasoning event handlers.
	closeTextIfOpen := func() {
		if textBlockStarted {
			codexWriteSSE(w, "content_block_stop", codexBlockStop{
				Type: "content_block_stop", Index: blockIndex,
			})
			textBlockStarted = false
			blockIndex++
		}
	}
	openReasoningBlock := func(itemID string) {
		if itemID == "" {
			return
		}
		if reasoningItemOpen[itemID] {
			return
		}
		closeTextIfOpen()
		// Mirror textBlockStarted convention: don't bump blockIndex on
		// block_start; it's bumped at block_stop.
		reasoningItemBlockIdx[itemID] = blockIndex
		codexWriteSSE(w, "content_block_start", codexBlockStart{
			Type:  "content_block_start",
			Index: blockIndex,
			ContentBlock: codexAnthropicBlock{
				Type:     "thinking",
				Thinking: "",
			},
		})
		reasoningItemOpen[itemID] = true
	}

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		eventType, _ := event["type"].(string)

		// Diagnostic: log every distinct event type seen, once per process.
		// Lets us discover whether the chatgpt codex endpoint actually emits
		// reasoning_summary_* events (private endpoint may differ from
		// public /v1/responses spec). LoadOrStore is atomic — concurrent
		// SSE streams seeing the same event type only log it once.
		if _, loaded := codexSeenEventTypes.LoadOrStore(eventType, struct{}{}); !loaded {
			// Sample a small slice of the event payload.
			snip, _ := json.Marshal(event)
			if len(snip) > 200 {
				snip = snip[:200]
			}
			fmt.Fprintf(os.Stderr, "[%s] codex event type seen: %s | %s\n", provider.AppName, eventType, string(snip))
		}

		switch eventType {
		case "response.output_text.delta":
			delta, _ := event["delta"].(string)
			if delta != "" {
				// Close any reasoning block currently open — text follows
				// reasoning in upstream order.
				for rItemID, open := range reasoningItemOpen {
					if !open || reasoningItemClosed[rItemID] {
						continue
					}
					codexWriteSSE(w, "content_block_stop", codexBlockStop{
						Type: "content_block_stop", Index: reasoningItemBlockIdx[rItemID],
					})
					reasoningItemClosed[rItemID] = true
					blockIndex++
				}
				// Lazy-start a text block
				if !textBlockStarted {
					codexWriteSSE(w, "content_block_start", codexBlockStart{
						Type:         "content_block_start",
						Index:        blockIndex,
						ContentBlock: codexAnthropicBlock{Type: "text", Text: ""},
					})
					textBlockStarted = true
				}
				codexWriteSSE(w, "content_block_delta", codexBlockDelta{
					Type:  "content_block_delta",
					Index: blockIndex,
					Delta: map[string]any{"type": "text_delta", "text": delta},
				})
				if flusher != nil {
					flusher.Flush()
				}
			}

		case "response.reasoning_summary_part.added":
			// First signal a reasoning item is producing summary output.
			// We open the thinking block here so subsequent text deltas
			// have a target index. Some upstreams skip this event and go
			// straight to summary_text.delta — handled there too via lazy
			// open.
			itemID, _ := event["item_id"].(string)
			openReasoningBlock(itemID)
			if flusher != nil {
				flusher.Flush()
			}

		case "response.reasoning_summary_text.delta":
			itemID, _ := event["item_id"].(string)
			delta, _ := event["delta"].(string)
			if delta == "" {
				continue
			}
			openReasoningBlock(itemID)
			bIdx, ok := reasoningItemBlockIdx[itemID]
			if !ok {
				continue
			}
			codexWriteSSE(w, "content_block_delta", codexBlockDelta{
				Type:  "content_block_delta",
				Index: bIdx,
				Delta: map[string]any{"type": "thinking_delta", "thinking": delta},
			})
			if flusher != nil {
				flusher.Flush()
			}

		case "response.reasoning_summary_text.done":
			// Soft-flush; the .done for the whole reasoning item is
			// `output_item.done` (handled below). If a single summary part
			// finishes but more parts follow on the same item, we leave the
			// block open and continue appending.
			itemID, _ := event["item_id"].(string)
			text, _ := event["text"].(string)
			if itemID == "" || text == "" {
				continue
			}
			// If the upstream skipped streaming deltas and only sent .done,
			// emit the full text as a single delta.
			openReasoningBlock(itemID)
			if reasoningItemClosed[itemID] {
				continue
			}
			// (No-op if deltas already streamed the same text — duplicate
			// emission would double the thinking content. Anthropic's spec
			// only allows incremental deltas, so we skip the .done body.)

		case "response.reasoning_summary_part.done":
			// Boundary between summary parts within the same reasoning item.
			// Keep the block open; treat the full reasoning item close as
			// the boundary instead.
			_ = event

		case "response.output_item.added":
			// Start a new block as soon as the item is announced so that
			// subsequent `function_call_arguments.delta` events can stream into
			// it. Previously we only handled `.done`, which made every tool call
			// non-streaming (bad UX for large argument payloads).
			item, _ := event["item"].(map[string]any)
			if item == nil {
				continue
			}
			itemType, _ := item["type"].(string)
			if itemType == "reasoning" {
				// Reasoning items don't have streamed args; lazy-open on
				// first summary delta. No-op here.
				continue
			}
			if itemType != "function_call" {
				continue
			}
			itemID, _ := item["id"].(string)
			if itemID == "" {
				continue
			}
			if _, exists := toolItemBlockIdx[itemID]; exists {
				continue // already started
			}
			// Close any open text block first
			if textBlockStarted {
				codexWriteSSE(w, "content_block_stop", codexBlockStop{
					Type: "content_block_stop", Index: blockIndex,
				})
				blockIndex++
				textBlockStarted = false
			}
			// Close any open reasoning block — tool_use comes after.
			for rItemID, open := range reasoningItemOpen {
				if !open || reasoningItemClosed[rItemID] {
					continue
				}
				codexWriteSSE(w, "content_block_stop", codexBlockStop{
					Type: "content_block_stop", Index: reasoningItemBlockIdx[rItemID],
				})
				reasoningItemClosed[rItemID] = true
				blockIndex++
			}
			callID, _ := item["call_id"].(string)
			name, _ := item["name"].(string)
			toolUseID := "toolu_" + callID
			if callID == "" {
				toolUseID = "toolu_" + codexRandomID()
			}
			codexWriteSSE(w, "content_block_start", codexBlockStart{
				Type:  "content_block_start",
				Index: blockIndex,
				ContentBlock: codexAnthropicBlock{
					Type:  "tool_use",
					ID:    toolUseID,
					Name:  name,
					Input: json.RawMessage("{}"),
				},
			})
			toolItemBlockIdx[itemID] = blockIndex
			toolItemOrder = append(toolItemOrder, itemID)
			blockIndex++
			if flusher != nil {
				flusher.Flush()
			}

		case "response.function_call_arguments.delta":
			itemID, _ := event["item_id"].(string)
			delta, _ := event["delta"].(string)
			if itemID == "" || delta == "" {
				continue
			}
			// Accumulate; actual input_json_delta emit happens at block_stop
			// so we can validate the final JSON.
			toolItemArgs[itemID] += delta

		case "response.function_call_arguments.done":
			itemID, _ := event["item_id"].(string)
			if itemID == "" {
				continue
			}
			// Prefer the authoritative `arguments` field if present; fall back
			// to accumulated deltas.
			if args, ok := event["arguments"].(string); ok && args != "" {
				toolItemArgs[itemID] = args
			}
			if toolItemClosed[itemID] {
				continue
			}
			bIdx, ok := toolItemBlockIdx[itemID]
			if !ok {
				continue // added event was missed; output_item.done will handle it
			}
			args := codexValidatedToolArgs(toolItemArgs[itemID])
			codexWriteSSE(w, "content_block_delta", codexBlockDelta{
				Type:  "content_block_delta",
				Index: bIdx,
				Delta: map[string]any{
					"type":         "input_json_delta",
					"partial_json": args,
				},
			})
			codexWriteSSE(w, "content_block_stop", codexBlockStop{
				Type: "content_block_stop", Index: bIdx,
			})
			toolItemClosed[itemID] = true
			if flusher != nil {
				flusher.Flush()
			}

		case "response.output_item.done":
			// Fallback path when upstream doesn't emit the streaming events
			// above (older Codex backends). Handles function_call items that
			// weren't previously started.
			item, _ := event["item"].(map[string]any)
			if item == nil {
				continue
			}
			itemType, _ := item["type"].(string)
			if itemType == "reasoning" {
				itemID, _ := item["id"].(string)
				if itemID == "" {
					continue
				}
				if !reasoningItemOpen[itemID] || reasoningItemClosed[itemID] {
					continue
				}
				codexWriteSSE(w, "content_block_stop", codexBlockStop{
					Type: "content_block_stop", Index: reasoningItemBlockIdx[itemID],
				})
				reasoningItemClosed[itemID] = true
				blockIndex++
				if flusher != nil {
					flusher.Flush()
				}
				continue
			}
			if itemType != "function_call" {
				continue
			}
			itemID, _ := item["id"].(string)
			if itemID != "" && toolItemClosed[itemID] {
				continue // already handled via streaming path
			}

			// Close any open text block first
			if textBlockStarted {
				codexWriteSSE(w, "content_block_stop", codexBlockStop{
					Type: "content_block_stop", Index: blockIndex,
				})
				blockIndex++
				textBlockStarted = false
			}

			callID, _ := item["call_id"].(string)
			name, _ := item["name"].(string)
			arguments, _ := item["arguments"].(string)

			toolUseID := "toolu_" + callID
			if callID == "" {
				toolUseID = "toolu_" + codexRandomID()
			}

			var bIdx int
			if itemID != "" {
				if existing, ok := toolItemBlockIdx[itemID]; ok {
					bIdx = existing
				} else {
					bIdx = blockIndex
					toolItemBlockIdx[itemID] = bIdx
					toolItemOrder = append(toolItemOrder, itemID)
					codexWriteSSE(w, "content_block_start", codexBlockStart{
						Type:  "content_block_start",
						Index: bIdx,
						ContentBlock: codexAnthropicBlock{
							Type: "tool_use", ID: toolUseID, Name: name,
							Input: json.RawMessage("{}"),
						},
					})
					blockIndex++
				}
			} else {
				bIdx = blockIndex
				codexWriteSSE(w, "content_block_start", codexBlockStart{
					Type:  "content_block_start",
					Index: bIdx,
					ContentBlock: codexAnthropicBlock{
						Type: "tool_use", ID: toolUseID, Name: name,
						Input: json.RawMessage("{}"),
					},
				})
				blockIndex++
			}

			args := codexValidatedToolArgs(arguments)
			codexWriteSSE(w, "content_block_delta", codexBlockDelta{
				Type:  "content_block_delta",
				Index: bIdx,
				Delta: map[string]any{
					"type":         "input_json_delta",
					"partial_json": args,
				},
			})
			codexWriteSSE(w, "content_block_stop", codexBlockStop{
				Type: "content_block_stop", Index: bIdx,
			})
			if itemID != "" {
				toolItemClosed[itemID] = true
			}
			if flusher != nil {
				flusher.Flush()
			}

		case "response.completed":
			if respObj, ok := event["response"].(map[string]any); ok {
				if usage, ok := respObj["usage"].(map[string]any); ok {
					if ot, ok := usage["output_tokens"].(float64); ok {
						outputTokens = int(ot)
					}
				}
			}

		case "response.incomplete":
			// Upstream terminated early. Extract the authoritative reason so
			// downstream can distinguish max_tokens truncation from content
			// filter vs unknown. Previously fell through to end_turn,
			// silently hiding truncation.
			if respObj, ok := event["response"].(map[string]any); ok {
				if details, ok := respObj["incomplete_details"].(map[string]any); ok {
					if r, _ := details["reason"].(string); r != "" {
						incompleteReason = r
					}
				}
				if usage, ok := respObj["usage"].(map[string]any); ok {
					if ot, ok := usage["output_tokens"].(float64); ok {
						outputTokens = int(ot)
					}
				}
			}

		case "response.failed", "error":
			// Upstream reported a hard failure. Capture the message so we can
			// surface it as an Anthropic-format error event and stop cleanly.
			upstreamFailed = true
			if respObj, ok := event["response"].(map[string]any); ok {
				if errObj, ok := respObj["error"].(map[string]any); ok {
					if msg, _ := errObj["message"].(string); msg != "" {
						upstreamErrMsg = msg
					}
				}
			}
			if upstreamErrMsg == "" {
				if errObj, ok := event["error"].(map[string]any); ok {
					if msg, _ := errObj["message"].(string); msg != "" {
						upstreamErrMsg = msg
					}
				}
			}
			if upstreamErrMsg == "" {
				upstreamErrMsg = "codex upstream failed"
			}
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] codex SSE scanner error: %v\n", provider.AppName, err)
	}

	// Close any blocks that were left open (text, or tool_use blocks that
	// never got a function_call_arguments.done). This is the single shared
	// cleanup path so failure/truncation and normal completion both produce
	// a well-formed SSE tail.
	closeAllOpenBlocks()

	hasToolUse := len(toolItemOrder) > 0

	if upstreamFailed {
		// Emit Anthropic-format error event. CC treats this as a turn-level
		// failure and will not try to resume. We still send message_stop so
		// any partial content just rendered gets flushed.
		codexWriteSSE(w, "error", map[string]any{
			"type": "error",
			"error": map[string]string{
				"type":    "api_error",
				"message": upstreamErrMsg,
			},
		})
		codexWriteSSE(w, "message_stop", codexMsgStop{Type: "message_stop"})
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	stopReason := mapCodexStopReason(finishReason, incompleteReason, hasToolUse)
	// Diagnostic: log the inputs + mapping so we can see why downstream CC
	// sometimes records an empty stop_reason.
	fmt.Fprintf(os.Stderr, "[%s] codex stop_reason debug finishReason=%q incompleteReason=%q hasToolUse=%v → stop_reason=%q\n",
		provider.AppName, finishReason, incompleteReason, hasToolUse, stopReason)

	msgDelta := codexMsgDelta{Type: "message_delta", Usage: codexAnthropicUsage{OutputTokens: outputTokens}}
	msgDelta.Delta.StopReason = stopReason
	codexWriteSSE(w, "message_delta", msgDelta)
	codexWriteSSE(w, "message_stop", codexMsgStop{Type: "message_stop"})
	if flusher != nil {
		flusher.Flush()
	}
}
