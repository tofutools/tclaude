package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Portable export/import (JOH-341). A template's wire JSON already
// round-trips (show --json → create/edit --file), but that is an
// internal wire shape. Export/import promote it to a deliberate,
// portable interchange format: the same inner template JSON wrapped in a
// small versioned envelope so a task force can be shared with a friend, a
// coworker, or your own other machine as one file.
//
//	GET  /v1/templates/{name}/export   → the envelope (open, read-only)
//	POST /v1/templates/import          → import an envelope (templates.manage)
//
// Because the envelope wraps the SAME inner template JSON that every
// other path uses, new template fields (work_pattern JOH-336, per-role
// launch profiles JOH-239, future process/choreography specs) ride along
// automatically — the envelope is serialization, not schema, so there is
// no migration here.
const (
	// templateExportFormat tags the envelope so an import can reject an
	// unrelated JSON file with a clear error instead of a confusing
	// field-by-field validation failure.
	templateExportFormat = "tclaude-task-force"
	// templateExportVersion is the highest envelope format version this
	// build writes and can import. Bump it when an older reader would lose a
	// safety-relevant field, even if that field is structurally additive.
	// Import accepts any version <= this and rejects anything newer with
	// an "upgrade tclaude" message.
	templateExportVersion = 3
)

// templateExportEnvelope is the portable file shape: a small versioned
// wrapper around the existing inner template JSON. ExportedAt is
// informational provenance only — import ignores it. The inner Template
// carries no machine-local identity (templateToJSON emits no DB id, and
// export blanks the local created_at/updated_at timestamps), so the file
// is a pure blueprint for another machine.
type templateExportEnvelope struct {
	Format        string       `json:"format"`
	FormatVersion int          `json:"format_version"`
	ExportedAt    string       `json:"exported_at,omitempty"`
	Template      templateJSON `json:"template"`
	// Roles embeds the full definitions of every role the template's agents
	// reference (JOH-240), so the export stays portable — an import re-creates
	// any that are MISSING on the target machine (never overwriting an existing
	// role of the same name; the same sacred-edits rule the seed follows). A
	// reference whose definition isn't embedded and doesn't exist locally is
	// dropped on import, exactly like an unknown spawn-profile reference.
	Roles []roleJSON `json:"roles,omitempty"`
	// Profiles embeds the full definitions of every spawn profile the template's
	// agents reference by name (JOH-350) — the profile
	// now carries the agent's launch shape AND its birth-time permissions/owner,
	// so it must travel for the export to reproduce the same team elsewhere.
	// Import materializes any that are MISSING locally (never overwriting an
	// existing profile of the same name — same sacred-edits rule as roles); a
	// reference that stays unresolved is dropped + warned on import, exactly like
	// today. Version 3 guarantees that the explicit disabled switch and its
	// independently retained reason are interpreted together; a version-2
	// reader would treat any remembered reason as disabled, so version-3 exports
	// must be rejected by that older reader.
	Profiles []spawnProfileJSON `json:"profiles,omitempty"`
	// SavedPrompts embeds the saved prompts those profiles reference; import
	// creates the missing ones before the profiles, never overwriting.
	SavedPrompts []db.SavedPrompt `json:"saved_prompts,omitempty"`
}

