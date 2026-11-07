package agentd

import (
	"cmp"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// One resource state model for a group's things (tcl-1lth8e): the
// snapshot's group_resources lists every group's pages, boards, processes and
// shells (its own and its agents' directory shells) in one shape, so the Groups tab and the Collab rail read one model
// (resource-model.js). The server knows only whether something is live
// (running) or exists with nothing live (closed); the browser layers what it
// has open on top (open-tab, floating, minimized) and calls a live page or
// shell it has not opened "background".

const (
	resourceStateRunning = "running"
	resourceStateClosed  = "closed"

	resourceKindShell = "shell"
)

type groupResource struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	GroupID int64  `json:"group_id"`
	Title   string `json:"title"`
	State   string `json:"state"`
	Owner   string `json:"owner,omitempty"`
	Detail  string `json:"detail,omitempty"`
	// Session is a shell's tmux session, which the dashboard kills it by.
	Session string `json:"session,omitempty"`
	// Agent and Which are an agent shell's (a shell in an agent's start,
	// current or worktree directory): the agent it opens for and which of its
	// directories. A group's own shell has neither.
	Agent string `json:"agent,omitempty"`
	Which string `json:"which,omitempty"`
	// Locked is the lock against agent changes (group_resource_locks.go), for
	// the Collab panel's and tab strip's lock icon.
	Locked bool `json:"locked,omitempty"`
}

// groupResourcesSnapshot lists the resources of every active group, and the
// boards in none (group_id 0: the Groups tab's Ungrouped lists them, so one
// made from the Collab panel can be moved into a group). alive is the
// snapshot's set of live tmux sessions.
func groupResourcesSnapshot(groups []*db.AgentGroup, alive map[string]struct{}) []groupResource {
	active := map[int64]*db.AgentGroup{}
	for _, g := range groups {
		if g != nil && !g.IsArchived() {
			active[g.ID] = g
		}
	}
	out := groupPageResources(active)
	out = append(out, groupBoardResources(active)...)
	out = append(out, groupProcessResources(active)...)
	for _, kind := range []string{resourceKindBoard, resourceKindProcess} {
		locked, err := db.GroupResourceLocks(kind)
		if err != nil {
			slog.Warn("group resources: locks unavailable", "kind", kind, "error", err)
			continue
		}
		for i := range out {
			if out[i].Kind == kind && locked[out[i].ID] {
				out[i].Locked = true
			}
		}
	}
	// A group's shell lists only while its tmux session is up. It has no
	// saved state, so once it exits there is nothing left to show: a closed
	// row would stay forever with no way to remove it. The ＋ menu starts a
	// new one.
	for _, g := range active {
		session := groupTermSessionName(g.Name)
		if _, ok := alive[session]; !ok {
			continue
		}
		out = append(out, groupResource{Kind: resourceKindShell, ID: g.Name, GroupID: g.ID, Title: "Group shell", State: resourceStateRunning, Session: session, Detail: g.DefaultCwd})
	}
	out = append(out, agentShellResources(active, alive)...)
	slices.SortFunc(out, func(a, b groupResource) int {
		return cmp.Or(cmp.Compare(a.GroupID, b.GroupID), cmp.Compare(a.Kind, b.Kind), strings.Compare(a.Title, b.Title), strings.Compare(a.ID, b.ID))
	})
	return out
}

// agentShellResources are the running shells in an agent's directories
// (dashboard_term.go's termSessionNameForGroup), each under the group it was
// opened from, or else the agent's own group: the one it was spawned into
// while it is still a member, else its first. Like a group's shell, one lists
// only while its tmux session is up. Its ID is the dashboard pane key the
// shell opens under (term:<agent>:<which>:<group>), so the browser matches it
// to the tab, window or background row showing it. A shell of a superseded
// generation, or of an agent in no active group, is not a group's.
func agentShellResources(active map[int64]*db.AgentGroup, alive map[string]struct{}) []groupResource {
	var shells []backgroundTmuxSession
	for name := range alive {
		if strings.HasPrefix(name, "tclaude-term-") {
			shells = append(shells, backgroundTmuxSession{name: name})
		}
	}
	if len(shells) == 0 {
		return nil
	}
	// Only a current member of an active group has a group to list its
	// shell under, so only those conversations are hashed (classifyShells),
	// not every conversation ever: this runs on every snapshot poll.
	convs := map[string]struct{}{}
	byName := map[string]*db.AgentGroup{}
	names := make([]string, 0, len(active))
	for _, g := range active {
		byName[g.Name] = g
		names = append(names, g.Name)
		members, err := db.ListAgentGroupMembers(g.ID)
		if err != nil {
			slog.Warn("group resources: group members unavailable", "group", g.Name, "error", err)
			continue
		}
		for _, m := range members {
			if m.ConvID != "" {
				convs[m.ConvID] = struct{}{}
			}
		}
	}
	if len(convs) == 0 {
		return nil
	}
	spawnGroups, err := db.AgentSpawnGroupIDs()
	if err != nil {
		slog.Warn("group resources: spawn groups unavailable", "error", err)
	}
	var out []groupResource
	for _, t := range classifyShells(shells, nil, convs, names) {
		if t.Kind != "shell" || t.AgentID == "" || t.Superseded {
			continue
		}
		g := byName[t.Group]
		if g == nil {
			g = agentHomeGroup(t.AgentID, active, spawnGroups)
		}
		if g == nil {
			continue
		}
		start, current, worktree, _ := resolveDirs(t.ConvID)
		out = append(out, groupResource{
			Kind: resourceKindShell, ID: t.Key, GroupID: g.ID, Title: agentShellTitle(t.Label, t.Which), State: resourceStateRunning,
			Session: t.TmuxSession, Detail: pickDir(t.Which, start, current, worktree), Agent: t.AgentID, Which: t.Which,
		})
	}
	return out
}

