package processcmd

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
)

// processRunWaitJSON mirrors the daemon's view of a waiting wait step.
type processRunWaitJSON struct {
	Duration  string     `json:"duration,omitempty"`
	Until     string     `json:"until,omitempty"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	DueAt     *time.Time `json:"dueAt,omitempty"`
	Failure   string     `json:"failure,omitempty"`
}

// label is what the wait waits for, as authored: "3 days", "until <time>",
// or both.
func (w *processRunWaitJSON) label() string {
	if w == nil {
		return "wait"
	}
	var parts []string
	if w.Duration != "" {
		parts = append(parts, "for "+displayProcessEventField(w.Duration))
	}
	if w.Until != "" {
		parts = append(parts, "until "+displayProcessEventField(w.Until))
	}
	if len(parts) == 0 {
		return "wait"
	}
	return "wait " + strings.Join(parts, ", at most ")
}

// status is how long the wait has left at now.
func (w *processRunWaitJSON) status(now time.Time) string {
	switch {
	case w == nil || w.DueAt == nil:
		return "starting"
	case w.Failure != "":
		return "failing: " + displayProcessEventField(w.Failure)
	}
	left := w.DueAt.Sub(now)
	due := w.DueAt.Local().Format("2006-01-02 15:04 MST")
	if left <= 0 {
		return "due now (was due " + due + ")"
	}
	return left.Round(time.Second).String() + " left, until " + due
}

type processSkipWaitParams struct {
	RunID    string `pos:"true" help:"Process run id"`
	Node     string `long:"node" help:"The waiting wait step's node id"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout"`
	Remote   string `long:"remote" optional:"true" help:"Act on a run of a trusted peer node instead (label or instance id); needs its process.runs.manage grant to this node"`
}

func processSkipWaitCmd() *cobra.Command {
	return boa.CmdT[processSkipWaitParams]{
		Use:   "skip-wait",
		Short: "End a waiting wait step now; the run goes on",
		Long: "Ends one wait step that is waiting for its time, as skipped by you: the run goes on along the step's " +
			"route as if its time had come. Needs process.runs.manage. tclaude process show lists the waiting steps.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *processSkipWaitParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *processSkipWaitParams, _ *cobra.Command, _ []string) {
			exitProcessRuntime(runProcessSkipWait(p, os.Stdout, os.Stderr), os.Stderr)
		},
	}.ToCobra()
}

func runProcessSkipWait(p *processSkipWaitParams, stdout, stderr io.Writer) error {
	if err := requireProcessRuntime(stderr); err != nil {
		return err
	}
	node := strings.TrimSpace(p.Node)
	if node == "" {
		return fmt.Errorf("--node is required")
	}
	ask, err := agent.ParseAskHuman(p.AskHuman)
	if err != nil {
		return err
	}
	request := struct {
		NodeID string `json:"nodeId"`
	}{NodeID: node}
	var response struct {
		Run processRunJSON `json:"run"`
	}
	path := "/v1/process/runs/" + url.PathEscape(strings.TrimSpace(p.RunID)) + "/skip-wait"
	if err := processRequest(p.Remote, http.MethodPost, path, request, &response,
		agent.DaemonOpts{AskHuman: ask, RetryOutput: stderr}); err != nil {
		return err
	}
	printProcessRemoteBanner(stdout, p.Remote)
	fmt.Fprintf(stdout, "Skipped the wait at %s for process run %s.\n", node, response.Run.ID)
	printProcessRun(stdout, &response.Run)
	return nil
}
