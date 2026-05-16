package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Group templates — reusable blueprints for instantiating a working
// group. A template is NOT a group export: an export is a conv-bound
// snapshot of a live group (DB rows + .jsonl), whereas a template has
// no conv-ids. Instantiating one creates a fresh group and spawns one
// new agent per template-agent spec.
//
// Wire surface (daemon Unix socket, SO_PEERCRED auth):
//
//	GET    /v1/templates                       → list templates
//	POST   /v1/templates                       → create a template
//	GET    /v1/templates/{name}                → fetch one template
//	PATCH  /v1/templates/{name}                → replace a template (full state)
//	DELETE /v1/templates/{name}                → delete a template
//	POST   /v1/templates/{name}/instantiate    → create a group + spawn its team
//	POST   /v1/templates/from-group            → snapshot a live group into a template
//
// Reads are open (introspection, like /v1/permissions); mutations are
// gated on templates.manage; instantiate is gated on
// templates.instantiate. Both slugs are effectively human-only by
// default — instantiate in particular spawns a whole team at once.

// templateAgentJSON is the wire shape for one agent in a template —
// used both in request bodies (the dashboard editor) and responses.
type templateAgentJSON struct {
	Name           string   `json:"name"`
	Role           string   `json:"role,omitempty"`
	Descr          string   `json:"descr,omitempty"`
	InitialMessage string   `json:"initial_message,omitempty"`
	IsOwner        bool     `json:"is_owner,omitempty"`
	Permissions    []string `json:"permissions"`
}

// templateJSON is the wire shape for a whole template. CreatedAt /
// UpdatedAt are response-only (ignored on input).
type templateJSON struct {
	Name           string              `json:"name"`
	Descr          string              `json:"descr,omitempty"`
	DefaultContext string              `json:"default_context,omitempty"`
	Agents         []templateAgentJSON `json:"agents"`
	CreatedAt      string              `json:"created_at,omitempty"`
	UpdatedAt      string              `json:"updated_at,omitempty"`
}

// templateToJSON projects a db.GroupTemplate onto the wire shape, with
// non-nil slices so the dashboard's JS .map() never trips on null.
func templateToJSON(t *db.GroupTemplate) templateJSON {
	out := templateJSON{
		Name:           t.Name,
		Descr:          t.Descr,
		DefaultContext: t.DefaultContext,
		Agents:         []templateAgentJSON{},
	}
	if !t.CreatedAt.IsZero() {
		out.CreatedAt = t.CreatedAt.Format(time.RFC3339)
	}
	if !t.UpdatedAt.IsZero() {
		out.UpdatedAt = t.UpdatedAt.Format(time.RFC3339)
	}
	for _, a := range t.Agents {
		perms := a.Permissions
		if perms == nil {
			perms = []string{}
		}
		out.Agents = append(out.Agents, templateAgentJSON{
			Name:           a.Name,
			Role:           a.Role,
			Descr:          a.Descr,
			InitialMessage: a.InitialMessage,
			IsOwner:        a.IsOwner,
			Permissions:    perms,
		})
	}
	return out
}

// collectTemplatesSnapshot builds the dashboard Templates tab's data.
// Returns an empty (non-nil) slice on error or when there are no
// templates, so the page's JS .map() never trips on null.
func collectTemplatesSnapshot() []templateJSON {
	out := []templateJSON{}
	templates, err := db.ListGroupTemplates()
	if err != nil {
		return out
	}
	for _, t := range templates {
		out = append(out, templateToJSON(t))
	}
	return out
}

