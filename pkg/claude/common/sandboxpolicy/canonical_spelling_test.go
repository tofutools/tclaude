package sandboxpolicy

// Spelling canonicalization (TCL-981), volume-adaptive like guard_path_test.go:
// each test asks the volume what it does and then asserts the answer that
// semantics requires, so neither a case-sensitive nor a case-insensitive run
// degenerates into a skip.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/unicode/norm"
)

func TestCanonicalHostSpellingRestoresOnDiskCaseWhenTheVolumeFolds(t *testing.T) {
	root := tempRoot(t)
	folds := volumeSemantics(t, root)

	onDisk := filepath.Join(root, "TclaudeState", "Data")
	require.NoError(t, os.MkdirAll(onDisk, 0o755))
	variant := filepath.Join(root, "tclaudestate", "data")

	got := CanonicalHostSpelling(variant)
	if folds {
		assert.Equal(t, onDisk, got,
			"a folding volume must report the real on-disk spelling, so every later lexical comparison lines up")
		return
	}
	assert.Equal(t, variant, got,
		"a case-sensitive volume must be left byte-for-byte alone")
}

func TestCanonicalHostSpellingIsIdempotentAndPreservesExactSpelling(t *testing.T) {
	root := tempRoot(t)
	onDisk := filepath.Join(root, "Exact", "Spelling")
	require.NoError(t, os.MkdirAll(onDisk, 0o755))

	once := CanonicalHostSpelling(onDisk)
	assert.Equal(t, onDisk, once, "a path already spelled the way the filesystem stores it must not move")
	assert.Equal(t, once, CanonicalHostSpelling(once), "canonicalization must be idempotent")
}

func TestCanonicalHostSpellingReattachesUnresolvableRemainder(t *testing.T) {
	root := tempRoot(t)
	folds := volumeSemantics(t, root)
	existing := filepath.Join(root, "Present")
	require.NoError(t, os.MkdirAll(existing, 0o755))

	// The tail does not exist, so it has no on-disk name to read. It must come
	// back exactly as authored rather than being dropped or guessed at.
	authoredTail := filepath.Join("Not", "Created", "Yet")
	got := CanonicalHostSpelling(filepath.Join(root, "present", authoredTail))
	if folds {
		assert.Equal(t, filepath.Join(existing, authoredTail), got)
		return
	}
	assert.Equal(t, filepath.Join(root, "present", authoredTail), got)
}

func TestCanonicalHostSpellingLeavesUnreadableAndRelativePathsAlone(t *testing.T) {
	assert.Equal(t, "relative/path", CanonicalHostSpelling("relative/path"),
		"a non-absolute path is only cleaned; it names nothing to restore against")

	if os.Geteuid() == 0 {
		t.Skip("root traverses regardless, so an unreadable ancestor cannot be staged")
	}
	root := tempRoot(t)
	gate := filepath.Join(root, "gate")
	require.NoError(t, os.MkdirAll(filepath.Join(gate, "Inner"), 0o755))
	require.NoError(t, os.Chmod(gate, 0o000))
	t.Cleanup(func() { _ = os.Chmod(gate, 0o755) })

	authored := filepath.Join(gate, "inner", "leaf")
	assert.Equal(t, authored, CanonicalHostSpelling(authored),
		"an unreadable tree yields the authored spelling; the guard layer, not this one, decides containment")
}

func TestCanonicalHostSpellingRestoresNormalizationOnFoldingVolumes(t *testing.T) {
	root := tempRoot(t)
	nfc := norm.NFC.String("café")
	nfd := norm.NFD.String("café")
	require.NotEqual(t, nfc, nfd)

	onDisk := filepath.Join(root, nfc)
	require.NoError(t, os.MkdirAll(onDisk, 0o755))

	got := CanonicalHostSpelling(filepath.Join(root, nfd))
	if info, err := os.Lstat(filepath.Join(root, nfd)); err == nil {
		onDiskInfo, statErr := os.Lstat(onDisk)
		require.NoError(t, statErr)
		if os.SameFile(info, onDiskInfo) {
			// The volume reaches one directory through both forms, so the stored
			// form is the one that must come back.
			assert.Equal(t, onDisk, got)
			return
		}
	}
	// A normalization-sensitive volume keeps the two forms apart, and the
	// authored spelling must survive untouched.
	assert.Equal(t, filepath.Join(root, nfd), got)
}

