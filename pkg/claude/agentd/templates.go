package agentd

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Group templates — reusable blueprints for instantiating a working
// group. A template is NOT a group export: an export is a conv-bound
// snapshot of a live group (DB rows + .jsonl), whereas a template has
// no conv-ids. Instantiating one creates a fresh group and spawns one
// new agent per template-agent spec.
//
// Wire surface (daemon Unix socket, SO_PEERCRED auth):
//
//	GET    /v1/templates                       → list templates
//	POST   /v1/templates                       → create a template
//	GET    /v1/templates/{name}                → fetch one template
//	PATCH  /v1/templates/{name}                → replace a template (full state)
//	DELETE /v1/templates/{name}                → delete a template
//	POST   /v1/templates/{name}/instantiate    → create a group + spawn its team
//	POST   /v1/templates/from-group            → snapshot a live group into a template (update: re-snapshot in place)
//	GET    /v1/templates/{name}/export         → a portable, versioned envelope (JOH-341)
//	POST   /v1/templates/import                → import a portable envelope (as=/update= query knobs)
//
// Reads are open (introspection, like /v1/permissions); mutations are
// gated on templates.manage; instantiate is gated on
// templates.instantiate. Both slugs are effectively human-only by
// default — instantiate in particular spawns a whole team at once.

// templateAgentJSON is the wire shape for one agent in a template —
// used both in request bodies (the dashboard editor) and responses.
type templateAgentJSON struct {
	Name           string   `json:"name"`
	Role           string   `json:"role,omitempty"`
	Descr          string   `json:"descr,omitempty"`
	InitialMessage string   `json:"initial_message,omitempty"`
	IsOwner        bool     `json:"is_owner,omitempty"`
	Permissions    []string `json:"permissions"`

	// Per-role launch profile (JOH-239). SpawnProfile references a spawn
	// profile by name (validated to exist at save); the five inline fields are
	// per-agent launch overrides that win over the referenced profile. All
	// omitempty — an absent value = unset, and the resolver at instantiate
	// falls through: per-agent inline → referenced profile → group default →
	// harness default. "agentType" from the issue is intentionally OUT OF SCOPE
	// (the spawn substrate has no agent-type concept).
	SpawnProfile string `json:"spawn_profile,omitempty"`
	Harness      string `json:"harness,omitempty"`
	Model        string `json:"model,omitempty"`
	Effort       string `json:"effort,omitempty"`
	Sandbox      string `json:"sandbox,omitempty"`
	Approval     string `json:"approval,omitempty"`
}

// workPatternEntryJSON is the wire shape for one work-pattern step —
// a routed briefing message: send_to is a roster agent's template-name
// or "all"; value may carry {{task}}, replaced with the
// per-instantiation task at delivery.
type workPatternEntryJSON struct {
	SendTo string `json:"send_to"`
	Value  string `json:"value"`
}

// templateJSON is the wire shape for a whole template. CreatedAt /
// UpdatedAt are response-only (ignored on input).
type templateJSON struct {
	Name           string                 `json:"name"`
	Descr          string                 `json:"descr,omitempty"`
	DefaultContext string                 `json:"default_context,omitempty"`
	Agents         []templateAgentJSON    `json:"agents"`
	WorkPattern    []workPatternEntryJSON `json:"work_pattern"`
	CreatedAt      string                 `json:"created_at,omitempty"`
	UpdatedAt      string                 `json:"updated_at,omitempty"`
}

// templateToJSON projects a db.GroupTemplate onto the wire shape, with
// non-nil slices so the dashboard's JS .map() never trips on null.
func templateToJSON(t *db.GroupTemplate) templateJSON {
	out := templateJSON{
		Name:           t.Name,
		Descr:          t.Descr,
		DefaultContext: t.DefaultContext,
		Agents:         []templateAgentJSON{},
		WorkPattern:    []workPatternEntryJSON{},
	}
	for _, e := range t.WorkPattern {
		out.WorkPattern = append(out.WorkPattern, workPatternEntryJSON{SendTo: e.SendTo, Value: e.Value})
	}
	if !t.CreatedAt.IsZero() {
		out.CreatedAt = t.CreatedAt.Format(time.RFC3339)
	}
	if !t.UpdatedAt.IsZero() {
		out.UpdatedAt = t.UpdatedAt.Format(time.RFC3339)
	}
	for _, a := range t.Agents {
		perms := a.Permissions
		if perms == nil {
			perms = []string{}
		}
		out.Agents = append(out.Agents, templateAgentJSON{
			Name:           a.Name,
			Role:           a.Role,
			Descr:          a.Descr,
			InitialMessage: a.InitialMessage,
			IsOwner:        a.IsOwner,
			Permissions:    perms,
			SpawnProfile:   a.SpawnProfile,
			Harness:        a.Harness,
			Model:          a.Model,
			Effort:         a.Effort,
			Sandbox:        a.Sandbox,
			Approval:       a.Approval,
		})
	}
	return out
}

// collectTemplatesSnapshot builds the dashboard Templates tab's data.
// Returns an empty (non-nil) slice on error or when there are no
// templates, so the page's JS .map() never trips on null.
func collectTemplatesSnapshot() []templateJSON {
	out := []templateJSON{}
	templates, err := db.ListGroupTemplates()
	if err != nil {
		return out
	}
	for _, t := range templates {
		out = append(out, templateToJSON(t))
	}
	return out
}

