package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Spawn profiles — named, reusable bundles of the spawn-agent dialog
// (most fields, NOT cwd / worktree), the store behind JOH-210. A profile
// pre-fills the dashboard spawn dialog, and a group's default profile is
// resolved server-side to fill blank LAUNCH fields for non-dialog spawns
// (group templates). See pkg/claude/common/db/spawn_profiles.go for the
// row shape.
//
// Wire surface (daemon Unix socket, SO_PEERCRED auth):
//
//	GET    /v1/spawn-profiles              → list profiles
//	POST   /v1/spawn-profiles              → create a profile
//	POST   /v1/spawn-profiles/from-agent   → capture a live agent's config into an unsaved seed
//	GET    /v1/spawn-profiles/export       → export selected/all profiles as a portable bundle
//	POST   /v1/spawn-profiles/import/inspect → preview a portable profile bundle
//	POST   /v1/spawn-profiles/import       → import a portable profile bundle
//	GET    /v1/spawn-profiles/{name}       → fetch one profile
//	PATCH  /v1/spawn-profiles/{name}       → replace a profile (full state)
//	DELETE /v1/spawn-profiles/{name}       → delete a profile
//
// Reads are open (introspection, like /v1/templates); mutations are gated on
// profiles.manage (effectively human-only — a profile is shared spawn config).
//
// Every field is OPTIONAL: a blank text field / absent toggle stores as unset
// (loads blank, leaves the launch default). The launch fields (harness / model
// / effort / sandbox / approval / auto_review / trust_dir) are validated
// against the PROFILE'S OWN harness at save — model/effort through that
// harness's ModelCatalog, not the Claude-only clcommon.ValidateModel — which is
// what makes a profile harness-correct and fixes CodeRabbit #343.

