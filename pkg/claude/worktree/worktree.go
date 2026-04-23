package worktree

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

type Params struct{}

func Cmd() *cobra.Command {
	return boa.CmdT[Params]{
		Use:         "worktree",
		Short:       "Manage git worktrees for parallel Claude sessions",
		Long:        "Create and manage git worktrees to work on multiple features in parallel.\n\nEach worktree gets its own directory and branch, allowing independent Claude sessions.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			AddCmd(),
			ListCmd(),
			RemoveCmd(),
			RestoreCmd(),
			SwitchCmd(),
		},
		RunFunc: func(params *Params, cmd *cobra.Command, args []string) {
			// Default to list
			if err := runList(&ListParams{}); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

// GitInfo contains information about the current git repository
type GitInfo struct {
	RepoRoot     string // Absolute path to repo root
	RepoName     string // Name of the repo (directory name)
	IsWorktree   bool   // True if current directory is already a worktree
	MainWorktree string // Path to the main worktree (if in a linked worktree)
}

// GetGitInfo returns information about the current git repository
func GetGitInfo() (*GitInfo, error) {
	// Check if we're in a git repo
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("not a git repository")
	}
	repoRoot := strings.TrimSpace(string(output))

	// Get repo name from path
	repoName := filepath.Base(repoRoot)

	// Check if this is a worktree
	cmd = exec.Command("git", "rev-parse", "--git-common-dir")
	commonDir, _ := cmd.Output()
	cmd = exec.Command("git", "rev-parse", "--git-dir")
	gitDir, _ := cmd.Output()

	isWorktree := strings.TrimSpace(string(commonDir)) != strings.TrimSpace(string(gitDir))

	var mainWorktree string
	if isWorktree {
		// The common dir points to the main worktree's .git
		commonDirPath := strings.TrimSpace(string(commonDir))
		if strings.HasSuffix(commonDirPath, "/.git") {
			mainWorktree = strings.TrimSuffix(commonDirPath, "/.git")
		} else {
			mainWorktree = filepath.Dir(commonDirPath)
		}
	}

	return &GitInfo{
		RepoRoot:     repoRoot,
		RepoName:     repoName,
		IsWorktree:   isWorktree,
		MainWorktree: mainWorktree,
	}, nil
}

// GetDefaultBranch returns the default branch (main or master)
func GetDefaultBranch() (string, error) {
	// Try to get the default branch from remote
	cmd := exec.Command("git", "symbolic-ref", "refs/remotes/origin/HEAD")
	output, err := cmd.Output()
	if err == nil {
		ref := strings.TrimSpace(string(output))
		// refs/remotes/origin/main -> main
		parts := strings.Split(ref, "/")
		if len(parts) > 0 {
			return parts[len(parts)-1], nil
		}
	}

	// Fallback: check if main or master exists
	for _, branch := range []string{"main", "master"} {
		cmd := exec.Command("git", "rev-parse", "--verify", branch)
		if err := cmd.Run(); err == nil {
			return branch, nil
		}
	}

	return "", fmt.Errorf("could not determine default branch (tried main, master)")
}

// BranchExists checks if a branch exists
func BranchExists(branch string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", branch)
	return cmd.Run() == nil
}

// WorktreeInfo represents a git worktree
type WorktreeInfo struct {
	Path   string // Absolute path to worktree
	Branch string // Branch checked out in worktree
	Commit string // Current commit SHA
	IsMain bool   // True if this is the main worktree
	IsBare bool   // True if this is a bare worktree
}

// ListWorktrees returns all git worktrees
func ListWorktrees() ([]WorktreeInfo, error) {
	cmd := exec.Command("git", "worktree", "list", "--porcelain")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list worktrees: %w", err)
	}

	var worktrees []WorktreeInfo
	var current WorktreeInfo

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			if current.Path != "" {
				worktrees = append(worktrees, current)
				current = WorktreeInfo{}
			}
			continue
		}

		if strings.HasPrefix(line, "worktree ") {
			current.Path = strings.TrimPrefix(line, "worktree ")
		} else if strings.HasPrefix(line, "HEAD ") {
			current.Commit = strings.TrimPrefix(line, "HEAD ")
		} else if strings.HasPrefix(line, "branch ") {
			ref := strings.TrimPrefix(line, "branch ")
			// refs/heads/main -> main
			current.Branch = strings.TrimPrefix(ref, "refs/heads/")
		} else if line == "bare" {
			current.IsBare = true
		}
	}

	// Add last worktree if any
	if current.Path != "" {
		worktrees = append(worktrees, current)
	}

	// Mark the main worktree (first one, or the non-linked one)
	if len(worktrees) > 0 {
		worktrees[0].IsMain = true
	}

	return worktrees, nil
}

// GetBranchCompletions returns branch names for shell completion
func GetBranchCompletions() []string {
	cmd := exec.Command("git", "branch", "-a", "--format=%(refname:short)")
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	var branches []string
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.Contains(line, "->") {
			// Remove origin/ prefix for remote branches
			branch := strings.TrimPrefix(line, "origin/")
			branches = append(branches, branch)
		}
	}

	// Deduplicate
	seen := make(map[string]bool)
	var unique []string
	for _, b := range branches {
		if !seen[b] {
			seen[b] = true
			unique = append(unique, b)
		}
	}

	return unique
}
