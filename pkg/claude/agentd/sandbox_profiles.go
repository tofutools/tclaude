package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

const (
	sandboxProfileExportFormat  = "tclaude-sandbox-profiles"
	sandboxProfileExportVersion = 1
)

// sandboxProfileBeforeMkdir is a test seam for exercising substitutions in
// the narrow window between portable validation and descriptor-relative
// creation. Production leaves it as a no-op.
var sandboxProfileBeforeMkdir = func(string) {}

type sandboxProfileJSON struct {
	Name        string                           `json:"name"`
	Filesystem  []sandboxpolicy.FilesystemGrant  `json:"filesystem"`
	Environment []sandboxpolicy.EnvironmentEntry `json:"environment"`
	Includes    []string                         `json:"includes,omitempty"`
	CreatedAt   string                           `json:"created_at,omitempty"`
	UpdatedAt   string                           `json:"updated_at,omitempty"`
}

type sandboxProfileExportEnvelope struct {
	Format           string                         `json:"format"`
	FormatVersion    int                            `json:"format_version"`
	ExportedAt       string                         `json:"exported_at,omitempty"`
	Profiles         []sandboxProfileJSON           `json:"profiles"`
	Assignments      *sandboxProfileAssignmentsJSON `json:"assignments,omitempty"`
	OnConflict       string                         `json:"on_conflict,omitempty"`       // import only: error|skip|overwrite
	ApplyAssignments bool                           `json:"apply_assignments,omitempty"` // import only; explicit to avoid cross-machine surprises
}

type sandboxProfileAssignmentsJSON struct {
	Global string            `json:"global,omitempty"`
	Groups map[string]string `json:"groups,omitempty"`
}

type sandboxProfilePreviewJSON struct {
	Before *sandboxProfileJSON `json:"before,omitempty"`
	After  sandboxProfileJSON  `json:"after"`
	// Revision couples an edit preview to its eventual PATCH. It is omitted for
	// creates, whose unique-name constraint already protects the commit.
	Revision string `json:"revision,omitempty"`
}

func sandboxProfileToJSON(p *db.SandboxProfile, localFields bool) sandboxProfileJSON {
	out := sandboxProfileJSON{
		Name: p.Name, Filesystem: p.Filesystem, Environment: p.Environment, Includes: p.Includes,
	}
	if localFields {
		if !p.CreatedAt.IsZero() {
			out.CreatedAt = p.CreatedAt.Format(time.RFC3339)
		}
		if !p.UpdatedAt.IsZero() {
			out.UpdatedAt = p.UpdatedAt.Format(time.RFC3339)
		}
	}
	return out
}

func buildSandboxProfile(body sandboxProfileJSON) (*db.SandboxProfile, []string, error) {
	normalized, missing, err := sandboxpolicy.NormalizeForPersistence(sandboxpolicy.Profile{
		Name: body.Name, Filesystem: body.Filesystem, Environment: body.Environment, Includes: body.Includes,
	})
	if err != nil {
		return nil, nil, err
	}
	return &db.SandboxProfile{
		Name: normalized.Name, Filesystem: normalized.Filesystem, Environment: normalized.Environment, Includes: normalized.Includes,
	}, missing, nil
}

func buildSandboxProfileForImport(body sandboxProfileJSON) (*db.SandboxProfile, []string, error) {
	normalized, missing, err := sandboxpolicy.NormalizeForImport(sandboxpolicy.Profile{
		Name: body.Name, Filesystem: body.Filesystem, Environment: body.Environment, Includes: body.Includes,
	})
	if err != nil {
		return nil, nil, err
	}
	return &db.SandboxProfile{
		Name: normalized.Name, Filesystem: normalized.Filesystem, Environment: normalized.Environment, Includes: normalized.Includes,
	}, missing, nil
}

