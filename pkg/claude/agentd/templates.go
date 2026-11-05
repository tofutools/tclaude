package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/cronexpr"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
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
//	POST   /v1/templates/{name}/reinforce      → deploy the roster INTO an existing group (JOH-376)
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
	Name           string               `json:"name"`
	Role           string               `json:"role,omitempty"`
	Descr          string               `json:"descr,omitempty"`
	InitialMessage string               `json:"initial_message,omitempty"`
	IsOwner        bool                 `json:"is_owner,omitempty"`
	Permissions    []db.PermissionGrant `json:"permissions"`

	// Role is a by-name reference to a role in the role library (JOH-240): the
	// agent inherits that role's behavioral brief and permission grants. Launch
	// shape remains independent. Empty = no role. Named
	// "role_ref" on the wire to stay distinct from the free-text Role display
	// label above.
	RoleRef string `json:"role_ref,omitempty"`

	// Per-role launch profile (JOH-239). SpawnProfile references a spawn
	// profile by name (validated to exist at save); the inline fields are
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

	// ProfileInline is the agent's template-LOCAL spawn profile: the full
	// spawn-profile shape (launch fields, ask-timeout, trust_dir / auto_review /
	// remote_control toggles, birth-time owner + permission overrides) embedded
	// in the template instead of referenced from the registry. It gives one
	// roster agent a bespoke launch config without polluting the shared profile
	// library, and travels with the template on export/import. Identity fields
	// (name/agent_name/role/descr/initial_message) and the spawn-dialog-only
	// toggles are rejected at save — those live on the template agent itself.
	// Resolution: the inline fields above → profile_inline → spawn_profile.
	ProfileInline *spawnProfileJSON `json:"profile_inline,omitempty"`

	// Wave is the agent's staged-spawn wave (JOH-244), default 0. Waves spawn
	// in ascending order; a template whose every agent is wave 0 spawns in one
	// synchronous pass (today's behaviour). The party's marching order.
	Wave int `json:"wave,omitempty"`

	// EffectiveIsOwner is DERIVED, read-only output: the owner bit a deploy of
	// this agent would grant right now, composed across the tiers the deploy
	// walks (referenced profile's owner default → profile_inline tri-state →
	// the legacy is_owner flag). Emitted so thin clients (the CLI's owner
	// column) don't have to re-resolve registry refs; ignored on save/import
	// (it re-derives from the stored fields), and omitted when false so
	// exports stay lean.
	EffectiveIsOwner bool `json:"effective_is_owner,omitempty"`
}

// rhythmJSON is the wire shape for one template rhythm (JOH-244): a recurring
// nudge materialized at deploy as a cron job on the group. Exactly one of
// interval / cron_expr sets the schedule; target_role filters to matching
// members at fire time ("" / "all" = whole group). The party's drumbeats.
type rhythmJSON struct {
	Name       string `json:"name"`
	TargetRole string `json:"target_role,omitempty"`
	Interval   string `json:"interval,omitempty"`  // Go duration ("10m") — mutually exclusive with cron_expr
	CronExpr   string `json:"cron_expr,omitempty"` // cronexpr expression — mutually exclusive with interval
	Subject    string `json:"subject,omitempty"`
	Body       string `json:"body"`
}

// workPatternEntryJSON is the wire shape for one work-pattern step —
// a routed briefing message: send_to is a roster agent's template-name
// or "all"; value may carry {{task}}, replaced with the
// per-instantiation task at delivery.
type workPatternEntryJSON struct {
	SendTo string `json:"send_to"`
	Value  string `json:"value"`
}

// processPhaseJSON is the wire shape for one process phase (JOH-242): an
// ordered chapter of the group's work. name is the phase handle (unique
// case-insensitively); roles are the role labels active in the phase (matched
// case-insensitively against a member's role, the same rule work-pattern
// --role routing uses; the literal "all" means every member); criteria is free
// prose (entry / exit / handoff in words — no DSL).
type processPhaseJSON struct {
	Name     string   `json:"name"`
	Roles    []string `json:"roles"`
	Criteria string   `json:"criteria,omitempty"`
}

// templateJSON is the wire shape for a whole template. CreatedAt /
// UpdatedAt are response-only (ignored on input).
type templateJSON struct {
	Name           string `json:"name"`
	Descr          string `json:"descr,omitempty"`
	DefaultContext string `json:"default_context,omitempty"`
	// PerAgentWorktrees seeds the deploy dialog's existing per-run checkbox.
	// It is a default, not a mandate: the human may toggle it for each spawn.
	PerAgentWorktrees bool                   `json:"per_agent_worktrees,omitempty"`
	Agents            []templateAgentJSON    `json:"agents"`
	WorkPattern       []workPatternEntryJSON `json:"work_pattern"`
	// Process is the template's declarative process spec (JOH-242): an ordered
	// list of phases. Empty/absent = no process (the feature is off).
	Process []processPhaseJSON `json:"process"`
	// Rhythms is the template's recurring-nudge declarations (JOH-244),
	// materialized as group cron jobs at deploy. Empty/absent = no rhythms.
	Rhythms []rhythmJSON `json:"rhythms"`
	// WaveMaxWait caps (seconds) how long each staged-spawn wave waits for the
	// prior wave to go idle before the next spawns anyway (JOH-244). 0 =
	// built-in default.
	WaveMaxWait int `json:"wave_max_wait,omitempty"`
	// OwnerScopes is the owner-scope map (TCL-1071) stamped onto the group
	// this template instantiates: slug → the scope that slug's owner-implied
	// bypass is confined to there. Empty/absent = the unrestricted bypass. It
	// lets a blueprint declare "a lead of a force deployed from me may spawn
	// only with the reviewer profile" once, instead of it being re-applied by
	// hand after every deploy.
	OwnerScopes map[string]PermissionScope `json:"owner_scopes,omitempty"`
	CreatedAt   string                     `json:"created_at,omitempty"`
	UpdatedAt   string                     `json:"updated_at,omitempty"`
}

// templateToJSON projects a db.GroupTemplate onto the wire shape, with
// non-nil slices so the dashboard's JS .map() never trips on null.
func templateToJSON(t *db.GroupTemplate) templateJSON {
	out := templateJSON{
		Name:              t.Name,
		Descr:             t.Descr,
		DefaultContext:    t.DefaultContext,
		PerAgentWorktrees: t.PerAgentWorktrees,
		Agents:            []templateAgentJSON{},
		WorkPattern:       []workPatternEntryJSON{},
		Process:           []processPhaseJSON{},
		Rhythms:           []rhythmJSON{},
		WaveMaxWait:       t.WaveMaxWait,
		OwnerScopes:       ownerScopesWireOmitEmpty(t.OwnerScopesJSON),
	}
	for _, e := range t.WorkPattern {
		out.WorkPattern = append(out.WorkPattern, workPatternEntryJSON{SendTo: e.SendTo, Value: e.Value})
	}
	for _, ph := range t.Process {
		roles := ph.Roles
		if roles == nil {
			roles = []string{}
		}
		out.Process = append(out.Process, processPhaseJSON{Name: ph.Name, Roles: roles, Criteria: ph.Criteria})
	}
	for _, rh := range t.Rhythms {
		out.Rhythms = append(out.Rhythms, rhythmToJSON(rh))
	}
	if !t.CreatedAt.IsZero() {
		out.CreatedAt = t.CreatedAt.Format(time.RFC3339)
	}
	if !t.UpdatedAt.IsZero() {
		out.UpdatedAt = t.UpdatedAt.Format(time.RFC3339)
	}
	// Batch the registry reads behind the derived effective_is_owner bits: one
	// lazy ListSpawnProfiles per template (and none for ref-free rosters)
	// instead of a GetSpawnProfile per roster agent — templateToJSON runs for
	// every template on every recurring dashboard poll.
	var profByName map[string]*db.SpawnProfile
	lookupProfile := func(name string) *db.SpawnProfile {
		if profByName == nil {
			profByName = map[string]*db.SpawnProfile{}
			if all, err := db.ListSpawnProfiles(); err == nil {
				for _, p := range all {
					profByName[p.Name] = p
				}
			}
		}
		return profByName[name]
	}
	for _, a := range t.Agents {
		perms := a.Permissions
		if perms == nil {
			perms = []db.PermissionGrant{}
		}
		aj := templateAgentJSON{
			Name:             a.Name,
			Role:             a.Role,
			Descr:            a.Descr,
			InitialMessage:   a.InitialMessage,
			IsOwner:          a.IsOwner,
			Permissions:      perms,
			RoleRef:          a.RoleRef,
			SpawnProfile:     a.SpawnProfile,
			Harness:          a.Harness,
			Model:            a.Model,
			Effort:           a.Effort,
			Sandbox:          a.Sandbox,
			Approval:         a.Approval,
			Wave:             a.Wave,
			EffectiveIsOwner: effectiveTemplateAgentOwner(a, lookupProfile),
		}
		if a.ProfileInline != nil {
			pj := profileInlineToJSON(a.ProfileInline)
			aj.ProfileInline = &pj
		}
		out.Agents = append(out.Agents, aj)
	}
	return out
}

// rhythmToJSON / rhythmFromJSON convert one rhythm between the db and wire
// shapes. The db stores the schedule as an interval-in-seconds or a cron expr;
// the wire carries the interval as a Go-duration string (the shape the cron
// modal + CLI already speak), so a stored interval renders back as "<n>s".
func rhythmToJSON(rh db.Rhythm) rhythmJSON {
	out := rhythmJSON{
		Name:       rh.Name,
		TargetRole: rh.TargetRole,
		CronExpr:   rh.CronExpr,
		Subject:    rh.Subject,
		Body:       rh.Body,
	}
	if rh.CronExpr == "" && rh.IntervalSeconds > 0 {
		out.Interval = (time.Duration(rh.IntervalSeconds) * time.Second).String()
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

// knownRoleNames / knownProfileNames return the role-library / spawn-profile
// names for an actionable validation-error hint — the same "here is what IS
// allowed" shape the unknown-permission-slug error uses. Both are best-effort:
// a DB read error yields nil (the caller degrades to a shorter message), never a
// second failure on an already-failing path. db.ListRoles / db.ListSpawnProfiles
// already order by name, so the output is stable.
func knownRoleNames() []string {
	roles, err := db.ListRoles()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, r.Name)
	}
	return out
}

func knownProfileNames() []string {
	profiles, err := db.ListSpawnProfiles()
	if err != nil {
		return nil
	}
	out := []string{}
	for _, p := range profiles {
		out = append(out, p.Name)
		out = append(out, p.Aliases...)
	}
	sort.Strings(out)
	return out
}

// availableHint renders a "known <kind>: a, b, c" clause naming what IS allowed,
// so a validation error for a dangling by-name reference tells an LLM caller
// exactly which values would resolve (the bar the unknown-slug error already
// sets). names is empty when the registry genuinely has none OR when the
// best-effort knownRoleNames/knownProfileNames read failed — so the empty-case
// wording stays neutral ("no <kind> available to reference") rather than
// asserting the registry is empty, which would be wrong on a read error.
func availableHint(kind string, names []string) string {
	if len(names) == 0 {
		return fmt.Sprintf("no %s available to reference", kind)
	}
	return fmt.Sprintf("known %s: %s", kind, strings.Join(names, ", "))
}

