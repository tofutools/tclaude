package git

import (
	"github.com/GiGurra/boa/pkg/boa"
	"github.com/tofutools/tclaude/pkg/claude/syncutil"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/spf13/cobra"
)

func Cmd() *cobra.Command {
	return boa.CmdT[boa.NoParams]{
		Use:         "git",
		Short:       "Sync Claude conversations across devices via git",
		ParamEnrich: common.DefaultParamEnricher(),
		Long: `Sync Claude conversations across multiple computers using a git repository.

This keeps ~/.claude/projects_sync as a git working directory separate from
the actual ~/.claude/projects, giving full control over the merge process.

Usage:
  tclaude git init <repo-url>   # Set up sync with a remote repo
  tclaude git sync              # Sync local and remote conversations
  tclaude git status            # Show sync status
  tclaude git fetch             # Fetch remote changes without merging`,
		SubCmds: []*cobra.Command{
			InitCmd(),
			SyncCmd(),
			StatusCmd(),
			FetchCmd(),
			RepairCmd(),
		},
	}.ToCobra()
}

// SyncDir returns the path to the sync directory (~/.claude/projects_sync)
func SyncDir() string {
	return syncutil.SyncDir()
}

// ProjectsDir returns the path to the actual projects directory (~/.claude/projects)
func ProjectsDir() string {
	return syncutil.ProjectsDir()
}

// IsInitialized checks if the sync directory is a git repository
func IsInitialized() bool {
	return syncutil.IsInitialized()
}
