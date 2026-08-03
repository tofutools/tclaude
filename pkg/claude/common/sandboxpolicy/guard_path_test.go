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
	"fmt"
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

// volumeSemantics reports whether dir folds case, measured DIRECTLY rather than
// by calling the production probe.
//
// Using volumeFoldsCase here would make the implementation its own oracle: a
// probe that answered wrongly would also set the expectation wrongly, and every
// assertion below would agree with the bug. So this stages a cased directory of
// its own and asks the filesystem itself.
func volumeSemantics(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "VolumeSemanticsProbe")
	if err := os.MkdirAll(probe, 0o755); err != nil {
		t.Fatalf("stage volume probe: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(probe) })

	probeInfo, err := os.Lstat(probe)
	require.NoError(t, err)
	variantInfo, variantErr := os.Lstat(filepath.Join(dir, "volumesemanticsprobe"))
	folds := variantErr == nil && os.SameFile(probeInfo, variantInfo)
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

// TestVolumeFoldsCaseAnswersFromTheDirectoryItself pins the probe's central
// property: it reports what THIS directory does, not what its parent does.
//
// Folding is not always a property of the volume. ext4's casefold is a
// PER-DIRECTORY attribute (chattr +F, inherited by children) on a single
// device, so a casefolded directory and its case-sensitive parent share one
// st_dev and no device check can tell them apart. A directory that is a mount
// point has the opposite problem: its own name is stored in the parent's
// filesystem, which may have entirely different semantics from its contents.
//
// Both are answered by respelling an entry the directory already holds and
// looking that respelling up INSIDE the directory, which is what the probe now
// does. Asking about the directory's own basename would interrogate the parent,
// and where the two disagree that is a definitive WRONG answer — which for a
// guard means allow.
// TestGuardContainsOrEqualUnresolvableSpellingFailsClosed asserts the
// refuse-when-indeterminate rule on EVERY runner, including root.
//
// The permission-based test below cannot do that: root ignores mode bits, so on
// a containerized CI runner its fail-closed branch never executes. A symlink
// loop is denied to root exactly as it is to anyone else — ELOOP is not a
// permission, it is an unresolvable path — so this pins the property
// unconditionally.
func TestGuardContainsOrEqualUnresolvableSpellingFailsClosed(t *testing.T) {
	root := tempRoot(t)
	// A self-referential symlink: lstat sees a link, stat fails with ELOOP.
	loop := filepath.Join(root, "loop")
	require.NoError(t, os.Symlink("loop", loop))
	_, statErr := os.Stat(loop)
	require.Error(t, statErr)
	require.False(t, os.IsNotExist(statErr),
		"the probe needs a non-ENOENT failure to exercise the indeterminate branch")

	// A case variant of the loop, so a folded relation is nominated and the
	// comparison actually reaches identity resolution.
	assert.True(t, GuardContainsOrEqual(loop, filepath.Join(root, "LOOP", "child")),
		"a spelling that cannot be resolved must refuse, never allow")

	// The same must hold whichever side is unresolvable.
	assert.True(t, GuardPathsIntersect(filepath.Join(root, "LOOP"), loop))
}

func TestVolumeFoldsCaseAnswersFromTheDirectoryItself(t *testing.T) {
	root := tempRoot(t)
	dir := filepath.Join(root, "probe-subject")
	child := filepath.Join(dir, "Entry")
	require.NoError(t, os.MkdirAll(child, 0o755))

	// Ground truth for THIS directory, measured directly.
	childInfo, err := os.Lstat(child)
	require.NoError(t, err)
	variantInfo, variantErr := os.Lstat(filepath.Join(dir, "eNTRY"))
	want := variantErr == nil && os.SameFile(childInfo, variantInfo)

	got, err := volumeFoldsCase(dir)
	require.NoError(t, err)
	assert.Equal(t, want, got,
		"the probe must report what %q itself does with its own entries", dir)

	// And it must be reached through the own-entries path, not the fallback.
	got, err = foldsByOwnEntries(dir, flipCase)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// TestVolumeFoldsCaseAnswersForAMountPoint is the concrete payoff: a mount point
// used to be unanswerable (its basename lives on the parent volume), and is now
// answered from its own contents.
func TestVolumeFoldsCaseAnswersForAMountPoint(t *testing.T) {
	answered := 0
	for _, mount := range []string{"/proc", "/sys", "/dev"} {
		if !isCrossDeviceMountPoint(mount) {
			continue
		}
		answered++
		_, err := volumeFoldsCase(mount)
		assert.NoError(t, err,
			"%q must be answerable from its own entries rather than from its parent volume", mount)

		// The fallback, in isolation, must still refuse for it: that path reads
		// the parent's filesystem, which is not the one being asked about.
		_, err = foldsByAncestorName(mount, flipCase)
		assert.ErrorIs(t, err, errSpellingProbeUnavailable,
			"the ancestor-name fallback must refuse to answer for %q from its parent volume", mount)
	}
	if answered == 0 {
		t.Log("no cross-device mount point on this host; the device-bounded fallback " +
			"is still asserted by TestFoldsByAncestorNameRefusesAcrossADeviceBoundary")
	}
	// Assert the mechanism regardless of this host's mount table.
	info, err := os.Lstat(tempRoot(t))
	require.NoError(t, err)
	_, ok := pathDevice(info)
	assert.True(t, ok, "device identity must be available for the fallback to bound itself")
}

// TestGuardProbeRefusesWhenTheDirectoryCannotAnswer covers the case the
// inside-out probe cannot settle: a directory holding no entry whose respelling
// differs from itself.
//
// The GUARD's probe must report that as unavailable rather than fall back to
// asking the parent. The parent is a different directory and may have different
// semantics — ext4 casefold is per-directory, and a mount point's name lives on
// the parent's filesystem entirely — so the parent's "does not fold" would be a
// definitive WRONG answer, and definitive-wrong here means allow.
func TestGuardProbeRefusesWhenTheDirectoryCannotAnswer(t *testing.T) {
	root := tempRoot(t)

	empty := filepath.Join(root, "empty")
	require.NoError(t, os.MkdirAll(empty, 0o755))
	_, err := volumeFoldsCase(empty)
	assert.ErrorIs(t, err, errSpellingProbeUnavailable,
		"an empty directory holds nothing to respell, and the guard must not "+
			"substitute its parent's answer")

	// Entries with no cased runes cannot answer either.
	uncased := filepath.Join(root, "uncased")
	require.NoError(t, os.MkdirAll(filepath.Join(uncased, "123"), 0o755))
	_, err = volumeFoldsCase(uncased)
	assert.ErrorIs(t, err, errSpellingProbeUnavailable)

	// A directory whose first probeEntryScanLimit entries are all uncased must
	// also report unavailable rather than downgrading: a cased entry could lie
	// just beyond the bound, so the question is unknown, not answered.
	crowded := filepath.Join(root, "crowded")
	require.NoError(t, os.MkdirAll(crowded, 0o755))
	for i := range probeEntryScanLimit + 8 {
		require.NoError(t, os.MkdirAll(
			filepath.Join(crowded, fmt.Sprintf("%03d", i)), 0o755))
	}
	_, err = volumeFoldsCase(crowded)
	assert.ErrorIs(t, err, errSpellingProbeUnavailable,
		"exhausting the scan bound without a cased entry is unknown, not resolved")

	// And that refusal must reach the guard as a REFUSAL, not an allow.
	assert.True(t, GuardContainsOrEqual(
		filepath.Join(empty, "Child"), filepath.Join(empty, "child", "leaf")),
		"an unanswerable directory must make the guard refuse")
}

// TestCanonicalizationProbeMayFallBackToTheParent pins the deliberate asymmetry:
// CanonicalHostSpelling's probe MAY consult the parent, because both of its
// outcomes degrade safely — a wrong "folds" starts a restoration that re-reads
// every directory to verify, and a wrong "does not fold" just leaves the
// authored spelling for the guard to judge.
func TestCanonicalizationProbeMayFallBackToTheParent(t *testing.T) {
	root := tempRoot(t)
	empty := filepath.Join(root, "empty")
	require.NoError(t, os.MkdirAll(empty, 0o755))
	requireSameVolume(t, root, empty)

	folds, err := volumeFoldsSpellingForCanonicalization(empty, flipCase)
	require.NoError(t, err, "the lax probe must answer where the strict one refuses")
	assert.Equal(t, volumeSemantics(t, root), folds)

	// The strict probe refuses for the very same directory. That divergence is
	// the point, not an accident.
	_, err = volumeFoldsCase(empty)
	assert.ErrorIs(t, err, errSpellingProbeUnavailable)
}

// TestFoldsByAncestorNameRefusesAcrossADeviceBoundary pins the bound on the
// fallback directly, without depending on this host's mount table.
func TestFoldsByAncestorNameRefusesAcrossADeviceBoundary(t *testing.T) {
	probed := 0
	for _, mount := range []string{"/proc", "/sys", "/dev"} {
		if !isCrossDeviceMountPoint(mount) {
			continue
		}
		probed++
		_, err := foldsByAncestorName(mount, flipCase)
		assert.ErrorIs(t, err, errSpellingProbeUnavailable)
	}
	if probed == 0 {
		t.Log("no cross-device mount point available on this host")
	}
	// The filesystem root has no parent, so the fallback has nothing to ask.
	_, err := foldsByAncestorName(string(filepath.Separator), flipCase)
	assert.ErrorIs(t, err, errSpellingProbeUnavailable)
}

// isCrossDeviceMountPoint reports whether path is a directory on a different
// device from its parent — i.e. a real mount point on this host.
func isCrossDeviceMountPoint(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	parentInfo, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return false
	}
	dev, ok := pathDevice(info)
	parentDev, parentOK := pathDevice(parentInfo)
	return ok && parentOK && dev != parentDev
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

// TestGuardGoverningDirPicksTheContainingDirectory pins which directory the
// probe asks. A name is stored in its parent, and the probe answers per
// directory (ext4 casefold is a per-directory attribute), so the directory that
// decides is the one containing the first component in which the two spellings
// differ — not either spelling itself.
func TestGuardGoverningDirPicksTheContainingDirectory(t *testing.T) {
	for _, tc := range []struct{ a, b, want string }{
		{"/home/u/.tclaude/data", "/home/u/.Tclaude/Data", "/home/u"},
		{"/home/u/x/Y", "/home/u/x/y", "/home/u/x"},
		{"/Alpha", "/alpha", "/"},
		{"/a/b/c", "/a/B/c", "/a"},
	} {
		assert.Equal(t, tc.want, guardGoverningDir(tc.a, tc.b),
			"%q vs %q", tc.a, tc.b)
	}
}

// TestGuardDoesNotRefuseWhenOnlyTheOtherSpellingIsAnEmptyDirectory is the
// regression for anchoring on the wrong directory.
//
// Anchoring the probe on whichever spelling happened to EXIST meant that an
// empty existing directory — which can answer nothing about its own folding —
// made the guard refuse. That is a fail-closed bug, not a hole, but it would
// have refused ordinary grants: a protected root is very often an empty
// directory on a fresh install.
func TestGuardDoesNotRefuseWhenOnlyTheOtherSpellingIsAnEmptyDirectory(t *testing.T) {
	root := tempRoot(t)
	folds := volumeSemantics(t, root)

	// An EMPTY directory, and a case variant of it that does not exist.
	existing := filepath.Join(root, "Protected")
	require.NoError(t, os.MkdirAll(existing, 0o755))
	entries, err := os.ReadDir(existing)
	require.NoError(t, err)
	require.Empty(t, entries, "the directory must be empty for this regression to bite")

	// The governing directory is root, which holds cased entries and can answer.
	assert.Equal(t, folds, GuardContainsOrEqual(existing, filepath.Join(root, "protected", "leaf")),
		"an empty existing spelling must not make the guard refuse; the "+
			"containing directory is what decides")
}
