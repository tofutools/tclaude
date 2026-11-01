package agentd

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
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
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"github.com/tofutools/tclaude/pkg/federation/jobrepo"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/legacy"
	"github.com/tofutools/tclaude/pkg/paf/privacy"
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
	managed, err := managedInstructionsForExport(convID)
	if err != nil {
		return nil, err
	}

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
	// Saved prompts name entries in the source's library; never resolve them
	// against the receiver's.
	seed.PromptRefs = nil
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
	b := &agentbundle.Bundle{ManagedInstructions: managed, MaxBytes: agentTransferLimit(), Manifest: agentbundle.Manifest{Format: agentbundle.Format, FormatVersion: 1, CreatedAt: time.Now().UTC().Format(time.RFC3339), TclaudeVersion: buildversion.AppVersion(), Agent: d, Placeholders: safe.Placeholders}}
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
	c := privacy.NewCollector()
	raw, _ := json.Marshal(b.Manifest.Agent)
	c.JSON(raw, "manifest.agent")
	metadata, _ := json.Marshal(struct {
		Placeholders []configbundle.Placeholder `json:"placeholders"`
		CreatedAt    string                     `json:"created_at"`
		Version      string                     `json:"tclaude_version"`
		Warnings     []string                   `json:"warnings"`
	}{b.Manifest.Placeholders, b.Manifest.CreatedAt, b.Manifest.TclaudeVersion, b.Manifest.Warnings})
	c.JSON(metadata, "manifest")
	history, err := b.OpenHistory()
	if err != nil {
		return nil, err
	}
	defer history.Close()
	if err := c.JSONLines(history, "history", agentRecordLimit()); err != nil {
		return nil, fmt.Errorf("federation.agent_history_record_max_bytes=%d: %w", agentRecordLimit(), err)
	}
	return bundleFindings(c.Findings()), nil
}

func bundleFindings(fs []paf.Finding) []agentbundle.Finding {
	out := []agentbundle.Finding{}
	for _, f := range fs {
		out = append(out, agentbundle.Finding{Kind: f.Kind, Count: f.Count, Locations: f.Locations})
	}
	return out
}

