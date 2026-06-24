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

// TestFormatContextPct covers the group-listing CTX% cell: a member
// whose statusline hook has never fired (HasSnapshot false) renders "—"
// rather than a misleading 0%, while a reported snapshot renders its
// rounded percentage.
func TestFormatContextPct(t *testing.T) {
	tests := []struct {
		name string
		in   groupContextEntry
		want string
	}{
		{"no snapshot", groupContextEntry{HasSnapshot: false, ContextPct: 0}, "—"},
		{"no snapshot ignores stray pct", groupContextEntry{HasSnapshot: false, ContextPct: 42}, "—"},
		{"genuine zero", groupContextEntry{HasSnapshot: true, ContextPct: 0}, "0%"},
		{"reported", groupContextEntry{HasSnapshot: true, ContextPct: 72.4}, "72%"},
		{"rounds up", groupContextEntry{HasSnapshot: true, ContextPct: 91.6}, "92%"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, formatContextPct(tt.in), "formatContextPct(%+v)", tt.in)
		})
	}
}

// TestFormatContextTokens covers the group-listing TOKENS cell:
// "<used> / <window>" when the absolute counts are present, empty
// otherwise (no snapshot, or pre-v2.1.132 CC that reports only a
// percentage).
func TestFormatContextTokens(t *testing.T) {
	tests := []struct {
		name string
		in   groupContextEntry
		want string
	}{
		{"no snapshot", groupContextEntry{HasSnapshot: false}, ""},
		{"no window size", groupContextEntry{HasSnapshot: true, TokensInput: 100, ContextWindowSize: 0}, ""},
		{"no tokens", groupContextEntry{HasSnapshot: true, ContextWindowSize: 200000}, ""},
		{
			"populated",
			groupContextEntry{HasSnapshot: true, TokensInput: 110000, TokensOutput: 10000, ContextWindowSize: 200000},
			"120k / 200k",
		},
		{
			"millions window",
			groupContextEntry{HasSnapshot: true, TokensInput: 600000, TokensOutput: 50000, ContextWindowSize: 1000000},
			"650k / 1.0M",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, formatContextTokens(tt.in), "formatContextTokens(%+v)", tt.in)
		})
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

// TestIsValidSpawnName mirrors the daemon-side spec
// (agentd.TestIsValidSpawnName). The daemon gate is authoritative; this
// CLI copy must agree with it so a name accepted here isn't rejected by
// the daemon (or vice versa).
func TestIsValidSpawnName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		// --- accepted ---
		{"empty (optional name)", "", true},
		{"plain alphanumeric", "abc123", true},
		{"hyphen", "code-reviewer", true},
		{"underscore", "code_reviewer", true},
		{"max length", strings.Repeat("a", MaxSpawnNameLen), true},

		// --- rejected ---
		{"oversize", strings.Repeat("a", MaxSpawnNameLen+1), false},
		{"space", "code reviewer", false},
		{"brackets", "[reviewer]", false},
		{"dot", "code.reviewer", false},
		{"slash", "code/reviewer", false},
		{"tab", "code\treviewer", false},
		{"emoji", "reviewer😀", false},
		{"latin extended", "café", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isValidSpawnName(tt.in), "isValidSpawnName(%q)", tt.in)
		})
	}
}

// TestNormalizeSpawnName covers the auto-normalization the spawn surfaces
// apply when config.agent.spawn_name_normalize is on (the default): any
// typed name is coerced into the [A-Za-z0-9_-] charset isValidSpawnName
// accepts, so "code reviewer!" "just works" rather than 400ing. The two
// invariants every case must hold — the output is always valid, and
// normalization is idempotent — are asserted in the loop below the table.
func TestNormalizeSpawnName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// Already-valid names pass through untouched.
		{"already valid", "code-reviewer_2", "code-reviewer_2"},
		{"plain", "abc123", "abc123"},
		{"empty stays empty", "", ""},

		// Spaces / punctuation collapse to a single dash.
		{"space", "code reviewer", "code-reviewer"},
		{"slash", "code/reviewer", "code-reviewer"},
		{"dot", "code.reviewer", "code-reviewer"},
		{"multiple spaces collapse", "code   reviewer", "code-reviewer"},
		{"mixed punctuation collapses", "code @#! reviewer", "code-reviewer"},

		// Leading/trailing junk is trimmed away.
		{"brackets trimmed", "[reviewer]", "reviewer"},
		{"leading punctuation", "!foo", "foo"},
		{"trailing punctuation", "foo!", "foo"},
		{"surrounded by junk + spaces", "  *foo*  ", "foo"}, // the dashes spaces/junk become are trimmed off the ends

		// Unicode → dash, trailing dash trimmed.
		{"latin extended", "café", "caf"},
		{"emoji only", "🎉", ""},
		{"emoji suffix", "reviewer😀", "reviewer"},

		// Length cap (64) with a clean trailing-dash trim after the cut.
		{"oversize truncated", strings.Repeat("a", MaxSpawnNameLen+10), strings.Repeat("a", MaxSpawnNameLen)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeSpawnName(tt.in)
			assert.Equalf(t, tt.want, got, "NormalizeSpawnName(%q)", tt.in)
			// Invariant 1: the result always clears the spawn-name gate, so a
			// caller can normalize-then-spawn with no second rejection.
			assert.Truef(t, isValidSpawnName(got),
				"NormalizeSpawnName(%q) = %q is not a valid spawn name", tt.in, got)
			// Invariant 2: idempotent — normalizing the output is a no-op.
			assert.Equalf(t, got, NormalizeSpawnName(got),
				"NormalizeSpawnName not idempotent on %q", tt.in)
		})
	}
}
