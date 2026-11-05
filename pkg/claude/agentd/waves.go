package agentd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
)

// Staged spawn — "waves" (JOH-244). A template agent carries a `wave` int; a
// deploy spawns wave 0 synchronously (so the HTTP call returns with real
// per-agent outcomes) and DEFERS the higher waves to a background runner. Wave
// N+1 starts only once wave N's agents are up and have gone idle — the party's
// marching order: bring up the lead, give it a planning beat, then the rest.
//
// The whole choreography is persisted (group_wave_choreography) and self-
// healing: a daemon restart mid-deploy re-arms the pending waves from the row,
// and a group delete cancels them (DeleteAgentGroup's cleanup tx).

// defaultWaveMaxWait is the fallback cap on how long a wave gate waits for the
// prior wave to go idle before the next wave spawns anyway — the backstop that
// keeps a crashed lead from wedging the force forever. A template may override
// it (GroupTemplate.WaveMaxWait); 0 means "use this default".
const defaultWaveMaxWait = 8 * time.Minute

// waveChoreographyTickInterval is how often the background runner re-checks
// every pending choreography's gate. Kept well under the wave max-wait so a
// wave that settles quickly still advances promptly; a few seconds keeps the
// deferred-wave latency low without the scan being costly (the table is
// normally empty).
var waveChoreographyTickInterval = 4 * time.Second

// waveSpawnResult carries one wave's spawn outcomes: the per-agent results (for
// the HTTP response), the name→conv map + spawn order (accumulated across waves
// for the work-pattern routing), and the settle-gate convs (the successfully
// spawned convs the NEXT wave waits on).
type waveSpawnResult struct {
	Results      []instantiateAgentResult
	Spawned      int
	Failed       int
	SpawnedConvs map[string]string
	SpawnedOrder []string
}

// partitionWaves groups a template's agents into ordered waves by their Wave
// field (ascending). Agents keep their in-wave ordinal order (the roster is
// already ordinal-sorted). A roster whose every agent is wave 0 yields a single
// wave — today's behaviour, one synchronous pass.
func partitionWaves(agents []db.GroupTemplateAgent) []db.WaveGroup {
	byWave := map[int][]db.GroupTemplateAgent{}
	nums := []int{}
	for _, a := range agents {
		if _, ok := byWave[a.Wave]; !ok {
			nums = append(nums, a.Wave)
		}
		byWave[a.Wave] = append(byWave[a.Wave], a)
	}
	sort.Ints(nums)
	out := make([]db.WaveGroup, 0, len(nums))
	for _, n := range nums {
		out = append(out, db.WaveGroup{Wave: n, Agents: byWave[n]})
	}
	return out
}

// waveDeploy is what every wave of one template deployment spawns with: the
// process and contexts each agent is briefed with, its working directory and
// worktrees, who deploys it and who grants its permissions, the template
// name, the owner policy, the write-proof, the pinned Codex git dir and the
// effective sandbox. The runner rebuilds it from the stored choreography for
// each later wave.
type waveDeploy struct {
	process              []db.ProcessPhase
	groupContext         string
	assignmentContext    string
	cwd                  string
	sharedWorktreePath   string
	sharedWorktreeBranch string
	perAgentWorktrees    *db.WavePerAgentWorktrees
	caller               string
	granter              string
	// templateName drives the auto-stamped task-force tag (JOH-380): every agent
	// this wave spawns gets the `tf:<templateName>` tag, so a group holding members
	// from several template deployments (instantiate + reinforce) can tell them
	// apart. Additive (a set — a re-deploy of the same template re-stamps the same
	// tag, an INSERT OR IGNORE no-op) and best-effort (a failed stamp only logs;
	// the agent is already spawned). Empty templateName skips the stamp — the seam
	// stays inert for any non-template caller of this path.
	templateName string
	// suppressOwner drops the per-agent template owner flag: a template may mark an
	// agent as a group owner, but when the roster is deployed INTO an existing group
	// (reinforce mode) ownership is never transferred — the existing group already
	// has its owner. A would-be owner is recorded with OwnerDropped instead of
	// AddAgentGroupOwner. Permission grants still apply; only ownership is dropped.
	// false for the create-new path (a fresh group's template owner IS its owner).
	suppressOwner     bool
	proofToken        string
	proofDirs         []string
	codexGitCommonDir string
	effectiveSandbox  *sandboxpolicy.Snapshot
}

// waveDeployFromChoreography rebuilds a deployment's wave inputs from its
// stored choreography, for the waves the runner spawns after the first.
func waveDeployFromChoreography(c *db.WaveChoreography) waveDeploy {
	return waveDeploy{
		process:              c.Process,
		groupContext:         c.GroupContext,
		assignmentContext:    c.AssignmentContext,
		cwd:                  c.Cwd,
		sharedWorktreePath:   c.WorktreePath,
		sharedWorktreeBranch: c.WorktreeBranch,
		perAgentWorktrees:    c.PerAgentWorktrees,
		caller:               c.Caller,
		granter:              c.Granter,
		templateName:         c.TemplateName,
		suppressOwner:        c.SuppressOwner,
		proofToken:           c.ProofToken,
		proofDirs:            c.ProofDirs,
		codexGitCommonDir:    c.CodexGitCommonDir,
		effectiveSandbox:     effectiveSandboxForWave(c),
	}
}

