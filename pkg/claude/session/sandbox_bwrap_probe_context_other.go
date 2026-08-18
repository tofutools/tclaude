//go:build !linux

package session

import (
	"fmt"

	"github.com/spf13/cobra"
)

const tclaudeLayerProbeCommand = "tclaude-layer-capability-probe"

// tclaudeLayerProbeCmd exists off Linux only so the subcommand set is the same
// on every platform. Darwin's Seatbelt boundary has no equivalent split: the
// probe and the launch both run `sandbox-exec` from the pane's own process, so
// there is no foreign confinement to reach into.
func tclaudeLayerProbeCmd() *cobra.Command {
	return &cobra.Command{
		Use:    tclaudeLayerProbeCommand,
		Short:  "Probe tclaude-layer host capability from the pane's confinement (internal)",
		Hidden: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("%s is only used by the Linux bubblewrap layer", tclaudeLayerProbeCommand)
		},
	}
}
