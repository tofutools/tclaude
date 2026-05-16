package agent

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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
		assert.Equal(t, tt.want, got, "formatTokens(%d)", tt.in)
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
			got := isValidFollowUp(tt.in)
			assert.Equal(t, tt.want, got, "isValidFollowUp(%q)", tt.in)
		})
	}
}

// TestIsValidInitialMessage mirrors the daemon-side check in
// agentd/handlers_test.go; the daemon is the security boundary, so its
// test is the authoritative spec — this CLI mirror keeps the fast
// local error path in sync. The brief rides in the new agent's inbox
// (a SQLite row), not a tmux pane, so newlines/tabs are fine and the
// cap is the generous MaxInitialMessageBytes (16384) rather than the
// follow-up's tmux-bound 4096.
func TestIsValidInitialMessage(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		// --- accepted ---
		{"empty means no brief", "", true},
		{"plain text", "review the auth module", true},
		{"newline", "first line\nsecond line", true},
		{"tab", "before\tafter", true},
		{"over the retired 4096 cap", strings.Repeat("a", 8000), true},
		{"max length", strings.Repeat("a", MaxInitialMessageBytes), true},

		// --- rejected ---
		{"oversize", strings.Repeat("a", MaxInitialMessageBytes+1), false},
		{"carriage return", "before\rafter", false},
		{"NUL", "before\x00after", false},
		{"DEL", "before\x7fafter", false},
		{"escape", "before\x1bafter", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isValidInitialMessage(tt.in), "isValidInitialMessage(%q)", tt.in)
		})
	}
}
