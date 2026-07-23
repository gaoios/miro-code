package main

import (
	"strings"
)

// ── Alternate backend routing ──
//
// Helpers used by createSessionWithOpts and spawn paths to decide whether a
// requested session should run on the CC stream-json backend (default), the
// codex JSON-RPC app-server backend, or the Grok Build headless backend. These
// decisions are pure functions of the session opts + global config — no I/O,
// no side effects — so callers can compute them under their own locks safely.

// resolveBackendKind returns the BackendKind for a session given an explicit
// caller hint (from the API body / spawn flag) and the model name. Rules,
// in priority order:
//
//  1. explicit alternate backend or CC     → caller intent wins
//  3. agents.codex.enabled is true AND
//     (a) model has "codex/" prefix, OR
//     (b) model is a key in agents.codex.model_map
//     → codex (auto-route by model)
//  4. agents.grok.enabled is true AND
//     (a) model has "grok/" prefix, OR
//     (b) model is a key in agents.grok.model_map
//     → grok (auto-route by model)
//  5. otherwise                             → cc (default, current behavior)
//
// Round 4 deliberately treats absence of the codex binary as a config error
// surfaced at spawn time (spawnCodex returns an error, createSessionWithOpts
// propagates it). We don't downgrade-to-cc silently here because that would
// hide misconfiguration from users who explicitly asked for codex.
func resolveBackendKind(explicit BackendKind, model string) BackendKind {
	switch explicit {
	case BackendCodex:
		return BackendCodex
	case BackendGrok:
		return BackendGrok
	case BackendKimi:
		return BackendKimi
	case BackendAgy:
		return BackendAgy
	case BackendCC:
		return BackendCC
	}
	if codexEnabled && isCodexModel(model) {
		return BackendCodex
	}
	if grokEnabled && isGrokModel(model) {
		return BackendGrok
	}
	if kimiEnabled && isKimiModel(model) {
		return BackendKimi
	}
	if agyEnabled && isAgyModel(model) {
		return BackendAgy
	}
	return BackendCC
}

// isCodexModel reports whether a model name should auto-route to the codex
// backend. The two heuristics:
//
//   - "codex/" prefix (e.g. "codex/gpt-5.1-codex-max") — same convention as
//     "zai/" / "minimax/" / etc. that providerModelName already understands
//   - presence in agents.codex.model_map — explicit allowlist of weiran model
//     names that map to codex. Lets the user keep using "opus[1m]" as the
//     identifier in the UI/config while routing the wire calls to codex.
//
// Both checks are case-sensitive — model names are conventionally lowercase
// in this codebase and matching upper/lower would mask typos.
func isCodexModel(model string) bool {
	if model == "" {
		return false
	}
	if strings.HasPrefix(model, "codex/") {
		return true
	}
	if codexModelMap != nil {
		if _, ok := codexModelMap[model]; ok {
			return true
		}
	}
	return false
}

// codexResolveModel returns the codex-side model name the backend should pass
// to thread/start. Resolution order:
//
//  1. agents.codex.model_map[model]        (explicit override)
//  2. strip "codex/" prefix if present     (codex/gpt-5.1-codex-max → gpt-5.1-codex-max)
//  3. fall through unchanged
//
// Empty input returns empty (codex falls back to its own default in that
// case).
func codexResolveModel(model string) string {
	if model == "" {
		return ""
	}
	if codexModelMap != nil {
		if mapped, ok := codexModelMap[model]; ok && mapped != "" {
			return mapped
		}
	}
	if strings.HasPrefix(model, "codex/") {
		return strings.TrimPrefix(model, "codex/")
	}
	return model
}

// isGrokModel reports whether a model name should auto-route to the Grok
// backend. The two heuristics mirror codex:
//
//   - "grok/" prefix (e.g. "grok/grok-4") to explicitly name the backend
//   - presence in agents.grok.model_map for UI/config aliases
func isGrokModel(model string) bool {
	if model == "" {
		return false
	}
	if strings.HasPrefix(model, "grok/") {
		return true
	}
	if grokModelMap != nil {
		if _, ok := grokModelMap[model]; ok {
			return true
		}
	}
	return false
}

// grokResolveModel returns the Grok-side model name to pass to Grok Build.
// Resolution order:
//
//  1. agents.grok.model_map[model]
//  2. strip "grok/" prefix
//  3. fall through unchanged
func grokResolveModel(model string) string {
	if model == "" {
		return ""
	}
	if grokModelMap != nil {
		if mapped, ok := grokModelMap[model]; ok && mapped != "" {
			return mapped
		}
	}
	if strings.HasPrefix(model, "grok/") {
		return strings.TrimPrefix(model, "grok/")
	}
	return model
}

func isKimiModel(model string) bool {
	if model == "" {
		return false
	}
	if strings.HasPrefix(model, "kimi/") {
		return true
	}
	if kimiModelMap != nil {
		_, ok := kimiModelMap[model]
		return ok
	}
	return false
}

func kimiResolveModel(model string) string {
	if model == "" {
		return ""
	}
	if kimiModelMap != nil {
		if mapped, ok := kimiModelMap[model]; ok && mapped != "" {
			return mapped
		}
	}
	return strings.TrimPrefix(model, "kimi/")
}

func isAgyModel(model string) bool {
	if model == "" {
		return false
	}
	if strings.HasPrefix(model, "agy/") || strings.HasPrefix(model, "antigravity/") {
		return true
	}
	if agyModelMap != nil {
		_, ok := agyModelMap[model]
		return ok
	}
	return false
}

func agyResolveModel(model string) string {
	if model == "" {
		return ""
	}
	if agyModelMap != nil {
		if mapped, ok := agyModelMap[model]; ok && mapped != "" {
			return mapped
		}
	}
	model = strings.TrimPrefix(model, "agy/")
	return strings.TrimPrefix(model, "antigravity/")
}

// resolveCreateModel applies the server's default interactive model only when
// the selected backend uses the CC/provider model namespace. Alternate backends own
// their model defaults; passing the Claude default (for example "opus[1m]") to
// those CLIs produces invalid backend-native model flags.
func resolveCreateModel(model, category string, backendKind BackendKind, defaultInteractive string) string {
	if model != "" {
		return model
	}
	if category != "" && category != CategoryInteractive {
		return ""
	}
	if backendKind != "" && backendKind != BackendCC {
		return ""
	}
	return defaultInteractive
}
