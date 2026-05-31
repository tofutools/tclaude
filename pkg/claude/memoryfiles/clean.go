package memoryfiles

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// CleanParams configures `memory-files clean`.
type CleanParams struct {
	Dir string `pos:"true" optional:"true" help:"Project directory whose memory files to clean (defaults to current directory)"`

	Include []string `long:"include" optional:"true" help:"Glob selecting .md files to DELETE, matched against the file name (e.g. 'feedback_*', '*'). Repeatable. Default: every .md file."`
	Exclude []string `long:"exclude" optional:"true" help:"Glob selecting .md files to KEEP; overrides --include (e.g. 'MEMORY.md', 'project_*'). Repeatable."`

	// DryRun is declared before NoSiblings on purpose: boa's auto-short
	// enricher assigns the first free initial letter, and we want the
	// explicit -n to bind to --dry-run (matching `conv prune-empty`).
	// Declaring it first means --no-siblings sees 'n' already taken and
	// gets no short, instead of greedily grabbing -n itself.
	Yes    bool `short:"y" long:"yes" help:"Skip the confirmation prompt and delete."`
	DryRun bool `short:"n" long:"dry-run" help:"Show what would be deleted and kept, without deleting anything."`

	Prefix     bool `long:"prefix" help:"Scan sibling project dirs by encoded-name prefix instead of git worktrees. Catches leftover memory from removed worktrees, but may also match child dirs (<dir>/sub) and dotted siblings (<dir>.bak)."`
	NoSiblings bool `long:"no-siblings" help:"Clean only the exact project dir; ignore worktrees and prefix siblings. Takes precedence over --prefix."`
}

// CleanCmd returns the `memory-files clean` subcommand.
func CleanCmd() *cobra.Command {
	return boa.CmdT[CleanParams]{
		Use:   "clean",
		Short: "Delete memory files for a project (and its worktree siblings)",
		Long: "Delete Claude Code memory files for a project directory.\n\n" +
			"Sibling scan strategy (which ~/.claude/projects dirs are touched):\n" +
			"  default      the target repo's live git worktrees (precise).\n" +
			"  --prefix     every dir whose encoded name is prefixed by the target's\n" +
			"               (catches removed-worktree leftovers; may over-match child\n" +
			"               dirs / dotted siblings).\n" +
			"  --no-siblings  only the exact project dir.\n\n" +
			"Only top-level .md files directly in each memory/ dir are considered —\n" +
			"subdirectories (e.g. a stray .idea/) are never traversed. Those .md files\n" +
			"are classified using --include / --exclude globs: a file is deleted when it\n" +
			"matches an --include (or none were given, meaning all) AND no --exclude. The\n" +
			"full to-delete / to-keep split is printed before anything is removed;\n" +
			"without --dry-run or --yes you are asked to confirm.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *CleanParams, _ *cobra.Command, _ []string) {
			if code := RunClean(params, os.Stdout, os.Stderr, os.Stdin); code != 0 {
				os.Exit(code)
			}
		},
	}.ToCobra()
}

