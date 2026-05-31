package memoryfiles

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// LsParams configures `memory-files ls`.
type LsParams struct {
	Dir        string `pos:"true" optional:"true" help:"Project directory whose memory files to list (defaults to current directory)"`
	NoSiblings bool   `long:"no-siblings" help:"List only the exact project dir; skip worktree-sibling project dirs that share its encoded prefix."`
}

// LsCmd returns the `memory-files ls` subcommand.
func LsCmd() *cobra.Command {
	return boa.CmdT[LsParams]{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List a project's memory files (and its worktree siblings')",
		Long: "List the top-level .md memory files for a project directory and, by\n" +
			"default, every worktree-sibling project dir sharing its encoded prefix\n" +
			"(pass --no-siblings to restrict to the exact dir). Shows each file's size\n" +
			"and last-modified time, grouped by project dir.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *LsParams, _ *cobra.Command, _ []string) {
			if code := RunLs(params, os.Stdout, os.Stderr); code != 0 {
				os.Exit(code)
			}
		},
	}.ToCobra()
}

// RunLs is the testable core of `memory-files ls`.
func RunLs(params *LsParams, stdout, stderr *os.File) int {
	targetDir, err := resolveTargetDir(params.Dir)
	if err != nil {
		fmt.Fprintf(stderr, "Error getting current directory: %v\n", err)
		return 1
	}

	projectDirs, encoded, err := resolveProjectDirs(targetDir, !params.NoSiblings)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	if len(projectDirs) == 0 {
		fmt.Fprintf(stdout, "No Claude project directories found for %s (encoded: %s).\n", targetDir, encoded)
		return 0
	}

	files := listMemoryMD(projectDirs)
	if len(files) == 0 {
		fmt.Fprintf(stdout, "No memory files found under %d matched project dir(s).\n", len(projectDirs))
		return 0
	}

	// Group by project dir, preserving the (sorted) order listMemoryMD
	// returns.
	byDir := map[string][]memFile{}
	var order []string
	for _, f := range files {
		if _, seen := byDir[f.projectDir]; !seen {
			order = append(order, f.projectDir)
		}
		byDir[f.projectDir] = append(byDir[f.projectDir], f)
	}

	var totalBytes int64
	for i, pd := range order {
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		fmt.Fprintf(stdout, "%s  (%d file(s))\n", pd, len(byDir[pd]))
		tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
		for _, f := range byDir[pd] {
			totalBytes += f.size
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", f.rel, humanizeBytes(f.size), f.modTime.Format("2006-01-02 15:04"))
		}
		_ = tw.Flush()
	}

	fmt.Fprintf(stdout, "\nTotal: %d file(s) across %d project dir(s), %s.\n", len(files), len(order), humanizeBytes(totalBytes))
	return 0
}

// humanizeBytes renders a byte count compactly (B / KB / MB), enough
// for the small markdown files memory holds.
func humanizeBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}
