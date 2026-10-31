package agentd

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// The fleet brief (`GET /v1/fleet`, `tclaude agent fleet`) is the whole
// fleet at a glance for the operator's chief of staff, the companion
// Steward: every active agent with its status, groups, task and presented
// PRs, plus a ranked list of what needs the operator now. `agent ls` cannot
// serve this: an agent only sees the members of its own groups, and the
// Steward lives in the steward scribe group.

const PermFleetRead = "fleet.read"

// readsWholeFleet reports whether an agent caller sees every agent and group,
// as the operator does. It widens what a listing shows and nothing else: who an
// agent can message is decided at send. A federated action runs as its source
// conv but is not that conv's local read, so it stays scoped.
func readsWholeFleet(r *http.Request, conv string) bool {
	return conv != "" && peerActionFromRequest(r) == nil && collabRoutesEnabled() && holdsPermission(conv, PermFleetRead)
}

// fleetIdleAfter is how long an online agent has to sit idle before the
// brief calls it out: long enough that a turn boundary is not news, short
// enough that a finished or stuck worker is not forgotten.
const fleetIdleAfter = 10 * time.Minute

type fleetBrief struct {
	GeneratedAt time.Time   `json:"generated_at"`
	Counts      fleetCounts `json:"counts"`
	// Attention is what needs the operator, most urgent first.
	Attention []fleetItem `json:"attention"`
	// Agents is every active agent, grouped agents first by group name.
	Agents []fleetAgent `json:"agents"`
	// UnreadMessages counts unread notifications in the Messages tab.
	UnreadMessages int `json:"unread_messages"`
}

type fleetCounts struct {
	Agents   int `json:"agents"`
	Online   int `json:"online"`
	Working  int `json:"working"`
	Idle     int `json:"idle"`
	NeedsYou int `json:"needs_you"`
	Errored  int `json:"errored"`
	Offline  int `json:"offline"`
}

type fleetAgent struct {
	AgentID      string     `json:"agent_id"`
	Title        string     `json:"title"`
	Groups       []string   `json:"groups"`
	Role         string     `json:"role,omitempty"`
	Online       bool       `json:"online"`
	Status       string     `json:"status,omitempty"`
	Detail       string     `json:"status_detail,omitempty"`
	Harness      string     `json:"harness,omitempty"`
	Model        string     `json:"model,omitempty"`
	Branch       string     `json:"branch,omitempty"`
	Subagents    int        `json:"subagent_count,omitempty"`
	LastActivity *time.Time `json:"last_activity,omitempty"`
	Task         string     `json:"task,omitempty"`
	TaskLabel    string     `json:"task_label,omitempty"`
	PRs          []fleetPR  `json:"prs,omitempty"`
}