// flaggedCredentials reports findings an export refuses unless a human
// allows them: any credential kind. High-entropy strings alone are advisory.
func flaggedCredentials(fs []agentbundle.Finding) bool {
	pf := make([]paf.Finding, 0, len(fs))
	for _, f := range fs {
		pf = append(pf, paf.Finding{Kind: f.Kind, Count: f.Count})
	}
	return privacy.Blocking(pf)
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
	b.Manifest.Agent.CarryPermissions = r.URL.Query().Get("carry_permissions") == "true"
	if b.Manifest.Agent.CarryPermissions {
		b.Manifest.Agent.Permissions = standingCarryRows(b.Manifest.Agent.Permissions)
	}
	if flaggedCredentials(b.Manifest.Findings) && r.URL.Query().Get("allow_flagged") != "true" {
		writeJSON(w, 422, map[string]any{"error": "suspected credentials: use --allow-flagged or export without --history", "code": "flagged_credentials", "findings": b.Manifest.Findings})
		return
	}
	if caller, human, ok := authedCaller(w, r); ok && b.Manifest.Agent.Origin != nil {
		b.Manifest.Agent.Origin.Trigger = arrivalTrigger(caller, res.ConvID, human)
	}
	// An export writes PAF unless asked for the legacy agent bundle v1,
	// which an older tclaude reads ("bundle" is its earlier name).
	format := r.URL.Query().Get("format")
	switch format {
	case "", agentArchivePAF:
		format = agentArchivePAF
	case agentArchiveV1, agentArchiveBundle:
		format = agentArchiveBundle
	default:
		writeError(w, 400, "format", "format must be paf (the default) or v1")
		return
	}
	carried := carriedWorkflowsFromQuery(r.URL.Query())
	if carried.any() && format != agentArchivePAF {
		writeError(w, 400, "format", "only a PAF can carry workflows: drop --format v1")
		return
	}
	workflows, ok := loadCarriedWorkflows(w, r, carried, agentOwnWorkflows(res.ConvID))
	if !ok {
		return
	}
	if fs := scanWorkflowBundles(workflows); len(fs) > 0 {
		b.Manifest.Findings = append(b.Manifest.Findings, bundleFindings(fs)...)
		if flaggedCredentials(b.Manifest.Findings) && r.URL.Query().Get("allow_flagged") != "true" {
			writeJSON(w, 422, map[string]any{"error": "suspected credentials in a carried workflow: use --allow-flagged, or leave the workflow out", "code": "flagged_credentials", "findings": b.Manifest.Findings})
			return
		}
	}
	parts := pafHistoryParts{Workflows: workflows, NoNative: r.URL.Query().Get("no_native") == "true", NoContext: r.URL.Query().Get("no_context") == "true", AllowedFindings: classify(peerFromContext(r.Context())) == classHuman && flaggedCredentials(b.Manifest.Findings)}
	if (parts.NoNative || parts.NoContext) && format != agentArchivePAF {
		writeError(w, 400, "format", "no_native and no_context apply only to format=paf")
		return
	}
	if id := r.URL.Query().Get("summary"); id != "" {
		if format != agentArchivePAF {
			writeError(w, 400, "format", "a summary travels only in a PAF: drop --format v1")
			return
		}
		caller, _, _ := authedCaller(w, r)
		if parts.Handoff, err = takeExportSummary(id, caller, res.ConvID); err != nil {
			writeError(w, 409, "summary", err.Error())
			return
		}
		// The summary is the agent's own text: it is scanned like the
		// conversation it summarizes.
		c := privacy.NewCollector()
		c.Text(parts.Handoff, "context/handoff.md")
		if privacy.Blocking(c.Findings()) && r.URL.Query().Get("allow_flagged") != "true" {
			writeJSON(w, 422, map[string]any{"error": "suspected credentials in the agent's summary: use --allow-flagged, or export without --with-summary", "code": "flagged_credentials", "findings": c.Findings()})
			return
		}
	}
	if parts.NoNative && parts.NoContext && b.Manifest.History != nil {
		writeError(w, 400, "format", "leaving out both the native history and the neutral context exports no conversation; export without history instead")
		return
	}
	var archive *os.File
	if !omitManagedInstructions(w, r, &b.ManagedInstructions, r.URL.Query().Get("omit_managed_instructions") == "true") {
		return
	}
	if format == agentArchivePAF {
		archive, err = archiveAgentPAFWith(b, false, parts)
	} else {
		archive, err = archiveAgentTransfer(b, format, false)
	}
	if err != nil {
		writeError(w, 400, "bundle_export", err.Error())
		return
	}
	if id := r.URL.Query().Get("summary"); id != "" {
		dropExportSummary(id)
	}
	if format == agentArchivePAF {
		w.Header().Set("Content-Type", paf.MediaType)
		w.Header().Set("Content-Disposition", `attachment; filename="agent.paf"`)
	} else {
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="agent.bundle.zip"`)
	}
	w.WriteHeader(200)
	defer func() { _ = archive.Close(); _ = os.Remove(archive.Name()) }()
	_, _ = io.Copy(w, archive)
}

type agentBundlePreview struct {
	ManagedInstructions           []managedInstructionPreview `json:"managed_instructions,omitempty"`
	ManagedInstructionDigest      string                      `json:"managed_instruction_digest,omitempty"`
	ManagedScriptApprovalRequired bool                        `json:"managed_script_approval_required,omitempty"`
	Permissions                   []carriedPermissionDecision `json:"permissions,omitempty"`
	Agent                         agentbundle.Definition      `json:"agent"`
	Cwd                           string                      `json:"cwd"`
	Worktree                      string                      `json:"worktree,omitempty"`
	Group                         string                      `json:"group,omitempty"`
	History                       bool                        `json:"history"`
	Unresolved                    []configbundle.Placeholder  `json:"unresolved,omitempty"`
	Warnings                      []string                    `json:"warnings"`
	Security                      string                      `json:"security"`
	Findings                      []agentbundle.Finding       `json:"findings,omitempty"`
	Landing                       *historyLanding             `json:"history_landing,omitempty"`
	Posture                       *landingPosture             `json:"posture,omitempty"`
	// Confirm lists what a human may keep of what this file would lose. Only a
	// file import has one.
	Confirm *importConfirmation `json:"confirm,omitempty"`
	// Kept and Dropped list the file's elevated items by what became of them.
	Kept    []string `json:"kept,omitempty"`
	Dropped []string `json:"dropped,omitempty"`
	// CarriedWorkflows are workflows the file also holds. Importing the agent
	// stores none of them; a workflow import of the same file reviews each.
	CarriedWorkflows []paf.WorkflowItem `json:"carried_workflows,omitempty"`
	Applied          bool               `json:"applied"`
	// Offline reports an applied import that landed without starting
	// (start=false); resume starts it.
	Offline bool                 `json:"offline,omitempty"`
	Spawn   *agent.SpawnResponse `json:"spawn,omitempty"`
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
	// neutral reads a PAF's neutral context while the archive file is open.
	var neutral func(paf.ReadOptions) (*transcript.Transcript, error)
	// rescan scans a PAF's whole content, including the neutral context and
	// attachments a lowered bundle does not carry.
	var rescan func(int) ([]paf.Finding, error)
	// direct is a file import by the caller, as opposed to a federation
	// offer, move or teleport handing over an already decoded bundle.
	direct := b == nil
	// handoff is the agent-written summary a PAF may carry, and fileLosses
	// its own loss report.
	var handoff string
	var fileLosses *paf.Losses
	// fileDigest binds a human's confirmation to the file they previewed.
	var fileDigest string
	// carriedWorkflows stay in the file: an agent import stores none of them.
	var carriedWorkflows []paf.WorkflowItem
	if b == nil {
		file, err := os.CreateTemp("", ".agent-import-archive-")
		if err != nil {
			writeError(w, 500, "archive", err.Error())
			return
		}
		defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
		hash := sha256.New()
		n, err := io.Copy(io.MultiWriter(file, hash), http.MaxBytesReader(w, r.Body, agentTransferLimit()+1))
		fileDigest = hex.EncodeToString(hash.Sum(nil))
		// A confirmation is for the one agent the human saw listed.
		if key := r.URL.Query().Get("agent"); key != "" {
			sum := sha256.Sum256([]byte(fileDigest + "\n" + key))
			fileDigest = hex.EncodeToString(sum[:])
		}
		if err != nil || n > agentTransferLimit() {
			writeError(w, 400, "archive", fmt.Sprintf("archive exceeds or cannot be read within federation.agent_transfer_max_bytes=%d", agentTransferLimit()))
			return
		}
		var info legacy.Info
		b, info, err = legacy.DecodeBundleFile(file, legacy.DecodeLimits{MaxBytes: agentTransferLimit(), MaxRecordBytes: pafContextRecordLimit(), AgentKey: r.URL.Query().Get("agent")}, "")
		if err != nil {
			writeError(w, 400, "archive", err.Error())
			return
		}
		defer func() { _ = b.Close() }()
		neutral, rescan, handoff, fileLosses = info.Context, info.Scan, info.Handoff, info.Losses
		carriedWorkflows = info.Workflows
		if foreign, _ := r.Context().Value(agentBundleForeignFileContextKey{}).(bool); foreign && fileLosses != nil {
			l := paf.CountsOnly(*fileLosses)
			fileLosses = &l
		}
	} else {
		fileLosses, _ = r.Context().Value(agentBundleLossesContextKey{}).(*paf.Losses)
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
	// start=false lands the applied agent offline: enrolled with its
	// conversation installed, started later by resume.
	noStart := q.Get("start") == "false"
	d := b.Manifest.Agent
	if name := q.Get("name"); name != "" {
		d.Name = name
	}
	// A performer lands under the role this node's workflow asks for. The
	// importer names it; the file only ever suggests one, so choosing an agent
	// out of a file takes no role from it.
	if q.Get("agent") != "" && direct {
		d.Role = ""
	}
	if role := q.Get("role"); role != "" && direct {
		d.Role = role
	}
	// The agent lands on its own harness unless the importer names another;
	// the conversation then translates (see planHistoryLanding).
	landingName, model := q.Get("harness"), q.Get("model")
	if landingName == "" {
		landingName = d.Harness
	}
	h, err := harness.ResolveSpawnable(landingName)
	if err != nil || h.UsesCommandInput() {
		msg := "unsupported agent harness"
		if q.Get("harness") == "" && direct {
			msg = fmt.Sprintf("harness %q is not available here; choose a landing harness with --harness", d.Harness)
		}
		writeError(w, 400, "harness", msg)
		return
	}
	src, _ := harness.Resolve(d.Harness)
	crossHarness := src == nil || src.Name != h.Name
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
	profile.PromptRefs = nil
	profile.IsOwner = nil
	profile.PermissionOverrides = nil
	profile.StartupContext = ""
	profile.InitialMessage = ""
	profile.AgentName = ""
	profile.Aliases = nil
	// A file's environment never lands, so nothing in it can demand a binding.
	accepted, _ := r.Context().Value(acceptedMoveContextKey{}).(bool)
	movedHere := authority != nil || accepted
	fileEnvironment := profile.Environment
	if !movedHere {
		profile.Environment = nil
	}
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
	var landingWarnings []string
	// What this file loses, and what a human may keep of it. Only a file import
	// or an offer a human accepts reaches a selection: every other path here
	// builds its own query.
	offerList, _ := r.Context().Value(offerConfirmKey{}).(*offerConfirmation)
	var keep importKeep
	switch {
	case offerList != nil:
		keep, fileDigest = offerList.keep, offerList.digest
	case direct:
		keep = readImportKeep(r)
	}
	selection := keep
	if !apply {
		selection = keep.preview()
	}
	if offerList != nil && offerList.unrestricted && !keep.named {
		// A request with no selection takes the list's ticks: all, from the
		// operator's own nodes, and none from anyone else, whatever they hold.
		selection = importKeep{human: true, named: true, all: true}
	}
	var fileDrops []fileDrop
	if crossHarness || model != "" {
		if err := json.Unmarshal(d.Profile, &profile); err != nil {
			writeError(w, 400, "profile", err.Error())
			return
		}
		if crossHarness {
			// Launch posture is harness specific: on another harness the
			// receiver's defaults and normal spawn checks apply instead.
			landingWarnings = append(landingWarnings, fmt.Sprintf("The launch posture recorded for %s is not applied on %s; receiver defaults and normal spawn checks apply.", d.Harness, h.Name))
			profile = spawnProfileJSON{Name: "agent"}
			missing = nil
		}
		profile.Harness = h.Name
		if model != "" {
			profile.Model = model
		}
		d.Profile, _ = json.Marshal(profile)
	}
	d.Harness = h.Name
	// A teleport's landing authority names the profile its spawn takes below.
	landingProfile := ""
	if authority != nil && authority.record.Profile != nil {
		landingProfile = authority.record.Profile.Name
	}
	if len(missing) == 0 {
		if err := json.Unmarshal(d.Profile, &profile); err != nil {
			writeError(w, 400, "profile", err.Error())
			return
		}
		recordedSandbox := profile
		profile = receiverSandboxProfile(profile, h, q.Get("group"), landingProfile)
		// The receiver owns a teleport or direct move, by landing authority or by
		// accepting the offer, and dropping its posture could stall the agent on
		// prompts nobody answers. Everything else is a file.
		if !movedHere || offerList != nil {
			landingWarnings = append(landingWarnings, droppedSandbox(recordedSandbox, profile, h, q.Get("group"), landingProfile)...)
			var drops []fileDrop
			profile, drops = receiverApprovalDrops(profile, h, q.Get("group"), landingProfile)
			fileDrops = append(fileDrops, drops...)
			profile.Environment = make([]sandboxpolicy.EnvironmentEntry, len(fileEnvironment))
			for i, entry := range fileEnvironment {
				profile.Environment[i] = sandboxpolicy.EnvironmentEntry{Name: entry.Name, Value: fileEnvironmentValue(entry, safe.Placeholders, values)}
			}
			// A teleport offer's route is no file's: its credential mode wrote it,
			// checked against this receiver's landing policy.
			route := ""
			if offer, _ := r.Context().Value(teleportOfferContextKey{}).(*db.FederationBundleOffer); offer != nil && offer.Descriptor.Teleport != nil {
				route, profile.ModelProxy = profile.ModelProxy, ""
			}
			profile, drops = receiverLaunchFieldsDrops(profile, h, q.Get("group"), landingProfile)
			fileDrops = append(fileDrops, drops...)
			if route != "" {
				profile.ModelProxy = route
			}
		} else if accepted {
			// The grant trusts this peer with moves, and it says nothing about what
			// the operator is accepting, so show what a file would have lost.
			_, kept := receiverApprovalProfile(profile, h, q.Get("group"), landingProfile)
			_, fields := receiverLaunchFieldsProfile(profile, h, q.Get("group"), landingProfile)
			if kept = append(kept, fields...); len(kept) > 0 {
				landingWarnings = append(landingWarnings, "This offer lands as a move from a peer holding agents.teleport.receive, so it keeps what a file would lose. As a file: "+strings.Join(kept, " "))
			}
		}
		if _, fail := buildProfileFromJSON(profile); fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
	}
	var findings []agentbundle.Finding
	if rescan != nil {
		var fs []paf.Finding
		fs, err = rescan(agentRecordLimit())
		findings = bundleFindings(fs)
	} else {
		findings, err = scanAgentBundle(b)
	}
	if err != nil {
		writeError(w, 400, "history", err.Error())
		return
	}
	carryPolicy, hasCarryPolicy := r.Context().Value(permissionCarryContextKey{}).(permissionCarryPolicy)
	if !hasCarryPolicy {
		carryPolicy = permissionCarryPolicy{Group: q.Get("group"), Enabled: d.CarryPermissions && q.Get("carry_permissions") == "true", AllowSensitive: q.Get("allow_sensitive_permissions") == "true"}
		if apply && carryPolicy.Enabled {
			if _, ok := requirePermission(w, r, PermPermissionsGrant); !ok {
				return
			}
		}
	}
	if carryPolicy.Enabled && len(d.Permissions) > 512 {
		writeError(w, 400, "permissions", "permission carry exceeds 512 rows")
		return
	}
	carryDecisions, carryOverrides := planCarriedPermissions(d.Permissions, carryPolicy)
	// Permissions join the list last, since it leaves out what carry applies. A
	// kept permission lands through the carry path, with its checks at launch.
	keptPermissions := map[string]bool{}
	if direct && !movedHere {
		fileDrops = append(fileDrops, fileAgentPermissionDrops(d.Permissions, q.Get("group"), carryOverrides, keptPermissions)...)
	}
	if offerList != nil {
		for i := range fileDrops {
			fileDrops[i].item.Default, fileDrops[i].item.Revealable = offerList.unrestricted, false
		}
	}
	settled := settleFileDrops(fileDrops, selection, &profile)
	landingWarnings = append(landingWarnings, settled.warnings...)
	if len(keptPermissions) > 0 {
		carried := slices.DeleteFunc(slices.Clone(d.Permissions), func(row agentbundle.Permission) bool {
			_, applied := carryOverrides[row.Slug]
			return !keptPermissions[row.Slug] && !applied
		})
		d.Permissions = carried
		carryPolicy = permissionCarryPolicy{Group: q.Get("group"), Enabled: true, AllowSensitive: true}
		carryDecisions, carryOverrides = planCarriedPermissions(d.Permissions, carryPolicy)
	}
	if authority != nil && authority.record.WorkerDefaults != nil {
		for i := range carryDecisions {
			d := &carryDecisions[i]
			if authority.record.WorkerDefaults.Permissions[d.Slug].Effect == "deny" && d.Effect != "deny" && d.Decision != "drop" {
				d.Decision, d.Reason = "drop", "receiver worker policy deny wins"
			}
		}
	}
	preview := agentBundlePreview{Permissions: carryDecisions, Findings: findings, Agent: d, Cwd: q.Get("cwd"), Worktree: q.Get("worktree"), Group: q.Get("group"), Unresolved: missing, Warnings: append([]string{}, b.Manifest.Warnings...), Security: "Permissions and ownership are advisory only; none will be copied. " + agentImportSecurityNote}
	if carryPolicy.Enabled {
		preview.Security = permissionCarrySummary(preview.Permissions) + " " + agentImportSecurityNote
	}
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
	preview.Warnings = append(preview.Warnings, landingWarnings...)
	// Budget the conversation for the model the agent will launch with: the
	// requested one, else the profile's, else on the same harness the model
	// the source last ran.
	landingModel := model
	if landingModel == "" {
		landingModel = profile.Model
	}
	if landingModel == "" && !crossHarness && d.Origin != nil {
		landingModel = d.Origin.Model
	}
	landing, warnings, err := planHistoryLanding(b, h, landingModel, preview.Cwd, neutral, landingOptions{Skip: q.Get("skip_history") == "true", BudgetNative: direct && landsOnSmallerWindow(h, d.Origin, landingModel), Summary: handoff})
	if err != nil {
		writeError(w, 400, "history", err.Error())
		return
	}
	defer landing.close()
	preview.Landing, preview.History = landing, landing.history != nil
	if noStart && h.UsesAuthoritativeServer() && landing.Rung == landingNative && landingModel != "" &&
		(d.Origin == nil || d.Origin.Model != landingModel) {
		// Only the launch prompt names a model to this harness, and an offline
		// landing sends none, so the imported conversation's model carries on.
		preview.Warnings = append(preview.Warnings, fmt.Sprintf(
			"This offline landing records %s, but its conversation continues on the model it last used until you change it there. Start the import instead to apply the model now.",
			landingModel))
	}
	// The posture this import launches under: the receiver's, which a
	// recorded posture can tighten but not widen.
	var postureGroup *db.AgentGroup
	if preview.Group != "" {
		if postureGroup, err = db.GetAgentGroupByName(preview.Group); err != nil {
			writeError(w, 500, "group", err.Error())
			return
		}
	}
	if pathError == "" && (postureGroup != nil || preview.Group == "") {
		var environment []sandboxpolicy.EnvironmentEntry
		if postureGroup != nil {
			environment = postureGroup.Environment
		}
		kept := receiverSandboxProfile(profile, h, q.Get("group"), landingProfile)
		preview.Posture, err = resolveLandingPosture(postureGroup, h.Name,
			kept.SandboxImplementation, kept.Sandbox, landingModel, preview.Cwd, environment)
		if err != nil {
			writeError(w, 500, "posture", err.Error())
			return
		}
	}
	preview.Warnings = append(preview.Warnings, warnings...)
	if direct {
		// A direct import already tried to land the neutral context; the
		// landing warnings say why when it did not.
		preview.Warnings = slices.DeleteFunc(preview.Warnings, func(s string) bool { return s == legacy.WarningContextOnly })
	}
	// A large cut of a file import needs the importer's confirmation, as a
	// morph's does. The full history is never lost: it stays in the file
	// and is saved beside the agent.
	needsConfirm := direct && landing.Losses != nil && landing.Losses.NeedsSummary && preview.History
	if needsConfirm {
		preview.Warnings = append(preview.Warnings, fmt.Sprintf("Cutting %d%% of the conversation needs confirmation: --confirm-history-loss.", landing.cutPercent()))
	}
	if !omitManagedInstructions(w, r, &b.ManagedInstructions, q.Get("omit_managed_instructions") == "true") {
		return
	}
	managedPreview, managedDigest, managedCode, managedErr := managedInstructionPreviews(b.ManagedInstructions)
	if managedErr != nil {
		writeError(w, 400, "managed_instructions", managedErr.Error())
		return
	}
	preview.ManagedInstructions, preview.ManagedInstructionDigest, preview.ManagedScriptApprovalRequired = managedPreview, managedDigest, managedCode
	if (direct && !movedHere) || offerList != nil {
		preview.Confirm = newImportConfirmation(fileDigest, fileDrops, keep.human, preview.Warnings)
	}
	preview.Kept, preview.Dropped = settled.kept, settled.dropped
	preview.CarriedWorkflows = carriedWorkflows
	if offerList != nil {
		// Refused here too, so a stale selection reserves no launch.
		if err := keep.check(fileDigest, fileImportItems(fileDrops)); err != nil {
			var refused *importKeepError
			errors.As(err, &refused)
			writeError(w, refused.status, refused.code, err.Error())
			return
		}
	}
	if !apply {
		writeJSON(w, 200, preview)
		return
	}
	if managedCode {
		if q.Get("approve_managed_scripts") != managedDigest {
			writeJSON(w, 409, map[string]any{"code": "managed_script_approval_required", "error": "Inspect the carried script files, then explicitly approve this transfer's digest or omit managed instructions.", "preview": preview})
			return
		}
		if !requireHuman(w, r, "managed instruction script approval or omission requires the receiving human") {
			return
		}
	}
	if needsConfirm && q.Get("confirm_history_loss") != "true" {
		writeJSON(w, 409, map[string]any{"code": "history_confirmation_required", "preview": preview,
			"error": fmt.Sprintf("landing on %s cuts about %d%% of the conversation. Confirm the cut with --confirm-history-loss (the full history stays in the file and is saved beside the agent), choose a model with a larger context window with --model, or import without the conversation with --skip-history.", landingModelLabel(h, landingModel), landing.cutPercent())})
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
	if err := keep.check(fileDigest, fileImportItems(fileDrops)); err != nil {
		var refused *importKeepError
		errors.As(err, &refused)
		writeError(w, refused.status, refused.code, err.Error())
		return
	}
	if noStart && !preview.History {
		writeError(w, 400, "no_start", "only an agent whose conversation lands can be imported without starting; import and start it instead")
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
	var launchShape map[string]any
	_ = json.Unmarshal(wire, &launchShape)
	var receiverOverrides map[string]db.PermissionOverride
	if raw, err := json.Marshal(launchShape["permission_overrides"]); err == nil {
		_ = json.Unmarshal(raw, &receiverOverrides)
	}
	if receiverOverrides == nil {
		receiverOverrides = map[string]db.PermissionOverride{}
	}
	for slug, override := range carryOverrides {
		if receiverOverrides[slug].Effect == "deny" && override.Effect != "deny" {
			for i := range preview.Permissions {
				if preview.Permissions[i].Slug == slug && preview.Permissions[i].Effect != "deny" {
					preview.Permissions[i].Decision = "drop"
					preview.Permissions[i].Reason = "receiver worker policy deny wins"
				}
			}
			continue
		}
		receiverOverrides[slug] = override
	}
	launchShape["permission_overrides"] = receiverOverrides
	if carryPolicy.Enabled {
		brief, _ := launchShape["initial_message"].(string)
		launchShape["initial_message"] = permissionCarrySummary(preview.Permissions) + "\n\n" + brief
	}
	wire, _ = json.Marshal(launchShape)
	inner := r.Clone(context.WithValue(r.Context(), permissionCarryLaunchContextKey{}, permissionCarryLaunch{Policy: carryPolicy, Rows: d.Permissions, Expected: carryOverrides}))
	if a := arrivalFromRequest(inner); a != nil && carryPolicy.Enabled {
		copy := *a
		copy.PermissionSummary = permissionCarrySummary(preview.Permissions)
		inner = inner.WithContext(context.WithValue(inner.Context(), arrivalContextKey{}, &copy))
	}
	inner = inner.WithContext(context.WithValue(inner.Context(), managedInstructionLaunchContextKey{}, b.ManagedInstructions))
	inner.Method = http.MethodPost
	inner.Body = io.NopCloser(bytes.NewReader(wire))
	inner.ContentLength = int64(len(wire))
	if preview.History {
		inner = inner.WithContext(context.WithValue(inner.Context(), bundleHistoryContextKey{}, landing.history))
	}
	if noStart {
		inner = inner.WithContext(context.WithValue(inner.Context(), bundleNoLaunchContextKey{}, true))
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
	if carryPolicy.Enabled {
		auditDecisions, _ := json.Marshal(preview.Permissions)
		recordFederationAudit("agent.permissions.carry", carryPolicy.Peer, spawned.AgentID, preview.Group, string(auditDecisions), 200)
	}
	if landing.full != nil {
		saveLandedHistory(spawned, landing, group)
	}
	if landing.history != nil {
		recordConvLosses(spawned.ConvID, fileLosses, landing.Losses)
	}
	logKeptImportItems(settled.kept)
	preview.Applied = true
	preview.Offline = noStart
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
	for _, key := range []string{"name", "aliases", "disabled", "disabled_reason", "operator_only", "agent_name", "role_ref", "role_refs", "prompt_refs", "startup_context", "created_at", "updated_at", "sync_worktree", "fetch_latest_worktree"} {
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

// receiverSandboxProfile applies the rule for a sandbox posture an agent bundle
// recorded: a file may tighten the receiver's posture, never widen it. These
// fields land on the spawn's explicit tier, above every receiver default, so
// anything not proven tighter is dropped and the receiver's posture applies.
//
// "Not looser" is the sandbox lineage guard's question, asked with the landing
// posture as the parent, around three cases it does not answer:
//
//   - tclaude's sandbox stays: a move must not take an agent's wall away, and
//     the normal launch gates refuse it where this node cannot run it. The guard
//     would refuse it from a parent whose harness has no proven posture. The
//     exception is receiverCapsTheWall.
//   - a landing under tclaude's sandbox keeps it: the guard would let a native
//     mode replace the wall, which also stops enforcing the sandbox profiles.
//   - an implementation with no OS confinement never stays: it cannot tighten,
//     and it would drop the receiver's sandbox profiles from the launch.
//
// Anything else that cannot be judged is dropped.
func receiverSandboxProfile(p spawnProfileJSON, h *harness.Harness, group, profile string) spawnProfileJSON {
	recorded := p
	p.Sandbox, p.SandboxImplementation = "", ""
	if isTclaudeLayer(recorded.SandboxImplementation) {
		if !receiverCapsTheWall(h, group, profile) {
			p.SandboxImplementation = string(sandboxpolicy.ImplementationTclaudeLayer)
		}
		return p
	}
	if recorded.Sandbox == "" && recorded.SandboxImplementation == "" {
		return p
	}
	// A relaunch profile spells a blank implementation as harness-builtin, which
	// spawn refuses for a harness with no OS sandbox. Blank is what that launch had.
	recordedImplementation := recorded.SandboxImplementation
	if implementation, err := sandboxpolicy.NormalizeImplementation(recordedImplementation); err == nil &&
		implementation == sandboxpolicy.ImplementationHarnessBuiltin &&
		harness.ValidateHarnessBuiltinOSSandbox(h) != nil {
		recordedImplementation = ""
	}
	implementation, err := validateSandboxImplementationForHarness(h, recordedImplementation)
	if err != nil {
		return p
	}
	normalized := normalizeLineageImplementation(implementation)
	if normalized.OmitsOSConfinement() {
		return p
	}
	landing, fail, err := resolveLaunchDefaults(group, profile, h.Name)
	if fail != nil || err != nil || isTclaudeLayer(string(landing.implementation)) {
		return p
	}
	landingMode, fail := resolveLaunchHarnessBuiltinMode(h, landing.sandbox, string(landing.implementation))
	if fail != nil {
		return p
	}
	recordedMode, fail := resolveLaunchHarnessBuiltinMode(h, recorded.Sandbox, implementation)
	if fail != nil {
		return p
	}
	if !spawnSandboxLineageAllowed(
		spawnLineageSandbox{Harness: h.Name, HarnessBuiltinMode: landingMode, Implementation: landing.implementation},
		spawnLineageSandbox{Harness: h.Name, HarnessBuiltinMode: recordedMode, Implementation: normalized}) {
		return p
	}
	p.Sandbox, p.SandboxImplementation = recorded.Sandbox, implementation
	return p
}

// receiverCapsTheWall reports whether the landing posture is a native mode the
// guard holds tclaude's sandbox to be looser than: Codex read-only or
// workspace-write, where the wall would launch danger-full-access writing its cwd
// subtree with the network open. A landing the guard cannot judge keeps the wall.
func receiverCapsTheWall(h *harness.Harness, group, profile string) bool {
	landing, fail, err := resolveLaunchDefaults(group, profile, h.Name)
	if fail != nil || err != nil || isTclaudeLayer(string(landing.implementation)) {
		return false
	}
	landingMode, fail := resolveLaunchHarnessBuiltinMode(h, landing.sandbox, string(landing.implementation))
	if fail != nil {
		return false
	}
	parent := spawnLineageSandbox{Harness: h.Name, HarnessBuiltinMode: landingMode, Implementation: landing.implementation}
	if _, judged := normalizeSpawnLineageSandbox(parent); !judged {
		return false
	}
	wallMode, fail := resolveLaunchHarnessBuiltinMode(h, "", string(sandboxpolicy.ImplementationTclaudeLayer))
	if fail != nil {
		return false
	}
	return !spawnSandboxLineageAllowed(parent, spawnLineageSandbox{
		Harness: h.Name, HarnessBuiltinMode: wallMode, Implementation: sandboxpolicy.ImplementationTclaudeLayer,
	})
}

// receiverApprovalProfile applies receiverSandboxProfile's rule to the approval
// policy, tool policy and auto-review a file recorded: they too land on the
// explicit tier, so what is not proven no wider than a spawn here would resolve
// is dropped and the receiver's value applies. Tightening stays. A value the
// guard cannot judge is dropped. It returns a warning for each one dropped.
// receiverContextFeatures is the map a file's context features land as, and what
// it asked for and did not get. An explicit map replaces the tiers' whole, so it
// starts from what a spawn here resolves, and takes the file's trims on top. A
// trim that takes away instructions or policy rather than a capability is not a
// tightening, and neither is keeping a feature a tier here trims.
func receiverContextFeatures(recorded map[string]string, h *harness.Harness, tiers []launchProfileTier) (map[string]string, []string) {
	names := slices.Sorted(maps.Keys(recorded))
	requested, err := harness.ResolveContextFeatures(h, recorded)
	if err != nil {
		return nil, names
	}
	landing, _, fail := resolveContextFeaturesLaunchField(nil, false, h, tiers)
	if fail != nil {
		return nil, names
	}
	out := maps.Clone(landing)
	if out == nil {
		out = map[string]string{}
	}
	var cut []string
	for _, feature := range slices.Sorted(maps.Keys(requested)) {
		switch trim := requested[feature] == harness.ContextFeatureOff; {
		case trim && !harness.ContextFeatureGuidesTheAgent(feature):
			out[feature] = harness.ContextFeatureOff
		case requested[feature] != landing[feature] && (trim || landing[feature] == harness.ContextFeatureOff):
			cut = append(cut, feature)
		}
	}
	if len(out) == 0 {
		return nil, cut
	}
	return out, cut
}

// agentImportSecurityNote closes every agent import's security line. It is true
// of every file, so it is no warning.
const agentImportSecurityNote = "Launch posture uses normal receiver spawn checks. Source groups, profile names and role references confer nothing: only the group chosen here applies."

// fileFieldDropped is the warning for what a file asks for and does not get.
func fileFieldDropped(what string) string {
	return fmt.Sprintf("This file asks for %s, which a spawn here would not give, so it is not applied.", what)
}

// filePostureDropped is the warning for a posture a file asks for and does not
// get. kept says what applies instead.
func filePostureDropped(field, value, kept string) string {
	return fmt.Sprintf("This file asks for %s %s, which is wider than this node would give, or cannot be compared: %s applies.", field, value, kept)
}

// droppedSandbox warns of the sandbox posture receiverSandboxProfile took out of
// recorded to leave kept. A file that asks for what the landing resolves to loses
// nothing: a harness with no OS sandbox never keeps its posture, even an equal one.
func droppedSandbox(recorded, kept spawnProfileJSON, h *harness.Harness, group, profile string) []string {
	if kept.Sandbox != "" || kept.SandboxImplementation != "" {
		return nil
	}
	value := cmp.Or(recorded.Sandbox, recorded.SandboxImplementation)
	if value == "" || sandboxMatchesLanding(recorded, h, group, profile) {
		return nil
	}
	return []string{filePostureDropped("sandbox", value, "this node's sandbox")}
}

func sandboxMatchesLanding(recorded spawnProfileJSON, h *harness.Harness, group, profile string) bool {
	landing, fail, err := resolveLaunchDefaults(group, profile, h.Name)
	if fail != nil || err != nil {
		return false
	}
	implementation, err := sandboxpolicy.NormalizeImplementation(recorded.SandboxImplementation)
	if err != nil || (recorded.SandboxImplementation != "" && implementation != landing.implementation) {
		return false
	}
	landingMode, fail := resolveLaunchHarnessBuiltinMode(h, landing.sandbox, string(landing.implementation))
	if fail != nil {
		return false
	}
	recordedMode, fail := resolveLaunchHarnessBuiltinMode(h, recorded.Sandbox, string(landing.implementation))
	return fail == nil && recordedMode == landingMode
}

// receiverLaunchFieldsProfile takes out of a file's profile the launch fields
// that widen an agent without being a posture, and returns a warning for each.
func receiverLaunchFieldsProfile(p spawnProfileJSON, h *harness.Harness, group, profile string) (spawnProfileJSON, []string) {
	p, drops := receiverLaunchFieldsDrops(p, h, group, profile)
	return p, renderFileDrops(drops)
}

// receiverLaunchFieldsDrops is receiverLaunchFieldsProfile, with each drop as
// what a human may keep: pre-trusting its directory, opening it to remote control
// or peer messaging, choosing its model route, keeping features a tier here trims,
// and setting its environment.
func receiverLaunchFieldsDrops(p spawnProfileJSON, h *harness.Harness, group, profile string) (spawnProfileJSON, []fileDrop) {
	var tiers []launchProfileTier
	if landing, fail, err := resolveLaunchDefaults(group, profile, h.Name); fail == nil && err == nil {
		tiers = landing.tiers
	}
	var drops []fileDrop
	drop := func(id, label, what, gives string, keep func(*spawnProfileJSON)) {
		drops = append(drops, fileDrop{warning: fileFieldDropped(what), keep: keep,
			item: importItem{ID: id, Kind: importKindLaunch, Label: label, Gives: gives, Default: true}})
	}
	gives := func(field string, value func(*db.SpawnProfile) *bool) bool {
		given, _, _, _, fail := resolveBoolLaunchField(field, false, false, h.Name, tiers, value,
			func(v bool) (bool, error) { return v, nil })
		return fail == nil && given
	}
	on := func(set func(*spawnProfileJSON, *bool)) func(*spawnProfileJSON) {
		return func(p *spawnProfileJSON) {
			yes := true
			set(p, &yes)
		}
	}
	// Left blank, a true field resolves to what a spawn here gives. A false one
	// only asks for less.
	if p.TrustDir != nil && *p.TrustDir {
		p.TrustDir = nil
		if !gives("trust_dir", func(tier *db.SpawnProfile) *bool { return tier.TrustDir }) {
			drop("trust_dir", "trusted working directory", "a trusted working directory",
				"Skips the harness's trust prompt for the working directory.",
				on(func(p *spawnProfileJSON, v *bool) { p.TrustDir = v }))
		}
	}
	if p.RemoteControl != nil && *p.RemoteControl {
		p.RemoteControl = nil
		if !gives("remote_control", func(tier *db.SpawnProfile) *bool { return tier.RemoteControl }) {
			drop("remote_control", "remote control", "remote control",
				"Opens the agent to remote control from outside this node.",
				on(func(p *spawnProfileJSON, v *bool) { p.RemoteControl = v }))
		}
	}
	if p.PeerMessaging != nil && *p.PeerMessaging {
		p.PeerMessaging = nil
		if !gives("peer_messaging", func(tier *db.SpawnProfile) *bool { return tier.PeerMessaging }) {
			drop("peer_messaging", "peer messaging", "peer messaging",
				"Lets the agent reach other sessions through the harness's own messaging.",
				on(func(p *spawnProfileJSON, v *bool) { p.PeerMessaging = v }))
		}
	}
	if p.AutoMemory != nil && *p.AutoMemory {
		p.AutoMemory = nil
		if !gives("auto_memory", func(tier *db.SpawnProfile) *bool { return tier.AutoMemory }) {
			drop("auto_memory", "auto memory", "auto memory",
				"Lets the harness load and write its automatic memory.",
				on(func(p *spawnProfileJSON, v *bool) { p.AutoMemory = v }))
		}
	}
	if p.CopilotAPI != nil && *p.CopilotAPI {
		p.CopilotAPI = nil
		if !gives("copilot_api", func(tier *db.SpawnProfile) *bool { return tier.CopilotAPI }) {
			drop("copilot_api", "Copilot API drive", "the Copilot API drive",
				"Drives Copilot through its API rather than its terminal.",
				on(func(p *spawnProfileJSON, v *bool) { p.CopilotAPI = v }))
		}
	}
	// Auto-continuing past a question an agent asked is an explicit opt-in.
	if recorded := strings.TrimSpace(p.AskUserQuestionTimeout); recorded != "" {
		p.AskUserQuestionTimeout = ""
		given, _, _, fail := resolveStringLaunchField("ask_user_question_timeout", "", h.Name, tiers,
			func(tier *db.SpawnProfile) string { return tier.AskUserQuestionTimeout },
			func(v string) (string, error) { return v, nil })
		if fail != nil || given != recorded {
			drop("ask_user_question_timeout", "question timeout "+recorded, "a question timeout of "+recorded,
				"Continues without an answer when a question the agent asked goes unanswered for "+recorded+".",
				func(p *spawnProfileJSON) { p.AskUserQuestionTimeout = recorded })
		}
	}
	// A proxy carries the whole conversation, and "off" steps around the one a
	// spawn here would use. Left blank, this resolves to the receiver's own.
	if recorded := strings.TrimSpace(p.ModelProxy); recorded != "" {
		p.ModelProxy = ""
		given, _, _, fail := resolveStringLaunchField("model_proxy", "", h.Name, tiers,
			func(tier *db.SpawnProfile) string { return tier.ModelProxy },
			func(v string) (string, error) { return v, nil })
		if fail != nil || (given != recorded && (recorded != "off" || given != "")) {
			drop("model_proxy", "model proxy "+recorded, "model proxy "+recorded,
				"Routes the agent's whole conversation through this proxy, or around this node's own.",
				func(p *spawnProfileJSON) { p.ModelProxy = recorded })
		}
	}
	if len(p.ContextFeatures) > 0 {
		recorded := maps.Clone(p.ContextFeatures)
		var cut []string
		p.ContextFeatures, cut = receiverContextFeatures(p.ContextFeatures, h, tiers)
		_, resolveErr := harness.ResolveContextFeatures(h, recorded)
		for _, feature := range cut {
			d := fileDrop{list: "context", name: feature}
			if resolveErr == nil {
				value := cmp.Or(recorded[feature], harness.ContextFeatureOff)
				d.item = importItem{ID: "context:" + feature, Kind: importKindContext, Label: "context feature " + feature + " " + value, Default: true,
					Gives: "Sets " + feature + " to " + value + ", which a spawn here would not: it can take away the repo's or organization's instructions, or keep what this node trims."}
				d.keep = func(p *spawnProfileJSON) {
					if p.ContextFeatures == nil {
						p.ContextFeatures = map[string]string{}
					}
					p.ContextFeatures[feature] = value
				}
			}
			drops = append(drops, d)
		}
	}
	// A variable can change what the harness loads or how it decides, so none is
	// taken from a file. The warning names them; their values may be secrets.
	for _, entry := range p.Environment {
		given := false
		for _, tier := range tiers {
			given = given || (tier.profile != nil && slices.Contains(tier.profile.Environment, entry))
		}
		if !given {
			drops = append(drops, fileDrop{list: "environment", name: entry.Name,
				item: environmentItem(entry, "Set in the agent's process. It can change what the harness loads or how it decides."),
				keep: func(p *spawnProfileJSON) { p.Environment = append(p.Environment, entry) }})
		}
	}
	p.Environment = nil
	return p, drops
}

func receiverApprovalProfile(p spawnProfileJSON, h *harness.Harness, group, profile string) (spawnProfileJSON, []string) {
	p, drops := receiverApprovalDrops(p, h, group, profile)
	return p, renderFileDrops(drops)
}

// receiverApprovalDrops is receiverApprovalProfile, with each drop as what a
// human may keep.
func receiverApprovalDrops(p spawnProfileJSON, h *harness.Harness, group, profile string) (spawnProfileJSON, []fileDrop) {
	recorded := p
	p.Approval, p.ToolGovernance, p.AutoReview = "", "", nil
	var drops []fileDrop
	drop := func(field, value, kept string) {
		d := fileDrop{warning: filePostureDropped(field, value, kept),
			item: importItem{ID: field, Kind: importKindPosture, Label: field + " " + value, Default: true}}
		switch field {
		case "approval":
			d.item.Gives = "Runs with this approval policy, wider than a spawn here: " + kept + " applies otherwise."
			d.keep = func(p *spawnProfileJSON) { p.Approval = recorded.Approval }
		case "tools":
			d.item.Gives = "Runs with this tool policy, wider than a spawn here: " + kept + " applies otherwise."
			d.keep = func(p *spawnProfileJSON) { p.ToolGovernance = recorded.ToolGovernance }
		default:
			d.item.ID = "auto_review"
			d.item.Gives = "Lets an automatic reviewer answer approvals: " + kept + " applies otherwise."
			d.keep = func(p *spawnProfileJSON) { p.AutoReview = recorded.AutoReview }
		}
		drops = append(drops, d)
	}
	recordedReview := recorded.AutoReview != nil && *recorded.AutoReview
	unjudged := func() (spawnProfileJSON, []fileDrop) {
		const kept = "the receiver's default"
		if recorded.Approval != "" {
			drop("approval", recorded.Approval, kept)
		}
		if recorded.ToolGovernance != "" {
			drop("tools", recorded.ToolGovernance, kept)
		}
		if recordedReview {
			drop("auto-review", "on", kept)
		}
		return p, drops
	}
	landing, fail, err := resolveLaunchDefaults(group, profile, h.Name)
	if fail != nil || err != nil {
		return unjudged()
	}
	landingApproval, _, _, fail := resolveStringLaunchField("approval", "", h.Name, landing.tiers,
		func(tier *db.SpawnProfile) string { return tier.Approval },
		func(raw string) (string, error) { return harness.ValidateApprovalPolicy(h, raw) })
	if fail != nil {
		return unjudged()
	}
	if landingApproval, err = harness.ResolveApprovalPolicy(h, landingApproval); err != nil {
		return unjudged()
	}
	landingReview, _, _, _, fail := resolveBoolLaunchField("auto_review", false, false, h.Name, landing.tiers,
		func(tier *db.SpawnProfile) *bool { return tier.AutoReview },
		func(v bool) (bool, error) { return harness.ResolveAutoReview(h, v) })
	if fail != nil {
		return unjudged()
	}
	landingTools, _, _, fail := resolveStringLaunchField("tools", "", h.Name, landing.tiers,
		func(tier *db.SpawnProfile) string { return tier.ToolGovernance },
		func(raw string) (string, error) { return harness.ValidateToolGovernance(h, raw) })
	if fail != nil {
		return unjudged()
	}
	if landingTools, err = harness.ResolveToolGovernance(h, landingTools); err != nil {
		return unjudged()
	}

	// A value equal to the landing's says nothing, and needs no judging.
	approval := landingApproval
	if recorded.Approval != "" && recorded.Approval != landingApproval {
		if harness.ApprovalLineageAllowed(h.Name, landingApproval, landingReview, h.Name, recorded.Approval, landingReview) {
			p.Approval, approval = recorded.Approval, recorded.Approval
		} else {
			drop("approval", recorded.Approval, "approval "+landingApproval)
		}
	}
	if recorded.AutoReview != nil && recordedReview != landingReview {
		if harness.ApprovalLineageAllowed(h.Name, landingApproval, landingReview, h.Name, approval, recordedReview) {
			p.AutoReview = recorded.AutoReview
		} else if recordedReview {
			drop("auto-review", "on", "auto-review off")
		}
	}
	if recorded.ToolGovernance != "" && recorded.ToolGovernance != landingTools {
		if harness.ToolGovernanceNotWider(h, landingTools, recorded.ToolGovernance) {
			p.ToolGovernance = recorded.ToolGovernance
		} else {
			kept := "the receiver's default"
			if landingTools != "" {
				kept = "tools " + landingTools
			}
			drop("tools", recorded.ToolGovernance, kept)
		}
	}
	return p, drops
}

// acceptedMoveContextKey marks an import of a teleport or move offer the
// receiver accepted by hand.
type acceptedMoveContextKey struct{}

// bundleNoLaunchContextKey marks an agent import that lands offline
// (start=false). Only handleAgentBundleImport sets it.
type bundleNoLaunchContextKey struct{}

// recordOfflineResumeProfile records where a server-authoritative conversation
// landed. Resume cannot rebuild this from the harness's store as it does
// elsewhere: a conversation in private state is in no listing the daemon can
// read. The fallback is the posture bound at enrollment, not a baseline.
func recordOfflineResumeProfile(convID, harnessName, cwd string) error {
	relaunch, err := db.AgentRelaunchProfileForConv(convID)
	if err != nil {
		return fmt.Errorf("read the landed agent's relaunch profile: %w", err)
	}
	if relaunch == nil {
		return fmt.Errorf("landed agent %s has no relaunch profile", short8(convID))
	}
	return db.SetConversationResumeProfile(convID, db.ConversationResumeProfile{
		Version: db.RelaunchProfileVersion, Harness: harnessName, Cwd: cwd, FallbackRelaunch: relaunch,
	})
}

// landSpawnOffline finishes an enrolled, history-installed spawn without
// starting a session. A conversation with no resume profile gets one when
// resume rebuilds it from the harness's own store, as for a group import
// member; the agent's relaunch profile was bound at enrollment.
func landSpawnOffline(p spawnParams, h *harness.Harness, convID string) (*spawnOutcome, *spawnFailure) {
	// Resume never pre-trusts, so keep the launch's trust_dir promise now:
	// otherwise the resumed pane stops on the trust dialog, and a Copilot API
	// drive refuses to resume at all.
	if p.TrustDir {
		if err := trustAgentDirectory(h, convID, p.Cwd); err != nil {
			slog.Warn("import: could not pre-trust the offline agent's directory", "harness", h.Name, "cwd", p.Cwd, "error", err)
		}
	}
	if p.Name != "" && isValidRenameTitle(p.Name) {
		if err := db.SetConvIndexCustomTitle(convID, p.Name, h.Name); err != nil {
			slog.Warn("import: offline landing title not recorded", "conv", short8(convID), "error", err)
		}
	}
	agentID := p.AgentID
	if agentID == "" {
		agentID, _ = db.AgentIDForConv(convID)
	}
	return &spawnOutcome{AgentID: agentID, ConvID: convID, Harness: p.Harness, Model: p.Model, Effort: p.Effort}, nil
}

// saveLandedHistory saves the whole conversation of a landing that cut it
// beside the landed agent, where morph keeps its artifacts, and tells the
// agent where it is. Best effort: the full history also stays in the file
// that was imported.
func saveLandedHistory(spawned agent.SpawnResponse, l *historyLanding, g *db.AgentGroup) {
	agentID := spawned.AgentID
	if agentID == "" {
		agentID, _ = db.AgentIDForConv(spawned.ConvID)
	}
	if agentID == "" || spawned.ConvID == "" {
		return
	}
	root, err := tclcommon.PrepareMorphArtifactsDir(agentID)
	if err != nil {
		slog.Warn("import: save full history", "agent", agentID, "error", err)
		return
	}
	dir := filepath.Join(root, "import-"+time.Now().UTC().Format("20060102T150405.000Z"))
	if err := os.Mkdir(dir, 0o700); err != nil {
		slog.Warn("import: save full history", "agent", agentID, "error", err)
		return
	}
	if err := harness.WriteHistoryArtifacts(l.full, dir); err != nil {
		slog.Warn("import: save full history", "agent", agentID, "error", err)
		return
	}
	saved := filepath.Join(dir, "transcript.md") + " (Markdown)"
	// The PAF context copy is best effort; name it only when it was written.
	if _, err := os.Lstat(filepath.Join(dir, "transcript.jsonl")); err == nil {
		saved += " and " + filepath.Join(dir, "transcript.jsonl") + " (PAF context, JSON lines)"
	}
	body := fmt.Sprintf("Your imported conversation was cut by about %d%% to fit %s's context window: the pinned start, compaction summaries and the newest turns are what you see. The whole conversation is saved at %s. Read the parts you need from there rather than assuming what was in them.",
		l.cutPercent(), landingModelLabel(mustHarness(l.Harness), l.Model), saved)
	var groupID int64
	if g != nil {
		groupID = g.ID
	}
	if _, err := queueAgentMessage(&db.AgentMessage{GroupID: groupID, ToConv: spawned.ConvID, Subject: "Your full imported history", Body: body}); err != nil {
		slog.Warn("import: tell the agent where its full history is", "agent", agentID, "error", err)
	}
}

func mustHarness(name string) *harness.Harness {
	h, err := harness.Resolve(name)
	if err != nil || h == nil {
		return &harness.Harness{Name: name, DisplayName: name}
	}
	return h
}
