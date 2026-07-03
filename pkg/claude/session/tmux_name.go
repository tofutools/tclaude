package session

import (
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// TmuxNameBase returns the human-facing base for a session's tmux name: an
// explicit label verbatim; else — with config session.tmux_name_style =
// "dir" — the sanitized basename of the session's working directory; else
// the first 8 chars of the (full) session id. The tmux name is where the
// id is deliberately rendered short; the stored PK keeps the full identity
// (JOH-248). A taken base is the caller's business (UniqueTmuxSessionName).
// Exported so the conv-resume paths share one definition with `session new`.
func TmuxNameBase(sessionID, label, workDir string) string {
	if label != "" {
		return label
	}
	if tmuxNameStyleFn() == config.TmuxNameStyleDir {
		if base := sanitizeTmuxName(filepath.Base(workDir)); base != "" {
			return base
		}
	}
	if len(sessionID) > 8 {
		return sessionID[:8]
	}
	return sessionID
}

// tmuxNameStyleFn is the seam tests use to pin the naming style without
// writing a real config file. Read live per call: session launches are
// rare, config.Load is cheap, and a live read means a config edit takes
// effect without restarting anything (same no-cache philosophy as
// focusRaiseOnly). config.Load returns a non-nil DefaultConfig even on
// error and ResolvedTmuxNameStyle is nil-safe, so this never panics and
// degrades to the historical id-prefix style.
var tmuxNameStyleFn = func() string {
	cfg, _ := config.Load()
	return cfg.ResolvedTmuxNameStyle()
}

// tmuxNameMax bounds a dir-derived tmux name so a deeply descriptive
// directory doesn't blow up the status line / `session ls` table. 32 keeps
// the interesting part of even long worktree names; if a cut collides,
// UniqueTmuxSessionName's -N suffix restores uniqueness.
const tmuxNameMax = 32

// sanitizeTmuxName maps a raw string onto a safe tmux session-name
// charset. tmux rejects '.' and ':' in session names outright ("bad
// session name"), and anything else outside [A-Za-z0-9_-] makes -t
// targets fragile (shell quoting, fnmatch metacharacters in tmux's
// prefix-match fallback) — so every other rune becomes '-', runs of '-'
// collapse, leading/trailing '-' are trimmed and the result is
// length-capped. Returns "" when nothing survives — the caller falls back
// to the id-prefix base.
func sanitizeTmuxName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			out = append(out, r)
		default:
			if len(out) > 0 && out[len(out)-1] != '-' {
				out = append(out, '-')
			}
		}
	}
	name := strings.TrimRight(string(out), "-")
	if len(name) > tmuxNameMax {
		name = strings.TrimRight(name[:tmuxNameMax], "-")
	}
	return name
}
