package agentd

import (
	"log/slog"
	"sort"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A group's collab resources beside its saved processes (tcl-dwzsq0): the
// drawing boards linked to it and the live pages shown into it, each with the
// operator's lock. They feed the dashboard's Resources section, so they are
// listed only to the operator (processContextVisible.human).

type processGroupDrawingBoard struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Editing      int    `json:"editing"` // agents with an open drawing board session
	AgentsLocked bool   `json:"agentsLocked,omitempty"`
}

type processGroupPage struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Title        string `json:"title"`
	Owner        string `json:"owner"`
	Kind         string `json:"kind"`
	GroupOwned   bool   `json:"groupOwned,omitempty"`
	AgentsLocked bool   `json:"agentsLocked,omitempty"`
	// State is "open" for a live page, "closed" for one saved with the group
	// that no stage serves (pages_saved.go), with why and when.
	State        string `json:"state"`
	ClosedReason string `json:"closedReason,omitempty"`
	ClosedAt     string `json:"closedAt,omitempty"`
}

// groupResources lists every group's drawing boards and pages, by group id.
func groupResources() (map[int64][]processGroupDrawingBoard, map[int64][]processGroupPage) {
	drawingBoards := map[int64][]processGroupDrawingBoard{}
	all, err := db.ListCollabDrawingBoards()
	if err != nil {
		slog.Warn("process context: drawing boards unavailable", "error", err)
	}
	editing := map[string]int{}
	if sessions, err := db.ListCollabDrawingBoardSessions(); err == nil {
		for _, s := range sessions {
			editing[s.DrawingBoardID]++
		}
	}
	locked, err := db.GroupResourceLocks(db.GroupResourceDrawingBoard)
	if err != nil {
		locked = map[string]bool{}
	}
	for _, b := range all {
		if b.GroupID == 0 {
			continue
		}
		drawingBoards[b.GroupID] = append(drawingBoards[b.GroupID], processGroupDrawingBoard{
			ID: b.ID, Title: b.Title, Editing: editing[b.ID], AgentsLocked: locked[b.ID],
		})
	}
	pages := map[int64][]processGroupPage{}
	for _, p := range livePages.list("") {
		livePages.mu.Lock()
		groupID := p.groupID
		v := processGroupPage{ID: p.id, Name: p.name, Title: p.title, Owner: p.owner, Kind: p.kind, AgentsLocked: p.agentsLocked,
			State: db.GroupPageOpen}
		livePages.mu.Unlock()
		if !pageOwnerInGroup(v.Owner, groupID) {
			continue
		}
		if isGroupPageOwner(v.Owner) {
			// The operator created it and no member has taken it over.
			v.Owner, v.GroupOwned = "", true
		}
		pages[groupID] = append(pages[groupID], v)
	}
	for groupID, closed := range savedClosedPages() {
		pages[groupID] = append(pages[groupID], closed...)
	}
	for _, list := range pages {
		sort.Slice(list, func(i, j int) bool { return list[i].Owner+"/"+list[i].Name < list[j].Owner+"/"+list[j].Name })
	}
	return drawingBoards, pages
}