// sortedStringSet returns a set's keys in stable ascending order — used to list
// the roster agent names a work_pattern step may route to.
func sortedStringSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
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
		Name:              name,
		Descr:             strings.TrimSpace(body.Descr),
		DefaultContext:    ctx,
		PerAgentWorktrees: body.PerAgentWorktrees,
		Agents:            []db.GroupTemplateAgent{},
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

		perms, permFail := normalizeBlueprintGrants(a.Permissions)
		if permFail != nil {
			permFail.Msg = fmt.Sprintf("agent %q: %s", an, permFail.Msg)
			return nil, permFail
		}

		// Template-local spawn profile: validated with the registry profiles'
		// own field validation (name-less, non-deploy fields rejected) so an
		// inline config can never store a value a registry profile couldn't.
		var inline *db.SpawnProfile
		if a.ProfileInline != nil {
			p, fail := buildInlineProfileFromJSON(*a.ProfileInline)
			if fail != nil {
				return nil, &spawnFailure{fail.Status, fail.Kind,
					fmt.Sprintf("agent %q: %s", an, fail.Msg)}
			}
			inline = p
		}

		// Role reference (JOH-240): validate the referenced role exists at save,
		// exactly as the spawn-profile reference is validated — so a template
		// can't persist a dangling role_ref. Blank = no role. Resolved BEFORE
		// access and briefing resolution below.
		roleRef := strings.TrimSpace(a.RoleRef)
		var role *db.Role
		if roleRef != "" {
			rl, err := db.GetRole(roleRef)
			if err != nil {
				return nil, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
			}
			if rl == nil {
				return nil, &spawnFailure{http.StatusBadRequest, "invalid_role",
					fmt.Sprintf("agent %q: role_ref %q does not name a role in the role library — %s. "+
						"Create it first (`tclaude agent roles create`, needs roles.manage) or clear role_ref.",
						an, roleRef, availableHint("roles", knownRoleNames()))}
			}
			role = rl
		}

		// Per-role launch profile (JOH-239). Validate the referenced spawn
		// profile exists and the inline overrides against the harness they will
		// launch on. The validation harness mirrors the instantiate-time
		// resolution — the agent's inline harness wins, else the template-local
		// profile's harness, else the referenced profile's, else the default
		// (Claude Code) — so a
		// value accepted here is checked against the same catalog the spawn
		// will use. Blank fields stay blank (Validate*, not Resolve*): the
		// launch boundary applies its own defaults at instantiate.
		launch, fail := validateTemplateAgentLaunch(an, a, inline, role)
		if fail != nil {
			return nil, fail
		}

		// Wave (JOH-244): the agent's staged-spawn wave. Non-negative; a sanity
		// cap keeps a typo from scheduling an absurd number of deferred beats.
		if a.Wave < 0 {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("agent %q: wave must be >= 0", an)}
		}
		if a.Wave > maxWaveNumber {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("agent %q: wave must be <= %d", an, maxWaveNumber)}
		}

		t.Agents = append(t.Agents, db.GroupTemplateAgent{
			Ordinal:        i,
			Name:           an,
			Role:           strings.TrimSpace(a.Role),
			Descr:          strings.TrimSpace(a.Descr),
			InitialMessage: im,
			IsOwner:        a.IsOwner,
			Permissions:    perms,
			RoleRef:        roleRef,
			SpawnProfile:   launch.SpawnProfile,
			Harness:        launch.Harness,
			Model:          launch.Model,
			Effort:         launch.Effort,
			Sandbox:        launch.Sandbox,
			Approval:       launch.Approval,
			ProfileInline:  inline,
			Wave:           a.Wave,
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
			targets := append([]string{`"all"`}, sortedStringSet(seenNames)...)
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("work_pattern step #%d: send_to %q must be \"all\" (broadcast) or one of the "+
					"roster agent names — valid targets: %s", i+1, sendTo, strings.Join(targets, ", "))}
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

	// Process spec (JOH-242): an ordered list of phases. Empty/absent = no
	// process (the feature is simply off — validated only when phases exist).
	// Phase names must be nonempty and unique case-insensitively (the current
	// phase is tracked by name, and the ## Process block reads by name); each
	// declared role entry must be nonempty. Criteria is free prose, uncapped
	// beyond the section length the composed context already tolerates. The
	// phase cap is a sanity bound, far above any real quest plan.
	const maxProcessPhases = 64
	if len(body.Process) > maxProcessPhases {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("process: at most %d phases", maxProcessPhases)}
	}
	phases, fail := buildProcessFromJSON(body.Process)
	if fail != nil {
		return nil, fail
	}
	t.Process = phases

	// Wave max-wait (JOH-244): the per-template cap on how long a wave gate
	// waits for the prior wave to settle. Non-negative; 0 = built-in default.
	if body.WaveMaxWait < 0 {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg", "wave_max_wait must be >= 0 (seconds)"}
	}
	t.WaveMaxWait = body.WaveMaxWait

	// Owner scopes (TCL-1071): validated through the SAME boundary the group
	// PATCH endpoint uses, so a template can never persist a narrowing the
	// gate would refuse to read — which would fail the deployed group's owner
	// bypass closed for every slug, long after the author left the editor.
	if len(body.OwnerScopes) > 0 {
		raw, err := json.Marshal(body.OwnerScopes)
		if err != nil {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_owner_scopes", err.Error()}
		}
		canonical, err := canonicalOwnerScopes(string(raw))
		if err != nil {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_owner_scopes", err.Error()}
		}
		t.OwnerScopesJSON = canonical
	}

	// Rhythms (JOH-244): recurring nudges materialized as group cron jobs at
	// deploy. Validated to the same rules the cron add path enforces (name
	// charset, exactly-one schedule mode, interval >= 30s, cron expr parse) so a
	// template can't persist a rhythm that would be rejected at materialize.
	rhythms, fail := buildRhythmsFromJSON(body.Rhythms)
	if fail != nil {
		return nil, fail
	}
	t.Rhythms = rhythms
	return t, nil
}

// buildProcessFromJSON validates a wire-shape process spec and converts it to
// the db shape (JOH-242). Empty/absent yields an empty slice (no process). It
// enforces: phase names nonempty + unique case-insensitively; each role entry
// nonempty (trimmed). Role labels keep their original case (display) but match
// case-insensitively at runtime. Whitespace is trimmed off names/roles;
// criteria is preserved verbatim (prose).
func buildProcessFromJSON(in []processPhaseJSON) ([]db.ProcessPhase, *spawnFailure) {
	out := []db.ProcessPhase{}
	seen := map[string]bool{}
	for i, ph := range in {
		name := strings.TrimSpace(ph.Name)
		if name == "" {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("process phase #%d: name is required", i+1)}
		}
		key := strings.ToLower(name)
		if seen[key] {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("duplicate process phase name %q — phase names must be unique (case-insensitive)", name)}
		}
		seen[key] = true

		roles := []string{}
		for _, r := range ph.Roles {
			r = strings.TrimSpace(r)
			if r == "" {
				return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
					fmt.Sprintf("process phase %q: a role entry is empty — remove it or name a role", name)}
			}
			roles = append(roles, r)
		}
		out = append(out, db.ProcessPhase{Name: name, Roles: roles, Criteria: strings.TrimSpace(ph.Criteria)})
	}
	return out, nil
}

// maxWaveNumber is a sanity cap on a template agent's wave (JOH-244) — far
// above any real staged deploy. It bounds how many deferred beats one deploy
// can schedule, so a typo can't create an absurdly long choreography.
const maxWaveNumber = 64

// maxRhythms caps how many rhythms one template declares — a sanity bound, far
// above any real drumbeat set.
const maxRhythms = 32

// rhythmRoleAll is the reserved rhythm/cron role token meaning "the whole
// group", the same broadcast sense "all" carries as a work-pattern target and a
// process phase role. Normalized to "" (no filter) at build time so the cron
// fan-out — which reads "" as whole-group — needs no special case.
const rhythmRoleAll = "all"

// buildRhythmsFromJSON validates a wire-shape rhythm list and converts it to
// the db shape (JOH-244). Empty/absent yields an empty slice (no rhythms). Each
// rhythm is held to the SAME rules the cron add path enforces — name charset,
// body required, exactly one of interval / cron_expr, interval >= 30s, cron
// expr parses — so a saved rhythm can never be a materialize-time surprise.
// Names must be unique within the template (they become the cron job's
// "<group>-<name>" handle). target_role "all" (case-insensitive) or empty
// normalizes to "" (whole group).
func buildRhythmsFromJSON(in []rhythmJSON) ([]db.Rhythm, *spawnFailure) {
	out := []db.Rhythm{}
	if len(in) > maxRhythms {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("rhythms: at most %d", maxRhythms)}
	}
	seen := map[string]bool{}
	for i, rh := range in {
		name := strings.TrimSpace(rh.Name)
		if name == "" {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("rhythm #%d: name is required", i+1)}
		}
		if err := validateCronName(name); err != nil {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("rhythm %q: %s", name, err.Error())}
		}
		key := strings.ToLower(name)
		if seen[key] {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("duplicate rhythm name %q — rhythm names must be unique (case-insensitive)", name)}
		}
		seen[key] = true

		body := strings.TrimSpace(rh.Body)
		if body == "" {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("rhythm %q: body is required (the message the nudge sends)", name)}
		}

		cronSpec := strings.TrimSpace(rh.CronExpr)
		intervalSpec := strings.TrimSpace(rh.Interval)
		var intervalSeconds int64
		switch {
		case cronSpec != "" && intervalSpec != "":
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("rhythm %q: interval and cron_expr are mutually exclusive — pick one schedule mode", name)}
		case cronSpec == "" && intervalSpec == "":
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("rhythm %q: one of interval / cron_expr is required", name)}
		case cronSpec != "":
			if err := cronexpr.Validate(cronSpec); err != nil {
				return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
					fmt.Sprintf("rhythm %q: %s", name, err.Error())}
			}
		default:
			d, err := time.ParseDuration(intervalSpec)
			if err != nil {
				return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
					fmt.Sprintf("rhythm %q: interval must be a Go duration like 10m / 1h / 30s; got %q", name, rh.Interval)}
			}
			if d < 30*time.Second {
				return nil, &spawnFailure{http.StatusBadRequest, "invalid_arg",
					fmt.Sprintf("rhythm %q: interval must be >= 30s (the scheduler tick interval)", name)}
			}
			intervalSeconds = int64(d.Seconds())
		}

		role := strings.TrimSpace(rh.TargetRole)
		if strings.EqualFold(role, rhythmRoleAll) {
			role = ""
		}

		out = append(out, db.Rhythm{
			Name:            name,
			TargetRole:      role,
			IntervalSeconds: intervalSeconds,
			CronExpr:        cronSpec,
			Subject:         strings.TrimSpace(rh.Subject),
			Body:            body,
		})
	}
	return out, nil
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

// composeInstantiationContext folds the per-instantiation assignment
// text into the template's reusable boilerplate. The template context
// is rarely-changed group-wide guidance; the assignment is the specific
// job for THIS group, so it lands under a "## <header>" section that
// every spawned agent sees in its startup briefing. header is "Task" for
// a plain instantiate and "Mission" for a deploy (JOH-245) — the section
// name is the only difference between the two paths' composed context.
func composeInstantiationContext(templateContext, assignment, header string) string {
	if strings.TrimSpace(header) == "" {
		header = "Mission"
	}
	templateContext = strings.TrimSpace(templateContext)
	assignment = strings.TrimSpace(assignment)
	section := "## " + header + "\n\n" + assignment
	switch {
	case assignment == "":
		return templateContext
	case templateContext == "":
		return section
	default:
		return templateContext + "\n\n" + section
	}
}

// joinContextBlocks joins non-blank startup-context blocks with a blank line,
// the separator composeInstantiationContext and appendRoleBlock use.
func joinContextBlocks(blocks ...string) string {
	var out []string
	for _, b := range blocks {
		if b = strings.TrimSpace(b); b != "" {
			out = append(out, b)
		}
	}
	return strings.Join(out, "\n\n")
}

// groupStartupContext is the group context a new member is briefed with: the
// group's standing context when includeStanding, then its mission block, which
// every member gets whatever include_group_context says. For a legacy group the
// mission block is empty, because the mission is still inside DefaultContext.
func groupStartupContext(g *db.AgentGroup, includeStanding bool) string {
	if g == nil {
		return ""
	}
	standing := ""
	if includeStanding {
		standing = g.DefaultContext
	}
	return joinContextBlocks(standing, g.MissionBlock())
}

