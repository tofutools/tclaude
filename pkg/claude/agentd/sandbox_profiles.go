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

// handleSandboxProfiles exposes the profile registry. Reads are open because
// profile environment is explicitly ordinary non-secret configuration. Writes
// require sandbox-profiles.manage, a stronger and separate authority from
// editing spawn-dialog presets.
func handleSandboxProfiles(w http.ResponseWriter, r *http.Request) {
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
		if _, ok := requirePermission(w, r, PermSandboxProfilesManage); !ok {
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
		if _, ok := requirePermission(w, r, PermSandboxProfilesManage); !ok {
			return
		}
		existing, err := db.GetSandboxProfile(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if existing == nil {
			writeError(w, http.StatusNotFound, "not_found", "no such sandbox profile")
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
		if err := db.UpdateSandboxProfile(p); errors.Is(err, db.ErrSandboxProfileNameTaken) {
			writeError(w, http.StatusConflict, "exists", err.Error())
			return
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": p.ID, "name": p.Name})
	case http.MethodDelete:
		if _, ok := requirePermission(w, r, PermSandboxProfilesManage); !ok {
			return
		}
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
		if _, err := db.SetAgentGroupSandboxProfile(g.Name, body.Name); errors.Is(err, db.ErrSandboxProfileNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"group": g.Name, "name": body.Name})
	case http.MethodDelete:
		if _, ok := requirePermission(w, r, PermSandboxProfilesManage); !ok {
			return
		}
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
	type plan struct {
		profile  *db.SandboxProfile
		existing *db.SandboxProfile
	}
	plans := make([]plan, 0, len(env.Profiles))
	seen := map[string]bool{}
	for i, body := range env.Profiles {
		p, err := buildSandboxProfile(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", fmt.Sprintf("profile #%d: %v", i+1, err))
			return
		}
		if seen[p.Name] {
			writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("sandbox profile %q appears more than once", p.Name))
			return
		}
		seen[p.Name] = true
		existing, err := db.GetSandboxProfile(p.Name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if existing != nil && conflict == "error" {
			writeError(w, http.StatusConflict, "exists", fmt.Sprintf("sandbox profile %q already exists", p.Name))
			return
		}
		plans = append(plans, plan{profile: p, existing: existing})
	}
	imported, skipped, warnings := []string{}, []string{}, []string{}
	for _, item := range plans {
		if item.existing != nil && conflict == "skip" {
			skipped = append(skipped, item.profile.Name)
			continue
		}
		if item.existing != nil {
			item.profile.ID = item.existing.ID
			if err := db.UpdateSandboxProfile(item.profile); err != nil {
				writeError(w, http.StatusInternalServerError, "io", err.Error())
				return
			}
		} else if _, err := db.CreateSandboxProfile(item.profile); err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		imported = append(imported, item.profile.Name)
	}
	if env.ApplyAssignments && env.Assignments != nil {
		if env.Assignments.Global != "" {
			if err := db.SetGlobalSandboxProfile(env.Assignments.Global); errors.Is(err, db.ErrSandboxProfileNotFound) {
				warnings = append(warnings, fmt.Sprintf("global assignment references missing sandbox profile %q", env.Assignments.Global))
			} else if err != nil {
				writeError(w, http.StatusInternalServerError, "io", err.Error())
				return
			}
		}
		for group, profile := range env.Assignments.Groups {
			if existingGroup, err := db.GetAgentGroupByName(group); err != nil {
				writeError(w, http.StatusInternalServerError, "io", err.Error())
				return
			} else if existingGroup == nil {
				warnings = append(warnings, fmt.Sprintf("group assignment skipped: no group %q", group))
				continue
			}
			if _, err := db.SetAgentGroupSandboxProfile(group, profile); errors.Is(err, db.ErrSandboxProfileNotFound) {
				warnings = append(warnings, fmt.Sprintf("group %q assignment references missing sandbox profile %q", group, profile))
			} else if err != nil {
				writeError(w, http.StatusInternalServerError, "io", err.Error())
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"imported": imported, "skipped": skipped, "warnings": warnings})
}