// spawnProfileJSON is the wire shape for a spawn profile. The five toggles are
// *bool so an absent/null field round-trips as unset (nil), distinct from an
// explicit false. CreatedAt / UpdatedAt are response-only (ignored on input).
type spawnProfileJSON struct {
	Name    string   `json:"name"`
	Aliases []string `json:"aliases,omitempty"`
	// Disabled is the authoritative spawn gate. DisabledReason is remembered
	// independently so enabling does not discard the operator's explanation.
	Disabled       *bool  `json:"disabled,omitempty"`
	DisabledReason string `json:"disabled_reason,omitempty"`

	// Launch fields — overlap clcommon.SpawnArgs.
	Harness  string `json:"harness,omitempty"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
	Sandbox  string `json:"sandbox,omitempty"`
	Approval string `json:"approval,omitempty"`
	// AskUserQuestionTimeout is the profile's Claude Code AskUserQuestion
	// idle-timeout default (inherit|never|60s|5m|10m; "" = unset), delivered
	// per-spawn via `--settings`. Claude-Code-only — a value on a Codex profile
	// is a 400 (buildProfileFromJSON gates it on the profile's harness).
	AskUserQuestionTimeout string `json:"ask_user_question_timeout,omitempty"`
	AutoReview             *bool  `json:"auto_review,omitempty"`
	TrustDir               *bool  `json:"trust_dir,omitempty"`
	// RemoteControl is the profile's "start with Claude Code Remote Access on"
	// default — tri-state (null = unset, false = off, true = on). A group's
	// remote-control policy overrides it at spawn (JOH-262).
	RemoteControl *bool `json:"remote_control,omitempty"`

	// Identity / enrollment fields (dialog-side).
	AgentName      string `json:"agent_name,omitempty"`
	Role           string `json:"role,omitempty"`
	Descr          string `json:"descr,omitempty"`
	InitialMessage string `json:"initial_message,omitempty"`

	// Dialog toggles.
	SyncWorktree               *bool `json:"sync_worktree,omitempty"`
	AutoFocus                  *bool `json:"auto_focus,omitempty"`
	IncludeGroupDefaultContext *bool `json:"include_group_default_context,omitempty"`

	// Birth-time access controls the profile pre-fills. IsOwner is
	// tri-state (null = unset). PermissionOverrides maps slug → "grant" | "deny"
	// (absent = no overrides); validated against the slug registry at save.
	IsOwner             *bool             `json:"is_owner,omitempty"`
	PermissionOverrides map[string]string `json:"permission_overrides,omitempty"`

	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// profileToJSON projects a db.SpawnProfile onto the wire shape.
func profileToJSON(p *db.SpawnProfile) spawnProfileJSON {
	disabled := p.Disabled
	out := spawnProfileJSON{
		Name:                       p.Name,
		Aliases:                    append([]string{}, p.Aliases...),
		Disabled:                   &disabled,
		DisabledReason:             p.DisabledReason,
		Harness:                    p.Harness,
		Model:                      p.Model,
		Effort:                     p.Effort,
		Sandbox:                    p.Sandbox,
		Approval:                   p.Approval,
		AskUserQuestionTimeout:     p.AskUserQuestionTimeout,
		AutoReview:                 p.AutoReview,
		TrustDir:                   p.TrustDir,
		RemoteControl:              p.RemoteControl,
		AgentName:                  p.AgentName,
		Role:                       p.Role,
		Descr:                      p.Descr,
		InitialMessage:             p.InitialMessage,
		SyncWorktree:               p.SyncWorktree,
		AutoFocus:                  p.AutoFocus,
		IncludeGroupDefaultContext: p.IncludeGroupDefaultContext,
		IsOwner:                    p.IsOwner,
		PermissionOverrides:        p.PermissionOverrides,
	}
	if !p.CreatedAt.IsZero() {
		out.CreatedAt = p.CreatedAt.Format(time.RFC3339)
	}
	if !p.UpdatedAt.IsZero() {
		out.UpdatedAt = p.UpdatedAt.Format(time.RFC3339)
	}
	return out
}

// profileInlineToJSON projects a template-local profile. Disabled state only
// belongs to saved registry profiles, so it must remain absent from this wire
// shape; buildInlineProfileFromJSON deliberately rejects the field when set.
func profileInlineToJSON(p *db.SpawnProfile) spawnProfileJSON {
	out := profileToJSON(p)
	out.Disabled = nil
	out.DisabledReason = ""
	return out
}

// collectProfilesSnapshot builds the dashboard's spawn-profile list for the
// poll (the palette dock, JOH-374). Returns an empty (non-nil) slice on error
// or when there are no profiles, so the page's JS .map() never trips on null.
// db.ListSpawnProfiles already orders by name, so the output is stable.
func collectProfilesSnapshot() []spawnProfileJSON {
	out := []spawnProfileJSON{}
	profiles, err := db.ListSpawnProfiles()
	if err != nil {
		return out
	}
	for _, p := range profiles {
		out = append(out, profileToJSON(p))
	}
	return out
}

// buildProfileFromJSON validates a wire-shape profile and converts it to a
// db.SpawnProfile. It returns a non-nil *spawnFailure (reused as a generic
// "bad request" carrier) on the first validation problem so the caller can map
// it straight onto writeError.
//
// The launch fields are validated against the profile's own harness:
// ResolveSpawnable turns a blank/explicit harness into the catalog to validate
// model + effort through (a blank harness validates against Claude, the
// default), and the sandbox / approval / auto_review / trust_dir gates reject a
// value the harness cannot take (e.g. a Codex sandbox on a Claude profile).
// Each field is optional — a blank text field / absent toggle stays unset, and
// the validators that would otherwise apply a launch-time default
// (ValidateSandboxMode / ValidateApprovalPolicy) keep blank as blank.
func buildProfileFromJSON(body spawnProfileJSON) (*db.SpawnProfile, *spawnFailure) {
	name := strings.TrimSpace(body.Name)
	if err := validateGroupName(name); err != nil {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg", "profile name: " + err.Error()}
	}
	aliases := make([]string, 0, len(body.Aliases))
	seenAliases := map[string]bool{}
	for _, raw := range body.Aliases {
		alias := strings.TrimSpace(raw)
		if err := validateGroupName(alias); err != nil {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg", "profile alias: " + err.Error()}
		}
		if alias == name {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg", fmt.Sprintf("profile alias %q duplicates the primary name", alias)}
		}
		if seenAliases[alias] {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg", fmt.Sprintf("profile alias %q appears more than once", alias)}
		}
		seenAliases[alias] = true
		aliases = append(aliases, alias)
	}
	disabledReason := normalizeProfileDisabledReason(body.DisabledReason)
	if len(disabledReason) > maxProfileDisabledReasonBytes {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg", fmt.Sprintf(
			"disabled_reason must be at most %d bytes", maxProfileDisabledReasonBytes)}
	}
	disabled := disabledReason != "" // compatibility for pre-v122 clients/files
	if body.Disabled != nil {
		disabled = *body.Disabled
	}
	if disabled && disabledReason == "" {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
			"disabled_reason is required when disabled is true"}
	}

	// Resolve the profile's harness — empty means Claude (the default), an
	// unknown/not-spawnable name is a 400. The resolved harness gives the
	// catalog the launch fields are validated against. The harness is stored
	// as the trimmed input (blank stays unset), not the resolved name.
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
	// Validate (don't default) the sandbox/approval: a blank profile field
	// stays blank so the launch boundary applies its own default at spawn time.
	sandbox, err := harness.ValidateSandboxMode(h, body.Sandbox)
	if err != nil {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_sandbox", err.Error()}
	}
	approval, err := harness.ValidateApprovalPolicy(h, body.Approval)
	if err != nil {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_approval", err.Error()}
	}
	// Validate (+ harness-gate) the AskUserQuestion timeout: a value on a
	// non-Claude profile is a 400, and a bad enum is rejected; inherit/blank
	// stays "" so the launch boundary adds no override at spawn time.
	askTimeout, err := harness.ResolveAskTimeoutMode(h, body.AskUserQuestionTimeout)
	if err != nil {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_ask_user_question_timeout", err.Error()}
	}
	if body.AutoReview != nil {
		if _, err := harness.ResolveAutoReview(h, *body.AutoReview); err != nil {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_auto_review", err.Error()}
		}
	}
	if body.TrustDir != nil {
		if _, err := harness.ResolveTrustDir(h, *body.TrustDir); err != nil {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_trust_dir", err.Error()}
		}
	}
	// A profile may default Remote Access on only for a harness that has it: a
	// remote_control=true on a Codex profile is a 400, the same gate the spawn
	// path applies (JOH-258/JOH-262). false / unset is always fine.
	if body.RemoteControl != nil {
		if _, err := harness.ResolveRemoteControl(h, *body.RemoteControl); err != nil {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_remote_control", err.Error()}
		}
	}

	// The agent_name becomes the spawned agent's display name (a /rename title
	// at spawn) — same slash/control-char rules a template agent name follows.
	agentName := strings.TrimSpace(body.AgentName)
	if agentName != "" {
		if strings.ContainsAny(agentName, "/\\") {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg", "agent_name must not contain slashes"}
		}
		for _, r := range agentName {
			if r < 0x20 || r == 0x7f {
				return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg", "agent_name must not contain control characters"}
			}
		}
	}

	im := strings.TrimSpace(body.InitialMessage)
	if !isValidInitialMessage(im) {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("initial_message must be at most %d characters; newlines and tabs "+
				"are allowed but other control characters are not", agent.MaxInitialMessageBytes)}
	}

	// Birth-time permission overrides: same registry + effect validation the
	// spawn boundary applies, so a profile can't persist an unknown slug. A
	// "default"/"" effect is dropped here too — the saved map holds only real
	// overrides.
	permOverrides, povErr := normalizeSpawnPermissionOverrides(body.PermissionOverrides)
	if povErr != "" {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_permission_overrides", povErr}
	}

	return &db.SpawnProfile{
		Name:                       name,
		Aliases:                    aliases,
		Disabled:                   disabled,
		DisabledReason:             disabledReason,
		Harness:                    hName,
		Model:                      model,
		Effort:                     effort,
		Sandbox:                    sandbox,
		Approval:                   approval,
		AskUserQuestionTimeout:     askTimeout,
		AutoReview:                 body.AutoReview,
		TrustDir:                   body.TrustDir,
		RemoteControl:              body.RemoteControl,
		AgentName:                  agentName,
		Role:                       strings.TrimSpace(body.Role),
		Descr:                      strings.TrimSpace(body.Descr),
		InitialMessage:             im,
		SyncWorktree:               body.SyncWorktree,
		AutoFocus:                  body.AutoFocus,
		IncludeGroupDefaultContext: body.IncludeGroupDefaultContext,
		IsOwner:                    body.IsOwner,
		PermissionOverrides:        permOverrides,
	}, nil
}

// buildInlineProfileFromJSON validates a template-agent's template-LOCAL spawn
// profile (profile_inline) and converts it to a name-less db.SpawnProfile for
// embedding in the template. It reuses buildProfileFromJSON's field validation
// wholesale (launch fields against the profile's own harness, permission
// overrides against the slug registry) but rejects the fields the template
// deploy path does not honour — identity fields live on the template agent row
// itself, and the spawn-dialog-only toggles have no meaning at deploy — so a
// value can never be stored and then silently ignored.
func buildInlineProfileFromJSON(body spawnProfileJSON) (*db.SpawnProfile, *spawnFailure) {
	reject := func(field string) *spawnFailure {
		return &spawnFailure{http.StatusBadRequest, "invalid_arg", fmt.Sprintf(
			"profile_inline: %q is not supported on a template-local profile — identity fields "+
				"(agent_name/role/descr/initial_message) belong on the template agent itself, and the "+
				"spawn-dialog toggles (sync_worktree/auto_focus/include_group_default_context) do not "+
				"apply to a template deploy", field)}
	}
	if strings.TrimSpace(body.Name) != "" {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
			"profile_inline: a template-local profile has no name — use spawn_profile to reference a registry profile by name"}
	}
	if body.Disabled != nil || strings.TrimSpace(body.DisabledReason) != "" {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
			"profile_inline: disabled state is only valid on a saved spawn profile"}
	}
	switch {
	case strings.TrimSpace(body.AgentName) != "":
		return nil, reject("agent_name")
	case strings.TrimSpace(body.Role) != "":
		return nil, reject("role")
	case strings.TrimSpace(body.Descr) != "":
		return nil, reject("descr")
	case strings.TrimSpace(body.InitialMessage) != "":
		return nil, reject("initial_message")
	case body.SyncWorktree != nil:
		return nil, reject("sync_worktree")
	case body.AutoFocus != nil:
		return nil, reject("auto_focus")
	case body.IncludeGroupDefaultContext != nil:
		return nil, reject("include_group_default_context")
	}
	// A placeholder name satisfies buildProfileFromJSON's name rule; the stored
	// inline profile is name-less by definition.
	body.Name = "inline"
	body.Aliases = nil
	p, fail := buildProfileFromJSON(body)
	if fail != nil {
		return nil, fail
	}
	p.Name = ""
	return p, nil
}

const maxProfileDisabledReasonBytes = 1024

func normalizeProfileDisabledReason(reason string) string {
	reason = strings.ReplaceAll(reason, "\r\n", "\n")
	reason = strings.ReplaceAll(reason, "\r", "\n")
	return strings.TrimSpace(reason)
}

func disabledProfileFailure(p *db.SpawnProfile) *spawnFailure {
	if p == nil || !p.Disabled {
		return nil
	}
	reason := strings.TrimSpace(p.DisabledReason)
	if reason == "" {
		reason = "no reason provided"
	}
	return &spawnFailure{
		Status: http.StatusConflict,
		Kind:   "profile_disabled",
		Msg:    fmt.Sprintf("spawn profile %q is disabled: %s", p.Name, reason),
	}
}

// handleSpawnProfiles dispatches the collection endpoint /v1/spawn-profiles:
// GET lists every profile (open, read-only), POST creates one (gated on
// profiles.manage).
func handleSpawnProfiles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		profiles, err := db.ListSpawnProfiles()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		out := []spawnProfileJSON{}
		for _, p := range profiles {
			out = append(out, profileToJSON(p))
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		if _, ok := requirePermission(w, r, PermProfilesManage); !ok {
			return
		}
		var body spawnProfileJSON
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		p, fail := buildProfileFromJSON(body)
		if fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
		id, err := db.CreateSpawnProfile(p)
		if errors.Is(err, db.ErrSpawnProfileNameTaken) {
			writeError(w, http.StatusConflict, "exists", err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		p.ID = id
		writeJSON(w, http.StatusCreated, map[string]any{
			"id":                         id,
			"name":                       p.Name,
			"supports_explicit_disabled": true,
			"profile":                    profileToJSON(p),
		})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST")
	}
}

// handleGlobalDefaultSpawnProfile exposes the single server-persisted default
// profile used after a group's own default. The dashboard edits the same value
// through its DB-backed preferences API; this endpoint gives the CLI an
// inspect/set/clear surface without creating a second source of truth.
func handleGlobalDefaultSpawnProfile(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		prof := globalDefaultProfile()
		name := ""
		if prof != nil {
			name = prof.Name
		}
		writeJSON(w, http.StatusOK, map[string]any{"name": name})
	case http.MethodPut:
		if _, ok := requirePermission(w, r, PermProfilesManage); !ok {
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		name := strings.TrimSpace(body.Name)
		if name == "" {
			writeError(w, http.StatusBadRequest, "invalid_arg", "profile name is required")
			return
		}
		prof, err := db.ResolveSpawnProfile(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if prof == nil {
			writeError(w, http.StatusBadRequest, "not_found", "no such profile")
			return
		}
		if err := db.SetDashboardProfileRef(dashboardDefaultProfilePrefKey, dashboardDefaultProfileIDPrefKey, prof.Name, prof.ID); err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"name": prof.Name})
	case http.MethodDelete:
		if _, ok := requirePermission(w, r, PermProfilesManage); !ok {
			return
		}
		if err := db.DeleteDashboardProfileRef(dashboardDefaultProfilePrefKey, dashboardDefaultProfileIDPrefKey); err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"name": ""})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET, PUT or DELETE")
	}
}

// handleSpawnProfileFromAgent captures a LIVE agent's observable launch config
// into an UNSAVED spawn-profile seed (JOH-393) — the reverse of the palette
// dock's spawn-from-profile drag. It never persists: the dashboard's
// drag-an-agent-onto-the-dock gesture opens the profile editor pre-filled with
// this seed, and an explicit Save is what creates the profile (via POST
// /v1/spawn-profiles). Gated on profiles.manage — a profile is shared spawn
// config, and this reads a conv's launch fields + granted permissions, the same
// human-level capture the group→template snapshot does per member.
//
// The {agent} selector resolves via agent.ResolveSelector (agent_id, conv-id,
// title / prefix). This is the profile twin of handleTemplateFromGroup's
// preview mode.
func handleSpawnProfileFromAgent(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermProfilesManage); !ok {
		return
	}
	var body struct {
		Agent string `json:"agent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	body.Agent = strings.TrimSpace(body.Agent)
	if body.Agent == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "agent is required")
		return
	}
	res, matches, err := agent.ResolveSelector(body.Agent)
	if errors.Is(err, agent.ErrAmbiguous) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":      "selector matches multiple conversations",
			"code":       "ambiguous",
			"candidates": peerEntriesFromResolved(matches),
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, seedProfileFromConv(res.ConvID))
}

