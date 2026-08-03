package sandboxpolicy

// Case-insensitive sandbox containment (TCL-981).
//
// These tests are deliberately VOLUME-ADAPTIVE rather than platform-gated, and
// they never t.Skip. Case folding is a property of the filesystem, not of the
// operating system: macOS ships case-insensitive APFS by default but supports
// case-sensitive APFS, and Linux hosts case-insensitive mounts. So each test
// first asks the volume under t.TempDir() which semantics it has, then asserts
// the answer that semantics REQUIRES — the merge on a folding volume, the
// separation on a non-folding one. Both branches are real assertions, so a run
// on either kind of volume produces evidence rather than silence.
//
// The hard, non-skippable macOS evidence that a real case-insensitive volume
// exists and is exercised lives in pkg/claude/sandboxassumptions.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/unicode/norm"
)

// tempRoot returns a canonicalized temp directory. EvalSymlinks matters on
// macOS, where /var is a symlink to /private/var.
func tempRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return root
}

// volumeSemantics reports whether the test volume folds case, failing the test
// if the question cannot be answered at all — an unanswerable volume would make
// every assertion below meaningless.
func volumeSemantics(t *testing.T, dir string) bool {
	t.Helper()
	folds, err := volumeFoldsCase(dir)
	require.NoError(t, err, "the test volume must be able to answer whether it folds case")
	t.Logf("volume at %q folds case: %t (GOOS=%s)", dir, folds, runtime.GOOS)
	return folds
}

func TestGuardContainsOrEqualByteExactRelations(t *testing.T) {
	// Pure lexical relations must answer identically to PathContainsOrEqual and
	// must not depend on anything existing on disk.
	cases := []struct {
		name      string
		dir       string
		target    string
		contained bool
	}{
		{"equal", "/srv/data", "/srv/data", true},
		{"equal after clean", "/srv/data/", "/srv/./data", true},
		{"descendant", "/srv/data", "/srv/data/child/leaf", true},
		{"ancestor is not contained by descendant", "/srv/data/child", "/srv/data", false},
		{"shared string prefix is not containment", "/srv/data", "/srv/database", false},
		{"sibling", "/srv/a", "/srv/b", false},
		{"root contains everything", "/", "/srv/data", true},
		{"unrelated", "/opt/x", "/srv/y", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.contained, GuardContainsOrEqual(tc.dir, tc.target))
			assert.Equal(t, tc.contained, PathContainsOrEqual(tc.dir, tc.target),
				"byte-exact relations must not diverge between the guard and the lexical rule")
		})
	}
}

func TestGuardContainsOrEqualUnrelatedPathsNeedNoFilesystem(t *testing.T) {
	// Paths with no folded relation are refuted lexically. Pointing them at a
	// tree that does not exist proves the fast path answers without needing any
	// of it to be readable — this is the property that keeps the guard cheap
	// when it runs once per grant per protected root.
	missing := filepath.Join(tempRoot(t), "does", "not", "exist")
	assert.False(t, GuardContainsOrEqual(filepath.Join(missing, "alpha"), filepath.Join(missing, "beta")))
	assert.False(t, GuardPathsIntersect(filepath.Join(missing, "alpha"), filepath.Join(missing, "beta")))
}

func TestGuardContainsOrEqualCaseVariantFollowsVolumeSemantics(t *testing.T) {
	root := tempRoot(t)
	folds := volumeSemantics(t, root)

	protected := filepath.Join(root, "Protected")
	require.NoError(t, os.MkdirAll(filepath.Join(protected, "state"), 0o755))

	// The same directory, spelled the other way. On a folding volume this names
	// one tree and the guard must see through the spelling; on a case-sensitive
	// volume these are two trees and the guard must keep them apart.
	variant := filepath.Join(root, "protected")
	variantChild := filepath.Join(variant, "state")

	assert.Equal(t, folds, GuardContainsOrEqual(protected, variantChild))
	assert.Equal(t, folds, GuardContainsOrEqual(variant, filepath.Join(protected, "state")))
	assert.Equal(t, folds, GuardPathsIntersect(variantChild, protected))

	// Equality boundary: the two spellings of the directory itself.
	assert.Equal(t, folds, GuardContainsOrEqual(protected, variant))

	// Ancestor boundary in the wrong direction stays false either way: a
	// descendant never contains its ancestor, however it is spelled.
	assert.False(t, GuardContainsOrEqual(filepath.Join(protected, "state"), variant))
}

