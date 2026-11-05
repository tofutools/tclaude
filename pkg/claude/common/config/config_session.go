package config

import (
	"strconv"
)

// TUI color schemes — config tui.color_scheme. Picks the color palette the
// interactive bubbletea "watch" views (`session ls -w`, `conv ls -w`, `agent
// inbox -w`) render with:
//
//	"default"            — the current palette (PR #738), tuned to stay
//	                       readable on light AND dark terminals; a little
//	                       dimmer on a dark background.
//	"dark-high-contrast" — the brighter pre-#738 palette (vivid yellow / green
//	                       / red, brighter header), higher contrast on a dark
//	                       terminal at the cost of light-terminal readability.
//
// An empty / unknown value falls back to "default", so a typo can never leave
// the views unstyled. The palette values themselves live in
// pkg/claude/common/tuistyle; this is only the selector the config file and
// the dashboard Config tab edit.
const (
	TUIColorSchemeDefault      = "default"
	TUIColorSchemeHighContrast = "dark-high-contrast"
)

// TUIConfig holds the interactive-TUI color-scheme selection.
type TUIConfig struct {
	// ColorScheme names the palette — one of the TUIColorScheme* constants.
	// Empty / unknown resolves to TUIColorSchemeDefault (see TUIColorScheme).
	ColorScheme string `json:"color_scheme,omitempty"`
}

// normalizeTUIColorScheme returns s when it's a known scheme, else "" (so the
// resolver falls back to its default for a blank or hand-edited garbage value).
func normalizeTUIColorScheme(s string) string {
	switch s {
	case TUIColorSchemeDefault, TUIColorSchemeHighContrast:
		return s
	default:
		return ""
	}
}

// TUIColorScheme reports the effective interactive-TUI color scheme — config
// tui.color_scheme. Default "default" (absent block / key or an unknown
// value); "dark-high-contrast" selects the brighter pre-#738 palette. Nil-safe
// on the receiver so callers need no guard.
func (c *Config) TUIColorScheme() string {
	if c != nil && c.TUI != nil {
		if s := normalizeTUIColorScheme(c.TUI.ColorScheme); s != "" {
			return s
		}
	}
	return TUIColorSchemeDefault
}

// Tmux-session naming styles — config session.tmux_name_style. The style
// picks the BASE for a spawned session's tmux name when no explicit
// --label is given; session.UniqueTmuxSessionName still disambiguates a
// taken base with a -N suffix, and the DB row keeps the full identity
// either way — the tmux name is only the human-facing handle (JOH-248),
// so the style can be switched (or switched back) at any time and only
// affects newly launched sessions.
//
//	"id"  — first 8 chars of the session id (the historical default)
//	"dir" — sanitized basename of the session's working directory, for
//	        recognisable names when switching sessions inside tmux
//
// An empty / unknown value falls back to "id", so a typo can never change
// launch behavior.
const (
	TmuxNameStyleID  = "id"
	TmuxNameStyleDir = "dir"
)

// SessionConfig holds session-launch tuning.
type SessionConfig struct {
	// TmuxNameStyle picks the tmux-session naming style — one of the
	// TmuxNameStyle* constants above. Applies to `session new` without
	// --label and to conversation resumes; agentd-spawned agents always
	// pass their agent name as the label and are unaffected.
	TmuxNameStyle string `json:"tmux_name_style,omitempty"`

	// AutoNameFromPrompt lets agentd make a bounded, non-interactive model
	// call to replace a free-floating session's deterministic display-name
	// fallback after its first prompt. It defaults off: starting a session
	// must not silently spend tokens or wait on another model invocation.
	AutoNameFromPrompt bool `json:"auto_name_from_prompt,omitempty"`

	// AutoJoinGroup lets a fresh terminal-owned `tclaude` / `session new`
	// launch discover an active agent group whose default cwd is the same
	// canonical directory as the launch cwd, then use the ordinary daemon spawn
	// path for that group. It is a pointer because the out-of-box default is ON:
	// nil / absent means true, while an explicit false disables discovery.
	AutoJoinGroup *bool `json:"auto_join_group,omitempty"`

	// AutoJoinOrCreateGroup extends directory discovery by creating a group when
	// no active group owns the canonical launch cwd. The derived group name is
	// based on the directory basename and disambiguated predictably. This is
	// deliberately opt-in: nil / absent means false.
	AutoJoinOrCreateGroup *bool `json:"auto_join_or_create_group,omitempty"`
}

