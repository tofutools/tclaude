package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Roles — the role library (JOH-240): named, reusable bundles of defaults a
// template roster agent can reference instead of re-typing them. A role
// carries a canonical role-brief (guidance folded into a "## Role" block in the
// referencing agent's startup context), a default launch shape (the same six
// launch fields template agents carry — JOH-239), and a default permission
// set. See pkg/claude/common/db/roles.go for the row shape.
//
// Wire surface (daemon Unix socket, SO_PEERCRED auth):
//
//	GET    /v1/roles          → list roles
//	POST   /v1/roles          → create a role
//	GET    /v1/roles/{name}   → fetch one role
//	PATCH  /v1/roles/{name}   → replace a role (full state)
//	DELETE /v1/roles/{name}   → delete a role
//
// Reads are open (introspection, like /v1/templates and /v1/spawn-profiles);
// mutations are gated on roles.manage (effectively human-only — a role is
// shared team config). The launch fields are validated against the ROLE'S OWN
// harness at save (model/effort through that harness's ModelCatalog), exactly
// as a spawn profile is — so a role stays harness-correct.

// roleJSON is the wire shape for a role. CreatedAt / UpdatedAt are
// response-only (ignored on input). Permissions is non-omitempty so a consumer
// can range over it safely.
type roleJSON struct {
	Name         string   `json:"name"`
	Descr        string   `json:"descr,omitempty"`
	Brief        string   `json:"brief,omitempty"`
	SpawnProfile string   `json:"spawn_profile,omitempty"`
	Harness      string   `json:"harness,omitempty"`
	Model        string   `json:"model,omitempty"`
	Effort       string   `json:"effort,omitempty"`
	Sandbox      string   `json:"sandbox,omitempty"`
	Approval     string   `json:"approval,omitempty"`
	Permissions  []string `json:"permissions"`
	CreatedAt    string   `json:"created_at,omitempty"`
	UpdatedAt    string   `json:"updated_at,omitempty"`
}

// roleToJSON projects a db.Role onto the wire shape, with a non-nil
// Permissions slice so the dashboard's JS .map() never trips on null.
func roleToJSON(rl *db.Role) roleJSON {
	perms := rl.Permissions
	if perms == nil {
		perms = []string{}
	}
	out := roleJSON{
		Name:         rl.Name,
		Descr:        rl.Descr,
		Brief:        rl.Brief,
		SpawnProfile: rl.SpawnProfile,
		Harness:      rl.Harness,
		Model:        rl.Model,
		Effort:       rl.Effort,
		Sandbox:      rl.Sandbox,
		Approval:     rl.Approval,
		Permissions:  perms,
	}
	if !rl.CreatedAt.IsZero() {
		out.CreatedAt = rl.CreatedAt.Format(time.RFC3339)
	}
	if !rl.UpdatedAt.IsZero() {
		out.UpdatedAt = rl.UpdatedAt.Format(time.RFC3339)
	}
	return out
}