// spawnWaveAgents spawns one wave's agents via the shared executeSpawn core,
// applying each agent's role behavior/access preset, launch profile, ownership
// and permission grants. Extracted from runInstantiation so the inline first wave and the
// background later waves run the SAME per-agent path. Best-effort per agent: a
// spawn/grant failure is recorded on that agent's result and skips just it —
// the partial-team-on-failure contract, per wave.
//
// existing maps a "<group>-<agent>" final name to a conv that is ALREADY a live
// member — the restart-idempotency guard. The runner spawns a wave and only
// persists the advanced cursor afterward (it needs the spawned conv-ids); a
// crash in that window would otherwise re-spawn the same wave on restart
// (executeSpawn does not dedupe by name). So the runner passes the group's
// current member names here, and an agent whose final name is already present
// is RECORDED (its existing conv threaded into the results) rather than
// re-spawned. nil/empty for the inline first wave (a fresh group has no
// members).
//
// d carries what every wave of one deployment shares; see waveDeploy.
// d carries what every wave of one deployment shares; see waveDeploy.
func spawnWaveAgents(g *db.AgentGroup, agents []db.GroupTemplateAgent, existing map[string]string, d waveDeploy) waveSpawnResult {
	wr := waveSpawnResult{
		Results:      []instantiateAgentResult{},
		SpawnedConvs: map[string]string{},
		SpawnedOrder: []string{},
	}
	for _, a := range agents {
		res, spawned := spawnWaveAgent(g, a, existing, d)
		if !spawned {
			wr.failMember(res)
			continue
		}
		wr.Spawned++
		wr.SpawnedConvs[a.Name] = res.ConvID
		wr.SpawnedOrder = append(wr.SpawnedOrder, res.ConvID)
		wr.Results = append(wr.Results, res)
	}
	return wr
}

// spawnWaveAgent decides one member of a wave. It reuses the conv a prior
// attempt at this wave already spawned, or places the member, resolves its
// roles, launch and access, briefs it and spawns it, then applies its access.
// spawned is false for a refused member, whose res carries the refusal; it is
// true for a reused or spawned member, whose res may still record a failed
// owner grant.
func spawnWaveAgent(g *db.AgentGroup, a db.GroupTemplateAgent, existing map[string]string, d waveDeploy) (res instantiateAgentResult, spawned bool) {
	finalName := g.Name + "-" + a.Name
	res = instantiateAgentResult{Name: a.Name, FinalName: finalName}
	// Restart idempotency: this agent already spawned in a prior (crashed)
	// attempt at this wave — reuse its conv instead of spawning a duplicate.
	if conv, ok := existing[finalName]; ok && conv != "" {
		res.ConvID = conv
		// Re-stamp on the restart-reuse path too: a crash between the prior
		// attempt's spawn and its tag stamp would otherwise lose the tag.
		// Idempotent (INSERT OR IGNORE), so re-stamping an already-tagged
		// reused conv is a no-op.
		stampTaskForceTag(conv, d.templateName)
		return res, true
	}
	place, err := placeWaveAgent(d, a.Name, finalName)
	if err != nil {
		res.Error = "create worktree: " + err.Error()
		return res, false
	}
	res.WorktreePath, res.WorktreeBranch = place.worktreePath, place.worktreeBranch
	// Resolve the behavior/access role from the agent itself, or from its
	// selected spawn profile when the agent leaves role_ref blank.
	roles, rfail := resolveTemplateAgentRoles(a)
	if rfail != nil {
		res.ErrorKind, res.Error = rfail.Kind, rfail.Msg
		return res, false
	}
	launch, lfail := resolveTemplateAgentLaunch(g, a, nil, place.cwd, d.caller)
	if lfail != nil {
		res.ErrorKind, res.Error = lfail.Kind, lfail.Msg
		// The failure is the case that MOST needs the attribution, and it was the
		// one case that had none: this branch used to record the message alone.
		// Since TCL-1110 the harness a member is judged against can come from the
		// group's or the workspace's default profile, so `"opus" is a Claude Code
		// model` can be the whole explanation an operator gets for a template that
		// says nothing about Codex. The echo carries the tier that chose the
		// harness, which is the sentence that makes the other one actionable.
		res.Resolved = templateResolverFailureEcho(launch)
		return res, false
	}
	// Resolve birth-time access before starting the harness. Role composition
	// can fail (for example when two roles grant one slug under incompatible
	// scopes); discovering that after executeSpawn would leave a live agent
	// whose promised access was never applied.
	owner, overrides, afail := resolveTemplateAgentAccess(a, roles)
	if afail != nil {
		res.ErrorKind, res.Error = afail.Kind, afail.Msg
		res.Resolved = templateResolverFailureEcho(launch)
		return res, false
	}
	proof, place, err := prepareWaveCwdProof(d, place)
	if err != nil {
		res.Error = "prepare cwd write-proof: " + err.Error()
		return res, false
	}
	res.WorktreePath = place.worktreePath
	initialMessage, pfail := waveInitialMessage(a.InitialMessage, launch)
	if pfail != nil {
		proof.cleanup()
		res.ErrorKind, res.Error = pfail.Kind, pfail.Msg
		return res, false
	}
	params := templateLaunchSpawnParams(launch)
	params.Name, params.Role, params.Descr = finalName, a.Role, a.Descr
	params.InitialMessage = initialMessage
	params.GroupContext = waveAgentContext(launch, roles, a.Role, d)
	params.Cwd, params.WorktreePath, params.WorktreeBranch = place.cwd, place.worktreePath, place.worktreeBranch
	params.DirWriteProofDirs, params.DirWriteProofToken, params.CwdWriteProofToken = proof.dirs, d.proofToken, proof.cwdToken
	params.CodexGitCommonDirPinned = spawnUsesPinnedGitCommonDir(launch.Harness, launch.Sandbox, launch.SandboxImplementation)
	if params.CodexGitCommonDirPinned {
		params.CodexGitCommonDir = d.codexGitCommonDir
	}
	params.EffectiveSandbox = waveSpawnSandbox(d.effectiveSandbox, launch, g.ID)
	params.explicitLaunch = templateRolesExplicit(a.Role, roles, time.Now())
	params.ReplyToConv, params.SpawnedByConv = d.caller, d.caller
	outcome, fail := executeSpawn(g, params)
	proof.cleanup()
	if fail != nil {
		res.ErrorKind, res.Error = fail.Kind, fail.Msg
		res.Resolved = templateEchoFallback(launch.Notes, launch.Info, launch.Warnings)
		return res, false
	}
	res.Resolved = mergeWaveSpawnEcho(outcome.Resolved, launch)
	res.ConvID = outcome.ConvID

	// Auto-stamp the task-force tag (JOH-380). The synchronous template
	// spawn has already run enrollSpawnedConv (inside executeSpawn), so the
	// actor's agent_id exists and resolves here.
	stampTaskForceTag(outcome.ConvID, d.templateName)

	// Apply the already-validated birth-time access controls: owner +
	// permission overrides from the referenced profile, composed with role
	// grants and legacy inline grants.
	applyWaveAgentAccess(&res, g, outcome.ConvID, owner, overrides, d)
	return res, true
}

