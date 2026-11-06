package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// An agent's own permission overrides under peer grants: permissions.grant
// and permissions.revoke, each held in every active group of the agent. What
// a change confers is held to the peer ceiling (peer_ceiling.go); what only
// takes authority away needs permissions.revoke alone.
//
// Per row, against what is stored (as handleDashboardPermissionsAPI writes
// it, a grant given no scope keeping the stored one):
//   - unchanged: nothing;
//   - grant: permissions.grant, and the ceiling for the scope it ends with,
//     unless that only narrows a grant already stored;
//   - deny, or default over a stored grant: permissions.revoke;
//   - default over a stored deny: lifting the deny restores whatever the
//     agent inherits, so permissions.grant and the ceiling for the slug
//     unscoped.
//
// The Steward's grants, and its carry for future Stewards, stay this node's
// operator's.

// peerAgentPermissionsBodyMax bounds a peer's permissions batch.
const peerAgentPermissionsBodyMax = 256 << 10

func servePeerAgentPermissions(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
	if v.unrestricted() {
		peerDashboardMux().ServeHTTP(w, peerAsLocalRequest(r))
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, peerAgentPermissionsBodyMax))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "read body: "+err.Error())
		return
	}
	var body struct {
		Conv      string                     `json:"conv"`
		Overrides map[string]string          `json:"overrides"`
		Scopes    map[string]json.RawMessage `json:"scopes"`
		Carry     map[string]bool            `json:"carry"`
	}
	if fail := peerStructureBody(raw, &body, "conv", "overrides", "scopes", "carry"); fail != nil {
		writeError(w, fail.status, fail.code, fail.msg)
		return
	}
	if len(body.Carry) > 0 {
		writeError(w, http.StatusForbidden, "permission", "keeping grants for future Stewards stays this node's operator's")
		return
	}
	conv, fail := peerAgentConv(v, body.Conv)
	if fail != nil {
		writeError(w, fail.status, fail.code, fail.msg)
		return
	}
	v.targetConv = conv
	if isLiveSteward(conv) || db.IsExternalStewardConv(conv) {
		writeError(w, http.StatusForbidden, "permission", "a Steward's permissions stay this node's operator's")
		return
	}
	if status, msg := peerAgentPermissionsFailure(v.peer.InstanceID, conv, body.Overrides, body.Scopes); status != 0 {
		writeError(w, status, "permission", msg)
		return
	}
	// The handler resolves the selector again: name the authorized agent's
	// conversation exactly.
	body.Conv, body.Carry = conv, nil
	next, err := json.Marshal(body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode", err.Error())
		return
	}
	a := &peerGrantAuthority{peer: v.peer.InstanceID, author: peerFollowUpAuthor(v, v.peer.InstanceID)}
	req := peerAsLocalRequest(r)
	req = req.WithContext(context.WithValue(req.Context(), peerGrantKey{}, a))
	req.Body = io.NopCloser(bytes.NewReader(next))
	req.ContentLength = int64(len(next))
	handleDashboardPermissionsAPI(w, req)
}

