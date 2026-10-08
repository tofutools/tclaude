package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

type fedStatusSent struct {
	hash string
	at   time.Time
}

func (rt *fedRuntime) sharedStatusForPeers(peers []db.FederationPeer) *statusSnapshot {
	groups, _ := db.ListAgentGroups()
	for _, p := range peers {
		if !rt.sessionPeerOnline(p.InstanceID) {
			continue
		}
		for _, g := range groups {
			if !g.IsArchived() && fedPeerAllows(p.InstanceID, g.ID, PermAgentsStatusRead) {
				return gatheredStatusSnapshot()
			}
		}
	}
	return nil
}
func fedGroupAgentStatuses(gid int64, s *statusSnapshot) []proto.AgentStatus {
	out := []proto.AgentStatus{}
	members, _ := db.ListAgentGroupMembers(gid)
	for _, m := range members {
		aid, _ := db.AgentIDForConv(m.ConvID)
		a, _ := db.GetAgent(aid)
		if a == nil || !a.Active() || a.CurrentConvID != m.ConvID {
			continue
		}
		st, known := s.states[m.ConvID]
		if !known {
			continue
		}
		row := proto.AgentStatus{Agent: aid, Name: s.names[m.ConvID], Role: m.Role, Online: isConvOnlineInSessions(s.sessions[m.ConvID], s.alive), Status: st.Status, WaitingReason: fedWaitingReason(st.Status), Harness: st.Harness, Model: st.Model, Effort: st.EffortLevel, Subagents: st.SubagentCount, BackgroundShells: st.BgShellCount, Monitors: st.MonitorCount, LastActivity: s.activity[m.ConvID], ExitReason: coarseStatusExitReason(st.ExitReason), RecoveryStatus: st.RecoveryStatus}
		task := taskRefViewFor(s.tasks[m.ConvID])
		row.TaskURL = task.TaskURL
		row.TaskLabel = task.TaskLabel
		if st.ContextWindowSize > 0 {
			row.Context = &proto.AgentContext{Percent: st.ContextPct, InputTokens: st.TokensInput, OutputTokens: st.TokensOutput, Window: st.ContextWindowSize}
		}
		out = append(out, row)
	}
	out = proto.SanitizeAgentStatuses(out)
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })
	return out
}

// This is called by the existing sessions observer. It adds no timer and uses
// the same gathered cache as dashboard, CLI and tools, once for the whole fanout.
func (rt *fedRuntime) pushAgentStatuses() {
	peers, err := db.ListFederationPeers()
	if err != nil {
		return
	}
	shared := rt.sharedStatusForPeers(peers)
	if shared == nil {
		rt.agentStatusMu.Lock()
		rt.agentStatusSent = nil
		rt.agentStatusMu.Unlock()
		return
	}
	debounce := max(2*time.Second, statusSnapshotWindow())
	groups, _ := db.ListAgentGroups()
	rows := map[int64][]proto.AgentStatus{}
	active := map[string]bool{}
	rt.agentStatusMu.Lock()
	defer rt.agentStatusMu.Unlock()
	if rt.agentStatusSent == nil {
		rt.agentStatusSent = map[string]fedStatusSent{}
	}
	for _, p := range peers {
		if !rt.sessionPeerOnline(p.InstanceID) {
			continue
		}
		u := proto.AgentStatusUpdatePayload{}
		sent := map[string]string{}
		for _, g := range groups {
			if g.IsArchived() || !fedPeerAllows(p.InstanceID, g.ID, PermAgentsStatusRead) {
				continue
			}
			key := p.InstanceID + "/" + g.Name
			active[key] = true
			if _, ok := rows[g.ID]; !ok {
				rows[g.ID] = fedGroupAgentStatuses(g.ID, shared)
			}
			raw, _ := json.Marshal(rows[g.ID])
			sum := sha256.Sum256(raw)
			hash := hex.EncodeToString(sum[:])
			old := rt.agentStatusSent[key]
			if old.hash == hash || time.Since(old.at) < debounce {
				continue
			}
			u.Groups = append(u.Groups, proto.AgentStatusGroupUpdate{Name: g.Name, Statuses: rows[g.ID], At: shared.observedAt, PublishedAt: time.Now().UTC()})
			sent[key] = hash
		}
		if len(u.Groups) > 0 && rt.sendControl(p.InstanceID, proto.KindAgentStatusUpdate, "", u) {
			for key, hash := range sent {
				rt.agentStatusSent[key] = fedStatusSent{hash: hash, at: time.Now()}
			}
		}
	}
	for key := range rt.agentStatusSent {
		if !active[key] {
			delete(rt.agentStatusSent, key)
		}
	}
}

