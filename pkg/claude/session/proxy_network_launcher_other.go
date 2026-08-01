//go:build !darwin

package session

import (
	"fmt"

	"github.com/spf13/cobra"
)

func tclaudeLayerDarwinProxyLauncherCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "tclaude-layer-darwin-proxy-launcher",
		Hidden: true,
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("the Seatbelt filtering-proxy launcher is Darwin-only")
		},
	}
}
