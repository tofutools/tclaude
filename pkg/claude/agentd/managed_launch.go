package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"

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
// sandbox steps fail with kind spawn_failed, io or permission_denied.
func resolveManagedLaunch(g *db.AgentGroup, p *spawnParams, parentConv string, keepUnconfiguredBare bool) *spawnFailure {
	if fail := applyDefaultProfile(g, p); fail != nil {
		return fail
	}
	snapshot := sandboxpolicy.OmittedProfilesSnapshot()
	if !sandboxProfilesDisabled(p.Harness, p.HarnessBuiltinMode, p.SandboxImplementation) {
		resolved, err := db.ResolveEffectiveSandboxSnapshot(g.ID, "")
		if err != nil {
			return &spawnFailure{http.StatusBadRequest, "spawn_failed", err.Error()}
		}
		snapshot = resolved
	}
	if parentConv != "" {
		parent, err := db.AgentEffectiveSandboxConfigForConv(parentConv)
		if err != nil {
			return &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		if parent == nil && sandboxpolicy.HasCapabilities(snapshot) {
			return &spawnFailure{http.StatusForbidden, "permission_denied", "owning principal predates effective sandbox snapshots"}
		}
		if parent != nil {
			validated, valErr := ensureAgentDirectoriesForRelaunch(*parent)
			if valErr != nil {
				return &spawnFailure{http.StatusBadRequest, "spawn_failed", valErr.Error()}
			}
			if containErr := sandboxpolicy.RequireContained(validated, snapshot); containErr != nil {
				return &spawnFailure{http.StatusForbidden, "permission_denied", containErr.Error()}
			}
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
	var profiles []*db.SpawnProfile
	slots := &pinnedDefaultProfiles{}
	var caller *db.SpawnProfile
	for _, tier := range spawnCopy.Tiers {
		prof, fail := spawnCopyProfile(tier)
		if fail != nil {
			return nil, fail
		}
		var slot **db.SpawnProfile
		switch tier.Tier {
		case db.SpawnCopyTierInline, db.SpawnCopyTierNamed:
			slot = &caller
		case db.SpawnCopyTierGroupDefault:
			slot = &slots.group
		case db.SpawnCopyTierGlobalDefault:
			slot = &slots.global
		default:
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_spawn_copy",
				fmt.Sprintf("spawn copy tier %q is not known", tier.Tier)}
		}
		if *slot != nil {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_spawn_copy",
				fmt.Sprintf("spawn copy has two profiles for the %s tier (%q and %q); a relaunch resolves one per tier",
					tier.Tier, (*slot).Name, prof.Name)}
		}
		*slot = prof
		profiles = append(profiles, prof)
	}
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
		if _, ok := explicit[f.key]; !ok {
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
// carries. The other spawn-copy keys (the profile environment and startup
// context, roles, the named sandbox profile, usage links,
// include_group_context) live outside the relaunch profile and are not
// re-applied from a copy yet; the pi drive stays as launched, since
// applyDefaultProfile does not resolve it. The opt-in toggles are seeded only
// when on, the only state a caller can request.
var spawnCopyRelaunchFields = []spawnCopyRelaunchField{
	{key: "model_proxy", profile: "ModelProxy", relaunch: "ModelProxy",
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
