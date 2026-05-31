package memoryfiles

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
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

	if params.DryRun {
		fmt.Fprintf(stdout, "\nDry run — no files deleted.\n")
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

	deleted := 0
	for _, f := range toDelete {
		if err := os.Remove(f.abs); err != nil {
			fmt.Fprintf(stderr, "Error deleting %s: %v\n", f.abs, err)
			continue
		}
		deleted++
	}

	removedDirs := pruneEmptyMemoryDirs(res.dirs)

	fmt.Fprintf(stdout, "Deleted %d file(s)", deleted)
	if removedDirs > 0 {
		fmt.Fprintf(stdout, ", removed %d empty memory dir(s)", removedDirs)
	}
	fmt.Fprintf(stdout, ".\n")
	return 0
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
// (filepath.Match semantics, against the bare file name).
func matchesAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, _ := filepath.Match(p, name); ok {
			return true
		}
	}
	return false
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
