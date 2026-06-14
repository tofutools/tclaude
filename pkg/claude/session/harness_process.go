package session

import (
	"slices"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// IsHarnessProcessName reports whether a process name is one of the
// coding-harness runtimes tclaude drives. Two process-tree walks rely on
// it to recognise the harness ancestor of a descendant process:
//
//   - FindClaudePID, walking up from a `tclaude session hook-callback`
//     (the callback is a child of the harness that invoked it), records a
//     real ancestor PID instead of 0.
//   - agentd's identity resolution (convIDForPID), walking up from the
//     socket peer, recognises an agent's harness ancestor so a Codex agent
//     is identified the same way a Claude Code one is (JOH-206).
//
// "node" is matched because Claude Code runs as a node process; the rest
// come from the harness registry (claude, codex, …), so a newly-registered
// harness binary is recognised without editing this function — the gap
// that left a Codex session at PID 0 (its parent is "codex", which the old
// hard-coded claude/node match missed), risking a false-positive reap of a
// non-tmux row.
func IsHarnessProcessName(name string) bool {
	return name == "node" || slices.Contains(harness.SpawnBinaries(), name)
}