// RunClean is the testable core of `memory-files clean`. It returns a
// process exit code and writes all user-facing output through the
// provided streams so tests can drive it without touching os.Std*.
func RunClean(params *CleanParams, stdout, stderr, stdin *os.File) int {
	// Validate globs up front, before touching anything: matchesAny
	// treats a non-matching and a *malformed* pattern identically, so a
	// typo'd --exclude (meant to keep files) would otherwise silently
	// fail to protect them and they'd be deleted.
	if err := validateGlobs(params.Include, params.Exclude); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	targetDir, err := resolveTargetDir(params.Dir)
	if err != nil {
		fmt.Fprintf(stderr, "Error getting current directory: %v\n", err)
		return 1
	}

	res, err := resolveProjectDirs(targetDir, scanModeFrom(params.NoSiblings, params.Prefix))
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	if res.note != "" {
		fmt.Fprintf(stderr, "Note: %s\n", res.note)
	}
	if len(res.dirs) == 0 {
		fmt.Fprintf(stdout, "No Claude project directories found for %s (encoded: %s).\n", targetDir, res.encoded)
		return 0
	}

	files := gatherMemoryFiles(res.dirs, params.Include, params.Exclude)
	if len(files) == 0 {
		fmt.Fprintf(stdout, "No memory files found under %d matched project dir(s).\n", len(res.dirs))
		return 0
	}

	printPreview(stdout, files)

	toDelete := selectForDeletion(files)
	if len(toDelete) == 0 {
		fmt.Fprintf(stdout, "\nNothing matches the delete filters — nothing to do.\n")
		return 0
	}

	// Work out which MEMORY.md index entries the deletion will leave dangling,
	// so we can show them in the preview and tidy them up afterwards. Computed
	// against the to-delete set (the files still exist on disk right now), so
	// the preview is accurate before anything is removed.
	delByDir, indexDeletedByDir := groupDeletions(toDelete)
	idxPlans := planIndexPrune(delByDir, indexDeletedByDir, stderr)
	if len(idxPlans) > 0 {
		idxTotal := 0
		for _, p := range idxPlans {
			idxTotal += len(p.entries)
		}
		fmt.Fprintf(stdout, "\nMEMORY.md %s to prune (dangling after deletion):\n", nEntries(idxTotal))
		printIndexPlan(stdout, idxPlans)
	}

	if params.DryRun {
		fmt.Fprintf(stdout, "\nDry run — no files deleted, no index entries pruned.\n")
		return 0
	}

	if !params.Yes {
		fmt.Fprintf(stdout, "\nDelete these %d file(s)? [y/N]: ", len(toDelete))
		reader := bufio.NewReader(stdin)
		// A closed/empty stdin (EOF) yields "" → treated as "no" below,
		// so the read error is intentionally ignored: it fails safe to abort.
		resp, _ := reader.ReadString('\n')
		resp = strings.TrimSpace(strings.ToLower(resp))
		if resp != "y" && resp != "yes" {
			fmt.Fprintf(stdout, "Aborted.\n")
			return 0
		}
	}

	deleted, failed := 0, 0
	deletedByDir := map[string]map[string]bool{}
	for _, f := range toDelete {
		if err := os.Remove(f.abs); err != nil {
			fmt.Fprintf(stderr, "Error deleting %s: %v\n", f.abs, err)
			failed++
			continue
		}
		deleted++
		if deletedByDir[f.memoryDir] == nil {
			deletedByDir[f.memoryDir] = map[string]bool{}
		}
		deletedByDir[f.memoryDir][f.rel] = true
	}

	// Tidy each modified index. Removal is driven by the names we ACTUALLY
	// deleted (not the intended set — a failed os.Remove must not orphan its
	// still-present file from the index), with treatMissingAsGone=false so we
	// only drop entries for files clean removed, leaving any pre-existing stale
	// entries for prune-index. We do NOT pre-skip dirs whose MEMORY.md was
	// targeted for deletion: pruneIndexFile is missing-safe (a deleted index
	// just yields no entries), and intent-based skipping would wrongly leave a
	// MEMORY.md whose OWN deletion FAILED still holding its deleted siblings'
	// stale entries.
	prunedEntries := 0
	for memDir, set := range deletedByDir {
		removed, perr := pruneIndexFile(memDir, set, false, false)
		if perr != nil {
			fmt.Fprintf(stderr, "Error pruning %s: %v\n", filepath.Join(memDir, memoryIndexFile), perr)
			failed++
			continue
		}
		prunedEntries += len(removed)
	}

	removedDirs := pruneEmptyMemoryDirs(res.dirs)

	fmt.Fprintf(stdout, "Deleted %d file(s)", deleted)
	if prunedEntries > 0 {
		fmt.Fprintf(stdout, ", pruned %s from MEMORY.md", nEntries(prunedEntries))
	}
	if removedDirs > 0 {
		fmt.Fprintf(stdout, ", removed %d empty memory dir(s)", removedDirs)
	}
	fmt.Fprintf(stdout, ".\n")

	// Surface partial failure to scripted callers: a clean that couldn't
	// remove everything it listed should not look like a success.
	if failed > 0 {
		fmt.Fprintf(stderr, "Failed to delete %d file(s).\n", failed)
		return 1
	}
	return 0
}

// groupDeletions buckets the to-delete files by their memory dir, returning
// the set of file names being deleted per dir and a flag per dir for whether
// MEMORY.md itself is among them (in which case there's no index left to
// tidy).
func groupDeletions(toDelete []memFile) (delByDir map[string]map[string]bool, indexDeletedByDir map[string]bool) {
	delByDir = map[string]map[string]bool{}
	indexDeletedByDir = map[string]bool{}
	for _, f := range toDelete {
		if delByDir[f.memoryDir] == nil {
			delByDir[f.memoryDir] = map[string]bool{}
		}
		delByDir[f.memoryDir][f.rel] = true
		if strings.EqualFold(f.rel, memoryIndexFile) {
			indexDeletedByDir[f.memoryDir] = true
		}
	}
	return delByDir, indexDeletedByDir
}

