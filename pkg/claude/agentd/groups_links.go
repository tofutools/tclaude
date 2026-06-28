package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// handleLinksAll is GET /v1/links — every link in the system, with
// group names resolved. Read-only and open to anyone (matches
// /v1/groups). Useful for the dashboard's graph view and the human-
// facing `tclaude agent groups links` overview verb.
func handleLinksAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	links, err := db.ListAllAgentGroupLinks()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	names := loadGroupNames()
	out := make([]linkJSON, 0, len(links))
	for _, l := range links {
		out = append(out, toLinkJSON(l, names))
	}
	writeJSON(w, http.StatusOK, out)
}

// canMessageResp is the wire form of GET /v1/can-message?from=&to=.
type canMessageResp struct {
	Allowed  bool   `json:"allowed"`
	Reason   string `json:"reason,omitempty"`
	ViaGroup string `json:"via_group,omitempty"`
	LinkID   int64  `json:"link_id,omitempty"`
	Message  string `json:"message,omitempty"`
}

// handleCanMessage is GET /v1/can-message?to=<conv>[&from=<conv>] — a
// debug probe for the "why can I message X?" CLI verb. `from` defaults
// to the caller's conv-id; a human can pass an explicit `from`. Always
// open: the answer is derivable by trial-and-error against the send
// endpoint anyway, and exposing it cleanly helps agents self-diagnose
// routing issues without spamming `tclaude agent message`.
func handleCanMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	toSel := strings.TrimSpace(r.URL.Query().Get("to"))
	if toSel == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "to is required")
		return
	}
	fromSel := strings.TrimSpace(r.URL.Query().Get("from"))
	if fromSel == "" {
		p := peerFromContext(r.Context())
		fromSel = p.ConvID
	}
	if fromSel == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"from is required when caller has no conv-id (human path)")
		return
	}

	fromRes, _, err := agent.ResolveSelector(fromSel)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("from: %s", err.Error()))
		return
	}
	toRes, _, err := agent.ResolveSelector(toSel)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("to: %s", err.Error()))
		return
	}

	if fromRes.ConvID == toRes.ConvID {
		writeJSON(w, http.StatusOK, canMessageResp{
			Allowed: false, Message: "cannot message self",
		})
		return
	}

	via, reason, err := db.CanSenderReachTarget(fromRes.ConvID, toRes.ConvID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if via == nil {
		writeJSON(w, http.StatusOK, canMessageResp{
			Allowed: false,
			Message: "no shared group, no owner-of relation, and no inter-group link reaches this target",
		})
		return
	}
	resp := canMessageResp{
		Allowed:  true,
		ViaGroup: via.Name,
	}
	// Reason is "shared-group" | "owner-of-group" | "via-link:<id>".
	if strings.HasPrefix(reason, "via-link:") {
		resp.Reason = "via-link"
		if id, perr := strconv.ParseInt(strings.TrimPrefix(reason, "via-link:"), 10, 64); perr == nil {
			resp.LinkID = id
		}
	} else {
		resp.Reason = reason
	}
	writeJSON(w, http.StatusOK, resp)
}

