package processcmd

import (
	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

type Params struct{}

func Cmd() *cobra.Command {
	cmd := boa.CmdT[Params]{
		Use:         "process",
		Short:       "Author process templates and drive daemon-owned runs",
		Long:        "Author process templates on the existing filesystem path and create, inspect, pause, resume, or explicitly reconcile daemon-owned sequential runs.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			templatesCmd(),
			runsCmd(),
			processRunCmd(),
			processShowCmd(),
			processEventsCmd(),
			processPauseCmd(),
			processResumeCmd(),
			processReconcileCmd(),
			processReissueCmd(),
			processRecordOutcomeCmd(),
			processDecideCmd(),
			processAssignCmd(),
			processResolveBlockedCmd(),
			processReportCmd(),
			processRescopeCmd(),
			unavailableRuntimeCmd("preview", "Preview a process-template change"),
			unavailableRuntimeCmd("apply", "Apply a process-template change"),
			unavailableRuntimeCmd("worklist", "Inspect process work"),
			unavailableRuntimeCmd("advance", "Advance a process run"),
			unavailableRuntimeCmd("unblock", "Resolve a blocked process node"),
			unavailableRuntimeCmd("observe", "Record a process command observation"),
			unavailableRuntimeCmd("resolve", "Resolve a human process obligation"),
			unavailableRuntimeCmd("verify", "Verify a process run"),
			unavailableRuntimeCmd("repair", "Repair a process run"),
		},
		RunFunc: func(_ *Params, cmd *cobra.Command, _ []string) {
			// Help is pure discoverability text; the sibling `runs` and
			// `templates` parents already print it ungated. Enforcement of
			// the feature flag lives on the leaf commands, daemon-side — the
			// bare command must not read private config to decide whether to
			// show its own help.
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
