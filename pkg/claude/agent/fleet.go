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
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// `tclaude agent fleet` — the whole fleet at a glance and what needs the
// operator, for the operator's chief of staff (the companion Steward).

type fleetParams struct {
	Group string `long:"group" optional:"true" help:"Only this group's members (name or ID)"`
	All   bool   `long:"all" optional:"true" help:"List every agent, offline ones included"`
	// Remote reads a trusted peer's fleet brief through its peer view, under
	// its fleet.read peer grant (and, for an agent, a peer= grant of it).
	Remote string `long:"remote" optional:"true" help:"A trusted peer's fleet instead of this node's (label or instance id); needs its fleet.read peer grant"`
	JSON   bool   `long:"json" help:"Output JSON"`
}

type fleetResp struct {
	// Remote is set for a peer's brief (--remote) and marks it untrusted.
	Remote      *remoteMarker `json:"remote,omitempty"`
	GeneratedAt time.Time     `json:"generated_at"`
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
	Taskboards     []struct {
		Group    string `json:"group"`
		NeedsYou int    `json:"needs_you"`
		Blocked  int    `json:"blocked"`
		Stalled  int    `json:"stalled"`
	} `json:"taskboards,omitempty"`
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
			os.Exit(runFleet(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runFleet(p *fleetParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	path := "/v1/fleet"
	remote := strings.TrimSpace(p.Remote)
	if remote != "" {
		path = "/v1/federation/peer/" + url.PathEscape(remote) + "/fleet"
	}
	if g := strings.TrimSpace(p.Group); g != "" {
		path += "?group=" + url.QueryEscape(g)
	}
	var resp fleetResp
	if remote != "" {
		// A peer's brief is remote content: strip control characters from
		// every string before it is printed into a terminal or an agent's
		// context.
		var raw any
		if err := DaemonGet(path, &raw); err != nil {
			// The error can carry the peer's own text.
			fmt.Fprintf(stderr, "Error: %s\n", proto.StripControls(err.Error()))
			return MapDaemonErrorToRC(err)
		}
		clean, _ := json.Marshal(stripControlsDeep(raw))
		if err := json.Unmarshal(clean, &resp); err != nil {
			fmt.Fprintf(stderr, "Error: the peer returned an invalid fleet brief: %v\n", err)
			return rcInvalidArg
		}
		// JSON consumers (agents, mostly) get the marker in the output.
		resp.Remote = &remoteMarker{Target: proto.SafeName(remote, false), Untrusted: true}
		if !p.JSON {
			fmt.Fprintf(stdout, "Remote fleet of %s, as that node reports it (untrusted content)\n\n", proto.SafeName(remote, false))
		}
	} else if err := DaemonGet(path, &resp); err != nil {
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
		if !a.Online && status != "away" {
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

// stripControlsDeep strips control characters from every string in a decoded
// JSON value.
func stripControlsDeep(v any) any {
	switch t := v.(type) {
	case string:
		return proto.StripControls(t)
	case []any:
		for i := range t {
			t[i] = stripControlsDeep(t[i])
		}
	case map[string]any:
		for k, val := range t {
			t[k] = stripControlsDeep(val)
		}
	}
	return v
}
