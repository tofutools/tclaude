package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/federation/client"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

type fedSessionObservation struct {
	state string
	since time.Time
}

func fedWaitingReason(state string) string {
	switch state {
	case session.StatusAwaitingPermission:
		return "permission"
	case session.StatusAwaitingInput:
		return "question"
	case session.StatusIdle:
		return "prompt"
	}
	return ""
}

// An opaque token binds discovery to a launch without exporting local pane
// handles or lifecycle callback authority. Session IDs alone are reusable.
func fedSessionIncarnation(row *db.SessionRow) string {
	identity, err := db.GetSessionExitLaunchIdentity(row.ID)
	if err != nil {
		return ""
	}
	h := sha256.Sum256([]byte(row.ID + "\x00" + row.CreatedAt.UTC().Format(time.RFC3339Nano) + "\x00" + row.TmuxSession + "\x00" + identity.Generation))
	return hex.EncodeToString(h[:])
}

// fedCatalogSessions shares only active agents in this group with live panes.
// Neither paths, prompt text nor tmux pane handles leave the instance.
func fedCatalogSessions(gid int64) []proto.CatalogSession {
	out := []proto.CatalogSession{}
	members, err := db.ListAgentGroupMembers(gid)
	if err != nil {
		return out
	}
	alive, err := session.LiveTmuxSessions()
	if err != nil {
		return out
	}
	rt := currentFederation()
	for _, m := range members {
		aid, _ := db.AgentIDForConv(m.ConvID)
		a, _ := db.GetAgent(aid)
		if a == nil || !a.Active() {
			continue
		}
		rows, _ := db.FindSessionsByConvID(m.ConvID)
		for _, row := range rows {
			if _, ok := alive[row.TmuxSession]; !ok || row.Status == session.StatusExited {
				continue
			}
			s := proto.CatalogSession{Agent: aid, Session: row.ID, Incarnation: fedSessionIncarnation(row), Name: agent.TitleFor(m.ConvID), Harness: row.Harness, State: row.Status, WaitingReason: fedWaitingReason(row.Status)}
			if rt != nil {
				rt.sessionsMu.Lock()
				if rt.sessionObservations == nil {
					rt.sessionObservations = map[string]fedSessionObservation{}
				}
				// The session ID can be reused on resume: include creation time so an
				// earlier runtime cannot lend its waiting duration to a fresh pane.
				key := aid + "/" + row.ID + "/" + row.CreatedAt.String()
				obs, ok := rt.sessionObservations[key]
				if !ok || obs.state != row.Status {
					obs = fedSessionObservation{state: row.Status, since: time.Now().UTC()}
					rt.sessionObservations[key] = obs
				}
				if s.WaitingReason != "" {
					since := obs.since
					s.WaitingObservedSince = &since
				}
				rt.sessionsMu.Unlock()
			}
			out = append(out, s)
			break // FindSessionsByConvID returns newest first; one current pane per agent.
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })
	return out
}

// Poll local state because harness callbacks also write from separate processes.
// No session read or wire traffic is done without an online authorized peer;
// unchanged snapshots generate no envelopes. The regular catalog is the heartbeat.
func (rt *fedRuntime) sessionPeerOnline(peer string) bool {
	return rt.cl != nil && rt.cl.Status().State == client.StateConnected && rt.isOnline(peer)
}

func (rt *fedRuntime) pushSessionTransitions() {
	if rt.cl == nil || rt.cl.Status().State != client.StateConnected {
		rt.sessionsMu.Lock()
		rt.sessionSent = nil
		rt.sessionObservations = nil // No lower bound can span an unobserved outage.
		rt.sessionsMu.Unlock()
		return
	}
	peers, err := db.ListFederationPeers()
	if err != nil {
		return
	}
	groups, err := db.ListAgentGroups()
	if err != nil {
		return
	}
	snapshots := map[int64][]proto.CatalogSession{}
	activeSent := map[string]bool{}
	for _, peer := range peers {
		if !rt.sessionPeerOnline(peer.InstanceID) {
			continue
		}
		update := proto.SessionsUpdatePayload{}
		for _, g := range groups {
			if !fedPeerAllows(peer.InstanceID, g.ID, PermSessionsRead) {
				continue
			}
			ss, ok := snapshots[g.ID]
			if !ok {
				ss = fedCatalogSessions(g.ID)
				snapshots[g.ID] = ss
			}
			raw, _ := json.Marshal(ss)
			key := peer.InstanceID + "/" + g.Name
			activeSent[key] = true
			rt.sessionsMu.Lock()
			if rt.sessionSent == nil {
				rt.sessionSent = map[string]string{}
			}
			previous, known := rt.sessionSent[key]
			rt.sessionsMu.Unlock()
			if known && previous == string(raw) {
				continue
			}
			update.Groups = append(update.Groups, proto.SessionGroupUpdate{Name: g.Name, Sessions: ss, At: time.Now().UTC()})
		}
		if len(update.Groups) == 0 {
			continue
		}
		// Re-read grants at send time, so a concurrent revoke cannot leak the
		// observer's already-built snapshot under its former permission.
		filtered := update.Groups[:0]
		for _, g := range update.Groups {
			local, _ := db.GetAgentGroupByName(g.Name)
			if local != nil && fedPeerAllows(peer.InstanceID, local.ID, PermSessionsRead) {
				filtered = append(filtered, g)
			}
		}
		update.Groups = filtered
		if len(update.Groups) == 0 {
			continue
		}
		if !rt.sendControl(peer.InstanceID, proto.KindSessionsUpdate, "", update) {
			continue
		}
		rt.sessionsMu.Lock()
		for _, g := range update.Groups {
			raw, _ := json.Marshal(g.Sessions)
			rt.sessionSent[peer.InstanceID+"/"+g.Name] = string(raw)
		}
		rt.sessionsMu.Unlock()
	}
	rt.sessionsMu.Lock()
	for key := range rt.sessionSent {
		if !activeSent[key] {
			delete(rt.sessionSent, key)
		}
	}
	// Bound observation memory to currently shared live sessions. Full catalogs
	// can repopulate entries when grants are added again.
	live := map[string]bool{}
	for _, ss := range snapshots {
		for _, s := range ss {
			live[s.Agent+"/"+s.Session+"/"] = true
		}
	}
	for key := range rt.sessionObservations {
		keep := false
		for prefix := range live {
			if strings.HasPrefix(key, prefix) {
				keep = true
				break
			}
		}
		if !keep {
			delete(rt.sessionObservations, key)
		}
	}
	rt.sessionsMu.Unlock()
}

