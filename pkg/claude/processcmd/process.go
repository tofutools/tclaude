package processcmd

import (
	"errors"
	"io"
	"net/http"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
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

// errProcessesDisabled carries the stable feature-disabled message for the
// client surfaces that resolve the flag through the daemon capability
// projection rather than an operation route (today: filesystem `templates
// ls`). The wording matches the daemon's route-gate response so a disabled
// surface reads identically no matter which layer detected it.
var errProcessesDisabled = errors.New(config.ProcessesDisabledMessage)

// requireProcessesEnabledViaDaemon asks the daemon — the sole authority on the
// experimental Processes flag — whether the surface is enabled, without the
// client ever reading private ~/.tclaude/data/config.json. Used by process CLI
// surfaces that do not otherwise contact the daemon; the daemon-owned runtime
// commands instead rely on their operation route's own stable disabled
// response. On a missing daemon it prints the standard guidance and returns
// the already-reported sentinel so callers render nothing further.
func requireProcessesEnabledViaDaemon(stderr io.Writer) error {
	if agent.RequireDaemonOrExit(stderr) != 0 {
		return errProcessDaemonAlreadyReported
	}
	var info struct {
		Processes bool `json:"processes"`
	}
	if err := agent.DaemonRequest(http.MethodGet, "/v1/info", nil, &info,
		agent.DaemonOpts{RetryOutput: stderr}); err != nil {
		return err
	}
	if !info.Processes {
		return errProcessesDisabled
	}
	return nil
}