// collectReferencedRoles gathers every role referenced directly by a template
// agent or indirectly by one of its spawn profiles. Missing roles are silently
// skipped — import degrades a dangling reference anyway. Order is stable
// (direct references first, then profile references) and each role appears once.
func collectReferencedRoles(t *db.GroupTemplate, profiles []spawnProfileJSON) ([]roleJSON, error) {
	seen := map[string]bool{}
	out := []roleJSON{}
	add := func(ref string) error {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			return nil
		}
		seen[ref] = true
		rl, err := db.GetRole(ref)
		if err != nil {
			return err
		}
		if rl == nil {
			return nil
		}
		j := roleToJSON(rl)
		// Portable: strip the machine-local timestamps (like the template's).
		j.CreatedAt = ""
		j.UpdatedAt = ""
		out = append(out, j)
		return nil
	}
	for _, a := range t.Agents {
		if err := add(a.RoleRef); err != nil {
			return nil, err
		}
	}
	for _, p := range profiles {
		refs := p.RoleRefs
		if len(refs) == 0 && p.RoleRef != "" {
			refs = []string{p.RoleRef}
		}
		for _, ref := range refs {
			if err := add(ref); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// collectReferencedProfiles gathers the full definitions of every spawn profile
// the template references, for embedding in a portable export (JOH-350). A
// profile is referenced directly by an agent's spawn_profile. Roles are
// behavior/access presets and never carry launch-profile references.
// Missing profiles (a ref whose row was deleted since authoring) are silently
// skipped — import degrades such a dangling ref anyway. Order is stable
// (first-referenced) and each profile is embedded once.
func collectReferencedProfiles(t *db.GroupTemplate) ([]spawnProfileJSON, error) {
	seen := map[string]bool{}
	out := []spawnProfileJSON{}
	add := func(ref string) error {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			return nil
		}
		seen[ref] = true
		p, err := db.ResolveSpawnProfile(ref)
		if err != nil {
			return err
		}
		if p == nil {
			return nil
		}
		j := profileToJSON(p)
		// Portable: strip the machine-local timestamps (like the template's).
		j.CreatedAt = ""
		j.UpdatedAt = ""
		out = append(out, j)
		return nil
	}
	for _, a := range t.Agents {
		if err := add(a.SpawnProfile); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// handleTemplateExport serves GET /v1/templates/{name}/export: the named
// template wrapped in a portable envelope. Open + read-only, like GET
// /v1/templates/{name} — an export reveals nothing a fetch doesn't.
func handleTemplateExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET")
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "missing template name")
		return
	}
	t, err := db.GetGroupTemplate(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if t == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such template")
		return
	}
	inner := templateToJSON(t)
	// The local DB timestamps describe THIS machine's row, not the
	// blueprint — strip them so the file is portable provenance-free (the
	// envelope's exported_at carries the only meaningful timestamp).
	inner.CreatedAt = ""
	inner.UpdatedAt = ""
	// Embed the referenced spawn profiles too (JOH-350) — sweeping the agents'
	// direct refs — so a profile-driven
	// team's launch shape + birth-time permissions travel with the export.
	profiles, err := collectReferencedProfiles(t)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	// Embed roles referenced either directly by the template or indirectly by
	// one of those profiles, so the complete preset graph stays portable.
	roles, err := collectReferencedRoles(t, profiles)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	prompts, err := collectReferencedSavedPrompts(profilesPromptRefs(profiles)...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, templateExportEnvelope{
		Format:        templateExportFormat,
		FormatVersion: templateExportVersion,
		ExportedAt:    time.Now().UTC().Format(time.RFC3339),
		Template:      inner,
		Roles:         roles,
		Profiles:      profiles,
		SavedPrompts:  prompts,
	})
}

// templateImportResult is the import response: the final stored name,
// whether an existing template was overwritten, and any degradation
// warnings (stripped profile refs / unknown permission slugs). warnings
// is always non-nil so a CLI/JS consumer can range over it safely.
type templateImportResult struct {
	Imported string   `json:"imported"`
	Updated  bool     `json:"updated"`
	Warnings []string `json:"warnings"`
}

