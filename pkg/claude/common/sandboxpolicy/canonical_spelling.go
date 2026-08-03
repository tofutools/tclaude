package sandboxpolicy

import (
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
const maxSpellingRestoreEntries = 50000

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
// It is conservative in every direction:
//   - On a volume that does not fold spellings (every ordinary Linux
//     filesystem, and case-sensitive APFS) it returns path unchanged after one
//     cheap probe, so behavior there is byte-for-byte what it was.
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
	anchor, err := nearestExistingDir(clean)
	if err != nil {
		return clean
	}
	// One probe decides whether any of this work is worth doing. On a
	// case-sensitive volume this is where the function stops.
	folds, err := volumeFoldsCase(anchor)
	if err != nil {
		return clean
	}
	if !folds {
		if normFolds, normErr := volumeFoldsNormalization(anchor); normErr != nil || !normFolds {
			return clean
		}
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
// An exact byte match wins immediately and costs no scan, which is the common
// case even on a folding volume. Only a name that is not already spelled the
// way the filesystem stores it falls through to the directory scan.
func onDiskSpelling(dir, name string) (string, bool) {
	if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
		// The component does not exist (or is unreachable): nothing to restore.
		return "", false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	if len(entries) > maxSpellingRestoreEntries {
		return "", false
	}
	folded := foldSpellingComponent(name)
	match := ""
	for _, entry := range entries {
		if entry.Name() == name {
			return name, true // already the on-disk spelling
		}
		if foldSpellingComponent(entry.Name()) != folded {
			continue
		}
		if match != "" {
			// Two entries fold together, so this directory's volume does not
			// actually fold them — the earlier probe answered for a different
			// mount. Refuse to guess.
			return "", false
		}
		match = entry.Name()
	}
	if match == "" {
		return "", false
	}
	return match, true
}

// foldSpellingComponent is foldGuardPath for a single component. It deliberately
// skips filepath.Clean, which would mangle a component that looks like "..".
func foldSpellingComponent(name string) string {
	return normalizeNFC(strings.ToLower(name))
}
