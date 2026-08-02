//go:build !linux

package session

import (
	"fmt"
	"github.com/spf13/cobra"
)

func tclaudeLayerRouteHelperCmd() *cobra.Command {
	return &cobra.Command{
		Use:    tclaudeLayerRouteHelperCommand,
		Short:  "Run the namespace-local group-route helper (internal)",
		Hidden: true,
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("%s is only available on Linux", tclaudeLayerRouteHelperCommand)
		},
	}
}