// buildTemplateFromJSON validates a wire-shape template and converts it
// to a db.GroupTemplate. It returns a non-nil *spawnFailure (reused as
// a generic "bad request" carrier) on the first validation problem so
// the caller can map it straight onto writeError.
//
// Validation:
//   - name follows the same rules as a group name (it is the route key
//     /v1/templates/{name} and, at instantiate time, the prefix of
//     every spawned agent's name)
//   - default_context is CRLF-normalised and capped at 16 KiB
//   - each agent name is non-empty, control-char-free, slash-free
//     (the final name "<group>-<agent>" is used as a /rename title)
//     and unique within the template
//   - each agent's initial_message clears the inbox charset/length rule
//   - each permission slug is registered (catches typos early)
//   - each work-pattern step names a roster agent (or "all") and its
//     value clears the same inbox charset/length rule
//
// Multiple agents may be marked owner — a group can have several
// owners, and a from-group snapshot of a multi-owner group must
// round-trip — so there is no owner-count cap.
func buildTemplateFromJSON(body templateJSON) (*db.GroupTemplate, *spawnFailure) {
	name := strings.TrimSpace(body.Name)
	if err := validateGroupName(name); err != nil {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg", "template name: " + err.Error()}
	}
	ctx, err := normalizeGroupContext(body.DefaultContext)
	if err != nil {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg", err.Error()}
	}
	t := &db.GroupTemplate{
		Name:           name,
		Descr:          strings.TrimSpace(body.Descr),
		DefaultContext: ctx,
		Agents:         []db.GroupTemplateAgent{},
	}
	seenNames := map[string]bool{}
	for i, a := range body.Agents {
		an := strings.TrimSpace(a.Name)
		if an == "" {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("agent #%d: name is required", i+1)}
		}
		if strings.ContainsAny(an, "/\\") {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("agent %q: name must not contain slashes", an)}
		}
		for _, r := range an {
			if r < 0x20 || r == 0x7f {
				return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
					fmt.Sprintf("agent %q: name must not contain control characters", an)}
			}
		}
		if an == "all" {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				`agent name "all" is reserved — it is the work_pattern broadcast target`}
		}
		if seenNames[an] {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("duplicate agent name %q — each agent in a template needs a distinct name", an)}
		}
		seenNames[an] = true

		im := strings.TrimSpace(a.InitialMessage)
		if !isValidInitialMessage(im) {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("agent %q: initial_message must be at most %d characters; newlines and tabs "+
					"are allowed but other control characters are not", an, agent.MaxInitialMessageBytes)}
		}

		perms := []string{}
		for _, slug := range a.Permissions {
			slug = strings.TrimSpace(slug)
			if slug == "" {
				continue
			}
			if !IsKnownPermSlug(slug) {
				return nil, &spawnFailure{http.StatusBadRequest, "unknown_slug",
					fmt.Sprintf("agent %q: unknown permission slug %q. Known slugs: %s.",
						an, slug, strings.Join(knownSlugs(), ", "))}
			}
			perms = append(perms, slug)
		}

		// Per-role launch profile (JOH-239). Validate the referenced spawn
		// profile exists and the inline overrides against the harness they will
		// launch on. The validation harness mirrors the instantiate-time
		// resolution — the agent's inline harness wins, else the referenced
		// profile's harness, else the default (Claude Code) — so a value accepted
		// here is checked against the same catalog the spawn will use. Blank
		// fields stay blank (Validate*, not Resolve*): the launch boundary applies
		// its own defaults at instantiate.
		launch, fail := validateTemplateAgentLaunch(an, a)
		if fail != nil {
			return nil, fail
		}
		t.Agents = append(t.Agents, db.GroupTemplateAgent{
			Ordinal:        i,
			Name:           an,
			Role:           strings.TrimSpace(a.Role),
			Descr:          strings.TrimSpace(a.Descr),
			InitialMessage: im,
			IsOwner:        a.IsOwner,
			Permissions:    perms,
			SpawnProfile:   launch.SpawnProfile,
			Harness:        launch.Harness,
			Model:          launch.Model,
			Effort:         launch.Effort,
			Sandbox:        launch.Sandbox,
			Approval:       launch.Approval,
		})
	}

	// Work pattern (JOH-336): every step must route somewhere real and
	// clear the inbox rule its delivery will be held to. Validated AFTER
	// the roster so send_to can check the full name set. The step cap is
	// a sanity bound, far above any real choreography.
	const maxWorkPatternSteps = 32
	if len(body.WorkPattern) > maxWorkPatternSteps {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("work_pattern: at most %d steps", maxWorkPatternSteps)}
	}
	for i, e := range body.WorkPattern {
		sendTo := strings.TrimSpace(e.SendTo)
		if sendTo != "all" && !seenNames[sendTo] {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("work_pattern step #%d: send_to %q is neither \"all\" nor a template agent name", i+1, sendTo)}
		}
		val := strings.TrimSpace(e.Value)
		if val == "" {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("work_pattern step #%d: value is required", i+1)}
		}
		if !isValidInitialMessage(val) {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("work_pattern step #%d: value must be at most %d characters; newlines and tabs "+
					"are allowed but other control characters are not", i+1, agent.MaxInitialMessageBytes)}
		}
		t.WorkPattern = append(t.WorkPattern, db.WorkPatternEntry{SendTo: sendTo, Value: val})
	}
	return t, nil
}

// templateAgentLaunch is the per-role launch profile of one template agent
// (JOH-239): a by-name spawn-profile reference plus inline launch overrides.
// It is the shape validateTemplateAgentLaunch returns (blanks preserved, for
// storage) and — after resolveTemplateAgentLaunch fills the referenced profile
// + harness secure defaults — the resolved shape the instantiator threads into
// spawnParams.
type templateAgentLaunch struct {
	SpawnProfile string
	Harness      string
	Model        string
	Effort       string
	Sandbox      string
	Approval     string
}

