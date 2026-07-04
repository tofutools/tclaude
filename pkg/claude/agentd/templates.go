package agentd

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/cronexpr"
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

	// Role is a by-name reference to a role in the role library (JOH-240): the
	// agent inherits that role's defaults (canonical role-brief, launch shape,
	// permission set) BENEATH its own overrides. Empty = no role. Named
	// "role_ref" on the wire to stay distinct from the free-text Role display
	// label above.
	RoleRef string `json:"role_ref,omitempty"`

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

	// Wave is the agent's staged-spawn wave (JOH-244), default 0. Waves spawn
	// in ascending order; a template whose every agent is wave 0 spawns in one
	// synchronous pass (today's behaviour). The party's marching order.
	Wave int `json:"wave,omitempty"`
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
	Name           string                 `json:"name"`
	Descr          string                 `json:"descr,omitempty"`
	DefaultContext string                 `json:"default_context,omitempty"`
	Agents         []templateAgentJSON    `json:"agents"`
	WorkPattern    []workPatternEntryJSON `json:"work_pattern"`
	// Process is the template's declarative process spec (JOH-242): an ordered
	// list of phases. Empty/absent = no process (the feature is off).
	Process []processPhaseJSON `json:"process"`
	// Rhythms is the template's recurring-nudge declarations (JOH-244),
	// materialized as group cron jobs at deploy. Empty/absent = no rhythms.
	Rhythms []rhythmJSON `json:"rhythms"`
	// WaveMaxWait caps (seconds) how long each staged-spawn wave waits for the
	// prior wave to go idle before the next spawns anyway (JOH-244). 0 =
	// built-in default.
	WaveMaxWait int    `json:"wave_max_wait,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
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
		Process:        []processPhaseJSON{},
		Rhythms:        []rhythmJSON{},
		WaveMaxWait:    t.WaveMaxWait,
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
			RoleRef:        a.RoleRef,
			SpawnProfile:   a.SpawnProfile,
			Harness:        a.Harness,
			Model:          a.Model,
			Effort:         a.Effort,
			Sandbox:        a.Sandbox,
			Approval:       a.Approval,
			Wave:           a.Wave,
		})
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

		// Role reference (JOH-240): validate the referenced role exists at save,
		// exactly as the spawn-profile reference is validated — so a template
		// can't persist a dangling role_ref. Blank = no role.
		roleRef := strings.TrimSpace(a.RoleRef)
		if roleRef != "" {
			rl, err := db.GetRole(roleRef)
			if err != nil {
				return nil, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
			}
			if rl == nil {
				return nil, &spawnFailure{http.StatusBadRequest, "invalid_role",
					fmt.Sprintf("agent %q: no role named %q in the role library", an, roleRef)}
			}
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

// launchAccum accumulates the effective launch fields as tiers overlay onto
// it, highest priority first. A blank field is still open to a lower tier.
type launchAccum struct {
	harness  string
	model    string
	effort   string
	sandbox  string
	approval string
}

// overlay fills this accumulator's still-blank fields from a lower-priority
// launch source, gated on harness compatibility (the same rule the
// group-default-profile overlay uses): a source is only inherited when the
// resolving harness is still unset (then it adopts the source's harness) or
// already matches the source's harness — so a source tuned for one harness
// never bleeds its model/effort into a spawn on another.
func (l *launchAccum) overlay(srcHarness, srcModel, srcEffort, srcSandbox, srcApproval string) {
	srcHarness = strings.TrimSpace(srcHarness)
	if l.harness != "" && harnessOrDefault(l.harness) != harnessOrDefault(srcHarness) {
		return
	}
	if l.harness == "" {
		l.harness = srcHarness
	}
	if l.model == "" {
		l.model = strings.TrimSpace(srcModel)
	}
	if l.effort == "" {
		l.effort = strings.TrimSpace(srcEffort)
	}
	if l.sandbox == "" {
		l.sandbox = strings.TrimSpace(srcSandbox)
	}
	if l.approval == "" {
		l.approval = strings.TrimSpace(srcApproval)
	}
}

// resolveTemplateAgentLaunch computes the effective launch fields for one
// instantiated template agent (JOH-239 + JOH-240). Resolution order, highest
// priority first:
//
//	per-agent inline override → per-agent spawn profile →
//	  role inline defaults → role's spawn profile → harness secure default
//
// (The group-default-profile tier of the general model is empty here — a
// freshly-instantiated group carries no default profile.) Each profile-like
// tier is inherited only when the spawn will run on that tier's harness (a
// mismatched harness skips it), then the chosen harness's secure launch
// defaults fill whatever is still blank and the whole shape is validated. role
// is the resolved role the agent references (nil = none).
//
// cwd is the resolved instantiation cwd; it drives the Codex sandbox cwd-safety
// guard so a template can't spawn a workspace-write Codex agent at/above $HOME.
//
// Returns a typed failure (recorded per-agent by the instantiator, never fatal
// to the rest of the roster) if a referenced profile vanished or a resolved
// value is invalid for the harness. The returned Harness is the resolved
// canonical name (e.g. "claude"); SpawnProfile is left empty (already consumed).
func resolveTemplateAgentLaunch(a db.GroupTemplateAgent, role *db.Role, cwd string) (templateAgentLaunch, *spawnFailure) {
	acc := launchAccum{
		harness:  strings.TrimSpace(a.Harness),
		model:    strings.TrimSpace(a.Model),
		effort:   strings.TrimSpace(a.Effort),
		sandbox:  strings.TrimSpace(a.Sandbox),
		approval: strings.TrimSpace(a.Approval),
	}

	// Tier 2: the agent's own referenced spawn profile.
	if ref := strings.TrimSpace(a.SpawnProfile); ref != "" {
		prof, err := db.GetSpawnProfile(ref)
		if err != nil {
			return templateAgentLaunch{}, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		if prof == nil {
			return templateAgentLaunch{}, &spawnFailure{http.StatusBadRequest, "invalid_profile",
				fmt.Sprintf("references spawn profile %q which no longer exists", ref)}
		}
		acc.overlay(prof.Harness, prof.Model, prof.Effort, prof.Sandbox, prof.Approval)
	}

	// Tier 3 + 4: the referenced role's inline defaults, then the role's own
	// spawn profile. A role tunes the launch shape only where the agent left it
	// blank (agent overrides win); the role's spawn profile fills what the
	// role's inline fields left open.
	if role != nil {
		acc.overlay(role.Harness, role.Model, role.Effort, role.Sandbox, role.Approval)
		if ref := strings.TrimSpace(role.SpawnProfile); ref != "" {
			prof, err := db.GetSpawnProfile(ref)
			if err != nil {
				return templateAgentLaunch{}, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
			}
			if prof == nil {
				return templateAgentLaunch{}, &spawnFailure{http.StatusBadRequest, "invalid_profile",
					fmt.Sprintf("role %q references spawn profile %q which no longer exists", role.Name, ref)}
			}
			acc.overlay(prof.Harness, prof.Model, prof.Effort, prof.Sandbox, prof.Approval)
		}
	}

	harnessName := acc.harness
	model := acc.model
	effort := acc.effort
	sandbox := acc.sandbox
	approval := acc.approval

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
	// Roles embeds the full definitions of every role the template's agents
	// reference (JOH-240), so the export stays portable — an import re-creates
	// any that are MISSING on the target machine (never overwriting an existing
	// role of the same name; the same sacred-edits rule the seed follows). A
	// reference whose definition isn't embedded and doesn't exist locally is
	// dropped on import, exactly like an unknown spawn-profile reference.
	Roles []roleJSON `json:"roles,omitempty"`
	// Profiles embeds the full definitions of every spawn profile the template's
	// agents (or their embedded roles) reference by name (JOH-350) — the profile
	// now carries the agent's launch shape AND its birth-time permissions/owner,
	// so it must travel for the export to reproduce the same team elsewhere.
	// Import materializes any that are MISSING locally (never overwriting an
	// existing profile of the same name — same sacred-edits rule as roles); a
	// reference that stays unresolved is dropped + warned on import, exactly like
	// today. This field is purely additive: a format_version-1 reader that
	// predates it simply ignores it and degrades the ref, so the envelope stays
	// version 1 (no bump).
	Profiles []spawnProfileJSON `json:"profiles,omitempty"`
}

// collectReferencedRoles gathers the full definitions of every role the
// template's agents reference, for embedding in a portable export. Missing
// roles (a ref whose row was deleted since the template was authored) are
// silently skipped — import degrades such a dangling ref anyway. Order is
// stable (first-referenced) and each role is embedded once.
func collectReferencedRoles(t *db.GroupTemplate) ([]roleJSON, error) {
	seen := map[string]bool{}
	out := []roleJSON{}
	for _, a := range t.Agents {
		ref := strings.TrimSpace(a.RoleRef)
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		rl, err := db.GetRole(ref)
		if err != nil {
			return nil, err
		}
		if rl == nil {
			continue
		}
		j := roleToJSON(rl)
		// Portable: strip the machine-local timestamps (like the template's).
		j.CreatedAt = ""
		j.UpdatedAt = ""
		out = append(out, j)
	}
	return out, nil
}

// collectReferencedProfiles gathers the full definitions of every spawn profile
// the template references, for embedding in a portable export (JOH-350). A
// profile is referenced either directly by an agent's spawn_profile or by a
// role the agent references (a role can name a default profile too), so both
// sources are swept — with the given roles (the ones already gathered for
// embedding) providing the role→profile links without a second role fetch.
// Missing profiles (a ref whose row was deleted since authoring) are silently
// skipped — import degrades such a dangling ref anyway. Order is stable
// (first-referenced) and each profile is embedded once.
func collectReferencedProfiles(t *db.GroupTemplate, roles []roleJSON) ([]spawnProfileJSON, error) {
	seen := map[string]bool{}
	out := []spawnProfileJSON{}
	add := func(ref string) error {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			return nil
		}
		seen[ref] = true
		p, err := db.GetSpawnProfile(ref)
		if err != nil {
			return err
		}
		if p == nil {
			return nil
		}
		j := profileToJSON(p)
		// Portable: strip the machine-local timestamps (like the template's).
		j.CreatedAt = ""
		j.UpdatedAt = ""
		out = append(out, j)
		return nil
	}
	for _, a := range t.Agents {
		if err := add(a.SpawnProfile); err != nil {
			return nil, err
		}
	}
	for _, rl := range roles {
		if err := add(rl.SpawnProfile); err != nil {
			return nil, err
		}
	}
	return out, nil
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
	// Embed the referenced role definitions so the export stays portable
	// (JOH-240) — import re-creates any that are missing on the target.
	roles, err := collectReferencedRoles(t)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	// Embed the referenced spawn profiles too (JOH-350) — sweeping both the
	// agents' direct refs and any the embedded roles carry — so a profile-driven
	// team's launch shape + birth-time permissions travel with the export.
	profiles, err := collectReferencedProfiles(t, roles)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, templateExportEnvelope{
		Format:        templateExportFormat,
		FormatVersion: templateExportVersion,
		ExportedAt:    time.Now().UTC().Format(time.RFC3339),
		Template:      inner,
		Roles:         roles,
		Profiles:      profiles,
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
		// A role reference that doesn't resolve locally — its definition wasn't
		// embedded in the export and no local role of that name exists (or the
		// embedded one collided with an existing role and wasn't re-created) —
		// is dropped so the agent falls through to its own overrides / the
		// harness default (JOH-240). Re-creatable roles were already restored
		// before this sanitize pass, so a surviving dangling ref is genuinely
		// unresolvable.
		if ref := strings.TrimSpace(a.RoleRef); ref != "" {
			rl, err := db.GetRole(ref)
			if err == nil && rl == nil {
				warnings = append(warnings, fmt.Sprintf(
					"agent %q: role %q does not exist here — dropped the reference; the agent will use its own launch overrides",
					label, ref))
				a.RoleRef = ""
			}
			// A GetRole error is left for buildTemplateFromJSON to surface.
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

// recreateMissingProfiles restores each embedded spawn profile that is MISSING
// on this machine (JOH-350), so a template imported from another machine can
// resolve its spawn_profile references (an agent's or an embedded role's). It
// mirrors recreateMissingRoles exactly: an existing profile of the same name is
// LEFT UNTOUCHED — never overwritten (sacred edits) — and the import reports it
// kept the local version; a definition that fails validation here is warned and
// skipped (the referencing agent/role then degrades in sanitize); a DB fault is
// a hard failure. It runs BEFORE recreateMissingRoles, because a role's own
// validation checks its referenced profile exists (buildRoleFromJSON), so the
// profiles must be in place before the roles that name them.
func recreateMissingProfiles(profiles []spawnProfileJSON) ([]string, *spawnFailure) {
	warnings := []string{}
	for _, pj := range profiles {
		name := strings.TrimSpace(pj.Name)
		if name == "" {
			continue
		}
		existing, err := db.GetSpawnProfile(name)
		if err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		if existing != nil {
			warnings = append(warnings, fmt.Sprintf(
				"spawn profile %q already exists here — kept the local version (import never overwrites a profile)", name))
			continue
		}
		p, fail := buildProfileFromJSON(pj)
		if fail != nil {
			warnings = append(warnings, fmt.Sprintf(
				"embedded spawn profile %q is invalid here (%s) — skipped; agents referencing it will use their own overrides",
				name, fail.Msg))
			continue
		}
		if _, err := db.CreateSpawnProfile(p); errors.Is(err, db.ErrSpawnProfileNameTaken) {
			// Lost a create race with a concurrent writer — the name now exists,
			// which is the outcome we wanted, so treat it as "kept".
			warnings = append(warnings, fmt.Sprintf(
				"spawn profile %q already exists here — kept the local version (import never overwrites a profile)", name))
		} else if err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		} else {
			warnings = append(warnings, fmt.Sprintf("imported spawn profile %q", name))
		}
	}
	return warnings, nil
}

// recreateMissingRoles restores each embedded role definition that is MISSING
// on this machine (JOH-240), so a template imported from another machine can
// resolve its role references. An existing role of the same name is LEFT
// UNTOUCHED — never overwritten — because a user's local edits to a role are
// sacred (the same rule the seed follows); the import reports that it kept the
// local version. A definition that fails validation is reported as a warning
// and skipped (the agent's role_ref then degrades in sanitizeImportedTemplate).
// A DB fault (not a validation problem) is returned as a hard failure — an
// import shouldn't silently swallow it.
//
// Two deliberate properties, both consistent with the rest of the import/
// instantiate path (which is intentionally non-atomic — a partial spawn is
// surfaced, not rolled back):
//   - No separate roles.manage gate. Import is a templates.manage operation;
//     re-creating a template's own EMBEDDED roles is part of making the imported
//     template usable, and it can only ADD missing roles (never overwrite). A
//     caller with templates.manage + instantiate can already grant arbitrary
//     slugs via a template's per-agent permissions, so this adds no privilege.
//     Requiring roles.manage too would just break portable import.
//   - Roles are created before buildTemplateFromJSON validates the template
//     (the ordering the sanitize pass needs — role refs must resolve first). A
//     template that fails later validation leaves the added roles behind; that
//     is benign (roles stand alone, and a re-import is idempotent — "kept
//     local").
func recreateMissingRoles(roles []roleJSON) ([]string, *spawnFailure) {
	warnings := []string{}
	for _, rj := range roles {
		name := strings.TrimSpace(rj.Name)
		if name == "" {
			continue
		}
		existing, err := db.GetRole(name)
		if err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		if existing != nil {
			warnings = append(warnings, fmt.Sprintf(
				"role %q already exists here — kept the local version (import never overwrites a role)", name))
			continue
		}
		rl, fail := buildRoleFromJSON(rj)
		if fail != nil {
			warnings = append(warnings, fmt.Sprintf(
				"embedded role %q is invalid here (%s) — skipped; agents referencing it will use their own overrides",
				name, fail.Msg))
			continue
		}
		if _, err := db.CreateRole(rl); errors.Is(err, db.ErrRoleNameTaken) {
			// Lost a create race with a concurrent writer — the name now exists,
			// which is the outcome we wanted, so treat it as "kept".
			warnings = append(warnings, fmt.Sprintf(
				"role %q already exists here — kept the local version (import never overwrites a role)", name))
		} else if err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		} else {
			warnings = append(warnings, fmt.Sprintf("imported role %q", name))
		}
	}
	return warnings, nil
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
	asName := strings.TrimSpace(r.URL.Query().Get("as"))
	update := r.URL.Query().Get("update") == "true"

	res, existed, fail := importTemplateEnvelope(env, asName, update)
	if fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	// A collision without update is a 409 here — the import verb never
	// clobbers silently (the starter-install path turns the same collision
	// into a friendly skip instead; see handleStarterInstall).
	if existed && !update {
		writeError(w, http.StatusConflict, "exists", fmt.Sprintf(
			"a template named %q already exists — re-import with update to overwrite it, or as=<new-name> to import under a different name",
			res.Imported))
		return
	}
	status := http.StatusCreated
	if res.Updated {
		status = http.StatusOK
	}
	writeJSON(w, status, res)
}

// importTemplateEnvelope is the shared portable-import pipeline: it version-
// gates one task-force envelope, re-creates any embedded role that is MISSING
// locally (never overwriting an existing one — sacred edits), sanitizes the
// machine-local references that don't resolve here (spawn profiles, unknown
// slugs, dangling role refs — each warned), validates + builds the template,
// then creates it (or, when update is set, replaces it in place). asName, when
// non-empty, renames the template on import.
//
// It returns the result, whether a same-named template already EXISTED, and a
// failure. When a template already exists and update is false, it does NOT
// write — the caller decides what the collision means: /v1/templates/import
// reports a 409, a starter install reports a friendly skip. This is the single
// importer both paths share (JOH-246) — there is no second import path.
func importTemplateEnvelope(env templateExportEnvelope, asName string, update bool) (templateImportResult, bool, *spawnFailure) {
	if strings.TrimSpace(env.Format) != templateExportFormat {
		return templateImportResult{}, false, &spawnFailure{http.StatusBadRequest, "invalid_format", fmt.Sprintf(
			"not a tclaude task-force export (format=%q, expected %q)", env.Format, templateExportFormat)}
	}
	if env.FormatVersion < 1 {
		return templateImportResult{}, false, &spawnFailure{http.StatusBadRequest, "invalid_format",
			"missing or invalid format_version — not a valid task-force export"}
	}
	if env.FormatVersion > templateExportVersion {
		return templateImportResult{}, false, &spawnFailure{http.StatusBadRequest, "version_too_new", fmt.Sprintf(
			"this export is format_version %d, but this tclaude supports up to %d — upgrade tclaude to import it",
			env.FormatVersion, templateExportVersion)}
	}

	body := env.Template
	if asName != "" {
		body.Name = asName
	}

	// Re-create any embedded spawn profile that is MISSING locally FIRST (JOH-350)
	// — before roles (a role's validation checks its referenced profile exists)
	// and before the sanitize pass checks agent profile references. An existing
	// profile of the same name is never overwritten (sacred edits).
	profileWarnings, pfail := recreateMissingProfiles(env.Profiles)
	if pfail != nil {
		return templateImportResult{}, false, pfail
	}
	// Re-create any embedded role that is MISSING locally, BEFORE the sanitize
	// pass checks role references — an existing role of the same name is never
	// overwritten (sacred edits). Warnings report what was created / skipped.
	roleWarnings, rfail := recreateMissingRoles(env.Roles)
	if rfail != nil {
		return templateImportResult{}, false, rfail
	}

	cleaned, sanitizeWarnings := sanitizeImportedTemplate(body)
	warnings := append([]string{}, profileWarnings...)
	warnings = append(warnings, roleWarnings...)
	warnings = append(warnings, sanitizeWarnings...)
	t, fail := buildTemplateFromJSON(cleaned)
	if fail != nil {
		return templateImportResult{}, false, fail
	}

	existing, err := db.GetGroupTemplate(t.Name)
	if err != nil {
		return templateImportResult{}, false, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
	}
	if existing != nil {
		if !update {
			// Collision — leave the stored template untouched; the caller decides
			// (409 for import, skip for starter install).
			return templateImportResult{Imported: t.Name, Warnings: warnings}, true, nil
		}
		// Overwrite in place: the envelope carries the full desired state, so this
		// is a wholesale replace (the PATCH contract), reusing the existing row's id.
		t.ID = existing.ID
		if err := db.UpdateGroupTemplate(t); errors.Is(err, db.ErrGroupTemplateNameTaken) {
			return templateImportResult{}, true, &spawnFailure{http.StatusConflict, "exists", err.Error()}
		} else if err != nil {
			return templateImportResult{}, true, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		return templateImportResult{Imported: t.Name, Updated: true, Warnings: warnings}, true, nil
	}

	if _, err := db.CreateGroupTemplate(t); errors.Is(err, db.ErrGroupTemplateNameTaken) {
		// Lost a create race with a concurrent writer — surface as a plain 409.
		return templateImportResult{}, false, &spawnFailure{http.StatusConflict, "exists", err.Error()}
	} else if err != nil {
		return templateImportResult{}, false, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
	}
	return templateImportResult{Imported: t.Name, Warnings: warnings}, false, nil
}

// composeInstantiationContext folds the per-instantiation assignment
// text into the template's reusable boilerplate. The template context
// is rarely-changed group-wide guidance; the assignment is the specific
// job for THIS group, so it lands under a "## <header>" section that
// every spawned agent sees in its startup briefing. header is "Task" for
// a plain instantiate and "Mission" for a deploy (JOH-245) — the section
// name is the only difference between the two paths' composed context.
func composeInstantiationContext(templateContext, assignment, header string) string {
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

// appendRoleBlock folds a role's canonical brief into an agent's startup
// context as a trailing "## Role" section (JOH-240). A blank brief is a no-op
// (returns the context unchanged), so the block only appears when the
// referenced role actually carries guidance.
func appendRoleBlock(groupContext, brief string) string {
	brief = strings.TrimSpace(brief)
	if brief == "" {
		return groupContext
	}
	section := "## Role\n\n" + brief
	if strings.TrimSpace(groupContext) == "" {
		return section
	}
	return groupContext + "\n\n" + section
}

// permOverride is one resolved birth-time permission decision for an
// instantiated template agent: a slug and its effect (grant | deny).
type permOverride struct {
	Slug   string
	Effect string // db.PermEffectGrant | db.PermEffectDeny
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
// Scope note: only the ROLE's default grant list (role.Permissions) feeds access
// here — a role's OWN referenced spawn profile (role.SpawnProfile) contributes
// launch fields (resolveTemplateAgentLaunch) but NOT owner/permission overrides.
// That matches the pre-JOH-350 contract (a role only ever contributed grants,
// never denies or ownership) and keeps the access seam a single, obvious
// profile: the one the AGENT picks. Widening it to role profiles would let a
// role silently deny/own through an indirection, which is deliberately not done.
func resolveTemplateAgentAccess(a db.GroupTemplateAgent, role *db.Role) (bool, []permOverride, *spawnFailure) {
	order := []string{}
	eff := map[string]string{}
	set := func(slug, effect string) {
		slug = strings.TrimSpace(slug)
		if slug == "" {
			return
		}
		if _, ok := eff[slug]; !ok {
			order = append(order, slug)
		}
		eff[slug] = effect
	}
	// Tier 1: the referenced role's default grants.
	if role != nil {
		for _, s := range role.Permissions {
			set(s, db.PermEffectGrant)
		}
	}
	owner := a.IsOwner
	// Tier 2: the agent's referenced spawn profile — its owner default + its
	// grant/deny overrides. Profile slugs are applied in sorted order so a
	// deploy's per-agent grant report is deterministic (the map itself is not).
	if ref := strings.TrimSpace(a.SpawnProfile); ref != "" {
		prof, err := db.GetSpawnProfile(ref)
		if err != nil {
			return false, nil, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		if prof == nil {
			return false, nil, &spawnFailure{http.StatusBadRequest, "invalid_profile",
				fmt.Sprintf("references spawn profile %q which no longer exists", ref)}
		}
		if prof.IsOwner != nil && *prof.IsOwner {
			owner = true
		}
		slugs := make([]string, 0, len(prof.PermissionOverrides))
		for slug := range prof.PermissionOverrides {
			slugs = append(slugs, slug)
		}
		sort.Strings(slugs)
		for _, slug := range slugs {
			set(slug, prof.PermissionOverrides[slug])
		}
	}
	// Tier 3: the agent's own inline grants (legacy per-agent list) — highest.
	for _, s := range a.Permissions {
		set(s, db.PermEffectGrant)
	}

	out := make([]permOverride, 0, len(order))
	for _, s := range order {
		out = append(out, permOverride{Slug: s, Effect: eff[s]})
	}
	return owner, out, nil
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
		// ContextOverride, when non-nil, replaces the template's own
		// default_context as the base the group's startup context is composed
		// from (JOH-356) — the group gets its OWN edited copy of the shared
		// context (the "Form a party" picker prefills this field from the
		// template, then the human edits it before submit; the stored template
		// is untouched). A pointer distinguishes "not supplied — use the
		// template's context" (nil) from "supplied, possibly cleared to empty"
		// (non-nil ""). Existing callers (the instantiate/deploy modals) omit
		// it and keep the template's context verbatim.
		ContextOverride *string `json:"context_override,omitempty"`
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
	descr := strings.TrimSpace(body.Descr)
	if descr == "" {
		descr = "Instantiated from template " + tmpl.Name
	}
	// A plain instantiate records the source template (so the dashboard can
	// still frame it as "from template X") but no mission — that is the
	// deploy verb's addition.
	runInstantiation(w, instantiateSpec{
		tmpl:            tmpl,
		caller:          caller,
		groupName:       body.GroupName,
		assignment:      body.Task,
		contextHeader:   "Task",
		cwd:             cwd,
		descr:           descr,
		sourceTemplate:  tmpl.Name,
		contextOverride: body.ContextOverride,
	})
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
	tmpl           *db.GroupTemplate
	caller         string
	groupName      string // already validated + collision-checked
	assignment     string // the task / mission free text
	contextHeader  string // "Task" | "Mission"
	cwd            string // already resolved
	descr          string // already defaulted
	mission        string // stored on the group row; "" for a plain instantiate
	sourceTemplate string // stored on the group row
	deployed       bool   // frames the response (adds mission + deployed)
	// contextOverride, when non-nil, replaces tmpl.DefaultContext as the base
	// the group context is composed from (JOH-356 — the "Form a party" picker's
	// editable copy of the shared context). nil = use the template's context.
	contextOverride *string
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
	// The base context is the template's default_context unless the caller
	// supplied its own edited copy (JOH-356) — the group's context is its own,
	// the stored template is never mutated.
	baseContext := tmpl.DefaultContext
	if spec.contextOverride != nil {
		baseContext = *spec.contextOverride
	}
	groupContext, err := normalizeGroupContext(composeInstantiationContext(baseContext, spec.assignment, spec.contextHeader))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}

	gid, err := db.CreateAgentGroup(spec.groupName, spec.descr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "create group: "+err.Error())
		return
	}
	// Best-effort post-create config — a failure here is logged, not
	// fatal: the group exists and the human can adjust it on the
	// dashboard. Mirrors the /v1/groups create path.
	if spec.cwd != "" {
		if _, err := db.SetAgentGroupDefaultCwd(spec.groupName, spec.cwd); err != nil {
			slog.Warn("instantiate: set default cwd failed", "group", spec.groupName, "error", err)
		}
	}
	if groupContext != "" {
		if _, err := db.SetAgentGroupDefaultContext(spec.groupName, groupContext); err != nil {
			slog.Warn("instantiate: set default context failed", "group", spec.groupName, "error", err)
		}
	}
	// Deployment provenance (JOH-245): what this force was deployed against
	// and from. Best-effort like the cwd/context above; a blank mission +
	// blank source_template is the "not a deployed force" default, so a
	// no-op write is harmless.
	if spec.mission != "" || spec.sourceTemplate != "" {
		if _, err := db.SetAgentGroupDeployMeta(spec.groupName, spec.mission, spec.sourceTemplate); err != nil {
			slog.Warn("instantiate: set deploy meta failed", "group", spec.groupName, "error", err)
		}
	}

	g := &db.AgentGroup{
		ID: gid, Name: spec.groupName, Descr: spec.descr, DefaultCwd: spec.cwd, DefaultContext: groupContext,
		Mission: spec.mission, SourceTemplate: spec.sourceTemplate,
	}
	granter := granterLabel(spec.caller)

	// Advisory process runtime (JOH-242): if the template carries a process,
	// snapshot it into the group's runtime state at the first phase (recording
	// an initial "" → first-phase transition). Best-effort like the meta writes
	// above; a group with no process simply has no state. The snapshotted phases
	// are ALSO rendered into every agent's ## Process block below.
	if len(tmpl.Process) > 0 {
		if err := db.InitGroupProcess(gid, tmpl.Process, granter); err != nil {
			slog.Warn("instantiate: init process state failed", "group", spec.groupName, "error", err)
		}
	}

	// Seeded rhythms (JOH-244): materialize the template's recurring nudges as
	// normal group cron jobs on the fresh group before any spawn, so they are
	// armed the moment the team comes up. Best-effort; owned by the deploying
	// identity.
	rhythmsCreated := materializeRhythms(g, tmpl.Rhythms, spec.caller)

	// Staged spawn — waves (JOH-244). Partition the roster by wave; spawn wave 0
	// synchronously (so this HTTP call returns real per-agent outcomes), and —
	// when higher waves exist — persist a choreography that the background
	// runner advances as each wave settles. A single-wave template (every agent
	// wave 0) is one synchronous pass, identical to pre-JOH-244 behaviour.
	waves := partitionWaves(tmpl.Agents)
	assignment := normalizeAssignment(spec.assignment)
	// A zero-agent template creates the group (and materializes rhythms) but
	// spawns nobody — mirror the pre-JOH-244 empty-roster behaviour instead of
	// indexing waves[0].
	if len(waves) == 0 {
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
		writeJSON(w, http.StatusCreated, resp)
		return
	}
	// Wave 0 spawns into a fresh group, so there are no prior members to dedupe
	// against (nil existing map).
	wr := spawnWaveAgents(g, waves[0].Agents, tmpl.Process, groupContext, spec.cwd, spec.caller, granter, nil)

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

	if len(waves) == 1 {
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
			GroupID:        gid,
			GroupName:      spec.groupName,
			TemplateName:   tmpl.Name,
			GroupContext:   groupContext,
			Cwd:            spec.cwd,
			Caller:         spec.caller,
			Granter:        granter,
			Assignment:     assignment,
			Process:        tmpl.Process,
			WorkPattern:    tmpl.WorkPattern,
			Waves:          waves,
			MaxWaitSeconds: tmpl.WaveMaxWait,
			NextWave:       1,
			GatingConvs:    wr.SpawnedOrder,
			Activated:      []string{},
			SpawnedConvs:   wr.SpawnedConvs,
			SpawnedOrder:   wr.SpawnedOrder,
			WaveDeadline:   time.Now().Add(waveMaxWaitDuration(tmpl.WaveMaxWait)),
		}
		if err := db.UpsertWaveChoreography(choreo); err != nil {
			slog.Warn("instantiate: persist choreography failed", "group", spec.groupName, "error", err)
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
	writeJSON(w, http.StatusCreated, resp)
}

// handleTemplateDeploy is the first-class "deploy a task force against a
// mission" verb (JOH-245): a thin wrapper over the shared runInstantiation
// core. Gated on templates.instantiate (deploy IS instantiate). Body:
// { mission, group_name?, cwd?, descr? }.
//
// mission is the team's assignment — free text or a Linear epic/issue link
// — and renders into the composed context under "## Mission" (instantiate's
// "## Task" analogue). When group_name is omitted it is DERIVED from the
// mission text (slugged + collision-uniquified); an explicit group_name is
// validated and 409s on a taken name, exactly like instantiate. The chosen
// mission + source template are recorded on the group row so the dashboard
// can show the group as a deployed force.
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
		Mission   string `json:"mission"`
		GroupName string `json:"group_name,omitempty"`
		Cwd       string `json:"cwd,omitempty"`
		Descr     string `json:"descr,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	mission := strings.TrimSpace(body.Mission)
	if mission == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "mission is required (the topic / problem / epic to deploy against)")
		return
	}

	groupName := strings.TrimSpace(body.GroupName)
	if groupName == "" {
		// Derive a sensible group name from the mission, uniquified against
		// existing groups. deriveGroupNameFromMission returns an already-valid,
		// already-free name.
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

	cwd, err := resolveSpawnCwd(body.Cwd)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_cwd", err.Error())
		return
	}

	descr := strings.TrimSpace(body.Descr)
	if descr == "" {
		descr = "Task force deployed from template " + tmpl.Name
	}
	runInstantiation(w, instantiateSpec{
		tmpl:           tmpl,
		caller:         caller,
		groupName:      groupName,
		assignment:     mission,
		contextHeader:  "Mission",
		cwd:            cwd,
		descr:          descr,
		mission:        mission,
		sourceTemplate: tmpl.Name,
		deployed:       true,
	})
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
			writeJSON(w, http.StatusCreated, fromGroupCreateJSON{
				templateJSON: templateToJSON(t),
				BlankBriefs:  countBlankBriefs(t),
			})
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
			// The role REFERENCE is likewise blueprint curation, not an
			// observable property of a live member — a re-snapshot preserves a
			// curated role_ref on name-match, exactly like the spawn-profile
			// ref and the brief (JOH-240).
			if prev.RoleRef != "" {
				t.Agents[i].RoleRef = prev.RoleRef
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
			BlankBriefs:  countBlankBriefs(t),
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
	// BlankBriefs counts agents still left with a blank per-agent brief after
	// the snapshot — see countBlankBriefs (JOH-344).
	BlankBriefs int `json:"blank_briefs"`
}

// fromGroupCreateJSON is the create-mode from-group response: the fresh
// template plus the blank-brief count (JOH-344). A from-group snapshot cannot
// recover per-agent briefs from a live group, so every agent comes through
// blank; blank_briefs lets the CLI/dashboard warn that a deploy of this
// template would tell its agents nothing. templateJSON embeds flat, so a
// consumer that only reads the template shape is unaffected.
type fromGroupCreateJSON struct {
	templateJSON
	BlankBriefs int `json:"blank_briefs"`
}

// countBlankBriefs counts template agents whose per-agent brief
// (initial_message) is blank — the agents a deploy would spawn with nothing to
// do. A from-group snapshot has no briefs to trace from a live group, so a
// fresh snapshot's agents are all blank; an update re-snapshot keeps curated
// briefs on name-match, so this reflects only those still empty afterwards.
func countBlankBriefs(t *db.GroupTemplate) int {
	n := 0
	for _, a := range t.Agents {
		if strings.TrimSpace(a.InitialMessage) == "" {
			n++
		}
	}
	return n
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
