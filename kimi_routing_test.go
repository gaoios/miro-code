package main

import "testing"

func TestResolveBackendKindKimiExplicitAndAuto(t *testing.T) {
	oldEnabled := kimiEnabled
	oldModelMap := kimiModelMap
	t.Cleanup(func() {
		kimiEnabled = oldEnabled
		kimiModelMap = oldModelMap
	})
	kimiEnabled = true
	kimiModelMap = map[string]string{"kimi-worker": "kimi-k2.5"}

	cases := []struct {
		name     string
		explicit BackendKind
		model    string
		want     BackendKind
	}{
		{name: "explicit kimi wins", explicit: BackendKimi, model: "opus", want: BackendKimi},
		{name: "explicit cc wins over kimi prefix", explicit: BackendCC, model: "kimi/kimi-k2.5", want: BackendCC},
		{name: "auto kimi prefix", model: "kimi/kimi-k2.5", want: BackendKimi},
		{name: "auto kimi model map", model: "kimi-worker", want: BackendKimi},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveBackendKind(tc.explicit, tc.model); got != tc.want {
				t.Fatalf("resolveBackendKind(%q, %q) = %q, want %q", tc.explicit, tc.model, got, tc.want)
			}
		})
	}
}

func TestKimiResolveModel(t *testing.T) {
	oldModelMap := kimiModelMap
	t.Cleanup(func() { kimiModelMap = oldModelMap })
	kimiModelMap = map[string]string{"worker": "kimi-k2.5"}

	cases := []struct{ in, want string }{
		{"", ""},
		{"worker", "kimi-k2.5"},
		{"kimi/kimi-k2.5", "kimi-k2.5"},
		{"kimi-k2-thinking", "kimi-k2-thinking"},
	}
	for _, tc := range cases {
		if got := kimiResolveModel(tc.in); got != tc.want {
			t.Fatalf("kimiResolveModel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolveSpawnModelPreservesKimiNativeModel(t *testing.T) {
	got, err := resolveSpawnModel("kimi-k2.5", "kimi")
	if err != nil {
		t.Fatalf("resolveSpawnModel returned error: %v", err)
	}
	if got != "kimi-k2.5" {
		t.Fatalf("resolveSpawnModel = %q, want kimi-k2.5", got)
	}
}
