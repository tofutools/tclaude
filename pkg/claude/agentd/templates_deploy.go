package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

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