// sanitizeImportedTemplate makes a foreign template instantiable on THIS
// machine without hard-failing on references that may not exist locally
// (JOH-341). It strips — and reports a warning for — each machine-local
// reference the target can't honour:
//
//   - a spawn-profile reference (JOH-239) naming a profile that doesn't
//     exist here: the ref is cleared, leaving the agent's inline launch
//     overrides intact, so the agent degrades to the group/harness
//     default instead of failing the whole import;
//   - a permission slug the local slug registry doesn't know: dropped
//     from that agent so buildTemplateFromJSON's strict slug check (which
//     is correct for create/edit) doesn't reject the import.
//
// Everything else (harness/model/effort/sandbox/approval) is validated
// against the same machine-independent harness catalog by
// buildTemplateFromJSON afterwards, so it stays strict. Returns the
// cleaned copy plus the ordered warning list.
func sanitizeImportedTemplate(body templateJSON) (templateJSON, []string) {
	warnings := []string{}
	agents := make([]templateAgentJSON, len(body.Agents))
	for i, a := range body.Agents {
		label := strings.TrimSpace(a.Name)
		if label == "" {
			label = fmt.Sprintf("#%d", i+1)
		}
		if ref := strings.TrimSpace(a.SpawnProfile); ref != "" {
			p, err := db.ResolveSpawnProfile(ref)
			if err == nil && p == nil {
				warnings = append(warnings, fmt.Sprintf(
					"agent %q: spawn profile %q does not exist here — dropped the reference; the agent will use the group/harness default",
					label, ref))
				a.SpawnProfile = ""
			}
			// A GetSpawnProfile error is left for buildTemplateFromJSON to
			// surface as a 500 — an import shouldn't silently swallow a DB fault.
		}
		// A role reference that doesn't resolve locally — its definition wasn't
		// embedded in the export and no local role of that name exists (or the
		// embedded one collided with an existing role and wasn't re-created) —
		// is dropped so the agent falls through to its own overrides / the
		// harness default (JOH-240). Re-creatable roles were already restored
		// before this sanitize pass, so a surviving dangling ref is genuinely
		// unresolvable.
		if ref := strings.TrimSpace(a.RoleRef); ref != "" {
			rl, err := db.GetRole(ref)
			if err == nil && rl == nil {
				warnings = append(warnings, fmt.Sprintf(
					"agent %q: role %q does not exist here — dropped the behavior/access reference",
					label, ref))
				a.RoleRef = ""
			}
			// A GetRole error is left for buildTemplateFromJSON to surface.
		}
		if len(a.Permissions) > 0 {
			kept := make([]db.PermissionGrant, 0, len(a.Permissions))
			for _, grant := range a.Permissions {
				s := strings.TrimSpace(grant.Slug)
				if s == "" {
					continue
				}
				if !IsKnownPermSlug(s) {
					warnings = append(warnings, fmt.Sprintf(
						"agent %q: unknown permission slug %q — dropped", label, s))
					continue
				}
				// A scope this build cannot make sense of degrades the same way
				// an unknown slug does — the whole grant is DROPPED with a
				// warning, never kept unscoped. Keeping it would import a
				// deliberately narrow grant as a wildcard; 400ing the import
				// would break the forward-compat contract the slug arm states
				// one line up.
				if _, err := canonicalPermissionScopeForSlug(s, strings.TrimSpace(grant.Scope)); err != nil {
					warnings = append(warnings, fmt.Sprintf(
						"agent %q: permission %q carries a scope this build cannot read (%v) — "+
							"the whole grant was dropped rather than imported unscoped", label, s, err))
					continue
				}
				grant.Slug = s
				kept = append(kept, grant)
			}
			a.Permissions = kept
		}
		// The template-local profile's overrides degrade the same way the legacy
		// list does: a slug this machine doesn't know (a template exported from a
		// newer tclaude) is dropped with a warning instead of 400ing the whole
		// import in buildInlineProfileFromJSON's slug validation.
		if a.ProfileInline != nil && len(a.ProfileInline.PermissionOverrides) > 0 {
			kept := map[string]db.PermissionOverride{}
			for slug, override := range a.ProfileInline.PermissionOverrides {
				s := strings.TrimSpace(slug)
				if s == "" {
					continue
				}
				if !IsKnownPermSlug(s) {
					warnings = append(warnings, fmt.Sprintf(
						"agent %q: unknown permission slug %q in its custom launch config — dropped", label, s))
					continue
				}
				kept[s] = override
			}
			pi := *a.ProfileInline
			pi.PermissionOverrides = kept
			a.ProfileInline = &pi
		}
		agents[i] = a
	}
	body.Agents = agents
	// The board setup: what this node's boards cannot take is left out.
	var boardWarnings []string
	body.Taskboard, boardWarnings = sanitizeTaskboardSetup(body.Taskboard, nil)
	warnings = append(warnings, boardWarnings...)
	return body, warnings
}