// appendRoleBlock folds a role's canonical brief into an agent's startup
// context as a trailing "## Role" section (JOH-240). A blank brief is a no-op
// (returns the context unchanged), so the block only appears when the
// referenced role actually carries guidance.
// roleBlockHeading starts each role block in a startup context.
const roleBlockHeading = "## Role\n\n"

func appendRoleBlock(groupContext, brief string) string {
	brief = strings.TrimSpace(brief)
	if brief == "" {
		return groupContext
	}
	section := roleBlockHeading + brief
	if strings.TrimSpace(groupContext) == "" {
		return section
	}
	return groupContext + "\n\n" + section
}

// permOverride is one resolved birth-time permission decision for an
// instantiated template agent: a slug and its effect (grant | deny).
type permOverride struct {
	Slug string
	// Override carries the effect (db.PermEffectGrant | db.PermEffectDeny)
	// plus the optional scope the winning tier authored. A tier that composes
	// per-slug must carry the whole value: replacing a scoped grant's effect
	// while dropping its scope would silently widen it.
	Override db.PermissionOverride
}

// effectiveTemplateAgentOwner computes the owner bit a deploy of this agent
// would grant — resolveTemplateAgentAccess's owner tiers (referenced profile's
// default → profile_inline tri-state → legacy flag raise), for DISPLAY
// (templateToJSON's effective_is_owner). lookupProfile resolves a registry
// profile by name; a nil result (dangling/unreadable ref) leaves that tier
// silent instead of failing, because a GET must render the template either way
// (the deploy path keeps its strict failure). Callers rendering many agents
// feed a batch map, not a per-call DB read.
func effectiveTemplateAgentOwner(a db.GroupTemplateAgent, lookupProfile func(string) *db.SpawnProfile) bool {
	owner := false
	if ref := strings.TrimSpace(a.SpawnProfile); ref != "" {
		if prof := lookupProfile(ref); prof != nil && prof.IsOwner != nil {
			owner = *prof.IsOwner
		}
	}
	if p := a.ProfileInline; p != nil && p.IsOwner != nil {
		owner = *p.IsOwner
	}
	if a.IsOwner {
		owner = true
	}
	return owner
}

// resolveTemplateAgentAccess computes the effective birth-time access controls
// for one instantiated template agent (JOH-350 / JOH-354): whether it is a
// group owner, and its ordered per-slug permission overrides. Ownership and
// permissions now RIDE the agent's referenced spawn profile — the same profile
// that carries its launch shape (resolveTemplateAgentLaunch) — so a template
// role's access is configured ONCE, in the profile, instead of a duplicated
// inline permission-checkbox list in the template editor.
//
// Composition, lowest → highest priority (a later tier wins per-slug):
//
//	role default grants → agent's spawn-profile overrides → agent inline grants
//
// So an inline grant beats a profile deny, and a profile deny beats a role
// grant. Ownership is the UNION of the agent's own is_owner flag and its
// profile's is_owner default — either marks it an owner. The (legacy) inline
// agent.Permissions grants remain honoured here so a template authored before
// the profile-picker cutover, or a bundled starter that still lists inline
// grants, keeps deploying its escalated leads correctly (no migration).
//
// The referenced profile is fetched here with a cheap loopback read; it is
// fetched again for launch fields in resolveTemplateAgentLaunch. The two
// resolutions are kept separate for clarity — a vanished profile is reported
// the same typed failure by both. A nil role contributes nothing.
//
// resolveTemplateAgentRoles applies the template identity precedence for saved
// role presets: an agent's direct role_ref wins, then its saved spawn profile's
// complete role set.
func resolveTemplateAgentRoles(a db.GroupTemplateAgent) ([]*db.Role, *spawnFailure) {
	refs := []string{}
	if ref := strings.TrimSpace(a.RoleRef); ref != "" {
		refs = []string{ref}
	}
	// A template agent's own role selection is explicit and wins. Otherwise a
	// referenced spawn profile may provide the behavior/access preset, matching
	// direct-spawn identity resolution. Template-local profiles reject role_ref.
	if len(refs) == 0 && (strings.TrimSpace(a.SpawnProfile) != "" || a.SpawnProfileID > 0) {
		var profile *db.SpawnProfile
		var err error
		if a.SpawnProfileID > 0 {
			profile, err = db.GetSpawnProfileByID(a.SpawnProfileID)
		} else {
			profile, err = db.ResolveSpawnProfile(a.SpawnProfile)
		}
		if err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		if profile == nil {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_profile",
				fmt.Sprintf("references spawn profile %q which no longer exists", a.SpawnProfile)}
		}
		refs = append([]string{}, profile.RoleRefs...)
		if len(refs) == 0 && strings.TrimSpace(profile.RoleRef) != "" {
			refs = []string{profile.RoleRef}
		}
	}
	refs = cleanRoleRefs(refs)
	if len(refs) == 0 {
		return nil, nil
	}
	roles := make([]*db.Role, 0, len(refs))
	for _, ref := range refs {
		role, err := db.GetRole(ref)
		if err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		if role != nil {
			roles = append(roles, role)
		}
	}
	// Preserve the existing graceful degradation for a role deleted outside the
	// guarded API: the agent still launches, without the vanished preset.
	return roles, nil
}

// Roles contribute grants only, never denies or ownership. Launch profiles stay
// independent, so a role cannot silently change how the agent is launched.
func resolveTemplateAgentAccess(a db.GroupTemplateAgent, roles []*db.Role) (bool, []permOverride, *spawnFailure) {
	order := []string{}
	eff := map[string]db.PermissionOverride{}
	set := func(slug string, override db.PermissionOverride) {
		slug = strings.TrimSpace(slug)
		if slug == "" {
			return
		}
		if _, ok := eff[slug]; !ok {
			order = append(order, slug)
		}
		eff[slug] = override
	}
	// Tier 1: the referenced role's default grants.
	for _, role := range roles {
		for _, s := range role.Permissions {
			candidate := db.PermissionOverride{Effect: db.PermEffectGrant, Scope: s.Scope}
			if existing, ok := eff[s.Slug]; ok && existing != candidate {
				return false, nil, &spawnFailure{http.StatusBadRequest, "role_scope_conflict",
					fmt.Sprintf("roles grant %s with incompatible scopes", s.Slug)}
			}
			set(s.Slug, candidate)
		}
	}
	// Ownership composes tri-state up the tiers, most specific last: the
	// referenced profile's owner default, then an explicit
	// profile_inline.is_owner — true OR false, so a template-local "not an
	// owner" really does override the registry profile's default, the same
	// more-specific-wins rule the per-slug overrides below follow — then the
	// legacy per-agent flag, which is a plain bool and can only grant (false
	// just means "unset" there).
	owner := false
	// Tier 2: the agent's referenced spawn profile — its owner default + its
	// grant/deny overrides. Profile slugs are applied in sorted order so a
	// deploy's per-agent grant report is deterministic (the map itself is not).
	if ref := strings.TrimSpace(a.SpawnProfile); ref != "" || a.SpawnProfileID > 0 {
		var prof *db.SpawnProfile
		var err error
		if a.SpawnProfileID > 0 {
			prof, err = db.GetSpawnProfileByID(a.SpawnProfileID)
		} else {
			prof, err = db.ResolveSpawnProfile(ref)
		}
		if err != nil {
			return false, nil, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		if prof == nil {
			return false, nil, &spawnFailure{http.StatusBadRequest, "invalid_profile",
				fmt.Sprintf("references spawn profile %q which no longer exists", ref)}
		}
		if fail := disabledProfileFailure(prof); fail != nil {
			return false, nil, fail
		}
		if prof.IsOwner != nil {
			owner = *prof.IsOwner
		}
		for _, slug := range db.SortedOverrideSlugs(prof.PermissionOverrides) {
			set(slug, prof.PermissionOverrides[slug])
		}
	}
	// Tier 2.5: the agent's template-local profile — more specific than the
	// registry reference, so its owner default + overrides apply on top of it.
	if p := a.ProfileInline; p != nil {
		if p.IsOwner != nil {
			owner = *p.IsOwner
		}
		for _, slug := range db.SortedOverrideSlugs(p.PermissionOverrides) {
			set(slug, p.PermissionOverrides[slug])
		}
	}
	// Tier 3: the agent's own legacy inline access — highest. The owner flag
	// is a plain bool (no "explicit false"), so it can only raise.
	if a.IsOwner {
		owner = true
	}
	for _, s := range a.Permissions {
		set(s.Slug, db.PermissionOverride{Effect: db.PermEffectGrant, Scope: s.Scope})
	}

	out := make([]permOverride, 0, len(order))
	for _, s := range order {
		out = append(out, permOverride{Slug: s, Override: eff[s]})
	}
	return owner, out, nil
}

// instantiateAgentResult is the per-agent outcome of an instantiation.
type instantiateAgentResult struct {
	Name           string `json:"name"`       // the template agent name
	FinalName      string `json:"final_name"` // "<group>-<name>"
	ConvID         string `json:"conv_id,omitempty"`
	Owner          bool   `json:"owner,omitempty"`
	WorktreePath   string `json:"worktree_path,omitempty"`
	WorktreeBranch string `json:"worktree_branch,omitempty"`
	// OwnerDropped records that this agent's template owner flag was NOT applied
	// because it was spawned into an EXISTING group (reinforce mode never
	// transfers ownership — see handleTemplateReinforce). Mutually exclusive with
	// Owner. Surfaced so the dashboard can tell the operator what was adjusted.
	OwnerDropped bool     `json:"owner_dropped,omitempty"`
	Granted      []string `json:"granted,omitempty"`
	// Resolved is this member's launch shape with per-field provenance — the same
	// echo the direct spawn path returns, per deployed agent (TCL-1097). Its Notes
	// carry the template-tier disclosures too, so there is ONE per-agent
	// disclosure channel rather than a structured echo with a flat `notes` list
	// beside it saying overlapping things.
	//
	// It REPLACES the copilot_api-only `notes` list TCL-1090 shipped as a stopgap:
	// that disclosure is now the CopilotAPI field of this echo. Two disclosure
	// mechanisms for one fact is the failure being avoided, so the old one is
	// gone rather than deprecated.
	Resolved  *agent.ResolvedLaunch `json:"resolved,omitempty"`
	ErrorKind string                `json:"error_kind,omitempty"`
	Error     string                `json:"error,omitempty"`
}

// handleTemplateInstantiate creates a fresh group from a template and
// spawns its whole agent team. Gated on templates.instantiate.
//
// Body: { group_name, task, cwd?, descr?, descr_override?, parent?, attachment_url?, attachment_label? }. group_name
// doubles as the agent-name prefix — agent "PO" in the template becomes
// "<group_name>-PO". task is the multi-line assignment, folded into the
// group's default_context so every member's startup briefing carries
// it. descr_override mirrors context_override's grammar (see the body
// struct below): present = honored verbatim INCLUDING empty, absent =
// today's default.
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
		GroupName         string                    `json:"group_name"`
		Task              string                    `json:"task,omitempty"`
		Cwd               string                    `json:"cwd,omitempty"`
		WorktreePath      string                    `json:"worktree_path,omitempty"`
		WorktreeBranch    string                    `json:"worktree_branch,omitempty"`
		PerAgentWorktrees *db.WavePerAgentWorktrees `json:"per_agent_worktrees,omitempty"`
		WriteProofToken   string                    `json:"write_proof_token,omitempty"`
		Descr             string                    `json:"descr,omitempty"`
		Parent            string                    `json:"parent,omitempty"`
		// DescrOverride mirrors ContextOverride's grammar for the group's
		// description (JOH-385): a pointer distinguishes "not supplied — use
		// the plain descr / its default" (nil) from "supplied, possibly cleared
		// to empty" (non-nil ""). When non-nil it WINS over descr and is honored
		// verbatim — an explicit "" produces a group with NO description,
		// suppressing the "Instantiated from template X" default. This is what
		// the summon dialog's copy mode needs to faithfully copy a source group
		// whose description is empty. Plain descr is left untouched (existing
		// callers — the CLI included — omit descr_override and keep today's exact
		// behavior). If BOTH are sent, descr_override wins: it's the more
		// explicit grammar (same precedence as context_override vs the template
		// context).
		DescrOverride *string `json:"descr_override,omitempty"`
		// ContextOverride, when non-nil, replaces the template's own
		// default_context as the base the group's startup context is composed
		// from (JOH-356) — the group gets its OWN edited copy of the shared
		// context (the "Form a party" picker prefills this field from the
		// template, then the human edits it before submit; the stored template
		// is untouched). A pointer distinguishes "not supplied — use the
		// template's context" (nil) from "supplied, possibly cleared to empty"
		// (non-nil ""). Existing callers (the instantiate/deploy modals) omit
		// it and keep the template's context verbatim.
		ContextOverride *string                           `json:"context_override,omitempty"`
		Environment     *[]sandboxpolicy.EnvironmentEntry `json:"environment,omitempty"`
		AttachmentURL   string                            `json:"attachment_url,omitempty"`
		AttachmentLabel string                            `json:"attachment_label,omitempty"`
		// AgentProfiles — the deploy form's per-member launch-profile resolution;
		// see handleTemplateDeploy's body for the full contract. Applied in
		// runInstantiation (applyAgentProfileOverrides) only to members with no
		// profile of their own.
		AgentProfiles   map[string]string     `json:"agent_profiles,omitempty"`
		RepositoryClone *groupRepositoryClone `json:"repository_clone,omitempty"`
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
	parentGroup, ok := validateInstantiationParent(w, body.GroupName, body.Parent)
	if !ok {
		return
	}
	if body.RepositoryClone != nil && caller != "" {
		writeError(w, http.StatusForbidden, "human_required", "repository cloning during group creation is available only to the dashboard human")
		return
	}
	repositoryPlan, err := prepareGroupRepositoryClone(body.RepositoryClone)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_repository", err.Error())
		return
	}
	attachmentURL, attachmentLabel, err := normalizeGroupAttachment(body.AttachmentURL, body.AttachmentLabel)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_attachment", err.Error())
		return
	}
	var groupEnvironment []sandboxpolicy.EnvironmentEntry
	if body.Environment != nil {
		groupEnvironment, err = sandboxpolicy.NormalizeEnvironment(*body.Environment)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_environment", err.Error())
			return
		}
	}
	// An existing-directory cwd uses resolveSpawnCwd so a typo cannot turn into
	// an N×30s spawn timeout. A repository workspace is intentionally absent at
	// this point: validate its absolute destination without requiring existence,
	// then runInstantiation clones it after the template preflight.
	var cwd string
	if repositoryPlan != nil {
		requestedCwd, cwdErr := resolveGroupDefaultCwd(body.Cwd)
		if cwdErr != nil {
			writeError(w, http.StatusBadRequest, "invalid_cwd", cwdErr.Error())
			return
		}
		if requestedCwd != "" && requestedCwd != repositoryPlan.Destination {
			writeError(w, http.StatusBadRequest, "invalid_cwd", "cwd must match the repository clone destination")
			return
		}
		cwd = repositoryPlan.Destination
	} else {
		cwd, err = resolveSpawnCwd(body.Cwd)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cwd", err.Error())
			return
		}
	}
	worktreePath, perAgentWorktrees, ok := resolveTemplateWorktreeInputs(w, body.WorktreePath, body.PerAgentWorktrees)
	if !ok {
		return
	}
	// Dir write-proof (spawn_dir_proof.go) — an agent caller instantiating a
	// template picks the cast's launch cwd (and worktree/repo) just like a
	// spawn, so it must prove its own sandbox can write there. Humans pass
	// through. Gates before any child spawns; pins the cast to the resolved
	// dirs on success.
	var proofDirs []string
	var resolvedRepo string
	var codexGitCommonDir string
	if repositoryPlan == nil {
		codexGitCommonDir, err = templateCodexGitCommonDir(cwd, perAgentWorktrees)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
	}
	cwd, worktreePath, resolvedRepo, codexGitCommonDir, proofDirs, ok = requireTemplateDirWriteProof(w, r, caller, body.WriteProofToken, cwd, worktreePath,
		templateRepoAnchor(perAgentWorktrees), templatePerAgentWorktreeParent(perAgentWorktrees), codexGitCommonDir)
	if !ok {
		return
	}
	if perAgentWorktrees != nil {
		perAgentWorktrees.Repo = resolvedRepo
	}
	descr := strings.TrimSpace(body.Descr)
	if descr == "" {
		descr = "Instantiated from template " + tmpl.Name
	}
	// descr_override (JOH-385), when supplied, wins over the plain descr and is
	// honored verbatim — including an explicit "", which suppresses the default
	// above and produces a group with NO description. This is what copy mode
	// needs to faithfully copy a source group whose description is empty.
	if body.DescrOverride != nil {
		descr = strings.TrimSpace(*body.DescrOverride)
	}
	task := strings.TrimSpace(body.Task)
	// A plain instantiate records the source template and the supplied task as
	// group provenance so the dashboard can frame the launched group with the
	// purpose it was created for. It still renders the assignment under "Task";
	// deploy is the path that renders under "Mission" and marks deployed=true.
	runInstantiation(w, instantiateSpec{
		tmpl:              tmpl,
		caller:            caller,
		groupName:         body.GroupName,
		assignment:        task,
		contextHeader:     "Task",
		cwd:               cwd,
		worktreePath:      worktreePath,
		worktreeBranch:    strings.TrimSpace(body.WorktreeBranch),
		perAgentWorktrees: perAgentWorktrees,
		proofToken:        strings.TrimSpace(body.WriteProofToken),
		proofDirs:         proofDirs,
		codexGitCommonDir: codexGitCommonDir,
		descr:             descr,
		mission:           task,
		sourceTemplate:    tmpl.Name,
		contextOverride:   body.ContextOverride,
		parentGroup:       parentGroup,
		agentProfiles:     body.AgentProfiles,
		repositoryClone:   repositoryPlan,
		attachRepository:  body.RepositoryClone != nil && body.RepositoryClone.Attach,
		attachmentURL:     attachmentURL,
		attachmentLabel:   attachmentLabel,
		environment:       groupEnvironment,
	})
}