// linkJSON is the wire form of an agent_group_links row. group names
// are denormalised onto the response so the CLI / dashboard don't
// need to do a second hop to translate ids → names.
type linkJSON struct {
	ID        int64  `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Mode      string `json:"mode"`
	CreatedAt string `json:"created_at"`
	// ByAgent is the stable agent_id of the creator (the actor); ByConv is
	// the conv-id snapshot kept for forensics. ByAgent is "" for a
	// human/un-enrolled creator (then only ByConv is meaningful).
	ByAgent string `json:"by_agent,omitempty"`
	ByConv  string `json:"by_conv,omitempty"`
}

// toLinkJSON renders a link row for the wire. `names` is a preloaded
// group-id→name map (see loadGroupNames) so a list of N links does
// 1 DB roundtrip total instead of 2N. Empty string is rendered when
// the referenced group is missing (deleted between fetch and resolve
// — shouldn't happen under FK CASCADE but the wire form stays valid
// either way).
func toLinkJSON(l *db.AgentGroupLink, names map[int64]string) linkJSON {
	return linkJSON{
		ID:        l.ID,
		From:      names[l.FromGroupID],
		To:        names[l.ToGroupID],
		Mode:      l.Mode,
		CreatedAt: l.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		ByAgent:   l.ByAgent,
		ByConv:    l.ByConv,
	}
}

// loadGroupNames returns id→name for every group. Used by the link
// list handlers to resolve from/to references without per-row lookups.
// Errors are swallowed so a transient DB hiccup just shows the names
// as empty strings rather than blocking the whole response.
func loadGroupNames() map[int64]string {
	groups, err := db.ListAgentGroups()
	if err != nil {
		return map[int64]string{}
	}
	out := make(map[int64]string, len(groups))
	for _, g := range groups {
		out[g.ID] = g.Name
	}
	return out
}

// handleGroupLinks dispatches GET / POST / PATCH / DELETE under
// /v1/groups/{name}/links[/{id}].
func handleGroupLinks(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, rest []string) {
	switch r.Method {
	case http.MethodGet:
		handleGroupLinksList(w, r, g)
	case http.MethodPost:
		handleGroupLinksAdd(w, r, g)
	case http.MethodPatch:
		if len(rest) < 1 || rest[0] == "" {
			writeError(w, http.StatusBadRequest, "invalid_arg", "missing link id")
			return
		}
		handleGroupLinksUpdate(w, r, g, rest[0])
	case http.MethodDelete:
		if len(rest) < 1 || rest[0] == "" {
			writeError(w, http.StatusBadRequest, "invalid_arg", "missing link id")
			return
		}
		handleGroupLinksRemove(w, r, g, rest[0])
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET, POST, PATCH, or DELETE")
	}
}

func handleGroupLinksList(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	dir := db.LinkDirection(strings.TrimSpace(r.URL.Query().Get("dir")))
	if dir == "" {
		dir = db.LinkBoth
	}
	switch dir {
	case db.LinkOut, db.LinkIn, db.LinkBoth:
	default:
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"dir must be one of: out, in, both")
		return
	}
	links, err := db.ListAgentGroupLinks(g.ID, dir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	names := loadGroupNames()
	out := make([]linkJSON, 0, len(links))
	for _, l := range links {
		out = append(out, toLinkJSON(l, names))
	}
	writeJSON(w, http.StatusOK, out)
}

// linkAddBody is the POST payload. `to` accepts a group name or id-as-
// number. mode defaults to members->members if unset. bidir creates
// the reverse edge in the same call (failures on the reverse don't
// roll back the forward edge — the caller can retry).
type linkAddBody struct {
	To    string `json:"to"`
	Mode  string `json:"mode,omitempty"`
	Bidir bool   `json:"bidir,omitempty"`
}

func handleGroupLinksAdd(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	caller, ok := requireGroupLinkAuthority(w, r, g, PermGroupsLinkAdd)
	if !ok {
		return
	}
	if !requireGroupActive(w, g) {
		return
	}
	var body linkAddBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	toGroup, err := resolveGroupSelector(body.To)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if !requireGroupActive(w, toGroup) {
		return
	}
	mode := strings.TrimSpace(body.Mode)
	if mode == "" {
		mode = db.LinkModeMembersToMembers
	}
	if !db.ValidLinkMode(mode) {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("unknown mode %q (supported: %s, %s)",
				mode, db.LinkModeMembersToMembers, db.LinkModeOwnersToMembers))
		return
	}

	id, err := db.InsertAgentGroupLink(g.ID, toGroup.ID, mode, auditedCaller(caller, PermGroupsLinkAdd))
	if err != nil {
		if errors.Is(err, db.ErrLinkExists) {
			writeError(w, http.StatusConflict, "exists", err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}

	out := map[string]any{"id": id, "from": g.Name, "to": toGroup.Name, "mode": mode}

	if body.Bidir {
		revID, err := db.InsertAgentGroupLink(toGroup.ID, g.ID, mode, auditedCaller(caller, PermGroupsLinkAdd))
		switch {
		case err == nil:
			out["reverse_id"] = revID
		case errors.Is(err, db.ErrLinkExists):
			out["reverse_id"] = "already-exists"
		default:
			out["reverse_error"] = err.Error()
		}
	}

	writeJSON(w, http.StatusOK, out)
}

// handleGroupLinksUpdate changes the mode of an existing link. From/to
// are immutable here — re-pointing an edge is logically delete + new,
// which the regular endpoints already cover. Auth reuses
// PermGroupsLinkAdd (editing terms is a recreate-shaped action; an
// owner of g passes without the slug ONLY when g is the link's FROM
// side — the owner-bypass is outbound-only). 409 on collision with
// another existing row.
func handleGroupLinksUpdate(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "link id must be integer")
		return
	}
	link, err := db.GetAgentGroupLinkByID(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if link == nil {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no link %d", id))
		return
	}
	// URL-scoping: the link must touch g (FROM or TO).
	if link.FromGroupID != g.ID && link.ToGroupID != g.ID {
		writeError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("link %d does not touch group %q", id, g.Name))
		return
	}
	// Auth: now that we know which side of the link g is on, apply
	// the owner-bypass-only-when-g-is-FROM rule.
	if _, ok := requireScopedLinkAuthority(w, r, g, link, PermGroupsLinkAdd); !ok {
		return
	}
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	mode := strings.TrimSpace(body.Mode)
	if !db.ValidLinkMode(mode) {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("unknown mode %q (supported: %s, %s)",
				mode, db.LinkModeMembersToMembers, db.LinkModeOwnersToMembers))
		return
	}
	if mode == link.Mode {
		// No-op; report success without touching the row.
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "mode": mode, "changed": false})
		return
	}
	n, err := db.UpdateAgentGroupLinkMode(id, mode)
	if err != nil {
		if errors.Is(err, db.ErrLinkExists) {
			writeError(w, http.StatusConflict, "exists",
				"another link with the same from/to/mode already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no link %d", id))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "mode": mode, "changed": true})
}

func handleGroupLinksRemove(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "link id must be integer")
		return
	}
	link, err := db.GetAgentGroupLinkByID(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if link == nil {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no link %d", id))
		return
	}
	// URL-scoping: reject deletes scoped under a group that doesn't
	// participate in the link. Keeps `/groups/{g}/links/{id}`
	// meaningful — id is namespaced under the group the caller
	// authenticated against.
	if link.FromGroupID != g.ID && link.ToGroupID != g.ID {
		writeError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("link %d does not touch group %q", id, g.Name))
		return
	}
	// Auth: owner-bypass-only-when-g-is-FROM. Owners of the TO side
	// must hold groups.link.rm — they can't unilaterally cut their
	// inbound channels.
	if _, ok := requireScopedLinkAuthority(w, r, g, link, PermGroupsLinkRm); !ok {
		return
	}
	n, err := db.DeleteAgentGroupLink(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("no link %d", id))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// requireGroupLinkAuthority gates link-create (POST) where g is the
// FROM side of the link by construction. Caller passes if:
//   - they are human (no claude ancestor), OR
//   - they are an owner of g (the FROM group), OR
//   - they hold the requested slug, possibly via the X-Tclaude-Ask-Human
//     popup escape hatch.
//
// Owner-of-FROM is intentionally one-sided: an owner of A can open
// outbound channels from A unilaterally, mirroring the owner-as-super-
// member semantics already used for messaging. Mutating links where g
// is the destination still requires the slug — see
// requireScopedLinkAuthority for the PATCH/DELETE path that has to
// look at the actual link direction.
//
// We probe ownership first (no side effects) so that an owner caller
// never triggers the slug-denied error path. If neither human nor
// owner, fall through to requirePermission which handles the slug /
// popup / 403-with-helpful-message branches uniformly.
func requireGroupLinkAuthority(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, perm string) (string, bool) {
	// The owner-of-g structural bypass applies only to a confirmed agent
	// (it needs a conv-id). The human, and every fail-closed class, are
	// handled uniformly by requirePermission below.
	p := peerFromContext(r.Context())
	if classify(p) == classAgent {
		isOwner, err := db.IsAgentGroupOwner(g.ID, p.ConvID)
		if err == nil && isOwner {
			return p.ConvID, true
		}
	}
	return requirePermission(w, r, perm)
}

// requireScopedLinkAuthority is the PATCH/DELETE variant: the link is
// already fetched, so we know whether g is the FROM or TO side. The
// owner-of-g bypass ONLY applies when g is the FROM side of the
// supplied link. Owners of the TO side must hold the slug — they can't
// unilaterally manage links that point INTO their group.
func requireScopedLinkAuthority(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, link *db.AgentGroupLink, perm string) (string, bool) {
	// Owner-of-g bypass: confirmed agent only, and only when g is the
	// FROM side of the link. The human and every fail-closed class are
	// handled uniformly by requirePermission below.
	p := peerFromContext(r.Context())
	if classify(p) == classAgent && link != nil && link.FromGroupID == g.ID {
		isOwner, err := db.IsAgentGroupOwner(g.ID, p.ConvID)
		if err == nil && isOwner {
			return p.ConvID, true
		}
	}
	return requirePermission(w, r, perm)
}

// resolveGroupSelector resolves `s` to an AgentGroup. Accepts a group
// name (exact match) or a numeric id. We avoid prefix matching here
// — group names are short and meant to be exact-typed; ambiguity in
// a permission-mutating endpoint would be surprising.
func resolveGroupSelector(s string) (*db.AgentGroup, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("group selector is empty")
	}
	if id, err := strconv.ParseInt(s, 10, 64); err == nil {
		g, err := db.GetAgentGroupByID(id)
		if err != nil {
			return nil, err
		}
		if g == nil {
			return nil, fmt.Errorf("no group with id %d", id)
		}
		return g, nil
	}
	g, err := db.GetAgentGroupByName(s)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, fmt.Errorf("no group named %q", s)
	}
	return g, nil
}