// recreateMissingProfiles restores each embedded spawn profile that is MISSING
// on this machine (JOH-350), so a template imported from another machine can
// resolve its spawn_profile references (an agent's or an embedded role's). It
// mirrors recreateMissingRoles exactly: an existing profile of the same name is
// LEFT UNTOUCHED — never overwritten (sacred edits) — and the import reports it
// kept the local version; a definition that fails validation here is warned and
// skipped (the referencing agent/role then degrades in sanitize); a DB fault is
// a hard failure. It runs BEFORE recreateMissingRoles, because a role's own
// validation checks its referenced profile exists (buildRoleFromJSON), so the
// profiles must be in place before the roles that name them.
func recreateMissingProfiles(caller string, profiles []spawnProfileJSON) ([]string, *spawnFailure) {
	warnings := []string{}
	for _, pj := range profiles {
		name := strings.TrimSpace(pj.Name)
		if name == "" {
			continue
		}
		existing, err := db.GetSpawnProfile(name)
		if err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		if existing != nil {
			warnings = append(warnings, fmt.Sprintf(
				"spawn profile %q already exists here — kept the local version (import never overwrites a profile)", name))
			continue
		}
		p, fail := buildProfileFromJSON(pj)
		if fail != nil {
			warnings = append(warnings, fmt.Sprintf(
				"embedded spawn profile %q is invalid here (%s) — skipped; agents referencing it will use their own overrides",
				name, fail.Msg))
			continue
		}
		if fail := profileConferralFailure(nil, caller, nil, p, "", libraryProfileReach); fail != nil {
			warnings = append(warnings, fmt.Sprintf(
				"embedded spawn profile %q was not imported: %s", name, fail.Msg))
			continue
		}
		if _, err := db.CreateSpawnProfile(p); errors.Is(err, db.ErrSpawnProfileNameTaken) {
			// Lost a create race with a concurrent writer — the name now exists,
			// which is the outcome we wanted, so treat it as "kept".
			warnings = append(warnings, fmt.Sprintf(
				"spawn profile %q already exists here — kept the local version (import never overwrites a profile)", name))
		} else if err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		} else {
			warnings = append(warnings, fmt.Sprintf("imported spawn profile %q", name))
		}
	}
	return warnings, nil
}

// recreateMissingRoles restores each embedded role definition that is MISSING
// on this machine (JOH-240), so a template imported from another machine can
// resolve its role references. An existing role of the same name is LEFT
// UNTOUCHED — never overwritten — because a user's local edits to a role are
// sacred (the same rule the seed follows); the import reports that it kept the
// local version. A definition that fails validation is reported as a warning
// and skipped (the agent's role_ref then degrades in sanitizeImportedTemplate).
// A DB fault (not a validation problem) is returned as a hard failure — an
// import shouldn't silently swallow it.
//
// Two deliberate properties, both consistent with the rest of the import/
// instantiate path (which is intentionally non-atomic — a partial spawn is
// surfaced, not rolled back):
//   - No separate roles.manage gate. Import is a templates.manage operation;
//     re-creating a template's own EMBEDDED roles is part of making the imported
//     template usable, and it can only ADD missing roles (never overwrite). A
//     caller with templates.manage + instantiate can already grant arbitrary
//     slugs via a template's per-agent permissions, so this adds no privilege.
//     Requiring roles.manage too would just break portable import.
//   - Roles are created before buildTemplateFromJSON validates the template
//     (the ordering the sanitize pass needs — role refs must resolve first). A
//     template that fails later validation leaves the added roles behind; that
//     is benign (roles stand alone, and a re-import is idempotent — "kept
//     local").
func recreateMissingRoles(caller string, roles []roleJSON) ([]string, *spawnFailure) {
	warnings := []string{}
	for _, rj := range roles {
		name := strings.TrimSpace(rj.Name)
		if name == "" {
			continue
		}
		existing, err := db.GetRole(name)
		if err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		if existing != nil {
			warnings = append(warnings, fmt.Sprintf(
				"role %q already exists here — kept the local version (import never overwrites a role)", name))
			continue
		}
		rl, fail := buildRoleFromJSON(rj)
		if fail != nil {
			warnings = append(warnings, fmt.Sprintf(
				"embedded role %q is invalid here (%s) — skipped; agents referencing it will use their own overrides",
				name, fail.Msg))
			continue
		}
		if fail := roleConferralFailure(caller, nil, rl); fail != nil {
			warnings = append(warnings, fmt.Sprintf(
				"embedded role %q was not imported: %s", name, fail.Msg))
			continue
		}
		if _, err := db.CreateRole(rl); errors.Is(err, db.ErrRoleNameTaken) {
			// Lost a create race with a concurrent writer — the name now exists,
			// which is the outcome we wanted, so treat it as "kept".
			warnings = append(warnings, fmt.Sprintf(
				"role %q already exists here — kept the local version (import never overwrites a role)", name))
		} else if err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		} else {
			warnings = append(warnings, fmt.Sprintf("imported role %q", name))
		}
	}
	return warnings, nil
}

