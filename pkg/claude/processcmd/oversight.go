package processcmd

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
)

// Run oversight on the command line: raise a concern about a run, answer one
// you hold, list a run's concerns, and set who oversees a run. The daemon
// side is pkg/claude/agentd/process_oversight*.go.

// processRunOverseerJSON mirrors the daemon's overseer view.
type processRunOverseerJSON struct {
	Ref               string `json:"ref"`
	AgentID           string `json:"agentId,omitempty"`
	Name              string `json:"name"`
	Via               string `json:"via"`
	EscalationAgentID string `json:"escalationAgentId,omitempty"`
	EscalationName    string `json:"escalationName,omitempty"`
}

// processRunConcernJSON mirrors the daemon's concern view.
type processRunConcernJSON struct {
	ID         int64      `json:"id"`
	NodeID     string     `json:"nodeId,omitempty"`
	Source     string     `json:"source"`
	Signal     string     `json:"signal,omitempty"`
	RaisedBy   string     `json:"raisedBy"`
	RaisedName string     `json:"raisedName"`
	Reason     string     `json:"reason"`
	State      string     `json:"state"`
	Level      string     `json:"level"`
	Holder     string     `json:"holder,omitempty"`
	HolderName string     `json:"holderName"`
	AnswerBy   *time.Time `json:"answerBy,omitempty"`
	MessageID  int64      `json:"messageId,omitempty"`
	Choices    []string   `json:"choices,omitempty"`
	Resolution string     `json:"resolution,omitempty"`
	ResolvedBy string     `json:"resolvedBy,omitempty"`
	Note       string     `json:"note,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	ResolvedAt *time.Time `json:"resolvedAt,omitempty"`
}

func (o processRunOverseerJSON) String() string {
	label := o.Name
	if label == "" {
		label = o.Ref
	}
	via := map[string]string{"set": "set on the run", "lead": "the group's lead", "starter": "started the run"}[o.Via]
	if via != "" {
		label += " (" + via + ")"
	}
	if o.EscalationName != "" {
		label += "; escalates to " + o.EscalationName
	}
	return label
}

// printProcessRunOversight renders the overseer line and the open concerns.
func printProcessRunOversight(out io.Writer, run *processRunJSON) {
	if run.Overseer != nil {
		fmt.Fprintf(out, "overseer=%s\n", displayProcessEventField(run.Overseer.String()))
	}
	var open []processRunConcernJSON
	for _, c := range run.Concerns {
		if c.State == "open" {
			open = append(open, c)
		}
	}
	if len(open) == 0 {
		return
	}
	fmt.Fprintln(out, "concerns:")
	tw := newTable(out)
	fmt.Fprintln(tw, "ID\tSTEP\tWITH\tFROM\tREASON")
	for _, c := range open {
		with := c.HolderName
		if with == "" {
			with = c.Level
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", c.ID, displayProcessEventField(c.NodeID),
			displayProcessEventField(with), displayProcessEventField(c.RaisedName),
			displayProcessEventField(processConcernLine(c.Reason)))
	}
	_ = tw.Flush()
}

func processConcernLine(reason string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(reason), "\n")
	if r := []rune(line); len(r) > 72 {
		line = string(r[:71]) + "…"
	}
	return line
}

// printProcessRunOversightNextSteps names how open concerns are answered.
func printProcessRunOversightNextSteps(out io.Writer, run *processRunJSON) {
	for _, c := range run.Concerns {
		if c.State != "open" {
			continue
		}
		if c.Level == "operator" {
			fmt.Fprintf(out, "Next: concern #%d waits on the operator's decision (Messages, or the run in the dashboard)\n", c.ID)
			continue
		}
		fmt.Fprintf(out, "Next: tclaude process answer-concern %s %d <continue|pause|escalate> --note \"…\"  (%s holds it)\n",
			run.ID, c.ID, displayProcessEventField(c.HolderName))
	}
}

type processConcernParams struct {
	RunID    string `pos:"true" help:"Process run id"`
	Reason   string `pos:"true" help:"Why: what is wrong or stuck, in a sentence or two"`
	Node     string `long:"node" optional:"true" help:"The step the concern is about"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout"`
}

