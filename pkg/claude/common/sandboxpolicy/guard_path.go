package sandboxpolicy

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

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
// The rule below is the same one session.seatbeltPathContains already applies
// when it compiles Seatbelt regions: a case/NFC-folded relation is only a
// NOMINATION, and it must be confirmed against real filesystem identity before
// it changes an answer. The emitter had this; the validator did not. That
// asymmetry is what this file closes.

// errSpellingProbeUnavailable reports that the filesystem's spelling-equivalence
// semantics could not be established. Guards treat it as a refusal, never as an
// approval.
var errSpellingProbeUnavailable = errors.New("cannot establish filesystem spelling equivalence")

// GuardContainsOrEqual reports whether dir is target or an ancestor of it, for
// the purpose of a sandbox SAFETY GUARD where a true answer means "refuse".
//
// It is deliberately biased. Beyond byte-exact containment it also accepts a
// case/NFC-folded relation, but only once that relation is confirmed — either
// because the two spellings resolve to the same filesystem object, or because
// the governing volume is shown to fold that class of spelling difference. When
// neither can be established (an unreadable path, an unprobeable volume), it
// answers TRUE: an unprovable non-relation is treated as a relation, so an
// operator gets a refusal to argue with rather than a silent hole.
//
// That bias is why this must never be used where a true answer grants access.
// For lexical lattice work on already-canonical paths, use PathContainsOrEqual.
//
// On a case-sensitive volume — every ordinary Linux filesystem, and
// case-sensitive APFS — the fold relation is refuted by file identity or by the
// volume probe, and this collapses to exactly PathContainsOrEqual's answer.
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
	// absolute path names one. A relative path would otherwise have an absolute
	// candidate prefix fabricated for it below and get probed against the wrong
	// tree entirely. Production always passes absolute paths (cleanDirectoryPath
	// requires it), so this is a precondition guard, and answering it lexically
	// is what keeps the documented "collapses to PathContainsOrEqual" property
	// true for every caller rather than only the well-behaved ones.
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
	prefix, ok := guardPathPrefix(dir, target)
	if !ok {
		return true
	}
	return guardSpellingsAlias(dir, prefix)
}

// guardSpellingsAlias reports whether two case/NFC-folded-equal path spellings
// really name one directory. File identity is authoritative when both spellings
// exist; otherwise the volume's own spelling semantics decide, and an
// unestablished answer refuses.
func guardSpellingsAlias(a, b string) bool {
	// os.Stat, not os.Lstat: a final component that is a SYMLINK to the other
	// spelling has its own inode, so lstat would report two objects where the
	// filesystem resolves to one and the guard would fail open. Following the
	// link is the fail-closed direction for a refusal test — the worst it can do
	// is merge two spellings that reach one directory, which is exactly what we
	// want to refuse. Callers normally resolve symlinks before reaching here
	// (canonicalDirectory and resolveSymlinks both run EvalSymlinks first); this
	// removes the reliance on them having done so.
	aInfo, aErr := os.Stat(a)
	bInfo, bErr := os.Stat(b)
	// The fallback is ONLY for a dangling symlink, which stats as missing while
	// genuinely naming an entry — without it such a spelling would fall through
	// to the not-yet-created branch below. It must be gated per side and only on
	// ENOENT: retrying an EACCES or ELOOP failure with lstat would succeed on the
	// symlink itself, hand SameFile two link inodes instead of the one directory
	// they resolve to, and answer false = ALLOW. That would also make the
	// unreadable-means-refuse rule underneath unreachable for a symlinked final
	// component, which is precisely the shape this Stat call exists to catch.
	if aErr != nil && os.IsNotExist(aErr) {
		if lInfo, lErr := os.Lstat(a); lErr == nil {
			aInfo, aErr = lInfo, nil
		}
	}
	if bErr != nil && os.IsNotExist(bErr) {
		if lInfo, lErr := os.Lstat(b); lErr == nil {
			bInfo, bErr = lInfo, nil
		}
	}
	if aErr == nil && bErr == nil {
		// Authoritative on every platform, including a case-insensitive mount
		// on Linux: one inode means one directory, two means two.
		return os.SameFile(aInfo, bInfo)
	}
	if (aErr != nil && !os.IsNotExist(aErr)) || (bErr != nil && !os.IsNotExist(bErr)) {
		return true // unreadable: cannot establish, so refuse
	}

	// At least one spelling does not exist yet — a grant may legitimately name a
	// directory the launch will create. Identity is unavailable, so ask the
	// volume that would host it whether it folds the differences in play.
	anchorSeed := a
	if aErr != nil {
		anchorSeed = b
	}
	anchor, err := nearestExistingDir(anchorSeed)
	if err != nil {
		return true
	}
	if guardCaseDiffers(a, b) {
		folds, err := volumeFoldsCase(anchor)
		if err != nil {
			return true
		}
		if !folds {
			return false
		}
	}
	if guardNormalizationDiffers(a, b) {
		folds, err := volumeFoldsNormalization(anchor)
		if err != nil {
			return true
		}
		if !folds {
			return false
		}
	}
	return true
}