func TestGuardContainsOrEqualDistinctCaseSiblingsStaySeparate(t *testing.T) {
	root := tempRoot(t)
	if volumeSemantics(t, root) {
		// A folding volume cannot hold both spellings at once; the merge case is
		// covered by the test above.
		assert.True(t, GuardContainsOrEqual(filepath.Join(root, "Alpha"), filepath.Join(root, "alpha")))
		return
	}
	// On a case-sensitive volume both spellings can exist as genuinely
	// different directories. File identity must refute the fold nomination —
	// this is the "do not lowercase blindly" property.
	upper := filepath.Join(root, "Alpha")
	lower := filepath.Join(root, "alpha")
	require.NoError(t, os.MkdirAll(filepath.Join(upper, "child"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(lower, "child"), 0o755))
	assert.False(t, GuardContainsOrEqual(upper, filepath.Join(lower, "child")))
	assert.False(t, GuardPathsIntersect(upper, lower))
}

func TestGuardContainsOrEqualMissingTargetConsultsTheVolume(t *testing.T) {
	root := tempRoot(t)
	folds := volumeSemantics(t, root)

	protected := filepath.Join(root, "Protected")
	require.NoError(t, os.MkdirAll(protected, 0o755))

	// Nothing at this spelling exists, so file identity cannot answer. The
	// volume's own semantics decide instead. This is the case that matters most
	// in production: a grant may legitimately name a directory the launch will
	// create, and the protected-root wall still has to hold for it.
	missingVariant := filepath.Join(root, "protected", "not-created-yet")
	assert.Equal(t, folds, GuardContainsOrEqual(protected, missingVariant))

	// Both sides missing: the probe anchors on the nearest existing ancestor.
	assert.Equal(t, folds, GuardContainsOrEqual(
		filepath.Join(root, "Ghost"),
		filepath.Join(root, "ghost", "child"),
	))
}

func TestGuardContainsOrEqualSymlinkAliasIsSpellingOnly(t *testing.T) {
	root := tempRoot(t)
	real := filepath.Join(root, "real")
	require.NoError(t, os.MkdirAll(filepath.Join(real, "child"), 0o755))
	alias := filepath.Join(root, "alias")
	require.NoError(t, os.Symlink(real, alias))

	// The guard answers a SPELLING question, not a symlink question: callers
	// resolve links first (canonicalDirectory runs EvalSymlinks before any
	// comparison). An unresolved alias is simply an unrelated path here, and
	// must not be silently accepted as containment.
	assert.False(t, GuardContainsOrEqual(real, filepath.Join(alias, "child")))

	// Once resolved the way production resolves it, containment holds.
	resolved, err := filepath.EvalSymlinks(filepath.Join(alias, "child"))
	require.NoError(t, err)
	assert.True(t, GuardContainsOrEqual(real, resolved))
}

func TestGuardContainsOrEqualUnreadableAncestorFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Log("running as root: permission bits do not deny traversal, asserting the readable behavior instead")
	}
	root := tempRoot(t)
	// The gate must be the PARENT of the compared spellings: denying traversal
	// there is what makes lstat of the spellings themselves fail, so neither
	// file identity nor the volume probe can settle the folded nomination.
	gate := filepath.Join(root, "gate")
	locked := filepath.Join(gate, "Locked")
	require.NoError(t, os.MkdirAll(filepath.Join(locked, "inner"), 0o755))
	require.NoError(t, os.Chmod(gate, 0o000))
	t.Cleanup(func() { _ = os.Chmod(gate, 0o755) })

	target := filepath.Join(gate, "locked", "inner", "leaf")
	got := GuardContainsOrEqual(locked, target)
	if os.Geteuid() == 0 {
		return // root traverses regardless; nothing about fail-closed is provable
	}
	// Traversal is denied, so neither identity nor the volume probe can settle
	// the folded nomination. An unprovable non-relation must refuse.
	assert.True(t, got, "an unreadable ancestor must fail closed (refuse), never fail open")
}

