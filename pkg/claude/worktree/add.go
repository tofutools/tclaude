package worktree

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/common"
)

type AddParams struct {
	Branch     string `pos:"true" help:"Branch name for the new worktree"`
	FromBranch string `long:"from-branch" optional:"true" help:"Base branch to create from (defaults to main/master)"`
	FromConv   string `long:"from-conv" optional:"true" help:"Conversation ID to copy to the new worktree"`
	Path       string `long:"path" optional:"true" help:"Custom path for the worktree (defaults to ../<repo>-<branch>)"`
	Detached   bool   `long:"detached" short:"d" help:"Don't start/attach to a Claude session"`
	Global     bool   `short:"g" help:"Search for conversation across all projects (with --from-conv)"`
}

func AddCmd() *cobra.Command {
	cmd := boa.CmdT[AddParams]{
		Use:         "add",
		Short:       "Create a new git worktree with a Claude session",
		Long:        "Create a new git worktree for parallel development.\n\nThis creates a new branch (if needed), sets up a worktree, optionally copies a conversation, and starts a Claude session.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *AddParams, cmd *cobra.Command, args []string) {
			if err := runAdd(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()

	// Register completions
	cmd.RegisterFlagCompletionFunc("from-branch", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return GetBranchCompletions(), cobra.ShellCompDirectiveNoFileComp
	})

	cmd.RegisterFlagCompletionFunc("from-conv", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		global, _ := cmd.Flags().GetBool("global")
		return clcommon.GetConversationCompletions(global), cobra.ShellCompDirectiveKeepOrder | cobra.ShellCompDirectiveNoFileComp
	})

	return cmd
}

func runAdd(params *AddParams) error {
	return RunAdd(params.Branch, params.FromBranch, params.FromConv, params.Path, params.Global, params.Detached)
}

// RunAdd creates a git worktree and optionally starts a Claude session.
// This is the core logic that can be called from CLI or programmatically.
func RunAdd(branch, fromBranch, fromConv, path string, global, detached bool) error {
	if branch == "" {
		return fmt.Errorf("branch name is required")
	}

	// Get git info
	gitInfo, err := GetGitInfo()
	if err != nil {
		return err
	}

	// Determine base branch
	baseBranch := fromBranch
	if baseBranch == "" {
		baseBranch, err = GetDefaultBranch()
		if err != nil {
			return fmt.Errorf("could not determine base branch: %w (use --from-branch to specify)", err)
		}
	}

	// Check base branch exists
	if !BranchExists(baseBranch) {
		return fmt.Errorf("base branch %q does not exist", baseBranch)
	}

	// Determine worktree path
	worktreePath := path
	if worktreePath == "" {
		// Default: ../<repo>-<branch>
		// Replace slashes in branch name to avoid creating subdirectories
		safeBranch := strings.ReplaceAll(branch, "/", "--")
		safeBranch = strings.ReplaceAll(safeBranch, "\\", "--")
		parentDir := filepath.Dir(gitInfo.RepoRoot)
		worktreePath = filepath.Join(parentDir, gitInfo.RepoName+"-"+safeBranch)
	}

	// Make path absolute
	if !filepath.IsAbs(worktreePath) {
		cwd, _ := os.Getwd()
		worktreePath = filepath.Join(cwd, worktreePath)
	}

	// Check if path already exists
	if _, err := os.Stat(worktreePath); err == nil {
		return fmt.Errorf("path already exists: %s", worktreePath)
	}

	// Check if branch already exists
	branchExists := BranchExists(branch)

	fmt.Printf("Creating worktree...\n")
	fmt.Printf("  Branch: %s", branch)
	if !branchExists {
		fmt.Printf(" (new, from %s)", baseBranch)
	}
	fmt.Printf("\n")
	fmt.Printf("  Path: %s\n", worktreePath)

	// Create worktree
	var gitCmd *exec.Cmd
	if branchExists {
		// Use existing branch
		gitCmd = exec.Command("git", "worktree", "add", worktreePath, branch)
	} else {
		// Create new branch from base
		gitCmd = exec.Command("git", "worktree", "add", "-b", branch, worktreePath, baseBranch)
	}

	gitCmd.Stdout = os.Stdout
	gitCmd.Stderr = os.Stderr

	if err := gitCmd.Run(); err != nil {
		return fmt.Errorf("failed to create worktree: %w", err)
	}

	fmt.Printf("\nWorktree created successfully.\n")

	// Copy conversation if specified
	var copiedConvID string
	if fromConv != "" {
		convID := clcommon.ExtractIDFromCompletion(fromConv)
		fmt.Printf("\nCopying conversation %s...\n", convID)

		result, err := convops.CopyConversationToPath(convID, worktreePath, global)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to copy conversation: %v\n", err)
		} else {
			copiedConvID = result.NewConvID
			fmt.Printf("  Copied to new project: %s\n", copiedConvID[:8])
		}
	}

	// Start session unless detached
	if !detached {
		fmt.Printf("\nStarting Claude session...\n")

		newParams := &session.NewParams{
			Dir: worktreePath,
		}

		// If we copied a conversation, resume it
		if copiedConvID != "" {
			newParams.Resume = copiedConvID
		}

		return session.RunNew(newParams)
	}

	fmt.Printf("\nTo start a session:\n")
	fmt.Printf("  cd %s && tclaude\n", worktreePath)
	if copiedConvID != "" {
		fmt.Printf("  # Or resume the copied conversation:\n")
		fmt.Printf("  cd %s && tclaude --resume %s\n", worktreePath, copiedConvID[:8])
	}

	return nil
}
