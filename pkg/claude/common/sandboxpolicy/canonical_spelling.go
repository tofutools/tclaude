package sandboxpolicy

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxSpellingRestoreEntries bounds the directory scan below. A directory with
// more entries than this is left un-restored rather than scanned: the guard
// layer (GuardContainsOrEqual) still catches a folded collision there, and a
// safety validator must not turn into an unbounded readdir on a path an
// operator chose. The cap is far above any plausible $HOME or ancestor of a
// protected root.
// It is a var rather than a const solely so the test can lower it and exercise
// the abandon path against a small staged directory instead of staging 50k
// entries. Production never assigns to it.
var maxSpellingRestoreEntries = 50000

// spellingRestoreChunk is how many directory entries are pulled per read. Large
// enough that an ordinary directory takes one syscall, small enough that a
// pathological one is abandoned after reading a few hundred entries rather than
// all of them.
const spellingRestoreChunk = 512

// CanonicalHostSpelling returns path rewritten so every existing component
// carries its real on-disk spelling.
//
// This is the canonicalization half of the case-insensitive containment
// contract. filepath.EvalSymlinks resolves links but is otherwise lexical, so
// it hands back whatever casing (and whatever Unicode normalization form) the
// caller supplied. On a case-insensitive volume that leaves two spellings of
// one directory looking like two directories to every lexical comparison
// downstream: the protected-root wall misses one of them, and two grants naming
// one physical directory never fold together, so deny-dominance in the grant
// lattice does not collapse them.
//
// Running every path — operator-authored grants AND the protected roots they
// are checked against — through this function makes those lexical comparisons
// answer correctly again, because both sides arrive spelled the same way.
//
// It runs UNCONDITIONALLY — there is no filesystem-semantics probe in front of
// it — because the restoration is self-verifying and is a provable no-op on a
// case-sensitive volume:
//
//   - a component is only rewritten when the directory actually holds exactly
//     one entry that folds onto it AND the authored spelling was never seen as
//     a literal entry;
//   - on a case-sensitive directory a spelling that resolves must also be
//     listed, so the literal-entry case always wins and nothing is rewritten.
//
// An earlier version gated this on an empirical "does this volume fold?" probe.
// That probe was a pure optimization, and it was the only part that could get
// the answer WRONG: a mistaken "does not fold" silently skipped restoration, so
// two case-variant grants stayed unfolded and deny-dominance never collapsed
// them. Deleting it removes that failure mode along with the probe.
//
// It is conservative in every other direction:
//   - It never creates, moves, or modifies anything.
//   - Any component it cannot resolve — nonexistent, unreadable, ambiguous, or
//     inside an implausibly large directory — ends the restoration, and the
//     remainder is re-attached exactly as authored. Callers must therefore
//     treat this as a normalization that improves comparisons, never as a
//     containment proof on its own; that is GuardContainsOrEqual's job.
func CanonicalHostSpelling(path string) string {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return clean
	}
	return restoreSpelling(clean)
}

// restoreSpelling walks path from the root, replacing each component with the
// on-disk entry whose folded name matches it.
func restoreSpelling(path string) string {
	parts := guardPathParts(path)
	current := string(filepath.Separator)
	for i, part := range parts {
		real, ok := onDiskSpelling(current, part)
		if !ok {
			// Stop here and re-attach the rest as authored: an unresolvable
			// component means every component below it is unresolvable too.
			return filepath.Join(append([]string{current}, parts[i:]...)...)
		}
		current = filepath.Join(current, real)
	}
	return current
}

// onDiskSpelling returns the real name of the entry in dir that folds to name.
//
// Finding the stored spelling of a name inherently requires enumerating the
// directory — there is no portable "what is this entry really called" call — so
// this reads incrementally rather than slurping. That matters for two reasons:
// an exact byte match (the common case even on a folding volume) stops the read
// as soon as it is seen instead of after the whole directory, and the entry cap
// bounds the WORK rather than merely the decision. os.ReadDir would defeat both:
// it reads every entry and sorts them before the caller sees anything, so a
// directory with millions of entries would be fully materialized just to be
// rejected — not something a safety validator may do on an operator-chosen path.
func onDiskSpelling(dir, name string) (string, bool) {
	result := scanForSpelling(dir, name)
	return result.name, result.ok
}

// spellingScan is onDiskSpelling's outcome, broken out so tests can assert on
// WHY a scan produced no restoration. Without that distinction, "abandoned at
// the entry cap" and "scanned everything and found nothing" are indistinguishable
// from the outside — which made an earlier cap test pass with the cap
// enforcement deleted.
type spellingScan struct {
	name string
	ok   bool
	// abandoned reports that the scan stopped at maxSpellingRestoreEntries with
	// entries still unread.
	abandoned bool
	// scanned counts entries examined, excluding an exact match that
	// short-circuits.
	scanned int
}

func scanForSpelling(dir, name string) spellingScan {
	if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
		// The component does not exist (or is unreachable): nothing to restore.
		return spellingScan{}
	}
	handle, err := os.Open(dir)
	if err != nil {
		return spellingScan{}
	}
	defer func() { _ = handle.Close() }()

	folded := foldSpellingComponent(name)
	match := ""
	ambiguous := false
	scanned := 0
	for {
		entries, readErr := handle.ReadDir(spellingRestoreChunk)
		for _, entry := range entries {
			if entry.Name() == name {
				// The authored spelling IS an entry of this directory, so it is
				// already the stored spelling and there is nothing to restore.
				// This is decisive even when folded siblings were seen earlier
				// in the scan — an exact entry is never ambiguous — and it ends
				// the read immediately.
				return spellingScan{name: name, ok: true, scanned: scanned}
			}
			scanned++
			if scanned > maxSpellingRestoreEntries {
				return spellingScan{abandoned: true, scanned: scanned}
			}
			if foldSpellingComponent(entry.Name()) != folded {
				continue
			}
			if match != "" {
				// Two entries fold together, so this directory does not in fact
				// fold them — the volume probe answered for a different mount,
				// or for a different directory on a mixed-semantics tree. Note
				// it but keep scanning: an exact entry may still appear and
				// settle the question decisively.
				ambiguous = true
				continue
			}
			match = entry.Name()
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return spellingScan{scanned: scanned}
		}
		if len(entries) == 0 {
			break
		}
	}
	if ambiguous || match == "" {
		// Refuse to guess: a WRONG restoration would rewrite a persisted grant
		// to name a different directory than the operator authored, which is far
		// worse than leaving the spelling alone.
		return spellingScan{scanned: scanned}
	}
	return spellingScan{name: match, ok: true, scanned: scanned}
}

// foldSpellingComponent is foldGuardPath for a single component. It deliberately
// skips filepath.Clean, which would mangle a component that looks like "..".
func foldSpellingComponent(name string) string {
	return normalizeNFC(strings.ToLower(name))
}
