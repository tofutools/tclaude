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
	Check     *struct {
		Checks int    `json:"checks"`
		Failed int    `json:"failed"`
		Last   string `json:"last"`
	} `json:"check,omitempty"`
	Checking  bool       `json:"checking,omitempty"`
	MaxChecks int        `json:"maxChecks,omitempty"`
	GivesUpAt *time.Time `json:"givesUpAt,omitempty"`
}

// checkProgress is a check wait's progress: "3 of 20 checks made, last: no".
func (w *processRunWaitJSON) checkProgress() string {
	progress := fmt.Sprintf("%d checks made", w.Check.Checks)
	if w.MaxChecks > 0 {
		progress = fmt.Sprintf("%d of %d checks made", w.Check.Checks, w.MaxChecks)
	}
	if w.Check.Last != "" {
		progress += ", last: " + displayProcessEventField(w.Check.Last)
	}
	if w.GivesUpAt != nil {
		progress += ", gives up " + w.GivesUpAt.Local().Format("2006-01-02 15:04 MST")
	}
	return progress
}

// label is what the wait waits for, as authored: "3 days", "until <time>",
// or both.
func (w *processRunWaitJSON) label() string {
	if w == nil {
		return "wait"
	}
	if w.Check != nil {
		return "wait for a criterion"
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
	case w != nil && w.Check != nil && w.Checking:
		return w.checkProgress() + "; checking now"
	case w == nil || w.DueAt == nil:
		return "starting"
	case w.Failure != "":
		return "failing: " + displayProcessEventField(w.Failure)
	}
	left := w.DueAt.Sub(now)
	due := w.DueAt.Local().Format("2006-01-02 15:04 MST")
	if w.Check != nil {
		progress := w.checkProgress()
		if left <= 0 {
			return progress + "; checking now"
		}
		return progress + "; next check in " + left.Round(time.Second).String()
	}
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

func processCheckNowCmd() *cobra.Command {
	return boa.CmdT[processSkipWaitParams]{
		Use:   "check-now",
		Short: "Make a wait's next check now, instead of when it is due",
		Long: "A wait step that waits for a condition checks it again and again. This makes its next check now: " +
			"its count and limits stay as they are. Needs process.runs.manage. To end the wait instead, use skip-wait.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *processSkipWaitParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *processSkipWaitParams, _ *cobra.Command, _ []string) {
			exitProcessRuntime(runProcessWaitAction(p, "check-now", os.Stdout, os.Stderr), os.Stderr)
		},
	}.ToCobra()
}

func runProcessSkipWait(p *processSkipWaitParams, stdout, stderr io.Writer) error {
	return runProcessWaitAction(p, "skip-wait", stdout, stderr)
}

// runProcessWaitAction posts skip-wait or check-now for one wait step.
func runProcessWaitAction(p *processSkipWaitParams, action string, stdout, stderr io.Writer) error {
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
	path := "/v1/process/runs/" + url.PathEscape(strings.TrimSpace(p.RunID)) + "/" + action
	if err := processRequest(p.Remote, http.MethodPost, path, request, &response,
		agent.DaemonOpts{AskHuman: ask, RetryOutput: stderr}); err != nil {
		return err
	}
	printProcessRemoteBanner(stdout, p.Remote)
	if action == "check-now" {
		fmt.Fprintf(stdout, "The wait at %s checks now, for process run %s.\n", node, response.Run.ID)
	} else {
		fmt.Fprintf(stdout, "Skipped the wait at %s for process run %s.\n", node, response.Run.ID)
	}
	printProcessRun(stdout, &response.Run)
	return nil
}
