package agentd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"github.com/tofutools/tclaude/pkg/federation/jobrepo"
)

const PermAgentBundleExport = "agent.bundle.export"
const PermAgentBundleImport = "agent.bundle.import"

// A native transcript may reach executeSpawn only from this permission-gated
// route, never through a user-controlled SpawnRequest resume/id parameter.
type bundleHistoryContextKey struct{}
type bundleHistoryLaunch struct {
	Path     string
	Raw      []byte
	SourceID string
	Format   string
}

func collectAgentBundle(convID string, withHistory bool) (*agentbundle.Bundle, error) {
	h := harnessForConv(convID)
	// Offline native conversations may have only their indexed harness tag.
	if rows, err := db.FindSessionsByConvID(convID); err != nil {
		return nil, err
	} else if len(rows) == 0 {
		if row, err := db.GetConvIndex(convID); err != nil {
			return nil, err
		} else if row != nil && row.Harness != "" {
			h, err = harness.Resolve(row.Harness)
			if err != nil {
				return nil, err
			}
		}
	}
	if h.UsesCommandInput() {
		return nil, errors.New("command-input harnesses cannot be bundled as agents")
	}
	historyWarning := ""
	if withHistory && h.Name == harness.OpenCodeName {
		if _, err := harness.OpenCodeExecutable(); err != nil {
			withHistory = false
			historyWarning = "OpenCode executable unavailable; exporting config only (no conversation history)"
		}
	}
	seed, err := seedProfileFromConv(convID)
	if err != nil {
		return nil, err
	}
	seed.Harness = h.Name
	source, err := db.AgentInitialSpawnConfigForConv(convID)
	if err != nil {
		return nil, err
	}
	var requested agent.SpawnRequest
	if source != "" {
		if err := json.Unmarshal([]byte(source), &requested); err != nil {
			return nil, err
		}
	}
	if requested.Profile != "" {
		p, err := db.ResolveSpawnProfile(requested.Profile)
		if err != nil {
			return nil, err
		}
		if p != nil {
			seed = profileToJSON(p)
			seed.Harness = h.Name
		}
	}
	// Explicit inline launch fields override the current named preset. The
	// overlapping profile fields share the existing spawn wire vocabulary.
	if source != "" {
		if err := json.Unmarshal([]byte(source), &seed); err != nil {
			return nil, err
		}
	}
	// The concrete observed launch wins over a named preset's current settings.
	observed, err := seedProfileFromConv(convID)
	if err != nil {
		return nil, err
	}
	if observed.Model != "" {
		seed.Model = observed.Model
	}
	if observed.Effort != "" {
		seed.Effort = observed.Effort
	}
	if observed.Sandbox != "" {
		seed.Sandbox = observed.Sandbox
	}
	if observed.Approval != "" {
		seed.Approval = observed.Approval
	}
	if relaunch, err := durableRelaunchConfigForConv(convID); err == nil {
		seed.SandboxImplementation = relaunch.SandboxImplementation
		seed.Sandbox = relaunch.Sandbox
		seed.Approval = relaunch.Approval
		seed.AutoReview = &relaunch.AutoReview
		seed.Model, seed.Effort = relaunch.Model, relaunch.Effort
		seed.ToolGovernance = relaunch.ToolGovernance
		seed.AskUserQuestionTimeout = relaunch.AskUserQuestionTimeout
		seed.AutoCompactWindow = relaunch.AutoCompactWindow
		seed.ContextFeatures = relaunch.ContextFeatures
		seed.ContextWindowMax = relaunch.ContextWindowMax
		if h.Name == harness.DefaultName {
			seed.AutoMemory, seed.PeerMessaging = &relaunch.AutoMemory, &relaunch.PeerMessaging
			seed.RemoteControl = &relaunch.RemoteControl
		}
		if h.Name == harness.CodexName {
			seed.CodexAppServer, seed.SSHWorkaround = &relaunch.CodexAppServer, &relaunch.SSHWorkaround
		}
		if h.Name == harness.CopilotName {
			seed.CopilotAPI = &relaunch.CopilotAPI
		}
	}
	name := agent.FreshTitle(convID)
	if name == "" {
		name = requested.Name
	}
	if name == "" {
		name = "imported-agent"
	}
	if requested.InitialMessage == "" {
		requested.InitialMessage = seed.InitialMessage
	}
	d := agentbundle.Definition{Name: name, Role: requested.Role, Description: requested.Descr, Harness: h.Name, ProfileName: requested.Profile, InitialMessage: requested.InitialMessage, StartupContext: seed.StartupContext, Paths: agentbundle.Paths{Cwd: requested.Cwd, Worktree: requested.WorktreePath, Branch: requested.WorktreeBranch}}
	sessions, err := db.FindSessionsByConvID(convID)
	if err != nil {
		return nil, err
	}
	if d.Paths.Cwd == "" && len(sessions) > 0 {
		d.Paths.Cwd = sessions[0].Cwd
	}
	if d.Paths.Cwd == "" {
		if row, err := db.GetConvIndex(convID); err == nil && row != nil {
			d.Paths.Cwd = row.ProjectPath
		}
	}
	// Resolving through the harness can need its CLI (OpenCode execs
	// `opencode`). The cwd is only a placeholder hint unless history is
	// exported, so a config-only bundle must not fail on it.
	needsHistoryCwd := withHistory && h.SupportsHistoryTransfer()
	if d.Paths.Cwd == "" && h.SupportsConvs() {
		ref, err := h.Convs.Resolve(convID, "", true)
		if err != nil && needsHistoryCwd {
			return nil, err
		}
		if err == nil && ref != nil {
			d.Paths.Cwd = ref.ProjectPath
		}
	}
	d.Origin = arrivalOrigin(convID, d.Paths.Cwd, seed.Model)
	if filepath.IsAbs(d.Paths.Cwd) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		d.Paths.RepoURL = jobrepo.OriginHint(ctx, d.Paths.Cwd)
		cancel()
	}
	groups, err := db.ListGroupsForConv(convID)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		m, err := db.FindMemberInGroup(g.ID, convID)
		if err != nil {
			return nil, err
		}
		if m == nil {
			continue
		}
		d.Groups = append(d.Groups, agentbundle.Group{Name: g.Name, Role: m.Role})
		if d.Role == "" {
			d.Role = m.Role
		}
		if d.Description == "" {
			d.Description = m.Descr
		}
		owner, err := db.IsAgentGroupOwner(g.ID, convID)
		if err != nil {
			return nil, err
		}
		if owner {
			for _, perm := range permissionRegistry {
				if perm.OwnerImplied {
					d.Permissions = append(d.Permissions, agentbundle.Permission{Slug: perm.Slug, Effect: "grant", Source: "ownership:" + g.Name})
				}
			}
		}
	}
	rows, err := db.ListAgentPermissionOverrideRowsForConv(convID)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		d.Permissions = append(d.Permissions, agentbundle.Permission{Slug: row.Slug, Effect: row.Effect, Scope: json.RawMessage(row.ScopeJSON), Source: "agent"})
	}
	groupRows, err := db.ListAgentGroupPermissionRowsForConv(convID)
	if err != nil {
		return nil, err
	}
	for _, row := range groupRows {
		d.Permissions = append(d.Permissions, agentbundle.Permission{Slug: row.Slug, Effect: "grant", Scope: json.RawMessage(row.ScopeJSON), Source: "group:" + row.GroupName})
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if cfg.Agent != nil {
		for _, slug := range cfg.Agent.DefaultPermissions {
			d.Permissions = append(d.Permissions, agentbundle.Permission{Slug: slug, Effect: "grant", Source: "defaults"})
		}
	}
	refs := append(append([]string{}, seed.RoleRefs...), seed.RoleRef)
	refs = append(refs, requested.RoleRefs...)
	if requested.RoleRef != "" {
		refs = append(refs, requested.RoleRef)
	}
	seenRoles := map[string]bool{}
	for _, ref := range refs {
		if ref == "" || seenRoles[ref] {
			continue
		}
		seenRoles[ref] = true
		role, err := db.GetRole(ref)
		if err != nil {
			return nil, err
		}
		if role == nil {
			continue
		}
		if role.Brief != "" {
			d.StartupContext += "\n\nRole guidance (" + role.Name + "):\n" + role.Brief
		}
		for _, perm := range role.Permissions {
			d.Permissions = append(d.Permissions, agentbundle.Permission{Slug: perm.Slug, Effect: "grant", Scope: json.RawMessage(perm.Scope), Source: "role:" + role.Name})
		}
	}
	for slug, perm := range seed.PermissionOverrides {
		d.Permissions = append(d.Permissions, agentbundle.Permission{Slug: slug, Effect: perm.Effect, Scope: json.RawMessage(perm.Scope), Source: "profile:" + requested.Profile})
	}
	// Role references, ownership and grants must never become launch inputs.
	seed.Name = "agent"
	seed.AgentName = ""
	seed.RoleRef = ""
	seed.RoleRefs = nil
	seed.IsOwner = nil
	seed.PermissionOverrides = nil
	seed.StartupContext = ""
	seed.InitialMessage = ""
	seed.CreatedAt = ""
	seed.UpdatedAt = ""
	seed.Aliases = nil
	raw, err := json.Marshal(seed)
	if err != nil {
		return nil, err
	}
	safe := configbundle.Bundle{Sections: map[string][]configbundle.Item{"profiles": {{Name: "agent", Value: raw}}}}
	if err := safe.Prepare(); err != nil {
		return nil, err
	}
	d.Profile = safe.Sections["profiles"][0].Value
	b := &agentbundle.Bundle{MaxBytes: agentTransferLimit(), Manifest: agentbundle.Manifest{Format: agentbundle.Format, FormatVersion: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339), TclaudeVersion: buildversion.AppVersion(), Agent: d, Placeholders: safe.Placeholders}}
	if historyWarning != "" {
		b.Manifest.Warnings = append(b.Manifest.Warnings, historyWarning)
	}
	if len(safe.Omitted) > 0 {
		b.Manifest.Warnings = append(b.Manifest.Warnings, "Structured credential fields omitted: "+strings.Join(safe.Omitted, ", "))
	}
	if actor, err := db.GetAgentByConv(convID); err != nil {
		return nil, err
	} else if actor != nil {
		task, err := db.GetAgentTaskRef(actor.AgentID)
		if err != nil {
			return nil, err
		}
		b.Manifest.Agent.TaskURL = task.URL
		b.Manifest.Agent.TaskLabel = task.Label
	}
	if withHistory {
		if !h.SupportsHistoryTransfer() {
			b.Manifest.Warnings = append(b.Manifest.Warnings, h.DisplayName+" does not support imported history; exporting config only")
		} else {
			if err := snapshotAgentHistory(b, h, convID, d.Paths.Cwd); err != nil {
				_ = b.Close()
				return nil, err
			}
		}
	}
	b.Manifest.Findings, err = scanAgentBundle(b)
	if err != nil {
		_ = b.Close()
		return nil, err
	}
	return b, nil
}

