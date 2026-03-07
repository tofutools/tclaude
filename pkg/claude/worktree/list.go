package worktree

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/tofutools/tclaude/pkg/claude/common/table"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/spf13/cobra"
)

type ListParams struct {
	Verbose bool `short:"v" help:"Show additional details"`
}

func ListCmd() *cobra.Command {
	cmd := boa.CmdT[ListParams]{
		Use:         "ls",
		Short:       "List git worktrees",
		Long:        "List all git worktrees in the current repository.",
		Aliases:     []string{"list"},
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *ListParams, cmd *cobra.Command, args []string) {
			if err := runList(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()

	return cmd
}

func runList(params *ListParams) error {
	// Check we're in a git repo
	_, err := GetGitInfo()
	if err != nil {
		return err
	}

	worktrees, err := ListWorktrees()
	if err != nil {
		return err
	}

	if len(worktrees) == 0 {
		fmt.Println("No worktrees found.")
		return nil
	}

	// Get current directory to mark with *
	cwd, _ := os.Getwd()
	cwd, _ = filepath.EvalSymlinks(cwd) // Resolve symlinks for comparison

	// Find the longest path and branch for alignment
	maxPathLen := 0
	maxBranchLen := 0
	for _, wt := range worktrees {
		if len(wt.Path) > maxPathLen {
			maxPathLen = len(wt.Path)
		}
		b := wt.Branch
		if b == "" {
			b = "(detached)"
		}
		if wt.IsMain {
			b += " [root]"
		}
		if len(b) > maxBranchLen {
			maxBranchLen = len(b)
		}
	}

	// Cap path width based on terminal width, leaving room for marker + gaps + branch
	// Layout: "* " (2) + path + "  " (2) + branch
	termWidth := table.GetTerminalWidth()
	maxAllowed := termWidth - 2 - 2 - maxBranchLen
	if maxAllowed < 20 {
		maxAllowed = 20
	}
	if maxPathLen > maxAllowed {
		maxPathLen = maxAllowed
	}

	for _, wt := range worktrees {
		path := wt.Path
		if len(path) > maxPathLen {
			// Truncate from start with ellipsis
			path = "..." + path[len(path)-maxPathLen+3:]
		}

		branch := wt.Branch
		if branch == "" {
			branch = "(detached)"
		}

		// Mark current directory with *
		marker := "  "
		if wt.Path == cwd || strings.HasPrefix(cwd, wt.Path+"/") {
			marker = "* "
		}

		// Mark root worktree
		rootMarker := ""
		if wt.IsMain {
			rootMarker = " [root]"
		}

		if params.Verbose {
			commit := wt.Commit
			if len(commit) > 8 {
				commit = commit[:8]
			}
			fmt.Printf("%s%-*s  %-20s  %s%s\n", marker, maxPathLen, path, branch, commit, rootMarker)
		} else {
			fmt.Printf("%s%-*s  %s%s\n", marker, maxPathLen, path, branch, rootMarker)
		}
	}

	return nil
}

// FindWorktreeByBranch finds a worktree by branch name
func FindWorktreeByBranch(branch string) (*WorktreeInfo, error) {
	worktrees, err := ListWorktrees()
	if err != nil {
		return nil, err
	}

	for _, wt := range worktrees {
		if wt.Branch == branch {
			return &wt, nil
		}
	}

	return nil, fmt.Errorf("worktree for branch %q not found", branch)
}

// FindWorktreeByPath finds a worktree by path (exact or suffix match)
func FindWorktreeByPath(pathQuery string) (*WorktreeInfo, error) {
	worktrees, err := ListWorktrees()
	if err != nil {
		return nil, err
	}

	// Make query absolute if it looks like a path
	if strings.HasPrefix(pathQuery, "/") || strings.HasPrefix(pathQuery, ".") {
		absQuery, err := filepath.Abs(pathQuery)
		if err == nil {
			pathQuery = absQuery
		}
	}

	// Normalize path for comparison (git uses forward slashes even on Windows)
	normalizedQuery := filepath.ToSlash(pathQuery)

	// Try exact match first
	for _, wt := range worktrees {
		if filepath.ToSlash(wt.Path) == normalizedQuery {
			return &wt, nil
		}
	}

	// Try suffix match (e.g., "tofu-feature" matches "/Users/.../tofu-feature")
	for _, wt := range worktrees {
		wtPath := filepath.ToSlash(wt.Path)
		if strings.HasSuffix(wtPath, "/"+pathQuery) || filepath.Base(wt.Path) == pathQuery {
			return &wt, nil
		}
	}

	return nil, fmt.Errorf("worktree %q not found", pathQuery)
}