// handleTemplateImport serves POST /v1/templates/import: read a portable
// envelope and store its template locally. Gated on templates.manage
// (it writes a template, exactly like create/edit).
//
// Query knobs:
//   - as=<name>   store under a different name (rename on import)
//   - update=true overwrite an existing template of that name in place
//     (reuses the wholesale-replace machinery PATCH uses); without it, a
//     name collision is a 409 so an import never clobbers silently.
//
// Portability handling: the envelope's format/version are checked first
// (a newer format_version is rejected with an upgrade message), then
// machine-local references that may be absent here are stripped + warned
// (sanitizeImportedTemplate) so the stored template stays instantiable.
func handleTemplateImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST")
		return
	}
	caller, ok := requirePermission(w, r, PermTemplatesManage)
	if !ok {
		return
	}
	var env templateExportEnvelope
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "not valid task-force JSON: "+err.Error())
		return
	}
	if a := peerGrantFromRequest(r); a != nil {
		// The embedded profiles and roles carry launch settings and grants
		// for this node's spawns, which import judges against a local caller
		// only: from a linked node they take unrestricted trust.
		if len(env.Profiles) > 0 || len(env.Roles) > 0 {
			writeError(w, http.StatusForbidden, "permission",
				"a task force that embeds spawn profiles or roles takes unrestricted trust; export it without them")
			return
		}
		if len(env.SavedPrompts) > 0 {
			if _, ok := a.gate(w, r, PermPromptsManage, ActionContext{}); !ok {
				return
			}
		}
		if fail := peerTemplateEditFailure(r, nil, env.Template); fail != nil {
			writeSpawnFailure(w, fail)
			return
		}
	}
	asName := strings.TrimSpace(r.URL.Query().Get("as"))
	update := r.URL.Query().Get("update") == "true"
	// The template's board setup comes along unless left out.
	if queryFalse(r.URL.Query().Get("board_options")) {
		env.Template.Taskboard = nil
	}

	res, existed, fail := importTemplateEnvelope(caller, env, asName, update, httpTemplateOwnerScopesGate(w, r))
	if fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	// A collision without update is a 409 here — the import verb never
	// clobbers silently (the starter-install path turns the same collision
	// into a friendly skip instead; see handleStarterInstall).
	if existed && !update {
		writeError(w, http.StatusConflict, "exists", fmt.Sprintf(
			"a template named %q already exists — re-import with update to overwrite it, or as=<new-name> to import under a different name",
			res.Imported))
		return
	}
	status := http.StatusCreated
	if res.Updated {
		status = http.StatusOK
	}
	writeJSON(w, status, res)
}