// waveAgentPlacement is where one member works: its cwd, and the worktree it
// is attached to (its own per-agent worktree, or the deployment's shared one).
type waveAgentPlacement struct {
	cwd            string
	worktreePath   string
	worktreeBranch string
}

// placeWaveAgent cuts the member's own worktree when the deployment asks for
// per-agent worktrees, making it the cwd under worktree_as_cwd. Otherwise the
// member works in the deployment's cwd and shared worktree.
func placeWaveAgent(d waveDeploy, agentName, finalName string) (waveAgentPlacement, error) {
	place := waveAgentPlacement{
		cwd:            d.cwd,
		worktreePath:   strings.TrimSpace(d.sharedWorktreePath),
		worktreeBranch: strings.TrimSpace(d.sharedWorktreeBranch),
	}
	wt := d.perAgentWorktrees
	if wt == nil {
		return place, nil
	}
	branch := perAgentBranchName(wt.BranchPrefix, agentName, finalName)
	path, err := worktree.AddWorktreeIn(wt.Repo, branch, wt.FromBranch, "")
	if err != nil {
		return waveAgentPlacement{}, err
	}
	place.worktreePath, place.worktreeBranch = path, branch
	if wt.WorktreeAsCwd {
		place.cwd = path
	}
	return place, nil
}

// waveCwdProof is the write-proof one member's spawn carries: the token proving
// its cwd (empty when its cwd is not proofed), the proofed dirs, and a cleanup
// that removes any marker planted for this member alone.
type waveCwdProof struct {
	cwdToken string
	dirs     []string
	cleanup  func()
}

// prepareWaveCwdProof proves the member's cwd. A member in the deployment's cwd
// carries the deployment's token for it. A member whose cwd is its own
// per-agent worktree gets a marker planted there, provided the worktree's
// parent was proofed, and is re-placed at the worktree's resolved path. Any
// other cwd goes unproofed.
func prepareWaveCwdProof(d waveDeploy, place waveAgentPlacement) (waveCwdProof, waveAgentPlacement, error) {
	proof := waveCwdProof{dirs: d.proofDirs, cleanup: func() {}}
	switch {
	case d.proofToken != "" && place.cwd == d.cwd:
		proof.cwdToken = d.proofToken
	case d.proofToken != "" && d.perAgentWorktrees != nil && d.perAgentWorktrees.WorktreeAsCwd && place.cwd == place.worktreePath:
		resolved, dirs, cleanup, err := preparePerAgentCwdWriteProof(d.proofToken, d.proofDirs, place.cwd)
		if err != nil {
			return waveCwdProof{}, place, err
		}
		place.cwd, place.worktreePath = resolved, resolved
		proof = waveCwdProof{cwdToken: d.proofToken, dirs: dirs, cleanup: cleanup}
	}
	return proof, place, nil
}

// waveInitialMessage puts the launch's saved prompts ahead of the member's own
// initial message. A shell member takes no prompts.
func waveInitialMessage(msg string, launch templateAgentLaunch) (string, *spawnFailure) {
	if len(launch.PromptRefs) == 0 || launch.Harness == harness.ShellName {
		return msg, nil
	}
	return composeSavedPrompts(launch.PromptRefs, msg)
}

// waveSpawnSandbox is the member's own copy of the deployment's effective
// sandbox, carrying its launch environment and resolved against its group.
// nil when the deployment has none.
func waveSpawnSandbox(base *sandboxpolicy.Snapshot, launch templateAgentLaunch, groupID int64) *sandboxpolicy.Snapshot {
	if base == nil {
		return nil
	}
	copy := *base
	copy.LaunchEnvironment = append([]sandboxpolicy.EnvironmentEntry(nil), launch.Environment...)
	copy.RefreshGroupEnvironment = true
	copy.LaunchEnvironmentOverrides = append(
		[]sandboxpolicy.EnvironmentEntry(nil), launch.EnvironmentOverrides...)
	copy.ResolutionGroupID = groupID
	return &copy
}

