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
	// The filesystem is read FIRST, and non-fatally, so that removing the anchor
	// reports the observed pre-fix value — the victim directory exists — rather
	// than aborting on the return code and leaving that unstated.
	assert.NoDirExists(t, victim,
		"the refusal has to happen BEFORE the mkdir, not merely be reported after it")
	require.ErrorContains(t, err,
		"is neither an allocated per-agent state root nor this host's OpenCode state root",
		"pinned to the reason: an unrelated refusal here would keep this test green while the anchor quietly stopped firing")
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
	assert.NoDirExists(t, victim)
	require.ErrorContains(t, err,
		"is neither below its state root",
		"pinned to the reason, not to the presence of any error")
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
	assert.NoDirExists(t, victim)
	require.ErrorContains(t, err, "has no durable state allocation")
	require.ErrorContains(t, err, "is not an allocated per-agent state root")
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
	accepted := session.TclaudeLayerLaunchContract{
		HarnessName: harness.OpenCodeName,
		StateRoot:   filepath.Join(real, ".opencode"),
		StateDirs: []string{
			filepath.Join(real, ".local", "share", "opencode"),
			filepath.Join(real, ".cache", "opencode"),
			filepath.Join(real, ".config", "opencode"),
			filepath.Join(real, ".local", "state", "opencode"),
		},
	}
	require.NoError(t, requireOpenCodeAnchoredStateTargets(accepted))

	// NEGATIVE CONTROL. Acceptance alone is satisfied by an anchor that accepts
	// everything — this test passed unchanged when a reviewer stubbed the legacy
	// arm out. A non-matching root under the SAME symlinked home must still be
	// refused, which is what makes the acceptance above evidence that the two
	// spellings were compared and matched rather than never compared.
	refused := accepted
	refused.StateRoot = filepath.Join(real, ".opencode-impostor")
	require.ErrorContains(t, requireOpenCodeAnchoredStateTargets(refused),
		"is neither an allocated per-agent state root nor this host's OpenCode state root")
}

// The filtered posture, driven through the production layout builder and its
// filtered isolation step. #1832's first round asserted this posture was
// unaffected without exercising it; a claim about a posture is worth what the
// test that reaches it is worth.
func TestPrepareOpenCodeTclaudeLayerStateAcceptsProducedFilteredLayout(t *testing.T) {
	stateRoot, _ := allocatedOpenCodeConfigDir(t)
	layout, err := openCodeStateLayoutForAllocation(db.OpenCodeAgentStateAllocation{
		AgentID:   filepath.Base(stateRoot),
		Mode:      db.OpenCodeStatePrivate,
		StateRoot: stateRoot,
	})
	require.NoError(t, err)
	require.NoError(t, isolateOpenCodeFilteredConfig(layout))
	require.Len(t, layout.stateDirs, 4)

	contract := session.TclaudeLayerLaunchContract{
		HarnessName:   harness.OpenCodeName,
		StateRoot:     stateRoot,
		StateDirs:     layout.stateDirs,
		WriteDirs:     layout.stateDirs,
		ReadOnlyBinds: layout.readOnlyBinds,
	}
	require.NoError(t, requireOpenCodeAnchoredStateTargets(contract))

	// The filtered posture's directories are all below the state root, so this
	// acceptance must not be riding on the ambient arm — the anchor must never
	// derive the ambient set at all.
	//
	// Pinned by making that derivation IMPOSSIBLE rather than merely relocated.
	// An earlier version moved the ambient bases to fresh temp paths and
	// asserted acceptance; a cold-review mutation restoring eager derivation
	// left it green, because relocating a base does not make
	// ambientOpenCodeStateAppDirs fail — openCodeXDGBase falls back to $HOME and
	// canonicalizeMissingOpenCodePath happily walks up to an existing ancestor.
	// The assertion could not fail, so it pinned nothing.
	//
	// Pointing XDG_CACHE_HOME through a REGULAR FILE is what breaks it:
	// canonicalizeMissingOpenCodePath returns any non-ENOENT Lstat error, and a
	// file used as a directory component gives ENOTDIR. Lazy derivation never
	// reaches it and accepts; eager derivation returns that error and this test
	// fails.
	//
	// Two things are deliberately NOT touched, and both exclusions are the point
	// rather than conveniences, because each would fail this test for a reason
	// that has nothing to do with laziness:
	//
	//   - $HOME. Clearing it looks like a cleaner way to break the ambient set,
	//     but refuseOpenCodeProtectedStateRoot resolves the home directory too,
	//     so the anchor fails before it ever reaches the ambient arm. Tried
	//     first, and it failed with "$HOME is not defined" from the protected
	//     paths — a green-to-red move that would have proved nothing.
	//   - XDG_DATA_HOME. It anchors the private state ALLOCATION, not the
	//     ambient answer, so breaking it strands the allocation and refuses with
	//     the environment-change message #1822 documented.
	notADirectory := filepath.Join(t.TempDir(), "regular-file")
	require.NoError(t, os.WriteFile(notADirectory, []byte("x"), 0o600))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(notADirectory, "under-a-file"))
	require.Error(t, func() error {
		_, err := ambientOpenCodeStateAppDirs()
		return err
	}(),
		"the fixture only discriminates if deriving the ambient set is now impossible")
	require.NoError(t, requireOpenCodeAnchoredStateTargets(contract),
		"a private posture names no ambient directory, so it must not derive one")
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

