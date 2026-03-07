package worktree

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/spf13/cobra"
)

type RestoreParams struct {
	Branch   string `pos:"true" help:"Branch name to restore (local or remote)"`
	Path     string `long:"path" optional:"true" help:"Custom path for the worktree (defaults to ../<repo>-<branch>)"`
	Detached bool   `long:"detached" short:"d" help:"Don't start/attach to a Claude session"`
}

func RestoreCmd() *cobra.Command {
	return boa.CmdT[RestoreParams]{
		Use:   "restore",
		Short: "Restore a worktree from a local or remote branch",
		Long: "Restore a previously deleted worktree.\n\n" +
			"If the local branch still exists, creates a worktree from it.\n" +
			"If only the remote branch remains, fetches and creates a local tracking branch.\n" +
			"Then sets up a worktree and starts a Claude session.",
		ParamEnrich: common.DefaultParamEnricher(),
		ValidArgsFunc: func(p *RestoreParams, cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				return getRestorableBranchCompletions(), cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
		RunFunc: func(params *RestoreParams, cmd *cobra.Command, args []string) {
			if err := runRestore(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runRestore(params *RestoreParams) error {
	if params.Branch == "" {
		return fmt.Errorf("branch name is required")
	}

	branch := params.Branch

	// Strip origin/ prefix if provided (user might type origin/feat/foo)
	branch = strings.TrimPrefix(branch, "origin/")

	// If the local branch still exists, just create a worktree from it
	if BranchExists(branch) {
		fmt.Printf("Local branch %q found, creating worktree...\n", branch)
		return RunAdd(branch, "", "", params.Path, false, params.Detached)
	}

	// Local branch is gone — try to restore from remote
	fmt.Printf("Fetching from origin...\n")
	fetchCmd := exec.Command("git", "fetch", "origin")
	fetchCmd.Stdout = os.Stdout
	fetchCmd.Stderr = os.Stderr
	if err := fetchCmd.Run(); err != nil {
		return fmt.Errorf("failed to fetch from origin: %w", err)
	}

	remoteBranch := "origin/" + branch
	if !BranchExists(remoteBranch) {
		return fmt.Errorf("branch %q not found locally or on origin", branch)
	}

	// Create worktree with local branch tracking the remote
	return RunAdd(branch, remoteBranch, "", params.Path, false, params.Detached)
}

// getRestorableBranchCompletions returns branches that can be restored:
// local branches without an active worktree + remote-only branches
func getRestorableBranchCompletions() []string {
	// Get branches that already have a worktree
	worktrees, _ := ListWorktrees()
	worktreeBranches := make(map[string]bool)
	for _, wt := range worktrees {
		if wt.Branch != "" {
			worktreeBranches[wt.Branch] = true
		}
	}

	// Get local branches without a worktree
	localCmd := exec.Command("git", "branch", "--format=%(refname:short)")
	localOutput, err := localCmd.Output()
	if err != nil {
		return nil
	}
	localBranches := make(map[string]bool)
	var completions []string
	for _, line := range strings.Split(string(localOutput), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		localBranches[line] = true
		if !worktreeBranches[line] {
			completions = append(completions, line)
		}
	}

	// Add remote-only branches (no local counterpart)
	remoteCmd := exec.Command("git", "branch", "-r", "--format=%(refname:short)")
	remoteOutput, err := remoteCmd.Output()
	if err != nil {
		return completions
	}
	for _, line := range strings.Split(string(remoteOutput), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "->") {
			continue
		}
		branch := strings.TrimPrefix(line, "origin/")
		if !localBranches[branch] {
			completions = append(completions, branch)
		}
	}

	return completions
}