// agentHomeGroup is the active group an agent's shell lists under: the one it
// was spawned into while it is still a member, else its first by name
// (ListGroupsForAgent's order).
func agentHomeGroup(agentID string, active map[int64]*db.AgentGroup, spawnGroups map[string]int64) *db.AgentGroup {
	groups, err := db.ListGroupsForAgent(agentID)
	if err != nil {
		slog.Warn("group resources: agent groups unavailable", "agent_id", agentID, "error", err)
		return nil
	}
	var first *db.AgentGroup
	for _, g := range groups {
		if g == nil || active[g.ID] == nil {
			continue
		}
		if g.ID == spawnGroups[agentID] {
			return g
		}
		if first == nil {
			first = g
		}
	}
	return first
}

// agentShellTitle names an agent shell for its agent, and for its directory
// when that is not the agent's current one.
func agentShellTitle(agent, which string) string {
	if which == "current" {
		return agent + " shell"
	}
	return fmt.Sprintf("%s shell (%s)", agent, which)
}

// groupPageResources are the live pages shared in a group (running) and the
// saved ones no stage serves (closed).
func groupPageResources(active map[int64]*db.AgentGroup) []groupResource {
	var out []groupResource
	live := map[string]bool{}
	for _, p := range livePages.list("") {
		livePages.mu.Lock()
		r := groupResource{Kind: resourceKindPage, ID: p.id, GroupID: p.groupID, Title: p.title, State: resourceStateRunning, Owner: p.owner, Locked: p.agentsLocked}
		livePages.mu.Unlock()
		live[r.ID] = true
		if active[r.GroupID] == nil || !pageOwnerInGroup(r.Owner, r.GroupID) {
			continue
		}
		if isGroupPageOwner(r.Owner) {
			r.Owner = ""
		}
		out = append(out, r)
	}
	rows, err := db.ListGroupPages()
	if err != nil {
		slog.Warn("group resources: saved pages unavailable", "error", err)
		return out
	}
	for _, row := range rows {
		// A saved page lists even after its shower left the group, so the
		// operator can still delete it (a reopen there answers not_member).
		if live[row.ID] || active[row.GroupID] == nil {
			continue
		}
		r := groupResource{Kind: resourceKindPage, ID: row.ID, GroupID: row.GroupID, Title: row.Title, State: resourceStateClosed,
			Owner: row.Owner, Detail: row.ClosedReason, Locked: row.AgentsLocked}
		if isGroupPageOwner(r.Owner) {
			r.Owner = ""
		}
		out = append(out, r)
	}
	return out
}

// groupBoardResources are the boards linked to a group. A board linked to
// none, or to a group that is archived, lists with group_id 0: it is the
// operator's still, and nothing else reaches it. A board has nothing that
// runs; who is editing it is its detail.
func groupBoardResources(active map[int64]*db.AgentGroup) []groupResource {
	boards, err := db.ListCollabBoards()
	if err != nil {
		slog.Warn("group resources: boards unavailable", "error", err)
		return nil
	}
	editing := map[string]int{}
	if sessions, err := db.ListCollabBoardSessions(); err == nil {
		for _, s := range sessions {
			editing[s.BoardID]++
		}
	}
	var out []groupResource
	for _, b := range boards {
		groupID := b.GroupID
		if active[groupID] == nil {
			groupID = 0
		}
		r := groupResource{Kind: resourceKindBoard, ID: b.ID, GroupID: groupID, Title: b.Title, State: resourceStateClosed}
		if r.Title == "" {
			r.Title = b.ID
		}
		if n := editing[b.ID]; n > 0 {
			r.Detail = pluralCount(n, "editing")
		}
		out = append(out, r)
	}
	return out
}

// groupProcessResources are a group's pinned processes and the templates
// it has active runs of, one per template id (the key the dashboard opens a
// process by), running while a run of it is active in the group. The title
// is the template id: the snapshot reads no template source, and the views
// name a process from the process context they already load.
func groupProcessResources(active map[int64]*db.AgentGroup) []groupResource {
	pins, err := db.ListAllProcessGroupPins()
	if err != nil {
		slog.Warn("group resources: process pins unavailable", "error", err)
	}
	var out []groupResource
	for groupID := range active {
		running := map[string]int{}
		var order []string
		runs, err := db.ListActiveGroupProcessRunSummaries(groupID, db.MaxProcessRunReadPage)
		if err != nil {
			slog.Warn("group resources: process runs unavailable", "group_id", groupID, "error", err)
		}
		if len(runs) == db.MaxProcessRunReadPage {
			slog.Debug("group resources: active runs past the read cap are not counted", "group_id", groupID, "cap", len(runs))
		}
		for _, run := range runs {
			id := processRunTemplateID(run.TemplateRef)
			if running[id] == 0 {
				order = append(order, id)
			}
			running[id]++
		}
		seen := map[string]bool{}
		add := func(id string) {
			if id == "" || seen[id] {
				return
			}
			seen[id] = true
			r := groupResource{Kind: resourceKindProcess, ID: id, GroupID: groupID, Title: id, State: resourceStateClosed}
			if n := running[id]; n > 0 {
				r.State, r.Detail = resourceStateRunning, pluralCount(n, "running")
			}
			out = append(out, r)
		}
		for _, id := range pins[groupID] {
			add(id)
		}
		for _, id := range order {
			add(id)
		}
	}
	return out
}

// processRunTemplateID is the template id of a run's template ref
// (<id>@sha256:<hash>).
func processRunTemplateID(ref string) string {
	id, _, _ := strings.Cut(ref, "@")
	return id
}

func pluralCount(n int, what string) string {
	return fmt.Sprintf("%d %s", n, what)
}