// F1 from #1832's cold review, CONFIRMED before it was fixed: ReadOnlyStateDirs
// is a THIRD MkdirAll sink inside PrepareTclaudeLayerHarnessState, and the only
// containment it applies is LEXICAL against the unresolved state root. A
// symlink planted inside a genuinely allocated root — by the sandboxed agent
// that owns that directory — therefore satisfies the lexical test while
// resolving outside it.
//
// The state root here is really allocated, so the anchor's own arms all pass
// cleanly. This is specifically about the sink the first version of the anchor
// did not cover.
func TestPrepareOpenCodeTclaudeLayerStateRefusesReadOnlyStateDirEscape(t *testing.T) {
	stateRoot, _ := allocatedOpenCodeConfigDir(t)
	victimParent := filepath.Join(t.TempDir(), "victim-parent")
	require.NoError(t, os.MkdirAll(victimParent, 0o700))
	// The escape hatch: an in-root name that resolves out of the root.
	require.NoError(t, os.Symlink(victimParent, filepath.Join(stateRoot, "escape")))
	escaped := filepath.Join(stateRoot, "escape", "created-by-daemon")
	victim := filepath.Join(victimParent, "created-by-daemon")
	require.NoDirExists(t, victim)

	stateDir := filepath.Join(stateRoot, "data", "opencode")
	require.NoError(t, os.MkdirAll(stateDir, 0o700))
	spec := &session.TclaudeLayerLaunchSpec{
		Version: session.TclaudeLayerLaunchSpecVersion,
		Contract: session.TclaudeLayerLaunchContract{
			HarnessName:       harness.OpenCodeName,
			StateRoot:         stateRoot,
			StateDirs:         []string{stateDir},
			WriteDirs:         []string{stateDir},
			ReadOnlyStateDirs: []string{escaped},
			ReadOnlyBinds: []session.TclaudeLayerReadOnlyBind{{
				Source: stateDir, Target: stateDir,
			}},
			// Contract-supplied, so it proves nothing — which is the point: the
			// read-only sink's other gate reads this same persisted artifact.
			ProfileFilesystem: nil,
		},
		Effective: sandboxpolicy.EffectiveProfile{
			Filesystem: []sandboxpolicy.FilesystemGrant{
				{Path: escaped, Access: sandboxpolicy.AccessRead},
			},
		},
	}

	err := prepareOpenCodeTclaudeLayerState(spec)
	assert.NoDirExists(t, victim,
		"a daemon-side mkdir escaped the state root through an in-root symlink")
	require.ErrorContains(t, err,
		"is not below its state root",
		"pinned to the reason, not to the presence of any error")
	// The retired spelling kept as a NEGATIVE needle. The read-only arm never
	// consults the ambient set, so offering it as a criterion would name a test
	// this refusal does not apply, and would point an operator at the
	// environment change the migration note assigns that exact sentence to.
	require.NotContains(t, err.Error(),
		"nor one of this host's ambient OpenCode state directories",
		"the read-only arm must not offer a criterion it never applies")
	// The containment test ran on the RESOLVED path, so the refusal has to show
	// it. Without this the sentence contradicts itself in the only case that
	// reaches this arm: the quoted directory literally begins with the quoted
	// state root, because the escape is a symlink the message never mentions.
	require.Contains(t, err.Error(), "(tested as ",
		"a refusal decided on a transformed path must not quote only the contract spelling")
	require.Contains(t, err.Error(), victim,
		"the path shown must be the one the containment check actually ran on")
}

