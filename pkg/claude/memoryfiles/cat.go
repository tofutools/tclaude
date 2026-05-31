package memoryfiles

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// memoryIndexFile is Claude's per-project memory index, printed first.
const memoryIndexFile = "MEMORY.md"

// CatParams configures `memory-files cat`.
type CatParams struct {
	Dir        string `pos:"true" optional:"true" help:"Project directory whose memory files to print (defaults to current directory)"`
	Prefix     bool   `long:"prefix" help:"Scan sibling project dirs by encoded-name prefix instead of git worktrees (may over-match child dirs / dotted siblings)."`
	NoSiblings bool   `long:"no-siblings" help:"Print only the exact project dir; ignore worktrees and prefix siblings. Takes precedence over --prefix."`
}

// CatCmd returns the `memory-files cat` subcommand.
func CatCmd() *cobra.Command {
	return boa.CmdT[CatParams]{
		Use:   "cat",
		Short: "Print a project's memory files (MEMORY.md first), with separators",
		Long: "Print the full contents of the top-level .md memory files for a project\n" +
			"directory and its sibling project dirs. By default siblings are the target\n" +
			"repo's live git worktrees; --prefix scans by encoded-name prefix instead,\n" +
			"and --no-siblings restricts to the exact dir.\n\n" +
			"MEMORY.md (the index) is printed first within each project dir, followed by\n" +
			"the rest alphabetically. Each file is introduced by a separator banner\n" +
			"naming it, so the concatenated output stays readable.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *CatParams, _ *cobra.Command, _ []string) {
			if code := RunCat(params, os.Stdout, os.Stderr); code != 0 {
				os.Exit(code)
			}
		},
	}.ToCobra()
}

// RunCat is the testable core of `memory-files cat`.
func RunCat(params *CatParams, stdout, stderr *os.File) int {
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

	files := listMemoryMD(res.dirs)
	if len(files) == 0 {
		fmt.Fprintf(stdout, "No memory files found under %d matched project dir(s).\n", len(res.dirs))
		return 0
	}
	orderMemoryIndexFirst(files)

	const bar = "================================================================================"
	for _, f := range files {
		fmt.Fprintf(stdout, "%s\n", bar)
		fmt.Fprintf(stdout, " memory/%s  —  %s\n", f.rel, f.projectDir)
		fmt.Fprintf(stdout, "%s\n", bar)
		content, readErr := os.ReadFile(f.abs)
		if readErr != nil {
			fmt.Fprintf(stderr, "Error reading %s: %v\n", f.abs, readErr)
			fmt.Fprintf(stdout, "<unreadable: %v>\n\n", readErr)
			continue
		}
		_, _ = stdout.Write(content)
		// Guarantee a trailing newline + blank line between files even
		// when a file does not end in a newline.
		if len(content) > 0 && !strings.HasSuffix(string(content), "\n") {
			fmt.Fprintln(stdout)
		}
		fmt.Fprintln(stdout)
	}
	return 0
}

// orderMemoryIndexFirst reorders files so that, within each project
// dir, MEMORY.md comes first and the rest stay alphabetical. The
// ordering is total — project dir asc, then index-first, then name asc.
func orderMemoryIndexFirst(files []memFile) {
	sort.Slice(files, func(i, j int) bool {
		if files[i].projectDir != files[j].projectDir {
			return files[i].projectDir < files[j].projectDir
		}
		iIdx := strings.EqualFold(files[i].rel, memoryIndexFile)
		jIdx := strings.EqualFold(files[j].rel, memoryIndexFile)
		if iIdx != jIdx {
			return iIdx // the index sorts before its siblings
		}
		return files[i].rel < files[j].rel
	})
}