// handleSandboxProfiles exposes the profile registry. Every method requires
// sandbox-profiles.manage: values are explicitly documented as non-secret, but
// a mistaken credential must not become readable by every local agent.
func handleSandboxProfiles(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermSandboxProfilesManage); !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		profiles, err := db.ListSandboxProfiles()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		out := make([]sandboxProfileJSON, 0, len(profiles))
		for _, p := range profiles {
			out = append(out, sandboxProfileToJSON(p, true))
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var body sandboxProfileJSON
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		p, missing, err := buildSandboxProfile(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", err.Error())
			return
		}
		if r.URL.Query().Get("dry_run") != "" {
			existing, err := db.GetSandboxProfile(p.Name)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "io", err.Error())
				return
			}
			if existing != nil {
				writeError(w, http.StatusConflict, "exists", db.ErrSandboxProfileNameTaken.Error())
				return
			}
			writeJSON(w, http.StatusOK, sandboxProfilePreviewJSON{
				After: sandboxProfileToJSON(p, false),
			})
			return
		}
		id, err := db.CreateSandboxProfile(p)
		if errors.Is(err, db.ErrSandboxProfileNameTaken) {
			writeError(w, http.StatusConflict, "exists", err.Error())
			return
		}
		if errors.Is(err, db.ErrSandboxProfileInvalidInclude) {
			writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": p.Name, "missing": missing})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST")
	}
}

func handleSandboxProfileByName(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermSandboxProfilesManage); !ok {
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "missing sandbox profile name")
		return
	}
	switch r.Method {
	case http.MethodGet:
		p, err := db.GetSandboxProfile(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if p == nil {
			writeError(w, http.StatusNotFound, "not_found", "no such sandbox profile")
			return
		}
		writeJSON(w, http.StatusOK, sandboxProfileToJSON(p, true))
	case http.MethodPatch:
		existing, err := db.GetSandboxProfile(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if existing == nil {
			writeError(w, http.StatusNotFound, "not_found", "no such sandbox profile")
			return
		}
		if revision := r.URL.Query().Get("revision"); revision != "" && revision != existing.UpdatedAt.Format(time.RFC3339Nano) {
			writeError(w, http.StatusConflict, "changed", "sandbox profile changed since preview; reopen it and review the latest changes")
			return
		}
		var body sandboxProfileJSON
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		p, missing, err := buildSandboxProfile(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", err.Error())
			return
		}
		p.ID = existing.ID
		if r.URL.Query().Get("dry_run") != "" {
			if p.Name != existing.Name {
				collision, err := db.GetSandboxProfile(p.Name)
				if err != nil {
					writeError(w, http.StatusInternalServerError, "io", err.Error())
					return
				}
				if collision != nil && collision.ID != existing.ID {
					writeError(w, http.StatusConflict, "exists", db.ErrSandboxProfileNameTaken.Error())
					return
				}
			}
			before := sandboxProfileToJSON(existing, false)
			writeJSON(w, http.StatusOK, sandboxProfilePreviewJSON{
				Before:   &before,
				After:    sandboxProfileToJSON(p, false),
				Revision: existing.UpdatedAt.Format(time.RFC3339Nano),
			})
			return
		}
		var updateErr error
		if revision := r.URL.Query().Get("revision"); revision != "" {
			updateErr = db.UpdateSandboxProfileIfUnchanged(p, revision)
		} else {
			updateErr = db.UpdateSandboxProfile(p)
		}
		if errors.Is(updateErr, db.ErrSandboxProfileNameTaken) {
			writeError(w, http.StatusConflict, "exists", updateErr.Error())
			return
		} else if errors.Is(updateErr, db.ErrSandboxProfileInvalidInclude) {
			writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", updateErr.Error())
			return
		} else if errors.Is(updateErr, db.ErrSandboxProfileChanged) {
			writeError(w, http.StatusConflict, "changed", "sandbox profile changed since preview; reopen it and review the latest changes")
			return
		} else if updateErr != nil {
			writeError(w, http.StatusInternalServerError, "io", updateErr.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": p.ID, "name": p.Name, "missing": missing})
	case http.MethodDelete:
		n, err := db.DeleteSandboxProfile(name)
		if errors.Is(err, db.ErrSandboxProfileIncludedBy) {
			writeError(w, http.StatusConflict, "included", err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "no such sandbox profile")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET, PATCH or DELETE")
	}
}

// handleSandboxProfileDirectories backs the dashboard editor's explicit
// mkdir-p affordance. Inspect is side-effect free; create only materializes
// paths that the normal portable-profile validator identifies as missing.
// A strict normalization after creation makes the response fail closed if a
// path did not become a real, safe directory.
func handleSandboxProfileDirectories(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermSandboxProfilesManage); !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST")
		return
	}
	var body sandboxProfileJSON
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	// Directory inspection/creation is independent of the draft's name and
	// environment fields. Validate only the filesystem rules so an unrelated
	// in-progress environment edit cannot hide or block the mkdir affordance.
	body.Name = "directory-preview"
	body.Environment = nil
	profile, missing, err := buildSandboxProfile(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", err.Error())
		return
	}
	creatable := creatableSandboxProfileDirectories(profile, missing)
	if r.PathValue("action") == "create" {
		for _, path := range creatable {
			sandboxProfileBeforeMkdir(path)
			if err := mkdirAllNoFollow(path, 0o755); err != nil {
				writeError(w, http.StatusInternalServerError, "io", fmt.Sprintf("create directory %q: %v", path, err))
				return
			}
		}
		active := make([]sandboxpolicy.FilesystemGrant, 0, len(profile.Filesystem))
		for _, grant := range profile.Filesystem {
			if grant.Access != sandboxpolicy.AccessDeny {
				active = append(active, grant)
			}
		}
		if _, err := sandboxpolicy.Normalize(sandboxpolicy.Profile{
			Name: profile.Name, Filesystem: active, Environment: profile.Environment,
		}); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", "validate created directories: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"created": creatable})
		return
	} else if r.PathValue("action") != "inspect" {
		writeError(w, http.StatusNotFound, "not_found", "unknown sandbox-profile directory action")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"missing": missing, "creatable": creatable})
}

