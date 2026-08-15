//go:build !linux

package session

import (
	"fmt"

	"github.com/spf13/cobra"
)

func tclaudeLayerFilteredBootstrapCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "tclaude-layer-filtered-bootstrap",
		Hidden: true,
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("the filtered-network bootstrap is Linux-only")
		},
	}
}

func tclaudeLayerProxyBootstrapCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "tclaude-layer-proxy-bootstrap",
		Hidden: true,
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("the proxy-network bootstrap is Linux-only")
		},
	}
}
