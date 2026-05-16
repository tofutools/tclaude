package session

import (
	"errors"
	"fmt"
)

// ClaudeAncestorCheck reports whether the current process is running
// underneath a Claude Code instance — i.e. whether a `claude`/`node`
// process appears anywhere in this process's own ancestry, walked up
// from os.Getppid().
//
// It is a package var, not a plain func, so tests (and the agentd flow
// tests) can substitute a deterministic implementation — "pretend we
// have a claude ancestor" — without literally running the test binary
// under Claude Code. Production points it at FindClaudePID, the same
// process-tree walk agentd's identity middleware uses (via convIDForPID)
// to tell a human caller apart from an agent.
var ClaudeAncestorCheck = func() bool {
	return FindClaudePID() != 0
}

// ErrNestedClaudeSpawn is the sentinel wrapped by the error
// GuardAgainstNestedSpawn returns, so callers/tests can match it with
// errors.Is regardless of the human-readable explanation appended.
var ErrNestedClaudeSpawn = errors.New("refusing to launch a nested Claude Code session")

// GuardAgainstNestedSpawn refuses to start a new Claude Code session
// when the calling `tclaude` process is itself running underneath a
// Claude Code instance. This stops a runaway chain of CC instances
// launching each other directly via `tclaude session new` (or bare
// `tclaude`).
//
// Daemon-initiated spawns are deliberately unaffected: `tclaude agent
// spawn` / `groups resume` make the agentd daemon fork
// `tclaude session new`. agentd is started by the human and is not a CC
// instance, so a daemon-forked `session new` has agentd (and the
// human's shell) in its ancestry — no `claude`/`node` — and
// ClaudeAncestorCheck returns false for it. Agents that genuinely need
// another session go through `tclaude agent spawn`, which the daemon
// gates on the `groups.spawn` permission.
//
// Known limitation: if the human starts `tclaude agentd serve` from
// inside a Claude Code session, daemon-forked spawns would inherit that
// claude ancestor and be refused too. agentd is expected to be launched
// from a plain shell / login / tray, so this is accepted rather than
// papered over with a bypass flag a CC instance could trivially set.
func GuardAgainstNestedSpawn() error {
	if !ClaudeAncestorCheck() {
		return nil
	}
	return fmt.Errorf("%w\n\n"+
		"This tclaude process is running inside an existing Claude Code\n"+
		"instance. Launching another Claude Code session from here is\n"+
		"blocked to prevent runaway, nested agent spawns.\n\n"+
		"  - Agents that need another session should use `tclaude agent spawn`,\n"+
		"    which is routed through the agentd daemon and gated by the\n"+
		"    `groups.spawn` permission.\n"+
		"  - Humans should run `tclaude` from a terminal that is not attached\n"+
		"    to a Claude Code session",
		ErrNestedClaudeSpawn)
}
