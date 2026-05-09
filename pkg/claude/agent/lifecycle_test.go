package agent

import (
	"strings"
	"testing"
)

func TestFormatTokens(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{47, "47"},
		{999, "999"},
		{1_000, "1k"},
		{1_500, "1k"}, // truncates by integer division — fine, this is an estimate
		{130_000, "130k"},
		{999_999, "999k"},
		{1_000_000, "1.0M"},
		{1_300_000, "1.3M"},
	}
	for _, tt := range tests {
		got := formatTokens(tt.in)
		if got != tt.want {
			t.Errorf("formatTokens(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestIsValidFollowUp mirrors the daemon-side check; the daemon is the
// security boundary, so its TestIsValidFollowUp is the authoritative
// spec — this CLI mirror just keeps a fast local error path in sync.
func TestIsValidFollowUp(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		// --- accepted: prose ---
		{"plain text", "now write up your findings", true},
		{"slash inside", "save notes to /tmp/foo.md", true},
		{"max length", strings.Repeat("a", 4096), true},

		// --- rejected ---
		{"empty", "", false},
		{"oversize 4097", strings.Repeat("a", 4097), false},
		{"newline", "first\nsecond", false},
		{"tab", "before\tafter", false},
		{"DEL", "before\x7fafter", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isValidFollowUp(tt.in); got != tt.want {
				t.Errorf("isValidFollowUp(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