// validateTemplateAgentLaunch validates one template agent's per-role launch
// profile at SAVE time and returns the normalized fields to store (JOH-239).
// It checks the referenced spawn profile exists and validates the inline
// overrides against the harness they will launch on — the agent's inline
// harness wins, else the referenced profile's harness, else the default (Claude
// Code) — so a value accepted here is checked against the same catalog the
// spawn will use. Blank fields stay blank (Validate*, not Resolve*): the launch
// boundary applies its own secure defaults at instantiate. Mirrors
// buildProfileFromJSON's harness-scoped validation.
func validateTemplateAgentLaunch(agentName string, a templateAgentJSON) (templateAgentLaunch, *spawnFailure) {
	profRef := strings.TrimSpace(a.SpawnProfile)
	var refProfile *db.SpawnProfile
	if profRef != "" {
		p, err := db.GetSpawnProfile(profRef)
		if err != nil {
			return templateAgentLaunch{}, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		if p == nil {
			return templateAgentLaunch{}, &spawnFailure{http.StatusBadRequest, "invalid_profile",
				fmt.Sprintf("agent %q: no spawn profile named %q", agentName, profRef)}
		}
		refProfile = p
	}
	inlineHarness := strings.TrimSpace(a.Harness)
	valHarness := inlineHarness
	if valHarness == "" && refProfile != nil {
		valHarness = refProfile.Harness
	}
	h, err := harness.ResolveSpawnable(valHarness)
	if err != nil {
		return templateAgentLaunch{}, &spawnFailure{http.StatusBadRequest, "invalid_harness",
			fmt.Sprintf("agent %q: %s", agentName, err.Error())}
	}
	model, err := h.Models.ValidateModel(strings.TrimSpace(a.Model))
	if err != nil {
		return templateAgentLaunch{}, &spawnFailure{http.StatusBadRequest, "invalid_model",
			fmt.Sprintf("agent %q: %s", agentName, err.Error())}
	}
	effort, err := h.Models.ValidateEffort(strings.TrimSpace(a.Effort))
	if err != nil {
		return templateAgentLaunch{}, &spawnFailure{http.StatusBadRequest, "invalid_effort",
			fmt.Sprintf("agent %q: %s", agentName, err.Error())}
	}
	sandbox, err := harness.ValidateSandboxMode(h, a.Sandbox)
	if err != nil {
		return templateAgentLaunch{}, &spawnFailure{http.StatusBadRequest, "invalid_sandbox",
			fmt.Sprintf("agent %q: %s", agentName, err.Error())}
	}
	approval, err := harness.ValidateApprovalPolicy(h, a.Approval)
	if err != nil {
		return templateAgentLaunch{}, &spawnFailure{http.StatusBadRequest, "invalid_approval",
			fmt.Sprintf("agent %q: %s", agentName, err.Error())}
	}
	// Store the inline harness as typed (blank stays blank so it falls through to
	// the profile at instantiate), NOT the resolved validation harness.
	return templateAgentLaunch{
		SpawnProfile: profRef,
		Harness:      inlineHarness,
		Model:        model,
		Effort:       effort,
		Sandbox:      sandbox,
		Approval:     approval,
	}, nil
}

// resolveTemplateAgentLaunch computes the effective launch fields for one
// instantiated template agent (JOH-239). Resolution order:
//
//	per-agent inline override → referenced spawn profile → harness secure default
//
// (The group-default-profile tier of the general model is empty here — a
// freshly-instantiated group carries no default profile — so the order
// collapses to those three.) It mirrors handleGroupSpawn's overlay +
// secure-default resolution: the referenced profile is inherited only when the
// spawn will run on the profile's harness (a mismatched harness skips it,
// exactly as the group-default-profile overlay does), then the chosen harness's
// secure launch defaults fill whatever is still blank and the whole shape is
// validated.
//
// cwd is the resolved instantiation cwd; it drives the Codex sandbox cwd-safety
// guard so a template can't spawn a workspace-write Codex agent at/above $HOME.
//
// Returns a typed failure (recorded per-agent by the instantiator, never fatal
// to the rest of the roster) if the referenced profile vanished or a resolved
// value is invalid for the harness. The returned Harness is the resolved
// canonical name (e.g. "claude"); SpawnProfile is left empty (already consumed).
func resolveTemplateAgentLaunch(a db.GroupTemplateAgent, cwd string) (templateAgentLaunch, *spawnFailure) {
	harnessName := strings.TrimSpace(a.Harness)
	model := strings.TrimSpace(a.Model)
	effort := strings.TrimSpace(a.Effort)
	sandbox := strings.TrimSpace(a.Sandbox)
	approval := strings.TrimSpace(a.Approval)

	if ref := strings.TrimSpace(a.SpawnProfile); ref != "" {
		prof, err := db.GetSpawnProfile(ref)
		if err != nil {
			return templateAgentLaunch{}, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		if prof == nil {
			return templateAgentLaunch{}, &spawnFailure{http.StatusBadRequest, "invalid_profile",
				fmt.Sprintf("references spawn profile %q which no longer exists", ref)}
		}
		if harnessName == "" || harnessOrDefault(harnessName) == harnessOrDefault(prof.Harness) {
			if harnessName == "" {
				harnessName = prof.Harness
			}
			if model == "" {
				model = prof.Model
			}
			if effort == "" {
				effort = prof.Effort
			}
			if sandbox == "" {
				sandbox = prof.Sandbox
			}
			if approval == "" {
				approval = prof.Approval
			}
		}
	}

	h, err := resolveSpawnHarness(harnessName)
	if err != nil {
		return templateAgentLaunch{}, &spawnFailure{http.StatusBadRequest, "invalid_harness", err.Error()}
	}
	if model, err = h.Models.ValidateModel(model); err != nil {
		return templateAgentLaunch{}, &spawnFailure{http.StatusBadRequest, "invalid_model", err.Error()}
	}
	if effort, err = h.Models.ValidateEffort(effort); err != nil {
		return templateAgentLaunch{}, &spawnFailure{http.StatusBadRequest, "invalid_effort", err.Error()}
	}
	if sandbox, err = harness.ResolveSandboxMode(h, sandbox); err != nil {
		return templateAgentLaunch{}, &spawnFailure{http.StatusBadRequest, "invalid_sandbox", err.Error()}
	}
	if approval, err = harness.ResolveApprovalPolicy(h, approval); err != nil {
		return templateAgentLaunch{}, &spawnFailure{http.StatusBadRequest, "invalid_approval", err.Error()}
	}
	// Codex sandbox cwd-safety: a writable Codex sandbox confines writes to the
	// cwd subtree, so a cwd at/above $HOME would expose ~/.tclaude / ~/.codex /
	// ~/.claude. Refuse per-agent here, mirroring handleGroupSpawn's guard.
	if home, herr := os.UserHomeDir(); herr == nil && harness.CodexSandboxCwdConflict(sandbox, cwd, home) {
		return templateAgentLaunch{}, &spawnFailure{http.StatusBadRequest, "invalid_cwd", fmt.Sprintf(
			"refusing to spawn a %s agent in %q under sandbox %q: it would expose "+
				"~/.tclaude / ~/.codex / ~/.claude to the agent's writes", h.Name, cwd, sandbox)}
	}

	return templateAgentLaunch{
		Harness:  h.Name,
		Model:    model,
		Effort:   effort,
		Sandbox:  sandbox,
		Approval: approval,
	}, nil
}

// traceMemberLaunch re-traces a live group member's OBSERVABLE launch fields
// from its most-recent session row for a from-group template snapshot (JOH-239)
// — harness, model, effort, sandbox. approval is not recorded on the session
// row (Codex-only, re-applied as the secure default at re-instantiate), so it
// is not traced. Each field is normalized through the traced harness's catalog
// and dropped to "" if it doesn't validate (e.g. the session's model DISPLAY
// alias rather than the resume-safe model_id), so a snapshot never stores a
// value that would fail at the next instantiate. A member with no session row
// (pruned) or no observable value yields all-blank — "inherit the group
// default", the pre-JOH-239 behaviour.
func traceMemberLaunch(convID string) templateAgentLaunch {
	prof, err := db.SessionLaunchProfileForConv(convID)
	if err != nil || prof == (db.SessionLaunchProfile{}) {
		return templateAgentLaunch{}
	}
	h, err := harness.ResolveSpawnable(prof.Harness)
	if err != nil {
		return templateAgentLaunch{}
	}
	out := templateAgentLaunch{}
	// Store the harness only when it differs from the default, so a plain Claude
	// member round-trips to a blank (inherit) harness rather than a noisy
	// explicit "claude" on every agent.
	if harnessOrDefault(prof.Harness) != harness.DefaultName {
		out.Harness = h.Name
	}
	if m, err := h.Models.ValidateModel(prof.ModelID); err == nil {
		out.Model = m
	}
	if e, err := h.Models.ValidateEffort(prof.Effort); err == nil {
		out.Effort = e
	}
	if s, err := harness.ValidateSandboxMode(h, prof.SandboxMode); err == nil {
		out.Sandbox = s
	}
	return out
}

// handleTemplates dispatches the collection endpoint /v1/templates:
// GET lists every template (open, read-only), POST creates one (gated
// on templates.manage).
func handleTemplates(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		templates, err := db.ListGroupTemplates()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		out := []templateJSON{}
		for _, t := range templates {
			out = append(out, templateToJSON(t))
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		if _, ok := requirePermission(w, r, PermTemplatesManage); !ok {
			return
		}
		var body templateJSON
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		t, fail := buildTemplateFromJSON(body)
		if fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
		id, err := db.CreateGroupTemplate(t)
		if errors.Is(err, db.ErrGroupTemplateNameTaken) {
			writeError(w, http.StatusConflict, "exists", err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": t.Name})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST")
	}
}

// handleTemplateByName dispatches /v1/templates/{name}: GET fetches one
// template (open), PATCH replaces it wholesale, DELETE removes it.
//
// PATCH is a full replace, not a field-merge: the dashboard editor
// always posts the template's complete desired state, so a partial
// merge would have no caller and only invite drift between the form
// and the stored rows.
func handleTemplateByName(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "missing template name")
		return
	}
	switch r.Method {
	case http.MethodGet:
		t, err := db.GetGroupTemplate(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if t == nil {
			writeError(w, http.StatusNotFound, "not_found", "no such template")
			return
		}
		writeJSON(w, http.StatusOK, templateToJSON(t))
	case http.MethodPatch:
		if _, ok := requirePermission(w, r, PermTemplatesManage); !ok {
			return
		}
		existing, err := db.GetGroupTemplate(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if existing == nil {
			writeError(w, http.StatusNotFound, "not_found", "no such template")
			return
		}
		var body templateJSON
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		t, fail := buildTemplateFromJSON(body)
		if fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
		t.ID = existing.ID
		if err := db.UpdateGroupTemplate(t); errors.Is(err, db.ErrGroupTemplateNameTaken) {
			writeError(w, http.StatusConflict, "exists", err.Error())
			return
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": t.ID, "name": t.Name})
	case http.MethodDelete:
		if _, ok := requirePermission(w, r, PermTemplatesManage); !ok {
			return
		}
		n, err := db.DeleteGroupTemplate(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "no such template")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET, PATCH or DELETE")
	}
}

// Portable export/import (JOH-341). A template's wire JSON already
// round-trips (show --json → create/edit --file), but that is an
// internal wire shape. Export/import promote it to a deliberate,
// portable interchange format: the same inner template JSON wrapped in a
// small versioned envelope so a task force can be shared with a friend, a
// coworker, or your own other machine as one file.
//
//	GET  /v1/templates/{name}/export   → the envelope (open, read-only)
//	POST /v1/templates/import          → import an envelope (templates.manage)
//
// Because the envelope wraps the SAME inner template JSON that every
// other path uses, new template fields (work_pattern JOH-336, per-role
// launch profiles JOH-239, future process/choreography specs) ride along
// automatically — the envelope is serialization, not schema, so there is
// no migration here.
const (
	// templateExportFormat tags the envelope so an import can reject an
	// unrelated JSON file with a clear error instead of a confusing
	// field-by-field validation failure.
	templateExportFormat = "tclaude-task-force"
	// templateExportVersion is the highest envelope format version this
	// build writes and can import. Bump it only on a breaking change to
	// the envelope (not the inner template — that grows fields freely).
	// Import accepts any version <= this and rejects anything newer with
	// an "upgrade tclaude" message.
	templateExportVersion = 1
)

// templateExportEnvelope is the portable file shape: a small versioned
// wrapper around the existing inner template JSON. ExportedAt is
// informational provenance only — import ignores it. The inner Template
// carries no machine-local identity (templateToJSON emits no DB id, and
// export blanks the local created_at/updated_at timestamps), so the file
// is a pure blueprint for another machine.
type templateExportEnvelope struct {
	Format        string       `json:"format"`
	FormatVersion int          `json:"format_version"`
	ExportedAt    string       `json:"exported_at,omitempty"`
	Template      templateJSON `json:"template"`
}

// handleTemplateExport serves GET /v1/templates/{name}/export: the named
// template wrapped in a portable envelope. Open + read-only, like GET
// /v1/templates/{name} — an export reveals nothing a fetch doesn't.
func handleTemplateExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET")
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "missing template name")
		return
	}
	t, err := db.GetGroupTemplate(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if t == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such template")
		return
	}
	inner := templateToJSON(t)
	// The local DB timestamps describe THIS machine's row, not the
	// blueprint — strip them so the file is portable provenance-free (the
	// envelope's exported_at carries the only meaningful timestamp).
	inner.CreatedAt = ""
	inner.UpdatedAt = ""
	writeJSON(w, http.StatusOK, templateExportEnvelope{
		Format:        templateExportFormat,
		FormatVersion: templateExportVersion,
		ExportedAt:    time.Now().UTC().Format(time.RFC3339),
		Template:      inner,
	})
}

// templateImportResult is the import response: the final stored name,
// whether an existing template was overwritten, and any degradation
// warnings (stripped profile refs / unknown permission slugs). warnings
// is always non-nil so a CLI/JS consumer can range over it safely.
type templateImportResult struct {
	Imported string   `json:"imported"`
	Updated  bool     `json:"updated"`
	Warnings []string `json:"warnings"`
}

// sanitizeImportedTemplate makes a foreign template instantiable on THIS
// machine without hard-failing on references that may not exist locally
// (JOH-341). It strips — and reports a warning for — each machine-local
// reference the target can't honour:
//
//   - a spawn-profile reference (JOH-239) naming a profile that doesn't
//     exist here: the ref is cleared, leaving the agent's inline launch
//     overrides intact, so the agent degrades to the group/harness
//     default instead of failing the whole import;
//   - a permission slug the local slug registry doesn't know: dropped
//     from that agent so buildTemplateFromJSON's strict slug check (which
//     is correct for create/edit) doesn't reject the import.
//
// Everything else (harness/model/effort/sandbox/approval) is validated
// against the same machine-independent harness catalog by
// buildTemplateFromJSON afterwards, so it stays strict. Returns the
// cleaned copy plus the ordered warning list.
func sanitizeImportedTemplate(body templateJSON) (templateJSON, []string) {
	warnings := []string{}
	agents := make([]templateAgentJSON, len(body.Agents))
	for i, a := range body.Agents {
		label := strings.TrimSpace(a.Name)
		if label == "" {
			label = fmt.Sprintf("#%d", i+1)
		}
		if ref := strings.TrimSpace(a.SpawnProfile); ref != "" {
			p, err := db.GetSpawnProfile(ref)
			if err == nil && p == nil {
				warnings = append(warnings, fmt.Sprintf(
					"agent %q: spawn profile %q does not exist here — dropped the reference; the agent will use the group/harness default",
					label, ref))
				a.SpawnProfile = ""
			}
			// A GetSpawnProfile error is left for buildTemplateFromJSON to
			// surface as a 500 — an import shouldn't silently swallow a DB fault.
		}
		if len(a.Permissions) > 0 {
			kept := make([]string, 0, len(a.Permissions))
			for _, slug := range a.Permissions {
				s := strings.TrimSpace(slug)
				if s == "" {
					continue
				}
				if !IsKnownPermSlug(s) {
					warnings = append(warnings, fmt.Sprintf(
						"agent %q: unknown permission slug %q — dropped", label, s))
					continue
				}
				kept = append(kept, s)
			}
			a.Permissions = kept
		}
		agents[i] = a
	}
	body.Agents = agents
	return body, warnings
}

// handleTemplateImport serves POST /v1/templates/import: read a portable
// envelope and store its template locally. Gated on templates.manage
// (it writes a template, exactly like create/edit).
//
// Query knobs:
//   - as=<name>   store under a different name (rename on import)
//   - update=true overwrite an existing template of that name in place
//     (reuses the wholesale-replace machinery PATCH uses); without it, a
//     name collision is a 409 so an import never clobbers silently.
//
// Portability handling: the envelope's format/version are checked first
// (a newer format_version is rejected with an upgrade message), then
// machine-local references that may be absent here are stripped + warned
// (sanitizeImportedTemplate) so the stored template stays instantiable.
func handleTemplateImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST")
		return
	}
	if _, ok := requirePermission(w, r, PermTemplatesManage); !ok {
		return
	}
	var env templateExportEnvelope
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "not valid task-force JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(env.Format) != templateExportFormat {
		writeError(w, http.StatusBadRequest, "invalid_format", fmt.Sprintf(
			"not a tclaude task-force export (format=%q, expected %q)", env.Format, templateExportFormat))
		return
	}
	if env.FormatVersion < 1 {
		writeError(w, http.StatusBadRequest, "invalid_format",
			"missing or invalid format_version — not a valid task-force export")
		return
	}
	if env.FormatVersion > templateExportVersion {
		writeError(w, http.StatusBadRequest, "version_too_new", fmt.Sprintf(
			"this export is format_version %d, but this tclaude supports up to %d — upgrade tclaude to import it",
			env.FormatVersion, templateExportVersion))
		return
	}

	body := env.Template
	if as := strings.TrimSpace(r.URL.Query().Get("as")); as != "" {
		body.Name = as
	}
	update := r.URL.Query().Get("update") == "true"

	cleaned, warnings := sanitizeImportedTemplate(body)
	t, fail := buildTemplateFromJSON(cleaned)
	if fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}

	existing, err := db.GetGroupTemplate(t.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if existing != nil {
		if !update {
			writeError(w, http.StatusConflict, "exists", fmt.Sprintf(
				"a template named %q already exists — re-import with update to overwrite it, or as=<new-name> to import under a different name",
				t.Name))
			return
		}
		// Overwrite in place: the envelope carries the full desired state,
		// so this is a wholesale replace (the PATCH contract), reusing the
		// existing row's id.
		t.ID = existing.ID
		if err := db.UpdateGroupTemplate(t); errors.Is(err, db.ErrGroupTemplateNameTaken) {
			writeError(w, http.StatusConflict, "exists", err.Error())
			return
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, templateImportResult{Imported: t.Name, Updated: true, Warnings: warnings})
		return
	}

	if _, err := db.CreateGroupTemplate(t); errors.Is(err, db.ErrGroupTemplateNameTaken) {
		// Lost a create race with a concurrent writer — surface as a plain
		// 409 (the human can retry with update).
		writeError(w, http.StatusConflict, "exists", err.Error())
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, templateImportResult{Imported: t.Name, Updated: false, Warnings: warnings})
}

