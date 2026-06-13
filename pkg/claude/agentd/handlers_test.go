package agentd

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestPickWithLiveness exercises the conv-id → session-row picker that
// backs handleWhoamiContext. The bug we're guarding against: rows
// arrive ordered by updated_at DESC from FindSessionsByConvID, so
// "newest" is candidates[0] — but if a stale row leads the list and
// only an older row's tmux is alive, we want the alive one.
func TestPickWithLiveness(t *testing.T) {
	row := func(id, tmux string) *db.SessionRow {
		return &db.SessionRow{ID: id, TmuxSession: tmux}
	}

	tests := []struct {
		name       string
		candidates []*db.SessionRow
		alive      map[string]bool
		wantID     string
	}{
		{
			name:       "empty list returns nil",
			candidates: nil,
			alive:      map[string]bool{},
			wantID:     "",
		},
		{
			name:       "single live row picked",
			candidates: []*db.SessionRow{row("s-1", "tmux-1")},
			alive:      map[string]bool{"tmux-1": true},
			wantID:     "s-1",
		},
		{
			name: "live among multiple wins over latest-but-dead",
			candidates: []*db.SessionRow{
				row("newest-dead", "tmux-newest"),
				row("older-alive", "tmux-older"),
			},
			alive:  map[string]bool{"tmux-older": true},
			wantID: "older-alive",
		},
		{
			name: "no live rows falls back to first (latest by updated_at)",
			candidates: []*db.SessionRow{
				row("newest", "tmux-1"),
				row("older", "tmux-2"),
			},
			alive:  map[string]bool{},
			wantID: "newest",
		},
		{
			name: "row with empty tmux is never alive",
			candidates: []*db.SessionRow{
				row("first", ""),
				row("second", "tmux-2"),
			},
			alive:  map[string]bool{"": true, "tmux-2": true},
			wantID: "second",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isAlive := func(tmux string) bool {
				if tmux == "" {
					return false
				}
				return tt.alive[tmux]
			}
			got := pickWithLiveness(tt.candidates, isAlive)
			if tt.wantID == "" {
				assert.Nil(t, got, "got %+v, want nil", got)
				return
			}
			require.NotNil(t, got, "got nil, want id=%q", tt.wantID)
			assert.Equal(t, tt.wantID, got.ID, "got id mismatch")
		})
	}
}

// TestContextPctLookupByConvID locks down the conv-id → session-id
// resolution chain that backs /v1/whoami/context. The original bug:
// the handler queried GetCompactState with the conv-id directly, but
// context_pct is keyed by tclaude session ID (the statusbar hook only
// has TCLAUDE_SESSION_ID at write time). Result was always 0.
//
// This test exercises the full chain — write context_pct via the
// statusbar path (by session ID), look the row up by conv-id, then
// read compact state by the row's session ID — the same shape the
// handler runs.
func TestContextPctLookupByConvID(t *testing.T) {
	setupTestDB(t)

	// One conv, one session row.
	convID := "conv-aaaa-bbbb"
	sess := &db.SessionRow{
		ID:          "tmux-label-1",
		TmuxSession: "tmux-label-1",
		ConvID:      convID,
		CreatedAt:   time.Now(),
	}
	require.NoError(t, db.SaveSession(sess), "SaveSession")

	// Statusbar writes by session ID (not conv-id).
	require.NoError(t, db.UpdateContextPct("tmux-label-1", 47.0), "UpdateContextPct")

	// Replay the handler's lookup chain.
	rows, err := db.FindSessionsByConvID(convID)
	require.NoError(t, err, "FindSessionsByConvID")
	require.Len(t, rows, 1, "expected 1 session row for conv")
	pct, pending, err := db.GetCompactState(rows[0].ID)
	require.NoError(t, err, "GetCompactState")
	assert.Equal(t, 47.0, pct, "context_pct via conv-id lookup")
	assert.Equal(t, float64(0), pending, "compact_pending")

	// Regression guard: querying compact state with the conv-id
	// directly (the buggy path) must not return the populated value.
	// Different keys: the row's id is "tmux-label-1", the conv-id is
	// "conv-aaaa-bbbb". The buggy lookup will either error (sql: no
	// rows) or return zero — either is fine, what matters is it does
	// NOT return 47.0.
	pctBuggy, _, errBuggy := db.GetCompactState(convID)
	if errBuggy == nil {
		assert.NotEqual(t, 47.0, pctBuggy,
			"buggy path (GetCompactState by conv-id) should not return populated value")
	}
}

// TestIsValidFollowUp locks down the self-lifecycle follow-up charset.
// Unlike rename titles (cross-agent keystroke-injection surface), the
// follow-up only feeds the agent's own pane — so we relax the charset
// to printable + space, but still reject control chars (each newline
// would land as a prompt-submit in tmux send-keys, fragmenting the
// follow-up across multiple turns).
func TestIsValidFollowUp(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		// --- accepted: prose ---
		{"plain text", "now write up your findings", true},
		{"multi space", "spaces  are  fine  here", true},
		{"slash inside", "save notes to /tmp/foo.md", true},
		{"quotes", `call it "draft"`, true},
		{"path with brackets", "scan ~/repo/[2025]/*.md", true},
		{"unicode emoji", "summarise 🎉", true},
		{"unicode latin", "café review", true},
		{"max length", strings.Repeat("a", 4096), true},

		// --- rejected: empty / oversize ---
		{"empty", "", false},
		{"oversize 4097", strings.Repeat("a", 4097), false},

		// --- rejected: control chars (each would split prompts) ---
		{"newline", "first line\nsecond line", false},
		{"tab", "before\tafter", false},
		{"carriage return", "before\rafter", false},
		{"NUL", "before\x00after", false},
		{"DEL", "before\x7fafter", false},
		{"escape", "before\x1bafter", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isValidFollowUp(tt.in), "isValidFollowUp(%q)", tt.in)
		})
	}
}

