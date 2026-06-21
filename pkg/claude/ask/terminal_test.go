package ask

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// allTermEnv is every env var resolveTerminalID consults; clearTermEnv empties
// them so a test starts from a known-blank environment (the test process may
// itself run inside tmux / Windows Terminal, which would otherwise leak in).
var allTermEnv = []string{
	"TCLAUDE_ASK_TERM",
	"WT_SESSION", "TERM_SESSION_ID", "ITERM_SESSION_ID", "TMUX_PANE",
	"GNOME_TERMINAL_SCREEN", "KONSOLE_DBUS_SERVICE", "KONSOLE_DBUS_SESSION",
	"WEZTERM_PANE", "TILIX_ID", "TERMINATOR_UUID",
	"KITTY_WINDOW_ID", "ALACRITTY_WINDOW_ID", "WINDOWID",
}

// clearTermEnv blanks every terminal env var (t.Setenv restores them on
// cleanup). An empty value reads as unset because resolveTerminalID treats
// whitespace-only as absent.
func clearTermEnv(t *testing.T) {
	t.Helper()
	for _, k := range allTermEnv {
		t.Setenv(k, "")
	}
}

// stubControllingTTY pins controllingTTYFn to a fixed value (or "") so the
// resolveTerminalID chain is deterministic regardless of the test runner's
// real controlling terminal.
func stubControllingTTY(t *testing.T, v string) {
	t.Helper()
	prev := controllingTTYFn
	controllingTTYFn = func() string { return v }
	t.Cleanup(func() { controllingTTYFn = prev })
}

// TestResolveTerminalID_Priority exercises the full fidelity ladder: override >
// per-tab env > controlling tty > per-window env > ppid, including the
// Konsole multi-var pairing and the "tty beats per-window, per-tab beats tty"
// orderings that are the point of JOH-251.
func TestResolveTerminalID_Priority(t *testing.T) {
	cases := []struct {
		name       string
		env        map[string]string
		tty        string // controllingTTYFn stub
		wantID     string
		wantSource string
	}{
		{
			name:       "override wins over everything",
			env:        map[string]string{"TCLAUDE_ASK_TERM": "explicit", "TMUX_PANE": "%1"},
			tty:        "pts/9",
			wantID:     "explicit",
			wantSource: "override (TCLAUDE_ASK_TERM)",
		},
		{
			name:       "windows terminal",
			env:        map[string]string{"WT_SESSION": "guid-123"},
			tty:        "pts/9",
			wantID:     "guid-123",
			wantSource: "windows-terminal",
		},
		{
			name:       "tmux pane id is preserved verbatim",
			env:        map[string]string{"TMUX_PANE": "%22"},
			wantID:     "%22",
			wantSource: "tmux",
		},
		{
			name:       "gnome terminal screen",
			env:        map[string]string{"GNOME_TERMINAL_SCREEN": "/org/gnome/Terminal/screen/abc"},
			wantID:     "/org/gnome/Terminal/screen/abc",
			wantSource: "gnome-terminal",
		},
		{
			name:       "konsole pairs service and session",
			env:        map[string]string{"KONSOLE_DBUS_SERVICE": ":1.42", "KONSOLE_DBUS_SESSION": "/Sessions/3"},
			wantID:     ":1.42//Sessions/3",
			wantSource: "konsole",
		},
		{
			name:       "konsole with only service is skipped, falls to tty",
			env:        map[string]string{"KONSOLE_DBUS_SERVICE": ":1.42"},
			tty:        "pts/7",
			wantID:     "pts/7",
			wantSource: "controlling-tty",
		},
		{
			name:       "per-tab env beats the controlling tty",
			env:        map[string]string{"WEZTERM_PANE": "4"},
			tty:        "pts/7",
			wantID:     "4",
			wantSource: "wezterm",
		},
		{
			name:       "controlling tty beats per-window env",
			env:        map[string]string{"KITTY_WINDOW_ID": "7"},
			tty:        "pts/2",
			wantID:     "pts/2",
			wantSource: "controlling-tty",
		},
		{
			name:       "per-window env used when no tty",
			env:        map[string]string{"KITTY_WINDOW_ID": "7"},
			tty:        "",
			wantID:     "7",
			wantSource: "kitty",
		},
		{
			name:       "x11 windowid as last per-window option",
			env:        map[string]string{"WINDOWID": "0x4200003"},
			tty:        "",
			wantID:     "0x4200003",
			wantSource: "x11-window",
		},
		{
			name:       "ppid fallback when nothing else is available",
			env:        nil,
			tty:        "",
			wantID:     "", // checked by prefix below
			wantSource: "ppid",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearTermEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			stubControllingTTY(t, tc.tty)

			id, source := resolveTerminalID()
			assert.Equal(t, tc.wantSource, source, "source")
			if tc.wantSource == "ppid" {
				assert.True(t, strings.HasPrefix(id, "ppid-"), "ppid fallback id, got %q", id)
			} else {
				assert.Equal(t, tc.wantID, id, "id")
			}
		})
	}
}