func processConcernCmd() *cobra.Command {
	return boa.CmdT[processConcernParams]{
		Use:   "concern",
		Short: "Raise a concern about a process run with its overseer",
		Long: "Anyone taking part in a run (its overseer, its group's members, its performers, whoever started it) " +
			"can raise a concern. It goes to the run's overseer, who continues, pauses or escalates it; an overseer " +
			"that does not answer in time is passed over and the run pauses. The chain is the overseer, then the " +
			"group's escalation agent (a member with the role \"escalation\" holding process.runs.manage), then the " +
			"operator, who gets a decision card. Raising a concern you would hold yourself sends it up the chain.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *processConcernParams, _ *cobra.Command, _ []string) {
			exitProcessRuntime(runProcessConcern(p, os.Stdout, os.Stderr), os.Stderr)
		},
	}.ToCobra()
}

func runProcessConcern(p *processConcernParams, stdout, stderr io.Writer) error {
	if err := requireProcessRuntime(stderr); err != nil {
		return err
	}
	reason := strings.TrimSpace(p.Reason)
	if reason == "" {
		return fmt.Errorf("a concern needs a reason")
	}
	ask, err := agent.ParseAskHuman(p.AskHuman)
	if err != nil {
		return err
	}
	request := map[string]any{"reason": reason}
	if node := strings.TrimSpace(p.Node); node != "" {
		request["nodeId"] = node
	}
	var response struct {
		Concern processRunConcernJSON `json:"concern"`
	}
	path := "/v1/process/runs/" + url.PathEscape(strings.TrimSpace(p.RunID)) + "/concerns"
	if err := agent.DaemonRequest(http.MethodPost, path, request, &response,
		agent.DaemonOpts{AskHuman: ask, RetryOutput: stderr}); err != nil {
		return err
	}
	c := response.Concern
	fmt.Fprintf(stdout, "Raised concern #%d on process run %s; it is with %s.\n", c.ID, strings.TrimSpace(p.RunID),
		displayProcessEventField(processConcernHolder(c)))
	return nil
}

func processConcernHolder(c processRunConcernJSON) string {
	if c.HolderName != "" {
		return c.HolderName
	}
	return "the " + c.Level
}

type processAnswerConcernParams struct {
	RunID     string `pos:"true" help:"Process run id"`
	ConcernID string `pos:"true" help:"Concern number"`
	Action    string `pos:"true" help:"continue (the run goes on, and resumes if paused), pause (hold the run) or escalate (pass it up the chain)"`
	Note      string `long:"note" optional:"true" help:"Why, for the run's log and whoever raised it"`
	AskHuman  string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout"`
}

func processAnswerConcernCmd() *cobra.Command {
	return boa.CmdT[processAnswerConcernParams]{
		Use:   "answer-concern",
		Short: "Answer a concern you hold on a process run: continue, pause or escalate",
		Long: "The concern's holder (the run's overseer, or the group's escalation agent once it is passed up) answers it. " +
			"Anyone holding process.runs.manage may answer at any level. A concern with the operator is answered on " +
			"its decision card instead.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *processAnswerConcernParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Action).SetAlternatives([]string{"continue", "pause", "escalate"})
			return nil
		},
		RunFunc: func(p *processAnswerConcernParams, _ *cobra.Command, _ []string) {
			exitProcessRuntime(runProcessAnswerConcern(p, os.Stdout, os.Stderr), os.Stderr)
		},
	}.ToCobra()
}

func runProcessAnswerConcern(p *processAnswerConcernParams, stdout, stderr io.Writer) error {
	if err := requireProcessRuntime(stderr); err != nil {
		return err
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(p.ConcernID), "#"), 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("concern must be a positive number")
	}
	ask, err := agent.ParseAskHuman(p.AskHuman)
	if err != nil {
		return err
	}
	request := map[string]any{"action": strings.TrimSpace(p.Action)}
	if note := strings.TrimSpace(p.Note); note != "" {
		request["note"] = note
	}
	var response struct {
		Concern processRunConcernJSON `json:"concern"`
	}
	path := fmt.Sprintf("/v1/process/runs/%s/concerns/%d/answer", url.PathEscape(strings.TrimSpace(p.RunID)), id)
	if err := agent.DaemonRequest(http.MethodPost, path, request, &response,
		agent.DaemonOpts{AskHuman: ask, RetryOutput: stderr}); err != nil {
		return err
	}
	c := response.Concern
	if c.State == "open" {
		fmt.Fprintf(stdout, "Concern #%d passed on; it is with %s.\n", c.ID, displayProcessEventField(processConcernHolder(c)))
		return nil
	}
	fmt.Fprintf(stdout, "Concern #%d settled: %s.\n", c.ID, displayProcessEventField(c.Resolution))
	return nil
}