const (
	profileExportFormat  = "tclaude-spawn-profiles"
	profileExportVersion = 4
)

type profileExportEnvelope struct {
	Format        string             `json:"format"`
	FormatVersion int                `json:"format_version"`
	ExportedAt    string             `json:"exported_at,omitempty"`
	Profiles      []spawnProfileJSON `json:"profiles"`
}

type profileImportInspectResult struct {
	Format        string                 `json:"format"`
	FormatVersion int                    `json:"format_version"`
	ExportedAt    string                 `json:"exported_at,omitempty"`
	Profiles      []profileImportPreview `json:"profiles"`
}

type profileImportPreview struct {
	Name        string   `json:"name"`
	Aliases     []string `json:"aliases,omitempty"`
	Exists      bool     `json:"exists"`
	Valid       bool     `json:"valid"`
	Error       string   `json:"error,omitempty"`
	DefaultName string   `json:"default_name,omitempty"`
}

type profileImportDecision struct {
	Name    string `json:"name"`
	Include *bool  `json:"include,omitempty"`
	Action  string `json:"action,omitempty"` // create | overwrite | rename | skip
	As      string `json:"as,omitempty"`
}

type profileImportRequest struct {
	Format        string                  `json:"format"`
	FormatVersion int                     `json:"format_version"`
	ExportedAt    string                  `json:"exported_at,omitempty"`
	Profiles      []spawnProfileJSON      `json:"profiles"`
	Decisions     []profileImportDecision `json:"decisions,omitempty"`
}