// The refusal subject must not attribute the symlink to the LEAF. The
// inequality that triggers the parenthetical is produced by a symlink in any
// component, and on the realistic trigger — a symlinked HOME or XDG_DATA_HOME,
// the host shape #1822 documented — the leaf is written literally. Telling an
// operator their state root is a symlink then sends them looking for something
// that is not there.
func TestOpenCodeStateRootSubjectDoesNotBlameTheLeaf(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "real-parent")
	require.NoError(t, os.MkdirAll(parent, 0o700))
	link := filepath.Join(t.TempDir(), "link-parent")
	require.NoError(t, os.Symlink(parent, link))

	const leaf = "agt_00000000000000000000000000000042"
	stateRoot := filepath.Join(link, leaf)
	require.NoError(t, os.Mkdir(stateRoot, 0o700))
	resolved, err := filepath.EvalSymlinks(stateRoot)
	require.NoError(t, err)
	require.NotEqual(t, stateRoot, resolved,
		"the fixture only discriminates if resolution actually changes the path")

	// The contract's own spelling carries a ".." so the lexical step is visible
	// too: the subject must quote what the CONTRACT said and append what the
	// check ran on, not silently substitute one for the other.
	spelled := filepath.Join(link, "extra") + "/../" + leaf
	subject := openCodeStateRootSubject(spelled, stateRoot, resolved)
	assert.Contains(t, subject, spelled,
		"the contract's own words must appear; quoting the normalized form as its own is what misled")
	assert.Contains(t, subject, resolved,
		"the value the check ran on still has to be shown; hiding it is what #1822 fixed")
	// Two retired spellings as negative needles. Both named a MECHANISM the
	// code does not perform: the leaf is not the symlink, and Clean is not
	// resolution.
	assert.NotContains(t, subject, "a symlink",
		"the leaf here is a literal directory; only a component above it is a link")
	assert.NotContains(t, subject, "resolving to",
		"Clean collapses \"..\" lexically before anything resolves; naming resolution invites kernel semantics")
}

// A purely LEXICAL escape — ".." with no symlink anywhere — must also show the
// path that was tested. This is the case the first version of the subject block
// missed: it triggered on "resolved != clean", which is false here because the
// only transformation was Clean, so the refusal quoted a spelling beginning
// with the state root and said it was not below it, explaining nothing.
//
// Built by string concatenation, NOT filepath.Join: Join cleans, which would
// collapse the ".." before the contract ever carried it, and the test would
// then pass while exercising nothing.
func TestOpenCodeAnchoredStateTargetsShowsTheTestedPathForLexicalEscapes(t *testing.T) {
	stateRoot, _ := allocatedOpenCodeConfigDir(t)
	spelled := stateRoot + "/data/../../../../../../etc"

	err := requireOpenCodeAnchoredStateTargets(session.TclaudeLayerLaunchContract{
		HarnessName: harness.OpenCodeName,
		StateRoot:   stateRoot,
		StateDirs:   []string{spelled},
	})
	require.ErrorContains(t, err, "is neither below its state root")
	require.Contains(t, err.Error(), "(tested as ",
		"a lexical escape is transformed too, and the refusal must say what it tested")
	require.NotContains(t, err.Error(), "resolving to",
		"Clean is not symlink resolution; naming it that invites kernel semantics")
}

// The LEGACY arm's refusal, which four rounds of per-delta review never reached
// because no delta touched it. It compares resolved against resolvedLegacy and
// used to print the UNRESOLVED left side against the RESOLVED right side, so on
// a symlinked host the two operands sat in different namespaces with nothing
// saying why. The private arm avoided that via openCodeStateRootSubject; this
// one did not.
func TestOpenCodeAnchoredStateRootLegacyArmShowsBothOperandsAsCompared(t *testing.T) {
	home := isolatedOpenCodeHost(t)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".opencode"), 0o700))

	// Not agent-id shaped, so it takes the legacy arm; reached through a
	// symlinked component so the spelling and the compared value differ.
	real := filepath.Join(home, "real-parent")
	require.NoError(t, os.MkdirAll(filepath.Join(real, "not-an-agent"), 0o700))
	link := filepath.Join(home, "link-parent")
	require.NoError(t, os.Symlink(real, link))
	// String concatenation, NOT filepath.Join, and with a ".." so the spelling
	// and the cleaned form actually DIFFER. Built with Join first, this test
	// passed under a mutation that dropped the contract's spelling entirely —
	// because with nothing to clean the two values were the same string, so
	// substituting one for the other changed nothing. The hazard has to survive
	// to the code under test.
	spelled := link + "/extra/../not-an-agent"
	cleaned := canonicalOpenCodeRuntimePath(spelled)
	require.NotEqual(t, spelled, cleaned,
		"the fixture only discriminates if the contract's spelling differs from the cleaned form")

	_, err := requireOpenCodeAnchoredStateRoot(spelled, cleaned)
	require.ErrorContains(t, err, "is neither an allocated per-agent state root")
	require.Contains(t, err.Error(), spelled,
		"the contract's own spelling must appear")
	require.Contains(t, err.Error(), "(tested as ",
		"the operand the comparison actually ran on must be shown, not silently substituted")
}
