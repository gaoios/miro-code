package main

import (
	"strings"
	"testing"
)

func TestResolveBackendKindOpencodeExplicitAndAuto(t *testing.T) {
	oldEnabled := opencodeEnabled
	oldModelMap := opencodeModelMap
	t.Cleanup(func() {
		opencodeEnabled = oldEnabled
		opencodeModelMap = oldModelMap
	})
	opencodeEnabled = true
	opencodeModelMap = map[string]string{"opencode-worker": "opencode-go/deepseek-flash"}

	cases := []struct {
		name     string
		explicit BackendKind
		model    string
		want     BackendKind
	}{
		{name: "explicit opencode wins", explicit: BackendOpencode, model: "opus", want: BackendOpencode},
		{name: "explicit cc wins over opencode prefix", explicit: BackendCC, model: "opencode/opencode-go/deepseek-flash", want: BackendCC},
		{name: "auto opencode prefix", model: "opencode/opencode-go/deepseek-flash", want: BackendOpencode},
		{name: "auto opencode model map", model: "opencode-worker", want: BackendOpencode},
		{name: "plain model remains cc", model: "opus", want: BackendCC},
		{name: "native provider prefix is not a routing prefix", model: "opencode-go/deepseek-flash", want: BackendCC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveBackendKind(tc.explicit, tc.model); got != tc.want {
				t.Fatalf("resolveBackendKind(%q, %q) = %q, want %q", tc.explicit, tc.model, got, tc.want)
			}
		})
	}
	opencodeEnabled = false
	if got := resolveBackendKind("", "opencode/opencode-go/deepseek-flash"); got != BackendCC {
		t.Fatalf("disabled opencode auto-route = %q, want cc", got)
	}
}

func TestOpencodeResolveModel(t *testing.T) {
	oldModelMap := opencodeModelMap
	t.Cleanup(func() { opencodeModelMap = oldModelMap })
	opencodeModelMap = map[string]string{"worker": "opencode-go/deepseek-flash"}

	cases := []struct{ in, want string }{
		{"", ""},
		{"worker", "opencode-go/deepseek-flash"},
		{"opencode/opencode-go/deepseek-flash", "opencode-go/deepseek-flash"},
		{"other-provider/model", "other-provider/model"},
	}
	for _, tc := range cases {
		if got := opencodeResolveModel(tc.in); got != tc.want {
			t.Fatalf("opencodeResolveModel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolveSpawnModelPreservesOpencodeNativeModel(t *testing.T) {
	got, err := resolveSpawnModel("opencode-go/deepseek-flash", "opencode")
	if err != nil {
		t.Fatalf("resolveSpawnModel returned error: %v", err)
	}
	if got != "opencode-go/deepseek-flash" {
		t.Fatalf("resolveSpawnModel = %q, want opencode-go/deepseek-flash", got)
	}
}

func TestOpencodeWorkerDispatchRule(t *testing.T) {
	oldCodex, oldGrok, oldKimi, oldAgy := codexEnabled, grokEnabled, kimiEnabled, agyEnabled
	oldEnabled, oldHint, oldTools := opencodeEnabled, opencodeDispatchHint, toolCLIs
	t.Cleanup(func() {
		codexEnabled, grokEnabled, kimiEnabled, agyEnabled = oldCodex, oldGrok, oldKimi, oldAgy
		opencodeEnabled, opencodeDispatchHint, toolCLIs = oldEnabled, oldHint, oldTools
	})
	codexEnabled, grokEnabled, kimiEnabled, agyEnabled = false, false, false, false
	opencodeEnabled, opencodeDispatchHint, toolCLIs = true, "Use for bounded verification.", nil
	rules := buildWorkerDispatchRules()
	for _, want := range []string{"opencode — multi-model execution and verification worker", "soul spawn --bare --backend opencode --model <native-model-id>", "agents.opencode.model_map", "Use for bounded verification."} {
		if !strings.Contains(rules, want) {
			t.Fatalf("dispatch rules missing %q", want)
		}
	}
	opencodeEnabled = false
	if got := buildWorkerDispatchRules(); got != "" {
		t.Fatalf("disabled workers still emit rules: %q", got)
	}
}
