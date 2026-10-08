package agentd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

const PermConfigExport = "config.export"
const PermConfigImport = "config.import"

// Safe config keys are an allowlist, so a future credential-bearing setting
// cannot accidentally start travelling. Agent defaults have their own section.
var bundleConfigKeys = []string{"log_level", "startup_timing", "record_hooks", "terminal", "log_rotation", "focus", "slop", "conv_watch", "session_watch", "cost", "ask", "audit", "routes", "claude_resume", "claude_cleanup_period_days", "dashboard", "tui", "usage"}
var bundleImportMu sync.Mutex

func bundleRaw(v any) (json.RawMessage, error) { return json.Marshal(v) }
func bundleAdd(b *configbundle.Bundle, section, name string, v any) error {
	raw, err := bundleRaw(v)
	if err != nil {
		return err
	}
	b.Sections[section] = append(b.Sections[section], configbundle.Item{Name: name, Value: raw})
	return nil
}

func collectConfigBundle(r *http.Request) (*configbundle.Bundle, error) {
	b := &configbundle.Bundle{Format: configbundle.Format, FormatVersion: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339), TclaudeVersion: buildversion.AppVersion(), Sections: map[string][]configbundle.Item{}}
	roles, err := db.ListRoles()
	if err != nil {
		return nil, err
	}
	for _, rl := range roles {
		v := roleToJSON(rl)
		v.CreatedAt = ""
		v.UpdatedAt = ""
		if err := bundleAdd(b, "roles", rl.Name, v); err != nil {
			return nil, err
		}
	}
	sandboxes, err := db.ListSandboxProfiles()
	if err != nil {
		return nil, err
	}
	for _, p := range sandboxes {
		env := sandboxProfileExportEnvelope{Format: sandboxProfileExportFormat, FormatVersion: sandboxProfileExportVersion, Profiles: []sandboxProfileJSON{sandboxProfileToJSON(p, false)}}
		if err := bundleAdd(b, "sandbox-profiles", p.Name, env); err != nil {
			return nil, err
		}
	}
	profiles, err := db.ListSpawnProfiles()
	if err != nil {
		return nil, err
	}
	for _, p := range profiles {
		env := profileExportEnvelope{Format: profileExportFormat, FormatVersion: profileExportVersion, Profiles: []spawnProfileJSON{stripProfileExportLocalFields(profileToJSON(p))}}
		if err := bundleAdd(b, "profiles", p.Name, env); err != nil {
			return nil, err
		}
	}
	templates, err := db.ListGroupTemplates()
	if err != nil {
		return nil, err
	}
	for _, t := range templates {
		inner := templateToJSON(t)
		inner.CreatedAt = ""
		inner.UpdatedAt = ""
		// Dependencies are separate selectable sections, never hidden writes from
		// importing a template while skipping its roles or profiles.
		env := templateExportEnvelope{Format: templateExportFormat, FormatVersion: templateExportVersion, Template: inner}
		if err := bundleAdd(b, "templates", t.Name, env); err != nil {
			return nil, err
		}
	}
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		return nil, err
	}
	records, err := fs.ListTemplates(r.Context())
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, record := range records {
		if seen[record.ID] {
			continue
		}
		seen[record.ID] = true
		head, err := fs.GetTemplateHead(r.Context(), record.ID)
		if err != nil {
			return nil, err
		}
		source, err := fs.GetTemplateSource(r.Context(), head.Ref)
		if err != nil {
			return nil, err
		}
		if err := bundleAdd(b, "process-templates", record.ID, map[string]string{"source": string(source)}); err != nil {
			return nil, err
		}
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	perms := []string{}
	if cfg.Agent != nil {
		perms = append(perms, cfg.Agent.DefaultPermissions...)
	}
	if err := bundleAdd(b, "default-permissions", "defaults", perms); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	for _, key := range bundleConfigKeys {
		if value, ok := obj[key]; ok {
			if err := bundleAdd(b, "config", key, value); err != nil {
				return nil, err
			}
		}
	}
	if cfg.Federation != nil {
		if err := bundleAdd(b, "config", "federation.node_labels", cfg.Federation.NodeLabels); err != nil {
			return nil, err
		}
	}
	for section := range b.Sections {
		slices.SortFunc(b.Sections[section], func(a, c configbundle.Item) int { return strings.Compare(a.Name, c.Name) })
	}
	return b, nil
}