// foldGuardPath is the nomination key: case- and NFC-folded. It matches
// session.seatbeltFoldedPath byte for byte (ToLower first, then NFC) so the
// validator nominates exactly the spellings the Seatbelt emitter does.
//
// Known residual: strings.ToLower is Unicode simple lowercasing, not APFS/HFS+'s
// own case-folding table. A rune where the two disagree (Greek final sigma ς/σ,
// U+0130 İ) would fail to nominate a pair the volume does in fact merge, and the
// zero-I/O rejection above would then answer false. Fixing that means moving to
// full case folding (x/text/cases.Fold), which MUST happen in both layers at
// once — a validator that nominated more spellings than the emitter merges, or
// fewer, would put the two out of step, and that disagreement is precisely the
// class of bug this work exists to remove. Tracked as a follow-up rather than
// changed here unilaterally; TestFoldGuardPathMatchesTheSeatbeltEmitterRule
// pins the two together so they cannot drift apart by accident.
func foldGuardPath(path string) string {
	return normalizeNFC(strings.ToLower(filepath.Clean(path)))
}

// normalizeNFC is the single NFC entry point for this package, so the validator
// and session.seatbeltFoldedPath cannot drift apart on which form they fold to.
func normalizeNFC(s string) string { return norm.NFC.String(s) }

// guardCaseDiffers reports whether a and b still differ once Unicode
// normalization is taken out of the picture — i.e. the residual difference is
// one of letter case.
func guardCaseDiffers(a, b string) bool {
	return norm.NFC.String(a) != norm.NFC.String(b)
}

// guardNormalizationDiffers reports whether a and b differ in Unicode
// normalization form, independently of any case difference.
func guardNormalizationDiffers(a, b string) bool {
	lowerA, lowerB := strings.ToLower(a), strings.ToLower(b)
	return lowerA != lowerB && norm.NFC.String(lowerA) == norm.NFC.String(lowerB)
}

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
// unestablished rather than as a refutation.
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

// nearestExistingDir walks up from path to the closest ancestor that exists, so
// a volume probe can be run on a real directory even when the grant names one
// that has not been created yet.
func nearestExistingDir(path string) (string, error) {
	for cur := filepath.Clean(path); ; {
		info, err := os.Lstat(cur)
		if err == nil {
			if info.IsDir() {
				return cur, nil
			}
			// A non-directory ancestor cannot host the probe; its parent can.
		} else if os.IsPermission(err) {
			// Traversal is denied, so no ancestor below this point can be
			// inspected either. Report it: the caller must fail closed rather
			// than probe some unrelated ancestor further up and trust that
			// answer for a directory it cannot see.
			return "", err
		}
		// Every other lstat failure — ENOENT for a path not created yet,
		// ENOTDIR for a path whose ancestor is a regular file, ELOOP for a
		// symlink cycle — means this candidate cannot be the anchor, so keep
		// walking up rather than giving up.
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", errSpellingProbeUnavailable
		}
		cur = parent
	}
}

// volumeFoldsCase reports whether the filesystem hosting dir treats
// case-variant spellings of a name as one name.
//
// The probe is empirical rather than a platform assumption, because the answer
// is a property of the VOLUME, not of the OS: macOS ships case-insensitive APFS
// by default but supports case-sensitive APFS, and Linux hosts case-insensitive
// mounts (vfat, ciopfs, ext4 with the casefold feature). It flips the case of an
// existing directory's own basename and asks whether that spelling reaches the
// same inode. It creates nothing and modifies nothing.
//
// A directory whose name has no cased letters cannot answer the question, so the
// probe walks up until one can. Reaching the root without an answer returns
// errSpellingProbeUnavailable, which guards read as a refusal.
func volumeFoldsCase(dir string) (bool, error) {
	return volumeFoldsSpelling(dir, flipCase)
}