func creatableSandboxProfileDirectories(profile *db.SandboxProfile, missing []string) []string {
	missingSet := make(map[string]bool, len(missing))
	for _, path := range missing {
		missingSet[path] = true
	}
	out := make([]string, 0, len(missing))
	for _, grant := range profile.Filesystem {
		// A missing deny rule is already restrictive and creating its target
		// would unexpectedly mutate the host without enabling an allowance.
		if grant.Access != sandboxpolicy.AccessDeny && missingSet[grant.Path] {
			out = append(out, grant.Path)
		}
	}
	return out
}

func handleGlobalSandboxProfile(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		p, err := db.GetGlobalSandboxProfile()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		name := ""
		if p != nil {
			name = p.Name
		}
		writeJSON(w, http.StatusOK, map[string]any{"name": name})
	case http.MethodPut:
		if _, ok := requirePermission(w, r, PermSandboxProfilesManage); !ok {
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		body.Name = strings.TrimSpace(body.Name)
		if body.Name == "" {
			writeError(w, http.StatusBadRequest, "invalid_arg", "sandbox profile name is required")
			return
		}
		if err := db.SetGlobalSandboxProfile(body.Name); errors.Is(err, db.ErrSandboxProfileNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"name": body.Name})
	case http.MethodDelete:
		if _, ok := requirePermission(w, r, PermSandboxProfilesManage); !ok {
			return
		}
		if err := db.SetGlobalSandboxProfile(""); err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"name": ""})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET, PUT or DELETE")
	}
}

func handleGroupSandboxProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut || r.Method == http.MethodDelete {
		if _, ok := requirePermission(w, r, PermSandboxProfilesManage); !ok {
			return
		}
	}
	group := r.PathValue("group")
	g, err := db.GetAgentGroupByName(group)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if g == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such group")
		return
	}
	switch r.Method {
	case http.MethodGet:
		p, err := db.GetAgentGroupSandboxProfile(g.Name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		name := ""
		if p != nil {
			name = p.Name
		}
		writeJSON(w, http.StatusOK, map[string]any{"group": g.Name, "name": name})
	case http.MethodPut:
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		body.Name = strings.TrimSpace(body.Name)
		if body.Name == "" {
			writeError(w, http.StatusBadRequest, "invalid_arg", "sandbox profile name is required")
			return
		}
		if _, err := db.SetAgentGroupSandboxProfile(g.Name, body.Name); errors.Is(err, db.ErrSandboxProfileNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"group": g.Name, "name": body.Name})
	case http.MethodDelete:
		if _, err := db.SetAgentGroupSandboxProfile(g.Name, ""); err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"group": g.Name, "name": ""})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET, PUT or DELETE")
	}
}

func handleSandboxProfilesExport(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermSandboxProfilesManage); !ok {
		return
	}
	names := requestedProfileExportNames(r)
	out := []sandboxProfileJSON{}
	exportedNames := map[string]bool{}
	if len(names) == 0 {
		profiles, err := db.ListSandboxProfiles()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		for _, p := range profiles {
			out = append(out, sandboxProfileToJSON(p, false))
			exportedNames[p.Name] = true
		}
	} else {
		// A named export follows includes transitively so the bundle stays
		// self-contained: import validates the reference graph and would
		// reject a profile whose include is neither local nor in the bundle.
		requested := map[string]bool{}
		for _, name := range names {
			requested[name] = true
		}
		pending := append([]string{}, names...)
		for i := 0; i < len(pending); i++ {
			name := pending[i]
			if exportedNames[name] {
				continue
			}
			p, err := db.GetSandboxProfile(name)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "io", err.Error())
				return
			}
			if p == nil {
				kind := "sandbox profile"
				if !requested[name] {
					kind = "included sandbox profile"
				}
				writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no such %s %q", kind, name))
				return
			}
			out = append(out, sandboxProfileToJSON(p, false))
			exportedNames[p.Name] = true
			pending = append(pending, p.Includes...)
		}
	}
	var assignments *sandboxProfileAssignmentsJSON
	if strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_assignments")), "true") {
		var err error
		assignments, err = collectSandboxProfileAssignments(exportedNames)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, sandboxProfileExportEnvelope{
		Format: sandboxProfileExportFormat, FormatVersion: sandboxProfileExportVersion,
		ExportedAt: time.Now().UTC().Format(time.RFC3339), Profiles: out, Assignments: assignments,
	})
}

func collectSandboxProfileAssignments(exportedNames map[string]bool) (*sandboxProfileAssignmentsJSON, error) {
	out := &sandboxProfileAssignmentsJSON{Groups: map[string]string{}}
	global, err := db.GetGlobalSandboxProfile()
	if err != nil {
		return nil, err
	}
	if global != nil && exportedNames[global.Name] {
		out.Global = global.Name
	}
	groups, err := db.ListAgentGroups()
	if err != nil {
		return nil, err
	}
	for _, group := range groups {
		profile, err := db.GetAgentGroupSandboxProfile(group.Name)
		if err != nil {
			return nil, err
		}
		if profile != nil && exportedNames[profile.Name] {
			out.Groups[group.Name] = profile.Name
		}
	}
	return out, nil
}

