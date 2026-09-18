package app

import "testing"

func TestResolveBackendKindGrokExplicitAndAuto(t *testing.T) {
	oldCodexEnabled := codexEnabled
	oldCodexModelMap := codexModelMap
	oldGrokEnabled := grokEnabled
	oldGrokModelMap := grokModelMap
	t.Cleanup(func() {
		codexEnabled = oldCodexEnabled
		codexModelMap = oldCodexModelMap
		grokEnabled = oldGrokEnabled
		grokModelMap = oldGrokModelMap
	})

	codexEnabled = true
	codexModelMap = map[string]string{"codex-alias": "gpt-5.1-codex-max"}
	grokEnabled = true
	grokModelMap = map[string]string{"grok-alias": "grok-4-fast"}

	cases := []struct {
		name     string
		explicit BackendKind
		model    string
		want     BackendKind
	}{
		{name: "explicit grok wins", explicit: BackendGrok, model: "opus", want: BackendGrok},
		{name: "explicit cc wins over grok prefix", explicit: BackendCC, model: "grok/grok-4", want: BackendCC},
		{name: "auto grok prefix", model: "grok/grok-4", want: BackendGrok},
		{name: "auto grok model map", model: "grok-alias", want: BackendGrok},
		{name: "auto codex prefix still routes codex", model: "codex/gpt-5.1-codex-max", want: BackendCodex},
		{name: "auto codex model map still routes codex", model: "codex-alias", want: BackendCodex},
		{name: "default remains cc", model: "opus[1m]", want: BackendCC},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveBackendKind(tc.explicit, tc.model); got != tc.want {
				t.Fatalf("resolveBackendKind(%q, %q) = %q, want %q", tc.explicit, tc.model, got, tc.want)
			}
		})
	}
}

func TestResolveBackendKindGrokDisabledDoesNotAutoRoute(t *testing.T) {
	oldGrokEnabled := grokEnabled
	oldGrokModelMap := grokModelMap
	t.Cleanup(func() {
		grokEnabled = oldGrokEnabled
		grokModelMap = oldGrokModelMap
	})

	grokEnabled = false
	grokModelMap = map[string]string{"grok-alias": "grok-4-fast"}

	if got := resolveBackendKind("", "grok/grok-4"); got != BackendCC {
		t.Fatalf("disabled grok prefix routed to %q, want cc", got)
	}
	if got := resolveBackendKind("", "grok-alias"); got != BackendCC {
		t.Fatalf("disabled grok model map routed to %q, want cc", got)
	}
}

func TestGrokResolveModel(t *testing.T) {
	oldGrokModelMap := grokModelMap
	t.Cleanup(func() { grokModelMap = oldGrokModelMap })
	grokModelMap = map[string]string{"research": "grok-4-fast"}

	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"research", "grok-4-fast"},
		{"grok/grok-4", "grok-4"},
		{"grok-3-mini", "grok-3-mini"},
	}
	for _, tc := range cases {
		if got := grokResolveModel(tc.in); got != tc.want {
			t.Fatalf("grokResolveModel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolveCreateModelSkipsClaudeDefaultForExplicitAltBackends(t *testing.T) {
	cases := []struct {
		name        string
		model       string
		category    string
		backendKind BackendKind
		want        string
	}{
		{name: "explicit model wins", model: "grok-4", backendKind: BackendGrok, want: "grok-4"},
		{name: "cc interactive uses default", backendKind: BackendCC, want: "opus[1m]"},
		{name: "auto interactive uses default", backendKind: "", want: "opus[1m]"},
		{name: "grok interactive keeps empty", backendKind: BackendGrok, want: ""},
		{name: "codex interactive keeps empty", backendKind: BackendCodex, want: ""},
		{name: "spawn category does not use default", category: "spawn", backendKind: "", want: ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := resolveCreateModel(tc.model, tc.category, tc.backendKind, "opus[1m]")
			if got != tc.want {
				t.Fatalf("resolveCreateModel() = %q, want %q", got, tc.want)
			}
		})
	}
}
