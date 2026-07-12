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

type sandboxProfileJSON struct {
	Name        string                           `json:"name"`
	Filesystem  []sandboxpolicy.FilesystemGrant  `json:"filesystem"`
	Environment []sandboxpolicy.EnvironmentEntry `json:"environment"`
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
		Name: p.Name, Filesystem: p.Filesystem, Environment: p.Environment,
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

func buildSandboxProfile(body sandboxProfileJSON) (*db.SandboxProfile, error) {
	normalized, err := sandboxpolicy.Normalize(sandboxpolicy.Profile{
		Name: body.Name, Filesystem: body.Filesystem, Environment: body.Environment,
	})
	if err != nil {
		return nil, err
	}
	return &db.SandboxProfile{
		Name: normalized.Name, Filesystem: normalized.Filesystem, Environment: normalized.Environment,
	}, nil
}

func buildSandboxProfileForImport(body sandboxProfileJSON) (*db.SandboxProfile, []string, error) {
	normalized, missing, err := sandboxpolicy.NormalizeForImport(sandboxpolicy.Profile{
		Name: body.Name, Filesystem: body.Filesystem, Environment: body.Environment,
	})
	if err != nil {
		return nil, nil, err
	}
	return &db.SandboxProfile{
		Name: normalized.Name, Filesystem: normalized.Filesystem, Environment: normalized.Environment,
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
		p, err := buildSandboxProfile(body)
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
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": p.Name})
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
		p, err := buildSandboxProfile(body)
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
		} else if errors.Is(updateErr, db.ErrSandboxProfileChanged) {
			writeError(w, http.StatusConflict, "changed", "sandbox profile changed since preview; reopen it and review the latest changes")
			return
		} else if updateErr != nil {
			writeError(w, http.StatusInternalServerError, "io", updateErr.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": p.ID, "name": p.Name})
	case http.MethodDelete:
		n, err := db.DeleteSandboxProfile(name)
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
		for _, name := range names {
			p, err := db.GetSandboxProfile(name)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "io", err.Error())
				return
			}
			if p == nil {
				writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no such sandbox profile %q", name))
				return
			}
			out = append(out, sandboxProfileToJSON(p, false))
			exportedNames[p.Name] = true
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
	warnings := []sandboxProfileImportPathWarning{}
	for i, body := range env.Profiles {
		profile, missing, err := buildSandboxProfileForImport(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", fmt.Sprintf("profile #%d: %v", i+1, err))
			return
		}
		profiles = append(profiles, sandboxProfileToJSON(profile, false))
		for _, path := range missing {
			warnings = append(warnings, sandboxProfileImportPathWarning{
				Profile: profile.Name,
				Path:    path,
				Message: "path does not exist locally; edit the profile before use",
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": profiles, "warnings": warnings})
}
