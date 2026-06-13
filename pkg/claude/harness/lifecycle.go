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
}
