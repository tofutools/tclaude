package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// sandboxCommonRuleCatalogJSON carries both parts of the filesystem editor's
// explanatory feed: audited deny presets the dashboard can insert as ORDINARY
// rows, and immutable harness-global rows it renders only as launch context.
// Nothing from GlobalFilesystem is accepted by profile write routes. The
// preset field remains named "categories" so an older dashboard build keeps
// rendering that portion of the feed.
type sandboxCommonRuleCatalogJSON struct {
	Version           int                               `json:"version"`
	Platform          string                            `json:"platform"`
	Home              string                            `json:"home"`
	Categories        []sandboxpolicy.CommonRule        `json:"categories"`
	Informational     []map[string]any                  `json:"informational"`
	GlobalFilesystem  []sandboxGlobalFilesystemRuleJSON `json:"global_filesystem"`
	GlobalNetwork     []sandboxGlobalAccessRuleJSON     `json:"global_network"`
	GlobalUnixSockets []sandboxGlobalAccessRuleJSON     `json:"global_unix_sockets"`
	NetworkPacks      []sandboxAccessTemplateJSON       `json:"network_packs"`
	// NetworkTemplates is a compatibility alias for older dashboards. New
	// editors persist pack IDs from NetworkPacks instead of inserting rows.
	NetworkTemplates     []sandboxAccessTemplateJSON `json:"network_templates"`
	SocketTemplates      []sandboxAccessTemplateJSON `json:"socket_templates"`
	GlobalConfigWarnings []string                    `json:"global_config_warnings"`
}

func handleSandboxCommonRuleCatalog(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermSandboxProfilesManage); !ok {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "resolve home directory")
		return
	}
	home, err = sandboxpolicy.CanonicalCommonRuleHome(home)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	rules, err := sandboxpolicy.CommonRuleCatalog(home, runtime.GOOS)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	global := sandboxGlobalFilesystemRules(home)
	globalAccess := sandboxGlobalAccessRules(home)
	writeJSON(w, http.StatusOK, sandboxCommonRuleCatalogJSON{
		Version:              sandboxpolicy.CommonRuleCatalogVersion,
		Platform:             runtime.GOOS,
		Home:                 home,
		Categories:           rules,
		GlobalFilesystem:     global.Filesystem,
		GlobalNetwork:        globalAccess.Network,
		GlobalUnixSockets:    globalAccess.UnixSockets,
		NetworkPacks:         sandboxNetworkPacks(),
		NetworkTemplates:     sandboxNetworkTemplates(),
		SocketTemplates:      sandboxSocketTemplates(),
		GlobalConfigWarnings: append(global.Warnings, globalAccess.Warnings...),
		Informational: []map[string]any{
			{"id": "system.runtime", "label": "System runtime roots", "removable": false, "description": "Execution, DNS, and TLS runtime roots remain available and are not affected by these rules."},
			{"id": "workspace.mechanics", "label": "Workspace and Git mechanics", "removable": false, "description": "The active workspace and exact verified Git common/admin paths are reopened when a deny covers them. The broader repository container is not reopened, so direct creation of sibling worktrees is unavailable under a home-wide deny; create/broker the worktree before launch."},
			{"id": "agentd.control-plane", "label": "tclaude control plane", "removable": false, "description": "Only the agentd socket/control plane is reopened; private ~/.tclaude/data remains denied."},
		},
	})
}

// sandboxProfileMaxBodyBytes bounds a profile document. The PATCH handler reads
// the body whole so it can tell an absent pre_launch from an explicit empty
// one, and an unbounded ReadAll would hold whatever was sent. Generous: the
// largest real profile is orders of magnitude under it.
const sandboxProfileMaxBodyBytes = 8 << 20

const (
	sandboxProfileExportFormat        = "tclaude-sandbox-profiles"
	sandboxProfileExportVersion       = 16
	sandboxProfileExportVersionLegacy = 9
)