// templateLaunchSpawnParams is the spawn request a resolved template launch
// asks for: harness, model, effort, sandbox, approval and every launch toggle,
// with the tiers that chose them. The member's identity, placement, brief and
// write-proof are the caller's to add.
func templateLaunchSpawnParams(launch templateAgentLaunch) spawnParams {
	return spawnParams{
		ProfileContext:              launch.StartupContext,
		Harness:                     launch.Harness,
		HarnessSource:               launch.HarnessSource,
		Model:                       launch.Model,
		ModelProxy:                  launch.ModelProxy,
		ModelSource:                 launch.ModelSource,
		Effort:                      launch.Effort,
		EffortSource:                launch.EffortSource,
		HarnessBuiltinMode:          launch.Sandbox,
		HarnessBuiltinModeSource:    launch.SandboxSource,
		SandboxImplementation:       launch.SandboxImplementation,
		SandboxImplementationSource: launch.SandboxImplementationSource,
		ApprovalPolicy:              launch.Approval,
		ToolGovernance:              launch.ToolGovernance,
		TrustDir:                    launch.TrustDir,
		TrustDirSet:                 launch.TrustDirSet,
		AutoReview:                  launch.AutoReview,
		AutoReviewSet:               launch.AutoReviewSet,
		RemoteControl:               launch.RemoteControl,
		AutoMemory:                  launch.AutoMemory,
		PeerMessaging:               launch.PeerMessaging,
		launchTogglesResolved:       true,
		UsageSpawnProfile:           launch.UsageSpawnProfile,
		spawnCopyCallerTiers:        launch.SpawnCopyTiers,
		relaunchOverrides:           launch.RelaunchOverrides,
		defaultProfiles:             launch.DefaultProfiles,
		SSHWorkaround:               launch.SSHWorkaround,
		SSHWorkaroundSet:            launch.SSHWorkaroundSet,
		SSHWorkaroundSource:         launch.SSHWorkaroundSource,
		ContextFeatures:             launch.ContextFeatures,
		AutoCompactWindow:           launch.AutoCompactWindow,
		ContextWindowMax:            launch.ContextWindowMax,
		ContextWindowMaxSource:      launch.ContextWindowMaxSource,
		CopilotAPI:                  launch.CopilotAPI,
		CopilotAPISet:               launch.CopilotAPISet,
		CopilotAPISource:            launch.CopilotAPISource,
		CodexAppServer:              launch.CodexAppServer,
		CodexAppServerSet:           launch.CodexAppServerSet,
		CodexAppServerSource:        launch.CodexAppServerSource,
		FastMode:                    launch.FastMode,
		FastModeSet:                 launch.FastModeSet,
		FastModeSource:              launch.FastModeSource,
		AskUserQuestionTimeout:      launch.AskUserQuestionTimeout,
	}
}

// mergeWaveSpawnEcho completes a spawned member's launch echo with the
// template-tier disclosures from launch, so one echo per member tells the whole
// story: one per-agent disclosure channel, not a template-tier list beside a
// spawn-tier one. The echo is built INSIDE the spawn, because the group/global
// default tiers are applied there: anything they decided is invisible to the
// template-tier resolution. A refused spawn reports the template tiers alone
// (templateEchoFallback), since a disclosure about the tiers that produced the
// attempted launch is exactly what explains a failure like "that profile's
// model is not valid here".
func mergeWaveSpawnEcho(echo *agent.ResolvedLaunch, launch templateAgentLaunch) *agent.ResolvedLaunch {
	if echo == nil {
		return templateEchoFallback(launch.Notes, launch.Info, launch.Warnings)
	}
	// Template tiers first, then the spawn's own: the reader gets them in
	// resolution order. Copied rather than appended in place, because
	// launch.Notes is the template walk's own slice and append could otherwise
	// write through its spare capacity.
	// Deduplicated: a default-tier value the template walk skipped is
	// skipped again by the spawn's own pass over the same pinned tiers.
	echo.Notes = uniqueNotes(append(append([]string(nil), launch.Notes...), echo.Notes...))
	echo.Info = append(append([]string(nil), launch.Info...), echo.Info...)
	echo.Warnings = append(append([]string(nil), launch.Warnings...), echo.Warnings...)
	return echo
}

// waveAgentContext composes a member's startup context from the deployment's
// group and assignment contexts, its roles' briefs and the template process.
func waveAgentContext(launch templateAgentLaunch, roles []*db.Role, memberRole string, d waveDeploy) string {
	// Fold the role brief ("## Role") + the template process ("## Process")
	// into THIS agent's startup context — no-ops when absent.
	// The group's standing context follows include_group_context; the
	// mission/task, role and process blocks are always delivered.
	agentContext := ""
	if launch.IncludeGroupContext {
		agentContext = d.groupContext
	}
	agentContext = joinContextBlocks(agentContext, d.assignmentContext)
	for _, role := range roles {
		agentContext = appendRoleBlock(agentContext, role.Brief)
	}
	agentContext = appendProcessBlock(agentContext, d.process, memberRole)
	return agentContext
}

// applyWaveAgentAccess applies a spawned member's already-validated
// birth-time access: group ownership (dropped in reinforce mode) and the
// permission overrides from its profile, roles and inline grants. A failure
// only logs, or records on res: the member is already spawned.
func applyWaveAgentAccess(res *instantiateAgentResult, g *db.AgentGroup, conv string,
	owner bool, overrides []permOverride, d waveDeploy) {
	if owner {
		switch {
		case d.suppressOwner:
			// Reinforce mode: never transfer ownership of an existing group.
			res.OwnerDropped = true
		default:
			if err := db.AddAgentGroupOwner(g.ID, conv, d.granter); err != nil {
				slog.Warn("wave spawn: grant owner failed",
					"group", g.Name, "conv", conv, "error", err)
				res.Error = "spawned, but grant-owner failed: " + err.Error()
			} else {
				res.Owner = true
			}
		}
	}
	for _, ov := range overrides {
		if err := db.SetAgentPermissionOverrideWithScope(conv, ov.Slug,
			ov.Override.Effect, ov.Override.Scope, d.granter); err != nil {
			slog.Warn("wave spawn: apply permission override failed",
				"conv", conv, "slug", ov.Slug, "effect", ov.Override.Effect,
				"scope", ov.Override.Scope, "error", err)
			continue
		}
		if ov.Override.Effect == db.PermEffectGrant {
			res.Granted = append(res.Granted, ov.Slug)
		}
	}
}

// failMember records a member that did NOT spawn, and blanks its worktree fields
// on the way out. A per-agent worktree is created before the launch is resolved
// and before the spawn is attempted, so a failed member can genuinely have one —
// but nothing is attached to it, and a response naming a path for an agent that
// does not exist asserts a relationship that was never formed. The operator then
// goes looking for an agent in that directory.
//
// The worktree and its branch are NOT removed here: that is a real leak, made
// much more reachable by TCL-1110 (a configuration fault fails EVERY member of a
// roster rather than one by accident), and it is tracked as TCL-1115 rather than
// fixed inside a launch-resolution change. This function fixes the lie, not the
// leak — and the two are different bugs even though they share a line.
func (wr *waveSpawnResult) failMember(res instantiateAgentResult) {
	res.WorktreePath = ""
	res.WorktreeBranch = ""
	wr.Failed++
	wr.Results = append(wr.Results, res)
}

