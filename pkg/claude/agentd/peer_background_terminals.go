package agentd

import (
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// servePeerBackgroundTerminals lists this node's unattached tclaude terminals
// to a peer. Unrestricted trust sees the whole listing, as this node's own
// operator does. Any other peer sees only what its grants let it open over
// the federation terminal stream: the shells of groups granting it
// sessions.shell, and the agents of groups granting it sessions.watch or
// sessions.attach. Ending one (the kill route) stays unrestricted-only, and
// no row is killable here.
//
//	GET /api/background-terminals
func servePeerBackgroundTerminals(w http.ResponseWriter, _ *http.Request, v *peerView, _ peerViewRule) {
	sessions, err := listBackgroundTmuxSessions()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "tmux", "background terminals unavailable")
		return
	}
	// Not the local handler: that adds the board rows, and boards stay here.
	if db.FederationPeerUnrestricted(v.peer.InstanceID) {
		writeJSON(w, http.StatusOK, backgroundTerminalsResponse{Terminals: classifyBackgroundTerminals(sessions)})
		return
	}
	shellGroups, agentConvs := peerBackgroundGrants(v.peer.InstanceID)
	out := []backgroundTerminal{}
	for _, t := range classifyBackgroundTerminals(sessions) {
		keep := false
		switch t.Kind {
		case "shell", "group_shell":
			keep = t.Group != "" && shellGroups[t.Group] && !t.Superseded
		case "agent":
			keep = t.AgentID != "" && agentConvs[t.ConvID]
		}
		if keep {
			t.Killable = false
			out = append(out, t)
		}
	}
	writeJSON(w, http.StatusOK, backgroundTerminalsResponse{Terminals: out})
}

// peerBackgroundGrants names the live groups granting peer sessions.shell,
// and the member conversations of those granting it a terminal.
func peerBackgroundGrants(peer string) (shellGroups map[string]bool, agentConvs map[string]bool) {
	shellGroups, agentConvs = map[string]bool{}, map[string]bool{}
	groups, err := db.ListAgentGroups()
	if err != nil {
		return
	}
	for _, g := range groups {
		if g == nil || g.IsArchived() {
			continue
		}
		if fedPeerAllows(peer, g.ID, PermSessionsShell) {
			shellGroups[g.Name] = true
		}
		if !fedPeerAllows(peer, g.ID, PermSessionsWatch) && !fedPeerAllows(peer, g.ID, PermSessionsAttach) {
			continue
		}
		members, err := db.ListAgentGroupMembers(g.ID)
		if err != nil {
			continue
		}
		for _, m := range members {
			agentConvs[m.ConvID] = true
		}
	}
	return
}
