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
//	GET    /v1/spawn-profiles          → list profiles
//	POST   /v1/spawn-profiles          → create a profile
//	GET    /v1/spawn-profiles/{name}   → fetch one profile
//	PATCH  /v1/spawn-profiles/{name}   → replace a profile (full state)
//	DELETE /v1/spawn-profiles/{name}   → delete a profile
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
	Harness    string `json:"harness,omitempty"`
	Model      string `json:"model,omitempty"`
	Effort     string `json:"effort,omitempty"`
	Sandbox    string `json:"sandbox,omitempty"`
	Approval   string `json:"approval,omitempty"`
	AutoReview *bool  `json:"auto_review,omitempty"`
	TrustDir   *bool  `json:"trust_dir,omitempty"`

	// Identity / enrollment fields (dialog-side).
	AgentName      string `json:"agent_name,omitempty"`
	Role           string `json:"role,omitempty"`
	Descr          string `json:"descr,omitempty"`
	InitialMessage string `json:"initial_message,omitempty"`

	// Dialog toggles.
	SyncWorktree               *bool `json:"sync_worktree,omitempty"`
	AutoFocus                  *bool `json:"auto_focus,omitempty"`
	IncludeGroupDefaultContext *bool `json:"include_group_default_context,omitempty"`

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
		AutoReview:                 p.AutoReview,
		TrustDir:                   p.TrustDir,
		AgentName:                  p.AgentName,
		Role:                       p.Role,
		Descr:                      p.Descr,
		InitialMessage:             p.InitialMessage,
		SyncWorktree:               p.SyncWorktree,
		AutoFocus:                  p.AutoFocus,
		IncludeGroupDefaultContext: p.IncludeGroupDefaultContext,
	}
	if !p.CreatedAt.IsZero() {
		out.CreatedAt = p.CreatedAt.Format(time.RFC3339)
	}
	if !p.UpdatedAt.IsZero() {
		out.UpdatedAt = p.UpdatedAt.Format(time.RFC3339)
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

	return &db.SpawnProfile{
		Name:                       name,
		Harness:                    hName,
		Model:                      model,
		Effort:                     effort,
		Sandbox:                    sandbox,
		Approval:                   approval,
		AutoReview:                 body.AutoReview,
		TrustDir:                   body.TrustDir,
		AgentName:                  agentName,
		Role:                       strings.TrimSpace(body.Role),
		Descr:                      strings.TrimSpace(body.Descr),
		InitialMessage:             im,
		SyncWorktree:               body.SyncWorktree,
		AutoFocus:                  body.AutoFocus,
		IncludeGroupDefaultContext: body.IncludeGroupDefaultContext,
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
