package agentd

import (
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// The Copilot half of the sandbox-lineage matrix, pinned as an exact list
// (TCL-989 PR2).
//
// It is measured here rather than folded into the PR1 delta file because it is
// a different kind of change. PR1 re-classified children that were already in
// the matrix, and its safety argument is that no parent gained a launch it
// could not already mint. PR2 admits a harness that was refused outright, so
// every entry below IS a new launch — which is exactly why it has to be
// enumerated rather than described.
//
// The one admitted pair is `tclaude-layer` + `off`: tclaude's own OS wall is
// the single claimed boundary and Copilot's own experimental MXC sandbox is
// asserted down. The launch boundary re-verifies that assertion on every path
// that can start a pane; this matrix only decides who may ask for it.

// copilotLineageParents is every parent posture the matrix can present,
// including the Copilot ones whose admission depends on the persisted
// implementation rather than the mode alone.
func copilotLineageParents() []spawnLineageSandbox {
	return []spawnLineageSandbox{
		{Harness: harness.DefaultName, Mode: harness.ClaudeSandboxOff},
		{Harness: harness.DefaultName, Mode: harness.ClaudeSandboxInherit},
		{Harness: harness.DefaultName, Mode: harness.ClaudeSandboxOn},
		{Harness: harness.CodexName, Mode: harness.SandboxDangerFull},
		{Harness: harness.CodexName, Mode: harness.SandboxManagedProfile},
		{Harness: harness.CodexName, Mode: harness.SandboxWorkspaceWrite},
		{Harness: harness.CodexName, Mode: harness.SandboxReadOnly},
		{Harness: harness.OpenCodeName, Mode: harness.OpenCodeSandboxOff},
		{Harness: harness.OpenCodeName, Mode: harness.OpenCodeSandboxTclaudeLayer},
		{Harness: harness.OpenCodeName, Mode: harness.OpenCodeSandboxAccessControl},
		// The proven Copilot parent, and the two legacy rows that look like it
		// but assert nothing about who owns the wall.
		{
			Harness: harness.CopilotName, Mode: harness.CopilotSandboxOff,
			Implementation: sandboxpolicy.ImplementationTclaudeLayer,
		},
		{
			Harness: harness.CopilotName, Mode: harness.CopilotSandboxOff,
			Implementation: sandboxpolicy.ImplementationHarnessBuiltin,
		},
		{
			Harness: harness.CopilotName, Mode: harness.CopilotSandboxInherit,
			Implementation: sandboxpolicy.ImplementationTclaudeLayer,
		},
	}
}

func lineageKey(s spawnLineageSandbox) string {
	if s.Harness == harness.CopilotName {
		return fmt.Sprintf("%s/%s[%s]", s.Harness, s.Mode, s.Implementation)
	}
	return fmt.Sprintf("%s/%s", s.Harness, s.Mode)
}

// TestSandboxLineageCopilotMatrix pins WHO may mint the proven Copilot child.
//
// The parents that appear are the ones whose own launch is at least as
// contained as the child's: a full-access parent trivially, and the confined
// ones because the outer wall the child gets is the same wall a `tclaude-layer`
// / managed-profile / access-control parent already lives behind. The Codex
// `read-only` and `workspace-write` parents are absent on purpose — they are
// the two postures PR1 already tightened against tclaude-walled children,
// whose cwd subtree the outer wall writes.
func TestSandboxLineageCopilotMatrix(t *testing.T) {
	provenChild := spawnLineageSandbox{
		Harness: harness.CopilotName, Mode: harness.CopilotSandboxOff,
		Implementation: sandboxpolicy.ImplementationTclaudeLayer,
	}
	var admitted []string
	for _, parent := range copilotLineageParents() {
		if spawnSandboxLineageAllowed(parent, provenChild) {
			admitted = append(admitted, lineageKey(parent))
		}
	}
	sort.Strings(admitted)
	require.Equal(t, []string{
		"claude/inherit",
		"claude/off",
		"claude/on",
		"codex/danger-full-access",
		"codex/tclaude-agent",
		"copilot/off[tclaude-layer]",
		"opencode/access-control",
		"opencode/off",
		"opencode/tclaude-layer",
	}, admitted, "exactly these parents may mint the proven Copilot child")
}

// A Copilot PARENT is classified by its persisted PAIR, not its mode. This is
// the one place parent-side implementation is read; Claude, Codex and OpenCode
// parents keep PR1's mode-only classification until TCL-991.
//
// The distinction is load-bearing rather than pedantic. Copilot `off` under
// `harness-builtin` asserts only that Copilot's own experimental wall is not
// engaged and that NOTHING replaced it — an unconfined agent wearing the same
// mode string as the most contained one.
func TestSandboxLineageCopilotParentOutboundMatrix(t *testing.T) {
	children := []spawnLineageSandbox{
		{
			Harness: harness.CopilotName, Mode: harness.CopilotSandboxOff,
			Implementation: sandboxpolicy.ImplementationTclaudeLayer,
		},
		{
			Harness: harness.CopilotName, Mode: harness.CopilotSandboxOff,
			Implementation: sandboxpolicy.ImplementationHarnessBuiltin,
		},
		{Harness: harness.CopilotName, Mode: harness.CopilotSandboxInherit},
		{Harness: harness.DefaultName, Mode: harness.ClaudeSandboxOn},
		{Harness: harness.DefaultName, Mode: harness.ClaudeSandboxInherit},
		{Harness: harness.DefaultName, Mode: harness.ClaudeSandboxOff},
		{Harness: harness.CodexName, Mode: harness.SandboxReadOnly},
		{Harness: harness.CodexName, Mode: harness.SandboxWorkspaceWrite},
		{Harness: harness.CodexName, Mode: harness.SandboxManagedProfile},
		{Harness: harness.CodexName, Mode: harness.SandboxDangerFull},
		{Harness: harness.OpenCodeName, Mode: harness.OpenCodeSandboxAccessControl},
		{Harness: harness.OpenCodeName, Mode: harness.OpenCodeSandboxTclaudeLayer},
		{Harness: harness.OpenCodeName, Mode: harness.OpenCodeSandboxOff},
	}

	t.Run("proven parent", func(t *testing.T) {
		parent := spawnLineageSandbox{
			Harness: harness.CopilotName, Mode: harness.CopilotSandboxOff,
			Implementation: sandboxpolicy.ImplementationTclaudeLayer,
		}
		var admitted []string
		for _, child := range children {
			if spawnSandboxLineageAllowed(parent, child) {
				admitted = append(admitted, lineageKey(child))
			}
		}
		sort.Strings(admitted)
		// No fully-open child, and no OpenCode child: tclaude's layer confines
		// OpenCode through its authoritative server rather than by wrapping the
		// pane, and no equivalence between that topology and this one has been
		// proven.
		require.Equal(t, []string{
			"claude/on",
			"codex/read-only",
			"codex/tclaude-agent",
			"codex/workspace-write",
			"copilot/off[tclaude-layer]",
		}, admitted, "the proven Copilot parent's outbound set")
	})

	for _, parent := range []spawnLineageSandbox{
		{
			Harness: harness.CopilotName, Mode: harness.CopilotSandboxOff,
			Implementation: sandboxpolicy.ImplementationHarnessBuiltin,
		},
		{
			Harness: harness.CopilotName, Mode: harness.CopilotSandboxOff,
			Implementation: sandboxpolicy.ImplementationStacked,
		},
		{
			Harness: harness.CopilotName, Mode: harness.CopilotSandboxInherit,
			Implementation: sandboxpolicy.ImplementationTclaudeLayer,
		},
		{Harness: harness.CopilotName, Mode: ""},
	} {
		t.Run("unproven parent "+lineageKey(parent), func(t *testing.T) {
			for _, child := range children {
				require.Falsef(t, spawnSandboxLineageAllowed(parent, child),
					"an unproven Copilot parent may mint nothing, got %s", lineageKey(child))
			}
		})
	}
}

// The `stacked` exclusion, stated on the harness it matters most for. Stacked
// runs Copilot's own experimental MXC policy nested inside tclaude's, so the
// effective confinement is the unreviewed intersection of two policies while
// the row names one.
func TestSandboxLineageCopilotRejectsStackedOnBothSides(t *testing.T) {
	stacked := spawnLineageSandbox{
		Harness: harness.CopilotName, Mode: harness.CopilotSandboxOff,
		Implementation: sandboxpolicy.ImplementationStacked,
	}
	require.False(t, spawnSandboxLineageAllowed(
		spawnLineageSandbox{Harness: harness.DefaultName, Mode: harness.ClaudeSandboxOff},
		stacked), "a stacked Copilot child is not the reviewed topology")
	require.False(t, copilotProvenLineageLaunch(stacked))
}

// Every non-Copilot verdict must be byte-for-byte what PR1 left behind. The
// delta file measures PR1's own change; this pins that PR2 moved nothing else,
// including for the parents that gained a Copilot child.
func TestSandboxLineageCopilotAdmissionMovesNoOtherVerdict(t *testing.T) {
	nonCopilot := []spawnLineageSandbox{
		{Harness: harness.DefaultName, Mode: harness.ClaudeSandboxInherit},
		{Harness: harness.DefaultName, Mode: harness.ClaudeSandboxOn},
		{Harness: harness.DefaultName, Mode: harness.ClaudeSandboxOff},
		{Harness: harness.CodexName, Mode: harness.SandboxReadOnly},
		{Harness: harness.CodexName, Mode: harness.SandboxWorkspaceWrite},
		{Harness: harness.CodexName, Mode: harness.SandboxManagedProfile},
		{Harness: harness.CodexName, Mode: harness.SandboxDangerFull},
		{Harness: harness.OpenCodeName, Mode: harness.OpenCodeSandboxAccessControl},
		{Harness: harness.OpenCodeName, Mode: harness.OpenCodeSandboxTclaudeLayer},
		{Harness: harness.OpenCodeName, Mode: harness.OpenCodeSandboxOff},
	}
	var admitted []string
	for _, parent := range lineageDeltaParents() {
		for _, child := range nonCopilot {
			for _, implementation := range []sandboxpolicy.Implementation{
				sandboxpolicy.ImplementationHarnessBuiltin,
				sandboxpolicy.ImplementationTclaudeLayer,
			} {
				c := child
				c.Implementation = implementation
				c.Mode = lineageConfinementMode(c)
				if spawnSandboxLineageAllowed(parent, c) {
					admitted = append(admitted,
						fmt.Sprintf("%s -> %s[%s]", lineageKey(parent), lineageKey(child), implementation))
				}
			}
		}
	}
	sort.Strings(admitted)
	require.Equal(t, nonCopilotLineageBaseline, admitted,
		"PR2 must not move a single non-Copilot verdict")
}

// nonCopilotLineageBaseline is the complete non-Copilot verdict set, captured
// from the merged PR1 tree (origin/main 81056c5cd) BEFORE this change and
// re-asserted against it after, so "PR2 changed nothing else" is a measurement
// rather than a claim.
var nonCopilotLineageBaseline = []string{
	"claude/inherit -> claude/inherit[harness-builtin]",
	"claude/inherit -> claude/inherit[tclaude-layer]",
	"claude/inherit -> claude/off[tclaude-layer]",
	"claude/inherit -> claude/on[harness-builtin]",
	"claude/inherit -> claude/on[tclaude-layer]",
	"claude/inherit -> codex/danger-full-access[tclaude-layer]",
	"claude/inherit -> codex/read-only[harness-builtin]",
	"claude/inherit -> codex/read-only[tclaude-layer]",
	"claude/inherit -> codex/tclaude-agent[harness-builtin]",
	"claude/inherit -> codex/tclaude-agent[tclaude-layer]",
	"claude/inherit -> codex/workspace-write[harness-builtin]",
	"claude/inherit -> codex/workspace-write[tclaude-layer]",
	"claude/off -> claude/inherit[harness-builtin]",
	"claude/off -> claude/inherit[tclaude-layer]",
	"claude/off -> claude/off[harness-builtin]",
	"claude/off -> claude/off[tclaude-layer]",
	"claude/off -> claude/on[harness-builtin]",
	"claude/off -> claude/on[tclaude-layer]",
	"claude/off -> codex/danger-full-access[harness-builtin]",
	"claude/off -> codex/danger-full-access[tclaude-layer]",
	"claude/off -> codex/read-only[harness-builtin]",
	"claude/off -> codex/read-only[tclaude-layer]",
	"claude/off -> codex/tclaude-agent[harness-builtin]",
	"claude/off -> codex/tclaude-agent[tclaude-layer]",
	"claude/off -> codex/workspace-write[harness-builtin]",
	"claude/off -> codex/workspace-write[tclaude-layer]",
	"claude/off -> opencode/access-control[harness-builtin]",
	"claude/off -> opencode/access-control[tclaude-layer]",
	"claude/off -> opencode/off[harness-builtin]",
	"claude/off -> opencode/off[tclaude-layer]",
	"claude/off -> opencode/tclaude-layer[harness-builtin]",
	"claude/off -> opencode/tclaude-layer[tclaude-layer]",
	"claude/on -> claude/off[tclaude-layer]",
	"claude/on -> claude/on[harness-builtin]",
	"claude/on -> claude/on[tclaude-layer]",
	"claude/on -> codex/danger-full-access[tclaude-layer]",
	"claude/on -> codex/read-only[harness-builtin]",
	"claude/on -> codex/read-only[tclaude-layer]",
	"claude/on -> codex/tclaude-agent[harness-builtin]",
	"claude/on -> codex/tclaude-agent[tclaude-layer]",
	"claude/on -> codex/workspace-write[harness-builtin]",
	"claude/on -> codex/workspace-write[tclaude-layer]",
	"codex/danger-full-access -> claude/inherit[harness-builtin]",
	"codex/danger-full-access -> claude/inherit[tclaude-layer]",
	"codex/danger-full-access -> claude/off[harness-builtin]",
	"codex/danger-full-access -> claude/off[tclaude-layer]",
	"codex/danger-full-access -> claude/on[harness-builtin]",
	"codex/danger-full-access -> claude/on[tclaude-layer]",
	"codex/danger-full-access -> codex/danger-full-access[harness-builtin]",
	"codex/danger-full-access -> codex/danger-full-access[tclaude-layer]",
	"codex/danger-full-access -> codex/read-only[harness-builtin]",
	"codex/danger-full-access -> codex/read-only[tclaude-layer]",
	"codex/danger-full-access -> codex/tclaude-agent[harness-builtin]",
	"codex/danger-full-access -> codex/tclaude-agent[tclaude-layer]",
	"codex/danger-full-access -> codex/workspace-write[harness-builtin]",
	"codex/danger-full-access -> codex/workspace-write[tclaude-layer]",
	"codex/danger-full-access -> opencode/access-control[harness-builtin]",
	"codex/danger-full-access -> opencode/access-control[tclaude-layer]",
	"codex/danger-full-access -> opencode/off[harness-builtin]",
	"codex/danger-full-access -> opencode/off[tclaude-layer]",
	"codex/danger-full-access -> opencode/tclaude-layer[harness-builtin]",
	"codex/danger-full-access -> opencode/tclaude-layer[tclaude-layer]",
	"codex/read-only -> codex/read-only[harness-builtin]",
	"codex/read-only -> codex/read-only[tclaude-layer]",
	"codex/tclaude-agent -> claude/inherit[harness-builtin]",
	"codex/tclaude-agent -> claude/inherit[tclaude-layer]",
	"codex/tclaude-agent -> claude/off[tclaude-layer]",
	"codex/tclaude-agent -> claude/on[harness-builtin]",
	"codex/tclaude-agent -> claude/on[tclaude-layer]",
	"codex/tclaude-agent -> codex/danger-full-access[tclaude-layer]",
	"codex/tclaude-agent -> codex/read-only[harness-builtin]",
	"codex/tclaude-agent -> codex/read-only[tclaude-layer]",
	"codex/tclaude-agent -> codex/tclaude-agent[harness-builtin]",
	"codex/tclaude-agent -> codex/tclaude-agent[tclaude-layer]",
	"codex/tclaude-agent -> codex/workspace-write[harness-builtin]",
	"codex/tclaude-agent -> codex/workspace-write[tclaude-layer]",
	"codex/workspace-write -> codex/read-only[harness-builtin]",
	"codex/workspace-write -> codex/read-only[tclaude-layer]",
	"codex/workspace-write -> codex/workspace-write[harness-builtin]",
	"codex/workspace-write -> codex/workspace-write[tclaude-layer]",
	"opencode/access-control -> claude/off[tclaude-layer]",
	"opencode/access-control -> claude/on[harness-builtin]",
	"opencode/access-control -> claude/on[tclaude-layer]",
	"opencode/access-control -> codex/danger-full-access[tclaude-layer]",
	"opencode/access-control -> codex/read-only[harness-builtin]",
	"opencode/access-control -> codex/read-only[tclaude-layer]",
	"opencode/access-control -> codex/tclaude-agent[harness-builtin]",
	"opencode/access-control -> codex/tclaude-agent[tclaude-layer]",
	"opencode/access-control -> codex/workspace-write[harness-builtin]",
	"opencode/access-control -> codex/workspace-write[tclaude-layer]",
	"opencode/access-control -> opencode/access-control[harness-builtin]",
	"opencode/access-control -> opencode/access-control[tclaude-layer]",
	"opencode/access-control -> opencode/tclaude-layer[harness-builtin]",
	"opencode/access-control -> opencode/tclaude-layer[tclaude-layer]",
	"opencode/off -> claude/inherit[harness-builtin]",
	"opencode/off -> claude/inherit[tclaude-layer]",
	"opencode/off -> claude/off[harness-builtin]",
	"opencode/off -> claude/off[tclaude-layer]",
	"opencode/off -> claude/on[harness-builtin]",
	"opencode/off -> claude/on[tclaude-layer]",
	"opencode/off -> codex/danger-full-access[harness-builtin]",
	"opencode/off -> codex/danger-full-access[tclaude-layer]",
	"opencode/off -> codex/read-only[harness-builtin]",
	"opencode/off -> codex/read-only[tclaude-layer]",
	"opencode/off -> codex/tclaude-agent[harness-builtin]",
	"opencode/off -> codex/tclaude-agent[tclaude-layer]",
	"opencode/off -> codex/workspace-write[harness-builtin]",
	"opencode/off -> codex/workspace-write[tclaude-layer]",
	"opencode/off -> opencode/access-control[harness-builtin]",
	"opencode/off -> opencode/access-control[tclaude-layer]",
	"opencode/off -> opencode/off[harness-builtin]",
	"opencode/off -> opencode/off[tclaude-layer]",
	"opencode/off -> opencode/tclaude-layer[harness-builtin]",
	"opencode/off -> opencode/tclaude-layer[tclaude-layer]",
	"opencode/tclaude-layer -> claude/off[tclaude-layer]",
	"opencode/tclaude-layer -> claude/on[harness-builtin]",
	"opencode/tclaude-layer -> claude/on[tclaude-layer]",
	"opencode/tclaude-layer -> codex/danger-full-access[tclaude-layer]",
	"opencode/tclaude-layer -> codex/read-only[harness-builtin]",
	"opencode/tclaude-layer -> codex/read-only[tclaude-layer]",
	"opencode/tclaude-layer -> codex/tclaude-agent[harness-builtin]",
	"opencode/tclaude-layer -> codex/tclaude-agent[tclaude-layer]",
	"opencode/tclaude-layer -> codex/workspace-write[harness-builtin]",
	"opencode/tclaude-layer -> codex/workspace-write[tclaude-layer]",
	"opencode/tclaude-layer -> opencode/tclaude-layer[harness-builtin]",
	"opencode/tclaude-layer -> opencode/tclaude-layer[tclaude-layer]",
}