// sandboxProfileBeforeMkdir is a test seam for exercising substitutions in
// the narrow window between portable validation and descriptor-relative
// creation. Production leaves it as a no-op.
var sandboxProfileBeforeMkdir = func(string) {}

type sandboxProfileJSON struct {
	ID                      int64                              `json:"id,omitempty"`
	Name                    string                             `json:"name"`
	Filesystem              []sandboxpolicy.FilesystemGrant    `json:"filesystem"`
	FilesystemSpellings     *sandboxpolicy.FilesystemSpellings `json:"filesystem_spellings"`
	Environment             []sandboxpolicy.EnvironmentEntry   `json:"environment"`
	AgentDirectories        []string                           `json:"agent_directories,omitempty"`
	FilesystemRoot          sandboxpolicy.FilesystemRootMode   `json:"filesystem_root,omitempty"`
	HarnessConfig           sandboxpolicy.HarnessConfigAccess  `json:"harness_config,omitempty"`
	NetworkAccess           sandboxpolicy.NetworkAccess        `json:"network_access,omitempty"`
	Network                 *sandboxpolicy.NetworkRules        `json:"network,omitempty"`
	UnixSockets             *sandboxpolicy.UnixSocketRules     `json:"unix_sockets,omitempty"`
	ResourceLimits          sandboxpolicy.ResourceLimits       `json:"resource_limits,omitempty"`
	DarwinAllowMachRegister bool                               `json:"darwin_allow_mach_register,omitempty"`
	PreLaunch               []sandboxpolicy.PreLaunchBlock     `json:"pre_launch,omitempty"`
	Includes                []string                           `json:"includes,omitempty"`
	CreatedAt               string                             `json:"created_at,omitempty"`
	UpdatedAt               string                             `json:"updated_at,omitempty"`
	// Tombstones. TCL-791 removed break-glass; these fields exist ONLY so a
	// payload still carrying them is refused loudly rather than silently
	// dropped as an unknown JSON key. Detection is on the RAW JSON, so it works
	// whatever shape the caller sent. They are never stored and never emitted.
	BreakGlassFilesystem   json.RawMessage `json:"break_glass_filesystem,omitempty"`
	BreakGlassAcknowledged *bool           `json:"break_glass_acknowledged,omitempty"`
}

type sandboxProfileExportEnvelope struct {
	Format           string                         `json:"format"`
	FormatVersion    int                            `json:"format_version"`
	ExportedAt       string                         `json:"exported_at,omitempty"`
	Profiles         []sandboxProfileJSON           `json:"profiles"`
	Assignments      *sandboxProfileAssignmentsJSON `json:"assignments,omitempty"`
	OnConflict       string                         `json:"on_conflict,omitempty"`       // import only: error|skip|overwrite
	ApplyAssignments bool                           `json:"apply_assignments,omitempty"` // import only; explicit to avoid cross-machine surprises
	// Tombstone: see sandboxProfileJSON. An envelope-level acknowledgement is
	// refused loudly rather than ignored.
	BreakGlassAcknowledged *bool `json:"break_glass_acknowledged,omitempty"`
}

type sandboxProfileAssignmentsJSON struct {
	Global string            `json:"global,omitempty"`
	Groups map[string]string `json:"groups,omitempty"`
}

type sandboxProfilePreviewJSON struct {
	Before        *sandboxProfileJSON          `json:"before,omitempty"`
	After         sandboxProfileJSON           `json:"after"`
	AccessNotices []sandboxpolicy.AccessNotice `json:"notices,omitempty"`
	// Revision couples an edit preview to its eventual PATCH. It is omitted for
	// creates, whose unique-name constraint already protects the commit.
	Revision string `json:"revision,omitempty"`
}