// buildTemplateFromJSON validates a wire-shape template and converts it
// to a db.GroupTemplate. It returns a non-nil *spawnFailure (reused as
// a generic "bad request" carrier) on the first validation problem so
// the caller can map it straight onto writeError.
//
// Validation:
//   - name follows the same rules as a group name (it is the route key
//     /v1/templates/{name} and, at instantiate time, the prefix of
//     every spawned agent's name)
//   - default_context is CRLF-normalised and capped at 16 KiB
//   - each agent name is non-empty, control-char-free, slash-free
//     (the final name "<group>-<agent>" is used as a /rename title)
//     and unique within the template
//   - each agent's initial_message clears the inbox charset/length rule
//   - each permission slug is registered (catches typos early)
//   - at most one agent is marked owner
func buildTemplateFromJSON(body templateJSON) (*db.GroupTemplate, *spawnFailure) {
	name := strings.TrimSpace(body.Name)
	if err := validateGroupName(name); err != nil {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg", "template name: " + err.Error()}
	}
	ctx, err := normalizeGroupContext(body.DefaultContext)
	if err != nil {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg", err.Error()}
	}
	t := &db.GroupTemplate{
		Name:           name,
		Descr:          strings.TrimSpace(body.Descr),
		DefaultContext: ctx,
		Agents:         []db.GroupTemplateAgent{},
	}
	seenNames := map[string]bool{}
	owners := 0
	for i, a := range body.Agents {
		an := strings.TrimSpace(a.Name)
		if an == "" {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("agent #%d: name is required", i+1)}
		}
		if strings.ContainsAny(an, "/\\") {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("agent %q: name must not contain slashes", an)}
		}
		for _, r := range an {
			if r < 0x20 || r == 0x7f {
				return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
					fmt.Sprintf("agent %q: name must not contain control characters", an)}
			}
		}
		if seenNames[an] {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("duplicate agent name %q — each agent in a template needs a distinct name", an)}
		}
		seenNames[an] = true

		im := strings.TrimSpace(a.InitialMessage)
		if !isValidInitialMessage(im) {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("agent %q: initial_message must be at most %d characters; newlines and tabs "+
					"are allowed but other control characters are not", an, agent.MaxInitialMessageBytes)}
		}

		perms := []string{}
		for _, slug := range a.Permissions {
			slug = strings.TrimSpace(slug)
			if slug == "" {
				continue
			}
			if !IsKnownPermSlug(slug) {
				return nil, &spawnFailure{http.StatusBadRequest, "unknown_slug",
					fmt.Sprintf("agent %q: unknown permission slug %q. Known slugs: %s.",
						an, slug, strings.Join(knownSlugs(), ", "))}
			}
			perms = append(perms, slug)
		}
		if a.IsOwner {
			owners++
		}
		t.Agents = append(t.Agents, db.GroupTemplateAgent{
			Ordinal:        i,
			Name:           an,
			Role:           strings.TrimSpace(a.Role),
			Descr:          strings.TrimSpace(a.Descr),
			InitialMessage: im,
			IsOwner:        a.IsOwner,
			Permissions:    perms,
		})
	}
	if owners > 1 {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
			"at most one agent may be marked owner"}
	}
	return t, nil
}

