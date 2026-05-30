// Package agent implements `tclaude agent` — coordination between
// Claude Code conversations.
//
// See the agent-coordination tickets on Linear (project tclaude, JOH-10).
package agent

import (
	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// Cmd returns the `tclaude agent` cobra command.
func Cmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "agent",
		Short:       "Coordinate between Claude Code conversations",
		Long:        "Look up other agents (named conversations), send messages, and manage allow-listed groups.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			whoamiCmd(),
			renameCmd(),
			compactCmd(),
			reincarnateCmd(),
			cloneCmd(),
			stopCmd(),
			resumeCmd(),
			promoteCmd(),
			retireCmd(),
			reinstateCmd(),
			deleteCmd(),
			contextInfoCmd(),
			dirCmd(),
			lookupCmd(),
			lsCmd(),
			messageCmd(),
			replyCmd(),
			notifyHumanCmd(),
			groupsCmd(),
			aliasCmd(),
			spawnCmd(),
			inboxCmd(),
			permissionsCmd(),
			sudoCmd(),
			cronCmd(),
			templatesCmd(),
			dashboardCmd(),
		},
	}.ToCobra()
}
