package session

import (
	"maps"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// ApplyClaudeResumeEnv merges tclaude's configured CLAUDE_CODE_RESUME_*
// overrides into env, so a spawned/resumed Claude Code pane doesn't stop on the
// interactive "Resume from summary" chooser — a TUI prompt a detached,
// tmux-driven resume can't answer, which would otherwise hang the automation.
//
// The overrides live in ~/.tclaude/config.json (never ~/.claude/settings.json,
// so the operator's manual `claude` runs and the dashboard's config diff viewer
// stay clean) and are Claude-Code-specific, so this is a no-op for any other
// harness — Codex has no such prompt. It only ever ADDS keys, leaving env's
// existing entries untouched.
//
// It is the single seam both spawn paths route through: session.runNew (the
// daemon's `tclaude session new -r` resume) and conv.resumeLaunchCmd (watch-mode
// resume). It is applied to fresh launches as well as resumes; the vars are
// harmless on a fresh session (the chooser only fires on resume), and gating on
// the harness rather than on "is a resume" keeps the call sites simple.
//
// Best-effort by design: a config that can't be loaded (corrupt / unreadable)
// leaves Claude Code on its own defaults rather than failing the spawn —
// suppressing the prompt is a convenience, not a correctness requirement.
func ApplyClaudeResumeEnv(h *harness.Harness, env map[string]string) {
	if h == nil || env == nil || h.Name != harness.DefaultName {
		return
	}
	cfg, err := config.Load()
	if err != nil {
		return
	}
	maps.Copy(env, cfg.ClaudeResumeEnv())
}