type profileImportResult struct {
	Imported []profileImportApplied `json:"imported"`
	Skipped  []string               `json:"skipped"`
	Warnings []string               `json:"warnings"`
}

type profileImportApplied struct {
	Source  string `json:"source"`
	Name    string `json:"name"`
	Updated bool   `json:"updated"`
}

func profileImportRequestEnvelope(req profileImportRequest) profileExportEnvelope {
	return profileExportEnvelope{
		Format:        req.Format,
		FormatVersion: req.FormatVersion,
		ExportedAt:    req.ExportedAt,
		Profiles:      req.Profiles,
	}
}

func stripProfileExportLocalFields(p spawnProfileJSON) spawnProfileJSON {
	p.CreatedAt = ""
	p.UpdatedAt = ""
	return p
}

// normalizeLegacyProfileDisabledState preserves the safety semantics of older
// bundles, where a non-empty reason itself was the disable switch. Callers pass
// the first format version that writes an independent disabled boolean.
func normalizeLegacyProfileDisabledState(profiles []spawnProfileJSON, formatVersion, explicitStateVersion int) []spawnProfileJSON {
	if formatVersion >= explicitStateVersion {
		return profiles
	}
	out := append([]spawnProfileJSON{}, profiles...)
	for i := range out {
		disabled := strings.TrimSpace(out[i].DisabledReason) != ""
		out[i].Disabled = &disabled
	}
	return out
}