// TestResolveTerminalID_WhitespaceIsUnset confirms a var set to whitespace is
// treated as absent (so a quoted-empty export doesn't pin a blank id).
func TestResolveTerminalID_WhitespaceIsUnset(t *testing.T) {
	clearTermEnv(t)
	t.Setenv("TMUX_PANE", "   ")
	stubControllingTTY(t, "pts/5")

	id, source := resolveTerminalID()
	assert.Equal(t, "pts/5", id)
	assert.Equal(t, "controlling-tty", source)
}

// TestTerminalKey_Composition checks TerminalKey = <id>.<bootID> and tracks
// resolveTerminalID's chosen id.
func TestTerminalKey_Composition(t *testing.T) {
	clearTermEnv(t)
	t.Setenv("TCLAUDE_ASK_TERM", "fixed-term")
	stubControllingTTY(t, "")

	key := TerminalKey()
	assert.True(t, strings.HasPrefix(key, "fixed-term."), "key starts with the term id, got %q", key)
	assert.Equal(t, "fixed-term."+bootID(), key)
}

// TestTTYName covers the tty_nr → id decode, including the bit-packed minor
// reconstruction (man 5 proc: minor = bits 31–20 high + bits 7–0 low) and the
// pts-vs-other rendering.
func TestTTYName(t *testing.T) {
	cases := []struct {
		name  string
		ttyNr int
		want  string
	}{
		{"zero is no controlling terminal", 0, ""},
		{"pts/3", (136 << 8) | 3, "pts/3"},
		{"pts/255 (max in major 136)", (136 << 8) | 255, "pts/255"},
		{"pts/256 spills into major 137 (minor 0)", 137 << 8, "pts/256"},
		{"legacy console major 4", (4 << 8) | 1, "tty4-1"},
		// minor 0x305 = 773: low byte 0x05, high bits (773>>8)=3 packed at 31–20.
		{"high minor bits reconstructed", (200 << 8) | 0x05 | (3 << 20), "tty200-773"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ttyName(tc.ttyNr))
		})
	}
}

// TestControllingTTY_RealProcSanity calls the real probe (it reads
// /proc/self/stat) and asserts only the contract: either "" (no /proc or no
// controlling terminal, e.g. CI) or a well-formed line id. It must never panic
// or return a malformed string.
func TestControllingTTY_RealProcSanity(t *testing.T) {
	got := controllingTTY()
	if got == "" {
		return // valid: no controlling terminal here
	}
	assert.True(t,
		strings.HasPrefix(got, "pts/") || strings.HasPrefix(got, "tty"),
		"unexpected controlling-tty id %q", got)
}

// TestPrintWhere reports the resolved bucket: term-key (id.boot), source, cwd,
// and the conv mapping — "none yet" with no thread, then the conv-id + harness
// once one is recorded for that (term, cwd).
func TestPrintWhere(t *testing.T) {
	setupAskTestDB(t)
	clearTermEnv(t)
	t.Setenv("TCLAUDE_ASK_TERM", "where-term")
	stubControllingTTY(t, "")

	var buf strings.Builder
	require.NoError(t, printWhere("/repo/here", &buf))
	out := buf.String()
	assert.Contains(t, out, "term-key:  where-term."+bootID())
	assert.Contains(t, out, "source: override (TCLAUDE_ASK_TERM)")
	assert.Contains(t, out, "cwd:       /repo/here")
	assert.Contains(t, out, "none yet", "no thread yet → fresh note")

	// Record a thread for this exact (term-key, cwd), then it should be shown.
	require.NoError(t, db.SetAskThread("where-term."+bootID(), "/repo/here", "conv-xyz", "claude"))
	var buf2 strings.Builder
	require.NoError(t, printWhere("/repo/here", &buf2))
	out2 := buf2.String()
	assert.Contains(t, out2, "conv-id:   conv-xyz")
	assert.Contains(t, out2, "harness: claude")
}
