package processcmd

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/common"
)

type processReportParams struct {
	Outcome string `pos:"true" help:"Outcome of the node's work: succeeded or failed"`
	Run     string `long:"run" optional:"true" help:"Process run id from the dispatch message; optional when you have exactly one open node"`
	Node    string `long:"node" optional:"true" help:"Node id from the dispatch message"`
	Attempt int    `long:"attempt" optional:"true" help:"Attempt number from the dispatch message"`
	Note    string `long:"note" optional:"true" help:"Short summary of the result, or why it failed"`
}

// processReportCmd is the fixed report-back verb an agent performer runs when
// it has finished a process node it was handed. It is authenticated by the
// calling agent's socket identity: only the agent the attempt was delivered to
// can report it, and only once.
func processReportCmd() *cobra.Command {
	return boa.CmdT[processReportParams]{
		Use:   "report",
		Short: "Report the outcome of a process node you were asked to perform",
		Long: "Run this from the agent that received a process node in its inbox, exactly once, " +
			"when the work is done. Copy --run, --node, and --attempt from the dispatch message; " +
			"they may be omitted when you have exactly one open process node.",
		ParamEnrich: common.DefaultParamEnricher(),
		ValidArgs:   []string{string(engine.ProgramSucceeded), string(engine.ProgramFailed)},
		RunFunc: func(p *processReportParams, _ *cobra.Command, _ []string) {
			exitProcessRuntime(runProcessReport(p, os.Stdout, os.Stderr), os.Stderr)
		},
	}.ToCobra()
}

func runProcessReport(p *processReportParams, stdout, stderr io.Writer) error {
	outcome := engine.ProgramOutcome(strings.TrimSpace(p.Outcome))
	if outcome != engine.ProgramSucceeded && outcome != engine.ProgramFailed {
		return fmt.Errorf("outcome must be %s or %s", engine.ProgramSucceeded, engine.ProgramFailed)
	}
	if err := requireProcessRuntime(stderr); err != nil {
		return err
	}
	request := struct {
		RunID   string                `json:"runId,omitempty"`
		NodeID  string                `json:"nodeId,omitempty"`
		Attempt int                   `json:"attempt,omitempty"`
		Outcome engine.ProgramOutcome `json:"outcome"`
		Note    string                `json:"note,omitempty"`
	}{RunID: strings.TrimSpace(p.Run), NodeID: strings.TrimSpace(p.Node), Attempt: p.Attempt,
		Outcome: outcome, Note: strings.TrimSpace(p.Note)}
	var response struct {
		Status  string `json:"status"`
		RunID   string `json:"runId"`
		NodeID  string `json:"nodeId"`
		Attempt int    `json:"attempt"`
	}
	if err := agent.DaemonRequest(http.MethodPost, "/v1/process/report", request, &response,
		agent.DaemonOpts{RetryOutput: stderr}); err != nil {
		return err
	}
	switch response.Status {
	case "duplicate":
		fmt.Fprintf(stdout, "Already reported %s for run %s node %s attempt %d; nothing changed.\n",
			outcome, response.RunID, response.NodeID, response.Attempt)
	case "recorded":
		fmt.Fprintf(stdout, "Recorded %s for run %s node %s attempt %d; the daemon will apply it shortly.\n",
			outcome, response.RunID, response.NodeID, response.Attempt)
	default:
		fmt.Fprintf(stdout, "Reported %s for run %s node %s attempt %d.\n",
			outcome, response.RunID, response.NodeID, response.Attempt)
	}
	return nil
}