// templateResolverFailureEcho builds the echo for a member the RESOLVER
// refused, from the partial launch it carried out with the failure. Only the
// harness is populated: it is the one field decided before any field-level
// validation can fail, and it is the one an operator cannot otherwise infer —
// the rest were never resolved, and inventing values for them would describe a
// launch that does not exist.
//
// Returns nil when the resolver failed before choosing a harness (a profile that
// vanished, a profile the caller may not use). Those failures name their own
// cause already, and an echo attributing a harness nobody had chosen yet would
// be worse than none.
func templateResolverFailureEcho(partial templateAgentLaunch) *agent.ResolvedLaunch {
	if strings.TrimSpace(partial.Harness) == "" {
		return templateEchoFallback(partial.Notes, partial.Info, partial.Warnings)
	}
	return &agent.ResolvedLaunch{
		Harness: agent.ResolvedField{
			Value: partial.Harness, Source: partial.HarnessSource,
		},
		Notes: partial.Notes, Info: partial.Info, Warnings: partial.Warnings,
	}
}

// templateEchoFallback carries the template-tier disclosures on their own when
// there is no spawn echo to hang them on — a member whose spawn was REFUSED, or
// (defensively) an outcome from a path that produced none. A refusal is exactly
// when the tiers that produced the refused value are worth reading.
func templateEchoFallback(notes, info, warnings []string) *agent.ResolvedLaunch {
	if len(notes) == 0 && len(info) == 0 && len(warnings) == 0 {
		return nil
	}
	return &agent.ResolvedLaunch{Notes: notes, Info: info, Warnings: warnings}
}

func preparePerAgentCwdWriteProof(token string, proofDirs []string, cwd string) (string, []string, func(), error) {
	resolvedCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", nil, nil, err
	}
	parent := filepath.Dir(resolvedCwd)
	if !dirListContains(proofDirs, parent) {
		return "", nil, nil, fmt.Errorf("worktree parent %s was not write-proofed", parent)
	}
	marker := filepath.Join(resolvedCwd, dirWriteProofFilePrefix+token)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		return "", nil, nil, err
	}
	dirs := append([]string{}, proofDirs...)
	if !dirListContains(dirs, resolvedCwd) {
		dirs = append(dirs, resolvedCwd)
	}
	return resolvedCwd, dirs, func() { _ = os.Remove(marker) }, nil
}

func dirListContains(dirs []string, want string) bool {
	for _, dir := range dirs {
		if dir == want {
			return true
		}
	}
	return false
}

func perAgentBranchName(prefix, agentName, finalName string) string {
	base := strings.TrimSpace(prefix)
	if base != "" {
		base += "-" + strings.TrimSpace(agentName)
	} else {
		base = finalName
	}
	name := agent.NormalizeSpawnName(base)
	if name == "" {
		name = agent.NormalizeSpawnName(finalName)
	}
	if name == "" {
		name = "agent-worktree"
	}
	return name
}

// stampTaskForceTag stamps the auto task-force tag `tf:<templateName>` on
// the actor behind conv (JOH-380). A blank templateName or conv is a
// no-op. Best-effort: it resolves the conv to its stable agent_id and
// unions the tag onto the agent's set (db.AddAgentTags, INSERT OR IGNORE),
// logging — never failing — since the agent is already spawned. It runs
// for BOTH the synchronous wave-0 spawn and the background choreography
// waves, so every template-deployed member carries the marker.
//
// db.TaskForceTag guarantees a storable tag (it strips tag-invalid chars
// and length-truncates), so the only remaining way the stamp can be
// refused is the per-agent tag COUNT cap — an agent already carrying
// MaxAgentTags free-form tags keeps them and the system marker degrades
// to the logged warning below. That is an accepted residual: it needs an
// operator to have manually filled all 16 tag slots, and the marker is
// still recoverable (re-deploy after trimming a tag), so we don't special-
// case the cap here.
func stampTaskForceTag(conv, templateName string) {
	tag := db.TaskForceTag(templateName)
	if tag == "" || conv == "" {
		return
	}
	agentID, err := db.AgentIDForConv(conv)
	if err != nil {
		slog.Warn("wave spawn: resolve agent for task-force tag failed",
			"conv", conv, "template", templateName, "error", err)
		return
	}
	if agentID == "" {
		slog.Warn("wave spawn: no actor to stamp task-force tag",
			"conv", conv, "template", templateName)
		return
	}
	if err := db.AddAgentTags(agentID, tag); err != nil {
		slog.Warn("wave spawn: stamp task-force tag failed",
			"agent", agentID, "template", templateName, "tag", tag, "error", err)
	}
}

// groupMemberNames maps a group's live members by their effective name
// ("<group>-<agent>", the spawn's rename title / pending name) to their conv —
// the restart-idempotency key spawnWaveAgents dedupes against. agent.CachedTitle
// resolves a member's custom title, falling back to the pending name set at
// enrollment (before the fork), so a just-spawned-but-not-yet-titled member is
// still matched.
func groupMemberNames(g *db.AgentGroup) map[string]string {
	out := map[string]string{}
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		slog.Warn("wave runner: member-name map failed", "group", g.Name, "error", err)
		return out
	}
	for _, m := range members {
		if name := agent.CachedTitle(m.ConvID); name != "" {
			out[name] = m.ConvID
		}
	}
	return out
}

// normalizeAssignment folds CRLF → LF on the per-run assignment so the
// work-pattern's per-step charset re-gate (isValidInitialMessage rejects '\r')
// doesn't flunk a CRLF-authored --task/--mission file. Trimmed.
func normalizeAssignment(assignment string) string {
	assignment = strings.TrimSpace(assignment)
	assignment = strings.ReplaceAll(assignment, "\r\n", "\n")
	return strings.ReplaceAll(assignment, "\r", "\n")
}

