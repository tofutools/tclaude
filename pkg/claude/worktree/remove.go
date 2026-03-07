package worktree

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/spf13/cobra"
)

type RemoveParams struct {
	Target       string `pos:"true" help:"Branch name or path of the worktree to remove"`
	Force        bool   `short:"f" help:"Force removal even if worktree has changes"`
	DeleteBranch bool   `long:"delete-branch" short:"D" help:"Also delete the branch after removing worktree"`
}

func RemoveCmd() *cobra.Command {
	cmd := boa.CmdT[RemoveParams]{
		Use:         "rm <branch-or-path>",
		Short:       "Remove a git worktree",
		Long:        "Remove a git worktree by branch name or path.\n\nBy default, won't remove worktrees with uncommitted changes (use -f to force).",
		Aliases:     []string{"remove"},
		ParamEnrich: common.DefaultParamEnricher(),
		ValidArgsFunc: func(p *RemoveParams, cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				return getWorktreeCompletions(), cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
		RunFunc: func(params *RemoveParams, cmd *cobra.Command, args []string) {
			if err := runRemove(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()

	return cmd
}

func runRemove(params *RemoveParams) error {
	if params.Target == "" {
		return fmt.Errorf("branch name or path is required")
	}

	// Check we're in a git repo
	_, err := GetGitInfo()
	if err != nil {
		return err
	}

	// Find the worktree
	wt, err := FindWorktreeByBranch(params.Target)
	if err != nil {
		// Try by path
		wt, err = FindWorktreeByPath(params.Target)
		if err != nil {
			return fmt.Errorf("worktree %q not found (tried as branch and path)", params.Target)
		}
	}

	if wt.IsMain {
		return fmt.Errorf("cannot remove the main worktree")
	}

	// Confirm removal
	if !params.Force {
		fmt.Printf("Remove worktree?\n")
		fmt.Printf("  Path: %s\n", wt.Path)
		fmt.Printf("  Branch: %s\n", wt.Branch)
		if params.DeleteBranch {
			fmt.Printf("  (branch will also be deleted)\n")
		}
		fmt.Printf("\nProceed? [y/N]: ")

		reader := bufio.NewReader(os.Stdin)
		response, _ := reader.ReadString('\n')
		response = strings.TrimSpace(strings.ToLower(response))
		if response != "y" && response != "yes" {
			fmt.Println("Aborted.")
			return nil
		}
	}

	// Remove worktree
	args := []string{"worktree", "remove"}
	if params.Force {
		args = append(args, "--force")
	}
	args = append(args, wt.Path)

	cmd := exec.Command("git", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to remove worktree: %w", err)
	}

	fmt.Printf("Removed worktree: %s\n", wt.Path)

	// Delete branch if requested
	if params.DeleteBranch && wt.Branch != "" {
		args := []string{"branch", "-d"}
		if params.Force {
			args = []string{"branch", "-D"}
		}
		args = append(args, wt.Branch)

		cmd := exec.Command("git", args...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to delete branch %s: %s\n", wt.Branch, string(output))
		} else {
			fmt.Printf("Deleted branch: %s\n", wt.Branch)
		}
	}

	return nil
}

// getWorktreeCompletions returns completions for worktree branches/paths
func getWorktreeCompletions() []string {
	worktrees, err := ListWorktrees()
	if err != nil {
		return nil
	}

	var completions []string
	for _, wt := range worktrees {
		if wt.IsMain {
			continue // Don't suggest main worktree for removal
		}
		if wt.Branch != "" {
			completions = append(completions, wt.Branch)
		}
	}

	return completions
}
