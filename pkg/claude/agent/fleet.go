package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent fleet` — the whole fleet at a glance and what needs the
// operator, for the operator's chief of staff (the companion Steward).

type fleetParams struct {
	Group string `long:"group" optional:"true" help:"Only this group's members (name or ID)"`
	All   bool   `long:"all" optional:"true" help:"List every agent, offline ones included"`
	JSON  bool   `long:"json" help:"Output JSON"`
}

type fleetResp struct {
	GeneratedAt time.Time `json:"generated_at"`
	Counts      struct {
		Agents   int `json:"agents"`
		Online   int `json:"online"`
		Working  int `json:"working"`
		Idle     int `json:"idle"`
		NeedsYou int `json:"needs_you"`
		Errored  int `json:"errored"`
		Offline  int `json:"offline"`
	} `json:"counts"`
	Attention []struct {
		Kind    string     `json:"kind"`
		AgentID string     `json:"agent_id"`
		Title   string     `json:"title"`
		Detail  string     `json:"detail"`
		URL     string     `json:"url"`
		Since   *time.Time `json:"since"`
	} `json:"attention"`
	Agents []struct {
		AgentID      string     `json:"agent_id"`
		Title        string     `json:"title"`
		Groups       []string   `json:"groups"`
		Role         string     `json:"role"`
		Online       bool       `json:"online"`
		Status       string     `json:"status"`
		Detail       string     `json:"status_detail"`
		Branch       string     `json:"branch"`
		Subagents    int        `json:"subagent_count"`
		LastActivity *time.Time `json:"last_activity"`
		Task         string     `json:"task"`
		TaskLabel    string     `json:"task_label"`
		PRs          []struct {
			URL   string `json:"url"`
			State string `json:"state"`
		} `json:"prs"`
	} `json:"agents"`
	UnreadMessages int `json:"unread_messages"`
}

func fleetCmd() *cobra.Command {
	return boa.CmdT[fleetParams]{
		Use:   "fleet",
		Short: "The whole fleet at a glance, and what needs the operator",
		Long: "Lists every active agent across all groups with its status, task and presented PRs, " +
			"and ranks what needs the operator now: pending approvals, permission prompts, questions, " +
			"failed turns, crashed agents being recovered, presented PRs, unread messages and agents idle " +
			"for a while. Offline agents are counted but only listed with --all.\n\n" +
			"Needs fleet.read (granted to the companion Steward).",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *fleetParams, _ *cobra.Command, _ []string) {
			os.Exit(runFleet(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runFleet(p *fleetParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	path := "/v1/fleet"
	if g := strings.TrimSpace(p.Group); g != "" {
		path += "?group=" + url.QueryEscape(g)
	}
	var resp fleetResp
	if err := DaemonGet(path, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(resp)
		return rcOK
	}
	renderFleet(stdout, &resp, p.All)
	return rcOK
}

func renderFleet(w io.Writer, r *fleetResp, all bool) {
	now := r.GeneratedAt
	if now.IsZero() {
		now = time.Now()
	}
	c := r.Counts
	fmt.Fprintf(w, "Fleet: %d agents, %d online (%d working, %d idle", c.Agents, c.Online, c.Working, c.Idle)
	if c.NeedsYou > 0 {
		fmt.Fprintf(w, ", %d need you", c.NeedsYou)
	}
	if c.Errored > 0 {
		fmt.Fprintf(w, ", %d failed", c.Errored)
	}
	fmt.Fprintf(w, "), %d offline\n", c.Offline)

	fmt.Fprintln(w)
	if len(r.Attention) == 0 {
		fmt.Fprintln(w, "Nothing needs you right now.")
	} else {
		fmt.Fprintln(w, "Needs you:")
		for _, a := range r.Attention {
			line := "  - "
			if a.Title != "" {
				line += a.Title + " "
			}
			line += a.Detail
			if a.Since != nil && !a.Since.IsZero() {
				line += " (" + fleetAgo(now.Sub(*a.Since)) + ")"
			}
			if a.URL != "" {
				line += " " + a.URL
			}
			if a.AgentID != "" {
				line += " [" + shortAgentID(a.AgentID, "") + "]"
			}
			fmt.Fprintln(w, line)
		}
	}

	fmt.Fprintln(w)
	group := "\x00"
	hidden := 0
	for _, a := range r.Agents {
		if !a.Online && !all {
			hidden++
			continue
		}
		g := "(no group)"
		if len(a.Groups) > 0 {
			g = strings.Join(a.Groups, ", ")
		}
		if g != group {
			fmt.Fprintf(w, "%s:\n", g)
			group = g
		}
		status := a.Status
		if !a.Online {
			status = "offline"
		} else if status == "" {
			status = "online"
		}
		line := fmt.Sprintf("  %-28s %-20s", truncate(a.Title, 28), status)
		if a.LastActivity != nil && !a.LastActivity.IsZero() {
			line += " " + fleetAgo(now.Sub(*a.LastActivity))
		}
		if a.Subagents > 0 {
			line += fmt.Sprintf(" +%d sub", a.Subagents)
		}
		if a.Role != "" {
			line += " role:" + a.Role
		}
		if a.Branch != "" {
			line += " branch:" + a.Branch
		}
		if a.Task != "" {
			label := a.TaskLabel
			if label == "" {
				label = a.Task
			}
			line += " task:" + label
		}
		for _, pr := range a.PRs {
			line += " pr:" + pr.URL
		}
		line += " [" + shortAgentID(a.AgentID, "") + "]"
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
	if hidden > 0 {
		fmt.Fprintf(w, "(%d offline agents not shown; --all lists them)\n", hidden)
	}
}

func fleetAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	}
}
