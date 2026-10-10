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
	if err != nil {
		return
	}
	var shared *statusSnapshot
	rt.publishCatalogStructure(string(raw), peers, func(peer string) bool {
		if shared == nil {
			shared = rt.sharedStatusForPeers(peers)
		}
		return rt.sendCatalog(peer, shared)
	})
}

// Advance only the peers whose replacement catalog was delivered. Transient
// rate limits retry next tick without duplicating successful peers' traffic.
func (rt *fedRuntime) publishCatalogStructure(structure string, peers []db.FederationPeer, send func(string) bool) {
	if rt.catalogStructure == nil {
		rt.catalogStructure = map[string]string{}
	}
	trusted := map[string]bool{}
	for _, p := range peers {
		trusted[p.InstanceID] = true
		if rt.isOnline(p.InstanceID) && rt.catalogStructure[p.InstanceID] != structure && send(p.InstanceID) {
			rt.catalogStructure[p.InstanceID] = structure
		}
	}
	for peer := range rt.catalogStructure {
		if !trusted[peer] {
			delete(rt.catalogStructure, peer)
		}
	}
}