// ResolvedTmuxNameStyle returns the effective tmux-session naming style,
// normalized to one of the TmuxNameStyle* constants. Nil-safe; empty and
// unknown values resolve to TmuxNameStyleID (the historical id-prefix
// names).
func (c *Config) ResolvedTmuxNameStyle() string {
	if c == nil || c.Session == nil {
		return TmuxNameStyleID
	}
	if c.Session.TmuxNameStyle == TmuxNameStyleDir {
		return TmuxNameStyleDir
	}
	return TmuxNameStyleID
}

// AutoNameFromPromptEnabled reports whether free-floating sessions may use a
// one-shot model call to refine their deterministic display-name fallback.
// Nil-safe and opt-in so the default launch path remains free and immediate.
func (c *Config) AutoNameFromPromptEnabled() bool {
	return c != nil && c.Session != nil && c.Session.AutoNameFromPrompt
}

// AutoJoinGroupEnabled reports the terminal-start directory discovery policy.
// It defaults on so a group configured for the current directory works with a
// bare `tclaude`; only an explicit false disables it.
func (c *Config) AutoJoinGroupEnabled() bool {
	return c == nil || c.Session == nil || c.Session.AutoJoinGroup == nil || *c.Session.AutoJoinGroup
}

// AutoJoinOrCreateGroupEnabled reports whether terminal startup may create a
// missing directory group. It is opt-in and therefore defaults off.
func (c *Config) AutoJoinOrCreateGroupEnabled() bool {
	return c != nil && c.Session != nil && c.Session.AutoJoinOrCreateGroup != nil && *c.Session.AutoJoinOrCreateGroup
}

// Browser-terminal attach resize modes — config dashboard.terminal_attach.mode.
const (
	TerminalAttachResizeRepair    = "repair"
	TerminalAttachResizeInitial   = "initial"
	TerminalAttachResizePreAttach = "pre_attach"

	DefaultTerminalAttachRepairDelayMS    = 250
	DefaultTerminalAttachPreAttachDelayMS = 250
	MaxTerminalAttachDelayMS              = 10000
)

// TerminalAttachConfig controls how an xterm-backed terminal establishes and
// repairs its initial geometry. All delays are milliseconds and pointers so an
// explicit zero remains distinct from an absent value.
//
// Modes:
//   - repair (default): initial resize, then the historical one-row nudge and
//     restore after RepairDelayMS.
//   - initial: initial resize only.
//   - pre_attach: send the initial size before the PTY command starts, wait
//     PreAttachDelayMS, then start the attachment; no post-attach nudge.
type TerminalAttachConfig struct {
	Mode                 string `json:"mode,omitempty"`
	InitialResizeDelayMS *int   `json:"initial_resize_delay_ms,omitempty"`
	RepairDelayMS        *int   `json:"repair_delay_ms,omitempty"`
	PreAttachDelayMS     *int   `json:"pre_attach_delay_ms,omitempty"`
}

// ResolvedTerminalAttach returns the effective browser-terminal attach mode
// and delays. Unknown modes degrade to the historical repair behavior while
// Validate reports the bad value to the Config tab. Invalid hand-edited delays
// are clamped so the live dashboard remains usable while validation reports
// them.
func (c *Config) ResolvedTerminalAttach() (mode string, initialDelayMS, repairDelayMS, preAttachDelayMS int) {
	mode = TerminalAttachResizeRepair
	repairDelayMS = DefaultTerminalAttachRepairDelayMS
	preAttachDelayMS = DefaultTerminalAttachPreAttachDelayMS
	if c == nil || c.Dashboard == nil || c.Dashboard.TerminalAttach == nil {
		return
	}
	t := c.Dashboard.TerminalAttach
	switch t.Mode {
	case "", TerminalAttachResizeRepair:
	case TerminalAttachResizeInitial, TerminalAttachResizePreAttach:
		mode = t.Mode
	}
	resolve := func(value *int, fallback int) int {
		if value == nil {
			return fallback
		}
		return min(MaxTerminalAttachDelayMS, max(0, *value))
	}
	initialDelayMS = resolve(t.InitialResizeDelayMS, 0)
	repairDelayMS = resolve(t.RepairDelayMS, DefaultTerminalAttachRepairDelayMS)
	preAttachDelayMS = resolve(t.PreAttachDelayMS, DefaultTerminalAttachPreAttachDelayMS)
	return
}

// TerminalTabEvent names why a dashboard terminal's tmux client went away —
// the rows of dashboard.close_terminal_tabs.
type TerminalTabEvent string

const (
	// TerminalTabSessionEnded: the tmux session is gone (the shell exited, the
	// agent exited, or the session was killed).
	TerminalTabSessionEnded TerminalTabEvent = "session_ended"
	// TerminalTabDetached: the tmux client exited but its session lives on
	// (prefix+d, or another client took the session over).
	TerminalTabDetached TerminalTabEvent = "detached"
)

