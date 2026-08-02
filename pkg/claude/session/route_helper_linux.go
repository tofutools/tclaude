//go:build linux

package session

import (
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/routeadapter"
)

func tclaudeLayerRouteHelperCmd() *cobra.Command {
	var (
		socketPath       string
		agentID          string
		convID           string
		launchGeneration string
		credential       string
		groupIDs         []int64
	)
	cmd := &cobra.Command{
		Use:    tclaudeLayerRouteHelperCommand,
		Short:  "Run the namespace-local group-route helper (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return routeadapter.RunHelper(cmd.Context(), routeadapter.HelperConfig{
				SocketPath: socketPath, AgentID: agentID, ConvID: convID,
				LaunchGeneration: launchGeneration, Credential: credential, GroupIDs: groupIDs,
			})
		},
	}
	cmd.Flags().StringVar(&socketPath, "socket", "", "agentd Unix socket (internal)")
	cmd.Flags().StringVar(&agentID, "agent-id", "", "stable agent identity (internal)")
	cmd.Flags().StringVar(&convID, "conv-id", "", "conversation identity (internal)")
	cmd.Flags().StringVar(&launchGeneration, "launch-generation", "", "launch generation (internal)")
	cmd.Flags().StringVar(&credential, "credential", "", "opaque helper credential (internal)")
	cmd.Flags().Int64SliceVar(&groupIDs, "group-id", nil, "explicit route-enabled group id (repeatable)")
	return cmd
}
