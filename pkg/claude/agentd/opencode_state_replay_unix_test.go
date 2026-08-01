//go:build linux || darwin

package agentd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// TCL-907. session.PrepareTclaudeLayerHarnessState MkdirAll's the contract's
// StateRoot with NO WriteDirs requirement at all, so a tampered persisted spec
// could aim that mkdir at any directory outside the protected roots.
//
// The victim here is deliberately NOT in WriteDirs, which is what makes it the
// state-root exemption rather than the general case: every other entry has to
// clear that membership check, and this one does not.
//
// Pre-fix, the whole spec is accepted and the victim is created on disk — see
// the mutation control in the PR body, which asserts the observed filesystem
// state rather than a return code.
func TestPrepareOpenCodeTclaudeLayerStateRefusesTamperedStateRoot(t *testing.T) {
	stateRoot, _ := allocatedOpenCodeConfigDir(t)
	victim := filepath.Join(t.TempDir(), "victim-state-root")
	require.NoDirExists(t, victim, "the fixture must not pre-create what the mkdir would create")
	legitimateDir := filepath.Join(stateRoot, "data", "opencode")
	require.NoError(t, os.MkdirAll(legitimateDir, 0o700))

	spec := &session.TclaudeLayerLaunchSpec{
		Version: session.TclaudeLayerLaunchSpecVersion,
		Contract: session.TclaudeLayerLaunchContract{
			HarnessName: harness.OpenCodeName,
			StateRoot:   victim,
			StateDirs:   []string{legitimateDir},
			WriteDirs:   []string{legitimateDir},
			ReadOnlyBinds: []session.TclaudeLayerReadOnlyBind{{
				Source: legitimateDir, Target: legitimateDir,
			}},
		},
	}

	err := prepareOpenCodeTclaudeLayerState(spec)
	require.ErrorContains(t, err,
		"is neither an allocated per-agent state root nor this host's OpenCode state root",
		"pinned to the reason: an unrelated refusal here would keep this test green while the anchor quietly stopped firing")
	assert.NoDirExists(t, victim,
		"the refusal has to happen BEFORE the mkdir, not merely be reported after it")
}

// The same class one level in: a state root this daemon really allocated, whose
// StateDirs then name a directory that is neither below it nor one of this
// host's ambient OpenCode directories. Fixing only the state root would leave
// every StateDirs entry standing on contract-supplied WriteDirs.
func TestPrepareOpenCodeTclaudeLayerStateRefusesTamperedStateDir(t *testing.T) {
	stateRoot, _ := allocatedOpenCodeConfigDir(t)
	victim := filepath.Join(t.TempDir(), "victim-state-dir")
	require.NoDirExists(t, victim)

	spec := &session.TclaudeLayerLaunchSpec{
		Version: session.TclaudeLayerLaunchSpecVersion,
		Contract: session.TclaudeLayerLaunchContract{
			HarnessName: harness.OpenCodeName,
			StateRoot:   stateRoot,
			StateDirs:   []string{victim},
			// Present in the contract's own writable set, which is exactly the
			// guard TCL-907 says proves nothing: it comes from the same
			// persisted artifact being validated.
			WriteDirs: []string{victim},
			ReadOnlyBinds: []session.TclaudeLayerReadOnlyBind{{
				Source: stateRoot, Target: stateRoot,
			}},
		},
	}

	err := prepareOpenCodeTclaudeLayerState(spec)
	require.ErrorContains(t, err,
		"is neither below its state root",
		"pinned to the reason, not to the presence of any error")
	assert.NoDirExists(t, victim)
}