// deliverWorkPattern delivers a template's work pattern (JOH-336) once the full
// roster is up — each step routed to one member by template-name or broadcast
// to every spawned member ("all"), with {{task}}/{{mission}} interpolated.
// Extracted from runInstantiation so both the single-wave inline path and the
// staged final-wave path deliver identically. Returns (delivered, errors);
// best-effort — a step that can't route is reported, never aborts the rest.
func deliverWorkPattern(g *db.AgentGroup, pattern []db.WorkPatternEntry, templateName, assignment, caller string,
	spawnedConvs map[string]string, spawnedOrder []string, rosterNames map[string]bool) (int, []string) {
	delivered := 0
	errs := []string{}
	for i, e := range pattern {
		msg := strings.ReplaceAll(e.Value, "{{task}}", assignment)
		msg = strings.ReplaceAll(msg, "{{mission}}", assignment)
		if msg == "" {
			errs = append(errs, fmt.Sprintf("step %d/%d (to %s): interpolated to an empty message — not sent",
				i+1, len(pattern), e.SendTo))
			continue
		}
		if !isValidInitialMessage(msg) {
			errs = append(errs, fmt.Sprintf("step %d/%d (to %s): interpolated message breaks the inbox charset/length rule — not sent",
				i+1, len(pattern), e.SendTo))
			continue
		}
		var targets []string
		switch e.SendTo {
		case "all":
			targets = spawnedOrder
			if len(targets) == 0 {
				errs = append(errs, fmt.Sprintf("step %d/%d: no members spawned — not sent", i+1, len(pattern)))
				continue
			}
		default:
			conv, ok := spawnedConvs[e.SendTo]
			if !ok {
				if rosterNames[e.SendTo] {
					errs = append(errs, fmt.Sprintf("step %d/%d: target %q did not spawn — not sent", i+1, len(pattern), e.SendTo))
				} else {
					errs = append(errs, fmt.Sprintf("step %d/%d: target %q is not in the roster (stale work-pattern step?) — not sent",
						i+1, len(pattern), e.SendTo))
				}
				continue
			}
			targets = []string{conv}
		}
		subject := fmt.Sprintf("[work-pattern %d/%d] %s", i+1, len(pattern), templateName)
		for _, conv := range targets {
			if _, err := queueAgentMessage(&db.AgentMessage{
				GroupID:      g.ID,
				FromConv:     caller,
				ToConv:       conv,
				Subject:      subject,
				Body:         msg,
				ToRecipients: targets,
			}); err != nil {
				slog.Warn("work-pattern insert failed", "group", g.Name, "step", i+1, "conv", conv, "error", err)
				errs = append(errs, fmt.Sprintf("step %d/%d (to %s): %v", i+1, len(pattern), e.SendTo, err))
				continue
			}
			delivered++
		}
	}
	return delivered, errs
}

// rosterNameSet is the set of a template's agent names (work-pattern routing
// checks send_to against it).
func rosterNameSet(agents []db.GroupTemplateAgent) map[string]bool {
	out := make(map[string]bool, len(agents))
	for _, a := range agents {
		out[a.Name] = true
	}
	return out
}

// pendingAgentCount is the number of agents across a choreography's not-yet-
// spawned waves (Waves[NextWave:]).
func pendingAgentCount(c *db.WaveChoreography) int {
	n := 0
	for i := c.NextWave; i < len(c.Waves); i++ {
		n += len(c.Waves[i].Agents)
	}
	return n
}

// waveMaxWaitDuration resolves a template's configured max-wait (seconds) to a
// duration, falling back to the built-in default when unset (0).
func waveMaxWaitDuration(seconds int) time.Duration {
	if seconds <= 0 {
		return defaultWaveMaxWait
	}
	return time.Duration(seconds) * time.Second
}

// waveStatusJSON is the lean wire shape of a group's staged-spawn status
// (JOH-244): the current wave (1-based) + total, and how many waves/agents are
// still pending. Surfaced in the group GET + the dashboard snapshot; absent
// once the choreography completes.
type waveStatusJSON struct {
	// CurrentWave is the highest wave spawned so far (1-based).
	CurrentWave int `json:"current_wave"`
	// TotalWaves is the total number of waves in this deploy.
	TotalWaves int `json:"total_waves"`
	// PendingWaves / PendingAgents are what is still to come.
	PendingWaves  int `json:"pending_waves"`
	PendingAgents int `json:"pending_agents"`
	// DeadlineAt is when the current gate's max-wait fires (RFC3339); the next
	// wave spawns then regardless of idle state.
	DeadlineAt string `json:"deadline_at,omitempty"`
}

// waveStatusToJSON projects a choreography row onto the lean status shape.
func waveStatusToJSON(c *db.WaveChoreography) waveStatusJSON {
	out := waveStatusJSON{
		CurrentWave:   c.NextWave, // NextWave is the next to spawn; the already-spawned count == NextWave (1-based current)
		TotalWaves:    len(c.Waves),
		PendingWaves:  c.PendingWaves(),
		PendingAgents: pendingAgentCount(c),
	}
	if !c.WaveDeadline.IsZero() {
		out.DeadlineAt = c.WaveDeadline.Format(time.RFC3339)
	}
	return out
}

// loadWaveStatus returns a group's staged-spawn status, or nil when the group
// has no pending choreography (single-wave deploy, or the deploy has completed).
func loadWaveStatus(groupID int64) *waveStatusJSON {
	c, err := db.GetWaveChoreography(groupID)
	if err != nil {
		slog.Warn("wave status: load failed", "group_id", groupID, "error", err)
		return nil
	}
	if c == nil {
		return nil
	}
	v := waveStatusToJSON(c)
	return &v
}