// buildRoleFromJSON validates a wire-shape role and converts it to a db.Role.
// It returns a non-nil *spawnFailure (the generic "bad request" carrier) on the
// first validation problem so the caller can map it straight onto writeError.
//
// Validation mirrors buildProfileFromJSON + validateTemplateAgentLaunch: the
// name is a group-name-shaped handle (and may not be a reserved routing target
// like "all"); the brief is normalized like a group context (CRLF fold + length
// cap); the launch fields are validated against the role's own harness; a
// spawn-profile reference must exist; and every permission slug must be known.
// Blank launch fields stay blank ("" = inherit at instantiate).
func buildRoleFromJSON(body roleJSON) (*db.Role, *spawnFailure) {
	name := strings.TrimSpace(body.Name)
	if err := validateGroupName(name); err != nil {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg", "role name: " + err.Error()}
	}
	if db.ReservedRoleNames[strings.ToLower(name)] {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("role name %q is reserved (it is a routing target elsewhere) — pick another name", name)}
	}

	// The brief becomes a "## Role" block in the referencing agent's startup
	// context, so hold it to the same normalization + length bound a group
	// context gets.
	brief, err := normalizeGroupContext(body.Brief)
	if err != nil {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg", "brief: " + err.Error()}
	}

	// Resolve the role's harness — empty means Claude (the default). The
	// resolved harness gives the catalog the launch fields are validated
	// against; the harness is stored as the trimmed input (blank stays unset).
	hName := strings.TrimSpace(body.Harness)
	h, err := harness.ResolveSpawnable(hName)
	if err != nil {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_harness", err.Error()}
	}
	model, err := h.Models.ValidateModel(strings.TrimSpace(body.Model))
	if err != nil {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_model", err.Error()}
	}
	effort, err := h.Models.ValidateEffort(strings.TrimSpace(body.Effort))
	if err != nil {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_effort", err.Error()}
	}
	sandbox, err := harness.ValidateSandboxMode(h, body.Sandbox)
	if err != nil {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_sandbox", err.Error()}
	}
	approval, err := harness.ValidateApprovalPolicy(h, body.Approval)
	if err != nil {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_approval", err.Error()}
	}

	// A referenced spawn profile must exist here — same existence check the
	// template-agent launch validation applies.
	profRef := strings.TrimSpace(body.SpawnProfile)
	if profRef != "" {
		p, err := db.GetSpawnProfile(profRef)
		if err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		if p == nil {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_profile",
				fmt.Sprintf("no spawn profile named %q", profRef)}
		}
	}

	perms := []string{}
	for _, slug := range body.Permissions {
		slug = strings.TrimSpace(slug)
		if slug == "" {
			continue
		}
		if !IsKnownPermSlug(slug) {
			return nil, &spawnFailure{http.StatusBadRequest, "unknown_slug",
				fmt.Sprintf("unknown permission slug %q. Known slugs: %s.", slug, strings.Join(knownSlugs(), ", "))}
		}
		perms = append(perms, slug)
	}

	return &db.Role{
		Name:         name,
		Descr:        strings.TrimSpace(body.Descr),
		Brief:        brief,
		SpawnProfile: profRef,
		Harness:      hName,
		Model:        model,
		Effort:       effort,
		Sandbox:      sandbox,
		Approval:     approval,
		Permissions:  perms,
	}, nil
}

// handleRoles dispatches the collection endpoint /v1/roles: GET lists every
// role (open, read-only), POST creates one (gated on roles.manage).
func handleRoles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		roles, err := db.ListRoles()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		out := []roleJSON{}
		for _, rl := range roles {
			out = append(out, roleToJSON(rl))
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		if _, ok := requirePermission(w, r, PermRolesManage); !ok {
			return
		}
		var body roleJSON
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		rl, fail := buildRoleFromJSON(body)
		if fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
		id, err := db.CreateRole(rl)
		if errors.Is(err, db.ErrRoleNameTaken) {
			writeError(w, http.StatusConflict, "exists", err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": rl.Name})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST")
	}
}

// handleRoleByName dispatches /v1/roles/{name}: GET fetches one role (open),
// PATCH replaces it wholesale, DELETE removes it. PATCH is a full replace (the
// dashboard editor posts the complete desired state); renaming a role is a
// PATCH whose body carries the new name.
func handleRoleByName(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "missing role name")
		return
	}
	switch r.Method {
	case http.MethodGet:
		rl, err := db.GetRole(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if rl == nil {
			writeError(w, http.StatusNotFound, "not_found", "no such role")
			return
		}
		writeJSON(w, http.StatusOK, roleToJSON(rl))
	case http.MethodPatch:
		if _, ok := requirePermission(w, r, PermRolesManage); !ok {
			return
		}
		existing, err := db.GetRole(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if existing == nil {
			writeError(w, http.StatusNotFound, "not_found", "no such role")
			return
		}
		var body roleJSON
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		rl, fail := buildRoleFromJSON(body)
		if fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
		rl.ID = existing.ID
		if err := db.UpdateRole(rl); errors.Is(err, db.ErrRoleNameTaken) {
			writeError(w, http.StatusConflict, "exists", err.Error())
			return
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": rl.ID, "name": rl.Name})
	case http.MethodDelete:
		if _, ok := requirePermission(w, r, PermRolesManage); !ok {
			return
		}
		// Refuse while a template still references the role (JOH-351): roles
		// resolve at DEPLOY time, so deleting one a template names would silently
		// change that template's next deploy. Refusal is predictable — clear the
		// references (or repoint them) first. The check is a cheap indexed read.
		refs, err := db.TemplatesReferencingRole(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if len(refs) > 0 {
			plural := ""
			if len(refs) != 1 {
				plural = "s"
			}
			writeError(w, http.StatusConflict, "role_in_use",
				fmt.Sprintf("role %q is still referenced by %d template%s (%s) — edit those templates to drop or repoint the reference before deleting the role",
					name, len(refs), plural, strings.Join(refs, ", ")))
			return
		}
		n, err := db.DeleteRole(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "no such role")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET, PATCH or DELETE")
	}
}