type fleetPR struct {
	URL     string `json:"url"`
	State   string `json:"state,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// Attention kinds, in the order the brief ranks them.
const (
	fleetApproval   = "approval"   // an access request waiting on the operator
	fleetPrompt     = "prompt"     // a permission prompt in the agent's pane
	fleetQuestion   = "question"   // the agent asked the operator something
	fleetError      = "error"      // the agent's last turn failed
	fleetPRReady    = "pr"         // an open PR the agent presented
	fleetMessages   = "messages"   // unread notifications in Messages
	fleetIdleLonger = "idle"       // online and idle for a while
	fleetRecovering = "recovering" // a crashed agent: being recovered, or recovery stopped
)

var fleetRank = map[string]int{
	fleetApproval: 0, fleetPrompt: 1, fleetQuestion: 2, fleetError: 3,
	fleetRecovering: 4, fleetPRReady: 5, fleetMessages: 6, fleetIdleLonger: 7,
}

type fleetItem struct {
	Kind    string     `json:"kind"`
	AgentID string     `json:"agent_id,omitempty"`
	Title   string     `json:"title,omitempty"`
	Detail  string     `json:"detail"`
	URL     string     `json:"url,omitempty"`
	Since   *time.Time `json:"since,omitempty"`
}

func handleFleet(w http.ResponseWriter, r *http.Request) {
	convID, ok := requirePermission(w, r, PermFleetRead)
	if !ok {
		return
	}
	shared, ok := requestStatusSnapshot(w, r, nil)
	if !ok {
		return
	}
	brief, err := buildFleetBrief(shared, convID, strings.TrimSpace(r.URL.Query().Get("group")), time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if brief == nil {
		writeError(w, http.StatusNotFound, "not_found", "no group "+r.URL.Query().Get("group"))
		return
	}
	writeJSON(w, http.StatusOK, brief)
}

// buildFleetBrief assembles the brief. self (the caller's conv) is left
// out: the Steward does not need to be told about itself. A non-empty group
// narrows it to that group's members; nil means no such group.
func buildFleetBrief(shared *statusSnapshot, self, group string, now time.Time) (*fleetBrief, error) {
	groups, err := db.ListAgentGroups()
	if err != nil {
		return nil, err
	}
	if group != "" {
		g := matchVisibleGroup(groups, group)
		if g == nil {
			return nil, nil
		}
		groups = []*db.AgentGroup{g}
	}
	type membership struct {
		groups []string
		role   string
	}
	byConv := map[string]*membership{}
	for _, g := range groups {
		members, err := db.ListAgentGroupMembers(g.ID)
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			mb := byConv[m.ConvID]
			if mb == nil {
				mb = &membership{}
				byConv[m.ConvID] = mb
			}
			mb.groups = append(mb.groups, g.Name)
			if mb.role == "" {
				mb.role = m.Role
			}
		}
	}
	active, err := db.ListActiveAgents()
	if err != nil {
		return nil, err
	}
	tasks, _ := db.ListAgentTaskRefs()
	prs, err := listVisiblePresentedPRs()
	if err != nil {
		return nil, err
	}

	brief := &fleetBrief{GeneratedAt: now, Attention: []fleetItem{}, Agents: []fleetAgent{}}
	titles := map[string]string{} // agent_id → title, for approvals
	listed := map[string]bool{}   // conv → listed, for approvals under a group filter
	for _, e := range active {
		conv := e.CurrentConvID
		if conv == "" || conv == self {
			continue
		}
		mb := byConv[conv]
		if group != "" && mb == nil {
			continue
		}
		// The whole roster is walked on every brief, so stay on the shared
		// status snapshot and cached titles; only live agents pay for a
		// location (branch) lookup.
		state, rows := shared.stateFor(conv)
		fa := fleetAgent{
			AgentID: e.AgentID,
			Title:   agent.CachedTitle(conv),
			Groups:  []string{},
			Online:  isConvOnlineInSessions(rows, shared.alive),
			Harness: state.Harness,
			Model:   state.Model,
		}
		if presence, _ := db.ProjectedAgentFederationPresence(e.AgentID); presence != nil && presence.State == "away" {
			fa.Online = false
			fa.Status = "away"
		}
		if fa.Online {
			fa.Status, fa.Detail, fa.Subagents = state.Status, state.StatusDetail, state.SubagentCount
			fa.Branch = locationView(conv).Branch
		} else if fa.Status == "" {
			fa.Status = "offline"
		}
		if mb != nil {
			fa.Groups, fa.Role = mb.groups, mb.role
			sort.Strings(fa.Groups)
		}
		if at := shared.activity[conv]; at != nil {
			t := *at
			fa.LastActivity = &t
		}
		if t, ok := tasks[e.AgentID]; ok && t.URL != "" {
			fa.Task, fa.TaskLabel = t.URL, t.Label
		}
		for _, pr := range prs[e.AgentID] {
			if isTerminalPresentedPRState(pr.State) {
				continue
			}
			fa.PRs = append(fa.PRs, fleetPR{URL: pr.PRURL, State: pr.State, Summary: pr.Summary})
		}
		titles[e.AgentID] = fa.Title
		listed[conv] = true
		brief.Agents = append(brief.Agents, fa)
		countFleetAgent(&brief.Counts, fa)
		brief.Attention = append(brief.Attention, fleetAgentAttention(fa, state, now)...)
	}

	for _, req := range approvals.dashboardSnapshot() {
		// The snapshot also carries recently handled requests and
		// federation proposals; only a pending one waits on the operator.
		if req.Status != "pending" || req.Proposal != nil || (self != "" && (req.ConvID == self || req.CurrentConvID == self)) {
			continue
		}
		if group != "" && !listed[req.ConvID] && !listed[req.CurrentConvID] {
			continue
		}
		title := req.ConvTitle
		if t, ok := titles[req.AgentID]; ok && title == "" {
			title = t
		}
		item := fleetItem{Kind: fleetApproval, AgentID: req.AgentID, Title: title, Detail: "asks for " + approvalLine(req)}
		if t, err := time.Parse(time.RFC3339, req.CreatedAt); err == nil {
			item.Since = &t
		}
		brief.Attention = append(brief.Attention, item)
	}
	if group == "" {
		n, err := db.CountUnreadHumanMessages()
		if err != nil {
			return nil, err
		}
		if n > 0 {
			brief.UnreadMessages = n
			brief.Attention = append(brief.Attention, fleetItem{Kind: fleetMessages, Detail: plural(n, "unread message") + " in Messages"})
		}
	}

	sort.SliceStable(brief.Attention, func(i, j int) bool {
		a, b := brief.Attention[i], brief.Attention[j]
		if fleetRank[a.Kind] != fleetRank[b.Kind] {
			return fleetRank[a.Kind] < fleetRank[b.Kind]
		}
		// Oldest first: whoever has waited longest goes first; an item with
		// no time sorts after the timed ones.
		if (a.Since == nil) != (b.Since == nil) {
			return a.Since != nil
		}
		if a.Since != nil && !a.Since.Equal(*b.Since) {
			return a.Since.Before(*b.Since)
		}
		return a.Title < b.Title
	})
	sort.SliceStable(brief.Agents, func(i, j int) bool {
		a, b := brief.Agents[i], brief.Agents[j]
		ga, gb := groupsKey(a.Groups), groupsKey(b.Groups)
		if ga != gb {
			return ga < gb
		}
		if a.Online != b.Online {
			return a.Online
		}
		return strings.ToLower(a.Title) < strings.ToLower(b.Title)
	})
	return brief, nil
}

func countFleetAgent(c *fleetCounts, a fleetAgent) {
	c.Agents++
	if !a.Online {
		c.Offline++
		return
	}
	c.Online++
	switch a.Status {
	case session.StatusWorking, session.StatusMainAgentIdle:
		c.Working++
	case session.StatusIdle:
		c.Idle++
	case session.StatusAwaitingPermission, session.StatusAwaitingInput:
		c.NeedsYou++
	case session.StatusError:
		c.Errored++
	}
}

// fleetAgentAttention is what one agent's own state asks of the operator.
func fleetAgentAttention(a fleetAgent, state agentState, now time.Time) []fleetItem {
	var out []fleetItem
	item := func(kind, detail string) fleetItem {
		return fleetItem{Kind: kind, AgentID: a.AgentID, Title: a.Title, Detail: detail, Since: a.LastActivity}
	}
	if a.Online {
		switch a.Status {
		case session.StatusAwaitingPermission:
			out = append(out, item(fleetPrompt, "is waiting on a permission prompt"))
		case session.StatusAwaitingInput:
			out = append(out, item(fleetQuestion, "is waiting for your answer"))
		case session.StatusError:
			detail := "its last turn failed"
			if a.Detail != "" {
				detail += ": " + a.Detail
			}
			out = append(out, item(fleetError, detail))
		case session.StatusIdle:
			if a.LastActivity != nil && now.Sub(*a.LastActivity) >= fleetIdleAfter && a.Subagents == 0 {
				out = append(out, item(fleetIdleLonger, "has been idle for "+fleetAge(now.Sub(*a.LastActivity))))
			}
		}
	}
	detail := ""
	switch state.RecoveryStatus {
	case db.AgentRecoveryStatusCrashed, db.AgentRecoveryStatusRestarting, db.AgentRecoveryStatusBackoff:
		detail = "is being recovered (" + state.RecoveryStatus + ")"
	case db.AgentRecoveryStatusSuppressed:
		detail = "crashed and automatic recovery has stopped: it needs you to restart it"
	}
	if detail != "" {
		if state.RecoveryReason != "" {
			detail += ": " + state.RecoveryReason
		}
		out = append(out, item(fleetRecovering, detail))
	}
	for _, pr := range a.PRs {
		if strings.EqualFold(pr.State, "draft") {
			continue
		}
		detail := "presented a PR"
		if pr.Summary != "" {
			detail += ": " + pr.Summary
		}
		it := item(fleetPRReady, detail)
		it.URL = pr.URL
		out = append(out, it)
	}
	return out
}

// approvalLine says in a few words what an access request wants.
func approvalLine(req dashboardAccessRequest) string {
	perm := req.Perm
	if slugs, ok := strings.CutPrefix(perm, "sudo."); ok {
		return "temporary permissions for a plan (" + strings.ReplaceAll(slugs, ",", ", ") + ")"
	}
	if req.TargetGroup != "" {
		return perm + " on group " + req.TargetGroup
	}
	if req.TargetConvTitle != "" {
		return perm + " on " + req.TargetConvTitle
	}
	return perm
}

// fleetAge renders a duration the way the brief speaks it: "45s", "12m",
// "3h", "2d".
func fleetAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	default:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// groupsKey orders agents by their whole group list, which is also the CLI's
// section heading; ungrouped agents go last.
func groupsKey(groups []string) string {
	if len(groups) == 0 {
		return "\uffff"
	}
	return strings.Join(groups, ", ")
}
