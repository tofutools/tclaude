package workflows

import (
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/ccworkflows"
	"github.com/tofutools/tclaude/pkg/common"
)

// ShowParams configures `workflows show`.
type ShowParams struct {
	RunID    string  `pos:"true" help:"The run id (wf_...) to show."`
	Script   bool    `long:"script" help:"Print the workflow script behind the run instead of the tree."`
	Mermaid  bool    `long:"mermaid" help:"Emit a mermaid flowchart of the phase → agent fan-out (paste into GitHub / mermaid.live / docs)."`
	JSON     bool    `long:"json" help:"Emit the full run state as JSON."`
	Watch    bool    `long:"watch" help:"Follow an in-flight run: redraw the tree on a poll until it finishes (Ctrl-C to stop)."`
	Interval float64 `long:"interval" help:"Watch poll interval in seconds (floor 0.5)." default:"2"`
}

// ShowCmd returns the `workflows show` subcommand.
func ShowCmd() *cobra.Command {
	return boa.CmdT[ShowParams]{
		Use:   "show <runId>",
		Short: "Show a run's phase/agent fan-out tree (or its script)",
		Long: "Show a workflow run: overall status plus the phase → agent fan-out tree with\n" +
			"each agent's state, model, and token usage. --script dumps the script behind\n" +
			"the run; --mermaid emits a portable mermaid flowchart of the same tree (paste\n" +
			"into GitHub markdown, mermaid.live, or a doc); --json emits the full typed run\n" +
			"state; --watch follows an in-flight run, redrawing the tree on a poll until it\n" +
			"finishes (Ctrl-C to stop).\n\n" +
			"For an in-flight run, agent labels/phases are recovered by correlating the live\n" +
			"journal with the script's spawn order — best-effort for dynamic fan-out (marked\n" +
			"with ~ in the tree); token usage is a live estimate the completed record finalises.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *ShowParams, _ *cobra.Command, _ []string) {
			if code := RunShow(params, os.Stdout, os.Stderr); code != 0 {
				os.Exit(code)
			}
		},
	}.ToCobra()
}

// RunShow is the testable core of `workflows show`.
func RunShow(params *ShowParams, stdout, stderr io.Writer) int {
	if params.RunID == "" {
		fmt.Fprintln(stderr, "Error: a run id is required (see `workflows ls`).")
		return 2
	}

	// --watch follows the live tree; the one-shot --script/--mermaid/--json modes
	// take precedence (there is nothing to follow in a single dump).
	if params.Watch && !params.Script && !params.Mermaid && !params.JSON {
		return startWatch(params, stdout, stderr)
	}

	rs, ref, err := ccworkflows.FindRun(params.RunID)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	if params.Script {
		if rs.Script == "" {
			fmt.Fprintf(stderr, "Error: no script available for run %q.\n", params.RunID)
			return 1
		}
		fmt.Fprint(stdout, rs.Script)
		if !endsWithNewline(rs.Script) {
			fmt.Fprintln(stdout)
		}
		return 0
	}

	if params.Mermaid {
		mm := ccworkflows.Mermaid(rs)
		fmt.Fprint(stdout, mm)
		if !endsWithNewline(mm) {
			fmt.Fprintln(stdout)
		}
		return 0
	}

	if params.JSON {
		return writeJSON(stdout, stderr, rs)
	}

	writeRunTree(stdout, rs, ref)
	return 0
}

func writeRunTree(w io.Writer, rs *ccworkflows.RunState, ref *ccworkflows.RunRef) {
	fmt.Fprintf(w, "Run %s  [%s]", rs.RunID, rs.Status)
	if rs.WorkflowName != "" {
		fmt.Fprintf(w, "  workflow: %s", rs.WorkflowName)
	}
	fmt.Fprintln(w)

	// Metadata line.
	session := ""
	if ref != nil {
		session = shortID(ref.SessionID)
	}
	fmt.Fprintf(w, "Source: %s   Started: %s   Duration: %s   Agents: %d   Tokens: %s",
		rs.Source, fmtTimeMs(rs.StartTimeMs), fmtDurationMs(rs.DurationMs), rs.AgentCount, fmtCount(rs.TotalTokens))
	if session != "" {
		fmt.Fprintf(w, "   Session: %s", session)
	}
	fmt.Fprintln(w)
	if rs.Summary != "" {
		fmt.Fprintf(w, "Summary: %s\n", firstLine(rs.Summary, 100))
	}

	// Group agents by phase index.
	byPhase := map[int][]ccworkflows.Agent{}
	for _, a := range rs.Agents {
		byPhase[a.PhaseIndex] = append(byPhase[a.PhaseIndex], a)
	}

	for _, p := range rs.Phases {
		fmt.Fprintf(w, "\nPhase %d: %s  [%s]", p.Index, p.Title, dashIfEmpty(string(p.Status)))
		if p.Detail != "" {
			fmt.Fprintf(w, "  — %s", firstLine(p.Detail, 70))
		}
		fmt.Fprintln(w)
		writeAgentRows(w, byPhase[p.Index])
		delete(byPhase, p.Index)
	}

	// Any agents not attached to a known phase (phaseIndex 0 / unmapped).
	var orphans []ccworkflows.Agent
	for _, ags := range byPhase {
		orphans = append(orphans, ags...)
	}
	if len(orphans) > 0 {
		fmt.Fprintln(w, "\n(unassigned agents)")
		writeAgentRows(w, orphans)
	}
}

func writeAgentRows(w io.Writer, agents []ccworkflows.Agent) {
	if len(agents) == 0 {
		return
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	for _, a := range agents {
		label := a.Label
		if label == "" {
			label = shortID(a.ID)
		}
		// A leading ~ flags a best-effort (low-confidence) label.
		marker := "  • "
		if !a.LabelConfident {
			marker = "  ~ "
		}
		model := a.Model
		if model == "" {
			model = "-"
		}
		fmt.Fprintf(tw, "%s%s\t[%s]\t%s\ttokens=%s\ttools=%d\n",
			marker, label, a.State, model, fmtCount(a.Tokens), a.ToolCalls)
	}
	_ = tw.Flush()
}

func endsWithNewline(s string) bool {
	return len(s) > 0 && s[len(s)-1] == '\n'
}