// TerminalTabTarget names the kind of dashboard terminal — the columns of
// dashboard.close_terminal_tabs.
type TerminalTabTarget string

const (
	// TerminalTabGroupTerminal is a shell opened in a group's home dir.
	TerminalTabGroupTerminal TerminalTabTarget = "group_terminal"
	// TerminalTabAgentTerminal is a shell opened in an agent's directory.
	TerminalTabAgentTerminal TerminalTabTarget = "agent_terminal"
	// TerminalTabAgentWindow is an agent's own harness pane.
	TerminalTabAgentWindow TerminalTabTarget = "agent_window"
)

// TerminalTabCloseConfig is dashboard.close_terminal_tabs: one target set per
// event. Each switch is a *bool so absent keeps its default (see
// CloseTerminalTab) while an explicit false opts out.
type TerminalTabCloseConfig struct {
	SessionEnded *TerminalTabCloseTargets `json:"session_ended,omitempty"`
	Detached     *TerminalTabCloseTargets `json:"detached,omitempty"`
}

// TerminalTabCloseTargets holds one event's per-terminal-kind switches.
type TerminalTabCloseTargets struct {
	GroupTerminal *bool `json:"group_terminal,omitempty"`
	AgentTerminal *bool `json:"agent_terminal,omitempty"`
	AgentWindow   *bool `json:"agent_window,omitempty"`
}

// CloseTerminalTab reports whether a dashboard terminal tab of kind target
// should close itself on event. Defaults: shell terminals (group and agent
// dir) close once their session has ended, since reconnecting would only
// start a fresh shell; agent windows keep the tab so the exit stays visible,
// and a detach never closes anything. Nil-safe; unknown values are false.
func (c *Config) CloseTerminalTab(event TerminalTabEvent, target TerminalTabTarget) bool {
	def := event == TerminalTabSessionEnded &&
		(target == TerminalTabGroupTerminal || target == TerminalTabAgentTerminal)
	if c == nil || c.Dashboard == nil || c.Dashboard.CloseTerminalTabs == nil {
		return def
	}
	var targets *TerminalTabCloseTargets
	switch event {
	case TerminalTabSessionEnded:
		targets = c.Dashboard.CloseTerminalTabs.SessionEnded
	case TerminalTabDetached:
		targets = c.Dashboard.CloseTerminalTabs.Detached
	}
	if targets == nil {
		return def
	}
	var v *bool
	switch target {
	case TerminalTabGroupTerminal:
		v = targets.GroupTerminal
	case TerminalTabAgentTerminal:
		v = targets.AgentTerminal
	case TerminalTabAgentWindow:
		v = targets.AgentWindow
	}
	if v == nil {
		return def
	}
	return *v
}

// ClaudeResumeConfig tunes Claude Code's interactive "Resume from summary"
// chooser — the multiple-choice prompt CC shows when resuming a conversation
// that is BOTH old (≥ ThresholdMinutes since last activity) AND large
// (≥ TokenThreshold estimated tokens). That prompt breaks tclaude's scripted
// resume: a detached, tmux-driven pane can't answer a TUI it didn't expect, so
// the resume hangs. Raising either threshold high enough makes the prompt never
// fire (CC's gate shows it only when both conditions hold, so lifting one is
// enough) — `tclaude setup --install-resume-threshold-override` writes a large
// ThresholdMinutes for exactly this reason.
//
// tclaude applies these as the CLAUDE_CODE_RESUME_THRESHOLD_MINUTES /
// CLAUDE_CODE_RESUME_TOKEN_THRESHOLD environment variables on the `claude`
// process it spawns ONLY — it never writes them into ~/.claude/settings.json.
// That keeps the operator's manual `claude` runs untouched and makes this block
// (in ~/.tclaude/config.json) the single source of truth the dashboard Config
// tab and its diff viewer edit. The env vars are Claude-Code-specific, so the
// overrides are injected only when the spawned harness is Claude Code; Codex has
// no equivalent prompt and ignores the block.
//
// Both fields are pointers so "absent" is distinguishable from an explicit 0:
// a nil pointer omits the matching env var (Claude Code keeps its own built-in
// default — 70 minutes / 100,000 tokens), a set value injects it. The env vars
// are undocumented and version-specific (verified against Claude Code 2.1.187);
// if a future CC build renames or drops them the override degrades to a no-op,
// never an error — clear the block to revert.
type ClaudeResumeConfig struct {
	// ThresholdMinutes overrides CLAUDE_CODE_RESUME_THRESHOLD_MINUTES — the
	// minimum age (minutes since last activity) a conversation must reach
	// before the resume prompt is even considered. nil omits the var (CC's
	// 70-minute default). Set it very high (ResumeThresholdMinutesSuppress)
	// to suppress the prompt for tclaude's automation.
	ThresholdMinutes *int `json:"threshold_minutes,omitempty"`
	// TokenThreshold overrides CLAUDE_CODE_RESUME_TOKEN_THRESHOLD — the
	// minimum estimated context size (tokens) a conversation must reach
	// before the resume prompt is considered. nil omits the var (CC's
	// 100,000-token default). A secondary knob: raising ThresholdMinutes
	// alone already suppresses the prompt.
	TokenThreshold *int `json:"token_threshold,omitempty"`
}