// volumeFoldsSpelling is the shared probe body.
//
// It asks dir's OWN lookup semantics first, by respelling one of dir's existing
// child entries and testing whether that respelling reaches the same inode
// INSIDE dir. That is the only formulation that answers the question actually
// being asked, because folding is not always a property of the volume:
//
//   - ext4's casefold is a PER-DIRECTORY attribute (chattr +F, inherited by
//     children) on a single device, so a casefolded directory and its
//     case-sensitive parent share one st_dev.
//   - A directory that is a mount point has its own name stored in the parent's
//     filesystem, which may have entirely different semantics from its contents.
//
// The older formulation respelled dir's own BASENAME, which interrogates dir's
// parent. Where the two disagree that returns a definitive wrong answer, and a
// definitive wrong answer here makes a guard ALLOW.
//
// When dir has no child whose respelling differs (an empty directory, or one
// whose entries carry no case), the question cannot be answered from inside, and
// the probe falls back to the parent-based form — bounded by a device check, so
// it can never answer from a neighbouring filesystem. Anything still unanswered
// returns errSpellingProbeUnavailable, which guards read as a refusal.
func volumeFoldsSpelling(dir string, respell func(string) string) (bool, error) {
	if folds, err := foldsByOwnEntries(dir, respell); err == nil {
		return folds, nil
	} else if !errors.Is(err, errSpellingProbeUnavailable) {
		return false, err
	}
	return foldsByAncestorName(dir, respell)
}

// probeEntryScanLimit bounds the child scan. One getdents call's worth of
// entries is plenty to find a cased name in any real directory, and stopping
// there keeps the probe cheap enough to run per path.
const probeEntryScanLimit = 64

// foldsByOwnEntries answers from INSIDE dir: respell an entry dir already holds
// and see whether that spelling reaches the same inode within dir.
//
// It returns errSpellingProbeUnavailable when dir holds no entry whose
// respelling differs from itself, which is the only case the caller may fall
// back from.
func foldsByOwnEntries(dir string, respell func(string) string) (bool, error) {
	handle, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer func() { _ = handle.Close() }()

	entries, err := handle.ReadDir(probeEntryScanLimit)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	for _, entry := range entries {
		name := entry.Name()
		respelled := respell(name)
		if respelled == name {
			continue
		}
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			continue // raced away; try the next entry
		}
		otherInfo, err := os.Lstat(filepath.Join(dir, respelled))
		if err != nil {
			if os.IsNotExist(err) {
				// dir distinguishes the two spellings of its own entry.
				return false, nil
			}
			return false, err
		}
		return os.SameFile(info, otherInfo), nil
	}
	return false, errSpellingProbeUnavailable
}

// foldsByAncestorName is the fallback: respell a directory's own basename and
// look it up in the parent. This interrogates the PARENT's semantics, so every
// directory it touches must be on the same device as the directory asked about,
// and a device change ends the probe as indeterminate rather than answering from
// a neighbouring filesystem.
func foldsByAncestorName(dir string, respell func(string) string) (bool, error) {
	start := filepath.Clean(dir)
	startInfo, err := os.Lstat(start)
	if err != nil {
		return false, err
	}
	startDev, startDevOK := pathDevice(startInfo)

	sameVolume := func(path string) (bool, error) {
		info, err := os.Lstat(path)
		if err != nil {
			return false, err
		}
		dev, ok := pathDevice(info)
		if !startDevOK || !ok {
			// Without device identity the probe cannot prove it stayed on one
			// filesystem, and an unprovable answer must not become a definitive
			// "does not fold".
			return false, nil
		}
		return dev == startDev, nil
	}

	for cur := start; ; {
		base := filepath.Base(cur)
		parent := filepath.Dir(cur)
		if parent == cur {
			return false, errSpellingProbeUnavailable
		}
		if respelled := respell(base); respelled != base {
			info, err := os.Lstat(cur)
			if err != nil {
				return false, err
			}
			// The respelled name would be read out of `parent`, so `parent` is
			// the filesystem actually being questioned. It has to be the one we
			// were asked about.
			onSameVolume, err := sameVolume(parent)
			if err != nil {
				return false, err
			}
			if !onSameVolume {
				return false, errSpellingProbeUnavailable
			}
			otherInfo, err := os.Lstat(filepath.Join(parent, respelled))
			if err != nil {
				if os.IsNotExist(err) {
					// The respelling names nothing: the volume distinguishes it.
					return false, nil
				}
				return false, err
			}
			return os.SameFile(info, otherInfo), nil
		}
		// This component carries no case (or no normalization) to flip, so the
		// probe has to look further up — but only while it stays on the volume
		// the question was about.
		onSameVolume, err := sameVolume(parent)
		if err != nil {
			return false, err
		}
		if !onSameVolume {
			return false, errSpellingProbeUnavailable
		}
		cur = parent
	}
}

// flipCase inverts the case of every cased rune, so the respelling differs from
// the original whenever the name carries any case at all.
func flipCase(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsUpper(r):
			return unicode.ToLower(r)
		case unicode.IsLower(r):
			return unicode.ToUpper(r)
		default:
			return r
		}
	}, name)
}