// importTemplateEnvelope is the shared portable-import pipeline: it version-
// gates one task-force envelope, re-creates any embedded role that is MISSING
// locally (never overwriting an existing one — sacred edits), sanitizes the
// machine-local references that don't resolve here (spawn profiles, unknown
// slugs, dangling role refs — each warned), validates + builds the template,
// then creates it (or, when update is set, replaces it in place). asName, when
// non-empty, renames the template on import. ownerScopesGate authorizes an
// update that changes the stored template's owner scopes.
//
// It returns the result, whether a same-named template already EXISTED, and a
// failure. When a template already exists and update is false, it does NOT
// write — the caller decides what the collision means: /v1/templates/import
// reports a 409, a starter install reports a friendly skip. This is the single
// importer both paths share (JOH-246) — there is no second import path.
func importTemplateEnvelope(caller string, env templateExportEnvelope, asName string, update bool, ownerScopesGate templateOwnerScopesGate) (templateImportResult, bool, *spawnFailure) {
	if strings.TrimSpace(env.Format) != templateExportFormat {
		return templateImportResult{}, false, &spawnFailure{http.StatusBadRequest, "invalid_format", fmt.Sprintf(
			"not a tclaude task-force export (format=%q, expected %q)", env.Format, templateExportFormat)}
	}
	if env.FormatVersion < 1 {
		return templateImportResult{}, false, &spawnFailure{http.StatusBadRequest, "invalid_format",
			"missing or invalid format_version — not a valid task-force export"}
	}
	if env.FormatVersion > templateExportVersion {
		return templateImportResult{}, false, &spawnFailure{http.StatusBadRequest, "version_too_new", fmt.Sprintf(
			"this export is format_version %d, but this tclaude supports up to %d — upgrade tclaude to import it",
			env.FormatVersion, templateExportVersion)}
	}
	if env.FormatVersion >= 3 {
		if fail := validateExplicitProfileDisabledState(env.Profiles); fail != nil {
			return templateImportResult{}, false, fail
		}
	}
	env.Profiles = normalizeLegacyProfileDisabledState(env.Profiles, env.FormatVersion, 3)

	body := env.Template
	if asName != "" {
		body.Name = asName
	}

	// Re-create embedded roles first because an embedded spawn profile may
	// select one through role_ref. Existing roles are never overwritten.
	roleWarnings, rfail := recreateMissingRoles(caller, env.Roles)
	if rfail != nil {
		return templateImportResult{}, false, rfail
	}
	// Saved prompts likewise precede the profiles that reference them.
	promptWarnings, prfail := recreateMissingSavedPrompts(env.SavedPrompts)
	if prfail != nil {
		return templateImportResult{}, false, prfail
	}
	roleWarnings = append(roleWarnings, promptWarnings...)
	// Re-create any embedded spawn profile that is missing locally before the
	// sanitize pass checks agent profile references. Existing profiles are never
	// overwritten (sacred edits).
	profileWarnings, pfail := recreateMissingProfiles(caller, env.Profiles)
	if pfail != nil {
		return templateImportResult{}, false, pfail
	}

	cleaned, sanitizeWarnings := sanitizeImportedTemplate(body)
	warnings := append([]string{}, profileWarnings...)
	warnings = append(warnings, roleWarnings...)
	warnings = append(warnings, sanitizeWarnings...)
	t, fail := buildTemplateFromJSON(cleaned)
	if fail != nil {
		return templateImportResult{}, false, fail
	}

	existing, err := db.GetGroupTemplate(t.Name)
	if err != nil {
		return templateImportResult{}, false, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
	}
	if existing != nil {
		if !update {
			// Collision — leave the stored template untouched; the caller decides
			// (409 for import, skip for starter install).
			return templateImportResult{Imported: t.Name, Warnings: warnings}, true, nil
		}
		if fail := checkTemplateOwnerScopes(ownerScopesGate, existing, t.OwnerScopesJSON); fail != nil {
			return templateImportResult{}, true, fail
		}
		// Overwrite in place: the envelope carries the full desired state, so this
		// is a wholesale replace (the PATCH contract), reusing the existing row's id.
		t.ID = existing.ID
		if err := db.UpdateGroupTemplate(t); errors.Is(err, db.ErrGroupTemplateNameTaken) {
			return templateImportResult{}, true, &spawnFailure{http.StatusConflict, "exists", err.Error()}
		} else if err != nil {
			return templateImportResult{}, true, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		return templateImportResult{Imported: t.Name, Updated: true, Warnings: warnings}, true, nil
	}

	if _, err := db.CreateGroupTemplate(t); errors.Is(err, db.ErrGroupTemplateNameTaken) {
		// Lost a create race with a concurrent writer — surface as a plain 409.
		return templateImportResult{}, false, &spawnFailure{http.StatusConflict, "exists", err.Error()}
	} else if err != nil {
		return templateImportResult{}, false, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
	}
	return templateImportResult{Imported: t.Name, Warnings: warnings}, false, nil
}