// Counts are grouped by kind with at most three locations each, including for
// large tool-output transcripts. Suspected values never reach diagnostics.
func scanAgentBundle(b *agentbundle.Bundle) ([]agentbundle.Finding, error) {
	byKind := map[string]*agentbundle.Finding{}
	scan := func(text, location string) {
		for kind, count := range configbundle.CredentialKinds(text) {
			f := byKind[kind]
			if f == nil {
				f = &agentbundle.Finding{Kind: kind}
				byKind[kind] = f
			}
			f.Count += count
			if len(f.Locations) < 3 {
				f.Locations = append(f.Locations, location)
			}
		}
	}
	var walk func(any, string)
	walk = func(value any, location string) {
		switch v := value.(type) {
		case string:
			scan(v, location)
		case map[string]any:
			keys := make([]string, 0, len(v))
			for key := range v {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			for _, key := range keys {
				walk(v[key], location+"."+key)
			}
		case []any:
			for i, child := range v {
				walk(child, fmt.Sprintf("%s[%d]", location, i))
			}
		}
	}
	raw, _ := json.Marshal(b.Manifest.Agent)
	var definition any
	_ = json.Unmarshal(raw, &definition)
	walk(definition, "manifest.agent")
	metadata, _ := json.Marshal(struct {
		Placeholders []configbundle.Placeholder `json:"placeholders"`
		CreatedAt    string                     `json:"created_at"`
		Version      string                     `json:"tclaude_version"`
		Warnings     []string                   `json:"warnings"`
	}{b.Manifest.Placeholders, b.Manifest.CreatedAt, b.Manifest.TclaudeVersion, b.Manifest.Warnings})
	var meta any
	_ = json.Unmarshal(metadata, &meta)
	walk(meta, "manifest")
	history, err := b.OpenHistory()
	if err != nil {
		return nil, err
	}
	defer history.Close()
	scanner := bufio.NewScanner(history)
	scanner.Buffer(make([]byte, min(64<<10, agentRecordLimit())), agentRecordLimit())
	line := 0
	for scanner.Scan() {
		line++
		var record any
		if json.Unmarshal(scanner.Bytes(), &record) == nil {
			walk(record, fmt.Sprintf("history line %d", line))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan history within federation.agent_history_record_max_bytes=%d: %w", agentRecordLimit(), err)
	}
	kinds := make([]string, 0, len(byKind))
	for kind := range byKind {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	findings := []agentbundle.Finding{}
	for _, kind := range kinds {
		findings = append(findings, *byKind[kind])
	}
	return findings, nil
}

func handleAgentBundleExport(w http.ResponseWriter, r *http.Request) {
	selector := r.URL.Query().Get("agent")
	res, _, err := agent.ResolveSelector(selector)
	if err != nil {
		writeError(w, 404, "agent", err.Error())
		return
	}
	if _, ok := requireCrossAgentPermission(w, r, PermAgentBundleExport, res.ConvID); !ok {
		return
	}
	b, err := collectAgentBundle(res.ConvID, r.URL.Query().Get("history") == "true")
	if err != nil {
		writeError(w, 400, "bundle_export", err.Error())
		return
	}
	defer func() { _ = b.Close() }()
	if len(b.Manifest.Findings) > 0 && r.URL.Query().Get("allow_flagged") != "true" {
		writeJSON(w, 422, map[string]any{"error": "suspected credentials: use --allow-flagged or export without --history", "code": "flagged_credentials", "findings": b.Manifest.Findings})
		return
	}
	if caller, human, ok := authedCaller(w, r); ok && b.Manifest.Agent.Origin != nil {
		b.Manifest.Agent.Origin.Trigger = arrivalTrigger(caller, res.ConvID, human)
	}
	archive, err := archiveAgentBundle(b)
	if err != nil {
		writeError(w, 400, "bundle_export", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="agent.bundle.zip"`)
	w.WriteHeader(200)
	defer func() { _ = archive.Close(); _ = os.Remove(archive.Name()) }()
	_, _ = io.Copy(w, archive)
}

type agentBundlePreview struct {
	Agent      agentbundle.Definition     `json:"agent"`
	Cwd        string                     `json:"cwd"`
	Worktree   string                     `json:"worktree,omitempty"`
	Group      string                     `json:"group,omitempty"`
	History    bool                       `json:"history"`
	Unresolved []configbundle.Placeholder `json:"unresolved,omitempty"`
	Warnings   []string                   `json:"warnings"`
	Security   string                     `json:"security"`
	Findings   []agentbundle.Finding      `json:"findings,omitempty"`
	Applied    bool                       `json:"applied"`
	Spawn      *agent.SpawnResponse       `json:"spawn,omitempty"`
}

func handleAgentBundleImport(w http.ResponseWriter, r *http.Request) {
	authority := teleportLandingFromRequest(r)
	if authority != nil {
		if err := authority.check(); err != nil {
			writeError(w, 403, "teleport_revoked", err.Error())
			return
		}
	} else if _, ok := requirePermission(w, r, PermAgentBundleImport); !ok {
		return
	}
	b, _ := r.Context().Value(agentBundleDataContextKey{}).(*agentbundle.Bundle)
	if b == nil {
		file, err := os.CreateTemp("", ".agent-import-archive-")
		if err != nil {
			writeError(w, 500, "archive", err.Error())
			return
		}
		defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
		n, err := io.Copy(file, http.MaxBytesReader(w, r.Body, agentTransferLimit()+1))
		if err != nil || n > agentTransferLimit() {
			writeError(w, 400, "archive", fmt.Sprintf("archive exceeds or cannot be read within federation.agent_transfer_max_bytes=%d", agentTransferLimit()))
			return
		}
		b, err = agentbundle.DecodeFile(file, agentTransferLimit(), "")
		if err != nil {
			writeError(w, 400, "archive", err.Error())
			return
		}
		defer func() { _ = b.Close() }()
	}
	arrival, _ := r.Context().Value(arrivalContextKey{}).(*arrivalContext)
	if arrival == nil {
		arrival = &arrivalContext{Operation: "bundle import", Source: b.Manifest.Agent, Reason: "explicit import path"}
		r = r.WithContext(context.WithValue(r.Context(), arrivalContextKey{}, arrival))
	}
	if err := teleportImportBundle(r, b); err != nil {
		writeError(w, 409, "teleport_landing", err.Error())
		return
	}
	q := r.URL.Query()
	apply := q.Get("apply") == "true"
	d := b.Manifest.Agent
	if name := q.Get("name"); name != "" {
		d.Name = name
	}
	h, err := harness.ResolveSpawnable(d.Harness)
	if err != nil || h.UsesCommandInput() {
		writeError(w, 400, "harness", "unsupported agent harness")
		return
	}
	// Decode only the existing profile shape, then force every access-bearing
	// field out even for a malicious hand-edited manifest.
	var profile spawnProfileJSON
	if err := json.Unmarshal(d.Profile, &profile); err != nil {
		writeError(w, 400, "profile", err.Error())
		return
	}
	profile.Name = "agent"
	profile.Harness = d.Harness
	profile.RoleRef = ""
	profile.RoleRefs = nil
	profile.IsOwner = nil
	profile.PermissionOverrides = nil
	profile.StartupContext = ""
	profile.InitialMessage = ""
	profile.AgentName = ""
	profile.Aliases = nil
	rawProfile, _ := json.Marshal(profile)
	safe := configbundle.Bundle{Sections: map[string][]configbundle.Item{"profiles": {{Name: "agent", Value: rawProfile}}}, Placeholders: b.Manifest.Placeholders}
	values := map[string]string{}
	for _, binding := range q["set"] {
		name, value, ok := strings.Cut(binding, "=")
		if !ok || name == "" || name == "HOME" {
			writeError(w, 400, "binding", "--set requires name=value; HOME is automatic")
			return
		}
		values[name] = value
	}
	if q.Get("keep_paths") == "true" {
		values = safe.KeepPaths(values)
	}
	missing, err := safe.Resolve(values)
	if err != nil {
		writeError(w, 400, "binding", err.Error())
		return
	}
	d.Profile = safe.Sections["profiles"][0].Value
	if len(missing) == 0 {
		if err := json.Unmarshal(d.Profile, &profile); err != nil {
			writeError(w, 400, "profile", err.Error())
			return
		}
		if _, fail := buildProfileFromJSON(profile); fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
	}
	findings, err := scanAgentBundle(b)
	if err != nil {
		writeError(w, 400, "history", err.Error())
		return
	}
	preview := agentBundlePreview{Findings: findings, Agent: d, Cwd: q.Get("cwd"), Worktree: q.Get("worktree"), Group: q.Get("group"), Unresolved: missing, Warnings: append([]string{}, b.Manifest.Warnings...), Security: "Permissions and ownership are advisory only; none will be copied. Launch posture uses normal receiver spawn checks."}
	if preview.Cwd == "" && q.Get("keep_paths") == "true" {
		preview.Cwd = d.Paths.Cwd
		if preview.Worktree == "" {
			preview.Worktree = d.Paths.Worktree
		}
	}
	pathError := ""
	if preview.Cwd == "" {
		pathError = "set --cwd to remap the working directory, or use --keep-paths to reuse recorded paths"
	} else {
		cwd, err := filepath.Abs(preview.Cwd)
		if err != nil {
			pathError = err.Error()
		} else if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
			pathError = "working directory is missing or not a directory: " + cwd + "; use --cwd to remap it"
		} else {
			preview.Cwd = cwd
		}
	}
	if preview.Worktree != "" {
		abs, err := filepath.Abs(preview.Worktree)
		if err != nil {
			pathError = err.Error()
		} else if st, err := os.Stat(abs); err != nil || !st.IsDir() {
			pathError = "worktree hint is missing or not a directory: " + abs + "; use --worktree to remap it"
		} else {
			preview.Worktree = abs
		}
	}
	if pathError != "" {
		preview.Warnings = append(preview.Warnings, pathError)
	}
	if b.Manifest.History != nil && q.Get("skip_history") != "true" {
		if !h.SupportsHistoryTransfer() {
			preview.Warnings = append(preview.Warnings, h.DisplayName+" cannot resume imported history; importing config only")
		} else if b.Manifest.History.Format != h.History.Format() {
			writeError(w, 400, "history", "history format does not match harness")
			return
		} else if err := validateBundleHistory(h, b); err != nil {
			writeError(w, 400, "history", err.Error())
			return
		} else {
			preview.History = true
		}
	}
	preview.Warnings = append(preview.Warnings, "Source groups are advisory; only --group selects receiving membership. Source profile names and role references do not confer grants.")
	if !apply {
		writeJSON(w, 200, preview)
		return
	}
	if pathError != "" || len(missing) > 0 {
		writeJSON(w, 409, map[string]any{"error": "resolve paths before applying", "code": "unresolved_paths", "preview": preview})
		return
	}
	if preview.Group == "" {
		writeError(w, 400, "group", "--apply requires --group naming an existing receiving group, as with agent spawn")
		return
	}
	var group *db.AgentGroup
	if preview.Group != "" {
		group, err = db.GetAgentGroupByName(preview.Group)
		if err != nil {
			writeError(w, 500, "group", err.Error())
			return
		}
		if group == nil {
			writeError(w, 404, "group", "receiving group does not exist")
			return
		}
	}
	wire, err := agentBundleSpawnWire(d, profile, preview)
	if err != nil {
		writeError(w, 400, "spawn", err.Error())
		return
	}
	if authority != nil {
		var shape map[string]any
		_ = json.Unmarshal(wire, &shape)
		if authority.record.Profile != nil {
			shape["profile"] = authority.record.Profile.Name
		}
		if authority.record.WorkerDefaults != nil {
			shape["permission_overrides"] = authority.record.WorkerDefaults.Permissions
		}
		wire, _ = json.Marshal(shape)
	}
	inner := r.Clone(r.Context())
	inner.Method = http.MethodPost
	inner.Body = io.NopCloser(bytes.NewReader(wire))
	inner.ContentLength = int64(len(wire))
	if preview.History {
		inner = inner.WithContext(context.WithValue(inner.Context(), bundleHistoryContextKey{}, &bundleHistoryLaunch{Raw: b.Transcript, Path: b.TranscriptPath, SourceID: b.Manifest.History.SourceConvID, Format: b.Manifest.History.Format}))
	}
	rec := httptest.NewRecorder()
	handleGroupSpawn(rec, inner, group)
	if rec.Code != 200 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		return
	}
	var spawned agent.SpawnResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &spawned); err != nil {
		writeError(w, 500, "spawn", err.Error())
		return
	}
	preview.Applied = true
	preview.Spawn = &spawned
	writeJSON(w, 200, preview)
}

func agentBundleSpawnWire(d agentbundle.Definition, p spawnProfileJSON, preview agentBundlePreview) ([]byte, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, err
	}
	for _, key := range []string{"name", "aliases", "disabled", "disabled_reason", "operator_only", "agent_name", "role_ref", "role_refs", "startup_context", "created_at", "updated_at", "sync_worktree", "fetch_latest_worktree"} {
		delete(wire, key)
	}
	wire["name"] = d.Name
	wire["role"] = d.Role
	wire["descr"] = d.Description
	wire["harness"] = d.Harness
	wire["cwd"] = preview.Cwd
	wire["worktree_path"] = preview.Worktree
	wire["role_refs"] = []string{}
	wire["permission_overrides"] = map[string]any{}
	wire["is_owner"] = false
	// Guidance is text, not a role/profile grant. No source name is resolved
	// against receiver policy libraries.
	brief := d.StartupContext
	if d.InitialMessage != "" {
		if brief != "" {
			brief += "\n\n"
		}
		brief += d.InitialMessage
	}
	wire["initial_message"] = brief
	wire["task_ref_url"] = d.TaskURL
	wire["task_ref_label"] = d.TaskLabel
	return json.Marshal(wire)
}