// A per-agent-shaped state root whose agent id has no allocation is refused by
// the allocation authority, in the same words the config seed uses — the point
// of sharing one predicate rather than deriving a second notion of a valid
// per-agent root.
func TestPrepareOpenCodeTclaudeLayerStateRefusesUnallocatedAgentRoot(t *testing.T) {
	_, _ = allocatedOpenCodeConfigDir(t)
	const unallocatedID = "agt_00000000000000000000000000000042"
	victim := filepath.Join(t.TempDir(), unallocatedID)
	require.NoDirExists(t, victim)
	sibling := t.TempDir()

	spec := &session.TclaudeLayerLaunchSpec{
		Version: session.TclaudeLayerLaunchSpecVersion,
		Contract: session.TclaudeLayerLaunchContract{
			HarnessName: harness.OpenCodeName,
			StateRoot:   victim,
			StateDirs:   []string{sibling},
			WriteDirs:   []string{sibling},
			ReadOnlyBinds: []session.TclaudeLayerReadOnlyBind{{
				Source: sibling, Target: sibling,
			}},
		},
	}

	err := prepareOpenCodeTclaudeLayerState(spec)
	require.ErrorContains(t, err, "has no durable state allocation")
	require.ErrorContains(t, err, "is not an allocated per-agent state root")
	assert.NoDirExists(t, victim)
}

// The private posture, driven through the PRODUCTION layout builder rather than
// a hand-authored contract. Without this, a predicate that refused every real
// launch would still pass every refusal test above.
func TestPrepareOpenCodeTclaudeLayerStateAcceptsProducedPrivateLayout(t *testing.T) {
	stateRoot, _ := allocatedOpenCodeConfigDir(t)
	layout, err := openCodeStateLayoutForAllocation(db.OpenCodeAgentStateAllocation{
		AgentID:   filepath.Base(stateRoot),
		Mode:      db.OpenCodeStatePrivate,
		StateRoot: stateRoot,
	})
	require.NoError(t, err)
	require.Len(t, layout.stateDirs, 4)

	spec := &session.TclaudeLayerLaunchSpec{
		Version: session.TclaudeLayerLaunchSpecVersion,
		Contract: session.TclaudeLayerLaunchContract{
			HarnessName:   harness.OpenCodeName,
			StateRoot:     stateRoot,
			StateDirs:     layout.stateDirs,
			WriteDirs:     layout.stateDirs,
			ReadOnlyBinds: layout.readOnlyBinds,
		},
	}

	require.NoError(t, prepareOpenCodeTclaudeLayerState(spec))
	for _, path := range append([]string{stateRoot}, layout.stateDirs...) {
		info, statErr := os.Stat(path)
		require.NoError(t, statErr, "a legitimate launch must still materialize its state")
		assert.True(t, info.IsDir())
	}
}

// The legacy-shared posture, built by the PRODUCTION spec builder. This is the
// answer to "what happens to a real legacy spec": nothing changes. Its state
// root IS this host's ~/.opencode and its state directories ARE the four
// ambient OpenCode XDG app directories, both of which the anchor re-derives for
// itself, so it is accepted on host authority without consulting the allocation
// store at all.
func TestPrepareOpenCodeTclaudeLayerStateAcceptsProducedLegacySharedLayout(t *testing.T) {
	home := isolatedOpenCodeHost(t)
	cwd := filepath.Join(home, "work")
	require.NoError(t, os.MkdirAll(cwd, 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".opencode", "bin"), 0o700))
	snapshot := sandboxpolicy.EmptySnapshot()

	spec, err := session.BuildTclaudeLayerLaunchSpec(session.TclaudeLayerLaunchInput{
		HarnessName:  harness.OpenCodeName,
		Cwd:          cwd,
		GitWriteDirs: []string{cwd},
		Snapshot:     &snapshot,
	})
	require.NoError(t, err)
	// Asserted rather than assumed: if the builder stopped producing this shape,
	// the acceptance below would still pass while proving nothing about legacy.
	require.Equal(t, filepath.Join(home, ".opencode"), spec.Contract.StateRoot)
	require.Equal(t, []string{
		filepath.Join(home, "data", "opencode"),
		filepath.Join(home, "cache", "opencode"),
		filepath.Join(home, "config", "opencode"),
		filepath.Join(home, "state", "opencode"),
	}, spec.Contract.StateDirs)

	require.NoError(t, prepareOpenCodeTclaudeLayerState(&spec))
	for _, path := range append([]string{spec.Contract.StateRoot}, spec.Contract.StateDirs...) {
		info, statErr := os.Stat(path)
		require.NoError(t, statErr)
		assert.True(t, info.IsDir())
	}
}