func sandboxProfileToJSON(p *db.SandboxProfile, localFields bool) sandboxProfileJSON {
	out := sandboxProfileJSON{
		Name: p.Name, Filesystem: p.Filesystem,
		FilesystemSpellings: p.FilesystemSpellings,
		Environment:         p.Environment, AgentDirectories: p.AgentDirectories,
		FilesystemRoot: p.FilesystemRoot,
		HarnessConfig:  p.HarnessConfig,
		NetworkAccess:  sandboxpolicy.LegacyNetworkAccessForExport(p.Network, p.NetworkAccess),
		Network:        p.Network, UnixSockets: p.UnixSockets, ResourceLimits: p.ResourceLimits,
		DarwinAllowMachRegister: p.DarwinAllowMachRegister, PreLaunch: p.PreLaunch,
		Includes: p.Includes,
	}
	if localFields {
		out.ID = p.ID
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
	input := sandboxpolicy.Profile{
		Name: body.Name, Filesystem: body.Filesystem,
		FilesystemSpellings: body.FilesystemSpellings,
		Environment:         body.Environment, AgentDirectories: body.AgentDirectories, FilesystemRoot: body.FilesystemRoot,
		HarnessConfig: body.HarnessConfig, NetworkAccess: body.NetworkAccess,
		Network: body.Network, UnixSockets: body.UnixSockets, ResourceLimits: body.ResourceLimits,
		DarwinAllowMachRegister: body.DarwinAllowMachRegister, PreLaunch: body.PreLaunch,
		Includes: body.Includes,
	}
	var normalized sandboxpolicy.Profile
	var missing []string
	var err error
	if body.FilesystemSpellings == nil {
		normalized, missing, err = sandboxpolicy.NormalizeForAuthoring(input)
	} else {
		normalized, missing, err = sandboxpolicy.NormalizeForPersistence(input)
	}
	if err != nil {
		return nil, nil, err
	}
	return &db.SandboxProfile{
		Name: normalized.Name, Filesystem: normalized.Filesystem,
		FilesystemSpellings: normalized.FilesystemSpellings,
		Environment:         normalized.Environment, AgentDirectories: normalized.AgentDirectories,
		FilesystemRoot: normalized.FilesystemRoot,
		HarnessConfig:  normalized.HarnessConfig,
		NetworkAccess:  normalized.NetworkAccess, Network: normalized.Network,
		UnixSockets: normalized.UnixSockets, ResourceLimits: normalized.ResourceLimits,
		DarwinAllowMachRegister: normalized.DarwinAllowMachRegister, PreLaunch: normalized.PreLaunch,
		Includes: normalized.Includes,
	}, missing, nil
}

func buildSandboxProfileForImport(body sandboxProfileJSON) (*db.SandboxProfile, []string, error) {
	normalized, missing, err := sandboxpolicy.NormalizeForImport(sandboxpolicy.Profile{
		Name: body.Name, Filesystem: body.Filesystem,
		FilesystemSpellings: body.FilesystemSpellings,
		Environment:         body.Environment, AgentDirectories: body.AgentDirectories, FilesystemRoot: body.FilesystemRoot,
		HarnessConfig: body.HarnessConfig, NetworkAccess: body.NetworkAccess,
		Network: body.Network, UnixSockets: body.UnixSockets, ResourceLimits: body.ResourceLimits,
		DarwinAllowMachRegister: body.DarwinAllowMachRegister, PreLaunch: body.PreLaunch,
		Includes: body.Includes,
	})
	if err != nil {
		return nil, nil, err
	}
	return &db.SandboxProfile{
		Name: normalized.Name, Filesystem: normalized.Filesystem,
		FilesystemSpellings: normalized.FilesystemSpellings,
		Environment:         normalized.Environment, AgentDirectories: normalized.AgentDirectories,
		FilesystemRoot: normalized.FilesystemRoot,
		HarnessConfig:  normalized.HarnessConfig,
		NetworkAccess:  normalized.NetworkAccess, Network: normalized.Network,
		UnixSockets: normalized.UnixSockets, ResourceLimits: normalized.ResourceLimits,
		DarwinAllowMachRegister: normalized.DarwinAllowMachRegister, PreLaunch: normalized.PreLaunch,
		Includes: normalized.Includes,
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
		// Gated before the dry-run branch on purpose: a preview that quietly
		// dropped the field would render a profile the real save refuses,
		// which is the same silent divergence the gate exists to prevent.
		if fail := rejectBreakGlassPayload("save", body); fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
		accessNotices, err := db.SandboxProfileCompositionNotices(p)
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
				After: sandboxProfileToJSON(p, false), AccessNotices: accessNotices,
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
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": id, "name": p.Name, "missing": missing, "notices": accessNotices,
		})
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
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, sandboxProfileMaxBodyBytes))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		var body sandboxProfileJSON
		if err := json.Unmarshal(raw, &body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		// A PATCH that never mentions pre_launch must not delete it. Clients
		// build their save payload from a field whitelist — the dashboard does
		// — so a field the editor does not know about is simply absent, and
		// treating absent as "clear" would destroy an operator's setup script
		// when they merely renamed a profile. Probe the raw JSON: an absent key
		// and an explicit `[]` both decode to a nil slice, but only the second
		// one means "remove them".
		// Errors are impossible here and deliberately ignored: any input this
		// could reject was already rejected by the struct decode above.
		var present map[string]json.RawMessage
		_ = json.Unmarshal(raw, &present)
		if _, sent := present["pre_launch"]; !sent {
			body.PreLaunch = existing.PreLaunch
		}
		p, missing, err := buildSandboxProfile(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", err.Error())
			return
		}
		p.ID = existing.ID
		// See the create handler: the preview is gated too, so it can never
		// show an edit the save will refuse.
		if fail := rejectBreakGlassPayload("save", body); fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
		accessNotices, err := db.SandboxProfileCompositionNotices(p)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", err.Error())
			return
		}
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
				Before:        &before,
				After:         sandboxProfileToJSON(p, false),
				AccessNotices: accessNotices,
				Revision:      existing.UpdatedAt.Format(time.RFC3339Nano),
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
		writeJSON(w, http.StatusOK, map[string]any{
			"id": p.ID, "name": p.Name, "missing": missing, "notices": accessNotices,
		})
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
	// Gated like the save paths. This endpoint answers "which of these
	// directories are missing, and may I create them?", so a payload still
	// carrying break_glass_filesystem would otherwise get an answer computed
	// from a silently narrowed profile — and, on the create action, actually
	// mkdir for it. The refusal has to come before any of that.
	if fail := rejectBreakGlassPayload("inspect directories for", body); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
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
			assignmentBreakGlassBody
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
		if fail := rejectBreakGlassAssignment("global", body.BreakGlassAcknowledged); fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
		accessNotices, err := globalSandboxAssignmentCompositionNotices(body.Name)
		if errors.Is(err, db.ErrSandboxProfileNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if err := db.SetGlobalSandboxProfile(body.Name); errors.Is(err, db.ErrSandboxProfileNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"name": body.Name, "notices": accessNotices})
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
			assignmentBreakGlassBody
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
		if fail := rejectBreakGlassAssignment(fmt.Sprintf("group %q", g.Name), body.BreakGlassAcknowledged); fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
		accessNotices, err := groupSandboxAssignmentCompositionNotices(g.Name, body.Name)
		if errors.Is(err, db.ErrSandboxProfileNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if _, err := db.SetAgentGroupSandboxProfile(g.Name, body.Name); errors.Is(err, db.ErrSandboxProfileNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"group": g.Name, "name": body.Name, "notices": accessNotices})
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
	formatVersion := sandboxProfileExportVersionLegacy
	for _, profile := range out {
		if profile.Network != nil && profile.Network.PreserveCallerIdentity {
			formatVersion = 16
			break
		}
		if profile.HarnessConfig != "" {
			formatVersion = 15
			break
		}
		if profile.FilesystemRoot != "" {
			if formatVersion < 14 {
				formatVersion = 14
			}
			continue
		}
		if profile.Network != nil && profile.Network.Namespace != "" {
			if formatVersion < 13 {
				formatVersion = 13
			}
		}
		if profile.DarwinAllowMachRegister {
			if formatVersion < 12 {
				formatVersion = 12
			}
		}
		if profile.ResourceLimits.Enabled() {
			if formatVersion < 11 {
				formatVersion = 11
			}
		}
		if profile.Network != nil &&
			(len(profile.Network.DenyPacks) > 0 || len(profile.Network.Deny) > 0) &&
			formatVersion < 10 {
			formatVersion = 10
		}
	}
	writeJSON(w, http.StatusOK, sandboxProfileExportEnvelope{
		Format: sandboxProfileExportFormat, FormatVersion: formatVersion,
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
	if !supportedSandboxProfileExport(env.Format, env.FormatVersion) {
		writeError(w, http.StatusBadRequest, "invalid_format", fmt.Sprintf(
			"unsupported sandbox-profile export %q version %d", env.Format, env.FormatVersion))
		return
	}
	if fail := validateSandboxProfileExportVersionContent(env); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	if fail := rejectBreakGlassEnvelope(env); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
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
	sandboxImportBeforeTransactionForTest()
	result, err := db.ImportSandboxProfilesWithOptions(profiles, db.SandboxProfileImportOptions{
		OnConflict: conflict, Assignments: assignments,
	})
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
	for _, notice := range result.AccessNotices {
		result.Warnings = append(result.Warnings, notice.Detail)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"imported": result.Imported, "skipped": result.Skipped,
		"warnings": result.Warnings,
	})
}