type processConcernsParams struct {
	RunID    string `pos:"true" help:"Process run id"`
	All      bool   `long:"all" optional:"true" help:"Also list settled concerns"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout"`
}

func processConcernsCmd() *cobra.Command {
	return boa.CmdT[processConcernsParams]{
		Use:         "concerns",
		Short:       "List a process run's concerns and who oversees it",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *processConcernsParams, _ *cobra.Command, _ []string) {
			exitProcessRuntime(runProcessConcerns(p, os.Stdout, os.Stderr), os.Stderr)
		},
	}.ToCobra()
}

func runProcessConcerns(p *processConcernsParams, stdout, stderr io.Writer) error {
	if err := requireProcessRuntime(stderr); err != nil {
		return err
	}
	ask, err := agent.ParseAskHuman(p.AskHuman)
	if err != nil {
		return err
	}
	run, err := fetchProcessRun(strings.TrimSpace(p.RunID), ask, stderr)
	if err != nil {
		return err
	}
	if run.Overseer != nil {
		fmt.Fprintf(stdout, "overseer=%s\n", displayProcessEventField(run.Overseer.String()))
	}
	tw := newTable(stdout)
	fmt.Fprintln(tw, "ID\tSTATE\tSTEP\tWITH\tFROM\tREASON\tOUTCOME")
	shown := 0
	for _, c := range run.Concerns {
		if c.State != "open" && !p.All {
			continue
		}
		shown++
		with, outcome := processConcernHolder(c), ""
		if c.State != "open" {
			with = "-"
			outcome = c.Resolution
			if c.Note != "" {
				outcome += ": " + processConcernLine(c.Note)
			}
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", c.ID, c.State, displayProcessEventField(c.NodeID),
			displayProcessEventField(with), displayProcessEventField(c.RaisedName),
			displayProcessEventField(processConcernLine(c.Reason)), displayProcessEventField(outcome))
	}
	if shown == 0 {
		fmt.Fprintln(stdout, "No open concerns.")
		return nil
	}
	return tw.Flush()
}

type processOverseeParams struct {
	RunID    string `pos:"true" help:"Process run id"`
	Overseer string `pos:"true" help:"Agent name, agent id or conv id; operator; or default (the group's lead, else whoever started the run)"`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout"`
}

func processOverseeCmd() *cobra.Command {
	return boa.CmdT[processOverseeParams]{
		Use:         "oversee",
		Short:       "Set who oversees a process run (needs process.runs.manage)",
		Long:        "The overseer answers the run's concerns and may pause, resume and reassign its steps. Without one set, a run is overseen by its group's lead, else by whoever started it.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *processOverseeParams, _ *cobra.Command, _ []string) {
			exitProcessRuntime(runProcessOversee(p, os.Stdout, os.Stderr), os.Stderr)
		},
	}.ToCobra()
}

func runProcessOversee(p *processOverseeParams, stdout, stderr io.Writer) error {
	if err := requireProcessRuntime(stderr); err != nil {
		return err
	}
	ask, err := agent.ParseAskHuman(p.AskHuman)
	if err != nil {
		return err
	}
	var response struct {
		Changed  bool                    `json:"changed"`
		Overseer *processRunOverseerJSON `json:"overseer"`
	}
	path := "/v1/process/runs/" + url.PathEscape(strings.TrimSpace(p.RunID)) + "/overseer"
	if err := agent.DaemonRequest(http.MethodPost, path, map[string]any{"overseer": strings.TrimSpace(p.Overseer)}, &response,
		agent.DaemonOpts{AskHuman: ask, RetryOutput: stderr}); err != nil {
		return err
	}
	label := "-"
	if response.Overseer != nil {
		label = response.Overseer.String()
	}
	if response.Changed {
		fmt.Fprintf(stdout, "Process run %s is now overseen by %s.\n", strings.TrimSpace(p.RunID), displayProcessEventField(label))
	} else {
		fmt.Fprintf(stdout, "Process run %s is already overseen by %s.\n", strings.TrimSpace(p.RunID), displayProcessEventField(label))
	}
	return nil
}
