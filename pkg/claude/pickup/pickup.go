// Package pickup implements `tclaude pickup` — the operator's view of the AWB
// ready-polling processes agentd runs (agent.awb_proxy.ready_polling), and the
// way to unblock one whose dispatched issue will never finish.
//
// The daemon owns every read: it joins its dispatch row with the live AWB
// issue (fetched with the operator's credentials) and the live agent state, so
// this package is a thin renderer over GET /v1/pickup and
// POST /v1/pickup/{process}/reset. Both endpoints are human-only.
package pickup

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/common"
)

// daemonTimeout covers the daemon's parallel live AWB lookups (each bounded
// at 5s daemon-side) with headroom.
const daemonTimeout = 20 * time.Second

// Enabled reports whether the operator has configured an AWB proxy. The
// command tree is registered only then, so an unconfigured `tclaude pickup`
// is an unknown command rather than a hidden one that always fails.
func Enabled() bool {
	cfg, err := config.Load()
	return err == nil && cfg.AWBProxyEnabled()
}

// Cmd returns the `tclaude pickup` cobra command.
func Cmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "pickup",
		Short: "Inspect and reset the AWB ready-issue pickup processes",
		Long: "Inspect and reset the AWB ready-issue pickup processes agentd runs " +
			"(agent.awb_proxy.ready_polling).\n\n" +
			"Each process works one AWB issue at a time and only picks up the next one once " +
			"the current issue closes. If an issue is never completed, the process stays " +
			"blocked on it; `tclaude pickup reset <process>` releases it.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds:     []*cobra.Command{lsCmd(), resetCmd(), watchCmd()},
		RunFunc: func(_ *struct{}, cmd *cobra.Command, _ []string) {
			_ = cmd.Help()
		},
	}.ToCobra()
}

type lsParams struct {
	JSON bool `long:"json" help:"Print the raw daemon response as JSON"`
}

func lsCmd() *cobra.Command {
	return boa.CmdT[lsParams]{
		Use:         "ls",
		Aliases:     []string{"list"},
		Short:       "List pickup processes with their dispatched issue and agent status",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *lsParams, _ *cobra.Command, _ []string) {
			os.Exit(runLs(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runLs(p *lsParams, stdout, stderr io.Writer) int {
	list, err := fetchStatus()
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return agent.MapDaemonErrorToRC(err)
	}
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(list); err != nil {
			return agent.RCIOFailure
		}
		return agent.RCOK
	}
	if len(list.Processes) == 0 {
		fmt.Fprintln(stdout, "No AWB pickup processes configured (agent.awb_proxy.ready_polling).")
		return agent.RCOK
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROCESS\tWORKSPACE\tSTATE\tISSUE\tISSUE STATUS\tPHASE\tAGENT\tSESSION\tSINCE\tNOTE")
	now := time.Now()
	for _, proc := range list.Processes {
		r := rowFor(proc, now)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.process, r.workspace, r.state, r.issue, r.issueStatus, r.phase, r.agent, r.session, r.since, r.note)
	}
	if err := tw.Flush(); err != nil {
		return agent.RCIOFailure
	}
	return agent.RCOK
}

type resetParams struct {
	Process string `pos:"true" help:"Name of the pickup process to reset"`
	Issue   string `long:"issue" optional:"true" help:"Only reset if the process still holds this issue id"`
}

func resetCmd() *cobra.Command {
	return boa.CmdT[resetParams]{
		Use:   "reset",
		Short: "Release a pickup process from its current issue",
		Long: "Release a pickup process from its current issue so it polls for the next ready issue.\n\n" +
			"Only the daemon's dispatch record is removed. The AWB issue keeps its status and " +
			"assignee, and the agent that was working it keeps running; release/close the issue " +
			"and retire the agent yourself if they should not continue. If the issue is still " +
			"ready and unassigned it may be picked up again.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *resetParams, _ *cobra.Command, _ []string) {
			os.Exit(runReset(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runReset(p *resetParams, stdout, stderr io.Writer) int {
	process := strings.TrimSpace(p.Process)
	if process == "" {
		fmt.Fprintln(stderr, "Error: process name is required")
		return agent.RCInvalidArg
	}
	resp, err := resetProcess(process, strings.TrimSpace(p.Issue))
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return agent.MapDaemonErrorToRC(err)
	}
	fmt.Fprintln(stdout, resetSummary(resp))
	return agent.RCOK
}

func resetSummary(resp agent.AWBPickupResetResponse) string {
	if !resp.Reset || resp.Dispatch == nil {
		return fmt.Sprintf("Process %s has no dispatch in flight; nothing to reset.", resp.Process)
	}
	msg := fmt.Sprintf("Reset process %s: released issue %s (phase %s).", resp.Process, resp.Dispatch.IssueID, resp.Dispatch.Phase)
	if resp.Dispatch.AgentID != "" {
		msg += fmt.Sprintf(" Agent %s was not touched.", resp.Dispatch.AgentID)
	}
	return msg
}

func fetchStatus() (agent.AWBPickupList, error) {
	var out agent.AWBPickupList
	err := agent.DaemonRequest(http.MethodGet, "/v1/pickup", nil, &out, agent.DaemonOpts{Timeout: daemonTimeout})
	return out, err
}

func resetProcess(process, issueID string) (agent.AWBPickupResetResponse, error) {
	var out agent.AWBPickupResetResponse
	err := agent.DaemonRequest(http.MethodPost, "/v1/pickup/"+url.PathEscape(process)+"/reset",
		agent.AWBPickupResetRequest{IssueID: issueID}, &out, agent.DaemonOpts{Timeout: daemonTimeout})
	return out, err
}

// row is one process rendered to display strings, shared by ls and watch.
type row struct {
	process, workspace, state, issue, issueStatus, phase, agent, session, since, note string
}

func rowFor(p agent.AWBPickupProcess, now time.Time) row {
	r := row{process: p.Process, workspace: dash(p.Workspace), state: p.State,
		issue: "-", issueStatus: "-", phase: "-", agent: "-", session: "-", since: "-", note: p.Hint}
	if !p.Configured {
		r.process += " (unconfigured)"
	}
	if d := p.Dispatch; d != nil {
		r.issue = d.IssueID
		r.phase = d.Phase
		r.since = humanAge(now.Sub(d.CreatedAt))
		switch {
		case d.Issue != nil:
			r.issueStatus = d.Issue.Status
		case d.IssueError != "":
			r.issueStatus = "unknown"
			if r.note == "" {
				r.note = "AWB: " + d.IssueError
			}
		}
		if a := d.Agent; a != nil {
			switch {
			case a.Exists:
				r.agent = firstNonEmpty(a.Name, d.AgentID)
				if a.Retired {
					r.session = "retired"
				} else {
					r.session = dash(a.SessionStatus)
				}
			case a.PendingSpawn:
				r.agent, r.session = d.AgentID, "spawning"
			default:
				r.agent = "-"
			}
		}
	} else if p.LastPollAt != nil {
		r.since = "polled " + humanAge(now.Sub(*p.LastPollAt)) + " ago"
	}
	if r.note == "" {
		r.note = "-"
	}
	return r
}

func humanAge(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
