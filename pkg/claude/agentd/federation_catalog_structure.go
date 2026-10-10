package agentd

import (
	"encoding/json"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/client"
)

// The existing two-second observer notices structural changes made through any
// local write path, including spawn/enrollment and another process. Session and
// status deltas cannot introduce or withdraw catalog groups, or update rosters.
// Compare only structure here: no tmux/status gather and no fanout when unchanged.
// The periodic full catalog refresh still rebuilds from live DB authority.
func (rt *fedRuntime) pushCatalogStructureChanges() {
	if rt.cl == nil || rt.cl.Status().State != client.StateConnected {
		return
	}
	peers, err := db.ListFederationPeers()
	if err != nil || len(peers) == 0 {
		return
	}
	groups, err := db.ListAgentGroups()
	if err != nil {
		return
	}
	type member struct {
		Agent string
		Conv  string
		Name  string
		Role  string
	}
	type group struct {
		ID          int64
		Name        string
		Description string
		Members     []member
	}
	rows := []group{}
	for _, g := range groups {
		if g.IsArchived() {
			continue
		}
		row := group{ID: g.ID, Name: g.Name, Description: g.Descr}
		members, err := db.ListAgentGroupMembers(g.ID)
		if err != nil {
			return
		}
		for _, m := range members {
			aid, err := db.AgentIDForConv(m.ConvID)
			if err != nil {
				return
			}
			a, err := db.GetAgent(aid)
			if err != nil {
				return
			}
			if a == nil || !a.Active() {
				continue
			}
			row.Members = append(row.Members, member{Agent: aid, Conv: m.ConvID, Name: agent.TitleFor(m.ConvID), Role: m.Role})
		}
		rows = append(rows, row)
	}
	raw, err := json.Marshal(rows)
	if err != nil || string(raw) == rt.catalogStructure {
		return
	}
	rt.catalogStructure = string(raw)
	rt.broadcastCatalogs()
}