func validateInstantiationParent(w http.ResponseWriter, groupName, parentName string) (string, bool) {
	parentName = strings.TrimSpace(parentName)
	if parentName == "" {
		return "", true
	}
	if err := validateGroupName(parentName); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "parent: "+err.Error())
		return "", false
	}
	if parentName == groupName {
		writeError(w, http.StatusBadRequest, "invalid_arg", "parent must name an existing group other than the new group")
		return "", false
	}
	parent, err := db.GetAgentGroupByName(parentName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "lookup parent group: "+err.Error())
		return "", false
	}
	if parent == nil {
		writeError(w, http.StatusNotFound, "not_found", "no parent group named "+parentName)
		return "", false
	}
	return parentName, true
}

// templateRepoAnchor returns the per-agent-worktree repo — the git anchor the
// daemon uses when cutting each child's worktree — or "" when no per-agent
// worktrees are configured (the shared-cwd / shared-worktree case, already
// covered).
func templateRepoAnchor(perAgent *db.WavePerAgentWorktrees) string {
	if perAgent == nil {
		return ""
	}
	return strings.TrimSpace(perAgent.Repo)
}

// templatePerAgentWorktreeParent returns the directory that AddWorktreeIn's
// default `../<repo>-<branch>` paths are created under. A caller that proves
// only the repo can still be unable to write that sibling parent, so template
// proofing challenges this parent too.
func templatePerAgentWorktreeParent(perAgent *db.WavePerAgentWorktrees) string {
	repo := templateRepoAnchor(perAgent)
	if repo == "" {
		return ""
	}
	return filepath.Dir(repo)
}

func templateCodexGitCommonDir(cwd string, perAgent *db.WavePerAgentWorktrees) (string, error) {
	if perAgent != nil && perAgent.WorktreeAsCwd {
		return spawnGitCommonDir(
			harness.CodexName, harness.SandboxManagedProfile, "", perAgent.Repo)
	}
	return spawnGitCommonDir(
		harness.CodexName, harness.SandboxManagedProfile, "", cwd)
}

func resolveTemplateWorktreeInputs(w http.ResponseWriter, rawSharedPath string, rawPerAgent *db.WavePerAgentWorktrees) (string, *db.WavePerAgentWorktrees, bool) {
	var sharedPath string
	if strings.TrimSpace(rawSharedPath) != "" {
		wt, err := resolveSpawnCwd(rawSharedPath)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_worktree", err.Error())
			return "", nil, false
		}
		sharedPath = wt
	}
	if rawPerAgent == nil {
		return sharedPath, nil, true
	}
	pa := *rawPerAgent
	pa.Repo = expandTilde(strings.TrimSpace(pa.Repo))
	pa.FromBranch = strings.TrimSpace(pa.FromBranch)
	pa.BranchPrefix = strings.TrimSpace(pa.BranchPrefix)
	if pa.Repo == "" {
		writeError(w, http.StatusBadRequest, "invalid_worktree", "per_agent_worktrees.repo is required")
		return "", nil, false
	}
	root, err := worktree.RepoRootForPath(pa.Repo)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_worktree", err.Error())
		return "", nil, false
	}
	pa.Repo = root
	if pa.BranchPrefix == "" {
		writeError(w, http.StatusBadRequest, "invalid_worktree", "per_agent_worktrees.branch_prefix is required")
		return "", nil, false
	}
	return sharedPath, &pa, true
}

// instantiateSpec carries the fully-validated inputs of one
// instantiate-or-deploy run into the shared runInstantiation core: the
// resolved cwd, the caller, the group name, the per-run assignment text
// (a task or a mission) and the section header it renders under, plus the
// deployment provenance (mission / source_template) stamped on the group
// row. The two entry handlers (handleTemplateInstantiate,
// handleTemplateDeploy) each do their own body parse + name/cwd
// resolution, then hand off here so the group-create → spawn-team →
// work-pattern → response pipeline lives in exactly one place.
type instantiateSpec struct {
	tmpl              *db.GroupTemplate
	caller            string
	groupName         string // already validated + collision-checked
	assignment        string // the task / mission free text
	contextHeader     string // "Task" | "Mission"
	cwd               string // already resolved
	worktreePath      string // optional shared worktree path, already resolved
	worktreeBranch    string
	perAgentWorktrees *db.WavePerAgentWorktrees
	proofToken        string
	proofDirs         []string
	codexGitCommonDir string
	descr             string // already defaulted
	mission           string // stored on the group row; "" for a plain instantiate
	sourceTemplate    string // stored on the group row
	parentGroup       string // optional existing group to nest the new group under
	deployed          bool   // frames the response (adds mission + deployed)
	// contextOverride, when non-nil, replaces tmpl.DefaultContext as the base
	// the group context is composed from (JOH-356 — the "Form a party" picker's
	// editable copy of the shared context). nil = use the template's context.
	contextOverride *string
	// intoExisting, when non-nil, switches runInstantiation into REINFORCE mode
	// (JOH-376 / the reinforcements half of JOH-355): instead of creating a fresh
	// group, the template's roster is deployed INTO this already-resolved,
	// already-validated existing group. In that mode no group is created and no
	// GROUP-LEVEL field is touched (descr / context / cwd / max-members / process
	// / deploy-meta all stay as the group had them — the template's group-level
	// fields are ignored; the roster is what's being deployed). New members
	// inherit the EXISTING group's context + current process (so they match the
	// members already there), template owner flags are dropped (ownership is never
	// transferred), and the collision / max-members / in-flight-choreography
	// checks all ran up-front in handleTemplateReinforce. nil = create-new mode
	// (byte-identical to the pre-JOH-376 instantiate/deploy behaviour).
	intoExisting *db.AgentGroup
	// agentProfiles is the dashboard deploy form's per-member launch-profile
	// resolution, keyed by template-agent name. The deploy dialog prefills a
	// "default profile" from the group / dashboard default, lets the human
	// reconfigure it, and sends the resolved profile for every member that carried
	// NO profile of its own. It is applied by applyAgentProfileOverrides BEFORE the
	// roster is partitioned into waves, so both the synchronous wave 0 and the
	// persisted choreography for later waves carry it. Empty / nil = no overrides
	// (the roster spawns with exactly the template's stored per-member profiles).
	agentProfiles    map[string]string
	repositoryClone  *preparedGroupRepositoryClone
	attachRepository bool
	attachmentURL    string
	attachmentLabel  string
	environment      []sandboxpolicy.EnvironmentEntry
}