// handleTemplates dispatches the collection endpoint /v1/templates:
// GET lists every template (open, read-only), POST creates one (gated
// on templates.manage).
func handleTemplates(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		templates, err := db.ListGroupTemplates()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		out := []templateJSON{}
		for _, t := range templates {
			out = append(out, templateToJSON(t))
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		if _, ok := requirePermission(w, r, PermTemplatesManage); !ok {
			return
		}
		var body templateJSON
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		t, fail := buildTemplateFromJSON(body)
		if fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
		id, err := db.CreateGroupTemplate(t)
		if errors.Is(err, db.ErrGroupTemplateNameTaken) {
			writeError(w, http.StatusConflict, "exists", err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": t.Name})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST")
	}
}

// handleTemplateByName dispatches /v1/templates/{name}: GET fetches one
// template (open), PATCH replaces it wholesale, DELETE removes it.
//
// PATCH is a full replace, not a field-merge: the dashboard editor
// always posts the template's complete desired state, so a partial
// merge would have no caller and only invite drift between the form
// and the stored rows.
func handleTemplateByName(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "missing template name")
		return
	}
	switch r.Method {
	case http.MethodGet:
		t, err := db.GetGroupTemplate(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if t == nil {
			writeError(w, http.StatusNotFound, "not_found", "no such template")
			return
		}
		writeJSON(w, http.StatusOK, templateToJSON(t))
	case http.MethodPatch:
		if _, ok := requirePermission(w, r, PermTemplatesManage); !ok {
			return
		}
		existing, err := db.GetGroupTemplate(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if existing == nil {
			writeError(w, http.StatusNotFound, "not_found", "no such template")
			return
		}
		var body templateJSON
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		t, fail := buildTemplateFromJSON(body)
		if fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
		t.ID = existing.ID
		if err := db.UpdateGroupTemplate(t); errors.Is(err, db.ErrGroupTemplateNameTaken) {
			writeError(w, http.StatusConflict, "exists", err.Error())
			return
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": t.ID, "name": t.Name})
	case http.MethodDelete:
		if _, ok := requirePermission(w, r, PermTemplatesManage); !ok {
			return
		}
		n, err := db.DeleteGroupTemplate(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "no such template")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET, PATCH or DELETE")
	}
}

// composeInstantiationContext folds the per-instantiation task/project
// text into the template's reusable boilerplate. The template context
// is rarely-changed group-wide guidance; the task is the specific
// assignment for THIS group, so it lands under a "## Task" header that
// every spawned agent sees in its startup briefing.
func composeInstantiationContext(templateContext, task string) string {
	templateContext = strings.TrimSpace(templateContext)
	task = strings.TrimSpace(task)
	switch {
	case task == "":
		return templateContext
	case templateContext == "":
		return "## Task\n\n" + task
	default:
		return templateContext + "\n\n## Task\n\n" + task
	}
}

// instantiateAgentResult is the per-agent outcome of an instantiation.
type instantiateAgentResult struct {
	Name      string   `json:"name"`       // the template agent name
	FinalName string   `json:"final_name"` // "<group>-<name>"
	ConvID    string   `json:"conv_id,omitempty"`
	Owner     bool     `json:"owner,omitempty"`
	Granted   []string `json:"granted,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// handleTemplateInstantiate creates a fresh group from a template and
// spawns its whole agent team. Gated on templates.instantiate.
//
// Body: { group_name, task, cwd?, descr? }. group_name doubles as the
// agent-name prefix — agent "PO" in the template becomes
// "<group_name>-PO". task is the multi-line assignment, folded into the
// group's default_context so every member's startup briefing carries
// it.
//
// Agents are spawned sequentially via the shared executeSpawn core. A
// per-agent spawn failure is recorded and reported but does NOT abort
// the rest: tearing half-spawned agents back down is destructive, so a
// partial team is surfaced for the human to finish or retry by hand.
func handleTemplateInstantiate(w http.ResponseWriter, r *http.Request) {
	caller, ok := requirePermission(w, r, PermTemplatesUse)
	if !ok {
		return
	}
	tmplName := r.PathValue("name")
	tmpl, err := db.GetGroupTemplate(tmplName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if tmpl == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such template")
		return
	}

	var body struct {
		GroupName string `json:"group_name"`
		Task      string `json:"task,omitempty"`
		Cwd       string `json:"cwd,omitempty"`
		Descr     string `json:"descr,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	body.GroupName = strings.TrimSpace(body.GroupName)
	if err := validateGroupName(body.GroupName); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "group_name: "+err.Error())
		return
	}
	if existing, _ := db.GetAgentGroupByName(body.GroupName); existing != nil {
		writeError(w, http.StatusConflict, "exists", "a group named "+body.GroupName+" already exists")
		return
	}
	cwd, err := resolveGroupDefaultCwd(body.Cwd)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_cwd", err.Error())
		return
	}
	groupContext, err := normalizeGroupContext(composeInstantiationContext(tmpl.DefaultContext, body.Task))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}

	descr := strings.TrimSpace(body.Descr)
	if descr == "" {
		descr = "Instantiated from template " + tmpl.Name
	}
	gid, err := db.CreateAgentGroup(body.GroupName, descr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "create group: "+err.Error())
		return
	}
	// Best-effort post-create config — a failure here is logged, not
	// fatal: the group exists and the human can adjust it on the
	// dashboard. Mirrors the /v1/groups create path.
	if cwd != "" {
		if _, err := db.SetAgentGroupDefaultCwd(body.GroupName, cwd); err != nil {
			slog.Warn("instantiate: set default cwd failed", "group", body.GroupName, "error", err)
		}
	}
	if groupContext != "" {
		if _, err := db.SetAgentGroupDefaultContext(body.GroupName, groupContext); err != nil {
			slog.Warn("instantiate: set default context failed", "group", body.GroupName, "error", err)
		}
	}

	g := &db.AgentGroup{ID: gid, Name: body.GroupName, Descr: descr, DefaultCwd: cwd, DefaultContext: groupContext}
	granter := granterLabel(caller)

	results := []instantiateAgentResult{}
	spawned, failed := 0, 0
	for _, a := range tmpl.Agents {
		finalName := body.GroupName + "-" + a.Name
		res := instantiateAgentResult{Name: a.Name, FinalName: finalName}
		outcome, fail := executeSpawn(g, spawnParams{
			Name:           finalName,
			Role:           a.Role,
			Descr:          a.Descr,
			InitialMessage: a.InitialMessage,
			Cwd:            cwd,
			GroupContext:   groupContext,
			ReplyToConv:    caller,
		})
		if fail != nil {
			res.Error = fail.Msg
			failed++
			results = append(results, res)
			continue
		}
		res.ConvID = outcome.ConvID
		spawned++

		// Ownership + permission grants — best-effort. The agent is
		// already spawned and group-joined; a failed grant is logged and
		// surfaced in the result note but does not fail the whole
		// instantiation.
		if a.IsOwner {
			if err := db.AddAgentGroupOwner(gid, outcome.ConvID, granter); err != nil {
				slog.Warn("instantiate: grant owner failed",
					"group", body.GroupName, "conv", outcome.ConvID, "error", err)
				res.Error = "spawned, but grant-owner failed: " + err.Error()
			} else {
				res.Owner = true
			}
		}
		for _, slug := range a.Permissions {
			if err := db.GrantAgentPermission(outcome.ConvID, slug, granter); err != nil {
				slog.Warn("instantiate: grant permission failed",
					"conv", outcome.ConvID, "slug", slug, "error", err)
				continue
			}
			res.Granted = append(res.Granted, slug)
		}
		results = append(results, res)
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"group":    body.GroupName,
		"template": tmpl.Name,
		"agents":   results,
		"spawned":  spawned,
		"failed":   failed,
	})
}

