package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
)

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
	// The instantiating agent caps every member, through the same rule a
	// child it spawns is capped by.
	if spec.caller != "" {
		parent, fail := loadChildSandboxParent(spec.caller)
		if fail == nil {
			effectiveSandbox, fail = parent.contain(effectiveSandbox, nil, "instantiate the template")
		}
		if fail != nil {
			cleanupDirWriteProofMarkers(spec.proofToken, spec.proofDirs)
			writeSpawnFailure(w, fail)
			return
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
	wr := spawnWaveAgents(g, waves[0].Agents, nil, waveDeploy{
		process:              procPhases,
		groupContext:         groupContext,
		assignmentContext:    assignmentContext,
		cwd:                  spec.cwd,
		sharedWorktreePath:   spec.worktreePath,
		sharedWorktreeBranch: spec.worktreeBranch,
		perAgentWorktrees:    spec.perAgentWorktrees,
		caller:               spec.caller,
		granter:              granter,
		templateName:         tmpl.Name,
		suppressOwner:        reinforce,
		proofToken:           spec.proofToken,
		proofDirs:            spec.proofDirs,
		codexGitCommonDir:    spec.codexGitCommonDir,
		effectiveSandbox:     &effectiveSandbox,
	})

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
