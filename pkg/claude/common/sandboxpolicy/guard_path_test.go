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
	"golang.org/x/text/cases"
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

// TestFoldGuardPathMatchesTheSeatbeltEmitterRule is the pin TCL-985 was required
// to update deliberately rather than route around. session.seatbeltFoldedPath no
// longer restates the formula at all — it CALLS FoldGuardPath — so the pin that
// the two layers cannot drift now lives in
// session.TestSeatbeltFoldedPathIsTheValidatorNominationKey, which is the only
// place that can observe both. What remains here is the formula itself.
func TestFoldGuardPathMatchesTheSeatbeltEmitterRule(t *testing.T) {
	// Lowercase, then FULL case fold, then NFC. Composed, not substituted: see
	// foldGuardPath on why replacing ToLower with Fold would have opened a new
	// fail-open at U+0130.
	for _, path := range []string{"/A/B/", "/a/./b", "/" + norm.NFD.String("Café")} {
		assert.Equal(t,
			norm.NFC.String(cases.Fold().String(strings.ToLower(filepath.Clean(path)))),
			foldGuardPath(path))
	}
	assert.Equal(t, foldGuardPath("/A/B"), FoldGuardPath("/a/b/"),
		"the exported entry point is the same function")
}

// TestFoldGuardPathNominatesTheFullCaseFoldTable covers the runes TCL-981's
// simple-lowercase key missed, and the one rune that full folding alone would
// have missed in exchange. Each pair must produce ONE nomination key, or the
// validator answers "no folded relation" with no I/O and the guard fails open on
// a volume that merges the pair.
//
// This is pure-function evidence about the nomination key. It asserts the same
// thing on every platform and volume and needs no filesystem, which is why it
// carries no adaptive branch and no skip.
func TestFoldGuardPathNominatesTheFullCaseFoldTable(t *testing.T) {
	for _, tc := range []struct{ name, a, b string }{
		// Greek final sigma. Simple lowercasing maps Σ to σ and leaves ς alone,
		// so TCL-981's key kept these apart. Full folding merges them.
		{"final sigma", "/Users/ΟΔΟΣ", "/Users/οδος"},
		{"final vs medial sigma", "/Users/οδος", "/Users/οδοσ"},
		// Capital sharp S. Simple lowercasing gives ß; full folding gives "ss",
		// which is what reaches "STRASSE".
		{"capital sharp s", "/Users/STRAẞE", "/Users/strasse"},
		// U+0130 is the counterexample that keeps ToLower in the composition.
		// Full folding alone maps it to "i" + U+0307, which no longer meets a
		// plain "i"; lowercasing first maps it to "i" and the pair merges.
		{"dotted capital I", "/Users/İstanbul", "/Users/istanbul"},
		// The plain ASCII and NFC cases TCL-981 already handled must not regress.
		{"ascii case", "/Users/Dev/.TCLAUDE", "/users/dev/.tclaude"},
		{"nfc vs nfd", "/Users/" + norm.NFC.String("Café"), "/users/" + norm.NFD.String("café")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, foldGuardPath(tc.a), foldGuardPath(tc.b))
		})
	}

	// The key must still discriminate. A wider fold that collapsed unrelated
	// names would turn every guard into a blanket refusal.
	assert.NotEqual(t, foldGuardPath("/Users/dev"), foldGuardPath("/Users/dev2"))
	assert.NotEqual(t, foldGuardPath("/Users/alpha"), foldGuardPath("/Users/beta"))
}

// TestFoldGuardPathOnlyEverMergesMore is the load-bearing safety property of the
// TCL-985 folding change, and it is checked EXHAUSTIVELY rather than by example
// because the failure mode is a silent fail-open at one rune nobody thought of.
//
// A guard's nomination key may widen freely: every extra merge is an
// over-refusal, which os.SameFile refutes when both spellings exist. NARROWING
// is the dangerous direction — a pair the key stops merging is a pair that never
// reaches steps 3-4 at all, so the guard answers false with no I/O and a folding
// volume merges behind its back.
//
// That is a real risk here rather than a theoretical one, because full case
// folding is NOT a superset of simple lowercasing: U+0130 folds to "i" + U+0307
// while ToLower maps it to "i". This sweeps every code point, groups them by
// TCL-981's key, and requires that every pair that key merged is still merged.
// It is what justifies composing the two rules instead of substituting one.
func TestFoldGuardPathOnlyEverMergesMore(t *testing.T) {
	tcl981Key := func(s string) string { return normalizeNFC(strings.ToLower(s)) }

	groups := map[string][]rune{}
	for r := rune(0); r <= 0x10FFFF; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			// Surrogate halves are not characters; string(r) yields U+FFFD for
			// each, so they would form one meaningless group.
			continue
		}
		key := tcl981Key(string(r))
		groups[key] = append(groups[key], r)
	}

	merged := 0
	for _, runes := range groups {
		if len(runes) < 2 {
			continue
		}
		want := foldGuardPath("/" + string(runes[0]))
		for _, r := range runes[1:] {
			merged++
			require.Equal(t, want, foldGuardPath("/"+string(r)),
				"U+%04X and U+%04X share TCL-981's nomination key; the current key "+
					"must not split them, or the guard silently stops nominating a "+
					"pair a folding volume merges", runes[0], r)
		}
	}
	// Guard against the sweep going vacuous if the grouping above ever breaks.
	assert.Greater(t, merged, 1000,
		"the sweep must actually find case-equivalent runes to compare")
}