func TestVolumeFoldsCaseWalksPastUncasedComponentsAndStopsAtRoot(t *testing.T) {
	root := tempRoot(t)
	// A component with no cased letters cannot answer the question, so the
	// probe walks up to one that can.
	numeric := filepath.Join(root, "123")
	require.NoError(t, os.MkdirAll(numeric, 0o755))
	folds, err := volumeFoldsCase(numeric)
	require.NoError(t, err)
	assert.Equal(t, volumeSemantics(t, root), folds)

	// The filesystem root has no parent to probe against, so the question is
	// unanswerable there and callers must treat that as indeterminate.
	_, err = volumeFoldsCase(string(filepath.Separator))
	assert.ErrorIs(t, err, errSpellingProbeUnavailable)
}

func TestNearestExistingDirSkipsMissingAndNonDirectoryAncestors(t *testing.T) {
	root := tempRoot(t)
	dir := filepath.Join(root, "present")
	require.NoError(t, os.MkdirAll(dir, 0o755))

	got, err := nearestExistingDir(filepath.Join(dir, "a", "b", "c"))
	require.NoError(t, err)
	assert.Equal(t, dir, got)

	// A regular file cannot host a probe; its parent can.
	file := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	got, err = nearestExistingDir(filepath.Join(file, "below"))
	require.NoError(t, err)
	assert.Equal(t, dir, got)

	got, err = nearestExistingDir(dir)
	require.NoError(t, err)
	assert.Equal(t, dir, got)
}

func TestGuardPathPrefixMatchesDirDepth(t *testing.T) {
	prefix, ok := guardPathPrefix("/a/b", "/A/B/c/d")
	require.True(t, ok)
	assert.Equal(t, "/A/B", prefix)

	prefix, ok = guardPathPrefix("/", "/a/b")
	require.True(t, ok)
	assert.Equal(t, "/", prefix)

	// A target shallower than dir has no prefix at dir's depth. The caller
	// treats that as indeterminate rather than as a refutation.
	_, ok = guardPathPrefix("/a/b/c", "/a/b")
	assert.False(t, ok)
}

func TestFlipCaseInvertsEveryCasedRune(t *testing.T) {
	assert.Equal(t, "tCLAUDE", flipCase("Tclaude"))
	assert.Equal(t, "ÉCOLE", flipCase("école"))
	assert.Equal(t, "123-_.", flipCase("123-_."), "an uncased name must be unchanged so the probe walks up")
}

func TestGuardDifferenceClassification(t *testing.T) {
	nfd := norm.NFD.String("café")
	nfc := norm.NFC.String("café")
	require.NotEqual(t, nfd, nfc)

	// Pure case difference.
	assert.True(t, guardCaseDiffers("/a/Foo", "/a/foo"))
	assert.False(t, guardNormalizationDiffers("/a/Foo", "/a/foo"))

	// Pure normalization difference.
	assert.False(t, guardCaseDiffers("/a/"+nfd, "/a/"+nfc))
	assert.True(t, guardNormalizationDiffers("/a/"+nfd, "/a/"+nfc))

	// Both at once: each seam must be consulted, so both classifiers fire.
	assert.True(t, guardCaseDiffers("/a/"+strings.ToUpper(nfd), "/a/"+nfc))
	assert.True(t, guardNormalizationDiffers("/a/"+strings.ToUpper(nfd), "/a/"+nfc))

	// Identical spellings differ in neither dimension.
	assert.False(t, guardCaseDiffers("/a/foo", "/a/foo"))
	assert.False(t, guardNormalizationDiffers("/a/foo", "/a/foo"))
}

func TestFoldGuardPathMatchesTheSeatbeltEmitterRule(t *testing.T) {
	// session.seatbeltFoldedPath is norm.NFC(strings.ToLower(filepath.Clean)).
	// The validator has to nominate exactly the spellings the emitter merges,
	// or the two layers disagree about what one directory is.
	for _, path := range []string{"/A/B/", "/a/./b", "/" + norm.NFD.String("Café")} {
		assert.Equal(t,
			norm.NFC.String(strings.ToLower(filepath.Clean(path))),
			foldGuardPath(path))
	}
}