// handleGroupWavesGet serves GET /v1/groups/{name}/waves: the group's pending
// staged-spawn status, or 404 when the group has no pending choreography. Open
// + read-only, like the process GET.
func handleGroupWavesGet(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET")
		return
	}
	c, err := db.GetWaveChoreography(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if c == nil {
		writeError(w, http.StatusNotFound, "no_waves", "this group has no pending staged-spawn waves")
		return
	}
	writeJSON(w, http.StatusOK, waveStatusToJSON(c))
}

// startWaveChoreographyRunner runs the staged-spawn background runner in its
// own goroutine until stop closes (the daemon-wide quit channel). It sweeps
// every pending choreography each tick, spawning the next wave when its gate
// releases. Restart-safe — the durable group_wave_choreography table is the
// whole state, so a daemon that restarts mid-deploy resumes from it. The first
// sweep fires immediately so a restart re-arms pending waves without waiting a
// full interval.
func startWaveChoreographyRunner(stop <-chan struct{}) {
	go func() {
		sweepWaveChoreographies()
		t := time.NewTicker(waveChoreographyTickInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				sweepWaveChoreographies()
			}
		}
	}()
}

// sweepWaveChoreographies is one pass over every pending choreography row.
func sweepWaveChoreographies() {
	rows, err := db.ListWaveChoreographies()
	if err != nil {
		slog.Warn("wave runner: list failed", "error", err)
		return
	}
	for _, c := range rows {
		advanceChoreographyIfReady(c)
	}
}

// advanceChoreographyIfReady checks one choreography's gate and, if the current
// wave has settled (all gating convs up-and-idle or dead) OR the max-wait
// deadline has passed, spawns the next wave. When the last wave lands it
// delivers the work pattern and drops the row.
func advanceChoreographyIfReady(c *db.WaveChoreography) {
	// The group may have been deleted (delete cancels choreography in the same
	// tx, but a sweep can race a delete that hasn't committed our view yet, or a
	// stray row could outlive its group). Resolve fresh; a missing group ends
	// the choreography — self-healing.
	g, err := db.GetAgentGroupByID(c.GroupID)
	if err != nil {
		slog.Warn("wave runner: group lookup failed", "group", c.GroupName, "error", err)
		return
	}
	if g == nil {
		slog.Info("wave runner: group gone; dropping choreography", "group", c.GroupName)
		_ = db.DeleteWaveChoreography(c.GroupID)
		cleanupDirWriteProofMarkers(c.ProofToken, c.ProofDirs)
		return
	}
	if c.NextWave >= len(c.Waves) {
		// Nothing left to spawn — a stale row (shouldn't happen; the last-wave
		// path deletes it). Clean it up.
		_ = db.DeleteWaveChoreography(c.GroupID)
		cleanupDirWriteProofMarkers(c.ProofToken, c.ProofDirs)
		return
	}

	released, changed := gateReleased(c)
	if changed {
		// A new activation was observed this tick — persist it so the flag
		// survives a restart, even if the gate hasn't fully released yet.
		if err := db.UpsertWaveChoreography(c); err != nil {
			slog.Warn("wave runner: persist activation failed", "group", c.GroupName, "error", err)
		}
	}
	if !released {
		return
	}

	// Spawn the next wave. Pass the group's current member names so a wave that
	// was spawned but not yet persisted (a crash-in-window on the prior run) is
	// recognised and not duplicated — restart idempotency.
	wave := c.Waves[c.NextWave]
	slog.Info("wave runner: spawning wave", "group", c.GroupName, "wave", wave.Wave,
		"agents", len(wave.Agents), "index", c.NextWave, "of", len(c.Waves))
	wr := spawnWaveAgents(g, wave.Agents, groupMemberNames(g), waveDeployFromChoreography(c))

	// Accumulate the spawns for the final work-pattern routing.
	maps.Copy(c.SpawnedConvs, wr.SpawnedConvs)
	c.SpawnedOrder = append(c.SpawnedOrder, wr.SpawnedOrder...)

	c.NextWave++
	if c.NextWave >= len(c.Waves) {
		// Final wave is up — the roster is complete. Deliver the work pattern
		// (deferred until now precisely because the roster wasn't whole before)
		// and drop the choreography row.
		rosterNames := map[string]bool{}
		for _, wv := range c.Waves {
			for _, a := range wv.Agents {
				rosterNames[a.Name] = true
			}
		}
		delivered, patErrs := deliverWorkPattern(g, c.WorkPattern, c.TemplateName, c.Assignment, c.Caller,
			c.SpawnedConvs, c.SpawnedOrder, rosterNames)
		slog.Info("wave runner: choreography complete", "group", c.GroupName,
			"pattern_delivered", delivered, "pattern_errors", len(patErrs))
		if err := db.DeleteWaveChoreography(c.GroupID); err != nil {
			slog.Warn("wave runner: delete choreography failed", "group", c.GroupName, "error", err)
		}
		cleanupDirWriteProofMarkers(c.ProofToken, c.ProofDirs)
		return
	}

	// More waves remain — this newly-spawned wave becomes the next gate. Reset
	// the deadline and the activation set for the fresh cohort.
	c.GatingConvs = wr.SpawnedOrder
	c.Activated = []string{}
	c.WaveDeadline = time.Now().Add(waveMaxWaitDuration(c.MaxWaitSeconds))
	// A group delete can land during the spawn above (delete cancels the
	// choreography in its own tx). Re-check before persisting so a raced delete
	// isn't undone by re-inserting (upserting) the row we just deleted — a
	// resurrected row for a dead group. If the group is gone, drop the
	// choreography instead of resurrecting it (self-healing).
	if still, err := db.GetAgentGroupByID(c.GroupID); err == nil && still == nil {
		slog.Info("wave runner: group deleted mid-spawn; dropping choreography", "group", c.GroupName)
		_ = db.DeleteWaveChoreography(c.GroupID)
		cleanupDirWriteProofMarkers(c.ProofToken, c.ProofDirs)
		return
	}
	if err := db.UpsertWaveChoreography(c); err != nil {
		slog.Warn("wave runner: persist advance failed", "group", c.GroupName, "error", err)
	}
}

