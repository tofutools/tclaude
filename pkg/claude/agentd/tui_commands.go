package agentd

import (
	"net/url"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

func tuiTickCmd() tea.Cmd {
	return tea.Tick(tuiRefreshInterval, func(t time.Time) tea.Msg { return tuiTickMsg(t) })
}

// refreshCmd re-reads both listings off the daemon API. It runs as a
// bubbletea command (i.e. on its own goroutine) because the peers listing
// touches tmux and the conversation index, and the console must stay
// responsive while it does.
func (m tuiModel) refreshCmd() tea.Cmd {
	api := m.api
	generation := m.refreshGeneration
	withSessions := m.listsLocalSessions()
	return func() tea.Msg {
		var agents []tuiAgentRow
		if err := api.get("/v1/peers", &agents); err != nil {
			return tuiDataMsg{refreshGeneration: generation, err: err}
		}
		var groups []tuiGroupRow
		if err := api.get("/v1/groups", &groups); err != nil {
			return tuiDataMsg{refreshGeneration: generation, err: err}
		}
		// The profile list feeds one field of a form that is usually closed,
		// so its failure travels beside the listing rather than instead of it:
		// a console whose agents are all live and visible must not go blank
		// because a profile read hit the DB at a bad moment.
		var profiles []tuiProfileRow
		profilesErr := api.get("/v1/spawn-profiles", &profiles)
		// The host's own sessions, which have no /v1 shape — see
		// tui_sessions.go. Their failure travels beside the listing for the
		// same reason the profiles' does.
		var sessions []tuiSessionRow
		var sessionsErr error
		if withSessions {
			sessions, sessionsErr = tuiListLocalSessions()
		}
		sortTUIAgents(agents)
		sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
		sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
		return tuiDataMsg{
			refreshGeneration: generation,
			agents:            agents,
			groups:            groups,
			profiles:          profiles,
			sessions:          sessions,
			profilesErr:       profilesErr,
			sessionsErr:       sessionsErr,
		}
	}
}

// beginRefresh assigns each poll a generation so that a slower, older poll
// cannot overwrite newer state or settle mutation reconciliation.
func (m tuiModel) beginRefresh() (tuiModel, tea.Cmd) {
	m.refreshGeneration++
	m.refreshing = true
	return m, m.refreshCmd()
}

// sortTUIAgents orders the listing online-first, then by name, so the
// agents that can act sit at the top and a row keeps its place between
// refreshes.
func sortTUIAgents(agents []tuiAgentRow) {
	sort.SliceStable(agents, func(i, j int) bool {
		if agents[i].Online != agents[j].Online {
			return agents[i].Online
		}
		return strings.ToLower(agents[i].name()) < strings.ToLower(agents[j].name())
	})
}

// tuiResolveWorktreeCmd asks the daemon for the worktree the spawn form
// picked, before the spawn itself goes out. It runs as its own command
// because it is a git operation on the daemon's host — a fetchless one, but
// still a subprocess — and because its failures are the operator's to fix in
// the form (a branch that cannot be cut, a directory that is not a repo)
// rather than something to report over a listing they have already been
// returned to.
func tuiResolveWorktreeCmd(
	api tuiAPI, wtReq tuiWorktreeRequest, group string, formGen uint64, req agent.SpawnRequest,
) tea.Cmd {
	return func() tea.Msg {
		var resp tuiWorktreeResponse
		err := api.post(tuiWorktreePath, wtReq, &resp)
		return tuiWorktreeResolvedMsg{group: group, formGen: formGen, req: req, wt: resp, err: err}
	}
}

func tuiSpawnCmd(api tuiAPI, group string, req agent.SpawnRequest) tea.Cmd {
	return func() tea.Msg {
		var resp agent.SpawnResponse
		err := api.post("/v1/groups/"+url.PathEscape(group)+"/spawn", req, &resp)
		return tuiSpawnedMsg{group: group, resp: resp, err: err}
	}
}

// tuiRetireCmd retires one agent through the daemon's own verb. The
// require_offline precondition keeps Delete's progressive contract true even
// when another client resumes the agent while the confirmation is open. The
// remaining documented defaults still apply: the pane is asked to exit and
// the worktree is left alone.
func tuiRetireCmd(api tuiAPI, convID, name string) tea.Cmd {
	return func() tea.Msg {
		var res tuiRetireResult
		err := api.post("/v1/agent/"+url.PathEscape(convID)+"/retire?require_offline=1", nil, &res)
		return tuiRetiredMsg{agent: name, res: res, err: err}
	}
}

// tuiStopCmd turns one online agent off through the daemon's own stop verb.
// It sends no force query: Delete is the graceful step toward removal, so it
// asks the harness to exit cleanly and never drops unsubmitted input.
func tuiStopCmd(api tuiAPI, convID, name string) tea.Cmd {
	return func() tea.Msg {
		var res tuiStopResult
		err := api.post("/v1/agent/"+url.PathEscape(convID)+"/stop", nil, &res)
		return tuiStoppedMsg{agent: name, res: res, err: err}
	}
}

// tuiResumeCmd turns one offline agent back on through the daemon's own
// resume verb. Like the retire above it sends no query parameters, which
// leaves ?recreate=1 off: re-creating a launch directory the operator has
// since deleted is a decision the console has no way to put to them, so a
// vanished directory comes back as an error they can act on instead.
func tuiResumeCmd(api tuiAPI, convID, name string) tea.Cmd {
	return func() tea.Msg {
		var res tuiResumeResult
		err := api.post("/v1/agent/"+url.PathEscape(convID)+"/resume", nil, &res)
		return tuiResumedMsg{agent: name, res: res, err: err}
	}
}
