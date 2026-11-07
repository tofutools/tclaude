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
	root := boa.CmdT[struct{}]{
		Use:         "agent",
		Short:       "Coordinate between coding agents",
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
			morphCmd(),
			morphHandoffCmd(),
			cloneCmd(),
			teleportCmd(),
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
			bundleCmd(),
			snapshotCmd(),
			recoverCmd(),
			debugExportCmd(),
			groupsCmd(),
			aliasCmd(),
			spawnCmd(),
			queueCmd(),
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
			relaunchPolicyCmd(),
			sandboxCmd(),
			sandboxImplLegacyCmd(),
			taskForceCmd(),
			processCmd(),
			processTemplatesCmd(),
			dashboardCmd(),
			guideCmd(),
			fleetCmd(),
			knowledgeCmd(),
			stewardCmd(),
			drawingBoardCmd(),
			taskboardCmd(),
			resourcesCmd(),
			showCmd(),
			pageCmd(),
		},
	}.ToCobra()
	applyCrossAgentTargetHelp(root)
	return root
}