// effectiveSandboxForWave gives pre-snapshot choreography rows an explicit
// empty policy rather than resolving mutable assignments at run time. New rows
// always carry a snapshot; the fallback is a safe compatibility weakening.
func effectiveSandboxForWave(c *db.WaveChoreography) *sandboxpolicy.Snapshot {
	if c.EffectiveSandbox != nil {
		return c.EffectiveSandbox
	}
	empty := sandboxpolicy.EmptySnapshot()
	return &empty
}

// waveConvDead reports whether a session status means the member is done/dead
// (a crashed, exited, or errored spawn) — which, for the wave gate, counts as
// "done waiting": dead ≠ busy, so it never wedges the next wave.
func waveConvDead(status string) bool {
	return status == session.StatusExited || status == session.StatusError
}

// gateReleased reports whether the current wave's gate should release — i.e.
// the next wave may spawn. It returns (released, changed): `changed` is true
// when this check observed a new activation that the caller should persist.
//
// The gate releases when the max-wait deadline has passed OR every gating conv
// has SETTLED. A conv is settled when it is dead/gone (a failed or reaped
// member — dead ≠ busy, it doesn't block) OR it has been observed WORKING at
// least once (persisted in c.Activated) AND is now idle. The
// observed-working-then-idle rule matters because a freshly spawned agent
// starts idle (it hasn't begun its turn); releasing on that first idle would
// skip the planning beat the wave exists to grant.
//
// Latency note: the runner samples every waveChoreographyTickInterval, so a
// conv that flips working→idle entirely between two ticks is never observed
// working and can only advance via the max-wait deadline. In practice a real
// turn (the lead's planning beat) far exceeds the tick, so this is a rare
// worst-case, not the norm — the deadline is the correct backstop for it.
func gateReleased(c *db.WaveChoreography) (bool, bool) {
	changed := false
	if len(c.GatingConvs) == 0 {
		return true, changed // nothing to wait on (whole prior wave failed to spawn)
	}
	// Deadline backstop — but still fold in any activation we observe so the
	// persisted flag is honest (cheap; the caller persists on `changed`).
	deadlinePassed := !c.WaveDeadline.IsZero() && time.Now().After(c.WaveDeadline)

	activated := map[string]bool{}
	for _, conv := range c.Activated {
		activated[conv] = true
	}
	allSettled := true
	for _, conv := range c.GatingConvs {
		s, err := db.FindSessionByConvID(conv)
		if err != nil {
			// Transient read error — don't release on a DB hiccup; treat as
			// not-yet-settled (the deadline still backstops a persistent fault).
			allSettled = false
			continue
		}
		if s == nil {
			// No session row YET. In production `tclaude session new` writes the
			// row asynchronously after the fork, so a just-spawned gating conv is
			// legitimately row-less for a beat — treat that as "still coming up"
			// and HOLD the gate (never release on a not-yet-written row, which
			// would skip the beat entirely). A conv whose row genuinely vanished
			// (reaped/deleted) also holds here, but the deadline backstops it — a
			// truly-dead member surfaces as StatusExited via the reaper, caught
			// below, not as a missing row.
			allSettled = false
			continue
		}
		if waveConvDead(s.Status) {
			continue // exited / errored → dead ≠ busy, does not block
		}
		if s.Status == session.StatusWorking {
			// Actively in a turn — mark it activated (it has had, or is having,
			// its beat) and hold the gate.
			if !activated[conv] {
				activated[conv] = true
				c.Activated = append(c.Activated, conv)
				changed = true
			}
			allSettled = false
			continue
		}
		// Not working. Settled only if we've seen it work (had its beat);
		// otherwise it's still coming online — keep waiting (deadline backstops).
		if !activated[conv] {
			allSettled = false
		}
	}
	return deadlinePassed || allSettled, changed
}

// materializeRhythms turns a template's rhythms into normal group cron jobs on
// the freshly-instantiated group (JOH-244). Each becomes a job named
// "<group>-<rhythm>", targeting the group with the rhythm's role filter,
// owned by the deploying identity. Best-effort: a rhythm that fails to
// materialize is logged and skipped, never aborts the deploy. Returns the count
// created. Rhythms are validated at template save, so materialize is a plain
// insert.
func materializeRhythms(g *db.AgentGroup, rhythms []db.Rhythm, ownerConv string) int {
	created := 0
	for _, rh := range rhythms {
		job := &db.AgentCronJob{
			Name:            g.Name + "-" + rh.Name,
			TargetKind:      db.CronTargetGroup,
			GroupID:         g.ID,
			TargetRole:      rh.TargetRole,
			IntervalSeconds: rh.IntervalSeconds,
			CronExpr:        rh.CronExpr,
			Subject:         rh.Subject,
			Body:            rh.Body,
			Enabled:         true,
			OwnerConv:       ownerConv,
		}
		if _, err := db.InsertAgentCronJob(job); err != nil {
			slog.Warn("materialize rhythm failed", "group", g.Name, "rhythm", rh.Name, "error", err)
			continue
		}
		created++
	}
	return created
}

// templateRolesExplicit records a template agent's roles as its own explicit
// launch value: the template resolved them (its agent's role_ref ahead of the
// profile's), and its role briefs went into the group context rather than the
// profile context, so a re-applied spawn profile must not rewrite them.
func templateRolesExplicit(label string, roles []*db.Role, at time.Time) map[string]db.ExplicitLaunchValue {
	names := make([]string, 0, len(roles))
	for _, role := range roles {
		names = append(names, role.Name)
	}
	fields := map[string]json.RawMessage{}
	if raw, err := json.Marshal(label); err == nil {
		fields["Role"] = raw
	}
	if raw, err := json.Marshal(names); err == nil {
		fields["RoleRefs"] = raw
	}
	return map[string]db.ExplicitLaunchValue{"roles": {Fields: fields, Provenance: db.ExplicitAtTemplate, SetAt: at}}
}
