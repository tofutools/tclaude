package agentd

import (
	"net/http"
	"slices"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// GET /v1/groups/{name} over a peer view: groups show for a group on this
// node, read from a linked node (groups show --node, and the dashboard's
// /api/peer/{node}/v1/groups/{name}). This node evaluates it under its
// federation read model and widens nothing:
//
//   - Unrestricted trust reads it as this node's operator, as its snapshot.
//   - Otherwise the summary is the group's row as the peer's view of it
//     already is (GET /api/groups/{name}): visible groups only, members as
//     its roster, presence and status grants show them, and no owners or
//     links, which that view does not carry.
//   - The context and settings sections come from that same row, so each
//     field shows only where the view already shows it, and only to the
//     peer's operator whose peer holds groups.settings in the group
//     (groups.context.read and groups.settings.read are its peer aliases).
//     An agent principal gets the summary, as it gets no settings today.

func servePeerGroupShow(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if v.unrestricted() {
		servePeerAsLocal(w, r, v, peerViewRule{unrestrictedOnly: true})
		return
	}
	groups, _, err := v.groups()
	if err != nil {
		writeError(w, 503, "snapshot_unavailable", "shared status unavailable")
		return
	}
	i := slices.IndexFunc(groups, func(g dashboardGroup) bool { return g.Name == r.PathValue("name") })
	if i < 0 {
		writeError(w, 404, "not_found", "object not found")
		return
	}
	writeJSON(w, http.StatusOK, peerGroupShowView(v, groups[i]))
}

// peerGroupShowView is groups show's reply built from a peer's projected
// row of the group.
func peerGroupShowView(v *peerView, row dashboardGroup) groupShowView {
	out := groupShowView{
		Name: row.Name, Descr: row.Descr, Parent: row.Parent,
		Attachment: groupAttachmentView{URL: row.AttachmentURL, Label: row.AttachmentLabel, LabelOverride: row.AttachmentLabelOverride},
		Owners:     []ownerJSON{}, Members: []memberJSON{}, Links: []linkJSON{},
	}
	for _, m := range row.Members {
		out.Members = append(out.Members, memberJSON{AgentID: m.AgentID, ConvID: m.ConvID, Title: m.Title, Role: m.Role, Online: m.Online, Owner: m.Owner})
	}
	var gates map[string]bool
	if v.agent == nil {
		gates = peerGrantGates(v.peer.InstanceID, row.ID)
	}
	if gates[PermGroupsContextRead] {
		out.Context = &groupShowContext{
			Text: row.DefaultContext, Hash: db.GroupContextHash(row.DefaultContext),
			DefaultPrompts: slices.Clone(row.DefaultPrompts), ReinjectAfterCompact: row.ReinjectAfterCompact,
		}
	} else {
		out.Withheld = append(out.Withheld, groupShowWithheld{Section: "context", Slug: PermGroupsSettings})
	}
	if gates[PermGroupsSettingsRead] {
		settings := &groupShowSettings{
			DefaultCwd: row.DefaultCwd, DefaultProfile: row.DefaultProfile, SandboxProfile: row.SandboxProfile,
			MaxMembers: row.MaxMembers, NotifyEnabled: row.NotifyEnabled, RemoteControlPolicy: row.RemoteControlPolicy,
			OwnerScopes: row.OwnerScopes, Permissions: row.Permissions, PermissionScopes: row.PermissionScopes,
			UnreadablePermissionScopes: row.UnreadablePermissionScopes,
		}
		for _, entry := range row.Environment {
			settings.EnvironmentKeys = append(settings.EnvironmentKeys, entry.Name)
		}
		if len(settings.OwnerScopes) == 0 {
			settings.OwnerScopes = nil
		}
		if len(settings.PermissionScopes) == 0 {
			settings.PermissionScopes = nil
		}
		out.Settings = settings
	} else {
		out.Withheld = append(out.Withheld, groupShowWithheld{Section: "settings", Slug: PermGroupsSettings})
	}
	return out
}
