package main

import (
	"strings"
	"testing"
)

func TestBuildWorkerDispatchRulesDescribesKimiAsFrontendSpecialist(t *testing.T) {
	oldCodex, oldGrok, oldKimi, oldAgy := codexEnabled, grokEnabled, kimiEnabled, agyEnabled
	t.Cleanup(func() { codexEnabled, grokEnabled, kimiEnabled, agyEnabled = oldCodex, oldGrok, oldKimi, oldAgy })
	codexEnabled, grokEnabled, kimiEnabled = false, false, true

	rules := buildWorkerDispatchRules()
	for _, want := range []string{
		"CEO Worker Dispatch Policy",
		"Kimi",
		"K3",
		"frontend page specialist",
		"frontend",
		"presentation",
		"kimi-code/k3",
		"soul spawn --bare --backend kimi",
	} {
		if !strings.Contains(rules, want) {
			t.Fatalf("dispatch rules missing %q:\n%s", want, rules)
		}
	}
	if strings.Contains(rules, "scarce quota") {
		t.Fatalf("dispatch rules still describe Kimi quota as scarce:\n%s", rules)
	}
	if strings.Contains(rules, "primary frontend presentation and visual experience worker") {
		t.Fatalf("dispatch rules incorrectly describe Kimi as the primary worker:\n%s", rules)
	}
}

func TestBuildWorkerDispatchRulesEmptyWhenNoWorkersEnabled(t *testing.T) {
	oldCodex, oldGrok, oldKimi, oldAgy := codexEnabled, grokEnabled, kimiEnabled, agyEnabled
	t.Cleanup(func() { codexEnabled, grokEnabled, kimiEnabled, agyEnabled = oldCodex, oldGrok, oldKimi, oldAgy })
	codexEnabled, grokEnabled, kimiEnabled, agyEnabled = false, false, false, false
	if got := buildWorkerDispatchRules(); got != "" {
		t.Fatalf("rules = %q, want empty", got)
	}
}

func TestBuildWorkerDispatchRulesDescribesAntigravitySpecialist(t *testing.T) {
	oldCodex, oldGrok, oldKimi, oldAgy := codexEnabled, grokEnabled, kimiEnabled, agyEnabled
	t.Cleanup(func() { codexEnabled, grokEnabled, kimiEnabled, agyEnabled = oldCodex, oldGrok, oldKimi, oldAgy })
	codexEnabled, grokEnabled, kimiEnabled, agyEnabled = false, false, false, true

	rules := buildWorkerDispatchRules()
	for _, want := range []string{
		"Antigravity / Gemini 3.1 Pro — synthesis and alternate-perspective specialist",
		"gemini-3.1-pro-low",
		"soul spawn --bare --backend agy",
	} {
		if !strings.Contains(rules, want) {
			t.Fatalf("dispatch rules missing %q:\n%s", want, rules)
		}
	}
}

func TestBuildWorkerDispatchRulesIncludesLocalHintsAndToolCLIs(t *testing.T) {
	oldCodex, oldGrok, oldKimi, oldAgy := codexEnabled, grokEnabled, kimiEnabled, agyEnabled
	oldCodexHint, oldGrokHint := codexDispatchHint, grokDispatchHint
	oldKimiHint, oldAgyHint, oldTools := kimiDispatchHint, agyDispatchHint, toolCLIs
	t.Cleanup(func() {
		codexEnabled, grokEnabled, kimiEnabled, agyEnabled = oldCodex, oldGrok, oldKimi, oldAgy
		codexDispatchHint, grokDispatchHint = oldCodexHint, oldGrokHint
		kimiDispatchHint, agyDispatchHint, toolCLIs = oldKimiHint, oldAgyHint, oldTools
	})
	codexEnabled, grokEnabled, kimiEnabled, agyEnabled = true, false, false, false
	codexDispatchHint = "Use as the high-capacity default implementation worker."
	toolCLIs = map[string]toolCLIConfig{
		"meoo": {
			Enabled:      true,
			Binary:       "meoo",
			Capabilities: []string{"build", "deploy static sites"},
			DispatchHint: "Use only when the task targets the Meoo platform.",
		},
		"jimeng": {Enabled: false, Binary: "jimeng"},
	}

	rules := buildWorkerDispatchRules()
	for _, want := range []string{
		"Local dispatch guidance: Use as the high-capacity default implementation worker.",
		"Available tool CLIs",
		"meoo",
		"deploy static sites",
		"Use only when the task targets the Meoo platform.",
	} {
		if !strings.Contains(rules, want) {
			t.Fatalf("dispatch rules missing %q:\n%s", want, rules)
		}
	}
	if strings.Contains(rules, "jimeng") {
		t.Fatalf("disabled tool CLI leaked into dispatch rules:\n%s", rules)
	}
}

func TestBuildPromptInjectsWorkerDispatchPolicyForCEO(t *testing.T) {
	requireWorkspace(t)
	oldCodex, oldGrok, oldKimi, oldAgy := codexEnabled, grokEnabled, kimiEnabled, agyEnabled
	t.Cleanup(func() { codexEnabled, grokEnabled, kimiEnabled, agyEnabled = oldCodex, oldGrok, oldKimi, oldAgy })
	codexEnabled, grokEnabled, kimiEnabled, agyEnabled = true, true, true, true

	result := buildPrompt()
	if !strings.Contains(result.content, "CEO Worker Dispatch Policy") {
		t.Fatal("built prompt does not contain CEO worker dispatch policy")
	}
	for _, want := range []string{
		"Codex — engineering implementation worker",
		"Claude's core job is logical analysis, task decomposition",
		"Kimi K3 — frontend page specialist",
		"Antigravity / Gemini 3.1 Pro — synthesis and alternate-perspective specialist",
		"soul spawn --bare --backend agy --model gemini-3.1-pro-low",
	} {
		if !strings.Contains(result.content, want) {
			t.Fatalf("built prompt does not contain %q", want)
		}
	}
}