// The Darwin private posture replaces StateDirs[2] with the resolved AMBIENT
// config app directory when one exists, so a rule of "below the state root"
// alone would refuse a launch the layout itself produced. Exercised from either
// host by calling the predicate with that shape directly, because a
// darwin-only test would leave the accepting arm unverified in Linux CI —
// which is how the sibling gap in TCL-892 survived.
func TestOpenCodeAnchoredStateTargetsAcceptsAmbientConfigStateDir(t *testing.T) {
	stateRoot, _ := allocatedOpenCodeConfigDir(t)
	ambientConfig := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "opencode")
	require.NoError(t, os.MkdirAll(ambientConfig, 0o700))

	contract := session.TclaudeLayerLaunchContract{
		HarnessName: harness.OpenCodeName,
		StateRoot:   stateRoot,
		StateDirs: []string{
			filepath.Join(stateRoot, "data", "opencode"),
			filepath.Join(stateRoot, "cache", "opencode"),
			ambientConfig,
			filepath.Join(stateRoot, "state", "opencode"),
		},
	}
	require.NoError(t, requireOpenCodeAnchoredStateTargets(contract))

	// ...and the arm is narrow: a sibling of the ambient directory, which is
	// neither below the state root nor one of the four this host derives, is
	// still refused. Without this the acceptance above would be indistinguishable
	// from "anything outside the state root is allowed".
	contract.StateDirs[2] = filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "opencode-sibling")
	require.ErrorContains(t, requireOpenCodeAnchoredStateTargets(contract),
		"nor one of this host's ambient OpenCode state directories")
}

// The anchor compares paths for IDENTITY, and a path has more than one true
// spelling. A host that reaches its home through a symlink must not have its
// own legacy state root refused. Reproducible on Linux; the class first
// appeared as a macOS-only failure in #1822.
func TestOpenCodeAnchoredStateTargetsSurvivesSymlinkedHome(t *testing.T) {
	real := resolvedTestPath(t, t.TempDir())
	link := filepath.Join(t.TempDir(), "home-link")
	require.NoError(t, os.Symlink(real, link))
	t.Setenv("HOME", link)
	for _, name := range []string{
		"XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME",
	} {
		t.Setenv(name, "")
	}
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)

	// The contract carries the RESOLVED spelling, as the production builder
	// produces it; the anchor derives the unresolved one from HOME.
	require.NoError(t, requireOpenCodeAnchoredStateTargets(
		session.TclaudeLayerLaunchContract{
			HarnessName: harness.OpenCodeName,
			StateRoot:   filepath.Join(real, ".opencode"),
			StateDirs: []string{
				filepath.Join(real, ".local", "share", "opencode"),
				filepath.Join(real, ".cache", "opencode"),
				filepath.Join(real, ".config", "opencode"),
				filepath.Join(real, ".local", "state", "opencode"),
			},
		}))
}

// Non-OpenCode launch specs are untouched: the authority this anchor uses is
// OpenCode's allocation store, and there is none for the other harnesses.
// Stated as a test so the limit is on the record rather than implied by where
// the code happens to live.
func TestOpenCodeAnchoredStateTargetsIgnoresOtherHarnesses(t *testing.T) {
	isolatedOpenCodeHost(t)
	require.NoError(t, requireOpenCodeAnchoredStateTargets(
		session.TclaudeLayerLaunchContract{
			HarnessName: harness.DefaultName,
			StateRoot:   filepath.Join(t.TempDir(), "anything"),
			StateDirs:   []string{filepath.Join(t.TempDir(), "anything-else")},
		}))
}
