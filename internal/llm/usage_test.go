package llm

import "testing"

func TestUsageUsable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage *Usage
		want  bool
	}{
		{"missing", nil, false},
		{"zero", &Usage{}, true},
		{"measured", &Usage{InputTokens: 10, OutputTokens: 2}, true},
		{"negative input", &Usage{InputTokens: -1}, false},
		{"negative output", &Usage{InputTokens: 10, OutputTokens: -1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.usage.Usable(); got != tc.want {
				t.Fatalf("Usable()=%v, want %v", got, tc.want)
			}
		})
	}
}
