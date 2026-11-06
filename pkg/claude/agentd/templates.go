package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/cronexpr"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
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

// A template's owner scopes narrow what the owners of every group later
// deployed from it may do (templates_instantiate stamps them onto the new
// group). Changing a live group's owner scopes needs
// groups.settings.owner-scopes plus permissions.grant and permissions.revoke,
// so a write that changes or removes an existing template's owner scopes needs
// the same (awb tcl-yjjbfh); without them
// a templates.manage holder could loosen the narrowing a blueprint's author
// declared, and nothing on the deployed group would show it.
//
// A create changes nothing: there was no blueprint to loosen, and owner
// scopes only take reach away: a new template without them gives no more than
// default ownership. Deleting a scoped template removes its narrowing, so it
// is gated too. A rename keeps the template's scopes (renameTemplate carries
// them verbatim) and stays ungated, so rename-and-recreate under the old name
// is still possible: the gate protects a template's scopes, not its name.

// templateOwnerScopesSlugs are the slugs a group's owner-scope change needs
// (groups_handlers); a template's needs the same, unscoped, as a template has
// no group.
var templateOwnerScopesSlugs = []string{PermGroupsSettingsOwnerScopes, PermPermissionsGrant, PermPermissionsRevoke}

// templateOwnerScopesChange reports whether writing next over existing changes
// or removes existing's owner scopes. A nil existing (a create) is no change.
func templateOwnerScopesChange(existing *db.GroupTemplate, next string) bool {
	if existing == nil {
		return false
	}
	before, errBefore := canonicalOwnerScopes(existing.OwnerScopesJSON)
	after, errAfter := canonicalOwnerScopes(next)
	if errBefore != nil || errAfter != nil {
		return existing.OwnerScopesJSON != next
	}
	return before != after
}

// templateOwnerScopesGate authorizes a template write that changes an
// existing template's owner scopes. It returns nil to allow the write.
type templateOwnerScopesGate func() *spawnFailure

// httpTemplateOwnerScopesGate gates on the request's caller through
// requirePermission, so an agent can still ask the human; a refusal is written
// to w and reported as errSpawnResponseWritten.
func httpTemplateOwnerScopesGate(w http.ResponseWriter, r *http.Request) templateOwnerScopesGate {
	return func() *spawnFailure {
		for _, slug := range templateOwnerScopesSlugs {
			if _, ok := requirePermission(w, r, slug); !ok {
				return errSpawnResponseWritten
			}
		}
		return nil
	}
}

// callerTemplateOwnerScopesGate gates on caller ("" is the human operator)
// without writing a response, for a write judged inside a larger operation
// such as a config bundle import. With no response to answer, it cannot spend
// a use of a use-limited sudo grant, so it refuses one rather than let it
// authorize without counting; the template endpoints spend it.
func callerTemplateOwnerScopesGate(r *http.Request, caller string) templateOwnerScopesGate {
	return func() *spawnFailure {
		if caller == "" {
			return nil
		}
		for _, slug := range templateOwnerScopesSlugs {
			allowed, matched, err := permissionAllowsAction(r, caller, slug, ActionContext{})
			if err != nil {
				return &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
			}
			if !allowed {
				return &spawnFailure{http.StatusForbidden, "permission",
					"changing or removing a template's owner scopes requires permission " + slug}
			}
			if _, limited := limitedSudoSource(r, caller, slug, ActionContext{}); limited {
				return &spawnFailure{http.StatusForbidden, "permission",
					"a use-limited grant of " + slug + " cannot change a template's owner scopes from a bundle; edit or import the template itself"}
			}
			recordAuditPermissionScope(r, slug, matched)
		}
		return nil
	}
}

// checkTemplateOwnerScopes runs gate when writing next over existing changes
// its owner scopes.
func checkTemplateOwnerScopes(gate templateOwnerScopesGate, existing *db.GroupTemplate, next string) *spawnFailure {
	if !templateOwnerScopesChange(existing, next) {
		return nil
	}
	return gate()
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
			writeSpawnFailure(w, fail)
			return
		}
		if fail := peerTemplateEditFailure(r, nil, templateToJSON(t)); fail != nil {
			writeSpawnFailure(w, fail)
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
			writeSpawnFailure(w, fail)
			return
		}
		if fail := checkTemplateOwnerScopes(httpTemplateOwnerScopesGate(w, r), existing, t.OwnerScopesJSON); fail != nil {
			writeSpawnFailure(w, fail)
			return
		}
		was := templateToJSON(existing)
		if fail := peerTemplateEditFailure(r, &was, templateToJSON(t)); fail != nil {
			writeSpawnFailure(w, fail)
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
		existing, err := db.GetGroupTemplate(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if fail := checkTemplateOwnerScopes(httpTemplateOwnerScopesGate(w, r), existing, ""); fail != nil {
			writeSpawnFailure(w, fail)
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