// TestProtectedRootRefusesCaseVariantSpelling is the end-to-end statement of
// the bug TCL-981 exists to fix: on a case-insensitive volume, a filesystem
// rule that spells a protected root differently names the SAME directory, and
// the protected-root invariant must refuse it exactly as it refuses the exact
// spelling.
//
// The refusal is now unconditional — the same on every volume — and that is a
// deliberate, operator-approved trade. Deciding it per volume would mean
// answering "would this filesystem fold a spelling that does not exist yet",
// which has no reliable answer (fold semantics are per-directory, not per-
// volume) and which produced four separate fail-open defects while this change
// was in review. So an unrefutable case/NFC collision with a protected root is
// refused, full stop.
//
// The cost is precise and small: an operator who authors a path differing from
// a protected root ONLY by case or Unicode normalization, on a case-sensitive
// volume, where that path does not yet exist, now gets a refusal naming both
// paths instead of a silent accept. Creating the directory first, or spelling
// it the way it is spelled on disk, resolves it.
func TestProtectedRootRefusesCaseVariantSpelling(t *testing.T) {
	home, tclaudeData, _, _ := protectedHome(t)

	variant := filepath.Join(home, ".TCLAUDE", "Data")
	for _, access := range []Access{AccessRead, AccessWrite} {
		in := Profile{Name: "p", Filesystem: []FilesystemGrant{{Path: variant, Access: access}}}
		_, _, err := NormalizeForPersistence(in)
		require.Error(t, err,
			"a case-variant spelling of %q must be refused on every volume", tclaudeData)
		assert.Contains(t, err.Error(), "protected directory")
	}
}

// TestProtectedRootRefusesMissingCaseVariantDescendant covers the residue that
// spelling restoration cannot reach: a path that does not exist yet has no
// on-disk name to canonicalize, so only the guard stands between it and the
// protected tree. NormalizeForPersistence deliberately tolerates missing paths,
// which is exactly why this case matters.
func TestProtectedRootRefusesMissingCaseVariantDescendant(t *testing.T) {
	home, _, _, _ := protectedHome(t)

	missingVariant := filepath.Join(home, ".Tclaude", "Data", "not-created-yet")
	in := Profile{Name: "p", Filesystem: []FilesystemGrant{{Path: missingVariant, Access: AccessWrite}}}
	_, _, err := NormalizeForPersistence(in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "protected directory")
}

// TestOrdinaryMissingGrantsAreStillAccepted is the other half of that trade,
// and the reason it is narrow: only a path that case/NFC-folds ONTO a protected
// root is refused. An ordinary not-yet-created grant — including one that
// merely shares a prefix, or differs by more than spelling — is accepted
// exactly as before, with no filesystem interrogation at all.
func TestOrdinaryMissingGrantsAreStillAccepted(t *testing.T) {
	home, _, _, _ := protectedHome(t)

	for _, path := range []string{
		filepath.Join(home, "projects", "not-created-yet"),
		filepath.Join(home, ".tclaude-notes"),           // shares a prefix, folds onto nothing
		filepath.Join(home, ".tclaudedata"),             // no separator, so no containment
		filepath.Join(home, "Projects", "Mixed", "Case"),
	} {
		in := Profile{Name: "p", Filesystem: []FilesystemGrant{{Path: path, Access: AccessWrite}}}
		out, _, err := NormalizeForPersistence(in)
		require.NoError(t, err, "ordinary missing grant %q must still be accepted", path)
		require.Len(t, out.Filesystem, 1)
	}
}