var sandboxImportBeforeTransactionForTest = func() {}

// SetSandboxImportBeforeTransactionForTest installs the deterministic seam
// immediately before the authoritative DB transaction.
func SetSandboxImportBeforeTransactionForTest(fn func()) func() {
	previous := sandboxImportBeforeTransactionForTest
	if fn == nil {
		fn = func() {}
	}
	sandboxImportBeforeTransactionForTest = fn
	return func() { sandboxImportBeforeTransactionForTest = previous }
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
	if !supportedSandboxProfileExport(env.Format, env.FormatVersion) {
		writeError(w, http.StatusBadRequest, "invalid_format", fmt.Sprintf(
			"unsupported sandbox-profile export %q version %d", env.Format, env.FormatVersion))
		return
	}
	if fail := validateSandboxProfileExportVersionContent(env); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	if fail := rejectBreakGlassEnvelope(env); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
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
	writeJSON(w, http.StatusOK, map[string]any{
		"profiles": profiles, "warnings": warnings, "include_errors": includeErrors,
	})
}

// Version 2 adds network_access; version 3 adds read_baseline and
// break_glass_filesystem; version 4 adds semantic read_baseline_exclusions;
// version 5 REMOVES read_baseline and read_baseline_exclusions (TCL-623) —
// strictness is expressed as ordinary filesystem deny rows plus narrower
// reopens; version 6 REMOVES break_glass_filesystem (TCL-791).
// Version 7 adds network and unix_sockets; absent fields mean exactly what
// their profile meant before those axes existed. Version 8 adds the versioned
// filesystem_spellings sidecar; null means legacy spelling behavior, while a
// non-null empty document marks a modern profile with no alternate spellings.
// Version 9 adds the compositional network baseline and release-owned pack
// references. Legacy mode-based network rules remain readable.
// Version 10 adds deny-mode network entries and pack references. Exports remain
// v9 when every profile is deny-free so older releases can still import them;
// any deny state requires v10 because an older importer ignoring it would
// silently widen the authored policy.
// Version 11 adds resource limits, version 12 adds Darwin Mach-registration
// access, and version 13 adds the network namespace selection. A v13 field
// cannot be sent under an older envelope because ignoring `private` would
// silently replace its network boundary with the shared host namespace.
// Version 14 adds the explicit filesystem-root posture. Omitting it preserves
// the historical automatic derivation. Version 15 adds harness-config access,
// and version 16 adds the filtered Linux caller-identity opt-in.
//
// Older versions stay readable so imports from older installations keep
// working. The two removals are handled DIFFERENTLY on purpose. The retired
// read-baseline fields decode into nothing and are dropped: they expressed a
// restriction, so losing them is visible in the resulting profile and cannot
// widen access. break_glass_filesystem is a GRANT of protected access, and a
// bundle is operator input — silently importing a profile that is not what the
// file says would be exactly the failure this removal exists to prevent — so a
// v3/v4/v5 bundle still carrying it is REFUSED, naming the offending profiles,
// rather than quietly stripped. Such a bundle is importable after the operator
// edits the field out. Bundles without it import unchanged at every version.
func supportedSandboxProfileExport(format string, version int) bool {
	// Spelled out rather than derived from sandboxProfileExportVersion: every
	// past version must stay listed when the constant is bumped, and writing
	// `version == sandboxProfileExportVersion` would silently drop the previous
	// one each time.
	return format == sandboxProfileExportFormat &&
		version >= 1 && version <= sandboxProfileExportVersion
}

