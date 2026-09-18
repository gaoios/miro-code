package app

import (
	"encoding/json"
	"testing"
)

func TestExtractAssistantContextTotal(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int64
	}{
		{
			name: "anthropic full usage",
			raw: `{"type":"assistant","message":{"usage":{` +
				`"input_tokens":1000,"cache_creation_input_tokens":500,` +
				`"cache_read_input_tokens":9500,"output_tokens":200}}}`,
			want: 11000, // 1000 + 500 + 9500
		},
		{
			name: "only input tokens",
			raw:  `{"type":"assistant","message":{"usage":{"input_tokens":3000,"output_tokens":50}}}`,
			want: 3000,
		},
		{
			name: "no usage field",
			raw:  `{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`,
			want: 0,
		},
		{
			name: "malformed json",
			raw:  `not json`,
			want: 0,
		},
		{
			name: "empty message",
			raw:  `{}`,
			want: 0,
		},
		{
			name: "cache-heavy (third-party translated)",
			raw: `{"type":"assistant","message":{"usage":{` +
				`"input_tokens":50,"cache_creation_input_tokens":0,` +
				`"cache_read_input_tokens":128000}}}`,
			want: 128050,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractAssistantContextTotal(json.RawMessage(tc.raw))
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestUpdatePeakContext(t *testing.T) {
	sess := &serverSession{}

	// Initial update
	sess.updatePeakContext(1000)
	if sess.PeakContextTokens != 1000 {
		t.Errorf("after first update: got %d, want 1000", sess.PeakContextTokens)
	}

	// Higher value raises peak
	sess.updatePeakContext(5000)
	if sess.PeakContextTokens != 5000 {
		t.Errorf("after raise: got %d, want 5000", sess.PeakContextTokens)
	}

	// Lower value is ignored (monotonic)
	sess.updatePeakContext(3000)
	if sess.PeakContextTokens != 5000 {
		t.Errorf("after lower input: got %d, want 5000 (monotonic)", sess.PeakContextTokens)
	}

	// Zero/negative ignored
	sess.updatePeakContext(0)
	sess.updatePeakContext(-100)
	if sess.PeakContextTokens != 5000 {
		t.Errorf("after 0/neg: got %d, want 5000", sess.PeakContextTokens)
	}
}
