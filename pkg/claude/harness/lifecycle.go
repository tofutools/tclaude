package harness

// Lifecycle names the in-pane control slash commands a harness
// understands. tclaude drives long-running sessions by typing these into
// the harness's tmux pane (rename a session, compact its context,
// soft-exit it gracefully). The tokens are harness-specific and some
// harnesses lack a given control entirely.
//
// An empty token means "unsupported": the caller MUST skip that
// injection rather than type a command the pane can't parse. This matters
// because the tmux pane is an injection sink — typing an unknown `/foo`
// line submits it as a prompt. Callers gate on the Harness.Supports*
// helpers (which fold these tokens into booleans) before injecting.
type Lifecycle interface {
	// RenameCommand is the slash command that renames the session
	// (e.g. "/rename"); the title is appended by the caller. "" =
	// unsupported (tclaude falls back to its own title store).
	RenameCommand() string
	// CompactCommand is the slash command that compacts the session's
	// context (e.g. "/compact"). "" = unsupported (compaction is a no-op
	// for that harness).
	CompactCommand() string
	// SoftExitCommand is the slash command that ends the session
	// gracefully (e.g. "/exit"), as opposed to killing the tmux pane.
	// "" = unsupported (callers fall back to a hard tmux kill).
	SoftExitCommand() string
	// RemoteControlCommand is the slash command that TOGGLES the harness's
	// built-in remote-access feature (e.g. Claude Code's "/remote-control",
	// which exposes the session to claude.ai/code + the Claude mobile app).
	// "" = unsupported (the harness has no remote access; callers hide the
	// affordance). Note this is a toggle, not separate enable/disable: the
	// same command turns it on when off and off when on, so callers that
	// drive it must track the intended direction themselves (the harness
	// exposes no programmatic readback of the current state). See JOH-254.
	RemoteControlCommand() string
	// FastModeCommand is the no-argument command that toggles Fast mode for
	// the active thread. It is deliberately a compile-time harness token:
	// callers may inject it only after authoritative live state says Fast is
	// currently on. "" = unsupported.
	FastModeCommand() string
	// SoftExitPrefixKeys are tmux key names sent into the pane, in order,
	// immediately BEFORE the soft-exit command text (with the usual settle
	// gap after them). nil = send the command text alone, which is what
	// every harness whose TUI accepts a slash command in any state wants.
	//
	// It exists because a soft exit is the one injection that must land
	// whatever the pane is doing, and a TUI is free to refuse commands while
	// it is busy. Copilot 1.0.77 does exactly that: mid-turn it renders the
	// typed "/exit", then silently DISCARDS it on Enter — no exit, no queued
	// message, no transcript line — and with a permission dialog open the
	// same Enter accepts the dialog's default entry, which APPROVES the
	// pending command instead of exiting. A cancel key first puts the TUI
	// back into the state where the command is accepted. See
	// copilotfixture's soft-exit scenario for the measurement.
	SoftExitPrefixKeys() []string
}
