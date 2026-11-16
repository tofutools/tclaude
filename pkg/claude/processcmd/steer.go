package processcmd

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/common"
)

type processCancelParams struct {
	RunID    string `pos:"true" help:"Process run id"`
	Note     string `long:"note" optional:"true" help:"Why the run is given up, recorded in its log"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout"`
}

func processCancelCmd() *cobra.Command {
	return boa.CmdT[processCancelParams]{
		Use:   "cancel",
		Short: "Give a process run up: nothing more starts, work still out is withdrawn, and it ends canceled",
		Long: "Cancels a run from any running state (process.runs.manage). Agent steps still out are told to stop, " +
			"programs are stopped, and the run finishes canceled once they are accounted for. " +
			"An overseer without process.runs.manage raises a concern instead; the operator's card offers the cancel.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *processCancelParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *processCancelParams, _ *cobra.Command, _ []string) {
			exitProcessRuntime(runProcessCancel(p, os.Stdout, os.Stderr), os.Stderr)
		},
	}.ToCobra()
}

func runProcessCancel(p *processCancelParams, stdout, stderr io.Writer) error {
	if err := requireProcessRuntime(stderr); err != nil {
		return err
	}
	runID := strings.TrimSpace(p.RunID)
	if runID == "" {
		return fmt.Errorf("run id is required")
	}
	ask, err := agent.ParseAskHuman(p.AskHuman)
	if err != nil {
		return err
	}
	var response struct {
		Run processRunJSON `json:"run"`
	}
	request := map[string]string{"note": p.Note}
	if err := agent.DaemonRequest(http.MethodPost, "/v1/process/runs/"+url.PathEscape(runID)+"/cancel", request, &response,
		agent.DaemonOpts{AskHuman: ask, RetryOutput: stderr}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Canceled process run %s.\n", response.Run.ID)
	printProcessRun(stdout, &response.Run)
	return nil
}

type processBlockParams struct {
	RunID    string `pos:"true" help:"Process run id"`
	Node     string `long:"node" help:"Node id of the agent step that is out"`
	Reason   string `long:"reason" help:"Why the step cannot go on, shown to the operator and in the run log"`
	Attempt  int    `long:"attempt" optional:"true" help:"The attempt that is out (default: the one tclaude process show reports)"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout"`
}

func processBlockCmd() *cobra.Command {
	return boa.CmdT[processBlockParams]{
		Use:   "block",
		Short: "Park an agent step that is out, with a reason; it then waits on resolve-blocked",
		Long: "Marks one agent step blocked (the run's overseer, or process.runs.manage). Its performer is told to stop, " +
			"a late report of that attempt is refused, and the step waits on " +
			"tclaude process resolve-blocked retry, skip or cancel, like a step whose retries ran out.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *processBlockParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *processBlockParams, _ *cobra.Command, _ []string) {
			exitProcessRuntime(runProcessBlock(p, os.Stdout, os.Stderr), os.Stderr)
		},
	}.ToCobra()
}

func runProcessBlock(p *processBlockParams, stdout, stderr io.Writer) error {
	if err := requireProcessRuntime(stderr); err != nil {
		return err
	}
	runID, node, reason := strings.TrimSpace(p.RunID), strings.TrimSpace(p.Node), strings.TrimSpace(p.Reason)
	if runID == "" || node == "" {
		return fmt.Errorf("a run id and --node are required")
	}
	if reason == "" {
		return fmt.Errorf("--reason is required: the operator decides from it")
	}
	ask, err := agent.ParseAskHuman(p.AskHuman)
	if err != nil {
		return err
	}
	attempt := p.Attempt
	if attempt == 0 {
		run, err := fetchProcessRun(runID, ask, stderr)
		if err != nil {
			return err
		}
		for _, command := range run.Commands {
			if command.NodeID == node && command.Kind == engine.CommandAgent && command.Parkable {
				attempt = command.Attempt
			}
		}
		if attempt == 0 {
			return fmt.Errorf("node %s has no agent task step out in run %s that can be parked "+
				"(a decision, vote, or check or review stage cannot be; pause the run or raise a concern)", node, runID)
		}
	}
	request := map[string]any{"nodeId": node, "attempt": attempt, "reason": reason}
	var response struct {
		Run processRunJSON `json:"run"`
	}
	if err := agent.DaemonRequest(http.MethodPost, "/v1/process/runs/"+url.PathEscape(runID)+"/block", request, &response,
		agent.DaemonOpts{AskHuman: ask, RetryOutput: stderr}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Parked %s attempt %d of process run %s. Resolve it with: tclaude process resolve-blocked %s --node %s --attempt %d --action retry|skip|cancel\n",
		node, attempt, runID, runID, node, attempt)
	printProcessRun(stdout, &response.Run)
	return nil
}
