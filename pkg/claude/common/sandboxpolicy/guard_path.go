package sandboxpolicy

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// This file holds the *guard-biased* path comparison used by sandbox safety
// checks — the ones where a positive answer means "refuse".
//
// pathContainsOrEqual (profile.go) is byte-exact by design and stays that way:
// it is the lexical lattice operation the mount plan, shape, and snapshot code
// apply to paths that already came out of one canonicalizer, and it must remain
// pure and I/O-free.
//
// Safety guards are a different question. They compare an operator-authored
// spelling against a protected root, and on a case-insensitive volume (the APFS
// default on macOS) two spellings that differ only in case or in Unicode
// normalization name ONE directory. A byte-exact comparison there fails OPEN:
// a filesystem rule written as "~/.TCLAUDE/data" does not textually intersect
// the protected "~/.tclaude/data", so the protected-root invariant admits a
// write grant over exactly the daemon state it exists to make unreachable.
// filepath.EvalSymlinks does not save us — it is lexical plus readlink, so it
// hands back whatever casing the caller supplied.
//
// The rule below has exactly two sources of truth, and deliberately no third:
//
//   - a pure lexical case/NFC fold, which can only NOMINATE a collision; and
//   - filesystem identity (one inode means one directory), which can settle it.
//
// Anything a nomination cannot settle by identity is REFUSED. There is no
// attempt to reason about what a filesystem "would" do with a spelling that
// does not exist yet.
//
// That last point is a deliberate retreat from an earlier design, and the
// reason is worth keeping. This guard previously probed filesystem semantics
// empirically — flip a name's case, see whether the flipped spelling reaches
// the same inode — in order to answer the not-yet-created case. Four separate
// fail-open defects were found in that machinery during review, because "does
// this filesystem fold?" has no reliable answer:
//
//   - it is per-directory, not per-volume (ext4 casefold is a chattr attribute),
//     so neither a device check nor a parent's answer generalizes;
//   - a mount point's own name lives in the PARENT's filesystem, so asking about
//     the name asks the wrong filesystem; and
//   - case-flipping is not a round trip (U+0130 "İ", U+017F "ſ", invalid UTF-8),
//     so one oddly-named neighbouring file could make a folding directory answer
//     "case-sensitive" — poisoning every path governed by that directory.
//
// Each produced a DEFINITIVE wrong answer, and a definitive wrong answer in a
// guard is an ALLOW. Refusing the unprovable case costs an operator a clear
// error about a spelling they can create or rename; getting it wrong costs a
// sandboxed agent write access to the daemon's own state. The trade is not
// close.

// GuardContainsOrEqual reports whether dir is target or an ancestor of it, for
// the purpose of a sandbox SAFETY GUARD where a true answer means "refuse".
//
// The ladder is:
//
//  1. byte-exact containment — true.
//  2. no case/NFC-folded containment — false. Definitive, and free: a pure
//     lexical test, so an ordinary pair of unrelated paths costs no I/O.
//  3. folded containment, and both spellings resolve — os.SameFile decides.
//     One inode means one directory; two inodes mean two.
//  4. folded containment, and either spelling does not resolve — TRUE, refuse.
//
// Step 4 is the bias, and it is why this must never be used where a true answer
// GRANTS access. For lexical lattice work on already-canonical paths, use
// PathContainsOrEqual.
//
// On a case-sensitive volume this collapses to exactly PathContainsOrEqual's
// answer for every pair that is not a deliberate case/NFC variant of a
// protected path — such a pair never gets past step 2, which is lexical. That
// property is now a fact about pure functions rather than something that
// depends on a probe answering correctly.
func GuardContainsOrEqual(dir, target string) bool { return guardContainsOrEqual(dir, target) }

// GuardPathsIntersect reports whether either of a, b contains-or-equals the
// other under GuardContainsOrEqual. It is the guard-biased counterpart of
// pathsIntersect and carries the same refuse-on-true bias.
func GuardPathsIntersect(a, b string) bool {
	return guardContainsOrEqual(a, b) || guardContainsOrEqual(b, a)
}