// TestCaseVariantGrantsFoldIntoOneRuleWithDenyDominance is the grant-lattice
// half of the bug. Two rules naming one physical directory through different
// spellings used to persist as two independent rules, so deny-dominance never
// collapsed them and a write survived alongside the deny that was supposed to
// beat it.
func TestCaseVariantGrantsFoldIntoOneRuleWithDenyDominance(t *testing.T) {
	home, _, _, _ := protectedHome(t)
	folds := volumeSemantics(t, home)

	project := filepath.Join(home, "Project")
	require.NoError(t, os.MkdirAll(project, 0o755))

	in := Profile{Name: "p", Filesystem: []FilesystemGrant{
		{Path: project, Access: AccessWrite},
		{Path: filepath.Join(home, "project"), Access: AccessDeny},
	}}
	out, _, err := NormalizeForPersistence(in)
	require.NoError(t, err)

	if !folds {
		assert.Len(t, out.Filesystem, 2,
			"a case-sensitive volume has two directories here, so two rules is correct")
		return
	}
	require.Len(t, out.Filesystem, 1,
		"one physical directory must persist as one rule regardless of how each grant spelled it")
	assert.Equal(t, project, out.Filesystem[0].Path,
		"the folded rule must carry the real on-disk spelling")
	assert.Equal(t, AccessDeny, out.Filesystem[0].Access,
		"deny must dominate the write it now actually collides with")
}

// The next two tests call onDiskSpelling directly, which is the only way to
// reach it on a case-sensitive runner: CanonicalHostSpelling returns after the
// volume probe there and never descends. Both branches they cover decide
// whether a PERSISTED grant path gets rewritten, so leaving them untested on
// every Linux CI runner — and untestable on a folding one, which cannot hold two
// folded-equal siblings at all — would be the wrong place to have no coverage.

func TestOnDiskSpellingRefusesToGuessBetweenAmbiguousSiblings(t *testing.T) {
	root := tempRoot(t)
	if volumeSemantics(t, root) {
		// A folding volume cannot stage the ambiguity; assert the property that
		// makes it unstageable, so this branch is not vacuous either.
		require.NoError(t, os.MkdirAll(filepath.Join(root, "Foo"), 0o755))
		err := os.Mkdir(filepath.Join(root, "FOO"), 0o755)
		assert.Error(t, err, "a folding volume must reject a second folded-equal sibling")
		return
	}
	// Three spellings that all fold together. A restoration that picked one
	// would rewrite a persisted grant to name a DIFFERENT directory than the
	// operator authored — strictly worse than not restoring at all.
	for _, name := range []string{"FOO", "FoO", "foo"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, name), 0o755))
	}
	_, ok := onDiskSpelling(root, "fOO")
	assert.False(t, ok, "an ambiguous fold must refuse to guess")

	// An exact byte match is unambiguous even when folded siblings exist, and
	// must be returned as-is rather than being caught by the ambiguity check.
	got, ok := onDiskSpelling(root, "FoO")
	require.True(t, ok)
	assert.Equal(t, "FoO", got)
}

