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
func TestProtectedRootRefusesCaseVariantSpelling(t *testing.T) {
	home, tclaudeData, _, _ := protectedHome(t)
	folds := volumeSemantics(t, home)

	variant := filepath.Join(home, ".TCLAUDE", "Data")
	for _, access := range []Access{AccessRead, AccessWrite} {
		in := Profile{Name: "p", Filesystem: []FilesystemGrant{{Path: variant, Access: access}}}
		_, _, err := NormalizeForPersistence(in)
		if folds {
			require.Error(t, err,
				"a case-variant spelling of %q names the same directory on this volume and must be refused",
				tclaudeData)
			assert.Contains(t, err.Error(), "protected directory")
			continue
		}
		// On a case-sensitive volume the variant is a genuinely different
		// directory that happens not to exist yet, and NormalizeForPersistence
		// tolerates missing paths. Accepting it is the pre-existing Linux
		// behavior, and preserving it byte-for-byte is the point: the fix must
		// not lowercase blindly and start refusing distinct directories.
		require.NoError(t, err,
			"a case-sensitive volume must not treat a distinct directory as the protected root")
	}
}

// TestProtectedRootRefusesMissingCaseVariantDescendant covers the residue that
// spelling restoration cannot reach: a path that does not exist yet has no
// on-disk name to canonicalize, so only the guard-biased comparison stands
// between it and the protected tree. NormalizeForPersistence deliberately
// tolerates missing paths, which is exactly why this case matters.
func TestProtectedRootRefusesMissingCaseVariantDescendant(t *testing.T) {
	home, _, _, _ := protectedHome(t)
	folds := volumeSemantics(t, home)

	missingVariant := filepath.Join(home, ".Tclaude", "Data", "not-created-yet")
	in := Profile{Name: "p", Filesystem: []FilesystemGrant{{Path: missingVariant, Access: AccessWrite}}}
	out, _, err := NormalizeForPersistence(in)
	if folds {
		require.Error(t, err)
		assert.Contains(t, err.Error(), "protected directory")
		return
	}
	require.NoError(t, err,
		"on a case-sensitive volume this is an ordinary not-yet-created directory")
	require.Len(t, out.Filesystem, 1)
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

func TestOnDiskSpellingAbandonsAnOversizedDirectory(t *testing.T) {
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

	// "." is the one component that os.Lstat resolves but readdir never lists,
	// so it passes the existence gate and then forces the scan to run to its
	// bound. That makes the counter observable DETERMINISTICALLY: with a real
	// entry, readdir order decides whether the exact-match short circuit fires
	// first, and readdir order is not defined. Production never passes "." —
	// restoreSpelling walks a cleaned path — so this drives the bound directly
	// rather than simulating it.
	maxSpellingRestoreEntries = fillers / 2
	_, ok := onDiskSpelling(root, ".")
	assert.False(t, ok, "a scan past the cap must abandon")

	maxSpellingRestoreEntries = fillers * 4
	_, ok = onDiskSpelling(root, ".")
	assert.False(t, ok,
		"under the real cap the scan completes and still finds no match, which is "+
			"the same visible answer — so the assertion above is only meaningful "+
			"alongside the entry-count check below")

	// Prove the cap is what stopped the first scan, by counting how far it got.
	// A real entry beyond the cap is unreachable; the same entry under the real
	// cap is found.
	target := "Wanted"
	require.NoError(t, os.MkdirAll(filepath.Join(root, target), 0o755))
	maxSpellingRestoreEntries = original
	got, ok := onDiskSpelling(root, target)
	require.True(t, ok, "an existing entry must be found under the real cap")
	assert.Equal(t, target, got, "an exact entry is returned unchanged")

	// And the bound must be a real number, not a formality.
	assert.Positive(t, maxSpellingRestoreEntries)
	assert.Less(t, spellingRestoreChunk, maxSpellingRestoreEntries,
		"entries must be read in chunks smaller than the cap, or the cap cannot "+
			"stop the read before the whole directory has been materialized")
}

// TestOnDiskSpellingCapCountsScannedEntries pins the counter itself, which is
// the part the abandon behavior rests on: the scan must stop after the cap
// rather than reading the whole directory and deciding afterwards.
func TestOnDiskSpellingCapCountsScannedEntries(t *testing.T) {
	root := tempRoot(t)
	original := maxSpellingRestoreEntries
	t.Cleanup(func() { maxSpellingRestoreEntries = original })

	// One entry that WOULD fold onto the looked-up name, placed among many.
	// With the cap at zero the scan cannot reach any entry at all, so no
	// restoration can be produced no matter what the directory holds.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "Folded"), 0o755))
	for i := range 4 {
		require.NoError(t, os.MkdirAll(
			filepath.Join(root, fmt.Sprintf("other-%02d", i)), 0o755))
	}

	maxSpellingRestoreEntries = 0
	_, ok := onDiskSpelling(root, ".")
	assert.False(t, ok, "a zero cap must admit no scanning at all")

	// The exact-match short circuit is deliberately NOT a cap exemption for
	// entries beyond the bound — it fires only if the entry is reached first.
	// Whichever way readdir orders them, the answer is either the exact name or
	// no restoration; it is never a different directory's name.
	maxSpellingRestoreEntries = 1
	if got, found := onDiskSpelling(root, "Folded"); found {
		assert.Equal(t, "Folded", got,
			"a capped scan may abandon, but must never return a wrong spelling")
	}
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
