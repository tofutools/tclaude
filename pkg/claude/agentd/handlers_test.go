package agentd

import (
	"strings"
	"testing"
	"time"

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
				if got != nil {
					t.Fatalf("got %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("got nil, want id=%q", tt.wantID)
			}
			if got.ID != tt.wantID {
				t.Errorf("got id=%q, want id=%q", got.ID, tt.wantID)
			}
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
	if err := db.SaveSession(sess); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	// Statusbar writes by session ID (not conv-id).
	if err := db.UpdateContextPct("tmux-label-1", 47.0); err != nil {
		t.Fatalf("UpdateContextPct: %v", err)
	}

	// Replay the handler's lookup chain.
	rows, err := db.FindSessionsByConvID(convID)
	if err != nil {
		t.Fatalf("FindSessionsByConvID: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 session row for conv, got %d", len(rows))
	}
	pct, pending, err := db.GetCompactState(rows[0].ID)
	if err != nil {
		t.Fatalf("GetCompactState: %v", err)
	}
	if pct != 47.0 {
		t.Errorf("context_pct via conv-id lookup = %v, want 47", pct)
	}
	if pending != 0 {
		t.Errorf("compact_pending = %v, want 0", pending)
	}

	// Regression guard: querying compact state with the conv-id
	// directly (the buggy path) must not return the populated value.
	// Different keys: the row's id is "tmux-label-1", the conv-id is
	// "conv-aaaa-bbbb". The buggy lookup will either error (sql: no
	// rows) or return zero — either is fine, what matters is it does
	// NOT return 47.0.
	pctBuggy, _, errBuggy := db.GetCompactState(convID)
	if errBuggy == nil && pctBuggy == 47.0 {
		t.Error("buggy path (GetCompactState by conv-id) should not return populated value")
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
			if got := isValidFollowUp(tt.in); got != tt.want {
				t.Errorf("isValidFollowUp(%q) = %v, want %v", tt.in, got, tt.want)
			}
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
			if got := isValidRenameTitle(tt.in); got != tt.want {
				t.Errorf("isValidRenameTitle(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