// composeInstantiationContext folds the per-instantiation task/project
// text into the template's reusable boilerplate. The template context
// is rarely-changed group-wide guidance; the task is the specific
// assignment for THIS group, so it lands under a "## Task" header that
// every spawned agent sees in its startup briefing.
func composeInstantiationContext(templateContext, task string) string {
	templateContext = strings.TrimSpace(templateContext)
	task = strings.TrimSpace(task)
	switch {
	case task == "":
		return templateContext
	case templateContext == "":
		return "## Task\n\n" + task
	default:
		return templateContext + "\n\n## Task\n\n" + task
	}
}

// instantiateAgentResult is the per-agent outcome of an instantiation.
type instantiateAgentResult struct {
	Name      string   `json:"name"`       // the template agent name
	FinalName string   `json:"final_name"` // "<group>-<name>"
	ConvID    string   `json:"conv_id,omitempty"`
	Owner     bool     `json:"owner,omitempty"`
	Granted   []string `json:"granted,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// handleTemplateInstantiate creates a fresh group from a template and
// spawns its whole agent team. Gated on templates.instantiate.
//
// Body: { group_name, task, cwd?, descr? }. group_name doubles as the
// agent-name prefix — agent "PO" in the template becomes
// "<group_name>-PO". task is the multi-line assignment, folded into the
// group's default_context so every member's startup briefing carries
// it.
//
// Agents are spawned sequentially via the shared executeSpawn core. A
// per-agent spawn failure is recorded and reported but does NOT abort
// the rest: tearing half-spawned agents back down is destructive, so a
// partial team is surfaced for the human to finish or retry by hand.
func handleTemplateInstantiate(w http.ResponseWriter, r *http.Request) {
	caller, ok := requirePermission(w, r, PermTemplatesUse)
	if !ok {
		return
	}
	tmplName := r.PathValue("name")
	tmpl, err := db.GetGroupTemplate(tmplName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if tmpl == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such template")
		return
	}

	var body struct {
		GroupName string `json:"group_name"`
		Task      string `json:"task,omitempty"`
		Cwd       string `json:"cwd,omitempty"`
		Descr     string `json:"descr,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	body.GroupName = strings.TrimSpace(body.GroupName)
	if err := validateGroupName(body.GroupName); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "group_name: "+err.Error())
		return
	}
	if existing, _ := db.GetAgentGroupByName(body.GroupName); existing != nil {
		writeError(w, http.StatusConflict, "exists", "a group named "+body.GroupName+" already exists")
		return
	}
	// Existence-check the cwd with resolveSpawnCwd — the same validator
	// handleGroupSpawn uses — not resolveGroupDefaultCwd (which skips the
	// dir-exists check). executeSpawn passes cwd straight to the spawn
	// subprocess; a non-existent path there would only fail INSIDE each
	// `tclaude session new`, turning a typo into an N×30s conv-id-poll
	// timeout and an orphaned empty group. An empty cwd stays empty
	// (agents inherit the daemon's cwd, as for a plain spawn).
	cwd, err := resolveSpawnCwd(body.Cwd)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_cwd", err.Error())
		return
	}
	groupContext, err := normalizeGroupContext(composeInstantiationContext(tmpl.DefaultContext, body.Task))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}

	descr := strings.TrimSpace(body.Descr)
	if descr == "" {
		descr = "Instantiated from template " + tmpl.Name
	}
	gid, err := db.CreateAgentGroup(body.GroupName, descr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "create group: "+err.Error())
		return
	}
	// Best-effort post-create config — a failure here is logged, not
	// fatal: the group exists and the human can adjust it on the
	// dashboard. Mirrors the /v1/groups create path.
	if cwd != "" {
		if _, err := db.SetAgentGroupDefaultCwd(body.GroupName, cwd); err != nil {
			slog.Warn("instantiate: set default cwd failed", "group", body.GroupName, "error", err)
		}
	}
	if groupContext != "" {
		if _, err := db.SetAgentGroupDefaultContext(body.GroupName, groupContext); err != nil {
			slog.Warn("instantiate: set default context failed", "group", body.GroupName, "error", err)
		}
	}

	g := &db.AgentGroup{ID: gid, Name: body.GroupName, Descr: descr, DefaultCwd: cwd, DefaultContext: groupContext}
	granter := granterLabel(caller)

	results := []instantiateAgentResult{}
	spawned, failed := 0, 0
	// Successful spawns by template-agent name (and in spawn order) —
	// the routing table for the work-pattern deliveries below.
	spawnedConvs := map[string]string{}
	spawnedOrder := []string{}
	for _, a := range tmpl.Agents {
		finalName := body.GroupName + "-" + a.Name
		res := instantiateAgentResult{Name: a.Name, FinalName: finalName}
		// Resolve this role's launch profile (JOH-239): per-agent inline
		// override → referenced spawn profile → harness secure default. A
		// resolution failure (a referenced profile deleted since save, an invalid
		// value, a Codex sandbox/cwd conflict) is recorded per-agent and skips
		// just this spawn — same best-effort contract as an owner/permission
		// grant failure below.
		launch, lfail := resolveTemplateAgentLaunch(a, cwd)
		if lfail != nil {
			res.Error = lfail.Msg
			failed++
			results = append(results, res)
			continue
		}
		outcome, fail := executeSpawn(g, spawnParams{
			Name:           finalName,
			Role:           a.Role,
			Descr:          a.Descr,
			InitialMessage: a.InitialMessage,
			Cwd:            cwd,
			Harness:        launch.Harness,
			Model:          launch.Model,
			Effort:         launch.Effort,
			SandboxMode:    launch.Sandbox,
			ApprovalPolicy: launch.Approval,
			GroupContext:   groupContext,
			ReplyToConv:    caller,
			SpawnedByConv:  caller,
		})
		if fail != nil {
			res.Error = fail.Msg
			failed++
			results = append(results, res)
			continue
		}
		res.ConvID = outcome.ConvID
		spawned++
		spawnedConvs[a.Name] = outcome.ConvID
		spawnedOrder = append(spawnedOrder, outcome.ConvID)

		// Ownership + permission grants — best-effort. The agent is
		// already spawned and group-joined; a failed grant is logged and
		// surfaced in the result note but does not fail the whole
		// instantiation.
		if a.IsOwner {
			if err := db.AddAgentGroupOwner(gid, outcome.ConvID, granter); err != nil {
				slog.Warn("instantiate: grant owner failed",
					"group", body.GroupName, "conv", outcome.ConvID, "error", err)
				res.Error = "spawned, but grant-owner failed: " + err.Error()
			} else {
				res.Owner = true
			}
		}
		for _, slug := range a.Permissions {
			if err := db.GrantAgentPermission(outcome.ConvID, slug, granter); err != nil {
				slog.Warn("instantiate: grant permission failed",
					"conv", outcome.ConvID, "slug", slug, "error", err)
				continue
			}
			res.Granted = append(res.Granted, slug)
		}
		results = append(results, res)
	}

	// Work pattern (JOH-336): with the whole roster up, deliver the
	// template's routed briefing messages IN ORDER — each step to one
	// roster agent by name, or to every spawned member ("all"). {{task}}
	// interpolates the per-instantiation task. Distinct from the per-agent
	// initial_message (that rode each agent's own spawn welcome): the
	// pattern is the cross-cutting kick-off choreography — "brief the Lead
	// with the leadership frame, then everyone with the house rules".
	// Best-effort like the ownership/permission grants: a step whose
	// target failed to spawn (or whose interpolated body breaks the inbox
	// rule) is reported in pattern_errors, never aborts the rest.
	patternDelivered := 0
	patternErrors := []string{}
	// The task is interpolated into inbox bodies, so it gets the same
	// CRLF→LF fold the group context got via normalizeGroupContext — a
	// CRLF-authored --task file must not flunk every {{task}} step's
	// charset re-gate below (isValidInitialMessage rejects '\r').
	task := strings.TrimSpace(body.Task)
	task = strings.ReplaceAll(task, "\r\n", "\n")
	task = strings.ReplaceAll(task, "\r", "\n")
	rosterNames := map[string]bool{}
	for _, a := range tmpl.Agents {
		rosterNames[a.Name] = true
	}
	for i, e := range tmpl.WorkPattern {
		msg := strings.ReplaceAll(e.Value, "{{task}}", task)
		if msg == "" {
			// A bare "{{task}}" step with no task: save-time validation
			// can't catch it, so report it instead of delivering an
			// empty-bodied briefing.
			patternErrors = append(patternErrors,
				fmt.Sprintf("step %d/%d (to %s): interpolated to an empty message — not sent",
					i+1, len(tmpl.WorkPattern), e.SendTo))
			continue
		}
		if !isValidInitialMessage(msg) {
			patternErrors = append(patternErrors,
				fmt.Sprintf("step %d/%d (to %s): interpolated message breaks the inbox charset/length rule — not sent",
					i+1, len(tmpl.WorkPattern), e.SendTo))
			continue
		}
		var targets []string
		switch e.SendTo {
		case "all":
			targets = spawnedOrder
			if len(targets) == 0 {
				patternErrors = append(patternErrors,
					fmt.Sprintf("step %d/%d: no members spawned — not sent", i+1, len(tmpl.WorkPattern)))
				continue
			}
		default:
			conv, ok := spawnedConvs[e.SendTo]
			if !ok {
				// Distinguish a roster agent that failed to spawn from a
				// target the roster no longer carries at all (a from-group
				// re-snapshot keeps the curated pattern verbatim, so a step
				// can go stale when its agent's name wasn't recovered).
				if rosterNames[e.SendTo] {
					patternErrors = append(patternErrors,
						fmt.Sprintf("step %d/%d: target %q did not spawn — not sent", i+1, len(tmpl.WorkPattern), e.SendTo))
				} else {
					patternErrors = append(patternErrors,
						fmt.Sprintf("step %d/%d: target %q is not in the roster (stale work-pattern step?) — not sent",
							i+1, len(tmpl.WorkPattern), e.SendTo))
				}
				continue
			}
			targets = []string{conv}
		}
		subject := fmt.Sprintf("[work-pattern %d/%d] %s", i+1, len(tmpl.WorkPattern), tmpl.Name)
		for _, conv := range targets {
			if _, err := db.InsertAgentMessage(&db.AgentMessage{
				GroupID:  gid,
				FromConv: caller,
				ToConv:   conv,
				Subject:  subject,
				Body:     msg,
				// The full audience on every row — like handleMultiRecipient —
				// so `inbox read` renders an "all" step as one broadcast, not
				// as N private notes.
				ToRecipients: targets,
			}); err != nil {
				slog.Warn("instantiate: work-pattern insert failed",
					"group", body.GroupName, "step", i+1, "conv", conv, "error", err)
				patternErrors = append(patternErrors,
					fmt.Sprintf("step %d/%d (to %s): %v", i+1, len(tmpl.WorkPattern), e.SendTo, err))
				continue
			}
			patternDelivered++
			enqueueDeliveryForConv(conv)
		}
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"group":             body.GroupName,
		"template":          tmpl.Name,
		"agents":            results,
		"spawned":           spawned,
		"failed":            failed,
		"pattern_delivered": patternDelivered,
		"pattern_errors":    patternErrors,
	})
}

