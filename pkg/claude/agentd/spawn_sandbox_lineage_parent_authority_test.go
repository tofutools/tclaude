package agentd

import (
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// TCL-991 classifies a PARENT by the confinement its launch actually has rather
// than by the mode alone. This file measures the blast radius of that, and it
// does so over every (harness, mode, implementation) triple a session row can
// hold — derived from the recording path itself
// (ResolveSandboxMode -> ResolveSandboxImplementationMode), which is what
// decides what ends up in the row. That is a stronger statement than sampling
// the rows that happen to exist today: a shape absent from one operator's
// database is still reachable by anyone passing the flags.

// recordableLineageLaunch is one row shape, with the mode the launch actually
// records rather than the mode that was requested.
type recordableLineageLaunch struct {
	harnessName string
	requested   string
	recorded    string
	impl        sandboxpolicy.Implementation
}

func (l recordableLineageLaunch) sandbox() spawnLineageSandbox {
	return spawnLineageSandbox{
		Harness: l.harnessName, HarnessBuiltinMode: l.recorded, Implementation: l.impl,
	}
}

// modeOnlySandbox is the PRE-TCL-991 classification of the same row, used as
// the "before" side of the measurement.
//
// Only Claude and Codex are respelled, because they are the only arms whose
// parent classification this change touches: it is their tclaude-layer launch
// that records a no-confinement mode, and harness-builtin is precisely the
// implementation the remap leaves alone. Copilot's parent arm already read the
// persisted pair (TCL-989) and OpenCode's tclaude-layer topology has its own
// mode, so respelling those would fabricate a "before" verdict main never
// produced — a Copilot row respelled harness-builtin stops normalizing at all.
//
// The reconstruction was checked against the real thing rather than trusted:
// the admitted-pair set this file enumerates was captured by running the same
// enumeration against origin/main with the production change reverted, and it
// matches the "before" column here exactly.
func (l recordableLineageLaunch) modeOnlySandbox() spawnLineageSandbox {
	s := l.sandbox()
	switch l.harnessName {
	case harness.DefaultName, harness.CodexName:
		s.Implementation = sandboxpolicy.ImplementationHarnessBuiltin
	}
	return s
}

func (l recordableLineageLaunch) String() string {
	return fmt.Sprintf("%s/%s[%s]", l.harnessName, l.recorded, l.impl)
}

// recordableLineageLaunches enumerates every session row the launch path can
// write: each harness, each mode it publishes (plus the un-chosen blank
// request), under each implementation, run through the same resolvers spawn
// runs. Pairs the resolvers refuse never reach a row and are skipped.
func recordableLineageLaunches(t *testing.T) []recordableLineageLaunch {
	t.Helper()
	var out []recordableLineageLaunch
	for _, name := range harness.Names() {
		h, err := harness.Resolve(name)
		require.NoError(t, err)
		modes := []string{""}
		if h.SupportsSandbox() {
			modes = append(modes, h.Sandbox.Modes()...)
		}
		for _, mode := range modes {
			resolved, err := harness.ResolveSandboxMode(h, mode)
			if err != nil {
				continue
			}
			for _, impl := range []sandboxpolicy.Implementation{
				sandboxpolicy.ImplementationHarnessBuiltin,
				sandboxpolicy.ImplementationTclaudeLayer,
				sandboxpolicy.ImplementationStacked,
				sandboxpolicy.ImplementationOff,
			} {
				recorded, err := harness.ResolveSandboxImplementationMode(h, resolved, impl)
				if err != nil {
					continue
				}
				out = append(out, recordableLineageLaunch{
					harnessName: name, requested: mode,
					recorded: recorded, impl: impl,
				})
			}
		}
	}
	require.NotEmpty(t, out)
	return out
}

// The exact set of parent row shapes whose delegation authority moves, and the
// exact children each one loses. Anything else moving fails here and has to be
// argued for.
//
// Read the list as the answer to "who will start seeing refusals": ONLY parents
// recorded with sandbox_implementation=tclaude-layer, and only the Claude and
// Codex ones — the two harnesses whose tclaude-layer launch records a mode that
// spells no-confinement. A row with the implementation unset, harness-builtin,
// stacked or off keeps every verdict it has, which covers every launch made
// before the implementation axis existed.
func TestSandboxLineageParentAuthorityVerdictDelta(t *testing.T) {
	launches := recordableLineageLaunches(t)
	// Deduped by ROW shape: several requested modes record the same row (a
	// tclaude-layer launch discards the request), and the row is what the guard
	// reads, so each distinct pair must appear once.
	seen := map[string]bool{}
	var tightened, loosened []string
	for _, parent := range launches {
		for _, child := range launches {
			before := spawnSandboxLineageAllowed(parent.modeOnlySandbox(), child.sandbox())
			now := spawnSandboxLineageAllowed(parent.sandbox(), child.sandbox())
			if before == now {
				continue
			}
			entry := fmt.Sprintf("%s -> %s", parent, child)
			if seen[entry] {
				continue
			}
			seen[entry] = true
			if now {
				loosened = append(loosened, entry)
			} else {
				tightened = append(tightened, entry)
			}
		}
	}
	sort.Strings(tightened)
	sort.Strings(loosened)

	require.Empty(t, loosened,
		"reading the parent's implementation must never GRANT authority")
	require.Equal(t, []string{
		"claude/off[tclaude-layer] -> claude/inherit[harness-builtin]",
		"claude/off[tclaude-layer] -> claude/inherit[stacked]",
		"claude/off[tclaude-layer] -> claude/off[harness-builtin]",
		"claude/off[tclaude-layer] -> claude/off[off]",
		"claude/off[tclaude-layer] -> claude/off[stacked]",
		"claude/off[tclaude-layer] -> codex/danger-full-access[harness-builtin]",
		"claude/off[tclaude-layer] -> codex/danger-full-access[off]",
		"claude/off[tclaude-layer] -> codex/danger-full-access[stacked]",
		"claude/off[tclaude-layer] -> opencode/access-control[harness-builtin]",
		"claude/off[tclaude-layer] -> opencode/access-control[stacked]",
		"claude/off[tclaude-layer] -> opencode/off[harness-builtin]",
		"claude/off[tclaude-layer] -> opencode/off[off]",
		"claude/off[tclaude-layer] -> opencode/off[stacked]",
		"claude/off[tclaude-layer] -> opencode/tclaude-layer[tclaude-layer]",
		"codex/danger-full-access[tclaude-layer] -> claude/off[harness-builtin]",
		"codex/danger-full-access[tclaude-layer] -> claude/off[off]",
		"codex/danger-full-access[tclaude-layer] -> claude/off[stacked]",
		"codex/danger-full-access[tclaude-layer] -> codex/danger-full-access[harness-builtin]",
		"codex/danger-full-access[tclaude-layer] -> codex/danger-full-access[off]",
		"codex/danger-full-access[tclaude-layer] -> codex/danger-full-access[stacked]",
		"codex/danger-full-access[tclaude-layer] -> opencode/access-control[harness-builtin]",
		"codex/danger-full-access[tclaude-layer] -> opencode/access-control[stacked]",
		"codex/danger-full-access[tclaude-layer] -> opencode/off[harness-builtin]",
		"codex/danger-full-access[tclaude-layer] -> opencode/off[off]",
		"codex/danger-full-access[tclaude-layer] -> opencode/off[stacked]",
		"codex/danger-full-access[tclaude-layer] -> opencode/tclaude-layer[tclaude-layer]",
	}, tightened)
}

// The property the delta list above is an instance of, asserted directly so it
// survives the list being re-measured: only a tclaude-layer parent moves at all.
func TestSandboxLineageParentAuthorityMovesOnlyTclaudeLayerParents(t *testing.T) {
	launches := recordableLineageLaunches(t)
	for _, parent := range launches {
		if parent.impl == sandboxpolicy.ImplementationTclaudeLayer {
			continue
		}
		for _, child := range launches {
			require.Equalf(t,
				spawnSandboxLineageAllowed(parent.modeOnlySandbox(), child.sandbox()),
				spawnSandboxLineageAllowed(parent.sandbox(), child.sandbox()),
				"parent %s must keep its verdict for child %s", parent, child)
		}
	}
}

// The shape the operator's own spawn profiles use — sandbox=inherit (Claude) /
// tclaude-agent (Codex) with sandbox-impl unset — is untouched, pinned
// separately from the enumeration so the claim in the PR has a named test.
func TestSandboxLineageParentAuthorityLeavesDefaultSpawnProfilesAlone(t *testing.T) {
	launches := recordableLineageLaunches(t)
	for _, parent := range []spawnLineageSandbox{
		{Harness: harness.DefaultName, HarnessBuiltinMode: harness.ClaudeSandboxInherit},
		{Harness: harness.DefaultName, HarnessBuiltinMode: harness.ClaudeSandboxInherit,
			Implementation: sandboxpolicy.ImplementationHarnessBuiltin},
		{Harness: harness.CodexName, HarnessBuiltinMode: harness.SandboxManagedProfile},
		{Harness: harness.CodexName, HarnessBuiltinMode: harness.SandboxManagedProfile,
			Implementation: sandboxpolicy.ImplementationHarnessBuiltin},
	} {
		for _, child := range launches {
			require.Equalf(t,
				spawnSandboxLineageAllowed(
					spawnLineageSandbox{Harness: parent.Harness, HarnessBuiltinMode: parent.HarnessBuiltinMode,
						Implementation: sandboxpolicy.ImplementationHarnessBuiltin},
					child.sandbox()),
				spawnSandboxLineageAllowed(parent, child.sandbox()),
				"parent %s/%s[%s] must keep its verdict for child %s",
				parent.Harness, parent.HarnessBuiltinMode, parent.Implementation, child)
		}
	}
}

// A tclaude-layer parent is not merely narrowed; it lands on exactly the class
// its wall confines it to, so it keeps every child that class could always mint.
func TestSandboxLineageTclaudeLayerParentDelegatesAsItsConfinementClass(t *testing.T) {
	layerClaude := spawnLineageSandbox{
		Harness: harness.DefaultName, HarnessBuiltinMode: harness.ClaudeSandboxOff,
		Implementation: sandboxpolicy.ImplementationTclaudeLayer,
	}
	layerCodex := spawnLineageSandbox{
		Harness: harness.CodexName, HarnessBuiltinMode: harness.SandboxDangerFull,
		Implementation: sandboxpolicy.ImplementationTclaudeLayer,
	}
	launches := recordableLineageLaunches(t)

	claudeOn := spawnLineageSandbox{Harness: harness.DefaultName, HarnessBuiltinMode: harness.ClaudeSandboxOn}
	codexManaged := spawnLineageSandbox{Harness: harness.CodexName, HarnessBuiltinMode: harness.SandboxManagedProfile}
	for _, child := range launches {
		require.Equalf(t,
			spawnSandboxLineageAllowed(claudeOn, child.sandbox()),
			spawnSandboxLineageAllowed(layerClaude, child.sandbox()),
			"a tclaude-layer Claude parent must delegate exactly as claude/on for child %s", child)
		require.Equalf(t,
			spawnSandboxLineageAllowed(codexManaged, child.sandbox()),
			spawnSandboxLineageAllowed(layerCodex, child.sandbox()),
			"a tclaude-layer Codex parent must delegate exactly as the managed profile for child %s", child)
	}
}

// The refusal has to say WHY, or it reads as a guard bug: `claude sandbox "off"`
// is the loosest mode there is, so the pair alone explains nothing.
func TestSpawnSandboxLineageRefusalNamesImplementationDerivedAuthority(t *testing.T) {
	setupTestDB(t)
	const convID = "tclaude-layer-claude-parent"
	requireSaveLineageParent(t, convID, harness.DefaultName,
		harness.ClaudeSandboxOff, sandboxpolicy.ImplementationTclaudeLayer)

	fail := spawnSandboxLineageFailure(convID, harness.DefaultName,
		harness.ClaudeSandboxOff, string(sandboxpolicy.ImplementationOff))
	require.NotNil(t, fail, "a tclaude-walled parent may not mint a genuinely unconfined child")
	require.Equal(t, "sandbox_restricted", fail.Kind)
	require.Contains(t, fail.Msg, "authority is derived from its sandbox implementation")
	require.Contains(t, fail.Msg, string(sandboxpolicy.ImplementationTclaudeLayer))
	require.Contains(t, fail.Msg, harness.ClaudeSandboxOn,
		"the message must name the class the parent actually delegates as")

	// The note is withheld where it would be noise: an ordinary refusal whose
	// recorded mode already IS its authority.
	requireSaveLineageParent(t, "builtin-claude-parent", harness.DefaultName,
		harness.ClaudeSandboxOn, sandboxpolicy.ImplementationHarnessBuiltin)
	ordinary := spawnSandboxLineageFailure("builtin-claude-parent", harness.DefaultName,
		harness.ClaudeSandboxOff, string(sandboxpolicy.ImplementationHarnessBuiltin))
	require.NotNil(t, ordinary)
	require.NotContains(t, ordinary.Msg, "authority is derived")
}

// A tclaude-walled caller can no longer skip the directory write-proof: it
// cannot in fact write everywhere its child could, which is the whole premise
// of the exemption.
func TestDirWriteProofExemptionReadsParentImplementation(t *testing.T) {
	setupTestDB(t)
	for _, tc := range []struct {
		name        string
		harnessName string
		mode        string
		impl        sandboxpolicy.Implementation
		exempt      bool
	}{
		{"claude-off-builtin", harness.DefaultName, harness.ClaudeSandboxOff,
			sandboxpolicy.ImplementationHarnessBuiltin, true},
		{"claude-off-implementation-off", harness.DefaultName, harness.ClaudeSandboxOff,
			sandboxpolicy.ImplementationOff, true},
		{"claude-off-tclaude-layer", harness.DefaultName, harness.ClaudeSandboxOff,
			sandboxpolicy.ImplementationTclaudeLayer, false},
		{"codex-danger-builtin", harness.CodexName, harness.SandboxDangerFull,
			sandboxpolicy.ImplementationHarnessBuiltin, true},
		{"codex-danger-tclaude-layer", harness.CodexName, harness.SandboxDangerFull,
			sandboxpolicy.ImplementationTclaudeLayer, false},
		{"opencode-off-builtin", harness.OpenCodeName, harness.OpenCodeSandboxOff,
			sandboxpolicy.ImplementationHarnessBuiltin, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireSaveLineageParent(t, tc.name, tc.harnessName, tc.mode, tc.impl)
			exempt, err := dirWriteProofCallerExempt(tc.name)
			require.NoError(t, err)
			require.Equal(t, tc.exempt, exempt)
		})
	}
}

// requireSaveLineageParent writes the session row the guard reads a parent's
// posture from, spelling BOTH sandbox columns — the implementation column is
// the whole point of these cases, and the shared spawn helpers leave it blank.
func requireSaveLineageParent(
	t *testing.T, convID, harnessName, mode string,
	implementation sandboxpolicy.Implementation,
) {
	t.Helper()
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "sess-" + convID, ConvID: convID, Cwd: t.TempDir(),
		Harness: harnessName, SandboxMode: mode,
		SandboxImplementation: string(implementation),
	}))
}