// Claude Code resume-prompt environment variables. These gate CC's
// "Resume from summary" chooser; tclaude injects them per-spawn to keep the
// chooser from blocking a detached resume. Undocumented + version-specific
// (Claude Code 2.1.187) — treated as best-effort, so an unknown-to-CC name is
// simply ignored by the harness rather than an error here.
const (
	EnvResumeThresholdMinutes = "CLAUDE_CODE_RESUME_THRESHOLD_MINUTES"
	EnvResumeTokenThreshold   = "CLAUDE_CODE_RESUME_TOKEN_THRESHOLD"
)

// ResumeThresholdMinutesSuppress is the ThresholdMinutes value
// `tclaude setup --install-resume-threshold-override` writes to switch the
// "Resume from summary" prompt off for tclaude-spawned panes: 525,600,000
// minutes (1,000 years), so a resumed session's age can never reach it and the
// prompt never fires. A deliberately absurd, clearly-intentional sentinel —
// not a real threshold anyone would pick by hand.
const ResumeThresholdMinutesSuppress = 525_600_000

// ClaudeResumeEnv returns the CLAUDE_CODE_RESUME_* environment overrides to
// inject into a spawned Claude Code process, or an empty map when nothing is
// configured. Each set field contributes its env var; a nil field is omitted so
// Claude Code keeps its own default for that threshold. Nil-safe on the receiver
// so callers need no guard.
//
// It is harness-agnostic by construction — it just resolves the configured
// integers to their CC env-var names. The caller decides WHEN to apply them
// (only for the Claude Code harness), so this method never gates on harness.
func (c *Config) ClaudeResumeEnv() map[string]string {
	if c == nil || c.ClaudeResume == nil {
		return nil
	}
	env := map[string]string{}
	if c.ClaudeResume.ThresholdMinutes != nil {
		env[EnvResumeThresholdMinutes] = strconv.Itoa(*c.ClaudeResume.ThresholdMinutes)
	}
	if c.ClaudeResume.TokenThreshold != nil {
		env[EnvResumeTokenThreshold] = strconv.Itoa(*c.ClaudeResume.TokenThreshold)
	}
	return env
}

// ClaudeCleanupPeriodDaysOverride returns the tclaude-configured Claude Code
// transcript-retention override (config claude_cleanup_period_days) and whether
// tclaude should manage Claude Code's cleanupPeriodDays key at all. ok is false
// when the override is unset / non-positive, in which case Claude Code's own
// default (30 days) or the operator's hand-set settings.json value stands. When
// ok, days is the value tclaude writes into ~/.claude/settings.json. Nil-safe on
// the receiver.
func (c *Config) ClaudeCleanupPeriodDaysOverride() (days int, ok bool) {
	if c == nil || c.ClaudeCleanupPeriodDays <= 0 {
		return 0, false
	}
	return c.ClaudeCleanupPeriodDays, true
}

// ConvWatchConfig holds the watch view's persisted UI preferences.
type ConvWatchConfig struct {
	// Columns is the set of explicit column-visibility overrides, keyed by
	// column key ("harness", "project", "size", "modified", "groups"). A
	// key present here shadows that column's smart auto-default (e.g.
	// HARNESS auto-shows only when a non-Claude conv is present); an absent
	// key follows the auto rule. Written by the in-view column selector
	// (the `c` overlay); unknown keys are ignored by readers.
	Columns map[string]bool `json:"columns,omitempty"`
}

// SessionWatchConfig holds the live sessions view's persisted UI preferences.
type SessionWatchConfig struct {
	// Columns is the set of explicit column-visibility overrides. An absent
	// key follows that column's default; unknown keys are ignored by readers.
	// Written by the in-view `c` column selector.
	Columns map[string]bool `json:"columns,omitempty"`
}