func mergeAgentStatusGroup(g *proto.CatalogGroup, old *proto.CatalogGroup, created time.Time) {
	if !g.HasCap(proto.CapAgentStatus) {
		g.AgentStatuses = nil
		g.AgentStatusesAt = time.Time{}
		g.AgentStatusesUpdatedAt = time.Time{}
		g.AgentStatusesReceivedAt = time.Time{}
		return
	}
	pub := g.AgentStatusesUpdatedAt
	if pub.IsZero() {
		pub = created
	}
	invalid := pub.After(created.Add(time.Second)) || created.After(time.Now().Add(2*time.Minute)) || g.AgentStatusesAt.After(created.Add(2*time.Minute))
	if old != nil && old.HasCap(proto.CapAgentStatus) && (invalid || pub.Before(old.AgentStatusesUpdatedAt) || g.AgentStatusesAt.Before(old.AgentStatusesAt)) {
		g.AgentStatuses, g.AgentStatusesAt, g.AgentStatusesUpdatedAt, g.AgentStatusesReceivedAt = old.AgentStatuses, old.AgentStatusesAt, old.AgentStatusesUpdatedAt, old.AgentStatusesReceivedAt
		return
	}
	if invalid {
		g.AgentStatuses = nil
		g.AgentStatusesAt = time.Time{}
		pub = time.Time{}
	}
	g.AgentStatuses = proto.SanitizeAgentStatuses(g.AgentStatuses)
	g.AgentStatusesUpdatedAt = pub
	g.AgentStatusesReceivedAt = time.Now().UTC()
}
func mergeCatalogAgentStatuses(cat, previous *proto.CatalogPayload, created time.Time) {
	for i := range cat.Groups {
		g := &cat.Groups[i]
		var old *proto.CatalogGroup
		if previous != nil {
			for j := range previous.Groups {
				if previous.Groups[j].Name == g.Name {
					old = &previous.Groups[j]
					break
				}
			}
		}
		mergeAgentStatusGroup(g, old, created)
	}
}
func (rt *fedRuntime) acceptAgentStatusUpdate(from string, e *proto.Envelope) {
	var u proto.AgentStatusUpdatePayload
	if e.DecodePayload(&u) != nil {
		return
	}
	cat, catalogAt, err := fedCatalogFor(from)
	if err != nil || cat == nil {
		return
	}
	for _, next := range u.Groups {
		for i := range cat.Groups {
			g := &cat.Groups[i]
			if g.Name != next.Name || !g.HasCap(proto.CapAgentStatus) {
				continue
			}
			old := *g
			g.AgentStatuses = next.Statuses
			g.AgentStatusesAt = next.At
			g.AgentStatusesUpdatedAt = next.PublishedAt
			mergeAgentStatusGroup(g, &old, e.CreatedAt)
		}
	}
	raw, err := json.Marshal(cat)
	if err == nil {
		_ = db.PutFederationCatalog(from, string(raw), catalogAt)
	}
}
func remoteAgentStatusStale(rt *fedRuntime, peer string, g proto.CatalogGroup) bool {
	return rt == nil || !rt.sessionPeerOnline(peer) || g.AgentStatusesAt.IsZero() || g.AgentStatusesReceivedAt.IsZero() || time.Since(g.AgentStatusesAt) > fedStaleAfter || time.Since(g.AgentStatusesReceivedAt) > fedStaleAfter || g.AgentStatusesAt.After(time.Now().Add(2*time.Minute))
}

func coarseStatusExitReason(reason string) string {
	switch reason {
	case "":
		return ""
	case "unexpected", "resource_limit_oom":
		return "crashed"
	case "soft_exit", "daemon_kill":
		return "stopped"
	case "logout", "prompt_input_exit", "bypass_permissions_disabled":
		return "clean"
	default:
		return "unknown"
	}
}