// handleTemplateFromGroup snapshots a live group's structure into a new
// template — the reverse direction of instantiate. Gated on
// templates.manage. Body: { group, template_name }.
//
// It carries over the group's descr + default_context and one template
// agent per group member (role, descr, owner flag, the member's
// per-conv permission grants). It does NOT carry per-agent task briefs:
// a live group has no stored "initial message" per member, so
// initial_message comes through blank for the human to fill in the
// editor afterwards.
func handleTemplateFromGroup(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermTemplatesManage); !ok {
		return
	}
	var body struct {
		Group        string `json:"group"`
		TemplateName string `json:"template_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	body.Group = strings.TrimSpace(body.Group)
	body.TemplateName = strings.TrimSpace(body.TemplateName)
	if err := validateGroupName(body.TemplateName); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "template_name: "+err.Error())
		return
	}
	g, err := db.GetAgentGroupByName(body.Group)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if g == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such group "+body.Group)
		return
	}
	if existing, _ := db.GetGroupTemplate(body.TemplateName); existing != nil {
		writeError(w, http.StatusConflict, "exists", "a template named "+body.TemplateName+" already exists")
		return
	}

	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	ownerSet := map[string]bool{}
	if owners, err := db.ListAgentGroupOwners(g.ID); err == nil {
		for _, o := range owners {
			ownerSet[o.ConvID] = true
		}
	}

	t := &db.GroupTemplate{
		Name:           body.TemplateName,
		Descr:          g.Descr,
		DefaultContext: g.DefaultContext,
		Agents:         []db.GroupTemplateAgent{},
	}
	memberSet := map[string]bool{}
	usedNames := map[string]bool{}
	addAgent := func(convID, role, descr string, owner bool) {
		name := deriveTemplateAgentName(convID, role, len(t.Agents)+1, usedNames)
		perms, _ := db.ListAgentPermissionsForConv(convID)
		if perms == nil {
			perms = []string{}
		}
		t.Agents = append(t.Agents, db.GroupTemplateAgent{
			Ordinal:     len(t.Agents),
			Name:        name,
			Role:        role,
			Descr:       descr,
			IsOwner:     owner,
			Permissions: perms,
		})
	}
	for _, m := range members {
		memberSet[m.ConvID] = true
		addAgent(m.ConvID, m.Role, m.Descr, ownerSet[m.ConvID])
	}
	// Pure owners — owners that aren't members — still belong in the
	// snapshot so the template's owner isn't silently dropped.
	for ownerConv := range ownerSet {
		if !memberSet[ownerConv] {
			addAgent(ownerConv, "owner", "", true)
		}
	}

	id, err := db.CreateGroupTemplate(t)
	if errors.Is(err, db.ErrGroupTemplateNameTaken) {
		writeError(w, http.StatusConflict, "exists", err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	t.ID = id
	writeJSON(w, http.StatusCreated, templateToJSON(t))
}

// deriveTemplateAgentName picks a template-agent name when snapshotting
// a live group: the member's conversation title, sanitised into a
// slug-ish handle (the name becomes part of a /rename title at
// instantiate time). Falls back to the role, then to "agent-<n>", and
// disambiguates collisions with a numeric suffix. The human edits the
// template afterwards anyway, so this only needs to be a sensible
// starting point.
func deriveTemplateAgentName(convID, role string, ordinal int, used map[string]bool) string {
	base := sanitizeAgentName(agent.FreshTitle(convID))
	if base == "" {
		base = sanitizeAgentName(role)
	}
	if base == "" {
		base = fmt.Sprintf("agent-%d", ordinal)
	}
	name := base
	for i := 2; used[name]; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	used[name] = true
	return name
}

// sanitizeAgentName reduces an arbitrary title to a template-agent
// name: runs of non-[A-Za-z0-9._-] characters collapse to a single
// dash, and leading/trailing dashes are trimmed.
func sanitizeAgentName(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
		if ok {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteRune('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