// applyAgentProfileOverrides returns the roster with the deploy form's per-member
// launch-profile selections folded in (see instantiateSpec.agentProfiles): for a
// member the map names that carries NO launch config of its own, the map's profile
// becomes its spawn profile. "No config of its own" means no referenced spawn
// profile, no local profile, and no inline launch field. A role reference is
// deliberately irrelevant: it supplies behavior/access and composes with the
// selected launch profile. A member the map does not name,
// or one that is already configured, is left untouched — so a stale or broad
// override can never displace an explicit per-member choice. The client resolves
// the same eligibility before sending, so this is normally a no-op guard; it makes
// the contract hold regardless of what the request carries. Returns the input
// slice unchanged when there are no overrides; otherwise a shallow copy (only the
// SpawnProfile string is rewritten, so sharing the members' slice fields is safe).
func applyAgentProfileOverrides(agents []db.GroupTemplateAgent, overrides map[string]string) []db.GroupTemplateAgent {
	if len(overrides) == 0 {
		return agents
	}
	out := make([]db.GroupTemplateAgent, len(agents))
	copy(out, agents)
	for i := range out {
		a := &out[i]
		configured := strings.TrimSpace(a.SpawnProfile) != "" ||
			a.ProfileInline != nil ||
			strings.TrimSpace(a.Harness) != "" || strings.TrimSpace(a.Model) != "" ||
			strings.TrimSpace(a.Effort) != "" || strings.TrimSpace(a.Sandbox) != "" ||
			strings.TrimSpace(a.Approval) != ""
		if configured {
			continue // the member has its own launch config — the default never wins
		}
		if p := strings.TrimSpace(overrides[a.Name]); p != "" {
			a.SpawnProfile = p
		}
	}
	return out
}

// templateRosterExplicitlyDisablesSandboxProfiles reports whether every member
// pins Codex danger-full-access in its immutable per-agent fields (legacy
// direct overrides, then the template-local profile_inline tier). The
// instantiator normally freezes one global/group policy snapshot
// for the whole roster before spawning any wave. An all-explicit-danger roster
// never applies that snapshot, so containment-checking it would reject agent
// callers for capabilities none of their children receive.
//
// Do not resolve role or spawn-profile references here. Delayed waves resolve
// those mutable registries when they launch; using their value at deploy time
// to discard the frozen policy would let a later edit switch a queued member to
// managed sandboxing while leaving it with an empty policy snapshot.
func templateRosterExplicitlyDisablesSandboxProfiles(agents []db.GroupTemplateAgent) bool {
	if len(agents) == 0 {
		return false
	}
	for _, a := range agents {
		harnessName := strings.TrimSpace(a.Harness)
		harnessBuiltinMode := strings.TrimSpace(a.Sandbox)
		sandboxImplementation := ""
		if a.ProfileInline != nil {
			if harnessName == "" {
				harnessName = strings.TrimSpace(a.ProfileInline.Harness)
			}
			if harnessBuiltinMode == "" {
				harnessBuiltinMode = strings.TrimSpace(a.ProfileInline.Sandbox)
			}
			sandboxImplementation = strings.TrimSpace(a.ProfileInline.SandboxImplementation)
		}
		if !sandboxProfilesDisabled(harnessName, harnessBuiltinMode, sandboxImplementation) {
			return false
		}
	}
	return true
}

