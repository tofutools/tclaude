package processcmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
)

type reportParams struct {
	RunID       string `pos:"true" help:"Process run id"`
	NodeID      string `pos:"true" help:"Node being performed"`
	CommandID   string `long:"command" help:"Process command id from the performer brief"`
	Verdict     string `long:"verdict" help:"Result verdict"`
	EvidenceRef string `long:"evidence" help:"Evidence artifact/reference"`
	Feedback    string `long:"feedback" optional:"true" help:"Feedback for a retry loop"`
}

func reportCmd() *cobra.Command {
	return boa.CmdT[reportParams]{
		Use:         "report",
		Short:       "Report a structured agent performer result",
		ParamEnrich: common.DefaultParamEnricher(),
		Args:        cobra.ExactArgs(2),
		PreExecuteFunc: func(p *reportParams, _ *cobra.Command, _ []string) error {
			if err := requireProcessesEnabled(); err != nil {
				return err
			}
			if strings.TrimSpace(p.CommandID) == "" || strings.TrimSpace(p.Verdict) == "" {
				return fmt.Errorf("--command and --verdict are required")
			}
			if strings.TrimSpace(p.EvidenceRef) == "" {
				return fmt.Errorf("--evidence is required; claimed done is not done")
			}
			return nil
		},
		RunFunc: func(p *reportParams, _ *cobra.Command, _ []string) {
			var response struct {
				Actor string `json:"actor"`
			}
			err := agent.DaemonPost("/v1/process/runs/"+p.RunID+"/nodes/"+p.NodeID+"/report", map[string]string{
				"command_id":   strings.TrimSpace(p.CommandID),
				"verdict":      strings.TrimSpace(p.Verdict),
				"evidence_ref": strings.TrimSpace(p.EvidenceRef),
				"feedback":     strings.TrimSpace(p.Feedback),
			}, &response)
			if err != nil {
				exitWithError(err)
				return
			}
			fmt.Fprintf(os.Stdout, "Reported process result as %s\n", response.Actor)
		},
	}.ToCobra()
}
