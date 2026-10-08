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
	PermSessionsRead       = "sessions.read"
	PermSessionsWatch      = "sessions.watch"
	PermSessionsAttach     = "sessions.attach"
	PermGroupsRosterRead   = "groups.roster.read"
	PermGroupsPresenceRead = "groups.presence.read"
	PermMessageAttachments = "message.attachments"
	PermAgentsReceive      = "agents.receive"
)

// Wire capabilities stay stable while authority lives in regular peer slugs.
var federationPeerSlugs = map[string]string{
	PermGroupsRosterRead:   proto.CapRoster,
	PermSessionsRead:       proto.CapSessions,
	PermSessionsWatch:      proto.CapSessionsWatch,
	PermSessionsAttach:     proto.CapSessionsAttach,
	PermGroupsPresenceRead: proto.CapPresence,
	PermMessageDirect:      proto.CapMail,
	PermMessageAttachments: proto.CapAttachments,
	PermAgentsReceive:      proto.CapAgentsReceive,
	PermGroupsMembersSpawn: proto.CapSpawn,
	PermRoutesConsume:      proto.CapRoutes,
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
	grants, err := db.ListFederationPeerGrants(peer)
	if err != nil {
		return nil
	}
	var fallback *db.FederationPeerGrant
	for i := range grants {
		grant := &grants[i]
		if _, known := federationPeerSlugs[grant.Slug]; !known {
			continue
		}
		if slug != "" && grant.Slug != slug {
			continue
		}
		if grant.Scope == db.FederationGroupScope(groupID) {
			return grant
		}
		if grant.Scope == "" {
			fallback = grant
		}
	}
	return fallback
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
	p, err := resolveFederationPeer(in.Peer)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	if r.Method == http.MethodGet {
		grants, err := db.ListFederationPeerGrants(p.InstanceID)
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
	instanceSlug := in.Slug == "config.offer" || in.Slug == PermApprovalsAnswer || in.Slug == PermNodeRead
	if !groupSlug && !instanceSlug {
		writeError(w, http.StatusBadRequest, "invalid_arg", "slug is not supported for peers: "+in.Slug)
		return
	}
	scope := strings.TrimSpace(in.Scope)
	if in.Slug == PermAgentsReceive && scope == "" {
		writeError(w, 400, "invalid_arg", "agents.receive requires scope group=<local group>")
		return
	}
	if instanceSlug && scope != "" {
		writeError(w, 400, "invalid_arg", in.Slug+" is an unscoped instance grant")
		return
	}
	var gid int64
	if scope != "" {
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
	if scope == "" && !instanceSlug {
		warnings = append(warnings, "WARNING: unscoped peer grant covers every active group, including future groups")
	}
	if r.Method == http.MethodDelete {
		if in.Slug == PermApprovalsAnswer {
			finish := lockAwayAuthorityMutation(p.InstanceID)
			defer finish()
		}
		ok, err := db.DeleteFederationPeerGrant(p.InstanceID, in.Slug, scope)
		if err != nil {
			writeFedErr(w, err)
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "no such peer grant")
			return
		}
	} else {
		if in.Slug == PermGroupsMembersSpawn {
			if in.SpawnPolicy.MaxLive == 0 {
				in.SpawnPolicy.MaxLive = 2
			}
			if in.SpawnPolicy.MaxLive < 1 {
				writeError(w, http.StatusBadRequest, "invalid_arg", "max-live must be positive")
				return
			}
		} else if in.SpawnPolicy != (db.FederationSpawnPolicy{}) {
			writeError(w, http.StatusBadRequest, "invalid_arg", "launch settings apply only to groups.members.spawn")
			return
		}
		if in.Slug == PermMessageAttachments && !fedPeerMailCoversScope(p.InstanceID, gid) {
			warnings = append(warnings, "attachments require message.direct on the same group")
		}
		if err := db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: p.InstanceID, Slug: in.Slug, Scope: scope, SpawnPolicy: in.SpawnPolicy}); err != nil {
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
	grants, err := db.ListFederationPeerGrants(peer)
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