// TestGuardNominatesFoldedVariantsAcrossTheWholeGuard proves the widened key
// reaches GuardContainsOrEqual's answer rather than stopping at foldGuardPath.
// Neither spelling exists, so the nomination cannot be settled by identity and
// the documented step-4 refusal applies — on every platform and volume.
func TestGuardNominatesFoldedVariantsAcrossTheWholeGuard(t *testing.T) {
	root := tempRoot(t)
	assert.True(t, GuardContainsOrEqual(
		filepath.Join(root, "ΟΔΟΣ"), filepath.Join(root, "οδος", "child")),
		"a full-case-fold variant must nominate, then refuse for want of identity")
	assert.True(t, GuardPathsIntersect(
		filepath.Join(root, "İstanbul"), filepath.Join(root, "istanbul", "child")))
	assert.False(t, GuardContainsOrEqual(
		filepath.Join(root, "alpha"), filepath.Join(root, "beta", "child")),
		"an unrelated pair must stay a free lexical no")
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

// TestGuardNeverAllowsWhatByteExactContainmentRefuses pins the safety direction
// of the guard's relationship to pathContainsOrEqual: the guard may refuse MORE,
// never less. That one-way property is what makes it safe to substitute for the
// byte-exact comparison at every refusal site.
//
// It matters because the collapse is not exact. strings.ToLower is not injective
// beyond case, so pairs like U+212A KELVIN SIGN vs "k", or U+0130 vs "i",
// nominate a collision and reach the identity steps even though they are not
// case variants in the everyday sense. Those are over-refusals, and over-refusal
// is the harmless direction — the extra pairs are precisely the ones a folding
// volume might merge.
func TestGuardNeverAllowsWhatByteExactContainmentRefuses(t *testing.T) {
	root := tempRoot(t)

	pairs := [][2]string{
		{root, root},
		{root, filepath.Join(root, "child")},
		{filepath.Join(root, "a"), filepath.Join(root, "b")},
		{filepath.Join(root, "K"), filepath.Join(root, "K")},        // KELVIN SIGN
		{filepath.Join(root, "i"), filepath.Join(root, "İ")},        // dotted capital I
		{filepath.Join(root, "ſ"), filepath.Join(root, "s")},        // long s
		{filepath.Join(root, "café"), filepath.Join(root, "café")}, // NFC vs NFD
		{"/", filepath.Join(root, "deep", "path")},
		{filepath.Join(root, "deep", "path"), "/"},
		{"", ""},
		{root, ""},
	}

	for _, pair := range pairs {
		dir, target := pair[0], pair[1]
		if pathContainsOrEqual(dir, target) && !GuardContainsOrEqual(dir, target) {
			t.Errorf("GuardContainsOrEqual(%q, %q) = false but pathContainsOrEqual = true: "+
				"the guard must never allow what the byte-exact comparison refuses",
				dir, target)
		}
	}
}

// TestGuardOverRefusalIsRefutedByFileIdentity is the other half: an over-refusal
// from the widened nomination is not permanent. When both spellings actually
// exist as distinct directories, os.SameFile refutes the nomination and the
// guard returns to the byte-exact answer — so the widening costs nothing for
// paths that are really there.
func TestGuardOverRefusalIsRefutedByFileIdentity(t *testing.T) {
	root := tempRoot(t)

	// U+212A KELVIN SIGN lowercases to "k", so these nominate as a collision
	// without being a case variant of one another.
	kelvin := filepath.Join(root, "K")
	plain := filepath.Join(root, "k")

	// Unresolvable: nothing can refute the nomination, so the guard refuses.
	assert.True(t, GuardContainsOrEqual(kelvin, plain),
		"an unresolvable folded nomination must refuse")

	require.NoError(t, os.MkdirAll(kelvin, 0o755))
	require.NoError(t, os.MkdirAll(plain, 0o755))

	kelvinInfo, err := os.Lstat(kelvin)
	require.NoError(t, err)
	plainInfo, err := os.Lstat(plain)
	require.NoError(t, err)
	if os.SameFile(kelvinInfo, plainInfo) {
		// This volume genuinely merges them, so refusing is the correct answer.
		assert.True(t, GuardContainsOrEqual(kelvin, plain),
			"one inode means one directory, which must refuse")
		return
	}
	assert.False(t, GuardContainsOrEqual(kelvin, plain),
		"two distinct inodes refute the nomination, so the guard must allow — "+
			"the widened fold must not survive contact with the filesystem")
}
