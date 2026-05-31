package memoryfiles

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// MEMORY.md is Claude's per-project memory index: a markdown list whose
// items each point at a sibling memory file — `- [Title](file.md) — hook`.
// When a memory file is deleted, its index line is left dangling (it links
// to a file that no longer exists). The helpers here find and remove those
// dangling list items while leaving every other line — headers, prose,
// blockquotes, links to URLs — exactly as written.
//
// memoryIndexFile (the "MEMORY.md" constant) is declared in cat.go.

// indexEntryRe matches a markdown list item that carries a link, capturing
// the FIRST link's target. It anchors on a list bullet (-, *, +) so headers,
// prose, and blockquotes in MEMORY.md are never candidates for removal, and
// the captured group is the target inside the first `[text](target)` — which
// for a canonical one-link-per-entry index is the linked memory file.
var indexEntryRe = regexp.MustCompile(`^\s*[-*+]\s+.*?\[[^\]]*\]\(([^)]*)\)`)

// danglingEntry is one index line slated for removal, kept for reporting.
type danglingEntry struct {
	line   string // the full original line (CR-trimmed) — shown in previews
	target string // the link target that is gone
}

// targetIsGone reports whether a captured link target denotes a local memory
// file that should be treated as gone. URLs, in-page anchors, and mail links
// are never gone — they aren't files we manage, so their lines are always
// kept.
//
// Two callers, two boundaries:
//   - clean wants ONLY entries for the files it is deleting. It passes those
//     file names in alsoMissing and treatMissingAsGone=false, so an unrelated
//     entry whose file happens to be absent is left alone. (alsoMissing also
//     keeps a dry-run preview accurate before the files leave disk.)
//   - prune-index wants every entry whose file is absent on disk, so it passes
//     treatMissingAsGone=true (and a nil alsoMissing).
func targetIsGone(memDir, rawTarget string, alsoMissing map[string]bool, treatMissingAsGone bool) bool {
	target := strings.TrimSpace(rawTarget)
	// Drop an optional markdown link title: `(path "Title")`.
	if i := strings.IndexAny(target, " \t"); i >= 0 {
		target = target[:i]
	}
	// Drop any #fragment / ?query suffix.
	if i := strings.IndexAny(target, "#?"); i >= 0 {
		target = target[:i]
	}
	if target == "" {
		return false
	}
	// Not a local file reference (scheme:// or mailto:) → never prune.
	if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
		return false
	}
	base := filepath.Base(target)
	if alsoMissing[base] || alsoMissing[target] {
		return true
	}
	if !treatMissingAsGone {
		return false
	}
	p := target
	if !filepath.IsAbs(p) {
		p = filepath.Join(memDir, target)
	}
	_, err := os.Stat(p)
	return err != nil
}

// pruneIndexContent removes every index-entry line whose link target is gone,
// returning the rewritten content and the removed entries. Untouched lines —
// and the presence/absence of a trailing newline — are preserved byte for
// byte; when nothing is removed the original content is returned unchanged.
func pruneIndexContent(content, memDir string, alsoMissing map[string]bool, treatMissingAsGone bool) (string, []danglingEntry) {
	hadTrailingNL := strings.HasSuffix(content, "\n")
	lines := strings.Split(content, "\n")
	// A trailing "\n" makes Split yield a final empty element; drop it so the
	// re-join is clean and we re-add the newline explicitly at the end.
	if hadTrailingNL {
		lines = lines[:len(lines)-1]
	}

	var kept []string
	var removed []danglingEntry
	for _, ln := range lines {
		if m := indexEntryRe.FindStringSubmatch(ln); m != nil && targetIsGone(memDir, m[1], alsoMissing, treatMissingAsGone) {
			removed = append(removed, danglingEntry{
				line:   strings.TrimRight(ln, "\r"),
				target: strings.TrimSpace(m[1]),
			})
			continue
		}
		kept = append(kept, ln)
	}

	if len(removed) == 0 {
		return content, nil
	}
	out := strings.Join(kept, "\n")
	if out != "" && hadTrailingNL {
		out += "\n"
	}
	return out, removed
}

// pruneIndexFile rewrites memDir/MEMORY.md in place (unless dryRun), removing
// dangling index entries, and returns the removed entries. A missing index is
// not an error — there is simply nothing to prune (returns nil, nil).
func pruneIndexFile(memDir string, alsoMissing map[string]bool, treatMissingAsGone, dryRun bool) ([]danglingEntry, error) {
	indexPath := filepath.Join(memDir, memoryIndexFile)
	content, err := os.ReadFile(indexPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	newContent, removed := pruneIndexContent(string(content), memDir, alsoMissing, treatMissingAsGone)
	if len(removed) == 0 || dryRun {
		return removed, nil
	}
	// Preserve the file's existing permission bits.
	mode := fs.FileMode(0o644)
	if info, statErr := os.Stat(indexPath); statErr == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(indexPath, []byte(newContent), mode); err != nil {
		return removed, err
	}
	return removed, nil
}

// indexPrunePlan pairs a memory dir's index with the entries that would be
// pruned from it — used by both `clean` (post-delete tidy) and `prune-index`.
type indexPrunePlan struct {
	memDir  string
	entries []danglingEntry
}

// printIndexPlan prints each index file followed by the entry lines that will
// be removed from it, for the confirm/preview step.
func printIndexPlan(w *os.File, plans []indexPrunePlan) {
	for _, p := range plans {
		fmt.Fprintf(w, "\n%s\n", filepath.Join(p.memDir, memoryIndexFile))
		for _, e := range p.entries {
			fmt.Fprintf(w, "  %s\n", e.line)
		}
	}
}

// nEntries renders a count with the correctly pluralised noun.
func nEntries(n int) string {
	if n == 1 {
		return "1 entry"
	}
	return fmt.Sprintf("%d entries", n)
}
