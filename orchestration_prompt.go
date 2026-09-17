package main

import (
	"sort"
	"strings"
)

// buildWorkerDispatchRules gives the Claude supervisor a concise inventory of
// enabled workers. It is deliberately capability- and budget-aware: enabling a
// backend does not mean every task should be sent to it.
func buildWorkerDispatchRules() string {
	if !codexEnabled && !grokEnabled && !kimiEnabled && !agyEnabled && !opencodeEnabled && !hasEnabledToolCLI() {
		return ""
	}

	var b strings.Builder
	b.WriteString(`# === CEO Worker Dispatch Policy ===

You are the CEO/supervisor and remain accountable for the final result. Claude's core job is logical analysis, task decomposition, routing, synthesis, and final acceptance. Delegate only when a worker's specialty materially improves the outcome. Do not dispatch work merely to keep every worker busy. Give each worker a bounded task, project path, expected artifact, and verification criteria; then review its handoff before accepting it.

Worker tasks use the running Soul server:
` + "`soul spawn --bare --backend <backend> --model <native-model> --project <absolute-path> \"<task>\" --wait`" + `

`)
	if codexEnabled {
		writeWorkerRule(&b, "- **Codex — engineering implementation worker**: use for repository reading, code changes, backend and frontend engineering, tests, debugging, refactoring, infrastructure, automation, and sustained multi-step execution.", codexDispatchHint)
	}
	if grokEnabled {
		writeWorkerRule(&b, "- **Grok — independent challenge/review worker**: use for adversarial review, risk discovery, alternate approaches, and a second opinion. Keep it independent from the implementer's reasoning.", grokDispatchHint)
	}
	if kimiEnabled {
		writeWorkerRule(&b, "- **Kimi K3 — frontend page specialist**: use the native model `kimi-code/k3` for visually demanding frontend pages and presentation work: visual direction, information hierarchy, layout, typography, color systems, responsive UI, interaction polish, data visualization, landing pages, dashboards, single-file HTML, and browser-based visual QA. Invoke with `soul spawn --bare --backend kimi --model kimi-code/k3 ...`.", kimiDispatchHint)
	}
	if agyEnabled {
		writeWorkerRule(&b, "- **Antigravity / Gemini 3.1 Pro — synthesis and alternate-perspective specialist**: use native model `gemini-3.1-pro-low` for long-context synthesis, comparing competing plans, organizing research, analyzing multimodal or Google-ecosystem material, and producing an independent alternative perspective. Invoke with `soul spawn --bare --backend agy --model gemini-3.1-pro-low ...`.", agyDispatchHint)
	}
	if opencodeEnabled {
		writeWorkerRule(&b, "- **opencode — DeepSeek-backed execution and verification worker**: use for bounded implementation, test execution, and independent verification when its capability and budget fit the task. Invoke with `soul spawn --bare --backend opencode --model opencode-go/deepseek-flash ...`; model_map values are native provider/model IDs and pass through unchanged.", opencodeDispatchHint)
	}
	writeToolCLIRules(&b)
	b.WriteString("\nDefault routing: Claude analyzes and decomposes the request; implementation goes to the best enabled engineering worker; Kimi K3 takes visually demanding frontend presentation; Antigravity provides Gemini synthesis or an alternate perspective; Grok checks risks when an independent challenge is useful; Claude integrates and accepts the final result. A tool CLI is not a conversational worker: assign the task to a worker and explicitly tell that worker which registered tool to invoke.\n")
	return b.String()
}

func writeWorkerRule(b *strings.Builder, base, hint string) {
	b.WriteString(base)
	if hint = strings.TrimSpace(hint); hint != "" {
		b.WriteString(" Local dispatch guidance: ")
		b.WriteString(hint)
	}
	b.WriteByte('\n')
}

func hasEnabledToolCLI() bool {
	for _, tool := range toolCLIs {
		if tool.Enabled {
			return true
		}
	}
	return false
}

func writeToolCLIRules(b *strings.Builder) {
	names := make([]string, 0, len(toolCLIs))
	for name, tool := range toolCLIs {
		if tool.Enabled {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return
	}
	sort.Strings(names)
	b.WriteString("\nAvailable tool CLIs (invoke from a worker in the shared project environment):\n")
	for _, name := range names {
		tool := toolCLIs[name]
		binary := strings.TrimSpace(tool.Binary)
		if binary == "" {
			binary = name
		}
		b.WriteString("- **")
		b.WriteString(name)
		b.WriteString("** (`")
		b.WriteString(binary)
		b.WriteString("`)")
		if len(tool.Capabilities) > 0 {
			b.WriteString(": ")
			b.WriteString(strings.Join(tool.Capabilities, ", "))
		}
		if hint := strings.TrimSpace(tool.DispatchHint); hint != "" {
			b.WriteString(". Usage guidance: ")
			b.WriteString(hint)
		}
		b.WriteByte('\n')
	}
}