// runInstantiation is the shared core behind both `templates instantiate`
// and `task-force deploy` (JOH-245): it composes the group context (the
// assignment folded under spec.contextHeader), creates the group, records
// its deployment provenance, spawns one agent per template spec, applies
// ownership + permission grants, runs the work pattern, and writes the
// per-agent result. Deploy is just instantiate with a mission rendered as
// "## Mission" instead of "## Task", so the whole body is identical — only
// the section header, the stored provenance, and the response framing
// differ, all carried on spec.
func runInstantiation(w http.ResponseWriter, spec instantiateSpec) {
	tmpl := spec.tmpl
	reinforce := spec.intoExisting != nil

	// The base context + the process rendered into each new agent's briefing
	// differ by mode:
	//   - create-new: the template's default_context (or the caller's edited copy,
	//     JOH-356) + the template's process — the group is the template's.
	//   - reinforce (JOH-376): the EXISTING group's context + its CURRENT process.
	//     The template's group-level fields are deliberately ignored — the new
	//     members join an existing group, so they inherit THAT group's shared
	//     context and whatever process phase it is in, staying consistent with the
	//     members already there.
	baseContext := tmpl.DefaultContext
	if spec.contextOverride != nil {
		baseContext = *spec.contextOverride
	}
	procPhases := tmpl.Process
	if reinforce {
		baseContext = spec.intoExisting.DefaultContext
		// Load the existing group's current process snapshot (nil when the group
		// has no process — a no-op ## Process block, same as an empty template).
		if st, perr := db.GetGroupProcessState(spec.intoExisting.ID); perr != nil {
			slog.Warn("reinforce: load group process failed", "group", spec.groupName, "error", perr)
			procPhases = nil
		} else if st != nil {
			procPhases = st.Process
		} else {
			procPhases = nil
		}
	}

	// The size check runs on the whole brief, as it always has; the two parts
	// are then kept apart. groupContext is the group's standing context (what a
	// new group stores as its default_context, and what include_group_context
	// governs); assignmentContext is the mission/task, delivered to every member
	// whatever that toggle says. A reinforce delivers the existing group's own
	// mission block as well as the per-run task.
	if _, err := normalizeGroupContext(composeInstantiationContext(baseContext, spec.assignment, spec.contextHeader)); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	// Trimmed, as the composed context always was.
	groupContext, _ := normalizeGroupContext(strings.TrimSpace(baseContext))
	assignmentContext, _ := normalizeGroupContext(composeInstantiationContext("", spec.assignment, spec.contextHeader))
	if reinforce {
		assignmentContext = joinContextBlocks(spec.intoExisting.MissionBlock(), assignmentContext)
	}
	// The mission is stored apart from the context only when it is the text
	// this deploy briefs members with, which every create path guarantees.
	missionHeader := ""
	if m := normalizeAssignment(spec.mission); m != "" && m == normalizeAssignment(spec.assignment) {
		missionHeader = spec.contextHeader
	}
	// Fold deploy-time profile selections before resolving the shared sandbox
	// policy: they can select danger-full-access for otherwise-unconfigured
	// members and therefore decide whether any policy tier applies at all.
	roster := applyAgentProfileOverrides(tmpl.Agents, spec.agentProfiles)

	// Attenuation-only delegation, checked BEFORE anything is created or
	// spawned. A deploy is a minting surface like any other: its roster confers
	// birth-time grants composed from role defaults, the referenced profile and
	// the template-local one. Without this an agent whose own grant is narrowly
	// scoped could deploy a template that hands a worker the same slug unscoped
	// and act through the worker — the bypass the spawn-boundary check exists to
	// close. Up front, because refusing mid-roster would strand agents that
	// already spawned.
	//
	// It judges ROSTER, not tmpl.Agents: agent_profiles is a caller-supplied
	// request field that pins a registry profile onto any member with no launch
	// config of its own, and that profile's permission_overrides confer grants
	// the stored template never mentioned. Checking the template alone would
	// leave the widest tier of the composition unexamined. See
	// permission_attenuation.go.
	if err := checkTemplateDeployAttenuation(roster, spec.caller); err != nil {
		cleanupDirWriteProofMarkers(spec.proofToken, spec.proofDirs)
		writeError(w, http.StatusForbidden, "scope_not_attenuated", err.Error())
		return
	}

	// Freeze the target policy once for the whole template run. A create-new
	// group has no group assignment yet, so it composes the global tier only;
	// reinforce resolves the existing target group's assignment as well. Every
	// synchronous and delayed wave receives these exact bytes even if registry
	// rows or assignments change while the choreography is in flight.
	sandboxGroupID := int64(0)
	if reinforce {
		sandboxGroupID = spec.intoExisting.ID
	}
	effectiveSandbox := sandboxpolicy.DerivedOmittedProfilesSnapshot()
	var policyErr error
	if !templateRosterExplicitlyDisablesSandboxProfiles(roster) {
		effectiveSandbox, policyErr = db.ResolveEffectiveSandboxSnapshot(sandboxGroupID, "")
	}
	if policyErr != nil {
		cleanupDirWriteProofMarkers(spec.proofToken, spec.proofDirs)
		writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", policyErr.Error())
		return
	}
	if spec.caller != "" {
		parentSnapshot, err := db.AgentEffectiveSandboxConfigForConv(spec.caller)
		if err != nil {
			cleanupDirWriteProofMarkers(spec.proofToken, spec.proofDirs)
			writeError(w, http.StatusInternalServerError, "io", "load parent sandbox snapshot: "+err.Error())
			return
		}
		if parentSnapshot == nil {
			if sandboxpolicy.HasCapabilities(effectiveSandbox) {
				cleanupDirWriteProofMarkers(spec.proofToken, spec.proofDirs)
				writeError(w, http.StatusForbidden, "sandbox_profile_restricted",
					"this parent predates effective sandbox snapshots and may not inherit custom capabilities; relaunch it under current policy or ask the human to instantiate the template")
				return
			}
		} else {
			validatedParent, err := ensureAgentDirectoriesForRelaunch(*parentSnapshot)
			if err != nil {
				cleanupDirWriteProofMarkers(spec.proofToken, spec.proofDirs)
				writeError(w, http.StatusConflict, "sandbox_profile_changed", err.Error())
				return
			}
			if err := sandboxpolicy.RequireContained(validatedParent, effectiveSandbox); err != nil {
				cleanupDirWriteProofMarkers(spec.proofToken, spec.proofDirs)
				writeError(w, http.StatusForbidden, "sandbox_profile_restricted", err.Error())
				return
			}
		}
	}

	granter := granterLabel(spec.caller)

	var err error
	var g *db.AgentGroup
	var gid int64
	if reinforce {
		// Reinforce mode: the group already exists and was resolved + validated
		// (collisions, max-members, in-flight choreography) in
		// handleTemplateReinforce. Do NOT create a group and do NOT touch ANY
		// group-level field — descr / context / cwd / max-members / process /
		// deploy-meta all stay exactly as the group had them. The composed
		// groupContext above is used only for the NEW members' briefings; it is
		// never written back as the group's default_context.
		g = spec.intoExisting
		gid = g.ID
	} else {
		gid, err = db.CreateAgentGroupWithParentAndAttachment(
			spec.groupName, spec.descr, spec.parentGroup, spec.attachmentURL, spec.attachmentLabel)
		if err != nil {
			if errors.Is(err, db.ErrGroupParentNotFound) {
				writeError(w, http.StatusNotFound, "not_found", "no parent group named "+spec.parentGroup)
				return
			}
			if errors.Is(err, db.ErrGroupParentCycle) {
				writeError(w, http.StatusBadRequest, "invalid_arg", "parent must name an existing group other than the new group")
				return
			}
			writeError(w, http.StatusInternalServerError, "io", "create group: "+err.Error())
			return
		}
		// Reserve the authoritative group name and validate its parent before
		// cloning. The row has no members or settings yet, so clone failure can
		// roll it back safely and leave the operator's dialog retryable.
		if spec.repositoryClone != nil {
			if err := cloneGroupRepository(spec.repositoryClone); err != nil {
				if deleteErr := db.DeleteAgentGroup(spec.groupName); deleteErr != nil {
					slog.Error("instantiate: clone failed and empty group rollback failed",
						"group", spec.groupName, "error", deleteErr)
				}
				cleanupDirWriteProofMarkers(spec.proofToken, spec.proofDirs)
				writeError(w, http.StatusBadGateway, "clone_failed", err.Error())
				return
			}
			spec.cwd = spec.repositoryClone.Destination
			if spec.codexGitCommonDir, err = templateCodexGitCommonDir(spec.cwd, spec.perAgentWorktrees); err != nil {
				if deleteErr := db.DeleteAgentGroup(spec.groupName); deleteErr != nil {
					slog.Error("instantiate: git metadata failed and empty group rollback failed",
						"group", spec.groupName, "error", deleteErr)
				}
				writeError(w, http.StatusInternalServerError, "io", err.Error())
				return
			}
		}
		// Best-effort post-create config — a failure here is logged, not
		// fatal: the group exists and the human can adjust it on the
		// dashboard. Mirrors the /v1/groups create path.
		if spec.cwd != "" {
			if _, err := db.SetAgentGroupDefaultCwd(spec.groupName, spec.cwd); err != nil {
				slog.Warn("instantiate: set default cwd failed", "group", spec.groupName, "error", err)
			}
		}
		if spec.repositoryClone != nil && spec.attachRepository {
			if _, err := db.SetAgentGroupAttachment(spec.groupName, spec.repositoryClone.WebURL, spec.repositoryClone.Label); err != nil {
				slog.Warn("instantiate: set repository attachment failed", "group", spec.groupName, "error", err)
			}
		}
		if groupContext != "" {
			if _, err := db.SetAgentGroupDefaultContext(spec.groupName, groupContext); err != nil {
				slog.Warn("instantiate: set default context failed", "group", spec.groupName, "error", err)
			}
		}
		if spec.environment != nil {
			if _, err := db.SetAgentGroupEnvironment(spec.groupName, spec.environment); err != nil {
				slog.Warn("instantiate: set environment failed", "group", spec.groupName, "error", err)
			}
		}
		// Deployment provenance (JOH-245): what this force was deployed against
		// and from. Best-effort like the cwd/context above; a blank mission +
		// blank source_template is the "not a deployed force" default, so a
		// no-op write is harmless.
		if spec.mission != "" || spec.sourceTemplate != "" {
			if _, err := db.SetAgentGroupDeployMeta(spec.groupName, spec.mission, missionHeader, spec.sourceTemplate); err != nil {
				slog.Warn("instantiate: set deploy meta failed", "group", spec.groupName, "error", err)
			}
		}
		// Owner scopes (TCL-1071): the narrowing the blueprint declares for the
		// force it deploys. Deliberately NOT best-effort like the writes above.
		// Every other post-create field fails in a direction the operator can
		// see and fix — a missing cwd, a blank context. This one fails by
		// deploying a force whose owners hold MORE authority than the blueprint
		// says, and nothing about the running group would ever reveal it. So the
		// deploy is refused and the (still empty, still member-less) group is
		// taken back down rather than handed over half-configured.
		if tmpl.OwnerScopesJSON != "" {
			if _, err := db.SetAgentGroupOwnerScopes(spec.groupName, tmpl.OwnerScopesJSON); err != nil {
				slog.Error("instantiate: set owner scopes failed; refusing the deploy",
					"group", spec.groupName, "error", err)
				if delErr := db.DeleteAgentGroup(spec.groupName); delErr != nil {
					slog.Error("instantiate: could not remove the half-configured group",
						"group", spec.groupName, "error", delErr)
				}
				cleanupDirWriteProofMarkers(spec.proofToken, spec.proofDirs)
				writeError(w, http.StatusInternalServerError, "io",
					"could not apply the template's owner-scope narrowing to the new group, so the "+
						"deploy was refused rather than bringing up a force with a wider owner bypass "+
						"than the template declares: "+err.Error())
				return
			}
		}

		g = &db.AgentGroup{
			ID: gid, Name: spec.groupName, Descr: spec.descr, DefaultCwd: spec.cwd, DefaultContext: groupContext,
			Environment: spec.environment,
			Mission:     spec.mission, MissionHeader: missionHeader, SourceTemplate: spec.sourceTemplate, OwnerScopesJSON: tmpl.OwnerScopesJSON,
		}

		// Advisory process runtime (JOH-242): if the template carries a process,
		// snapshot it into the group's runtime state at the first phase (recording
		// an initial "" → first-phase transition). Best-effort like the meta writes
		// above; a group with no process simply has no state. The snapshotted phases
		// are ALSO rendered into every agent's ## Process block below. Reinforce
		// mode skips this entirely — it never re-inits an existing group's process.
		if len(tmpl.Process) > 0 {
			if err := db.InitGroupProcess(gid, tmpl.Process, granter); err != nil {
				slog.Warn("instantiate: init process state failed", "group", spec.groupName, "error", err)
			}
		}
		// The template's usage profiles become the new group's layer, before
		// any member spawns into it. Best-effort like the meta writes above.
		applyTemplateUsageProfiles(tmpl.ID, gid)
	}

	// Seeded rhythms (JOH-244): materialize the template's recurring nudges as
	// normal group cron jobs before any spawn, so they are armed the moment the
	// team comes up. Best-effort; owned by the deploying identity. In reinforce
	// mode these are still materialized ("rhythms still fire") — a job whose
	// "<group>-<rhythm>" name already exists on the group (e.g. a prior deploy of
	// the same template) is skipped by materializeRhythms, not duplicated. Note
	// that a rhythm is a role-filtered GROUP cron, so it fires group-wide by role;
	// the per-member scoping the reinforcement gives is the work pattern below,
	// which routes only to the newly-spawned members.
	rhythmsCreated := materializeRhythms(g, tmpl.Rhythms, spec.caller)

	// Staged spawn — waves (JOH-244). Partition the roster by wave; spawn wave 0
	// synchronously (so this HTTP call returns real per-agent outcomes), and —
	// when higher waves exist — persist a choreography that the background
	// runner advances as each wave settles. A single-wave template (every agent
	// wave 0) is one synchronous pass, identical to pre-JOH-244 behaviour.
	// Fold the deploy form's per-member profile selections into the roster before
	// partitioning — so wave 0 AND the persisted choreography for later waves both
	// carry them. Only members that carried no profile of their own are touched.
	waves := partitionWaves(roster)
	assignment := normalizeAssignment(spec.assignment)
	// A zero-agent template creates the group (and materializes rhythms) but
	// spawns nobody — mirror the pre-JOH-244 empty-roster behaviour instead of
	// indexing waves[0].
	if len(waves) == 0 {
		cleanupDirWriteProofMarkers(spec.proofToken, spec.proofDirs)
		resp := map[string]any{
			"group":             spec.groupName,
			"template":          tmpl.Name,
			"agents":            []instantiateAgentResult{},
			"spawned":           0,
			"failed":            0,
			"pattern_delivered": 0,
			"pattern_errors":    []string{},
		}
		if rhythmsCreated > 0 {
			resp["rhythms_created"] = rhythmsCreated
		}
		if spec.deployed {
			resp["deployed"] = true
			resp["mission"] = spec.mission
		}
		if reinforce {
			resp["reinforced"] = true
		}
		if spec.parentGroup != "" {
			resp["parent"] = spec.parentGroup
		}
		writeJSON(w, http.StatusCreated, resp)
		return
	}
	// Wave 0: create-new spawns into a fresh group (no prior members to dedupe
	// against, nil existing map); reinforce validated collisions up-front, so a
	// nil map is also correct here (the choreography's later waves still dedupe
	// via groupMemberNames in the runner). suppressOwner drops template owner
	// flags in reinforce mode — ownership is never transferred into an existing
	// group.
	wr := spawnWaveAgents(g, waves[0].Agents, procPhases, groupContext, assignmentContext, spec.cwd,
		spec.worktreePath, spec.worktreeBranch, spec.perAgentWorktrees,
		spec.caller, granter, tmpl.Name, nil, reinforce, spec.proofToken, spec.proofDirs, spec.codexGitCommonDir,
		&effectiveSandbox)

	resp := map[string]any{
		"group":    spec.groupName,
		"template": tmpl.Name,
		"agents":   wr.Results,
		"spawned":  wr.Spawned,
		"failed":   wr.Failed,
	}
	if rhythmsCreated > 0 {
		resp["rhythms_created"] = rhythmsCreated
	}
	if spec.parentGroup != "" {
		resp["parent"] = spec.parentGroup
	}

	if len(waves) == 1 {
		cleanupDirWriteProofMarkers(spec.proofToken, spec.proofDirs)
		// Single wave: the roster is already whole, so deliver the work pattern
		// now — exactly the pre-JOH-244 path.
		delivered, patErrs := deliverWorkPattern(g, tmpl.WorkPattern, tmpl.Name, assignment, spec.caller,
			wr.SpawnedConvs, wr.SpawnedOrder, rosterNameSet(tmpl.Agents))
		resp["pattern_delivered"] = delivered
		resp["pattern_errors"] = patErrs
	} else {
		// Multiple waves: the roster is NOT whole yet, so defer the work pattern
		// to the final wave and persist the choreography for the background
		// runner. The response reports wave 0's outcomes plus what is deferred.
		choreo := &db.WaveChoreography{
			GroupID:           gid,
			GroupName:         spec.groupName,
			TemplateName:      tmpl.Name,
			TemplateID:        tmpl.ID,
			EffectiveSandbox:  &effectiveSandbox,
			GroupContext:      groupContext,
			AssignmentContext: assignmentContext,
			Cwd:               spec.cwd,
			WorktreePath:      spec.worktreePath,
			WorktreeBranch:    spec.worktreeBranch,
			PerAgentWorktrees: spec.perAgentWorktrees,
			ProofToken:        spec.proofToken,
			ProofDirs:         spec.proofDirs,
			CodexGitCommonDir: spec.codexGitCommonDir,
			Caller:            spec.caller,
			Granter:           granter,
			Assignment:        assignment,
			Process:           procPhases,
			WorkPattern:       tmpl.WorkPattern,
			Waves:             waves,
			MaxWaitSeconds:    tmpl.WaveMaxWait,
			NextWave:          1,
			GatingConvs:       wr.SpawnedOrder,
			Activated:         []string{},
			SpawnedConvs:      wr.SpawnedConvs,
			SpawnedOrder:      wr.SpawnedOrder,
			WaveDeadline:      time.Now().Add(waveMaxWaitDuration(tmpl.WaveMaxWait)),
			SuppressOwner:     reinforce,
		}
		if err := db.UpsertWaveChoreography(choreo); err != nil {
			slog.Warn("instantiate: persist choreography failed", "group", spec.groupName, "error", err)
			cleanupDirWriteProofMarkers(spec.proofToken, spec.proofDirs)
		}
		pendingAgents := pendingAgentCount(choreo)
		resp["pattern_delivered"] = 0
		resp["pattern_errors"] = []string{}
		resp["waves_total"] = len(waves)
		resp["pending_waves"] = len(waves) - 1
		resp["pending_agents"] = pendingAgents
		resp["choreography_note"] = fmt.Sprintf(
			"wave 1/%d spawned; %d more agent(s) in %d more wave(s) will spawn as each wave settles (work pattern delivers once the roster is whole)",
			len(waves), pendingAgents, len(waves)-1)
	}

	// Deploy framing (JOH-245): the mission the force was deployed against,
	// so the CLI/dashboard can say "task force X deployed against <mission>".
	if spec.deployed {
		resp["deployed"] = true
		resp["mission"] = spec.mission
	}
	// Reinforce framing (JOH-376): mark the response as a reinforcement and
	// surface what was adjusted so ticket 4/4's dialog can render it. The
	// owner-drop note reports the synchronously-processed (wave 0) roster —
	// later-wave drops are reported through the same per-agent OwnerDropped flag
	// on their own (background) results, not here.
	if reinforce {
		resp["reinforced"] = true
		dropped := 0
		for _, r := range wr.Results {
			if r.OwnerDropped {
				dropped++
			}
		}
		if dropped > 0 {
			resp["owner_note"] = fmt.Sprintf(
				"%d template owner flag(s) dropped — reinforcing an existing group never transfers ownership",
				dropped)
		}
	}
	writeJSON(w, http.StatusCreated, resp)
}