func (rt *fedRuntime) acceptSessionUpdate(peer string, env *proto.Envelope) {
	var update proto.SessionsUpdatePayload
	if env.DecodePayload(&update) != nil {
		return
	}
	cat, received, err := fedCatalogFor(peer)
	if err != nil || cat == nil {
		return
	} // A state update never establishes authority.
	changed := false
	for _, u := range update.Groups {
		name := proto.SafeName(u.Name, false)
		if u.At.IsZero() || u.At.After(env.CreatedAt.Add(time.Minute)) {
			continue
		}
		for i := range cat.Groups {
			g := &cat.Groups[i]
			if g.Name != name || !g.HasCap(proto.CapSessions) || !u.At.After(g.SessionsAt) {
				continue
			}
			g.Sessions = proto.SanitizeSessions(u.Sessions)
			g.SessionsAt = u.At
			changed = true
		}
	}
	if changed {
		raw, err := json.Marshal(cat)
		if err == nil {
			_ = db.PutFederationCatalog(peer, string(raw), received)
		}
	}
}

type fedRemoteSession struct {
	Watch  bool `json:"watch"`
	Attach bool `json:"attach"`
	proto.CatalogSession
	Address    string    `json:"address"`
	Peer       string    `json:"peer"`
	Instance   string    `json:"instance"`
	Groups     []string  `json:"groups"`
	ObservedAt time.Time `json:"observed_at"`
	Stale      bool      `json:"stale"`
}

func handleFederationSessions(w http.ResponseWriter, r *http.Request) {
	caller, human, ok := authedCaller(w, r)
	if !ok {
		return
	}
	peers, err := db.ListFederationPeers()
	if err != nil {
		writeFedErr(w, err)
		return
	}
	if selector := strings.TrimSpace(r.URL.Query().Get("peer")); selector != "" {
		p, err := resolveFederationPeer(selector)
		if err != nil {
			writeFedErr(w, err)
			return
		}
		peers = []db.FederationPeer{*p}
	}
	rt := currentFederation()
	out := []fedRemoteSession{}
	for _, p := range peers {
		cat, _, err := fedCatalogFor(p.InstanceID)
		if err != nil || cat == nil {
			continue
		}
		indices := map[string]int{}
		for _, g := range cat.Groups {
			if !g.HasCap(proto.CapSessions) {
				continue
			}
			if !human {
				allowed, _, err := permissionAllowsAction(r, caller, PermSessionsRead, ActionContext{RemotePeer: p.InstanceID, RemoteGroup: g.Name})
				if err != nil || !allowed {
					continue
				}
			}
			for _, s := range g.Sessions {
				if idx, exists := indices[s.Agent]; exists {
					out[idx].Groups = append(out[idx].Groups, g.Name)
					out[idx].Watch = out[idx].Watch || g.HasCap(proto.CapSessionsWatch)
					out[idx].Attach = out[idx].Attach || g.HasCap(proto.CapSessionsAttach)
					if g.SessionsAt.After(out[idx].ObservedAt) {
						out[idx].CatalogSession = s
						out[idx].ObservedAt = g.SessionsAt
						out[idx].Stale = rt == nil || !rt.sessionPeerOnline(p.InstanceID) || time.Since(g.SessionsAt) > fedStaleAfter
					}
					continue
				}
				indices[s.Agent] = len(out)
				out = append(out, fedRemoteSession{Watch: g.HasCap(proto.CapSessionsWatch), Attach: g.HasCap(proto.CapSessionsAttach), CatalogSession: s, Address: s.Agent + "@" + fedFirst(p.Label, p.InstanceID), Peer: peerDisplay(&p), Instance: p.InstanceID, Groups: []string{g.Name}, ObservedAt: g.SessionsAt, Stale: rt == nil || !rt.sessionPeerOnline(p.InstanceID) || time.Since(g.SessionsAt) > fedStaleAfter})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	writeJSON(w, http.StatusOK, out)
}