func guardContainsOrEqual(dir, target string) bool {
	dir = filepath.Clean(dir)
	target = filepath.Clean(target)
	if pathContainsOrEqual(dir, target) {
		return true
	}
	// Spelling equivalence is a question about real directories, and only an
	// absolute path names one. Production always passes absolute paths
	// (cleanDirectoryPath requires it), so this is a precondition guard, and
	// answering it lexically keeps the documented "collapses to
	// PathContainsOrEqual" property true for every caller.
	if !filepath.IsAbs(dir) || !filepath.IsAbs(target) {
		return false
	}
	// No folded relation at all is a definitive answer: no filesystem spelling
	// rule can make these two paths name one tree, so no I/O is warranted.
	if !pathContainsOrEqual(foldGuardPath(dir), foldGuardPath(target)) {
		return false
	}
	// A folded relation holds. Compare dir against the segment prefix of target
	// at dir's own depth — that prefix is the only part of target dir could be.
	//
	// Note this means the possibly-missing TAIL of target never participates in
	// the comparison: the Codex cwd guard asking about "/Users/Dev" against
	// "/Users/dev/.tclaude/data" compares it against "/Users/dev", and both of
	// those normally exist. That is why step 4 is rarely reached in practice.
	prefix, ok := guardPathPrefix(dir, target)
	if !ok {
		return true
	}
	return guardSpellingsAlias(dir, prefix)
}

// guardSpellingsAlias reports whether two case/NFC-folded-equal path spellings
// really name one directory.
//
// Filesystem identity is the only evidence accepted. Every failure to obtain it
// — a path that does not exist yet, an unreadable ancestor, a dangling symlink,
// a symlink loop — refuses, because a folded nomination that cannot be refuted
// is treated as a collision. Collapsing all of those into one branch is
// deliberate: the previous version distinguished them, and the distinction is
// where two of this work's fail-opens lived.
//
// os.Stat rather than os.Lstat: a final component that is a SYMLINK to the
// other spelling has its own inode, so lstat would see two objects where the
// filesystem resolves to one, and the guard would fail open. Following the link
// is the fail-closed direction here — the worst it can do is merge two
// spellings that genuinely reach one directory, which is what we want to refuse.
func guardSpellingsAlias(a, b string) bool {
	aInfo, err := os.Stat(a)
	if err != nil {
		return true
	}
	bInfo, err := os.Stat(b)
	if err != nil {
		return true
	}
	return os.SameFile(aInfo, bInfo)
}

// foldGuardPath is the nomination key: case- and NFC-folded. It matches
// session.seatbeltFoldedPath byte for byte (ToLower first, then NFC) so the
// validator nominates exactly the spellings the Seatbelt emitter does.
//
// Known residual, tracked as TCL-985 and NOT closed by this file: strings.ToLower
// is Unicode simple lowercasing, not APFS/HFS+'s own case-folding table. A rune
// where the two disagree (Greek final sigma ς/σ, U+0130 İ) fails to nominate a
// pair the volume does in fact merge, and step 2 then answers false with no I/O.
// Fixing it means moving to full case folding (x/text/cases.Fold) in the
// validator AND the Seatbelt emitter at once — a validator that nominated a
// different set of spellings than the emitter merges would put the two out of
// step, which is the class of bug this work exists to remove.
// TestFoldGuardPathMatchesTheSeatbeltEmitterRule pins them together so they
// cannot drift apart by accident.
func foldGuardPath(path string) string {
	return normalizeNFC(strings.ToLower(filepath.Clean(path)))
}

// normalizeNFC is the single NFC entry point for this package, so the validator
// and session.seatbeltFoldedPath cannot drift apart on which form they fold to.
func normalizeNFC(s string) string { return norm.NFC.String(s) }

// guardPathParts splits a cleaned absolute path into its segments.
func guardPathParts(path string) []string {
	path = filepath.Clean(path)
	if path == string(filepath.Separator) {
		return nil
	}
	return strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
}

// guardPathPrefix returns the prefix of target that has dir's segment depth —
// the only candidate spelling of target that dir could be an alias of. It
// reports false when target is shallower than dir, which the caller treats as
// unestablished rather than as a refutation. Folding preserves segment count,
// so that case is unreachable once folded containment holds; it is a safety net
// rather than a live branch.
func guardPathPrefix(dir, target string) (string, bool) {
	dirParts := guardPathParts(dir)
	targetParts := guardPathParts(target)
	if len(dirParts) > len(targetParts) {
		return "", false
	}
	if len(dirParts) == 0 {
		return string(filepath.Separator), true
	}
	return filepath.Join(append(
		[]string{string(filepath.Separator)},
		targetParts[:len(dirParts)]...,
	)...), true
}
