// Package agent implements `tclaude agent` — coordination between
// Claude Code conversations.
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
			routesCmd(),
			whoamiCmd(),
			codexAppServerCmd(),
			renameCmd(),
			taskCmd(),
			presentPRCmd(),
			tagsCmd(),
			compactCmd(),
			interruptCmd(),
			remoteControlCmd(),
			reincarnateCmd(),
			cloneCmd(),
			seanceCmd(),
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
			clipboardCmd(),
			exportCmd(),
			debugExportCmd(),
			groupsCmd(),
			aliasCmd(),
			spawnCmd(),
			inboxCmd(),
			permissionsCmd(),
			sudoCmd(),
			cronCmd(),
			ordersCmd(),
			triggersCmd(),
			templatesCmd(),
			rolesCmd(),
			profilesCmd(),
			sandboxProfilesCmd(),
			sandboxImplCmd(),
			taskForceCmd(),
			processCmd(),
			processTemplatesCmd(),
			dashboardCmd(),
		},
	}.ToCobra()
}