// handleTemplateFromGroup snapshots a live group's structure into a
// template — the reverse direction of instantiate. Gated on
// templates.manage. Body: { group, template_name, update }.
//
// It carries over the group's descr + default_context and one template
// agent per group member (role, descr, owner flag, the member's
// per-conv permission grants). It does NOT carry per-agent task briefs:
// a live group has no stored "initial message" per member, so
// initial_message comes through blank for the human to fill in the
// editor afterwards.
//
// A taken template name is a hard 409 unless `update` is set, which
// re-snapshots the (possibly evolved) group into the existing template
// IN PLACE (JOH-337): the roster, owner flags, permissions and context
// are re-traced from the group, while curated per-agent briefs survive
// for roster agents that match an existing template agent by name —
// members titled "<group>-<name>" (instantiate's own naming) round-trip
// back to their template-agent <name>. With `update` set and no such
// template, it is simply created. The update response reports the
// roster diff (briefs_kept / added / removed).
func handleTemplateFromGroup(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermTemplatesManage); !ok {
		return
	}
	var body struct {
		Group        string `json:"group"`
		TemplateName string `json:"template_name"`
		Update       bool   `json:"update"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	body.Group = strings.TrimSpace(body.Group)
	body.TemplateName = strings.TrimSpace(body.TemplateName)
	if err := validateGroupName(body.TemplateName); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "template_name: "+err.Error())
		return
	}
	g, err := db.GetAgentGroupByName(body.Group)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if g == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such group "+body.Group)
		return
	}
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	ownerSet := map[string]bool{}
	if owners, err := db.ListAgentGroupOwners(g.ID); err == nil {
		for _, o := range owners {
			ownerSet[o.ConvID] = true
		}
	}

	// Resolving `existing` and writing the template are separate DB
	// round-trips, and with update set the contract is create-or-update —
	// a concurrent create/delete in that window must not surface as a
	// spurious 409/500. Losing a create race re-resolves and updates in
	// place; losing the template under an update re-resolves and creates.
	// One retry is enough: the second pass starts from freshly observed
	// state, and a second interleaved mutation falls through to the plain
	// conflict/error paths.
	for attempt := 0; ; attempt++ {
		existing, err := db.GetGroupTemplate(body.TemplateName)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if existing != nil && !body.Update {
			writeError(w, http.StatusConflict, "exists",
				"a template named "+body.TemplateName+" already exists (set update to re-snapshot it in place)")
			return
		}

		t := snapshotGroupTemplate(body.TemplateName, g, members, ownerSet, existing)

		if existing == nil {
			id, err := db.CreateGroupTemplate(t)
			if errors.Is(err, db.ErrGroupTemplateNameTaken) {
				if body.Update && attempt == 0 {
					continue // lost a create race — re-resolve and update in place
				}
				writeError(w, http.StatusConflict, "exists", err.Error())
				return
			}
			if err != nil {
				writeError(w, http.StatusInternalServerError, "io", err.Error())
				return
			}
			t.ID = id
			writeJSON(w, http.StatusCreated, templateToJSON(t))
			return
		}

		// Update in place. Curated per-agent briefs survive where the fresh
		// roster matches an existing template agent by name (a from-group
		// snapshot itself never sets briefs), and a curated descr/context is
		// never clobbered by a blank one from the group.
		prevByName := map[string]db.GroupTemplateAgent{}
		prevOrder := []string{}
		for _, a := range existing.Agents {
			prevByName[a.Name] = a
			prevOrder = append(prevOrder, a.Name)
		}
		briefsKept, added := []string{}, []string{}
		newNames := map[string]bool{}
		for i := range t.Agents {
			newNames[t.Agents[i].Name] = true
			prev, ok := prevByName[t.Agents[i].Name]
			if !ok {
				added = append(added, t.Agents[i].Name)
				continue
			}
			if prev.InitialMessage != "" {
				t.Agents[i].InitialMessage = prev.InitialMessage
				briefsKept = append(briefsKept, t.Agents[i].Name)
			}
			// The spawn-profile REFERENCE is blueprint curation, not an observable
			// launch field — a live member records its resolved model/effort/harness
			// (re-traced above) but not "which profile it was launched from". So an
			// update re-snapshot preserves a curated profile ref on name-match,
			// exactly like the brief (JOH-239). The inline overrides, being
			// observable, are left as re-traced (the live group wins).
			if prev.SpawnProfile != "" {
				t.Agents[i].SpawnProfile = prev.SpawnProfile
			}
		}
		removed := []string{}
		for _, n := range prevOrder {
			if !newNames[n] {
				removed = append(removed, n)
			}
		}
		// Descr describes the BLUEPRINT, not the instance — and instantiate
		// stamps groups with "Instantiated from template <name>", so pulling
		// the group's descr would clobber curated copy on every round-trip.
		// The existing template's descr wins unless it's blank. Context is
		// the opposite: it genuinely evolves in the live group (that's a key
		// thing a re-snapshot recaptures), so the group's wins unless blank.
		if existing.Descr != "" {
			t.Descr = existing.Descr
		}
		if t.DefaultContext == "" {
			t.DefaultContext = existing.DefaultContext
		}
		// A live group has no work pattern to trace — the pattern is
		// blueprint choreography (JOH-336), curated in the editor like the
		// briefs, so an update re-snapshot always keeps the existing one.
		t.WorkPattern = existing.WorkPattern
		t.ID = existing.ID
		if err := db.UpdateGroupTemplate(t); err != nil {
			if errors.Is(err, sql.ErrNoRows) && attempt == 0 {
				continue // template deleted underfoot — re-resolve and create
			}
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, fromGroupUpdateJSON{
			templateJSON: templateToJSON(t),
			Updated:      true,
			BriefsKept:   briefsKept,
			Added:        added,
			Removed:      removed,
		})
		return
	}
}

// snapshotGroupTemplate builds the template a from-group snapshot of
// this roster would store: one agent per group member, pure owners
// appended, descr/context taken from the group verbatim (the update
// path re-merges those against the existing template afterwards). In
// update mode (existing != nil) member names are recovered against the
// existing template via recoverTemplateAgentName so the re-snapshot
// round-trips.
func snapshotGroupTemplate(name string, g *db.AgentGroup, members []*db.AgentGroupMember, ownerSet map[string]bool, existing *db.GroupTemplate) *db.GroupTemplate {
	t := &db.GroupTemplate{
		Name:           name,
		Descr:          g.Descr,
		DefaultContext: g.DefaultContext,
		Agents:         []db.GroupTemplateAgent{},
	}
	existingNames := map[string]bool{}
	if existing != nil {
		for _, a := range existing.Agents {
			existingNames[a.Name] = true
		}
	}
	memberSet := map[string]bool{}
	// "all" is the work_pattern broadcast target (and rejected as an
	// agent name at create/PATCH) — pre-claiming it makes the derive
	// fallback disambiguate a member literally titled "all" instead of
	// snapshotting an unroutable roster name.
	usedNames := map[string]bool{"all": true}
	addAgent := func(convID, role, descr string, owner bool) {
		name := ""
		if existing != nil {
			name = recoverTemplateAgentName(convID, g.Name, usedNames, existingNames)
		}
		if name == "" {
			name = deriveTemplateAgentName(convID, role, len(t.Agents)+1, usedNames)
		}
		perms, _ := db.ListAgentPermissionsForConv(convID)
		if perms == nil {
			perms = []string{}
		}
		// Re-trace the member's OBSERVABLE launch fields (JOH-239) so a round-trip
		// preserves each role's launch shape. The spawn-profile REFERENCE is
		// blueprint curation, not observable — it is preserved by name-match in the
		// update path (handleTemplateFromGroup), like the per-agent brief.
		launch := traceMemberLaunch(convID)
		t.Agents = append(t.Agents, db.GroupTemplateAgent{
			Ordinal:     len(t.Agents),
			Name:        name,
			Role:        role,
			Descr:       descr,
			IsOwner:     owner,
			Permissions: perms,
			Harness:     launch.Harness,
			Model:       launch.Model,
			Effort:      launch.Effort,
			Sandbox:     launch.Sandbox,
		})
	}
	for _, m := range members {
		memberSet[m.ConvID] = true
		addAgent(m.ConvID, m.Role, m.Descr, ownerSet[m.ConvID])
	}
	// Pure owners — owners that aren't members — still belong in the
	// snapshot so the template's owner isn't silently dropped. Collect
	// and sort them so the resulting ordinals are reproducible across
	// two snapshots of the same group (a bare map range is unordered).
	pureOwners := []string{}
	for ownerConv := range ownerSet {
		if !memberSet[ownerConv] {
			pureOwners = append(pureOwners, ownerConv)
		}
	}
	sort.Strings(pureOwners)
	for _, ownerConv := range pureOwners {
		addAgent(ownerConv, "owner", "", true)
	}
	return t
}

// fromGroupUpdateJSON is the update-mode from-group response: the fresh
// template plus a roster-diff report. templateJSON embeds flat, so
// callers that only know the create shape (the dashboard's editor-open
// path, older CLIs) keep working unchanged.
type fromGroupUpdateJSON struct {
	templateJSON
	Updated    bool     `json:"updated"`
	BriefsKept []string `json:"briefs_kept"`
	Added      []string `json:"added"`
	Removed    []string `json:"removed"`
}

// recoverTemplateAgentName maps a live member back to an agent of the
// existing template during an update re-snapshot: a member titled
// "<group>-<name>" (what instantiate names its spawns) — or exactly
// "<name>" — for a template agent <name> keeps that name. Returns ""
// when the member matches no existing template agent (or the name was
// already claimed), letting the caller fall back to deriveTemplateAgentName.
//
// Titles are agent-controlled (self.rename is default-granted), so this
// matching is deliberately content-integrity only: a member squatting on
// another's title can at most inherit that agent's curated BRIEF slot in
// the blueprint. Owner flags and permissions are always re-traced from
// the live conv, the re-snapshot itself is human-initiated, and the
// roster diff in the response makes a hijacked slot visible.
func recoverTemplateAgentName(convID, groupName string, used, existingNames map[string]bool) string {
	title := sanitizeAgentName(agent.FreshTitle(convID))
	if title == "" {
		return ""
	}
	candidates := []string{}
	if stripped, ok := strings.CutPrefix(title, groupName+"-"); ok {
		candidates = append(candidates, stripped)
	}
	candidates = append(candidates, title)
	for _, c := range candidates {
		if c != "" && existingNames[c] && !used[c] {
			used[c] = true
			return c
		}
	}
	return ""
}

// deriveTemplateAgentName picks a template-agent name when snapshotting
// a live group: the member's conversation title, sanitised into a
// slug-ish handle (the name becomes part of a /rename title at
// instantiate time). Falls back to the role, then to "agent-<n>", and
// disambiguates collisions with a numeric suffix. The human edits the
// template afterwards anyway, so this only needs to be a sensible
// starting point.
func deriveTemplateAgentName(convID, role string, ordinal int, used map[string]bool) string {
	base := sanitizeAgentName(agent.FreshTitle(convID))
	if base == "" {
		base = sanitizeAgentName(role)
	}
	if base == "" {
		base = fmt.Sprintf("agent-%d", ordinal)
	}
	name := base
	for i := 2; used[name]; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	used[name] = true
	return name
}

// sanitizeAgentName reduces an arbitrary title to a template-agent
// name: runs of non-[A-Za-z0-9._-] characters collapse to a single
// dash, and leading/trailing dashes are trimmed.
func sanitizeAgentName(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
		if ok {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteRune('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