func validateSandboxProfileExportVersionContent(env sandboxProfileExportEnvelope) *spawnFailure {
	for _, profile := range env.Profiles {
		if env.FormatVersion < 16 && profile.Network != nil &&
			profile.Network.PreserveCallerIdentity {
			return &spawnFailure{
				Status: http.StatusBadRequest,
				Kind:   "invalid_format",
				Msg: fmt.Sprintf(
					"sandbox profile %q preserves filtered Linux caller identity, which requires export format version 16",
					profile.Name),
			}
		}
		if env.FormatVersion < 15 && profile.HarnessConfig != "" {
			return &spawnFailure{
				Status: http.StatusBadRequest,
				Kind:   "invalid_format",
				Msg: fmt.Sprintf(
					"sandbox profile %q contains a harness config access selection, which requires export format version 15",
					profile.Name),
			}
		}
		if env.FormatVersion < 14 && profile.FilesystemRoot != "" {
			return &spawnFailure{
				Status: http.StatusBadRequest,
				Kind:   "invalid_format",
				Msg: fmt.Sprintf(
					"sandbox profile %q contains a filesystem root selection, which requires export format version 14",
					profile.Name),
			}
		}
		if env.FormatVersion < 13 && profile.Network != nil &&
			profile.Network.Namespace != "" {
			return &spawnFailure{
				Status: http.StatusBadRequest,
				Kind:   "invalid_format",
				Msg: fmt.Sprintf(
					"sandbox profile %q contains a network namespace selection, which requires export format version 13",
					profile.Name),
			}
		}
		if env.FormatVersion < 12 && profile.DarwinAllowMachRegister {
			return &spawnFailure{
				Status: http.StatusBadRequest,
				Kind:   "invalid_format",
				Msg: fmt.Sprintf(
					"sandbox profile %q contains Darwin mach-register access, which requires export format version 12",
					profile.Name,
				),
			}
		}
		if env.FormatVersion < 11 && profile.ResourceLimits.Enabled() {
			return &spawnFailure{
				Status: http.StatusBadRequest,
				Kind:   "invalid_format",
				Msg: fmt.Sprintf(
					"sandbox profile %q contains resource limits, which require export format version 11",
					profile.Name,
				),
			}
		}
		if env.FormatVersion >= 10 {
			continue
		}
		if profile.Network == nil ||
			(len(profile.Network.DenyPacks) == 0 && len(profile.Network.Deny) == 0) {
			continue
		}
		return &spawnFailure{
			Status: http.StatusBadRequest,
			Kind:   "invalid_format",
			Msg: fmt.Sprintf(
				"sandbox profile %q contains network deny state, which requires export format version 10",
				profile.Name,
			),
		}
	}
	return nil
}