type configBundleRequest struct {
	KeepPaths bool                `json:"keep_paths,omitempty"`
	Bundle    configbundle.Bundle `json:"bundle"`
	Only      []string            `json:"only,omitempty"`
	Skip      []string            `json:"skip,omitempty"`
	Values    map[string]string   `json:"values,omitempty"`
	Apply     bool                `json:"apply,omitempty"`
	Replace   bool                `json:"replace,omitempty"`
}
type configBundleChange struct {
	Item     string          `json:"item"`
	Action   string          `json:"action"`
	Security bool            `json:"security,omitempty"`
	Before   json.RawMessage `json:"before,omitempty"`
	After    json.RawMessage `json:"after"`
}
type configBundlePreview struct {
	Changes         []configBundleChange       `json:"changes"`
	Unresolved      []configbundle.Placeholder `json:"unresolved,omitempty"`
	Applied         []string                   `json:"applied"`
	Warnings        []string                   `json:"warnings,omitempty"`
	SecurityChanges int                        `json:"security_changes"`
	sandboxApplied  bool
}

func handleConfigBundleExport(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermConfigExport); !ok {
		return
	}
	b, err := collectConfigBundle(r)
	if err != nil {
		writeError(w, 500, "export", err.Error())
		return
	}
	if err = b.Select(r.URL.Query()["only"], r.URL.Query()["skip"]); err != nil {
		writeError(w, 400, "selector", err.Error())
		return
	}
	if err = b.Prepare(); err != nil {
		writeError(w, 500, "export", err.Error())
		return
	}
	if len(b.Flags) > 0 && r.URL.Query().Get("allow_flagged") != "true" {
		writeJSON(w, 422, map[string]any{"error": "suspected credentials: exclude flagged items with --skip or explicitly use --allow-flagged", "code": "flagged_credentials", "flags": b.Flags})
		return
	}
	writeJSON(w, 200, b)
}

func handleConfigBundleImport(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermConfigImport); !ok {
		return
	}
	var in configBundleRequest
	// Reserve request overhead for a full 16 MiB bundle plus import options.
	r.Body = http.MaxBytesReader(w, r.Body, 17<<20)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&in); err != nil {
		writeError(w, 400, "json", err.Error())
		return
	}
	if err := in.Bundle.Validate(); err != nil {
		writeError(w, 400, "format", err.Error())
		return
	}
	if err := in.Bundle.Select(in.Only, in.Skip); err != nil {
		writeError(w, 400, "selector", err.Error())
		return
	}
	if in.KeepPaths {
		in.Values = in.Bundle.KeepPaths(in.Values)
	}
	unresolved, err := in.Bundle.Resolve(in.Values)
	if err != nil {
		writeError(w, 400, "values", err.Error())
		return
	}
	// Serialize bundle writers and re-read conflicts at apply time. Component
	// writers retain their own database uniqueness/CAS checks.
	bundleImportMu.Lock()
	defer bundleImportMu.Unlock()
	local, err := collectConfigBundle(r)
	if err != nil {
		writeError(w, 500, "preview", err.Error())
		return
	}
	preview := configBundlePreview{Changes: []configBundleChange{}, Unresolved: unresolved, Applied: []string{}}
	// Never send local secrets in the diff. Before values use the same projection
	// and structured credential omissions as exports.
	original := map[string]json.RawMessage{}
	for section, items := range local.Sections {
		for _, i := range items {
			original[section+"/"+i.Name] = i.Value
		}
	}
	if err := local.Prepare(); err != nil {
		writeError(w, 500, "preview", err.Error())
		return
	}
	var actions []func() error
	conflict := false
	for _, section := range configbundle.Sections {
		for _, item := range in.Bundle.Sections[section] {
			c := configBundleChange{Item: section + "/" + item.Name, Action: "create", After: item.Value, Security: section != "config" && section != "process-templates"}
			for _, old := range local.Sections[section] {
				if old.Name == item.Name {
					c.Before = old.Value
					c.Action = "replace"
					if jsonEqual(original[c.Item], item.Value) {
						c.Action = "unchanged"
					}
					break
				}
			}
			if c.Action == "replace" && !in.Replace {
				conflict = true
			}
			if c.Security && c.Action != "unchanged" {
				preview.SecurityChanges++
			}
			preview.Changes = append(preview.Changes, c)
			if c.Action == "unchanged" {
				continue
			}
			// Bind the plan now. Unresolved items remain visible but cannot apply.
			if len(unresolved) == 0 {
				action, err := planConfigBundleItem(r, section, item, in.Replace, &preview, &in.Bundle)
				if err != nil {
					writeError(w, 400, "invalid_item", c.Item+": "+err.Error())
					return
				}
				actions = append(actions, action)
			}
		}
	}
	if !in.Apply {
		writeJSON(w, 200, preview)
		return
	}
	if len(unresolved) > 0 {
		writeJSON(w, 409, map[string]any{"error": "resolve placeholders with --set name=value before applying", "code": "unresolved_placeholders", "preview": preview})
		return
	}
	if conflict {
		writeJSON(w, 409, map[string]any{"error": "conflicting items require --replace or exclusion with --skip", "code": "conflict", "preview": preview})
		return
	}
	n := 0
	for _, c := range preview.Changes {
		if c.Action == "unchanged" {
			continue
		}
		if err := actions[n](); err != nil {
			writeJSON(w, 409, map[string]any{"error": c.Item + ": " + err.Error(), "code": "apply_failed", "preview": preview})
			return
		}
		n++
		preview.Applied = append(preview.Applied, c.Item)
	}
	writeJSON(w, 200, preview)
}
func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return bytes.Equal(xa, ya)
}
func bundleFailure(f *spawnFailure) error {
	if f == nil {
		return nil
	}
	return errors.New(f.Msg)
}

