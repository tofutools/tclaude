package agentd

import (
	"cmp"
	"context"
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

	// groupResourceRunPages bounds the active-run read: pages of
	// db.MaxProcessRunReadPage runs each.
	groupResourceRunPages = 8
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
func groupResourcesSnapshot(ctx context.Context, groups []*db.AgentGroup, alive map[string]struct{}, processes bool) []groupResource {
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
		out = append(out, groupProcessResources(ctx, active)...)
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

// groupProcessResources are a group's pinned processes, running while a run
// of that template is active in the group, and the group's other active
// runs.
func groupProcessResources(ctx context.Context, active map[int64]*db.AgentGroup) []groupResource {
	runs := map[int64][]db.ProcessRunSummary{}
	after := ""
	for range groupResourceRunPages {
		page, err := db.ListActiveProcessRunSummaries(after, db.MaxProcessRunReadPage)
		if err != nil {
			slog.Warn("group resources: process runs unavailable", "error", err)
			break
		}
		for _, run := range page {
			if active[run.Scope.GroupID] != nil {
				runs[run.Scope.GroupID] = append(runs[run.Scope.GroupID], run)
			}
		}
		if len(page) < db.MaxProcessRunReadPage {
			break
		}
		after = page[len(page)-1].ID
	}
	saved := processSavedTemplates()
	var out []groupResource
	for groupID := range active {
		claimed := map[string]bool{}
		for _, proc := range saved.groupProcesses(ctx, groupID) {
			r := groupResource{Kind: resourceKindProcess, ID: proc.TemplateID, GroupID: groupID, Title: proc.Name, State: resourceStateClosed}
			n := 0
			for _, run := range runs[groupID] {
				if processRunTemplateID(run.TemplateRef) == proc.TemplateID {
					claimed[run.ID] = true
					n++
				}
			}
			if n > 0 {
				r.State, r.Detail = resourceStateRunning, pluralCount(n, "running")
			}
			out = append(out, r)
		}
		for _, run := range runs[groupID] {
			if claimed[run.ID] {
				continue
			}
			out = append(out, groupResource{Kind: resourceKindProcess, ID: run.ID, GroupID: groupID,
				Title: processRunTemplateID(run.TemplateRef), State: resourceStateRunning, Detail: run.Status})
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