// TestIsValidInitialMessage locks down the spawn initial-context
// brief rules. Unlike a follow-up, the brief is delivered to the new
// agent's inbox (not typed into a pane), so newlines and tabs are
// allowed — but NUL / escape / carriage-return still aren't, and an
// empty string is valid (it just means "no brief").
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
		{"blank line between paragraphs", "para one\n\npara two", true},
		{"tab", "before\tafter", true},
		{"unicode", "résumé 🎉", true},
		{"over the retired 4096 cap", strings.Repeat("a", 8000), true},
		{"max length", strings.Repeat("a", 16384), true},

		// --- rejected ---
		{"oversize 16385", strings.Repeat("a", 16385), false},
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

// TestIsValidRenameTitle locks down the rename-title charset rules. The
// daemon side is the actual security boundary, so this test is the
// authoritative spec — the CLI mirror in pkg/claude/agent/rename.go
// must stay in sync with these expectations.
func TestIsValidRenameTitle(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		// --- accepted ---
		{"plain alphanumeric", "abc123", true},
		{"hyphen", "code-reviewer", true},
		{"underscore", "code_reviewer", true},
		{"single space", "code reviewer", true},
		{"brackets", "[reviewer]", true},
		{"braces", "{reviewer}", true},
		{"parens", "(reviewer)", true},
		{"mixed", "[reviewer] code-frontend(v2)", true},
		{"max length 64", "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz0123456789AB", true},

		// --- rejected: empty / oversize ---
		{"empty", "", false},
		{"too long 65", "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz0123456789ABCD", false},

		// --- rejected: whitespace abuse ---
		{"double space", "code  reviewer", false},
		{"triple space", "code   reviewer", false},
		{"tab", "code\treviewer", false},
		{"newline", "code\nreviewer", false},
		{"carriage return", "code\rreviewer", false},
		{"NBSP", "code reviewer", false},

		// --- rejected: keystroke-injection vectors ---
		{"slash command", "foo /bash", false},
		{"single quote", "code'reviewer", false},
		{"double quote", "code\"reviewer", false},
		{"backtick", "code`reviewer", false},
		{"semicolon", "code;reviewer", false},
		{"pipe", "code|reviewer", false},
		{"dollar", "code$reviewer", false},
		{"backslash", "code\\reviewer", false},
		{"angle brackets", "code<reviewer>", false},

		// --- rejected: unicode / non-ASCII ---
		{"emoji", "reviewer😀", false},
		{"unicode dash", "reviewer–frontend", false},
		{"latin extended", "café", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isValidRenameTitle(tt.in), "isValidRenameTitle(%q)", tt.in)
		})
	}
}

// TestIsValidRenameSink locks down the send-keys charset gate used on
// the reincarnate/clone injection path (JOH-177). It shares its charset
// with isValidRenameTitle but is LENGTH-EXEMPT: a reincarnate carry
// title is `<predecessor>-r-<N>` / `<predecessor>-x`, and a predecessor
// already at the 64-char display max pushes the suffixed title past the
// cap — reusing isValidRenameTitle there would reject a legitimate
// title. The gate's job is the charset (no early-submit / control
// chars), not the length.
func TestIsValidRenameSink(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		// --- accepted: legitimate charset, including over-64 ---
		{"plain", "worker-r-1", true},
		{"archive suffix", "code reviewer-x", true},
		// The whole reason this helper exists: a max-length predecessor
		// plus a `-r-N` suffix exceeds isValidRenameTitle's 64-char cap
		// yet must still be injectable.
		{"max-len predecessor plus suffix (over 64)",
			"abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz0123456789AB-r-12", true},

		// --- rejected: nothing to inject ---
		{"empty", "", false},

		// --- rejected: early-submit / control-char injection vectors ---
		{"newline", "evil\nrm -rf", false},
		{"carriage return", "code\rreviewer", false},
		{"tab", "code\treviewer", false},
		{"NUL", "code\x00reviewer", false},

		// --- rejected: shell/keystroke metacharacters ---
		{"slash command", "foo /bash", false},
		{"double quote", "code\"reviewer", false},
		{"semicolon", "code;reviewer", false},
		{"unicode", "café", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isValidRenameSink(tt.in), "isValidRenameSink(%q)", tt.in)
		})
	}

	// Cross-check the length-exemption contract directly: a title that
	// isValidRenameTitle rejects ONLY for length must pass the sink.
	overCap := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz0123456789AB-r-1"
	assert.False(t, isValidRenameTitle(overCap), "precondition: title should exceed the 64-char cap")
	assert.True(t, isValidRenameSink(overCap), "length-exempt sink must accept a charset-clean over-cap title")
}
