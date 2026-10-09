package agentd

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Build directly in the local dashboard's schema, starting empty. The shared
// gather is immutable private data, not an authorization result or a full local
// dashboard payload. Every request projects current peer authority onto it.
func (v *peerView) snapshot() (snapshotPayload, error) {
	groups, agents, err := v.groups()
	if err != nil {
		return snapshotPayload{}, err
	}
	out := snapshotPayload{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano), Version: buildversion.AppVersion(),
		AssetsVersion: dashboardAssetsVersion, Groups: groups, Agents: agents, AgentRosterAuthoritative: true,
		ActivityBots:  activityBotsView{Regular: "emoji", Slop: "sprites", Wizard: "emoji"},
		HScrollFollow: true, GroupQuickOptions: "hover", DefaultTerminal: "web", DefaultDirectoryPicker: "web",
	}
	// Normal dashboard clients expect arrays/maps, even for withheld concepts.
	// Initialize only empty collections; this introduces no values or names.
	filterPeerFields(reflect.ValueOf(&out).Elem(), allPeerProjectionFields)
	emptyPeerCollections(reflect.ValueOf(&out).Elem())
	// The restricted projection always sends its (small) registry fields. Never
	// accept a local static_version, or let cached blobs cross a peer switch or
	// a permission revocation. Auth session fields stay blank for peer requests.
	return out, nil
}

func emptyPeerCollections(v reflect.Value) {
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			emptyPeerCollections(v.Field(i))
		}
	case reflect.Slice:
		if v.CanSet() && v.IsNil() {
			v.Set(reflect.MakeSlice(v.Type(), 0, 0))
		}
		for i := 0; i < v.Len(); i++ {
			emptyPeerCollections(v.Index(i))
		}
	case reflect.Map:
		if v.CanSet() && v.IsNil() {
			v.Set(reflect.MakeMap(v.Type()))
		}
	}
}

func peerDashboardState(s proto.AgentStatus) agentState {
	out := agentState{Status: s.Status, SubagentCount: s.Subagents, BgShellCount: s.BackgroundShells, MonitorCount: s.Monitors, Model: s.Model, EffortLevel: s.Effort, Harness: s.Harness, RecoveryStatus: s.RecoveryStatus}
	if s.Context != nil {
		out.ContextPct = s.Context.Percent
		out.TokensInput = s.Context.InputTokens
		out.TokensOutput = s.Context.OutputTokens
		out.ContextWindowSize = s.Context.Window
	}
	// Translate the coarse federation exit reason back to the dashboard's
	// established labels; arbitrary local exit text is never forwarded.
	switch s.ExitReason {
	case "crashed":
		out.ExitReason = "unexpected"
	case "stopped":
		out.ExitReason = "daemon_kill"
	case "clean":
		out.ExitReason = "logout"
	}
	return out
}

func (v *peerView) groups() ([]dashboardGroup, []dashboardAgent, error) {
	groups, err := db.ListAgentGroups()
	if err != nil {
		return nil, nil, err
	}
	out := []dashboardGroup{}
	agents := map[string]dashboardAgent{}
	var shared *statusSnapshot
	rules := peerViewRules()
	for _, g := range groups {
		if g.IsArchived() || (v != nil && !fedPeerGroupVisible(v.peer.InstanceID, g.ID)) {
			continue
		}
		row := dashboardGroup{Name: g.Name, Descr: g.Descr, Members: []dashboardMember{}}
		status := v.allows(rules["GET /api/snapshot"], g.ID)
		roster := v.allows(rules["feature:roster"], g.ID)
		presence := v.allows(rules["feature:presence"], g.ID)
		mail := v.allows(rules["POST /api/operator-message"], g.ID)
		states := map[string]proto.AgentStatus{}
		if status || presence {
			if shared == nil {
				shared = gatheredStatusSnapshot()
			}
			for _, s := range fedGroupAgentStatuses(g.ID, shared) {
				states[s.Agent] = s
			}
		}
		if status || roster || presence || mail {
			members, err := db.ListAgentGroupMembers(g.ID)
			if err != nil {
				return nil, nil, err
			}
			for _, m := range members {
				aid, err := db.AgentIDForConv(m.ConvID)
				if err != nil {
					return nil, nil, err
				}
				a, err := db.GetAgent(aid)
				if err != nil {
					return nil, nil, err
				}
				if a == nil || !a.Active() || a.CurrentConvID != m.ConvID {
					continue
				}
				ar := dashboardMember{AgentID: aid, ConvID: m.ConvID, Title: agent.TitleFor(m.ConvID)}
				if roster {
					ar.Role = m.Role
				}
				if s, ok := states[aid]; ok {
					if status {
						ar.State = peerDashboardState(s)
						ar.taskRefView = taskRefView{TaskURL: s.TaskURL, TaskLabel: s.TaskLabel}
					}
					ar.Online = s.Online
				}
				if ar.Online {
					row.Online++
				}
				filterPeerFields(reflect.ValueOf(&ar).Elem(), map[string]bool{"identity": true, "roster": roster, "presence": presence || status, "status": status})
				row.Members = append(row.Members, ar)
				prior, exists := agents[aid]
				if !exists {
					prior = dashboardAgent{AgentID: aid, ConvID: m.ConvID, Title: ar.Title, Groups: []string{}, OwnedGroups: []string{}, Effective: []string{}}
				}
				if status {
					prior.State = ar.State
					prior.taskRefView = ar.taskRefView
				}
				if status || presence {
					prior.Online = ar.Online
				}
				prior.Groups = append(prior.Groups, g.Name)
				agents[aid] = prior
			}
		}
		sort.Slice(row.Members, func(i, j int) bool { return row.Members[i].AgentID < row.Members[j].AgentID })
		filterPeerFields(reflect.ValueOf(&row).Elem(), allPeerProjectionFields)
		emptyPeerCollections(reflect.ValueOf(&row).Elem())
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	rows := []dashboardAgent{}
	for _, a := range agents {
		sort.Strings(a.Groups)
		rows = append(rows, a)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].AgentID < rows[j].AgentID })
	return out, rows, nil
}

