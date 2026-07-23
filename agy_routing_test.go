package main

import "testing"

func TestResolveBackendKindAgyExplicitAndAuto(t *testing.T) {
	oldEnabled, oldModelMap := agyEnabled, agyModelMap
	t.Cleanup(func() { agyEnabled, agyModelMap = oldEnabled, oldModelMap })
	agyEnabled = true
	agyModelMap = map[string]string{"antigravity-pro": "gemini-3.1-pro-low"}

	cases := []struct {
		name     string
		explicit BackendKind
		model    string
		want     BackendKind
	}{
		{name: "explicit agy wins", explicit: BackendAgy, model: "opus", want: BackendAgy},
		{name: "explicit cc wins over agy prefix", explicit: BackendCC, model: "agy/gemini-3.1-pro-low", want: BackendCC},
		{name: "auto agy prefix", model: "agy/gemini-3.1-pro-low", want: BackendAgy},
		{name: "auto antigravity prefix", model: "antigravity/gemini-3.1-pro-low", want: BackendAgy},
		{name: "auto agy model map", model: "antigravity-pro", want: BackendAgy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveBackendKind(tc.explicit, tc.model); got != tc.want {
				t.Fatalf("resolveBackendKind(%q, %q) = %q, want %q", tc.explicit, tc.model, got, tc.want)
			}
		})
	}
}

func TestAgyResolveModel(t *testing.T) {
	oldModelMap := agyModelMap
	t.Cleanup(func() { agyModelMap = oldModelMap })
	agyModelMap = map[string]string{"worker": "gemini-3.1-pro-low"}

	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"worker", "gemini-3.1-pro-low"},
		{"agy/gemini-3.1-pro-low", "gemini-3.1-pro-low"},
		{"antigravity/gemini-3.1-pro-high", "gemini-3.1-pro-high"},
	} {
		if got := agyResolveModel(tc.in); got != tc.want {
			t.Fatalf("agyResolveModel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
