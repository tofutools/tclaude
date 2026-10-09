package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

const (
	PermAgentsStatusRead   = "agents.status.read"
	PermSessionsRead       = "sessions.read"
	PermSessionsWatch      = "sessions.watch"
	PermSessionsAttach     = "sessions.attach"
	PermGroupsRosterRead   = "groups.roster.read"
	PermGroupsPresenceRead = "groups.presence.read"
	PermMessageAttachments = "message.attachments"
	PermAgentsReceive      = "agents.receive"
	PermJobsRun            = "jobs.run"
)

// Wire capabilities stay stable while authority lives in regular peer slugs.
var federationPeerSlugs = map[string]string{
	PermAgentsStatusRead:      proto.CapAgentStatus,
	PermGroupsRosterRead:      proto.CapRoster,
	PermSessionsRead:          proto.CapSessions,
	PermSessionsWatch:         proto.CapSessionsWatch,
	PermSessionsAttach:        proto.CapSessionsAttach,
	PermGroupsPresenceRead:    proto.CapPresence,
	PermMessageDirect:         proto.CapMail,
	PermMessageAttachments:    proto.CapAttachments,
	PermAgentsReceive:         proto.CapAgentsReceive,
	PermAgentsTeleportReceive: proto.CapTeleportReceive,
	PermGroupsMembersSpawn:    proto.CapSpawn,
	PermRoutesConsume:         proto.CapRoutes,
	PermJobsRun:               proto.CapJobs,
}

func fedPeerGroupGrant(peer string, groupID int64, slug string) *db.FederationPeerGrant {
	g, err := db.GetAgentGroupByID(groupID)
	if err != nil || g == nil || g.IsArchived() {
		return nil
	}
	if db.FederationPeerUnrestricted(peer) {
		if _, known := federationPeerSlugs[slug]; slug != "" && !known {
			return nil
		}
		cap := 8
		cfg, _ := config.Load()
		if cfg != nil && cfg.Federation != nil && cfg.Federation.UnrestrictedMaxLive > 0 {
			cap = cfg.Federation.UnrestrictedMaxLive
		}
		return &db.FederationPeerGrant{Peer: peer, Slug: slug, SpawnPolicy: db.FederationSpawnPolicy{MaxLive: cap}}
	}
	return fedPeerExplicitGroupGrant(peer, groupID, slug)
}

