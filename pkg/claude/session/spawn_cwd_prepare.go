package session

import (
	"fmt"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/common"
)

// prepareSpawnCwdParams drives the hidden bootstrap that runs inside the tmux
// pane, after cwd-proof validation and before the harness. It intentionally
// accepts no cwd argument. Agent-originated launches only refresh the base
// managed profile here; unlike human launches they deliberately do not derive
// a path grant from cwd, because any persisted pathname grant could be retargeted
// after validation by renaming a writable ancestor.
type prepareSpawnCwdParams struct {
	ManagedProfile bool `short:"-" long:"managed-profile" help:"Prepare the base managed Codex profile"`
}

func prepareSpawnCwdCmd() *cobra.Command {
	cmd := boa.CmdT[prepareSpawnCwdParams]{
		Use:         "prepare-spawn-cwd",
		Short:       "Internal cwd-bound setup for daemon spawns",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *prepareSpawnCwdParams, _ *cobra.Command, _ []string) {
			if err := runPrepareSpawnCwd(p); err != nil {
				fmt.Fprintln(os.Stderr, "Error: "+err.Error())
				os.Exit(1)
			}
		},
	}.ToCobra()
	cmd.Hidden = true
	return cmd
}

func runPrepareSpawnCwd(p *prepareSpawnCwdParams) error {
	if p.ManagedProfile {
		if _, err := harness.EnsureCodexAgentProfile(); err != nil {
			return fmt.Errorf("ensure base managed Codex profile: %w", err)
		}
	}
	return nil
}

func spawnCwdPrepareCommand(managedProfile bool) string {
	args := []string{"session", "prepare-spawn-cwd"}
	if managedProfile {
		args = append(args, "--managed-profile")
	}
	if len(args) == 2 {
		return ""
	}
	return clcommon.DetectCmd(args...)
}
