package agentd

import (
	"log/slog"
	"sort"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A group's collab resources beside its saved processes (tcl-dwzsq0): the
// boards linked to it and the live pages shown into it, each with the
// operator's lock. They feed the dashboard's Resources section, so they are
// listed only to the operator (processContextVisible.human).

type processGroupBoard struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Editing      int    `json:"editing"` // agents with an open board session
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
}

// groupResources lists every group's boards and pages, by group id.
func groupResources() (map[int64][]processGroupBoard, map[int64][]processGroupPage) {
	boards := map[int64][]processGroupBoard{}
	all, err := db.ListCollabBoards()
	if err != nil {
		slog.Warn("process context: boards unavailable", "error", err)
	}
	editing := map[string]int{}
	if sessions, err := db.ListCollabBoardSessions(); err == nil {
		for _, s := range sessions {
			editing[s.BoardID]++
		}
	}
	locked, err := db.GroupResourceLocks(db.GroupResourceBoard)
	if err != nil {
		locked = map[string]bool{}
	}
	for _, b := range all {
		if b.GroupID == 0 {
			continue
		}
		boards[b.GroupID] = append(boards[b.GroupID], processGroupBoard{
			ID: b.ID, Title: b.Title, Editing: editing[b.ID], AgentsLocked: locked[b.ID],
		})
	}
	pages := map[int64][]processGroupPage{}
	for _, p := range livePages.list("") {
		livePages.mu.Lock()
		groupID := p.groupID
		v := processGroupPage{ID: p.id, Name: p.name, Title: p.title, Owner: p.owner, Kind: p.kind, AgentsLocked: p.agentsLocked}
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
	for _, list := range pages {
		sort.Slice(list, func(i, j int) bool { return list[i].Owner+"/"+list[i].Name < list[j].Owner+"/"+list[j].Name })
	}
	return boards, pages
}