// peerAgentPermissionsFailure checks a peer's batch of conv's overrides, as
// described above: 0 when every row is covered, else the status and why.
// Rows the handler would refuse anyway (an unknown slug, a bad effect or
// scope) are left to it, as is its own refusal of unreadable stored scopes.
func peerAgentPermissionsFailure(peer, conv string, overrides map[string]string, scopes map[string]json.RawMessage) (int, string) {
	current, err := agentPermissionOverrides(conv)
	if err != nil {
		return http.StatusServiceUnavailable, "read the agent's permissions"
	}
	need := map[string]bool{}
	for slug, effect := range overrides {
		stored, held := current[slug]
		switch effect {
		case "default":
			if !held {
				continue
			}
			if stored.Effect == db.PermEffectDeny {
				need[PermPermissionsGrant] = true
				if err := peerCeilingCovers(peer, slug, nil); err != nil {
					return http.StatusForbidden, "lifting the deny of " + slug + " restores what the agent inherits: " + err.Error()
				}
				continue
			}
			need[PermPermissionsRevoke] = true
		case db.PermEffectDeny:
			if held && stored.Effect == db.PermEffectDeny {
				continue
			}
			need[PermPermissionsRevoke] = true
		case db.PermEffectGrant:
			desired := ""
			if held && stored.Effect == db.PermEffectGrant {
				desired = stored.Scope
			}
			if given, ok := scopes[slug]; ok {
				canonical, err := canonicalPermissionScopeForSlug(slug, string(given))
				if err != nil {
					continue // the handler refuses the batch
				}
				desired = canonical
			}
			if held && stored.Effect == db.PermEffectGrant {
				if desired == stored.Scope {
					continue
				}
				if peerRowNarrows(desired, stored.Scope) {
					need[PermPermissionsGrant] = true
					continue
				}
			}
			need[PermPermissionsGrant] = true
			if err := peerCeilingCoversGrant(peer, db.PermissionGrant{Slug: slug, Scope: desired}); err != nil {
				return http.StatusForbidden, err.Error()
			}
		}
	}
	for _, slug := range []string{PermPermissionsGrant, PermPermissionsRevoke} {
		if need[slug] && !peerAgentActionAllowed(peer, conv, slug) {
			return http.StatusForbidden, fmt.Sprintf("requires %s for every group of the agent", slug)
		}
	}
	return 0, ""
}

// peerReadsAgentPermissions reports whether a peer is shown an agent's own
// overrides: the grant that edits them, held in every group of the agent
// (agentHeld, from peerAgentGrants), so an editor never starts blank over
// rows the peer was not shown.
func peerReadsAgentPermissions(agentHeld []string) bool {
	for _, slug := range agentHeld {
		if slug == PermPermissionsGrant {
			return true
		}
	}
	return false
}

// projectPeerAgentPermissions fills the snapshot's permissions view with
// conv's overrides, their scopes and the ones this build cannot read.
func projectPeerAgentPermissions(perms *snapshotPermissionsView, conv string) error {
	rows, err := agentPermissionOverrides(conv)
	if err != nil {
		return err
	}
	effects := map[string]string{}
	for slug, row := range rows {
		effects[slug] = row.Effect
		if row.Scope == "" {
			continue
		}
		scope := permissionScopeFromJSON(row.Scope)
		if len(scope) == 0 {
			perms.UnreadableScopes[conv] = append(perms.UnreadableScopes[conv], slug)
			continue
		}
		if perms.Scopes == nil {
			perms.Scopes = map[string]map[string]PermissionScope{}
		}
		if perms.Scopes[conv] == nil {
			perms.Scopes[conv] = map[string]PermissionScope{}
		}
		perms.Scopes[conv][slug] = scope
	}
	perms.Overrides[conv] = effects
	return nil
}

// projectPeerAgentsPermissions projects the overrides of every agent in the
// view whose permissions the peer edits, with the scope picker's group
// values limited to the active groups the peer sees. It reports whether
// there was one.
func projectPeerAgentsPermissions(v *peerView, out *snapshotPayload, agents []dashboardAgent) (bool, error) {
	if v.agent != nil {
		return false, nil
	}
	held := v.agentGrants(nil)
	found := false
	for _, a := range agents {
		if !peerReadsAgentPermissions(held[a.AgentID]) {
			continue
		}
		if !found {
			found = true
			out.Permissions = snapshotPermissionsView{
				Defaults: []string{}, Grants: map[string][]string{},
				Overrides: map[string]map[string]string{}, UnreadableScopes: map[string][]string{},
			}
		}
		if err := projectPeerAgentPermissions(&out.Permissions, a.ConvID); err != nil {
			return false, err
		}
	}
	if !found {
		return false, nil
	}
	groups, err := db.ListAgentGroups()
	if err != nil {
		return false, err
	}
	var visible []*db.AgentGroup
	for _, g := range groups {
		if !g.IsArchived() && fedPeerGroupVisible(v.peer.InstanceID, g.ID) {
			visible = append(visible, g)
		}
	}
	out.Permissions.ScopeDimOptions = scopeDimOptionsSnapshot(visible, nil, nil, nil, nil, nil)
	return true, nil
}