func planConfigBundleItem(r *http.Request, section string, item configbundle.Item, replace bool, preview *configBundlePreview, selected *configbundle.Bundle) (func() error, error) {
	switch section {
	case "roles":
		var v roleJSON
		if err := json.Unmarshal(item.Value, &v); err != nil {
			return nil, err
		}
		if v.Name != item.Name {
			return nil, errors.New("item name differs from role name")
		}
		p, fail := buildRoleFromJSON(v)
		if fail != nil {
			return nil, bundleFailure(fail)
		}
		return func() error {
			old, err := db.GetRole(p.Name)
			if err != nil {
				return err
			}
			if old != nil {
				if !replace {
					return errors.New("role already exists")
				}
				p.ID = old.ID
				return db.UpdateRole(p)
			}
			_, err = db.CreateRole(p)
			return err
		}, nil
	case "profiles":
		var env profileExportEnvelope
		if err := json.Unmarshal(item.Value, &env); err != nil {
			return nil, err
		}
		if fail := validateProfileEnvelope(env); fail != nil {
			return nil, bundleFailure(fail)
		}
		if len(env.Profiles) != 1 || env.Profiles[0].Name != item.Name {
			return nil, errors.New("expected one matching profile")
		}
		check := env.Profiles[0]
		refs := append([]string{}, check.RoleRefs...)
		if check.RoleRef != "" {
			refs = append(refs, check.RoleRef)
		}
		for _, ref := range refs {
			old, err := db.GetRole(ref)
			if err != nil {
				return nil, err
			}
			if old == nil && !slices.ContainsFunc(selected.Sections["roles"], func(i configbundle.Item) bool { return i.Name == ref }) {
				return nil, fmt.Errorf("missing role %q; include its roles item", ref)
			}
		}
		check.RoleRef = ""
		check.RoleRefs = nil
		if _, fail := buildProfileFromJSON(check); fail != nil {
			return nil, bundleFailure(fail)
		}
		action := "create"
		if replace {
			action = "overwrite"
		}
		return func() error {
			_, fail := importProfiles(env, []profileImportDecision{{Name: item.Name, Action: action}})
			return bundleFailure(fail)
		}, nil
	case "sandbox-profiles":
		var env sandboxProfileExportEnvelope
		if err := json.Unmarshal(item.Value, &env); err != nil {
			return nil, err
		}
		if !supportedSandboxProfileExport(env.Format, env.FormatVersion) {
			return nil, errors.New("unsupported sandbox profile format")
		}
		if fail := validateSandboxProfileExportVersionContent(env); fail != nil {
			return nil, bundleFailure(fail)
		}
		if fail := rejectBreakGlassEnvelope(env); fail != nil {
			return nil, bundleFailure(fail)
		}
		if len(env.Profiles) != 1 || env.Profiles[0].Name != item.Name {
			return nil, errors.New("expected one matching sandbox profile")
		}
		if env.Assignments != nil || env.ApplyAssignments {
			return nil, errors.New("machine-local assignments are not portable")
		}
		_, _, err := buildSandboxProfileForImport(env.Profiles[0])
		if err != nil {
			return nil, err
		}
		conflict := "error"
		if replace {
			conflict = "overwrite"
		}
		return func() error {
			if preview.sandboxApplied {
				return nil
			}
			var profiles []*db.SandboxProfile
			for _, i := range selected.Sections["sandbox-profiles"] {
				unchanged := slices.ContainsFunc(preview.Changes, func(c configBundleChange) bool {
					return c.Item == "sandbox-profiles/"+i.Name && c.Action == "unchanged"
				})
				if unchanged {
					continue
				}
				var e sandboxProfileExportEnvelope
				if err := json.Unmarshal(i.Value, &e); err != nil {
					return err
				}
				p, _, err := buildSandboxProfileForImport(e.Profiles[0])
				if err != nil {
					return err
				}
				profiles = append(profiles, p)
			}
			_, err := db.ImportSandboxProfilesWithOptions(profiles, db.SandboxProfileImportOptions{OnConflict: conflict})
			if err == nil {
				preview.sandboxApplied = true
			}
			return err
		}, nil
	case "templates":
		var env templateExportEnvelope
		if err := json.Unmarshal(item.Value, &env); err != nil {
			return nil, err
		}
		if env.Format != templateExportFormat || env.FormatVersion < 1 || env.FormatVersion > templateExportVersion {
			return nil, errors.New("unsupported template format")
		}
		if env.Template.Name != item.Name {
			return nil, errors.New("item name differs from template name")
		}
		if len(env.Roles) > 0 || len(env.Profiles) > 0 {
			return nil, errors.New("bundle dependencies must use separate roles/profiles sections")
		}
		check := env.Template
		check.Agents = append([]templateAgentJSON{}, env.Template.Agents...)
		resolveProfile := func(name string) (*spawnProfileJSON, error) {
			for _, i := range selected.Sections["profiles"] {
				var e profileExportEnvelope
				if err := json.Unmarshal(i.Value, &e); err != nil {
					return nil, err
				}
				for _, p := range e.Profiles {
					if p.Name == name || slices.Contains(p.Aliases, name) {
						return &p, nil
					}
				}
			}
			p, err := db.ResolveSpawnProfile(name)
			if err != nil || p == nil {
				return nil, err
			}
			v := profileToJSON(p)
			return &v, nil
		}
		requireRef := func(section, name string) error {
			if name == "" {
				return nil
			}
			if slices.ContainsFunc(selected.Sections[section], func(i configbundle.Item) bool { return i.Name == name }) {
				return nil
			}
			if section == "roles" {
				p, err := db.GetRole(name)
				if err != nil {
					return err
				}
				if p != nil {
					return nil
				}
			} else {
				p, err := resolveProfile(name)
				if err != nil {
					return err
				}
				if p != nil {
					return nil
				}
			}
			return fmt.Errorf("missing %s dependency %q; include its bundle item", section, name)
		}
		for n, a := range check.Agents {
			if err := requireRef("profiles", a.SpawnProfile); err != nil {
				return nil, err
			}
			if err := requireRef("roles", a.RoleRef); err != nil {
				return nil, err
			}
			check.Agents[n].SpawnProfile = ""
			check.Agents[n].RoleRef = ""
			if a.SpawnProfile != "" && a.Harness == "" && (a.ProfileInline == nil || a.ProfileInline.Harness == "") {
				p, err := resolveProfile(a.SpawnProfile)
				if err != nil {
					return nil, err
				}
				if p != nil {
					check.Agents[n].Harness = p.Harness
				}
			}
			if a.ProfileInline != nil {
				inline := *a.ProfileInline
				for _, ref := range append(append([]string{}, inline.RoleRefs...), inline.RoleRef) {
					if err := requireRef("roles", ref); err != nil {
						return nil, err
					}
				}
				inline.RoleRef = ""
				inline.RoleRefs = nil
				check.Agents[n].ProfileInline = &inline
			}
		}
		if _, fail := buildTemplateFromJSON(check); fail != nil {
			return nil, bundleFailure(fail)
		}
		return func() error {
			res, exists, fail := importTemplateEnvelope(env, "", replace)
			preview.Warnings = append(preview.Warnings, res.Warnings...)
			if fail != nil {
				return bundleFailure(fail)
			}
			if exists && !replace {
				return errors.New("template already exists")
			}
			return nil
		}, nil
	case "process-templates":
		var v struct {
			Source string `json:"source"`
		}
		if err := json.Unmarshal(item.Value, &v); err != nil {
			return nil, err
		}
		parsed, err := model.Parse([]byte(v.Source))
		if err != nil {
			return nil, err
		}
		if parsed.Template == nil || parsed.Template.ID != item.Name {
			return nil, errors.New("source must declare the matching process id")
		}
		if parsed.Diagnostics.HasErrors() {
			return nil, errors.New("process source has validation errors")
		}
		fs, err := store.NewFS(processStoreRoot())
		if err != nil {
			return nil, err
		}
		hash := ""
		head, err := fs.GetTemplateHead(r.Context(), item.Name)
		if err == nil {
			source, err := fs.GetTemplateSource(r.Context(), head.Ref)
			if err != nil {
				return nil, err
			}
			hash = processSourceHash(source)
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		if hash != "" && !replace {
			return func() error { return errors.New("process template already exists") }, nil
		}
		return func() error { _, err := fs.PutTemplateEditorSource(r.Context(), parsed.Template, hash); return err }, nil
	case "default-permissions":
		if item.Name != "defaults" {
			return nil, errors.New("expected defaults item")
		}
		var perms []string
		if err := json.Unmarshal(item.Value, &perms); err != nil {
			return nil, err
		}
		for _, perm := range perms {
			if !IsKnownPermSlug(perm) {
				return nil, fmt.Errorf("unknown permission %q", perm)
			}
		}
		return func() error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if cfg.Agent == nil {
				cfg.Agent = &config.AgentConfig{}
			}
			cfg.Agent.DefaultPermissions = perms
			return config.Save(cfg)
		}, nil
	case "config":
		if item.Name == "federation.node_labels" {
			var labels []string
			if err := json.Unmarshal(item.Value, &labels); err != nil {
				return nil, err
			}
			labels, err := proto.NormalizeNodeLabels(labels)
			if err != nil {
				return nil, err
			}
			return func() error {
				cfg, err := config.Load()
				if err != nil {
					return err
				}
				if cfg.Federation == nil {
					cfg.Federation = &config.FederationConfig{}
				}
				cfg.Federation.NodeLabels = labels
				if err = config.Save(cfg); err != nil {
					return err
				}
				broadcastFederationCatalogs()
				return nil
			}, nil
		}
		if !slices.Contains(bundleConfigKeys, item.Name) {
			return nil, errors.New("config field is not portable")
		}
		// Typed decode validates the selected setting without accepting arbitrary
		// fields, credentials or authority settings from a hand-edited bundle.
		raw, _ := json.Marshal(map[string]json.RawMessage{item.Name: item.Value})
		var typed config.Config
		if err := json.Unmarshal(raw, &typed); err != nil {
			return nil, err
		}
		return func() error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			raw, err := json.Marshal(cfg)
			if err != nil {
				return err
			}
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(raw, &obj); err != nil {
				return err
			}
			obj[item.Name] = item.Value
			raw, err = json.Marshal(obj)
			if err != nil {
				return err
			}
			var updated config.Config
			if err = json.Unmarshal(raw, &updated); err != nil {
				return err
			}
			return config.Save(&updated)
		}, nil
	}
	return nil, errors.New("unsupported section")
}