func fedPeerExplicitGroupGrant(peer string, groupID int64, slug string) *db.FederationPeerGrant {
	grants, err := db.ListEffectiveFederationPeerGrants(peer)
	if err != nil {
		return nil
	}
	var candidates []db.FederationPeerGrant
	for _, grant := range grants {
		if _, known := federationPeerSlugs[grant.Slug]; !known {
			continue
		}
		if slug != "" && grant.Slug != slug {
			continue
		}
		if grant.Scope == "" || grant.Scope == db.FederationGroupScope(groupID) {
			candidates = append(candidates, grant)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	// Most specific scope wins. For launch policy only, direct policy wins
	// among equally specific matches; conflicting inherited policies refuse.
	rank := func(g db.FederationPeerGrant) int {
		n := 0
		if g.Scope != "" {
			n = 2
		}
		if g.PoolID == "" {
			n++
		}
		return n
	}
	best := candidates[0]
	for _, g := range candidates[1:] {
		if rank(g) > rank(best) {
			best = g
		}
	}
	if slug == PermGroupsMembersSpawn || slug == PermJobsRun {
		for _, g := range candidates {
			if rank(g) == rank(best) && !g.SpawnPolicy.Equal(best.SpawnPolicy) {
				return nil
			}
		}
	}
	return &best
}

func fedPeerAllows(peer string, groupID int64, slug string) bool {
	return fedPeerGroupGrant(peer, groupID, slug) != nil
}
func fedPeerGroupVisible(peer string, groupID int64) bool { return fedPeerAllows(peer, groupID, "") }

type fedPeerGrantReq struct {
	Peer        string                   `json:"peer"`
	Slug        string                   `json:"slug"`
	Scope       string                   `json:"scope,omitempty"`
	SpawnPolicy db.FederationSpawnPolicy `json:"spawn_policy,omitempty"`
}

// Display group names while storing stable group ids so renames cannot retarget
// grants, and deleting a group cannot authorize a replacement of the same name.
func fedDisplayPeerGrant(g db.FederationPeerGrant) db.FederationPeerGrant {
	if strings.HasPrefix(g.Scope, "group=") {
		id, _ := strconv.ParseInt(strings.TrimPrefix(g.Scope, "group="), 10, 64)
		if group, _ := db.GetAgentGroupByID(id); group != nil {
			g.Scope = "group=" + group.Name
		}
	}
	return g
}

func handleFederationPeerGrants(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "manage federation peer grants") {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET, POST or DELETE")
		return
	}
	var in fedPeerGrantReq
	if r.Method == http.MethodGet {
		in.Peer = r.URL.Query().Get("peer")
	} else if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	var p *db.FederationPeer
	var poolID string
	var err error
	if strings.HasPrefix(in.Peer, "group:") {
		g, e := db.GetFederationNodeGroup(strings.TrimPrefix(in.Peer, "group:"))
		if e != nil {
			writeError(w, 404, "node_group", e.Error())
			return
		}
		poolID = g.ID
		p = &db.FederationPeer{InstanceID: "group:" + g.Name}
		if r.Method != http.MethodGet {
			finish, e := lockNodeGroupAuthorityMutation(poolID, "")
			if e != nil {
				writeError(w, 500, "node_group", e.Error())
				return
			}
			defer finish()
		}
	} else {
		p, err = resolveFederationPeer(in.Peer)
	}
	if err != nil {
		writeFedErr(w, err)
		return
	}
	if r.Method == http.MethodGet {
		grants, err := db.ListEffectiveFederationPeerGrants(p.InstanceID)
		if poolID != "" {
			grants, err = db.ListFederationNodeGroupGrants(poolID)
		}
		if err != nil {
			writeFedErr(w, err)
			return
		}
		for i := range grants {
			grants[i] = fedDisplayPeerGrant(grants[i])
		}
		writeJSON(w, http.StatusOK, map[string]any{"grants": grants})
		return
	}
	_, groupSlug := federationPeerSlugs[in.Slug]
	instanceSlug := in.Slug == "config.offer" || in.Slug == PermApprovalsAnswer || in.Slug == PermNodeRead || in.Slug == PermCostsRead || in.Slug == PermFederationAuditRead
	if !groupSlug && !instanceSlug && (in.Slug != PermModelsProxy && in.Slug != PermModelsProxyLeased) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "slug is not supported for peers: "+in.Slug)
		return
	}
	scope := strings.TrimSpace(in.Scope)
	if (in.Slug == PermAgentsReceive || in.Slug == PermAgentsTeleportReceive) && scope == "" {
		writeError(w, 400, "invalid_arg", in.Slug+" requires scope group=<local group>")
		return
	}
	if instanceSlug && scope != "" {
		writeError(w, 400, "invalid_arg", in.Slug+" is an unscoped instance grant")
		return
	}
	var gid int64
	if scope != "" && (in.Slug == PermModelsProxy || in.Slug == PermModelsProxyLeased) {
		if !validModelProxyScope(scope) {
			writeError(w, 400, "invalid_arg", "models.proxy scope must be http_proxy=<name>")
			return
		}
	} else if scope != "" {
		if !strings.HasPrefix(scope, "group=") || strings.Contains(scope, ",") {
			writeError(w, http.StatusBadRequest, "invalid_arg", "peer scope must be group=<local group>")
			return
		}
		g, err := db.GetAgentGroupByName(strings.TrimPrefix(scope, "group="))
		if err != nil || g == nil || (r.Method != http.MethodDelete && g.IsArchived()) {
			writeError(w, http.StatusBadRequest, "invalid_arg", "no active local group by that name")
			return
		}
		gid = g.ID
		scope = db.FederationGroupScope(g.ID)
	}
	warnings := []string{}
	if scope == "" && !instanceSlug && (in.Slug != PermModelsProxy && in.Slug != PermModelsProxyLeased) {
		warnings = append(warnings, "WARNING: unscoped peer grant covers every active group, including future groups")
	}
	if r.Method == http.MethodDelete {
		if in.Slug == PermApprovalsAnswer && poolID == "" {
			finish := lockAwayAuthorityMutation(p.InstanceID)
			defer finish()
		}
		var ok bool
		if poolID != "" {
			ok, err = db.DeleteFederationNodeGroupGrant(poolID, in.Slug, scope)
		} else {
			ok, err = db.DeleteFederationPeerGrant(p.InstanceID, in.Slug, scope)
		}
		if err != nil {
			writeFedErr(w, err)
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "no such peer grant")
			return
		}
	} else {
		if err := normalizeSelectableProfiles(in.Slug, &in.SpawnPolicy); err != nil {
			writeError(w, 400, "invalid_arg", err.Error())
			return
		}
		if !validRequesterPaysPolicy(in.SpawnPolicy.RequesterPays) || in.SpawnPolicy.RequesterPays != "" && in.Slug != PermGroupsMembersSpawn {
			writeError(w, 400, "invalid_arg", "requester_pays requires groups.members.spawn and must be required, allowed or off")
			return
		}
		if in.SpawnPolicy.JobApproval != "" && (in.Slug != PermJobsRun || (in.SpawnPolicy.JobApproval != "manual" && in.SpawnPolicy.JobApproval != "auto")) {
			writeError(w, 400, "invalid_arg", "job_approval must be auto or manual and requires jobs.run")
			return
		}
		if in.Slug == PermGroupsMembersSpawn || in.Slug == PermJobsRun {
			if in.SpawnPolicy.MaxLive == 0 {
				in.SpawnPolicy.MaxLive = 2
			}
			if in.SpawnPolicy.MaxLive < 1 {
				writeError(w, http.StatusBadRequest, "invalid_arg", "max-live must be positive")
				return
			}
		} else if !in.SpawnPolicy.Equal(db.FederationSpawnPolicy{}) {
			writeError(w, http.StatusBadRequest, "invalid_arg", "launch settings apply only to groups.members.spawn or jobs.run")
			return
		}
		if in.Slug == PermMessageAttachments && !fedGrantMailCoversScope(p.InstanceID, poolID, gid) {
			warnings = append(warnings, "attachments require message.direct on the same group")
		}
		grant := db.FederationPeerGrant{Peer: p.InstanceID, Slug: in.Slug, Scope: scope, SpawnPolicy: in.SpawnPolicy}
		if poolID != "" {
			err = db.UpsertFederationNodeGroupGrant(poolID, grant)
		} else {
			err = db.UpsertFederationPeerGrant(grant)
		}
		if err != nil {
			writeFedErr(w, err)
			return
		}
	}
	setAuditTargetLabel(r, fmt.Sprintf("%s %s %s", p.InstanceID, in.Slug, scope))
	broadcastFederationCatalogs()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warnings": warnings})
}

func fedPeerMailCoversScope(peer string, groupID int64) bool {
	if groupID != 0 {
		return fedPeerAllows(peer, groupID, PermMessageDirect)
	}
	if db.FederationPeerUnrestricted(peer) {
		return true
	}
	grants, err := db.ListEffectiveFederationPeerGrants(peer)
	if err != nil {
		return false
	}
	for _, grant := range grants {
		if grant.Slug == PermMessageDirect && grant.Scope == "" {
			return true
		}
	}
	return false
}

func fedGrantMailCoversScope(peer, poolID string, groupID int64) bool {
	if poolID == "" {
		return fedPeerMailCoversScope(peer, groupID)
	}
	grants, err := db.ListFederationNodeGroupGrants(poolID)
	if err != nil {
		return false
	}
	for _, g := range grants {
		if g.Slug == PermMessageDirect && (g.Scope == "" || (groupID != 0 && g.Scope == db.FederationGroupScope(groupID))) {
			return true
		}
	}
	return false
}
