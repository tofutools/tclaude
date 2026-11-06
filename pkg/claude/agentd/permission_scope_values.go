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
// one board or process template, with what the picker shows and groups by.
type permissionScopeValueItem struct {
	Kind   string   `json:"kind"`
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Groups []string `json:"groups,omitempty"`
}

// handleDashboardPermissionScopeValues serves GET
// /api/permissions/scope-values?dim=resource: the resource catalogue the
// editor's resource= picker searches. It is loaded once when the picker opens,
// not on the 2s snapshot poll, because the process-template listing is a
// filesystem walk (see scopeDimOptionsSnapshot). Every board and every
// process template is offered, listed or not, since a grant may name either;
// a group that is archived grants nothing and is left off the groups.
//
// It names every resource and the groups each is linked to, so it is the
// operator's only: the dashboard's own authentication, never an agent's.
func handleDashboardPermissionScopeValues(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if dim := r.URL.Query().Get("dim"); dim != string(ScopeDimResource) {
		writeError(w, http.StatusBadRequest, "scope_dim", "no catalogue is served for dimension "+dim+"; the snapshot carries the others")
		return
	}
	items, err := resourceScopeCatalogue(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "scope_values", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// resourceScopeCatalogue lists every resource a resource= matcher may name,
// boards first, each kind sorted by id.
func resourceScopeCatalogue(r *http.Request) ([]permissionScopeValueItem, error) {
	items := []permissionScopeValueItem{}
	boards, err := db.ListCollabBoards()
	if err != nil {
		return nil, err
	}
	slices.SortFunc(boards, func(a, b *db.CollabBoard) int { return strings.Compare(a.ID, b.ID) })
	for _, b := range boards {
		item := permissionScopeValueItem{Kind: resourceScopeKindBoard, ID: b.ID, Title: b.Title}
		if b.GroupName != "" { // "" for no group or an archived one
			item.Groups = []string{b.GroupName}
		}
		items = append(items, item)
	}

	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		return nil, err
	}
	records, err := fs.ListTemplates(r.Context())
	if err != nil {
		return nil, err
	}
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
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.ID)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	for _, id := range ids {
		item := permissionScopeValueItem{Kind: resourceScopeKindProcessTemplate, ID: id, Title: id}
		if head, err := fs.GetTemplateHead(r.Context(), id); err == nil {
			if tmpl, err := fs.GetTemplate(r.Context(), head.Ref); err == nil && tmpl.Name != "" {
				item.Title = tmpl.Name
			}
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