func handleSandboxProfilesImport(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermSandboxProfilesManage); !ok {
		return
	}
	var env sandboxProfileExportEnvelope
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "not valid sandbox-profile JSON: "+err.Error())
		return
	}
	if env.Format != sandboxProfileExportFormat || env.FormatVersion != sandboxProfileExportVersion {
		writeError(w, http.StatusBadRequest, "invalid_format", fmt.Sprintf(
			"unsupported sandbox-profile export %q version %d", env.Format, env.FormatVersion))
		return
	}
	conflict := strings.ToLower(strings.TrimSpace(env.OnConflict))
	if conflict == "" {
		conflict = "error"
	}
	if conflict != "error" && conflict != "skip" && conflict != "overwrite" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "on_conflict must be error, skip, or overwrite")
		return
	}
	profiles := make([]*db.SandboxProfile, 0, len(env.Profiles))
	for i, body := range env.Profiles {
		p, _, err := buildSandboxProfileForImport(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", fmt.Sprintf("profile #%d: %v", i+1, err))
			return
		}
		profiles = append(profiles, p)
	}
	var assignments *db.SandboxProfileAssignments
	if env.ApplyAssignments && env.Assignments != nil {
		assignments = &db.SandboxProfileAssignments{Global: env.Assignments.Global, Groups: env.Assignments.Groups}
	}
	result, err := db.ImportSandboxProfiles(profiles, conflict, assignments)
	if errors.Is(err, db.ErrSandboxProfileNameTaken) {
		writeError(w, http.StatusConflict, "exists", err.Error())
		return
	}
	if errors.Is(err, db.ErrSandboxProfileInvalidImport) {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"imported": result.Imported, "skipped": result.Skipped, "warnings": result.Warnings})
}

type sandboxProfileImportPathWarning struct {
	Profile string `json:"profile"`
	Path    string `json:"path"`
	Message string `json:"message"`
}

// handleSandboxProfilesImportInspect validates and normalizes a portable
// bundle without writing it. Unlike ordinary profile creation, portability
// validation retains missing local paths and reports them as warnings so the
// dashboard can show an actionable preview.
func handleSandboxProfilesImportInspect(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermSandboxProfilesManage); !ok {
		return
	}
	var env sandboxProfileExportEnvelope
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "not valid sandbox-profile JSON: "+err.Error())
		return
	}
	if env.Format != sandboxProfileExportFormat || env.FormatVersion != sandboxProfileExportVersion {
		writeError(w, http.StatusBadRequest, "invalid_format", fmt.Sprintf(
			"unsupported sandbox-profile export %q version %d", env.Format, env.FormatVersion))
		return
	}
	profiles := make([]sandboxProfileJSON, 0, len(env.Profiles))
	built := make([]*db.SandboxProfile, 0, len(env.Profiles))
	warnings := []sandboxProfileImportPathWarning{}
	seen := map[string]bool{}
	for i, body := range env.Profiles {
		profile, missing, err := buildSandboxProfileForImport(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", fmt.Sprintf("profile #%d: %v", i+1, err))
			return
		}
		if seen[profile.Name] {
			writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", fmt.Sprintf("sandbox profile %q appears more than once", profile.Name))
			return
		}
		seen[profile.Name] = true
		profiles = append(profiles, sandboxProfileToJSON(profile, false))
		built = append(built, profile)
		for _, path := range missing {
			warnings = append(warnings, sandboxProfileImportPathWarning{
				Profile: profile.Name,
				Path:    path,
				Message: "path does not exist locally; the rule will target it if created",
			})
		}
	}
	// The preview gates the dashboard's Import button, so include-graph
	// problems the import would reject must surface here too — not after the
	// user has already confirmed a "valid" preview. The graph shape depends on
	// the conflict policy ("skip" keeps a clashing local profile's own
	// includes), and the policy is picked on the preview screen, so the
	// response carries per-policy errors for the client to gate the selector
	// with. Only a bundle invalid under every policy is rejected outright.
	inspection, err := db.InspectSandboxProfileImportGraph(built)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if inspection.OverwriteError != "" && inspection.SkipError != "" {
		writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", inspection.OverwriteError)
		return
	}
	includeErrors := map[string]string{}
	if inspection.OverwriteError != "" {
		includeErrors["overwrite"] = inspection.OverwriteError
	}
	if inspection.SkipError != "" {
		includeErrors["skip"] = inspection.SkipError
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": profiles, "warnings": warnings, "include_errors": includeErrors})
}