func validateExplicitProfileDisabledState(profiles []spawnProfileJSON) *spawnFailure {
	for _, profile := range profiles {
		if profile.Disabled == nil {
			return &spawnFailure{http.StatusBadRequest, "invalid_format", fmt.Sprintf(
				"profile %q is missing disabled — current-format exports must carry the explicit disabled state",
				profile.Name)}
		}
	}
	return nil
}

func validateProfileEnvelope(env profileExportEnvelope) *spawnFailure {
	if strings.TrimSpace(env.Format) != profileExportFormat {
		return &spawnFailure{http.StatusBadRequest, "invalid_format", fmt.Sprintf(
			"not a tclaude spawn-profile export (format=%q, expected %q)", env.Format, profileExportFormat)}
	}
	if env.FormatVersion < 1 {
		return &spawnFailure{http.StatusBadRequest, "invalid_format",
			"missing or invalid format_version — not a valid spawn-profile export"}
	}
	if env.FormatVersion > profileExportVersion {
		return &spawnFailure{http.StatusBadRequest, "version_too_new", fmt.Sprintf(
			"this export is format_version %d, but this tclaude supports up to %d — upgrade tclaude to import it",
			env.FormatVersion, profileExportVersion)}
	}
	seen := map[string]string{}
	for i, p := range env.Profiles {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			return &spawnFailure{http.StatusBadRequest, "invalid_arg", fmt.Sprintf("profile #%d has no name", i+1)}
		}
		if owner := seen[name]; owner != "" {
			return &spawnFailure{http.StatusBadRequest, "invalid_arg", fmt.Sprintf("profile %q appears more than once in the export", name)}
		}
		seen[name] = name
		for _, rawAlias := range p.Aliases {
			alias := strings.TrimSpace(rawAlias)
			if owner := seen[alias]; owner != "" {
				return &spawnFailure{http.StatusBadRequest, "invalid_arg", fmt.Sprintf("profile handle %q is used by both %q and %q in the export", alias, owner, name)}
			}
			seen[alias] = name
		}
	}
	return nil
}

