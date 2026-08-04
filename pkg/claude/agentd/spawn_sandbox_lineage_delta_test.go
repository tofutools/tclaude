package agentd

import (
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Judging a tclaude-layer child by the mode it LAUNCHES under rather than the
// mode it REQUESTED moves some verdicts. This file measures exactly which, so
// the change is a recorded decision instead of a claim, and pins the property
// that makes the loosened ones safe.
//
// The "before" side is reproduced honestly, which means reproducing the whole
// pre-PR path and not just the guard: handleGroupSpawn already ran
// ResolveHarnessBuiltinMode and then the resolver whose old body is today's
// ResolveNativeHarnessBuiltinMode BEFORE calling the guard. That resolver is
// the identity for Claude and Codex, but NOT for OpenCode — an OpenCode child
// requesting `access-control` under tclaude-layer reached the old guard already
// spelled `tclaude-layer`. Judging the raw request string here would invent a
// verdict change that never happened in production.

func lineageDeltaParents() []spawnLineageSandbox {
	return []spawnLineageSandbox{
		{Harness: harness.DefaultName, HarnessBuiltinMode: harness.ClaudeSandboxOff},
		{Harness: harness.DefaultName, HarnessBuiltinMode: harness.ClaudeSandboxInherit},
		{Harness: harness.DefaultName, HarnessBuiltinMode: harness.ClaudeSandboxOn},
		{Harness: harness.CodexName, HarnessBuiltinMode: harness.SandboxDangerFull},
		{Harness: harness.CodexName, HarnessBuiltinMode: harness.SandboxManagedProfile},
		{Harness: harness.CodexName, HarnessBuiltinMode: harness.SandboxWorkspaceWrite},
		{Harness: harness.CodexName, HarnessBuiltinMode: harness.SandboxReadOnly},
		{Harness: harness.OpenCodeName, HarnessBuiltinMode: harness.OpenCodeSandboxOff},
		{Harness: harness.OpenCodeName, HarnessBuiltinMode: harness.OpenCodeSandboxTclaudeLayer},
		{Harness: harness.OpenCodeName, HarnessBuiltinMode: harness.OpenCodeSandboxAccessControl},
	}
}

// Every child request shape a spawn boundary can present, including the blank
// mode a caller who chose nothing sends.
//
// Copilot is deliberately absent, and its absence is the point. This file
// measures ONE decision: what moved when a tclaude-layer child started being
// judged by the mode it launches under (TCL-989 PR1). For Claude, Codex and
// OpenCode that mapping is the whole story, and the two assertions below hold
// because a newly-admitted request only re-spells a launch the parent could
// already mint.
//
// Copilot's admission is a different decision made later, in PR2: the harness
// was not in the matrix at all, and adding it grants parents a launch they
// genuinely could not mint before. Folding it in here would both inflate this
// PR1 measurement and falsify the no-new-launch property the next test pins.
// The Copilot matrix is measured on its own terms by
// TestSandboxLineageCopilotMatrix in spawn_sandbox_lineage_copilot_test.go;
// what THIS file guarantees for PR2 is that every non-Copilot verdict is
// unchanged.
func lineageDeltaChildRequests() []struct{ Harness, Mode string } {
	return []struct{ Harness, Mode string }{
		{harness.DefaultName, ""},
		{harness.DefaultName, harness.ClaudeSandboxInherit},
		{harness.DefaultName, harness.ClaudeSandboxOn},
		{harness.DefaultName, harness.ClaudeSandboxOff},
		{harness.CodexName, harness.SandboxReadOnly},
		{harness.CodexName, harness.SandboxWorkspaceWrite},
		{harness.CodexName, harness.SandboxManagedProfile},
		{harness.CodexName, harness.SandboxDangerFull},
		{harness.OpenCodeName, harness.OpenCodeSandboxAccessControl},
		{harness.OpenCodeName, harness.OpenCodeSandboxTclaudeLayer},
	}
}

// forcedTclaudeLayerChild returns the child the guard now judges, or ok=false
// when the pair is refused before the guard ever sees it.
func forcedTclaudeLayerChild(t *testing.T, harnessName, requested string) (spawnLineageSandbox, bool) {
	t.Helper()
	h, err := harness.Resolve(harnessName)
	require.NoError(t, err)
	forced, err := harness.ResolveSandboxImplementationMode(
		h, resolveSpawnBoundaryMode(t, h, requested),
		sandboxpolicy.ImplementationTclaudeLayer)
	if err != nil {
		return spawnLineageSandbox{}, false
	}
	return spawnLineageSandbox{
		Harness: harnessName, HarnessBuiltinMode: forced,
		Implementation: sandboxpolicy.ImplementationTclaudeLayer,
	}, true
}

// preForcingTclaudeLayerChild returns the child the PRE-PR guard judged: the
// request after the spawn boundary's own defaulting and harness-native
// resolution, with no implementation and therefore no mapping.
func preForcingTclaudeLayerChild(t *testing.T, harnessName, requested string) (spawnLineageSandbox, bool) {
	t.Helper()
	h, err := harness.Resolve(harnessName)
	require.NoError(t, err)
	native, err := harness.ResolveNativeHarnessBuiltinMode(
		h, resolveSpawnBoundaryMode(t, h, requested),
		sandboxpolicy.ImplementationTclaudeLayer)
	if err != nil {
		return spawnLineageSandbox{}, false
	}
	return spawnLineageSandbox{Harness: harnessName, HarnessBuiltinMode: native}, true
}

// resolveSpawnBoundaryMode applies the secure default the daemon spawn boundary
// applies to a requested mode, so a blank request is judged as the mode it
// actually becomes rather than as "nothing chosen".
func resolveSpawnBoundaryMode(t *testing.T, h *harness.Harness, requested string) string {
	t.Helper()
	resolved, err := harness.ResolveHarnessBuiltinMode(h, requested)
	require.NoError(t, err)
	return resolved
}

// The measured delta, pinned as an exact list. A change to the mapping that
// moves any other verdict fails here and has to be argued for explicitly.
func TestSandboxLineageTclaudeLayerVerdictDelta(t *testing.T) {
	var loosened, tightened []string
	for _, parent := range lineageDeltaParents() {
		for _, req := range lineageDeltaChildRequests() {
			child, ok := forcedTclaudeLayerChild(t, req.Harness, req.Mode)
			if !ok {
				continue
			}
			beforeChild, beforeOK := preForcingTclaudeLayerChild(t, req.Harness, req.Mode)
			if !beforeOK {
				continue
			}
			before := spawnSandboxLineageAllowed(parent, beforeChild)
			now := spawnSandboxLineageAllowed(parent, child)
			if before == now {
				continue
			}
			entry := fmt.Sprintf("%s/%s -> %s/%s", parent.Harness, parent.HarnessBuiltinMode, req.Harness, req.Mode)
			if now {
				loosened = append(loosened, entry)
			} else {
				tightened = append(tightened, entry)
			}
		}
	}
	sort.Strings(loosened)
	sort.Strings(tightened)

	require.Equal(t, []string{
		"codex/read-only -> codex/read-only",
		"codex/workspace-write -> codex/read-only",
		"codex/workspace-write -> codex/workspace-write",
	}, tightened, "the tightened set must stay exactly these three")

	require.Equal(t, []string{
		"claude/inherit -> claude/off",
		"claude/inherit -> codex/danger-full-access",
		"claude/on -> claude/",
		"claude/on -> claude/inherit",
		"claude/on -> claude/off",
		"claude/on -> codex/danger-full-access",
		"codex/tclaude-agent -> claude/off",
		"codex/tclaude-agent -> codex/danger-full-access",
		"opencode/access-control -> claude/",
		"opencode/access-control -> claude/inherit",
		"opencode/access-control -> claude/off",
		"opencode/access-control -> codex/danger-full-access",
		"opencode/tclaude-layer -> claude/",
		"opencode/tclaude-layer -> claude/inherit",
		"opencode/tclaude-layer -> claude/off",
		"opencode/tclaude-layer -> codex/danger-full-access",
	}, loosened, "the loosened set must stay exactly these sixteen")
}

// The property that makes the loosened verdicts safe: none of them lets a
// parent mint a LAUNCH it could not already mint.
//
// A newly-admitted request differs from an already-admitted one only in the
// inner sandbox mode it asked for — a value the outer wall discards, so both
// requests produce the identical session row. The parent therefore gains no
// reachable launch, only the ability to spell the same one another way.
//
// The assertion is exactly that: for every newly-admitted pair, the old guard
// already admitted some other request shape from the same parent that forces to
// the same launch mode.
func TestSandboxLineageTclaudeLayerLooseningGrantsNoNewLaunch(t *testing.T) {
	checked := 0
	for _, parent := range lineageDeltaParents() {
		for _, req := range lineageDeltaChildRequests() {
			child, ok := forcedTclaudeLayerChild(t, req.Harness, req.Mode)
			if !ok {
				continue
			}
			beforeChild, beforeOK := preForcingTclaudeLayerChild(t, req.Harness, req.Mode)
			if !beforeOK {
				continue
			}
			if spawnSandboxLineageAllowed(parent, beforeChild) ||
				!spawnSandboxLineageAllowed(parent, child) {
				continue // not a newly-admitted pair
			}
			checked++

			// The blank mode is itself a valid equivalent request, so track
			// "found" separately from the mode it found.
			equivalentFound := false
			for _, alt := range lineageDeltaChildRequests() {
				if alt.Harness != req.Harness {
					continue
				}
				altChild, altOK := forcedTclaudeLayerChild(t, alt.Harness, alt.Mode)
				if !altOK || altChild.HarnessBuiltinMode != child.HarnessBuiltinMode {
					continue // would not produce the same launch
				}
				altBefore, altBeforeOK := preForcingTclaudeLayerChild(t, alt.Harness, alt.Mode)
				if altBeforeOK && spawnSandboxLineageAllowed(parent, altBefore) {
					equivalentFound = true
					break
				}
			}
			require.Truef(t, equivalentFound,
				"parent %s/%s newly admits %s/%q, but could NOT already mint the identical "+
					"launch (%s/%q) by any other request — that would be a real capability widening",
				parent.Harness, parent.HarnessBuiltinMode, req.Harness, req.Mode, child.Harness, child.HarnessBuiltinMode)
		}
	}
	require.NotZero(t, checked, "the property must actually have been exercised")
}

// Family (b), pinned directly: an explicit no-inner-sandbox request under the
// outer wall is admitted for a parent that is otherwise eligible. The requested
// `off` / `danger-full-access` is inert — tclaude's wall is what confines the
// child — so the guard judges the wall, not the discarded request.
func TestSandboxLineageAdmitsExplicitlyUnsandboxedTclaudeLayerChild(t *testing.T) {
	claudeOff, ok := forcedTclaudeLayerChild(t, harness.DefaultName, harness.ClaudeSandboxOff)
	require.True(t, ok)
	codexDanger, ok := forcedTclaudeLayerChild(t, harness.CodexName, harness.SandboxDangerFull)
	require.True(t, ok)

	for _, parent := range []spawnLineageSandbox{
		{Harness: harness.DefaultName, HarnessBuiltinMode: harness.ClaudeSandboxInherit},
		{Harness: harness.DefaultName, HarnessBuiltinMode: harness.ClaudeSandboxOn},
		{Harness: harness.CodexName, HarnessBuiltinMode: harness.SandboxManagedProfile},
		{Harness: harness.OpenCodeName, HarnessBuiltinMode: harness.OpenCodeSandboxTclaudeLayer},
		{Harness: harness.OpenCodeName, HarnessBuiltinMode: harness.OpenCodeSandboxAccessControl},
	} {
		require.Truef(t, spawnSandboxLineageAllowed(parent, claudeOff),
			"parent %s/%s must admit an explicitly-unsandboxed Claude tclaude-layer child",
			parent.Harness, parent.HarnessBuiltinMode)
		require.Truef(t, spawnSandboxLineageAllowed(parent, codexDanger),
			"parent %s/%s must admit an explicitly-unsandboxed Codex tclaude-layer child",
			parent.Harness, parent.HarnessBuiltinMode)
	}

	// Still gated by the parent's own confinement: a Codex workspace-write
	// parent cannot reach the managed-profile class either way.
	narrow := spawnLineageSandbox{Harness: harness.CodexName, HarnessBuiltinMode: harness.SandboxWorkspaceWrite}
	require.False(t, spawnSandboxLineageAllowed(narrow, claudeOff))
	require.False(t, spawnSandboxLineageAllowed(narrow, codexDanger))
}
