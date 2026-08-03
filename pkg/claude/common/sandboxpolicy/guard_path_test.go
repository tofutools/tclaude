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

// TestGuardContainsOrEqualCaseVariantFollowsFileIdentity covers the case where
// BOTH spellings resolve, which is the only case the guard answers from
// evidence rather than by refusing.
//
// Here — and only here — the answer is volume-dependent, because file identity
// itself is: on a folding volume both spellings reach one inode, on a
// case-sensitive one they reach two. Note the expectation is derived from
// staging the directories and asking the filesystem, not from calling the code
// under test.
func TestGuardContainsOrEqualCaseVariantFollowsFileIdentity(t *testing.T) {
	root := tempRoot(t)
	folds := volumeSemantics(t, root)

	protected := filepath.Join(root, "Protected")
	require.NoError(t, os.MkdirAll(filepath.Join(protected, "state"), 0o755))

	variant := filepath.Join(root, "protected")
	if !folds {
		// Stage the variant as a genuinely separate tree so both spellings
		// resolve; otherwise this would be the refuse-because-missing case,
		// which TestGuardRefusesEveryUnresolvableFoldedNomination owns.
		require.NoError(t, os.MkdirAll(filepath.Join(variant, "state"), 0o755))
	}
	variantChild := filepath.Join(variant, "state")

	assert.Equal(t, folds, GuardContainsOrEqual(protected, variantChild))
	assert.Equal(t, folds, GuardContainsOrEqual(variant, filepath.Join(protected, "state")))
	assert.Equal(t, folds, GuardPathsIntersect(variantChild, protected))

	// Equality boundary: the two spellings of the directory itself.
	assert.Equal(t, folds, GuardContainsOrEqual(protected, variant))

	// Ancestor boundary in the wrong direction stays false either way: a
	// descendant never contains its ancestor, however it is spelled, and this
	// is refuted lexically before any I/O.
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



// TestGuardRefusesEveryUnresolvableFoldedNomination is the core of the
// simplified contract, and it asserts the SAME outcome on every volume — no
// adaptive branching, because the answer no longer depends on what the
// filesystem would do with a spelling that does not exist.
//
// A folded nomination that cannot be settled by filesystem identity is refused.
// That single rule replaces the empirical volume probing that carried four
// separate fail-open defects: "does this filesystem fold?" is per-directory
// rather than per-volume, a mount point's own name lives on its parent's
// filesystem, and case-flipping is not a round trip — so every definitive
// answer that machinery produced could be a definitive WRONG answer, and in a
// guard that means allow.
func TestGuardRefusesEveryUnresolvableFoldedNomination(t *testing.T) {
	root := tempRoot(t)
	existing := filepath.Join(root, "Protected")
	require.NoError(t, os.MkdirAll(existing, 0o755))

	// Neither spelling exists.
	assert.True(t, GuardContainsOrEqual(
		filepath.Join(root, "Ghost"), filepath.Join(root, "ghost", "child")),
		"neither spelling resolves, so the nomination cannot be refuted")

	// Only one spelling exists — on a case-sensitive volume this is the case
	// that used to consult the volume probe and answer "allow".
	assert.True(t, GuardContainsOrEqual(existing, filepath.Join(root, "protected", "leaf")),
		"one spelling missing means identity cannot settle the nomination")
	assert.True(t, GuardPathsIntersect(filepath.Join(root, "protected"), existing))

	// A symlink loop: resolvable neither by stat nor by any probe. ELOOP is
	// denied to root exactly as to anyone else, so this asserts the refusal on
	// every runner including a containerized one running as uid 0.
	loop := filepath.Join(root, "loop")
	require.NoError(t, os.Symlink("loop", loop))
	_, statErr := os.Stat(loop)
	require.Error(t, statErr)
	require.False(t, os.IsNotExist(statErr), "the loop must fail with ELOOP, not ENOENT")
	assert.True(t, GuardContainsOrEqual(loop, filepath.Join(root, "LOOP", "child")),
		"an unresolvable spelling must refuse")

	// A dangling symlink resolves to nothing, and must refuse rather than being
	// compared by its own link inode.
	dangling := filepath.Join(root, "Dangling")
	require.NoError(t, os.Symlink(filepath.Join(root, "nonexistent-target"), dangling))
	assert.True(t, GuardContainsOrEqual(dangling, filepath.Join(root, "dangling", "child")))
}

// TestGuardIsUnaffectedByNeighbouringFileNames pins the property whose absence
// was the fourth fail-open.
//
// The deleted probe inspected OTHER entries of the governing directory to infer
// fold semantics, so a single neighbouring file whose name does not survive a
// case flip — U+0130 "İ", U+017F "ſ", invalid UTF-8 — could make a folding
// directory report itself case-sensitive and open every path governed by it.
// The guard now reads nothing but the two paths it was asked about, so an
// unrelated filename cannot change any answer.
func TestGuardIsUnaffectedByNeighbouringFileNames(t *testing.T) {
	root := tempRoot(t)
	protected := filepath.Join(root, "Protected")
	require.NoError(t, os.MkdirAll(protected, 0o755))

	before := GuardContainsOrEqual(protected, filepath.Join(root, "protected", "leaf"))

	// Names that a case-flip probe mishandles. Any the platform rejects are
	// skipped individually; the assertion is about the ones that land.
	staged := 0
	for _, name := range []string{"İstanbul", "ſign", "\xff\xfe-binary", "ΩMEGA"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err == nil {
			staged++
		}
	}
	require.Positive(t, staged, "at least one adversarial name must be stageable")

	assert.Equal(t, before,
		GuardContainsOrEqual(protected, filepath.Join(root, "protected", "leaf")),
		"an unrelated neighbouring filename must not change the guard's answer")
}
