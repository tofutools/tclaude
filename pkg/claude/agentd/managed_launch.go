package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// resolveManagedLaunch resolves a launch the daemon composes itself (trigger
// and process workers, scribe and Steward summons): the default profile tiers
// through applyDefaultProfile, then the sandbox snapshot and the common
// environment, the same inputs a direct spawn into the group gets.
//
// The snapshot is the group's sandbox-profile chain (global, then group),
// unless the resolved launch mode disables sandbox profiles. When parentConv
// names a spawning agent, the snapshot must be contained in that agent's own.
//
// keepUnconfiguredBare leaves the launch with no snapshot at all when no
// sandbox profile applies and no environment resolves. A scribe has always
// launched that way, and an empty snapshot is not equivalent to none (it
// turns on the harness-config floor and the other snapshot-driven launch
// rules), so nothing configured stays exactly as it was.
//
// A profile failure comes back as applyDefaultProfile reported it; the
// sandbox steps fail with kind spawn_failed or as a spawned child's would
// (childSandboxParent.contain).
func resolveManagedLaunch(g *db.AgentGroup, p *spawnParams, parentConv string, keepUnconfiguredBare bool) *spawnFailure {
	if fail := applyDefaultProfile(g, p); fail != nil {
		return fail
	}
	snapshot := sandboxpolicy.DerivedOmittedProfilesSnapshot()
	if !sandboxProfilesDisabled(p.Harness, p.HarnessBuiltinMode, p.SandboxImplementation) {
		resolved, err := db.ResolveEffectiveSandboxSnapshot(g.ID, "")
		if err != nil {
			return &spawnFailure{http.StatusBadRequest, "spawn_failed", err.Error()}
		}
		snapshot = resolved
	}
	// The owning agent caps what is launched on its behalf, through the same
	// rule a child it spawns is capped by.
	if parentConv != "" {
		parent, fail := loadChildSandboxParent(parentConv)
		if fail == nil {
			snapshot, fail = parent.contain(snapshot, nil, "start the worker")
		}
		if fail != nil {
			return fail
		}
	}
	p.EffectiveSandbox = &snapshot
	// A second pass fills the common environment now that there is a
	// snapshot to carry it; every other field is already resolved, so it
	// changes nothing else.
	if fail := applyDefaultProfile(g, p); fail != nil {
		return fail
	}
	if keepUnconfiguredBare && len(snapshot.Applied) == 0 && len(p.EffectiveSandbox.LaunchEnvironment) == 0 {
		p.EffectiveSandbox = nil
	}
	return nil
}

// fixedWorkdirDenial reports the sandbox rule that makes dir unreachable, for
// a launch whose working directory the caller cannot choose (a scribe always
// runs in its shared workdir). Only a deny at exactly dir does: a deny on a
// strict ancestor (deny ~, deny ~/.tclaude) leaves the launch directory
// reachable, because the launch reopens its workspace under a covering deny
// (session.sandboxDenyCoversPath and the workspace reopen beside it), as it
// does for every agent's cwd. "" when dir stays reachable.
func fixedWorkdirDenial(snapshot *sandboxpolicy.Snapshot, dir string) string {
	if snapshot == nil {
		return ""
	}
	dir = canonicalLaunchDir(dir)
	for _, grant := range snapshot.Effective.Filesystem {
		if grant.Access != sandboxpolicy.AccessDeny || grant.IsRemapped() || filepath.Clean(grant.Path) != dir {
			continue
		}
		var names []string
		for _, s := range snapshot.Effective.Provenance.Filesystem[grant.Path] {
			names = append(names, fmt.Sprintf("%q", s.Profile))
		}
		if len(names) == 0 {
			return fmt.Sprintf("a sandbox rule denies %s", grant.Path)
		}
		return fmt.Sprintf("sandbox profile %s denies %s", strings.Join(names, ", "), grant.Path)
	}
	return ""
}

// canonicalLaunchDir spells dir the way the snapshot spells its grant paths
// (symlinks resolved), for a directory that may not exist yet: the nearest
// existing ancestor is resolved and the missing tail kept.
func canonicalLaunchDir(dir string) string {
	dir = filepath.Clean(dir)
	var tail []string
	for cur := dir; ; {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(append([]string{resolved}, tail...)...)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return dir
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		cur = parent
	}
}

