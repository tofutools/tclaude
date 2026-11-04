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
// shell in one shape, so the Groups tab and the Collab rail read one model
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
}

// groupResourcesSnapshot lists the resources of every active group. alive is
// the snapshot's set of live tmux sessions; processes says whether the
// processes feature is on.
func groupResourcesSnapshot(groups []*db.AgentGroup, alive map[string]struct{}, processes bool) []groupResource {
	active := map[int64]*db.AgentGroup{}
	for _, g := range groups {
		if g != nil && !g.IsArchived() {
			active[g.ID] = g
		}
	}
	if len(active) == 0 {
		return nil
	}
	out := groupPageResources(active)
	out = append(out, groupBoardResources(active)...)
	if processes {
		out = append(out, groupProcessResources(active)...)
	}
	for _, g := range active {
		if _, ok := alive[groupTermSessionName(g.Name)]; ok {
			out = append(out, groupResource{Kind: resourceKindShell, ID: g.Name, GroupID: g.ID, Title: g.Name + " shell", State: resourceStateRunning})
		}
	}
	slices.SortFunc(out, func(a, b groupResource) int {
		return cmp.Or(cmp.Compare(a.GroupID, b.GroupID), cmp.Compare(a.Kind, b.Kind), strings.Compare(a.Title, b.Title), strings.Compare(a.ID, b.ID))
	})
	return out
}

// groupPageResources are the live pages shared in a group (running) and the
// saved ones no stage serves (closed).
func groupPageResources(active map[int64]*db.AgentGroup) []groupResource {
	var out []groupResource
	live := map[string]bool{}
	for _, p := range livePages.list("") {
		livePages.mu.Lock()
		r := groupResource{Kind: resourceKindPage, ID: p.id, GroupID: p.groupID, Title: p.title, State: resourceStateRunning, Owner: p.owner}
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
			Owner: row.Owner, Detail: row.ClosedReason}
		if isGroupPageOwner(r.Owner) {
			r.Owner = ""
		}
		out = append(out, r)
	}
	return out
}

// groupBoardResources are the boards linked to a group. A board has nothing
// that runs; who is editing it is its detail.
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
		if active[b.GroupID] == nil {
			continue
		}
		r := groupResource{Kind: resourceKindBoard, ID: b.ID, GroupID: b.GroupID, Title: b.Title, State: resourceStateClosed}
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
