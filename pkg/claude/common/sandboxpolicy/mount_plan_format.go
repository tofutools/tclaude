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
	case MountTmpfs:
		return "tmpfs"
	default:
		return fmt.Sprintf("mount-mode(%d)", int(m))
	}
}

// String renders the network posture as the stable token used in effective
// plan output and launch-fidelity messages.
func (p NetworkPosture) String() string {
	switch p {
	case NetworkHostOpen:
		return "host-open"
	case NetworkIsolatedWithAgentd:
		return "isolated-with-agentd"
	case NetworkFiltered:
		return "filtered"
	default:
		return fmt.Sprintf("network-posture(%d)", int(p))
	}
}

// String renders the root posture as the stable token used in effective plan
// output. It is printed alongside the network posture because since TCL-798 the
// two are independent: a plan can construct its root while keeping the host
// network, and a reader of a dry-run plan must be able to tell which.
func (p RootPosture) String() string {
	switch p {
	case RootHostInherited:
		return "host-inherited"
	case RootConstructed:
		return "constructed"
	default:
		return fmt.Sprintf("root-posture(%d)", int(p))
	}
}

// String renders the whole plan in application order. Entries are indented and
// the mode column is padded to the width of the longest mode token so paths
// line up, which is what makes a two-plan diff readable. The output always ends in a newline; an empty plan is stated
// explicitly rather than rendering as nothing, so "no entries" and "not
// rendered" cannot be confused in a dry-run surface.
func (p MountPlan) String() string {
	var b strings.Builder
	b.WriteString("mount-plan:\n")
	fmt.Fprintf(&b, "  network %s\n", p.NetworkPosture)
	fmt.Fprintf(&b, "  root %s\n", p.EffectiveRootPosture())
	for _, alias := range p.Aliases {
		fmt.Fprintf(&b, "  alias %s -> %s\n", alias.Link, alias.Target)
	}
	if len(p.Entries) == 0 {
		b.WriteString("  mounts  (empty)\n")
		return b.String()
	}
	for _, entry := range p.Entries {
		// A tmpfs discloses its ceiling, because "tmpfs /scratch" and "tmpfs
		// /scratch capped at 512MiB" are different policies and a plan diff has
		// to show the difference. An uncapped one prints bare rather than
		// printing a zero, which would read as "no space".
		if entry.Mode == MountTmpfs {
			if entry.SizeBytes > 0 {
				fmt.Fprintf(&b, "  %-5s %s (max %d bytes)\n", entry.Mode, entry.Path, entry.SizeBytes)
				continue
			}
			fmt.Fprintf(&b, "  %-5s %s\n", entry.Mode, entry.Path)
			continue
		}
		// A remapped entry discloses both sides. Printing only the guest path
		// would read as an ordinary same-path mount and hide which host
		// directory the authority actually came from.
		if entry.IsRemapped() {
			fmt.Fprintf(&b, "  %-5s %s <- %s\n", entry.Mode, entry.Path, entry.Source)
			continue
		}
		fmt.Fprintf(&b, "  %-5s %s\n", entry.Mode, entry.Path)
	}
	return b.String()
}
