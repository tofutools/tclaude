package sandboxpolicy

// Spelling canonicalization (TCL-981), volume-adaptive like guard_path_test.go:
// each test asks the volume what it does and then asserts the answer that
// semantics requires, so neither a case-sensitive nor a case-insensitive run
// degenerates into a skip.

import (
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
