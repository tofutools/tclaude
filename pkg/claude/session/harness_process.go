package session

import (
	"slices"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// isHarnessProcessName reports whether a process name is one of the
// coding-harness runtimes tclaude drives. The process-tree walk in
// FindClaudePID uses it to recognise the harness ancestor of a
// `tclaude session hook-callback` process (the callback is a child of the
// harness that invoked it).
//
// "node" is matched because Claude Code runs as a node process; the rest
// come from the harness registry (claude, codex, …), so a newly-registered
// harness binary is recognised without editing this function — the gap
// that left a Codex session at PID 0 (its parent is "codex", which the old
// hard-coded claude/node match missed), risking a false-positive reap of a
// non-tmux row.
func isHarnessProcessName(name string) bool {
	return name == "node" || slices.Contains(harness.SpawnBinaries(), name)
}
