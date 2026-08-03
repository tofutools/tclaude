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

	// Whether the mode bits actually deny traversal is a property of the RUNNER,
	// not of the test: root ignores them, and containerized CI often runs as
	// root. Ask the filesystem which world we are in rather than skipping — a
	// skip here would silently drop the fail-closed property from CI entirely.
	_, probeErr := os.Lstat(locked)
	denied := os.IsPermission(probeErr)
	t.Logf("traversal denied: %t (euid=%d, lstat err=%v)", denied, os.Geteuid(), probeErr)

	if !denied {
		// Traversal works, so identity is available and answers precisely: on a
		// case-sensitive volume the two spellings are different directories.
		lockedInfo, err := os.Lstat(locked)
		require.NoError(t, err)
		variantInfo, variantErr := os.Lstat(filepath.Join(gate, "locked"))
		want := variantErr == nil && os.SameFile(lockedInfo, variantInfo)
		assert.Equal(t, want, got,
			"with traversal permitted the answer must follow file identity exactly")
		return
	}
	// Traversal is denied, so neither identity nor the volume probe can settle
	// the folded nomination. An unprovable non-relation must refuse.
	assert.True(t, got, "an unreadable ancestor must fail closed (refuse), never fail open")
}

func TestVolumeFoldsCaseWalksPastUncasedComponentsWithinOneVolume(t *testing.T) {
	root := tempRoot(t)
	// A component with no cased letters cannot answer the question, so the probe
	// walks up to one that can. Generalizing an ancestor's answer to a
	// descendant is only sound WITHIN one filesystem, which is why the probe
	// refuses the moment it would cross a device boundary — see
	// TestVolumeFoldsCaseRefusesToAnswerFromAnotherVolume.
	numeric := filepath.Join(root, "123")
	require.NoError(t, os.MkdirAll(numeric, 0o755))
	requireSameVolume(t, root, numeric)

	folds, err := volumeFoldsCase(numeric)
	require.NoError(t, err)
	assert.Equal(t, volumeSemantics(t, root), folds)
}

func TestVolumeFoldsCaseStopsAtTheFilesystemRoot(t *testing.T) {
	// The filesystem root has no parent to probe against, so the question is
	// unanswerable there and callers must treat that as indeterminate.
	_, err := volumeFoldsCase(string(filepath.Separator))
	assert.ErrorIs(t, err, errSpellingProbeUnavailable)
}

// TestVolumeFoldsCaseRefusesToAnswerFromAnotherVolume pins the fix for the
// probe's most dangerous failure mode.
//
// A name is stored in its PARENT directory, so respelling a directory's own
// basename questions the parent's filesystem. When the directory is a mount
// point — or when the walk past uncased components crosses one — that is a
// DIFFERENT filesystem, whose case semantics say nothing about the one asked
// about. Answering "does not fold" from a neighbouring case-sensitive volume
// would be a definitive wrong answer, and a definitive wrong answer here makes
// a guard ALLOW: a case-insensitive mount (vfat/exFAT/CIFS, a casefolded ext4
// directory, a case-insensitive disk image on a case-sensitive macOS boot
// volume) would have its protected roots reachable through a variant spelling.
//
// Crossing a device boundary must therefore end the probe as indeterminate.
func TestVolumeFoldsCaseRefusesToAnswerFromAnotherVolume(t *testing.T) {
	// /proc, /sys and /dev are separate filesystems from / on Linux; /dev is on
	// macOS too. Find any real mount point rather than staging one, which would
	// need privileges CI does not have.
	crossings := []string{"/proc", "/sys", "/dev"}
	probed := 0
	for _, mount := range crossings {
		info, err := os.Lstat(mount)
		if err != nil || !info.IsDir() {
			continue
		}
		parentInfo, err := os.Lstat(filepath.Dir(mount))
		if err != nil {
			continue
		}
		mountDev, mountOK := pathDevice(info)
		parentDev, parentOK := pathDevice(parentInfo)
		if !mountOK || !parentOK || mountDev == parentDev {
			continue // not actually a mount point on this host
		}
		probed++
		_, err = volumeFoldsCase(mount)
		assert.ErrorIs(t, err, errSpellingProbeUnavailable,
			"%q is a mount point, so its own name lives on the parent volume and "+
				"the probe must refuse to answer for it", mount)
	}
	if probed == 0 {
		// Do not skip: say plainly that the property went unexercised here, and
		// still assert the mechanism it rests on.
		t.Log("no cross-device mount point available on this host; " +
			"asserting the device-identity mechanism directly instead")
	}
	// The mechanism itself, independent of any host's mount table: device
	// identity must be obtainable, or the probe has nothing to compare and every
	// answer would be an unjustified guess.
	info, err := os.Lstat(tempRoot(t))
	require.NoError(t, err)
	_, ok := pathDevice(info)
	assert.True(t, ok, "device identity must be available for the probe to bound itself")
}

// requireSameVolume fails the test when two paths are not on one filesystem, so
// a test that means to exercise the same-volume walk cannot silently turn into
// a cross-device case on an unusual host.
func requireSameVolume(t *testing.T, a, b string) {
	t.Helper()
	aInfo, err := os.Lstat(a)
	require.NoError(t, err)
	bInfo, err := os.Lstat(b)
	require.NoError(t, err)
	aDev, aOK := pathDevice(aInfo)
	bDev, bOK := pathDevice(bInfo)
	require.True(t, aOK && bOK, "device identity must be available")
	require.Equal(t, aDev, bDev, "%q and %q must be on one filesystem for this test", a, b)
}

// TestGuardContainsOrEqualRelativePathsAnswerLexically pins the precondition:
// only an absolute path names a real directory, so a relative one must get the
// byte-exact answer rather than having an absolute candidate fabricated for it
// and probed against an unrelated tree.
func TestGuardContainsOrEqualRelativePathsAnswerLexically(t *testing.T) {
	for _, tc := range []struct{ dir, target string }{
		{"A/b", "a/b/c"},
		{"a/b", "a/b/c"},
		{"relative", "/absolute/child"},
		{"/absolute", "relative/child"},
	} {
		assert.Equal(t, PathContainsOrEqual(tc.dir, tc.target),
			GuardContainsOrEqual(tc.dir, tc.target),
			"relative input %q/%q must not diverge from the lexical rule", tc.dir, tc.target)
	}
}

// TestGuardContainsOrEqualFollowsSymlinkedFinalComponent covers the identity
// step's one blind spot: a final component that is a SYMLINK to the other
// spelling has its own inode, so an lstat-based comparison would report two
// objects where the filesystem resolves to one — and a guard would fail open.
func TestGuardContainsOrEqualFollowsSymlinkedFinalComponent(t *testing.T) {
	root := tempRoot(t)
	real := filepath.Join(root, "protected")
	require.NoError(t, os.MkdirAll(filepath.Join(real, "child"), 0o755))
	// A case-variant spelling that is a symlink to the real directory. Both
	// spellings exist, so identity — not the volume probe — decides.
	variant := filepath.Join(root, "PROTECTED")
	if _, err := os.Lstat(variant); err != nil {
		require.NoError(t, os.Symlink(real, variant))
	}
	assert.True(t, GuardContainsOrEqual(real, filepath.Join(variant, "child")),
		"a symlinked case-variant spelling reaches the same tree and must be refused")
	assert.True(t, GuardPathsIntersect(variant, real))
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
