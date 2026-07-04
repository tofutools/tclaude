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
	Name string `json:"name"`

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
	out := spawnProfileJSON{
		Name:                       p.Name,
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
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": p.Name})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST")
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
		p, err := db.GetSpawnProfile(name)
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
		existing, err := db.GetSpawnProfile(name)
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
		writeJSON(w, http.StatusOK, map[string]any{"id": p.ID, "name": p.Name})
	case http.MethodDelete:
		if _, ok := requirePermission(w, r, PermProfilesManage); !ok {
			return
		}
		n, err := db.DeleteSpawnProfile(name)
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
