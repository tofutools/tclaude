package processcmd

import (
	"fmt"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/common"
)

type Params struct{}

func Cmd() *cobra.Command {
	cmd := boa.CmdT[Params]{
		Use:         "process",
		Short:       "Author process templates and drive daemon-owned runs",
		Long:        "Author process templates on the existing filesystem path and create, inspect, resume, or explicitly reconcile daemon-owned sequential runs.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			templatesCmd(),
			runsCmd(),
			processRunCmd(),
			processShowCmd(),
			processEventsCmd(),
			processResumeCmd(),
			processReconcileCmd(),
			processReissueCmd(),
			processRecordOutcomeCmd(),
			unavailableRuntimeCmd("preview", "Preview a process-template change"),
			unavailableRuntimeCmd("apply", "Apply a process-template change"),
			unavailableRuntimeCmd("worklist", "Inspect process work"),
			unavailableRuntimeCmd("advance", "Advance a process run"),
			unavailableRuntimeCmd("unblock", "Resolve a blocked process node"),
			unavailableRuntimeCmd("observe", "Record a process command observation"),
			unavailableRuntimeCmd("resolve", "Resolve a human process obligation"),
			unavailableRuntimeCmd("report", "Report process-node work"),
			unavailableRuntimeCmd("verify", "Verify a process run"),
			unavailableRuntimeCmd("repair", "Repair a process run"),
		},
		RunFunc: func(_ *Params, cmd *cobra.Command, _ []string) {
			if err := requireProcessesEnabled(); err != nil {
				exitWithError(err)
			}
			_ = cmd.Help()
		},
	}.ToCobra()
	return cmd
}

func unavailableRuntimeCmd(use, short string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: short + " (temporarily unavailable: no engine)",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return noEngineError()
		},
	}
	// Old runtime flags should still reach the explicit no-engine response
	// instead of failing first as unknown flags. Cobra's own --help remains
	// available, so the temporary command surface is discoverable.
	cmd.FParseErrWhitelist.UnknownFlags = true
	return cmd
}

func requireProcessesEnabled() error {
	if processesEnabled() {
		return nil
	}
	return fmt.Errorf("process commands are disabled; set features.processes=true in tclaude config to use this experimental surface")
}

func processesEnabled() bool {
	cfg, err := config.Load()
	return err == nil && cfg.ProcessesEnabled()
}

func exitWithError(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
