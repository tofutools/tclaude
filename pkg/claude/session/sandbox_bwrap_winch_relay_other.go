//go:build !linux

package session

import (
	"fmt"

	"github.com/spf13/cobra"
)

const tclaudeLayerWinchRelayCommand = "tclaude-layer-winch-relay"

func tclaudeLayerWinchRelayCmd() *cobra.Command {
	return &cobra.Command{
		Use:    tclaudeLayerWinchRelayCommand,
		Short:  "Relay terminal resize notifications into tclaude-layer (internal)",
		Hidden: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("%s is only used by the Linux bubblewrap layer", tclaudeLayerWinchRelayCommand)
		},
	}
}
