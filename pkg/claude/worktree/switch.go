package worktree

import (
	"fmt"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

type SwitchParams struct {
	Target string `pos:"true" optional:"true" help:"Branch name or path of the worktree to switch to"`
}

func SwitchCmd() *cobra.Command {
	cmd := boa.CmdT[SwitchParams]{
		Use:         "switch",
		Short:       "Output worktree path for switching (use with shell wrapper)",
		Long:        "Outputs the path of a worktree for use with cd.\n\nWith a shell wrapper, this enables: tclaude worktree switch <branch>",
		Aliases:     []string{"s", "checkout", "c"},
		ParamEnrich: common.DefaultParamEnricher(),
		ValidArgsFunc: func(p *SwitchParams, cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				return getWorktreeBranchCompletions(), cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
		RunFunc: func(params *SwitchParams, cmd *cobra.Command, args []string) {
			path, err := runSwitch(params)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			// Output just the path - the shell wrapper will cd to it
			fmt.Println(path)
		},
	}.ToCobra()

	return cmd
}

func runSwitch(params *SwitchParams) (string, error) {
	// Check we're in a git repo
	_, err := GetGitInfo()
	if err != nil {
		return "", err
	}

	// If no target specified, could show interactive picker in future
	// For now, require a target
	if params.Target == "" {
		return "", fmt.Errorf("branch name or path required")
	}

	// Try to find by branch first
	wt, err := FindWorktreeByBranch(params.Target)
	if err != nil {
		// Try by path
		wt, err = FindWorktreeByPath(params.Target)
		if err != nil {
			return "", fmt.Errorf("worktree %q not found", params.Target)
		}
	}

	return wt.Path, nil
}

// getWorktreeBranchCompletions returns branch names of existing worktrees
func getWorktreeBranchCompletions() []string {
	worktrees, err := ListWorktrees()
	if err != nil {
		return nil
	}

	var completions []string
	for _, wt := range worktrees {
		if wt.Branch != "" {
			completions = append(completions, wt.Branch)
		}
	}

	return completions
}