// planIndexPrune computes, per modified memory dir, which MEMORY.md entries
// the deletion would leave dangling — treating the to-delete set as already
// gone so the result is accurate before any file is removed. Dirs whose
// MEMORY.md is itself being deleted are skipped. Plans are returned sorted by
// dir for deterministic output; read errors are surfaced to stderr.
func planIndexPrune(delByDir map[string]map[string]bool, indexDeletedByDir map[string]bool, stderr *os.File) []indexPrunePlan {
	var plans []indexPrunePlan
	for memDir, set := range delByDir {
		if indexDeletedByDir[memDir] {
			continue
		}
		// treatMissingAsGone=false: clean removes only entries for the files
		// IT is deleting (carried in `set`), never unrelated stale entries —
		// those are prune-index's job.
		entries, err := pruneIndexFile(memDir, set, false, true) // dry-run: just compute
		if err != nil {
			fmt.Fprintf(stderr, "Error reading %s: %v\n", filepath.Join(memDir, memoryIndexFile), err)
			continue
		}
		if len(entries) > 0 {
			plans = append(plans, indexPrunePlan{memDir: memDir, entries: entries})
		}
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].memDir < plans[j].memDir })
	return plans
}

// gatherMemoryFiles lists the project dirs' top-level .md memory files
// (see listMemoryMD) and tags each with the clean classification — a
// file is marked for deletion when it matches an --include glob (or no
// includes were given) and no --exclude glob.
func gatherMemoryFiles(projectDirs, includes, excludes []string) []memFile {
	files := listMemoryMD(projectDirs)
	for i := range files {
		files[i].del = classify(files[i].rel, includes, excludes)
	}
	return files
}

// classify reports whether a memory file (identified by its file name)
// should be DELETED. A file is deleted when it matches an --include
// glob — or no includes were given, meaning "all .md files" — AND does
// not match any --exclude glob. Exclusions always win over inclusions.
func classify(name string, includes, excludes []string) bool {
	if matchesAny(excludes, name) {
		return false
	}
	if len(includes) == 0 {
		return true
	}
	return matchesAny(includes, name)
}

// matchesAny reports whether name matches any of the glob patterns
// (filepath.Match semantics, against the bare file name). It assumes the
// patterns were already vetted by validateGlobs, so a discarded
// ErrBadPattern here is unreachable in practice.
func matchesAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, _ := filepath.Match(p, name); ok {
			return true
		}
	}
	return false
}

// validateGlobs returns an error if any pattern is not a valid
// filepath.Match glob (e.g. an unclosed "[" character class). filepath.
// Match reports such patterns with ErrBadPattern; we surface it instead
// of letting matchesAny silently treat the pattern as non-matching.
func validateGlobs(groups ...[]string) error {
	for _, g := range groups {
		for _, p := range g {
			if _, err := filepath.Match(p, "x"); err != nil {
				return fmt.Errorf("invalid glob pattern %q: %w", p, err)
			}
		}
	}
	return nil
}

// selectForDeletion returns the subset of files classified for deletion.
func selectForDeletion(files []memFile) []memFile {
	var out []memFile
	for _, f := range files {
		if f.del {
			out = append(out, f)
		}
	}
	return out
}

// printPreview prints the to-delete / to-keep split grouped by project
// dir, followed by a summary line.
func printPreview(w *os.File, files []memFile) {
	byDir := map[string][]memFile{}
	var order []string
	for _, f := range files {
		if _, seen := byDir[f.projectDir]; !seen {
			order = append(order, f.projectDir)
		}
		byDir[f.projectDir] = append(byDir[f.projectDir], f)
	}

	fmt.Fprintf(w, "Memory files across %d project dir(s):\n", len(order))

	var delCount, keepCount int
	for _, pd := range order {
		fmt.Fprintf(w, "\n%s\n", pd)
		for _, f := range byDir[pd] {
			marker := "keep"
			if f.del {
				marker = "DEL "
				delCount++
			} else {
				keepCount++
			}
			fmt.Fprintf(w, "  [%s] memory/%s\n", marker, f.rel)
		}
	}
	fmt.Fprintf(w, "\nSummary: %d to delete, %d to keep.\n", delCount, keepCount)
}

// pruneEmptyMemoryDirs removes any memory/ dir that is now empty after
// deletion, returning how many it removed. A dir that still holds kept
// files (or non-empty subdirs) is left untouched.
func pruneEmptyMemoryDirs(projectDirs []string) int {
	removed := 0
	for _, pd := range projectDirs {
		memDir := filepath.Join(pd, "memory")
		entries, err := os.ReadDir(memDir)
		if err != nil {
			continue
		}
		if len(entries) == 0 {
			if err := os.Remove(memDir); err == nil {
				removed++
			}
		}
	}
	return removed
}
