package agentd

import (
	"net/http"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

// permissionScopeValueItem is one value the permission editor's picker may
// offer for a dimension whose catalogue is not on the snapshot: for resource=,
// one drawing board or process template, with what the picker shows and groups by.
type permissionScopeValueItem struct {
	Kind   string   `json:"kind"`
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Groups []string `json:"groups,omitempty"`
	// Value is the matcher the item stands for when it is not kind:id: for
	// peer=, the stored <instance id>[/<group>].
	Value string `json:"value,omitempty"`
	// Node names a peer= item's linked node, for the picker to group by.
	Node string `json:"node,omitempty"`
}

// handleDashboardPermissionScopeValues serves GET
// /api/permissions/scope-values?dim=resource: the resource catalogue the
// editor's resource= picker searches. It is loaded once when the picker opens,
// not on the 2s snapshot poll, because the process-template listing is a
// filesystem walk (see scopeDimOptionsSnapshot). Every drawing board and every
// process template is offered, listed or not, since a grant may name either;
// a group that is archived grants nothing and is left off the groups.
//
// It names every resource and the groups each is linked to, so it is the
// operator's only: the dashboard's own authentication, never an agent's.
func handleDashboardPermissionScopeValues(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	var items []permissionScopeValueItem
	var err error
	switch dim := r.URL.Query().Get("dim"); dim {
	case string(ScopeDimResource):
		items, err = resourceScopeCatalogue(r)
	case string(ScopeDimPeer):
		// This node's peers are its own: a linked node's operator reaching
		// this route as local (unrestricted trust) does not list them, as it
		// does not reach the federation routes either.
		if requestFromPeerNode(r) {
			writeError(w, http.StatusForbidden, "scope_dim", "this node's linked nodes are listed to its own operator only")
			return
		}
		items, err = peerScopeCatalogue()
	default:
		writeError(w, http.StatusBadRequest, "scope_dim", "no catalogue is served for dimension "+dim+"; the snapshot carries the others")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "scope_values", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// resourceScopeCatalogue lists every resource a resource= matcher may name,
// drawing boards first, each kind sorted by id.
func resourceScopeCatalogue(r *http.Request) ([]permissionScopeValueItem, error) {
	items := []permissionScopeValueItem{}
	drawingBoards, err := db.ListCollabDrawingBoards()
	if err != nil {
		return nil, err
	}
	slices.SortFunc(drawingBoards, func(a, b *db.CollabDrawingBoard) int { return strings.Compare(a.ID, b.ID) })
	for _, b := range drawingBoards {
		item := permissionScopeValueItem{Kind: resourceScopeKindDrawingBoard, ID: b.ID, Title: b.Title}
		if b.GroupName != "" { // "" for no group or an archived one
			item.Groups = []string{b.GroupName}
		}
		items = append(items, item)
	}

	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		return nil, err
	}
	// One head per template: the version walk ListTemplates does is not
	// needed to name a template.
	heads, err := fs.ListTemplateHeads(r.Context())
	if err != nil {
		return nil, err
	}
	slices.SortFunc(heads, func(a, b store.TemplateHead) int { return strings.Compare(a.ID, b.ID) })
	pinned, err := db.ProcessTemplatePinnedGroups()
	if err != nil {
		return nil, err
	}
	groups, err := db.ListAgentGroups()
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	for _, g := range groups {
		if g != nil && !g.IsArchived() {
			live[g.Name] = true
		}
	}
	for _, head := range heads {
		id := head.ID
		item := permissionScopeValueItem{Kind: resourceScopeKindProcessTemplate, ID: id, Title: id}
		if tmpl, err := fs.GetTemplate(r.Context(), head.Ref); err == nil && tmpl.Name != "" {
			item.Title = tmpl.Name
		}
		for _, g := range pinned[id] {
			if live[g] {
				item.Groups = append(item.Groups, g)
			}
		}
		slices.Sort(item.Groups)
		items = append(items, item)
	}
	return items, nil
}

// requestFromPeerNode reports a request a linked node's operator makes
// through the peer view (its own grants or unrestricted trust as local),
// which marks the request remote-authed with a bare true.
func requestFromPeerNode(r *http.Request) bool {
	peer, _ := r.Context().Value(remoteAuthedCtxKey{}).(bool)
	return peer || peerGrantFromRequest(r) != nil || peerActionFromRequest(r) != nil
}

// peerScopeCatalogue lists what a peer= matcher may name: each trusted peer
// node-wide, then each group its latest catalogue shows this node. A group
// is in that catalogue only when the peer grants this node something there,
// so these are the groups a peer-scoped grant can actually reach (which slug
// the peer grants is still the peer's to decide). The value is the stored
// form, the instance id; the title is the peer's label, the name an operator
// knows it by.
func peerScopeCatalogue() ([]permissionScopeValueItem, error) {
	peers, err := db.ListFederationPeers()
	if err != nil {
		return nil, err
	}
	items := []permissionScopeValueItem{}
	for i := range peers {
		p := &peers[i]
		node := peerDisplay(p)
		items = append(items, permissionScopeValueItem{Kind: "peer", ID: p.InstanceID, Value: p.InstanceID,
			Title: node + " (whole node)", Node: node})
		cat, _, err := fedCatalogFor(p.InstanceID)
		if err != nil || cat == nil {
			continue // no catalogue yet: the node-wide value still applies
		}
		names := make([]string, 0, len(cat.Groups))
		for _, g := range cat.Groups {
			if g.Name != "" {
				names = append(names, g.Name)
			}
		}
		slices.Sort(names)
		for _, name := range slices.Compact(names) {
			value := p.InstanceID + "/" + name
			items = append(items, permissionScopeValueItem{Kind: "peer", ID: value, Value: value,
				Title: node + " / " + name, Node: node})
		}
	}
	return items, nil
}