// Only counts of shared, identifiable agents enter a map card. Online and
// attention are computed from individually authorized presence/status fields.
// Resources are already cached node metadata, never a new probe on this path.
func servePeerSummary(w http.ResponseWriter, _ *http.Request, v *peerView, _ peerViewRule) {
	groups, agents, err := v.groups()
	if err != nil {
		writeError(w, 503, "snapshot_unavailable", "shared status unavailable")
		return
	}
	online, waiting := 0, 0
	for _, a := range agents {
		if a.Online {
			online++
		}
		if a.Online && (a.State.Status == session.StatusAwaitingInput || a.State.Status == session.StatusAwaitingPermission) {
			waiting++
		}
	}
	if v == nil || db.FederationPeerUnrestricted(v.peer.InstanceID) {
		// Unrestricted visibility includes loose agents, which are deliberately
		// absent from a restricted peer's group-scoped projection.
		active, err := db.ListActiveAgents()
		if err != nil {
			writeError(w, 503, "snapshot_unavailable", "shared status unavailable")
			return
		}
		seen := map[string]bool{}
		for _, a := range agents {
			seen[a.AgentID] = true
		}
		var shared *statusSnapshot
		for _, a := range active {
			if seen[a.AgentID] {
				continue
			}
			if shared == nil {
				shared = gatheredStatusSnapshot()
			}
			agents = append(agents, dashboardAgent{AgentID: a.AgentID})
			st := shared.states[a.CurrentConvID]
			live := isConvOnlineInSessions(shared.sessions[a.CurrentConvID], shared.alive)
			if live {
				online++
			}
			if live && (st.Status == session.StatusAwaitingInput || st.Status == session.StatusAwaitingPermission) {
				waiting++
			}
		}
	}
	out := map[string]any{"presence": "online", "shared_groups": len(groups), "shared_agents": len(agents), "online_agents": online, "waiting_for_input": waiting}
	if v == nil || fedPeerReadsNode(v.peer.InstanceID) {
		if node := localNodeMetadata(); node != nil {
			resources := node.Resources
			// Host-wide agent totals are not the shared counts displayed by the map.
			resources.Agents = nil
			out["resources"] = resources
			out["health"] = resources.Status
		}
	}
	// Revalidate every request so trust and grants cannot remain usable in a
	// browser cache after revocation. The ETag is over the authorized response.
	w.Header().Set("Cache-Control", "private, no-cache")
	writeJSON(w, 200, out)
}

func peerSummaryETag(w *peerViewResponse, r *http.Request) {
	tag := fmt.Sprintf(`"%x"`, sha256.Sum256(w.body.Bytes()))
	w.Header().Set("ETag", tag)
	for _, candidate := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		candidate = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(candidate), "W/"))
		if candidate == tag || candidate == "*" {
			w.status = http.StatusNotModified
			w.body.Reset()
			w.Header().Del("Content-Length")
			return
		}
	}
}

// Local map cards use the same lightweight summary without a federation scope.
// The cookie/Origin gate remains the local human authorization boundary.
func handleDashboardNodeSummary(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	serveLocalNodeSummary(w, r)
}

func serveLocalNodeSummary(w http.ResponseWriter, r *http.Request) {
	out := &peerViewResponse{header: make(http.Header)}
	servePeerSummary(out, r, nil, peerViewRule{})
	if out.statusCode() == 200 {
		peerSummaryETag(out, r)
	}
	for k, vals := range out.Header() {
		w.Header()[k] = vals
	}
	w.WriteHeader(out.statusCode())
	if r.Method != http.MethodHead {
		_, _ = w.Write(out.body.Bytes())
	}
}