func requestedProfileExportNames(r *http.Request) []string {
	q := r.URL.Query()
	names := append([]string{}, q["name"]...)
	for _, chunk := range q["names"] {
		names = append(names, strings.Split(chunk, ",")...)
	}
	out := []string{}
	seen := map[string]bool{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// handleSpawnProfilesExport serves GET /v1/spawn-profiles/export. With no
// name= query parameters it exports every profile; otherwise it exports the
// selected names in query order. Export is open/read-only, like GET profiles.
func handleSpawnProfilesExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET")
		return
	}
	names := requestedProfileExportNames(r)
	out := []spawnProfileJSON{}
	if len(names) == 0 {
		profiles, err := db.ListSpawnProfiles()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		for _, p := range profiles {
			out = append(out, stripProfileExportLocalFields(profileToJSON(p)))
		}
	} else {
		seenProfileIDs := map[int64]bool{}
		for _, name := range names {
			p, err := db.ResolveSpawnProfile(name)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "io", err.Error())
				return
			}
			if p == nil {
				writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no such profile %q", name))
				return
			}
			if seenProfileIDs[p.ID] {
				continue
			}
			seenProfileIDs[p.ID] = true
			out = append(out, stripProfileExportLocalFields(profileToJSON(p)))
		}
	}
	writeJSON(w, http.StatusOK, profileExportEnvelope{
		Format:        profileExportFormat,
		FormatVersion: profileExportVersion,
		ExportedAt:    time.Now().UTC().Format(time.RFC3339),
		Profiles:      out,
	})
}

func nextProfileImportName(name string) string {
	base := strings.TrimSpace(name)
	if base == "" {
		base = "imported-profile"
	}
	for i := 1; ; i++ {
		candidate := base + "-copy"
		if i > 1 {
			candidate = fmt.Sprintf("%s-copy-%d", base, i)
		}
		existing, err := db.ResolveSpawnProfile(candidate)
		if err != nil {
			return candidate
		}
		if existing == nil {
			return candidate
		}
	}
}

func inspectProfileEnvelope(env profileExportEnvelope) (profileImportInspectResult, *spawnFailure) {
	if fail := validateProfileEnvelope(env); fail != nil {
		return profileImportInspectResult{}, fail
	}
	if env.FormatVersion >= 4 {
		if fail := validateExplicitProfileDisabledState(env.Profiles); fail != nil {
			return profileImportInspectResult{}, fail
		}
	}
	env.Profiles = normalizeLegacyProfileDisabledState(env.Profiles, env.FormatVersion, 4)
	res := profileImportInspectResult{
		Format:        env.Format,
		FormatVersion: env.FormatVersion,
		ExportedAt:    env.ExportedAt,
		Profiles:      []profileImportPreview{},
	}
	for _, pj := range env.Profiles {
		name := strings.TrimSpace(pj.Name)
		existing, err := db.GetSpawnProfile(name)
		if err != nil {
			return profileImportInspectResult{}, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		prev := profileImportPreview{
			Name:        name,
			Exists:      existing != nil,
			Valid:       true,
			DefaultName: name,
		}
		if existing != nil {
			prev.DefaultName = nextProfileImportName(name)
		}
		allowedID := int64(0)
		if existing != nil {
			allowedID = existing.ID
		}
		built, fail := buildProfileFromJSON(pj)
		if fail != nil {
			prev.Valid = false
			prev.Error = fail.Msg
		} else {
			prev.Aliases = append([]string{}, built.Aliases...)
		}
		if fail == nil {
			if conflict, conflictErr := profileHandleConflict(built, allowedID); conflictErr != nil {
				return profileImportInspectResult{}, &spawnFailure{http.StatusInternalServerError, "io", conflictErr.Error()}
			} else if conflict != "" {
				prev.Valid = false
				prev.Error = conflict
			}
		}
		res.Profiles = append(res.Profiles, prev)
	}
	return res, nil
}

func handleSpawnProfilesImportInspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST")
		return
	}
	if _, ok := requirePermission(w, r, PermProfilesManage); !ok {
		return
	}
	var env profileExportEnvelope
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "not valid spawn-profile JSON: "+err.Error())
		return
	}
	res, fail := inspectProfileEnvelope(env)
	if fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func includeDecision(d profileImportDecision) bool {
	return d.Include == nil || *d.Include
}

func handleSpawnProfilesImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST")
		return
	}
	if _, ok := requirePermission(w, r, PermProfilesManage); !ok {
		return
	}
	var req profileImportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "not valid spawn-profile JSON: "+err.Error())
		return
	}
	res, fail := importProfiles(profileImportRequestEnvelope(req), req.Decisions)
	if fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func importProfiles(env profileExportEnvelope, decisions []profileImportDecision) (profileImportResult, *spawnFailure) {
	if fail := validateProfileEnvelope(env); fail != nil {
		return profileImportResult{}, fail
	}
	if env.FormatVersion >= 4 {
		if fail := validateExplicitProfileDisabledState(env.Profiles); fail != nil {
			return profileImportResult{}, fail
		}
	}
	env.Profiles = normalizeLegacyProfileDisabledState(env.Profiles, env.FormatVersion, 4)
	byName := make(map[string]spawnProfileJSON, len(env.Profiles))
	for _, p := range env.Profiles {
		byName[strings.TrimSpace(p.Name)] = p
	}
	if len(decisions) == 0 {
		decisions = make([]profileImportDecision, 0, len(env.Profiles))
		for _, p := range env.Profiles {
			decisions = append(decisions, profileImportDecision{Name: p.Name, Action: "create"})
		}
	}

	result := profileImportResult{Imported: []profileImportApplied{}, Skipped: []string{}, Warnings: []string{}}
	plannedHandles := map[string]string{}
	type importPlan struct {
		source  string
		target  string
		action  string
		profile spawnProfileJSON
	}
	plans := []importPlan{}

	for _, d := range decisions {
		source := strings.TrimSpace(d.Name)
		if source == "" {
			return profileImportResult{}, &spawnFailure{http.StatusBadRequest, "invalid_arg", "import decision is missing name"}
		}
		pj, ok := byName[source]
		if !ok {
			return profileImportResult{}, &spawnFailure{http.StatusBadRequest, "invalid_arg", fmt.Sprintf("import decision references unknown profile %q", source)}
		}
		action := strings.ToLower(strings.TrimSpace(d.Action))
		if action == "" {
			action = "create"
		}
		if !includeDecision(d) || action == "skip" {
			result.Skipped = append(result.Skipped, source)
			continue
		}
		target := source
		switch action {
		case "create", "overwrite":
		case "rename":
			target = strings.TrimSpace(d.As)
			if target == "" {
				return profileImportResult{}, &spawnFailure{http.StatusBadRequest, "invalid_arg", fmt.Sprintf("profile %q: rename requires as", source)}
			}
		default:
			return profileImportResult{}, &spawnFailure{http.StatusBadRequest, "invalid_arg", fmt.Sprintf("profile %q: unsupported import action %q", source, action)}
		}
		pj = stripProfileExportLocalFields(pj)
		pj.Name = target
		if action == "rename" && len(pj.Aliases) > 0 {
			// Rename creates a second profile rather than renaming the local row.
			// Unique aliases stay with their existing owner and cannot be copied.
			pj.Aliases = nil
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"profile %q: aliases were omitted from renamed copy %q", source, target))
		}
		built, fail := buildProfileFromJSON(pj)
		if fail != nil {
			return profileImportResult{}, fail
		}
		for _, handle := range append([]string{built.Name}, built.Aliases...) {
			if prior := plannedHandles[handle]; prior != "" {
				return profileImportResult{}, &spawnFailure{http.StatusBadRequest, "invalid_arg", fmt.Sprintf("profiles %q and %q both import handle %q", prior, source, handle)}
			}
			plannedHandles[handle] = source
		}
		plans = append(plans, importPlan{source: source, target: target, action: action, profile: pj})
	}

	for _, plan := range plans {
		existing, err := db.GetSpawnProfile(plan.target)
		if err != nil {
			return profileImportResult{}, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		if existing != nil && (plan.action == "create" || plan.action == "rename") {
			return profileImportResult{}, &spawnFailure{http.StatusConflict, "exists", fmt.Sprintf(
				"a spawn profile named %q already exists — choose overwrite or rename", plan.target)}
		}
		resolved, err := db.ResolveSpawnProfile(plan.target)
		if err != nil {
			return profileImportResult{}, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		if resolved != nil && existing == nil {
			return profileImportResult{}, &spawnFailure{http.StatusConflict, "exists", fmt.Sprintf(
				"profile name %q is already an alias of %q — choose another name", plan.target, resolved.Name)}
		}
		built, fail := buildProfileFromJSON(plan.profile)
		if fail != nil {
			return profileImportResult{}, fail
		}
		allowedID := int64(0)
		if existing != nil && plan.action == "overwrite" {
			allowedID = existing.ID
		}
		if conflict, conflictErr := profileHandleConflict(built, allowedID); conflictErr != nil {
			return profileImportResult{}, &spawnFailure{http.StatusInternalServerError, "io", conflictErr.Error()}
		} else if conflict != "" {
			return profileImportResult{}, &spawnFailure{http.StatusConflict, "exists", conflict}
		}
	}

	for _, plan := range plans {
		p, fail := buildProfileFromJSON(plan.profile)
		if fail != nil {
			return profileImportResult{}, fail
		}
		existing, err := db.GetSpawnProfile(plan.target)
		if err != nil {
			return profileImportResult{}, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		switch plan.action {
		case "overwrite":
			if existing != nil {
				p.ID = existing.ID
				if err := db.UpdateSpawnProfile(p); errors.Is(err, db.ErrSpawnProfileNameTaken) {
					return profileImportResult{}, &spawnFailure{http.StatusConflict, "exists", err.Error()}
				} else if err != nil {
					return profileImportResult{}, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
				}
				result.Imported = append(result.Imported, profileImportApplied{Source: plan.source, Name: plan.target, Updated: true})
				continue
			}
		}
		if _, err := db.CreateSpawnProfile(p); errors.Is(err, db.ErrSpawnProfileNameTaken) {
			return profileImportResult{}, &spawnFailure{http.StatusConflict, "exists", err.Error()}
		} else if err != nil {
			return profileImportResult{}, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		result.Imported = append(result.Imported, profileImportApplied{Source: plan.source, Name: plan.target})
	}
	return result, nil
}

func profileHandleConflict(p *db.SpawnProfile, allowedProfileID int64) (string, error) {
	for _, handle := range append([]string{p.Name}, p.Aliases...) {
		owner, err := db.ResolveSpawnProfile(handle)
		if err != nil {
			return "", err
		}
		if owner != nil && owner.ID != allowedProfileID {
			return fmt.Sprintf("profile handle %q is already owned by %q", handle, owner.Name), nil
		}
	}
	return "", nil
}

// seedProfileFromConv traces a live conv's observable launch config + granted
// permissions into an UNSAVED spawn-profile seed. It reuses the exact per-agent
// capture snapshotGroupTemplate applies to a group's roster — traceMemberLaunch
// (harness/model/effort/sandbox, each blank when it matches the harness default)
// plus ListAgentPermissionsForConv (granted slugs → "grant" overrides) —
// projected onto the profile wire shape. Name is left blank: every field is a
// pre-fill the editor lets the human review + name before saving, never a
// stored profile.
func seedProfileFromConv(convID string) spawnProfileJSON {
	launch := traceMemberLaunch(convID)
	seed := spawnProfileJSON{
		Harness: launch.Harness,
		Model:   launch.Model,
		Effort:  launch.Effort,
		Sandbox: launch.Sandbox,
	}
	if perms, _ := db.ListAgentPermissionsForConv(convID); len(perms) > 0 {
		overrides := make(map[string]string, len(perms))
		for _, s := range perms {
			if s != "" {
				overrides[s] = "grant"
			}
		}
		if len(overrides) > 0 {
			seed.PermissionOverrides = overrides
		}
	}
	return seed
}

// handleSpawnProfileByName dispatches /v1/spawn-profiles/{name}: GET fetches one
// profile (open), PATCH replaces it wholesale, DELETE removes it.
//
// PATCH is a full replace, not a field-merge: the dashboard editor always posts
// the profile's complete desired state, so a partial merge would have no caller
// and only invite drift between the form and the stored row. The {name} path
// segment identifies the row; renaming a profile is a PATCH whose body carries
// the new name.
func handleSpawnProfileByName(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "missing profile name")
		return
	}
	switch r.Method {
	case http.MethodGet:
		p, err := db.ResolveSpawnProfile(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if p == nil {
			writeError(w, http.StatusNotFound, "not_found", "no such profile")
			return
		}
		writeJSON(w, http.StatusOK, profileToJSON(p))
	case http.MethodPatch:
		if _, ok := requirePermission(w, r, PermProfilesManage); !ok {
			return
		}
		existing, err := db.ResolveSpawnProfile(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if existing == nil {
			writeError(w, http.StatusNotFound, "not_found", "no such profile")
			return
		}
		var body spawnProfileJSON
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		p, fail := buildProfileFromJSON(body)
		if fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
		p.ID = existing.ID
		if err := db.UpdateSpawnProfile(p); errors.Is(err, db.ErrSpawnProfileNameTaken) {
			writeError(w, http.StatusConflict, "exists", err.Error())
			return
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":                         p.ID,
			"name":                       p.Name,
			"supports_explicit_disabled": true,
			"profile":                    profileToJSON(p),
		})
	case http.MethodDelete:
		if _, ok := requirePermission(w, r, PermProfilesManage); !ok {
			return
		}
		existing, err := db.ResolveSpawnProfile(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if existing == nil {
			writeError(w, http.StatusNotFound, "not_found", "no such profile")
			return
		}
		n, err := db.DeleteSpawnProfile(existing.Name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "no such profile")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET, PATCH or DELETE")
	}
}
