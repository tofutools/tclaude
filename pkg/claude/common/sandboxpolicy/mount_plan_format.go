package sandboxpolicy

import (
	"fmt"
	"strings"
)

// Deterministic, diffable rendering of a MountPlan (TCL-751, epic requirement
// 8: escape-hatch and audit parity). The format is the groundwork for a future
// dry-run / effective-plan surface and is what the renderer's snapshot tests
// assert against, so it is a stable, line-oriented, human-diffable shape rather
// than a Go value dump: one entry per line, in plan order, so a diff between
// two plans points at the rule that changed.
//
// The methods live here rather than in mount_plan.go because that file is the
// IR contract owned by the walking-skeleton work (TCL-750); keeping the
// presentation layer in its own file keeps the two independently editable.

// String renders one mount mode as the short token used in plan output and
// operator-facing messages. It is total: an unrecognized value renders
// visibly rather than silently reading as one of the known modes.
func (m MountMode) String() string {
	switch m {
	case MountRO:
		return "ro"
	case MountRW:
		return "rw"
	case MountHide:
		return "hide"
	default:
		return fmt.Sprintf("mount-mode(%d)", int(m))
	}
}

// String renders the whole plan in application order. Entries are indented and
// the mode column is padded so paths line up, which is what makes a two-plan
// diff readable. The output always ends in a newline; an empty plan is stated
// explicitly rather than rendering as nothing, so "no entries" and "not
// rendered" cannot be confused in a dry-run surface.
func (p MountPlan) String() string {
	var b strings.Builder
	b.WriteString("mount-plan:\n")
	if len(p.Entries) == 0 {
		b.WriteString("  (empty)\n")
		return b.String()
	}
	for _, entry := range p.Entries {
		fmt.Fprintf(&b, "  %-4s %s\n", entry.Mode, entry.Path)
	}
	return b.String()
}