// handleTemplateDeploy is the first-class "deploy a task force against a
// mission" verb (JOH-245): a thin wrapper over the shared runInstantiation
// core. Gated on templates.instantiate (deploy IS instantiate). Body:
// { mission?, group_name?, cwd?, descr?, descr_override?, context_override?, parent? }.
//
// mission is the team's assignment — free text or a Linear epic/issue link
// — and renders into the composed context under "## Mission" (instantiate's
// "## Task" analogue). When group_name is omitted it is DERIVED from the
// mission text (slugged + collision-uniquified); an explicit group_name is
// validated and 409s on a taken name, exactly like instantiate. The chosen
// mission + source template are recorded on the group row so the dashboard
// can show the group as a deployed force.
//
// Mission is OPTIONAL (JOH-373). The unified summon dialog folded the retired
// "instantiate/cast" verb — which created a mission-less group from a name +
// optional task — into this one endpoint, so a blank mission is now a valid
// "just create the team" deploy: no mission is stored, the group is NOT framed
// as a deployed force (deployed=false), and no "## Mission" block is composed
// (composeInstantiationContext drops an empty assignment). A group name can't
// be derived from a blank mission, so one of {mission, group_name} is required.
// Existing callers always send a mission and are unaffected; the /instantiate
// wire endpoint is kept untouched for CLI/back-compat.
//
// Scope-out (stated in the PR): tclaude carries no Linear credentials, so a
// Linear-link mission is stored/rendered verbatim — no title pull. The
// group name then falls back to the template name (a bare URL has no
// readable words to slug).
func handleTemplateDeploy(w http.ResponseWriter, r *http.Request) {
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
		Mission           string                    `json:"mission"`
		GroupName         string                    `json:"group_name,omitempty"`
		Cwd               string                    `json:"cwd,omitempty"`
		WorktreePath      string                    `json:"worktree_path,omitempty"`
		WorktreeBranch    string                    `json:"worktree_branch,omitempty"`
		PerAgentWorktrees *db.WavePerAgentWorktrees `json:"per_agent_worktrees,omitempty"`
		WriteProofToken   string                    `json:"write_proof_token,omitempty"`
		Descr             string                    `json:"descr,omitempty"`
		Parent            string                    `json:"parent,omitempty"`
		// Mirrors the instantiate endpoint's override grammar so the dashboard
		// can deploy a top-level or nested task force while mirroring an existing
		// group's settings instead of the template defaults.
		DescrOverride   *string `json:"descr_override,omitempty"`
		ContextOverride *string `json:"context_override,omitempty"`
		AttachmentURL   string  `json:"attachment_url,omitempty"`
		AttachmentLabel string  `json:"attachment_label,omitempty"`
		// AgentProfiles carries the dashboard deploy form's per-member launch-profile
		// selection (JOH-…): keyed by template-agent name, each value the spawn
		// profile the deploy dialog resolved for a member that carried NO profile of
		// its own (the form prefilled it from the group / dashboard default and the
		// human could reconfigure it). It is a FORM-supplied override, not a
		// server-inferred default: the client resolves each member and sends the
		// result, and the roster is spawned with exactly what was sent. A member the
		// map does not name keeps its own template profile; an entry that names a
		// member which already carries a profile is ignored (applyAgentProfileOverrides
		// only fills a blank), so the map can never stomp an explicit template choice.
		AgentProfiles map[string]string `json:"agent_profiles,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	mission := strings.TrimSpace(body.Mission)
	groupName := strings.TrimSpace(body.GroupName)
	// A blank mission is a mission-less "cast" (JOH-373) — allowed, but then
	// there is nothing to derive a group name from, so require an explicit one.
	if mission == "" && groupName == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "provide a mission or a group name (a mission-less deploy needs a group name to create the team under)")
		return
	}

	if groupName == "" {
		// Derive a sensible group name from the (non-empty) mission, uniquified
		// against existing groups. deriveGroupNameFromMission returns an
		// already-valid, already-free name.
		groupName = deriveGroupNameFromMission(mission, tmpl.Name)
	} else {
		if err := validateGroupName(groupName); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", "group_name: "+err.Error())
			return
		}
		if existing, _ := db.GetAgentGroupByName(groupName); existing != nil {
			writeError(w, http.StatusConflict, "exists", "a group named "+groupName+" already exists")
			return
		}
	}
	// A derived name should always validate, but guard anyway — a slug of an
	// exotic mission that somehow produced an invalid name must not reach
	// CreateAgentGroup.
	if err := validateGroupName(groupName); err != nil {
		writeError(w, http.StatusInternalServerError, "io", "derived group name is invalid: "+err.Error())
		return
	}
	parentGroup, ok := validateInstantiationParent(w, groupName, body.Parent)
	if !ok {
		return
	}
	attachmentURL, attachmentLabel, err := normalizeGroupAttachment(body.AttachmentURL, body.AttachmentLabel)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_attachment", err.Error())
		return
	}

	cwd, err := resolveSpawnCwd(body.Cwd)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_cwd", err.Error())
		return
	}
	worktreePath, perAgentWorktrees, ok := resolveTemplateWorktreeInputs(w, body.WorktreePath, body.PerAgentWorktrees)
	if !ok {
		return
	}
	// Dir write-proof for an agent caller — same gate as instantiate.
	var proofDirs []string
	var resolvedRepo string
	codexGitCommonDir, err := templateCodexGitCommonDir(cwd, perAgentWorktrees)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	cwd, worktreePath, resolvedRepo, codexGitCommonDir, proofDirs, ok = requireTemplateDirWriteProof(w, r, caller, body.WriteProofToken, cwd, worktreePath,
		templateRepoAnchor(perAgentWorktrees), templatePerAgentWorktreeParent(perAgentWorktrees), codexGitCommonDir)
	if !ok {
		return
	}
	if perAgentWorktrees != nil {
		perAgentWorktrees.Repo = resolvedRepo
	}

	descr := strings.TrimSpace(body.Descr)
	if descr == "" {
		// A mission-less cast reads as "instantiated", a real deploy as
		// "deployed" — mirror the wording each retired verb used.
		if mission == "" {
			descr = "Instantiated from template " + tmpl.Name
		} else {
			descr = "Task force deployed from template " + tmpl.Name
		}
	}
	if body.DescrOverride != nil {
		descr = strings.TrimSpace(*body.DescrOverride)
	}
	runInstantiation(w, instantiateSpec{
		tmpl:              tmpl,
		caller:            caller,
		groupName:         groupName,
		assignment:        mission,
		contextHeader:     "Mission",
		cwd:               cwd,
		worktreePath:      worktreePath,
		worktreeBranch:    strings.TrimSpace(body.WorktreeBranch),
		perAgentWorktrees: perAgentWorktrees,
		proofToken:        strings.TrimSpace(body.WriteProofToken),
		proofDirs:         proofDirs,
		codexGitCommonDir: codexGitCommonDir,
		descr:             descr,
		mission:           mission,
		sourceTemplate:    tmpl.Name,
		parentGroup:       parentGroup,
		contextOverride:   body.ContextOverride,
		agentProfiles:     body.AgentProfiles,
		attachmentURL:     attachmentURL,
		attachmentLabel:   attachmentLabel,
		// Only frame as a deployed force when there IS a mission; a mission-less
		// cast records source_template but no mission and no "deployed" response.
		deployed: mission != "",
	})
}

// handleTemplateReinforce deploys a template's roster INTO an already-existing
// group (JOH-376 — the "reinforcements" half of JOH-355), instead of creating a
// fresh group the way instantiate/deploy do. It is the backend the palette's
// "drag a template onto a group" flow (ticket 4/4) consumes. Gated on
// templates.instantiate (reinforce IS instantiate, aimed at an existing group).
//
// Body: { group_name, task?, cwd? } — group_name names the EXISTING group to
// reinforce (NOT a new name to create). task is an optional per-run assignment
// folded into the new members' briefing under "## Task". cwd overrides the spawn
// directory for the new members; when omitted it defaults to the group's own
// default_cwd (the new members land where the group's existing members are).
//
// Design decisions (all validated up-front, before anything is spawned — a
// validation failure leaves the group untouched, never a half-deployed roster):
//
//   - Group must EXIST — a missing group is a 404 (unlike instantiate/deploy,
//     where a taken name is the 409). The template's group-level fields (descr /
//     context / max-members / process / owner) are IGNORED: the roster is what's
//     being deployed, and the group is left exactly as it was.
//   - Name collisions: any roster agent whose final "<group>-<agent>" name is
//     already a live member 409s the WHOLE call, listing the colliding names —
//     predictable, whole-or-nothing behaviour beats silent suffixing for an
//     operator-facing action (the lead's steer; agreed).
//   - max-members: enforced against (current members + full roster). A roster
//     that would push the group past its cap 409s up-front — never partially
//     spawned.
//   - Ownership: the template's per-agent owner flags are DROPPED. An existing
//     group already has its owner; reinforcement never transfers ownership. The
//     response notes how many flags were dropped.
//   - In-flight staged deploy: a multi-wave roster cannot be deployed into a
//     group that already has a pending wave choreography (the choreography table
//     is keyed by group, so a second one would clobber the first). That is a 409;
//     a single-wave reinforce is always fine.
//
// Mid-roster failure (after validation passes): best-effort continue, exactly as
// create-new does — a per-agent spawn/grant failure is recorded on that agent's
// result and skips just it. Tearing already-spawned members back down is
// destructive; a partial reinforcement is surfaced for the human to finish.
func handleTemplateReinforce(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GroupName         string                    `json:"group_name"`
		Task              string                    `json:"task,omitempty"`
		Cwd               string                    `json:"cwd,omitempty"`
		WorktreePath      string                    `json:"worktree_path,omitempty"`
		WorktreeBranch    string                    `json:"worktree_branch,omitempty"`
		PerAgentWorktrees *db.WavePerAgentWorktrees `json:"per_agent_worktrees,omitempty"`
		WriteProofToken   string                    `json:"write_proof_token,omitempty"`
		// AgentProfiles — the deploy form's per-member launch-profile resolution;
		// see handleTemplateDeploy's body for the full contract. Reinforce prefills
		// the form's default from the target group's own default profile, so a
		// blank member joins wearing the same launch shape the group's existing
		// members do (unless the human picked another in the dialog).
		AgentProfiles map[string]string `json:"agent_profiles,omitempty"`
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
	g, err := db.GetAgentGroupByName(body.GroupName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "lookup group: "+err.Error())
		return
	}
	if g == nil {
		writeError(w, http.StatusNotFound, "not_found",
			"no group named "+body.GroupName+" (reinforce deploys INTO an existing group; use instantiate/deploy to create a new one)")
		return
	}
	// Gate on templates.instantiate WITH the group-owner bypass — reinforce
	// mutates an existing group's membership, so, like /rebrief, a group's own
	// owner may reinforce it even without the global slug (owner-state fills only
	// the undecided gap; an explicit deny still wins). The group is resolved above
	// so it can be passed here.
	caller, ok := requireGroupPermission(w, r, PermTemplatesUse, g)
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

	// cwd: an explicit override, else the group's own default (so the new members
	// land alongside the group's existing members). Existence-checked like
	// instantiate — a non-existent dir must fail loudly here, not wedge each spawn.
	rawCwd := strings.TrimSpace(body.Cwd)
	if rawCwd == "" {
		rawCwd = g.DefaultCwd
	}
	cwd, err := resolveSpawnCwd(rawCwd)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_cwd", err.Error())
		return
	}
	worktreePath, perAgentWorktrees, ok := resolveTemplateWorktreeInputs(w, body.WorktreePath, body.PerAgentWorktrees)
	if !ok {
		return
	}
	// Dir write-proof for an agent caller — reinforce is also reachable via the
	// group-owner bypass, so a group owner reinforcing into a dir it cannot
	// itself write is exactly the escape this gate closes.
	var proofDirs []string
	var resolvedRepo string
	codexGitCommonDir, err := templateCodexGitCommonDir(cwd, perAgentWorktrees)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	cwd, worktreePath, resolvedRepo, codexGitCommonDir, proofDirs, ok = requireTemplateDirWriteProof(w, r, caller, body.WriteProofToken, cwd, worktreePath,
		templateRepoAnchor(perAgentWorktrees), templatePerAgentWorktreeParent(perAgentWorktrees), codexGitCommonDir)
	if !ok {
		return
	}
	if perAgentWorktrees != nil {
		perAgentWorktrees.Repo = resolvedRepo
	}

	// Up-front, whole-or-nothing validation — nothing is spawned until all of
	// these pass, so a rejection leaves the group exactly as it was.
	if httpErr := validateReinforcement(g, tmpl); httpErr != nil {
		writeError(w, httpErr.status, httpErr.code, httpErr.msg)
		return
	}

	runInstantiation(w, instantiateSpec{
		tmpl:              tmpl,
		caller:            caller,
		groupName:         g.Name,
		assignment:        body.Task,
		contextHeader:     "Task",
		cwd:               cwd,
		worktreePath:      worktreePath,
		worktreeBranch:    strings.TrimSpace(body.WorktreeBranch),
		perAgentWorktrees: perAgentWorktrees,
		proofToken:        strings.TrimSpace(body.WriteProofToken),
		proofDirs:         proofDirs,
		codexGitCommonDir: codexGitCommonDir,
		intoExisting:      g,
		agentProfiles:     body.AgentProfiles,
	})
}

// reinforceHTTPError is a validation rejection that handleTemplateReinforce
// turns into an HTTP error. Kept as a small typed value so validateReinforcement
// can express the several distinct refusals (missing/full/collision/in-flight)
// with their own status + code without writing to the ResponseWriter itself.
type reinforceHTTPError struct {
	status int
	code   string
	msg    string
}

// validateReinforcement runs the whole-or-nothing pre-spawn checks for deploying
// tmpl's roster into the existing group g (JOH-376): no final-name collision
// with a current member, the roster fits under max_members, and no multi-wave
// roster is deployed over a group that already has a pending staged deploy.
// Returns nil when the reinforcement may proceed, or a typed HTTP error the
// caller renders. Reads the group's current members ONCE (member names double as
// the collision key and the count).
//
// Scope note: this is up-front-within-the-request validation — nothing is
// spawned until all checks pass — NOT a cross-request lock. Two concurrent
// reinforces (or a reinforce racing a plain spawn / staged deploy) can both pass
// the max-members / choreography read here and then both act, exactly like the
// daemon's other member-count guard (checkSpawnGuardrails) and the deploy path's
// choreography upsert, none of which hold a per-group lock. That best-effort
// posture is deliberate and consistent across the daemon; reinforce is a
// human/coordinator action with negligible real concurrency. A per-group
// mutation lock would be the fix if that ever changes, but it is out of scope
// here (and would belong daemon-wide, not bolted onto this one path).
func validateReinforcement(g *db.AgentGroup, tmpl *db.GroupTemplate) *reinforceHTTPError {
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		return &reinforceHTTPError{http.StatusInternalServerError, "io", "count members: " + err.Error()}
	}
	// Current member names, resolved the same way spawnWaveAgents dedupes — a
	// custom title, falling back to the pending enrollment name. This is the
	// collision key.
	present := map[string]bool{}
	for _, m := range members {
		if name := agent.CachedTitle(m.ConvID); name != "" {
			present[name] = true
		}
	}
	collisions := []string{}
	for _, a := range tmpl.Agents {
		finalName := g.Name + "-" + a.Name
		if present[finalName] {
			collisions = append(collisions, finalName)
		}
	}
	if len(collisions) > 0 {
		return &reinforceHTTPError{http.StatusConflict, "name_collision",
			fmt.Sprintf("cannot reinforce %q: %d roster member name(s) already taken by a live member: %s. "+
				"Rename the template agent(s) or the existing member(s) and retry.",
				g.Name, len(collisions), strings.Join(collisions, ", "))}
	}

	// max-members: the group's hard cap binds a reinforcement the same way it
	// binds a plain spawn. Refuse a roster that would push it past the cap —
	// up-front, so nothing is partially spawned.
	if g.MaxMembers > 0 {
		want := len(members) + len(tmpl.Agents)
		if want > g.MaxMembers {
			return &reinforceHTTPError{http.StatusConflict, "group_full",
				fmt.Sprintf("cannot reinforce %q: %d current member(s) + %d roster agent(s) = %d would exceed the group's max_members cap (%d). "+
					"A human must raise max_members (`tclaude agent groups set-max-members %s <n>`) first.",
					g.Name, len(members), len(tmpl.Agents), want, g.MaxMembers, g.Name)}
		}
	}

	// In-flight staged deploy: the choreography table is keyed by group, so a
	// second (multi-wave) deploy into the same group would REPLACE the pending
	// one, orphaning its remaining waves. Refuse a multi-wave reinforce while a
	// choreography is pending. A single-wave reinforce never writes a
	// choreography, so it may proceed — but its names must not claim a slot a
	// QUEUED wave will spawn: spawnWaveAgents dedupes later waves against the
	// live-name set, so a reinforced name-twin would make that wave silently
	// adopt the reinforcement instead of spawning its own agent. Treat the
	// remaining choreography roster as reserved names.
	c, cerr := db.GetWaveChoreography(g.ID)
	if cerr != nil {
		return &reinforceHTTPError{http.StatusInternalServerError, "io", "check staged deploy: " + cerr.Error()}
	}
	if c != nil {
		if len(partitionWaves(tmpl.Agents)) > 1 {
			return &reinforceHTTPError{http.StatusConflict, "staged_deploy_in_flight",
				fmt.Sprintf("cannot reinforce %q with a multi-wave roster: the group has a staged deploy still in flight "+
					"(wave %d/%d). Wait for it to finish, or reinforce with a single-wave template.",
					g.Name, c.NextWave, len(c.Waves))}
		}
		queued := map[string]int{} // reserved finalName -> the wave that will spawn it
		for i := c.NextWave; i < len(c.Waves); i++ {
			for _, a := range c.Waves[i].Agents {
				queued[g.Name+"-"+a.Name] = c.Waves[i].Wave
			}
		}
		reserved := []string{}
		for _, a := range tmpl.Agents {
			finalName := g.Name + "-" + a.Name
			if wv, ok := queued[finalName]; ok {
				reserved = append(reserved, fmt.Sprintf("%s (queued in wave %d)", finalName, wv))
			}
		}
		if len(reserved) > 0 {
			return &reinforceHTTPError{http.StatusConflict, "name_collision",
				fmt.Sprintf("cannot reinforce %q: %d roster member name(s) reserved by the group's staged deploy still in flight: %s. "+
					"Rename the template agent(s), or wait for the staged deploy to finish.",
					g.Name, len(reserved), strings.Join(reserved, ", "))}
		}
	}
	return nil
}

// deriveGroupNameFromMission picks a group name for a deploy when the human
// gives none (JOH-245): slug the mission text into a lowercase-dashed
// handle, fall back to the template name when the mission is a bare URL (no
// readable words — e.g. a Linear link), and uniquify against existing
// groups with a -2 / -3 suffix. The returned name is guaranteed to pass
// validateGroupName and to be free at call time.
func deriveGroupNameFromMission(mission, templateName string) string {
	base := slugForMission(mission)
	if base == "" {
		// Bare URL (or all-punctuation mission): the mission carries no words
		// to name the force after, so name it after the template.
		base = slugify(templateName, 40)
	}
	if base == "" {
		base = "task-force"
	}
	name := base
	for i := 2; ; i++ {
		if existing, _ := db.GetAgentGroupByName(name); existing == nil {
			return name
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
}

// slugForMission slugs a mission into a group-name base, unless the mission
// is a BARE URL — a single whitespace-free token that looks like a link
// (an http(s):// URL or a scheme-less linear.app/… reference). A bare URL
// has no readable words to slug, BUT an issue-tracker link usually carries an
// issue key in its path (e.g. JOH-245); that key names the force far better
// than the template does, so it becomes the slug when present. A bare URL with
// no recognizable key still yields "" and the caller falls back to the template
// name. A mission that merely CONTAINS a URL amid text still slugs the text
// (the URL collapses to dashes and trims away).
func slugForMission(mission string) string {
	m := strings.TrimSpace(mission)
	if isBareURL(m) {
		if key := issueKeyFromURL(m); key != "" {
			return slugify(key, 40)
		}
		return ""
	}
	return slugify(m, 40)
}

// issueKeyRe matches an issue-key-shaped path segment: <letters>-<digits>,
// e.g. "JOH-245". Anchored so a longer segment ("JOH-245-title") does not match.
var issueKeyRe = regexp.MustCompile(`^[A-Za-z]+-[0-9]+$`)

// issueKeyFromURL pulls an issue-key-like segment out of a bare URL — a path
// segment shaped <letters>-<digits> (e.g. "JOH-245" in a Linear issue link
// linear.app/<org>/issue/JOH-245/<title-slug>). Returns the key lowercased, or
// "" when the URL carries no such segment. Generic pattern matching, not
// Linear-specific: a GitHub ".../issues/123" link has no letters-dashed key and
// falls through (its bare number is not a key), which is fine — the caller keeps
// its template-name fallback.
//
// To avoid latching onto an unrelated org/title segment that merely looks like a
// key (e.g. an org slug "acme-2"), a key-shaped segment immediately following an
// "issue"/"issues" path segment wins; only when no key follows such a segment
// does the first key-shaped segment anywhere in the path apply.
func issueKeyFromURL(s string) string {
	// Strip any query/fragment and the scheme, then split the path on '/'.
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '?'); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	segs := strings.Split(s, "/")
	firstKey := ""
	for i, seg := range segs {
		if !issueKeyRe.MatchString(seg) {
			continue
		}
		if firstKey == "" {
			firstKey = seg
		}
		if i > 0 {
			switch strings.ToLower(segs[i-1]) {
			case "issue", "issues":
				return strings.ToLower(seg)
			}
		}
	}
	return strings.ToLower(firstKey)
}

// isBareURL reports whether s is a single token that reads as a URL — an
// http(s):// link or a bare host/path beginning with a known link host.
// Used only to decide whether a mission has slug-worthy words.
func isBareURL(s string) bool {
	if s == "" || strings.ContainsAny(s, " \t\n") {
		return false
	}
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return true
	}
	// Scheme-less single-token links (e.g. "linear.app/team/issue/JOH-245").
	return strings.HasPrefix(lower, "linear.app/") || strings.HasPrefix(lower, "www.")
}

// slugify reduces arbitrary text to a lowercase, dash-separated handle:
// runs of non-[a-z0-9] characters collapse to a single dash, the result is
// lowercased and trimmed of leading/trailing dashes, and capped to max
// bytes (with any dash left dangling by the cut trimmed off). Suitable for
// a group name — validateGroupName only forbids slashes, control chars and
// edge whitespace, all of which this strips.
func slugify(s string, max int) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteRune('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if max > 0 && len(out) > max {
		out = strings.TrimRight(out[:max], "-")
	}
	return out
}