// uniqueNotes keeps the first occurrence of each note, in order.
func uniqueNotes(notes []string) []string {
	if len(notes) < 2 {
		return notes
	}
	seen := make(map[string]bool, len(notes))
	out := notes[:0:0]
	for _, n := range notes {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// managedProfileRefusal checks a spawn profile a managed worker's launch
// reads against ownerConv: the worker's own profile when one is given, and
// with defaults the group's default and the global default (the tiers
// applyDefaultProfile walks). Callers check the defaults after the owner's
// permission, so an owner who may not spawn at all is told that, and before
// any rate slot is spent. A disabled or operator-only profile refuses the
// worker with kind profile_disabled or profile_operator_only and a message
// naming the profile and the tier it sits on.
func managedProfileRefusal(g *db.AgentGroup, profile *db.SpawnProfile, ownerConv string, defaults bool) *spawnFailure {
	type tier struct {
		label   string
		profile *db.SpawnProfile
	}
	tiers := []tier{{"the worker's spawn profile", profile}}
	if defaults && g != nil {
		tiers = append(tiers,
			tier{fmt.Sprintf("the default spawn profile of group %s", g.Name), groupDefaultProfile(g)},
			tier{"the global default spawn profile", globalDefaultProfile()})
	}
	for _, tier := range tiers {
		if fail := profileSpawnFailure(tier.profile, ownerConv); fail != nil {
			return &spawnFailure{fail.Status, fail.Kind, fmt.Sprintf("%s (%s)", fail.Msg, tier.label)}
		}
	}
	return nil
}

// isManagedProfileRefusal reports a refusal managedProfileRefusal produces.
// It repeats on every firing until someone changes the profile, so callers
// record it as a failure and announce the first one.
func isManagedProfileRefusal(kind string) bool {
	return kind == "profile_disabled" || kind == "profile_operator_only"
}

// resolveSpawnCopyRelaunch resolves a spawn copy for relaunch: the copied
// profiles go through applyDefaultProfile as the tiers of the launch, exactly
// as a first spawn's profiles do, with the default tiers pinned to the copy
// so nothing is read live, and harness, model and effort held at the agent's
// stored values.
//
// The result holds only the fields the copy owns: a field with no explicit
// value whose resolved value is what the highest copied profile that sets it
// says. Anything else (a field no copied profile sets, a value the resolver
// skipped for this harness, an explicit value) is left nil, so the stored
// value stands and a harness or install default never replaces it. Before the
// owned fields are taken, the resolution runs again with every field the copy
// does not own seeded from the stored shape, so coupled fields (sandbox mode
// and implementation) resolve against what the agent really has.
func resolveSpawnCopyRelaunch(g *db.AgentGroup, spawnerConv, harnessName string, stored *db.AgentRelaunchProfile,
	spawnCopy db.AgentSpawnCopy, explicit map[string]db.ExplicitLaunchValue) (*db.AgentRelaunchProfile, *spawnFailure) {
	decoded, fail := decodeSpawnCopyTiers(spawnCopy)
	if fail != nil {
		return nil, fail
	}
	profiles, slots, caller := decoded.profiles, &decoded.slots, decoded.caller
	resolve := func(seed func(*spawnParams)) (db.AgentRelaunchProfile, *spawnFailure) {
		pinned := *slots
		p := spawnParams{Harness: harnessName, SpawnedByConv: spawnerConv, managedProfile: caller,
			defaultProfiles: &pinned}
		if stored != nil && stored.ModelID != nil {
			p.Model = *stored.ModelID
		}
		if stored != nil && stored.Effort != nil {
			p.Effort = *stored.Effort
		}
		if seed != nil {
			seed(&p)
		}
		if fail := applyDefaultProfile(g, &p); fail != nil {
			return db.AgentRelaunchProfile{}, fail
		}
		return relaunchProfileForSpawn(p), nil
	}
	// seedExcept feeds every stored value but the candidate fields back in
	// as explicit, so only the candidates resolve from the copy.
	seedExcept := func(candidate []bool) func(*spawnParams) {
		return func(p *spawnParams) {
			for i, f := range spawnCopyRelaunchFields {
				if !candidate[i] && f.seed != nil && stored != nil {
					if v, ok := f.storedValue(stored); ok {
						f.seed(p, v)
					}
				}
			}
		}
	}
	// Candidates are the fields with no explicit value that some copied
	// profile sets; the first pass shows which of them the copy really owns.
	candidate := make([]bool, len(spawnCopyRelaunchFields))
	for i, f := range spawnCopyRelaunchFields {
		if _, ok := explicit[f.key]; !ok && !f.seedOnly {
			candidate[i] = f.copySets(profiles)
		}
	}
	first, fail := resolve(seedExcept(candidate))
	if fail != nil {
		return nil, fail
	}
	owned := make([]bool, len(spawnCopyRelaunchFields))
	for i, f := range spawnCopyRelaunchFields {
		owned[i] = candidate[i] && f.copyDecides(profiles, &first)
	}
	resolved, fail := resolve(seedExcept(owned))
	if fail != nil {
		return nil, fail
	}
	out := &db.AgentRelaunchProfile{Version: db.RelaunchProfileVersion}
	for i, f := range spawnCopyRelaunchFields {
		if owned[i] {
			f.take(out, &resolved)
		}
	}
	return out, nil
}

// decodedSpawnCopy is a spawn copy's profiles by tier: the caller's own
// (inline or named) and the two defaults, and all of them in copy order.
type decodedSpawnCopy struct {
	caller   *db.SpawnProfile
	slots    pinnedDefaultProfiles
	profiles []*db.SpawnProfile
}

func decodeSpawnCopyTiers(spawnCopy db.AgentSpawnCopy) (decodedSpawnCopy, *spawnFailure) {
	var out decodedSpawnCopy
	for _, tier := range spawnCopy.Tiers {
		prof, fail := spawnCopyProfile(tier)
		if fail != nil {
			return out, fail
		}
		var slot **db.SpawnProfile
		switch tier.Tier {
		case db.SpawnCopyTierInline, db.SpawnCopyTierNamed:
			slot = &out.caller
		case db.SpawnCopyTierGroupDefault:
			slot = &out.slots.group
		case db.SpawnCopyTierGlobalDefault:
			slot = &out.slots.global
		default:
			return out, &spawnFailure{http.StatusBadRequest, "invalid_spawn_copy",
				fmt.Sprintf("spawn copy tier %q is not known", tier.Tier)}
		}
		if *slot != nil {
			return out, &spawnFailure{http.StatusBadRequest, "invalid_spawn_copy",
				fmt.Sprintf("spawn copy has two profiles for the %s tier (%q and %q); a relaunch resolves one per tier",
					tier.Tier, (*slot).Name, prof.Name)}
		}
		*slot = prof
		out.profiles = append(out.profiles, prof)
	}
	return out, nil
}

// launchTiers are the copy's profiles as a first spawn's tier stack, highest
// first: the caller's profile, then the group and global defaults.
func (d decodedSpawnCopy) launchTiers() []launchProfileTier {
	return []launchProfileTier{
		{profile: d.caller, source: profileSource(d.caller, agent.ProvCLIProfileSource)},
		{profile: d.slots.group, source: profileSource(d.slots.group, agent.ProvGroupProfileSource), defaultTier: true},
		{profile: d.slots.global, source: profileSource(d.slots.global, agent.ProvGlobalProfileSource), defaultTier: true},
	}
}

// spawnCopyProfile decodes one copied profile from its export format.
func spawnCopyProfile(tier db.SpawnCopyTier) (*db.SpawnProfile, *spawnFailure) {
	var body spawnProfileJSON
	if err := json.Unmarshal(tier.Profile, &body); err != nil {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_spawn_copy",
			fmt.Sprintf("spawn copy profile %q cannot be read: %v", tier.ProfileName, err)}
	}
	build := buildProfileFromJSON
	if tier.Tier == db.SpawnCopyTierInline {
		build = buildInlineProfileFromJSON
	}
	prof, fail := build(body)
	if fail != nil {
		return nil, fail
	}
	prof.ID = tier.ProfileID
	if tier.ProfileName != "" {
		prof.Name = tier.ProfileName
	}
	return prof, nil
}

// spawnCopyRelaunchField is one spawn-copy field a relaunch profile carries:
// its launchspec key, the db.SpawnProfile field a copied profile sets it in,
// the AgentRelaunchProfile field (a pointer) the resolved value lands in with
// the attribution fields that travel with it, and how a stored value is fed
// back into the resolver as an explicit one.
type spawnCopyRelaunchField struct {
	key      string
	profile  string
	relaunch string
	with     []string
	seed     func(p *spawnParams, v any)
	// seedOnly fields are fed back from the stored profile but never taken
	// from a copy: applying them needs a gate this path cannot run yet.
	seedOnly bool
}

// copySets reports whether any copied profile sets the field.
func (f spawnCopyRelaunchField) copySets(profiles []*db.SpawnProfile) bool {
	for _, prof := range profiles {
		if pv := reflect.ValueOf(*prof).FieldByName(f.profile); pv.IsValid() && !pv.IsZero() {
			return true
		}
	}
	return false
}

// copyDecides reports whether the copy owns the field: the highest copied
// profile that sets it says what the resolver resolved.
func (f spawnCopyRelaunchField) copyDecides(profiles []*db.SpawnProfile, resolved *db.AgentRelaunchProfile) bool {
	for _, prof := range profiles {
		pv := reflect.ValueOf(*prof).FieldByName(f.profile)
		if !pv.IsValid() || pv.IsZero() {
			continue
		}
		if pv.Kind() == reflect.Pointer {
			pv = pv.Elem()
		}
		rv, ok := f.storedValue(resolved)
		return ok && reflect.DeepEqual(pv.Interface(), rv)
	}
	return false
}

// storedValue is the field's value in a relaunch profile; false when unset.
func (f spawnCopyRelaunchField) storedValue(p *db.AgentRelaunchProfile) (any, bool) {
	v := reflect.ValueOf(p).Elem().FieldByName(f.relaunch)
	if !v.IsValid() || v.IsNil() {
		return nil, false
	}
	return v.Elem().Interface(), true
}

// take copies the field, and the fields that travel with it, from src.
func (f spawnCopyRelaunchField) take(dst, src *db.AgentRelaunchProfile) {
	d, s := reflect.ValueOf(dst).Elem(), reflect.ValueOf(src).Elem()
	for _, name := range append([]string{f.relaunch}, f.with...) {
		d.FieldByName(name).Set(s.FieldByName(name))
	}
}

// spawnCopyRelaunchFields are the spawn-copy fields a relaunch profile
// carries. Four other spawn-copy keys (the profile environment and startup
// context, usage links, include_group_context) live outside the relaunch
// profile and are re-applied by resolveSpawnCopyOutside; roles and the named
// sandbox profile are not re-applied from a copy; the pi drive stays as launched, since
// applyDefaultProfile does not resolve it. The opt-in toggles are seeded only
// when on, the only state a caller can request.
var spawnCopyRelaunchFields = []spawnCopyRelaunchField{
	// The model proxy is not re-applied yet: a first spawn gates it on the
	// spawner's models.proxy grant, which is judged per request and this
	// path does not carry one. It keeps the value from the first spawn.
	{key: "model_proxy", profile: "ModelProxy", relaunch: "ModelProxy", seedOnly: true,
		seed: func(p *spawnParams, v any) { p.ModelProxy = v.(string) }},
	{key: "sandbox_mode", profile: "Sandbox", relaunch: "HarnessBuiltinMode", with: []string{"HarnessBuiltinModeSource"},
		seed: func(p *spawnParams, v any) { p.HarnessBuiltinMode = v.(string) }},
	{key: "sandbox_implementation", profile: "SandboxImplementation", relaunch: "SandboxImplementation",
		seed: seedSandboxImplementation},
	{key: "approval", profile: "Approval", relaunch: "ApprovalPolicy",
		seed: func(p *spawnParams, v any) { p.ApprovalPolicy = v.(string) }},
	{key: "tools", profile: "ToolGovernance", relaunch: "ToolGovernance",
		seed: func(p *spawnParams, v any) { p.ToolGovernance = v.(string) }},
	{key: "ask_timeout", profile: "AskUserQuestionTimeout", relaunch: "AskUserQuestionTimeout",
		seed: func(p *spawnParams, v any) { p.AskUserQuestionTimeout = v.(string) }},
	{key: "context_window", profile: "AutoCompactWindow", relaunch: "AutoCompactWindow",
		seed: func(p *spawnParams, v any) { p.AutoCompactWindow = v.(string) }},
	{key: "context_window", profile: "ContextWindowMax", relaunch: "ConfiguredContextWindowMax",
		seed: func(p *spawnParams, v any) { p.ContextWindowMax = v.(int64) }},
	{key: "context_features", profile: "ContextFeatures", relaunch: "ContextFeatures",
		seed: func(p *spawnParams, v any) { p.ContextFeatures = v.(map[string]string) }},
	{key: "harness_drives", profile: "CopilotAPI", relaunch: "CopilotAPI", with: []string{"CopilotAPISource"},
		seed: func(p *spawnParams, v any) { p.CopilotAPI, p.CopilotAPISet = v.(bool), true }},
	{key: "harness_drives", profile: "CodexAppServer", relaunch: "CodexAppServer", with: []string{"CodexAppServerSource"},
		seed: func(p *spawnParams, v any) { p.CodexAppServer, p.CodexAppServerSet = v.(bool), true }},
	{key: "fast_mode", profile: "FastMode", relaunch: "FastMode",
		seed: func(p *spawnParams, v any) { p.FastMode, p.FastModeSet = v.(bool), true }},
	{key: "auto_review", profile: "AutoReview", relaunch: "ApprovalAutoReview",
		seed: func(p *spawnParams, v any) { p.AutoReview, p.AutoReviewSet = v.(bool), true }},
	{key: "remote_control", profile: "RemoteControl", relaunch: "RemoteControl",
		seed: func(p *spawnParams, v any) { p.RemoteControl = p.RemoteControl || v.(bool) }},
	{key: "auto_memory", profile: "AutoMemory", relaunch: "AutoMemory",
		seed: func(p *spawnParams, v any) { p.AutoMemory = p.AutoMemory || v.(bool) }},
	{key: "peer_messaging", profile: "PeerMessaging", relaunch: "PeerMessaging",
		seed: func(p *spawnParams, v any) { p.PeerMessaging = p.PeerMessaging || v.(bool) }},
	{key: "ssh_workaround", profile: "SSHWorkaround", relaunch: "SSHWorkaround", with: []string{"SSHWorkaroundSource"},
		seed: func(p *spawnParams, v any) { p.SSHWorkaround, p.SSHWorkaroundSet = v.(bool), true }},
}

// seedSandboxImplementation feeds a stored implementation back in. The stored
// shape spells "none chosen" as harness-builtin (relaunchProfileForSpawn), and
// a harness that has no sandbox of its own refuses that value given
// explicitly, so such a value is left unset, as the agent was launched.
func seedSandboxImplementation(p *spawnParams, v any) {
	value := v.(string)
	h, err := harness.Resolve(harnessOrDefault(p.Harness))
	if err != nil {
		return
	}
	if _, err := validateSandboxImplementationForHarness(h, value); err != nil {
		return
	}
	p.SandboxImplementation = value
}

// ---- spawn-copy fields outside the relaunch profile (launch slice 6f) ----

// Re-applying the spawn-copy fields that live outside the relaunch profile
// (launch slice 6f, tcl-g2q5wz). A first spawn resolves each from the profile
// tiers and the spawn request and stores it where it is read later:
//
//   - include_group_context: the startup snapshot's include_group_ctx, which
//     re-injection after compaction or /clear reads;
//   - profile_startup_context: the startup snapshot's profile_context, the
//     profile's text and the role briefs that follow it;
//   - roles: the role label (the agent's role in its spawn group), while it is
//     still the one the old copy gave;
//   - profile_env: the launch snapshot's environment overrides, the profile
//     tiers then the explicit entries, which every resume merges over the
//     group's current environment;
//   - usage_profile_links: the agent's usage-profile links that came from a
//     spawn profile (the ones the spawn named itself are its own choice).
//
// resolveSpawnCopyOutside resolves them from the new copy the way the first
// spawn did, keeping every value the spawn request set explicitly, and says
// what to write; db.ApplyAgentSpawnCopy writes it with the copy. A value equal
// to the stored one is not written, so re-applying the same copy changes
// nothing. A role's grants are re-applied with it (profile_reapply_grants.go,
// tcl-fg2kqk).

// spawnCopyKept is a field the re-apply could not write outside the relaunch
// profile, with why. Its old value stays in the copy written, so the preview
// keeps listing the difference instead of hiding it.
type spawnCopyKept struct {
	key, why string
}

func resolveSpawnCopyOutside(agentID string, g *db.AgentGroup, harnessName string, oldCopy *db.AgentSpawnCopy,
	newCopy db.AgentSpawnCopy, explicit map[string]db.ExplicitLaunchValue, startup *db.AgentStartupSnapshot,
	reapplierConv string) (db.SpawnCopyOutside, []spawnCopyKept, *spawnFailure) {
	var out db.SpawnCopyOutside
	var kept []spawnCopyKept
	next, fail := decodeSpawnCopyTiers(newCopy)
	if fail != nil {
		return out, nil, fail
	}
	tiers := next.launchTiers()
	var prevTiers []launchProfileTier
	if oldCopy != nil {
		prev, fail := decodeSpawnCopyTiers(*oldCopy)
		if fail != nil {
			return out, nil, fail
		}
		prevTiers = prev.launchTiers()
	}
	h, err := harness.Resolve(harnessOrDefault(harnessName))
	if err != nil {
		return out, nil, &spawnFailure{http.StatusUnprocessableEntity, "invalid_harness", err.Error()}
	}
	isExplicit := func(key string) bool { return explicit[key].Provenance != "" }

	// include_group_context: only a change the copy decides is written. What
	// an unset value means depends on the spawn path (a direct spawn includes,
	// a one-shot or scribe does not), so a copy that stops setting it keeps
	// the stored value; so does any case the copy did not change.
	if startup != nil && !isExplicit("include_group_context") {
		was, fail := resolveIncludeGroupContext(h, prevTiers)
		if fail != nil {
			return out, nil, fail
		}
		now, fail := resolveIncludeGroupContext(h, tiers)
		if fail != nil {
			return out, nil, fail
		}
		switch {
		case was == now:
		case now.set:
			if now.value != startup.IncludeGroupContext {
				out.IncludeGroupContext = &now.value
			}
		default:
			kept = append(kept, spawnCopyKept{"include_group_context",
				"the profile no longer sets it, and what unset means depends on how the agent was spawned"})
		}
	}

	if startup != nil {
		was, _ := resolveProfileStartupContext(harnessName, prevTiers)
		now, _ := resolveProfileStartupContext(harnessName, tiers)
		stored := strings.TrimSpace(startup.ProfileContext)
		rolesFromCopy := !isExplicit("roles")
		var wasRoles, nowRoles reappliedRoleText
		if rolesFromCopy {
			if wasRoles, fail = resolveReappliedRoleText(prevTiers); fail != nil {
				return out, nil, fail
			}
			if nowRoles, fail = resolveReappliedRoleText(tiers); fail != nil {
				return out, nil, fail
			}
		}
		briefsFollow := false
		switch {
		case strings.HasPrefix(stored, was):
			// What follows the profile's text is its role briefs; they are
			// rewritten from the roles the copy names now, with the roles'
			// current text, unless the spawn request (or a template) named its
			// own roles. The tail must look like what the old copy gave: its
			// roles' briefs, or as many role blocks as it has briefed roles
			// when a brief was edited since.
			tail := strings.TrimSpace(stored[len(was):])
			if rolesFromCopy && rolesTailFromCopy(tail, wasRoles) {
				tail, briefsFollow = nowRoles.briefs, true
			}
			if combined := joinContextParts(now, tail); combined != stored {
				out.ProfileContext = &combined
			}
		case was != now:
			kept = append(kept, spawnCopyKept{"profile_startup_context",
				"the agent's stored startup context does not start with the profile's old text"})
		}
		// The role label is the agent's role in its spawn group, rewritten only
		// while it is still the one the old copy gave, and only with the
		// briefs, so the two never part.
		if briefsFollow && startup.SpawnGroupID > 0 {
			if wasRoles.label != nowRoles.label {
				out.RoleLabel = &db.SpawnCopyRoleLabel{GroupID: startup.SpawnGroupID, From: wasRoles.label, To: nowRoles.label}
			}
		}
	} else {
		kept = append(kept, spawnCopyKept{"profile_startup_context", "the agent has no startup record"})
	}

	// The explicit entries stay on top of whatever the copy's tiers now give,
	// as they did at the first spawn.
	env, why, fail := resolveSpawnCopyEnvironment(agentID, g, tiers, prevTiers, explicit["profile_env"])
	if fail != nil {
		return out, nil, fail
	}
	switch {
	case why != "":
		kept = append(kept, spawnCopyKept{"profile_env", why})
	case env != nil && reapplierConv != "":
		// An environment reaches the agent's next launch: model endpoints,
		// proxies, tool hooks. Morphing an agent cannot change it, so neither
		// can an agent re-applying a profile to another one.
		return out, nil, &spawnFailure{http.StatusForbidden, "environment_operator_only",
			"this re-apply would change the agent's environment, which only the operator can re-apply; ask them to apply it"}
	case env != nil:
		out.EffectiveSandbox, out.EffectiveSandboxRaw = env.snapshot, env.raw
	}

	if !isExplicit("usage_profile_links") {
		ids, source, changed, err := spawnCopyUsageLinks(agentID, next)
		if err != nil {
			return out, nil, &spawnFailure{http.StatusInternalServerError, "io", "read usage profile links: " + err.Error()}
		}
		if changed {
			out.UsageLinkedSet, out.UsageLinkedIDs, out.UsageLinkedSource = true, ids, source
		}
	}
	return out, kept, nil
}

// reappliedRoleText is the role identity a copy's tiers give, as a spawn
// resolves it (resolveLaunchIdentity): the role label, and the role briefs
// that follow the profile's text in the startup context.
type reappliedRoleText struct {
	label, briefs string
}

func resolveReappliedRoleText(tiers []launchProfileTier) (reappliedRoleText, *spawnFailure) {
	var out reappliedRoleText
	refs, _ := resolveRoleRefsLaunchField(agent.SpawnRequest{}, tiers)
	var first string
	for _, ref := range refs {
		role, err := db.GetRole(ref)
		if err != nil {
			return out, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		if role == nil {
			// Deleted since: its brief goes, as its grants do.
			continue
		}
		if first == "" {
			first = role.Name
		}
		out.briefs = appendRoleBlock(out.briefs, role.Brief)
	}
	label, source, _ := resolveIdentityLaunchField(roleField, "", false, tiers,
		func(p *db.SpawnProfile) string { return p.Role }, nil)
	if label == "" && source == "" {
		label = first
	}
	out.label = label
	return out, nil
}

// rolesTailFromCopy reports whether tail, the stored startup context after
// the profile's text, is the role briefs the old copy's roles gave
// (appendRoleBlock): exactly their briefs now, or, when a brief has been
// edited since, as many role blocks. An agent whose roles did not come from
// the copy (a template's, kept elsewhere) has a tail that matches neither.
func rolesTailFromCopy(tail string, was reappliedRoleText) bool {
	if tail == was.briefs {
		return true
	}
	return tail != "" && strings.HasPrefix(tail, roleBlockHeading) &&
		strings.Count(tail, roleBlockHeading) == strings.Count(was.briefs, roleBlockHeading)
}

type includeGroupContextResolution struct {
	value, set bool
}

func resolveIncludeGroupContext(h *harness.Harness, tiers []launchProfileTier) (includeGroupContextResolution, *spawnFailure) {
	var res launchBoolResolution
	if _, fail := resolveLaunchBoolFields(h, tiers, &res, launchBoolIncludeGroupContext); fail != nil {
		return includeGroupContextResolution{}, fail
	}
	return includeGroupContextResolution{value: res.Value[launchBoolIncludeGroupContext], set: res.Set[launchBoolIncludeGroupContext]}, nil
}

// joinContextParts joins non-empty blocks the way appendRoleBlock does.
func joinContextParts(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n\n")
}

type spawnCopyEnvironment struct {
	snapshot *sandboxpolicy.Snapshot
	raw      string
}

// resolveSpawnCopyEnvironment is the agent's launch snapshot with its
// environment overrides recomputed from the copy's tiers and the spawn's
// explicit entries, or nil when they would not change. why, when set, says
// why a change the copy makes cannot be written (the agent has no resolved
// launch environment to replace).
func resolveSpawnCopyEnvironment(agentID string, g *db.AgentGroup, tiers, prevTiers []launchProfileTier,
	explicit db.ExplicitLaunchValue) (env *spawnCopyEnvironment, why string, fail *spawnFailure) {
	var explicitEnv []sandboxpolicy.EnvironmentEntry
	if raw, ok := explicit.Fields["Environment"]; ok {
		if err := json.Unmarshal(raw, &explicitEnv); err != nil {
			return nil, "", &spawnFailure{http.StatusConflict, "invalid_launch_layers", "the spawn's explicit environment cannot be read: " + err.Error()}
		}
	}
	environment, overrides, err := resolveCommonLaunchEnvironmentParts(g, tiers, explicitEnv)
	if err != nil {
		return nil, "", &spawnFailure{http.StatusBadRequest, "invalid_environment", err.Error()}
	}
	current, raw, err := db.AgentEffectiveSandboxConfigRaw(agentID)
	if err != nil {
		return nil, "", &spawnFailure{http.StatusInternalServerError, "io", "load the agent's launch snapshot: " + err.Error()}
	}
	if current == nil || !current.RefreshGroupEnvironment {
		// No resolved launch environment to replace. Only a change the copy
		// makes is worth saying so for.
		_, prevOverrides, err := resolveCommonLaunchEnvironmentParts(g, prevTiers, explicitEnv)
		if err == nil && sameEnvironment(overrides, prevOverrides) {
			return nil, "", nil
		}
		return nil, "the agent's launch record has no resolved environment to update", nil
	}
	if sameEnvironment(overrides, current.LaunchEnvironmentOverrides) {
		return nil, "", nil
	}
	next := *current
	next.LaunchEnvironment, next.LaunchEnvironmentOverrides = environment, overrides
	return &spawnCopyEnvironment{snapshot: &next, raw: raw}, "", nil
}

func sameEnvironment(a, b []sandboxpolicy.EnvironmentEntry) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// spawnCopyUsageLinks are the usage profiles the copy links the agent to, as a
// first spawn picks them (applySpawnUsageProfiles): those of the first copied
// profile with any, named then group default then global default, read live
// from the usage rules store. changed reports whether they differ from the
// agent's current linked set.
func spawnCopyUsageLinks(agentID string, d decodedSpawnCopy) (ids []int64, source string, changed bool, err error) {
	assignments, err := db.ListUsageProfileAssignments()
	if err != nil {
		return nil, "", false, err
	}
	for _, prof := range []*db.SpawnProfile{d.caller, d.slots.group, d.slots.global} {
		if prof == nil || prof.ID == 0 {
			continue
		}
		scopeID := strconv.FormatInt(prof.ID, 10)
		for _, a := range assignments {
			if a.Scope == db.UsageScopeSpawnProfile && a.ScopeID == scopeID {
				ids = append(ids, a.ProfileID)
			}
		}
		if len(ids) > 0 {
			source = usageLinkedSourcePrefix + prof.Name
			break
		}
	}
	var have []int64
	haveSource := ""
	for _, a := range assignments {
		if a.Scope == db.UsageScopeAgent && a.ScopeID == agentID && strings.HasPrefix(a.Source, usageLinkedSourcePrefix) {
			have = append(have, a.ProfileID)
			haveSource = a.Source
		}
	}
	slices.Sort(ids)
	slices.Sort(have)
	changed = !slices.Equal(ids, have) || (len(ids) > 0 && source != haveSource)
	return ids, source, changed, nil
}

// keepCopyFields is newCopy with the given fields' values taken back from the
// old copy, tier by tier (matched by tier and profile ID), so the stored copy
// says what the agent really runs with.
func keepCopyFields(newCopy db.AgentSpawnCopy, oldCopy *db.AgentSpawnCopy, kept []spawnCopyKept) db.AgentSpawnCopy {
	if oldCopy == nil || len(kept) == 0 {
		return newCopy
	}
	keys := map[string]bool{}
	for _, k := range kept {
		keys[k.key] = true
	}
	keep := func(jsonKey string) bool {
		f, ok := launchFieldForProfileField(spawnProfileJSONGoNames[jsonKey])
		return ok && keys[f.Key]
	}
	out := db.AgentSpawnCopy{CopiedAt: newCopy.CopiedAt, Tiers: append([]db.SpawnCopyTier(nil), newCopy.Tiers...)}
	for i, tier := range out.Tiers {
		var old *db.SpawnCopyTier
		for j := range oldCopy.Tiers {
			if oldCopy.Tiers[j].Tier == tier.Tier && oldCopy.Tiers[j].ProfileID == tier.ProfileID {
				old = &oldCopy.Tiers[j]
				break
			}
		}
		merged, copied := profileJSONMap(tier.Profile), map[string]json.RawMessage{}
		if old != nil {
			copied = profileJSONMap(old.Profile)
		}
		if merged == nil {
			continue
		}
		for key := range merged {
			if keep(key) {
				delete(merged, key)
			}
		}
		for key, raw := range copied {
			if keep(key) {
				merged[key] = raw
			}
		}
		if raw, err := json.Marshal(merged); err == nil {
			out.Tiers[i].Profile = raw
		}
	}
	return out
}