// TestScanForSpellingAbandonsAtTheEntryCap pins that the cap bounds the WORK,
// not merely the decision. That distinction is the whole point of reading the
// directory in chunks instead of calling os.ReadDir, and it is invisible from
// onDiskSpelling's (name, ok) result: "abandoned at the cap" and "read
// everything and found nothing" are both ("", false). An earlier version of
// this test asserted only on that pair and stayed green with the cap
// enforcement deleted, so it asserts on spellingScan's counters instead.
//
// Do NOT add t.Parallel() here or to any test that assigns to
// maxSpellingRestoreEntries — the cap is package state.
func TestScanForSpellingAbandonsAtTheEntryCap(t *testing.T) {
	root := tempRoot(t)
	// Lower the cap rather than staging 50k entries: abandoning past the cap is
	// the same code path at any cap value, and staging the real number would cost
	// seconds while proving no more.
	original := maxSpellingRestoreEntries
	t.Cleanup(func() { maxSpellingRestoreEntries = original })

	const fillers = 8
	for i := range fillers {
		require.NoError(t, os.MkdirAll(
			filepath.Join(root, fmt.Sprintf("filler-%02d", i)), 0o755))
	}

	// "." is the one component os.Lstat resolves but readdir never lists, so it
	// passes the existence gate and then forces the scan to run to its bound
	// without any exact-match short circuit racing readdir order. Production
	// never passes "." — restoreSpelling walks a cleaned path — so this drives
	// the bound directly rather than simulating it.
	const entryCap = fillers / 2
	maxSpellingRestoreEntries = entryCap
	capped := scanForSpelling(root, ".")
	assert.False(t, capped.ok, "a scan past the cap must restore nothing")
	assert.True(t, capped.abandoned, "the scan must report that the cap stopped it")
	assert.Equal(t, entryCap+1, capped.scanned,
		"the scan must stop at the entry that exceeds the cap, leaving the rest of "+
			"the directory unread — a cap that merely filtered the RESULT would "+
			"have scanned all %d entries", fillers)

	// The same directory under a cap it fits inside is read to the end and
	// reports the opposite: not abandoned, and every entry examined. Without this
	// half, `abandoned` could be hardcoded true.
	maxSpellingRestoreEntries = fillers * 4
	complete := scanForSpelling(root, ".")
	assert.False(t, complete.ok, "there is still no entry named \".\" to restore")
	assert.False(t, complete.abandoned, "a directory inside the cap is not abandoned")
	assert.Equal(t, fillers, complete.scanned, "every entry must have been examined")

	// And the production bound must be a real number, not a formality: if the
	// chunk size were >= the cap, the first read would already have materialized
	// more entries than the cap allows and the bound would be decorative.
	assert.Positive(t, original)
	assert.Less(t, spellingRestoreChunk, original,
		"entries must be read in chunks smaller than the cap, or the cap cannot "+
			"stop the read before the whole directory has been materialized")
}

// TestScanForSpellingNeverReturnsAWrongSpellingUnderTheCap is the safety half:
// abandoning is allowed to lose a restoration, but must never invent one. The
// exact-match short circuit is deliberately NOT a cap exemption, so whichever
// way readdir orders the entries the answer is either the exact name or no
// restoration — never a different directory's name.
//
// Do NOT add t.Parallel() here (see above).
func TestScanForSpellingNeverReturnsAWrongSpellingUnderTheCap(t *testing.T) {
	root := tempRoot(t)
	original := maxSpellingRestoreEntries
	t.Cleanup(func() { maxSpellingRestoreEntries = original })

	require.NoError(t, os.MkdirAll(filepath.Join(root, "Folded"), 0o755))
	for i := range 4 {
		require.NoError(t, os.MkdirAll(
			filepath.Join(root, fmt.Sprintf("other-%02d", i)), 0o755))
	}

	for _, capValue := range []int{0, 1, 2, 3} {
		maxSpellingRestoreEntries = capValue
		result := scanForSpelling(root, "Folded")
		if result.ok {
			assert.Equal(t, "Folded", result.name,
				"a capped scan may abandon, but must never return a wrong spelling")
		}
		assert.LessOrEqual(t, result.scanned, capValue+1,
			"the scan must not examine entries beyond the cap")
	}

	// A zero cap admits no scanning at all, so nothing can be restored no matter
	// what the directory holds.
	maxSpellingRestoreEntries = 0
	zero := scanForSpelling(root, ".")
	assert.False(t, zero.ok, "a zero cap must admit no scanning at all")
	assert.True(t, zero.abandoned)
}

// TestRestoreSpellingReattachesFromTheFirstUnresolvableComponent pins the
// degrade path directly, since it is likewise unreachable through
// CanonicalHostSpelling on a case-sensitive volume.
func TestRestoreSpellingReattachesFromTheFirstUnresolvableComponent(t *testing.T) {
	root := tempRoot(t)
	existing := filepath.Join(root, "Present")
	require.NoError(t, os.MkdirAll(existing, 0o755))

	// Everything below "Present" is missing, so restoration must stop there and
	// re-attach the authored remainder verbatim — never drop it, never guess it.
	authored := filepath.Join(existing, "Missing", "Deeper")
	assert.Equal(t, authored, restoreSpelling(authored))

	// A fully existing path comes back as the on-disk spelling it already is.
	assert.Equal(t, existing, restoreSpelling(existing))
}
