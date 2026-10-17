package agentd

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Remote spawn launch settings: what a peer may choose per spawn request
// instead of leaving it to this node's grant policy, profiles and group
// defaults. Each is its own peer grant, group-scoped like the
// groups.members.spawn grant it extends, and closed by default:
//
//   - groups.members.spawn.cwd, .harness, .model, .effort: the launch
//     directory, harness, model and effort;
//   - groups.members.spawn.profile: any enabled spawn profile that is not
//     operator-only (the grant's --allow-profile list names single ones);
//   - groups.members.spawn.roles: the role set, from this node's roles (roles
//     carry permissions here);
//   - groups.members.spawn.sandbox: an explicit sandbox profile tier;
//   - groups.members.spawn.permissions: the approval, tool and sandbox
//     modes, the trust-folder answer and the per-slug permissions;
//   - groups.members.spawn.environment: environment overrides;
//   - groups.members.spawn.owner: group ownership at birth;
//   - groups.members.spawn.features: the harness features, context and
//     window settings, the description and task link;
//   - groups.members.spawn.prompts: the saved prompt set, from this node's
//     saved prompts (replacing the profile and group defaults);
//   - groups.members.spawn.worktree: launching in a git worktree, which the
//     peer creates first through the worktree routes (peer_spawn_worktrees.go);
//   - groups.members.spawn.all: every field above.
//
// The last six ride as launch settings (FederationSpawnOverrides.Launch):
// keys of the ordinary spawn request (fedSpawnLaunchKeys), laid over the
// launch this node builds, so the group spawn path validates them exactly as
// it does a local spawn's.
//
// They apply only together with an automatic spawn (a groups.members.spawn
// grant, or unrestricted trust, which opens all of them): a request waiting
// for the operator may set none. The ordinary group spawn path still applies
// every launch guardrail to the result.
const (
	fedOverrideCwd     = "cwd"
	fedOverrideHarness = "harness"
	fedOverrideModel   = "model"
	fedOverrideEffort  = "effort"
	fedOverrideProfile = "profile"
	fedOverrideRoles   = "roles"
	fedOverrideSandbox = "sandbox"
	// Launch settings (fedSpawnLaunchKeys).
	fedOverridePermissions = "permissions"
	fedOverrideEnvironment = "environment"
	fedOverrideOwner       = "owner"
	fedOverrideFeatures    = "features"
	fedOverridePrompts     = "prompts"
	fedOverrideWorktree    = "worktree"

	PermGroupsMembersSpawnCwd         = "groups.members.spawn.cwd"
	PermGroupsMembersSpawnHarness     = "groups.members.spawn.harness"
	PermGroupsMembersSpawnModel       = "groups.members.spawn.model"
	PermGroupsMembersSpawnEffort      = "groups.members.spawn.effort"
	PermGroupsMembersSpawnProfile     = "groups.members.spawn.profile"
	PermGroupsMembersSpawnRoles       = "groups.members.spawn.roles"
	PermGroupsMembersSpawnSandbox     = "groups.members.spawn.sandbox"
	PermGroupsMembersSpawnPermissions = "groups.members.spawn.permissions"
	PermGroupsMembersSpawnEnvironment = "groups.members.spawn.environment"
	PermGroupsMembersSpawnOwner       = "groups.members.spawn.owner"
	PermGroupsMembersSpawnFeatures    = "groups.members.spawn.features"
	PermGroupsMembersSpawnPrompts     = "groups.members.spawn.prompts"
	PermGroupsMembersSpawnWorktree    = "groups.members.spawn.worktree"
	PermGroupsMembersSpawnAll         = "groups.members.spawn.all"
)

var fedSpawnOverrideFields = []string{fedOverrideCwd, fedOverrideHarness, fedOverrideModel, fedOverrideEffort, fedOverrideProfile, fedOverrideRoles, fedOverrideSandbox,
	fedOverridePermissions, fedOverrideEnvironment, fedOverrideOwner, fedOverrideFeatures, fedOverridePrompts, fedOverrideWorktree}

// fedSpawnFieldSlugs maps each field to the grant that opens it.
var fedSpawnFieldSlugs = map[string]string{
	fedOverrideCwd:     PermGroupsMembersSpawnCwd,
	fedOverrideHarness: PermGroupsMembersSpawnHarness,
	fedOverrideModel:   PermGroupsMembersSpawnModel,
	fedOverrideEffort:  PermGroupsMembersSpawnEffort,
	fedOverrideProfile: PermGroupsMembersSpawnProfile,
	fedOverrideRoles:   PermGroupsMembersSpawnRoles,
	fedOverrideSandbox: PermGroupsMembersSpawnSandbox,

	fedOverridePermissions: PermGroupsMembersSpawnPermissions,
	fedOverrideEnvironment: PermGroupsMembersSpawnEnvironment,
	fedOverrideOwner:       PermGroupsMembersSpawnOwner,
	fedOverrideFeatures:    PermGroupsMembersSpawnFeatures,
	fedOverridePrompts:     PermGroupsMembersSpawnPrompts,
	fedOverrideWorktree:    PermGroupsMembersSpawnWorktree,
}

// fedSpawnLaunchKeys are the spawn request keys (agent.SpawnRequest) a remote
// spawn may carry as launch settings, each with the field that opens it. Not
// carried: what only means something on the requesting node (focus,
// attachments, queueing) and the break-glass acknowledgement, which only this
// node's own operator gives.
var fedSpawnLaunchKeys = map[string]string{
	"approval":                  fedOverridePermissions,
	"tools":                     fedOverridePermissions,
	"auto_review":               fedOverridePermissions,
	"ask_user_question_timeout": fedOverridePermissions,
	"trust_dir":                 fedOverridePermissions,
	"permission_overrides":      fedOverridePermissions,
	"sandbox":                   fedOverridePermissions,
	"sandbox_implementation":    fedOverridePermissions,
	"allow_unenforced_sandbox":  fedOverridePermissions,
	"omit_sandbox_profiles":     fedOverridePermissions,
	"environment":               fedOverrideEnvironment,
	"is_owner":                  fedOverrideOwner,
	"include_group_context":     fedOverrideFeatures,
	"context_features":          fedOverrideFeatures,
	"remote_control":            fedOverrideFeatures,
	"auto_memory":               fedOverrideFeatures,
	"peer_messaging":            fedOverrideFeatures,
	"copilot_api":               fedOverrideFeatures,
	"codex_app_server":          fedOverrideFeatures,
	"fast_mode":                 fedOverrideFeatures,
	"ssh_workaround":            fedOverrideFeatures,
	"auto_compact_window":       fedOverrideFeatures,
	"context_window_max":        fedOverrideFeatures,
	"descr":                     fedOverrideFeatures,
	"task_ref_url":              fedOverrideFeatures,
	"prompt_refs":               fedOverridePrompts,
	"worktree_path":             fedOverrideWorktree,
	"worktree_branch":           fedOverrideWorktree,
}

// fedSpawnMaxLaunch bounds a request's launch settings as stored JSON.
const fedSpawnMaxLaunch = 32 << 10

var (
	// No leading '-': a model reaches harness command lines, where it must
	// never read as a flag.
	fedSpawnModelRE  = regexp.MustCompile(`^[A-Za-z0-9._:/@\[\]+][A-Za-z0-9._:/@\[\]+-]{0,127}$`)
	fedSpawnEffortRE = regexp.MustCompile(`^[a-z]{1,32}$`)
)

const fedSpawnMaxRoles = 16

// fedSpawnFieldGranted reports whether peer holds the grant for one field
// in group, or the all-fields grant, at least as specific as the spawn grant
// in force there: a group-scoped spawn grant narrows what a broader field
// grant opens, as its own policy narrows a broader spawn grant's.
// Unrestricted trust is the caller's.
func fedSpawnFieldGranted(peer string, groupID int64, field string) bool {
	slug, ok := fedSpawnFieldSlugs[field]
	if !ok {
		return false
	}
	spawn := fedPeerExplicitGroupGrant(peer, groupID, PermGroupsMembersSpawn)
	if spawn == nil {
		return false
	}
	for _, s := range []string{slug, PermGroupsMembersSpawnAll} {
		if g := fedPeerExplicitGroupGrant(peer, groupID, s); g != nil && fedGrantRank(*g) >= fedGrantRank(*spawn) {
			return true
		}
	}
	return false
}

// fedSpawnOverridesAllowed reports which launch settings peer may choose for
// a spawn into group. Only an automatic spawn (a grant, or unrestricted
// trust) opens any: an operator-approved request carries none.
func fedSpawnOverridesAllowed(peer string, groupID int64) map[string]bool {
	out := map[string]bool{}
	if fedPeerGroupGrant(peer, groupID, PermGroupsMembersSpawn) == nil {
		return out
	}
	unrestricted := db.FederationPeerUnrestricted(peer)
	for _, field := range fedSpawnOverrideFields {
		if unrestricted || fedSpawnFieldGranted(peer, groupID, field) {
			out[field] = true
		}
	}
	return out
}

// validateFedSpawnOverrides checks the shape of requested overrides; whether
// the peer may set them is checkFedSpawnOverrides.
func validateFedSpawnOverrides(o db.FederationSpawnOverrides) error {
	if o.Cwd != "" {
		if len(o.Cwd) > 4096 || proto.StripControls(o.Cwd) != o.Cwd || !filepath.IsAbs(o.Cwd) || filepath.Clean(o.Cwd) != o.Cwd {
			return fmt.Errorf("requested directory must be a clean absolute path")
		}
	}
	if o.Harness != "" {
		if _, ok := harness.Get(o.Harness); !ok || !fedSpawnHarnessOffered(o.Harness) {
			return fmt.Errorf("requested harness is unknown on this node or cannot take a spawn request")
		}
	}
	if o.Model != "" && !fedSpawnModelRE.MatchString(o.Model) {
		return fmt.Errorf("invalid requested model")
	}
	if o.Effort != "" && !fedSpawnEffortRE.MatchString(o.Effort) {
		return fmt.Errorf("invalid requested effort")
	}
	if len(o.Roles) > fedSpawnMaxRoles {
		return fmt.Errorf("at most %d requested roles", fedSpawnMaxRoles)
	}
	for _, name := range o.Roles {
		if len(name) > 128 || validateGroupName(name) != nil {
			return fmt.Errorf("invalid requested role")
		}
		if role, err := db.GetRole(name); err != nil || role == nil {
			return fmt.Errorf("requested role %q is not a role on this node", name)
		}
	}
	if o.SandboxProfile != "" {
		if len(o.SandboxProfile) > 128 || validateGroupName(o.SandboxProfile) != nil {
			return fmt.Errorf("invalid requested sandbox profile")
		}
		if profile, err := db.GetSandboxProfile(o.SandboxProfile); err != nil || profile == nil {
			return fmt.Errorf("requested sandbox profile %q is not a sandbox profile on this node", o.SandboxProfile)
		}
	}
	return validateFedSpawnLaunch(o.Launch)
}

// validateFedSpawnLaunch checks that launch settings name only carried keys
// and decode as the spawn request's fields. Their values are the group spawn
// path's to judge, as a local spawn's are.
func validateFedSpawnLaunch(launch map[string]json.RawMessage) error {
	if len(launch) == 0 {
		return nil
	}
	for key := range launch {
		if _, ok := fedSpawnLaunchKeys[key]; !ok {
			return fmt.Errorf("a remote spawn cannot set %q", key)
		}
	}
	raw, err := json.Marshal(launch)
	if err != nil || len(raw) > fedSpawnMaxLaunch {
		return fmt.Errorf("requested launch settings are over %d bytes", fedSpawnMaxLaunch)
	}
	if err := json.Unmarshal(raw, new(agent.SpawnRequest)); err != nil {
		return fmt.Errorf("invalid requested launch settings: %v", err)
	}
	return nil
}

// fedSpawnLaunchFields lists the fields a request's launch settings need,
// sorted.
func fedSpawnLaunchFields(launch map[string]json.RawMessage) []string {
	var out []string
	for key := range launch {
		if field := fedSpawnLaunchKeys[key]; field != "" && !slices.Contains(out, field) {
			out = append(out, field)
		}
	}
	slices.Sort(out)
	return out
}

// fedSpawnLaunchBody is the group spawn body of a launch: this node's own
// request with the launch settings laid over it, as raw JSON so a setting
// sent empty (no prompts, no permission overrides) stays an explicit empty
// rather than an omitted field the profile tiers would fill. A description
// goes ahead of this node's own "spawned for" line rather than replacing it.
func fedSpawnLaunchBody(spawn agent.SpawnRequest, launch map[string]json.RawMessage) ([]byte, error) {
	base, err := json.Marshal(spawn)
	if err != nil || len(launch) == 0 {
		return base, err
	}
	merged := map[string]json.RawMessage{}
	if err := json.Unmarshal(base, &merged); err != nil {
		return nil, err
	}
	for key, value := range launch {
		if _, ok := fedSpawnLaunchKeys[key]; ok {
			merged[key] = value
		}
	}
	var descr string
	if raw, ok := launch["descr"]; ok {
		if err := json.Unmarshal(raw, &descr); err != nil {
			return nil, err
		}
	}
	if descr = strings.TrimSpace(descr); descr != "" {
		merged["descr"], _ = json.Marshal(descr + " — " + spawn.Descr)
	} else {
		merged["descr"], _ = json.Marshal(spawn.Descr)
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(out, new(agent.SpawnRequest)); err != nil {
		return nil, err
	}
	return out, nil
}

// fedSpawnHarnessOffered reports whether a remote spawn may choose a harness:
// a spawn request always starts with a brief, which the shell harness refuses.
func fedSpawnHarnessOffered(name string) bool {
	return name != harness.ShellName
}

// fedSpawnHarnesses lists the harnesses a peer may choose here.
func fedSpawnHarnesses() []string {
	var out []string
	for _, name := range harness.Names() {
		if fedSpawnHarnessOffered(name) {
			out = append(out, name)
		}
	}
	return out
}

// checkFedHarnessProfile refuses a requested harness that conflicts with the
// harness an explicitly chosen profile (the requested one, or the grant's
// pinned --profile) runs: the launch would silently skip that profile's
// harness-specific settings. Ambient group/global default profiles are not
// consulted; the spawn path already lets an explicit harness skip them.
func checkFedHarnessProfile(profileName, requested string) error {
	if requested == "" || profileName == "" {
		return nil
	}
	profile, err := db.ResolveSpawnProfile(profileName)
	if err != nil || profile == nil || profile.Harness == "" {
		return nil
	}
	if pinned := harnessOrDefault(profile.Harness); pinned != requested {
		return fmt.Errorf("profile %q runs harness %q, not the requested %q", profile.Name, pinned, requested)
	}
	return nil
}

// fedSpawnOverridesStillAllowed is what an operator approval launches with:
// a requested setting the operator did not set explicitly applies only while
// the current grant or trust still lets the peer choose it (a revoked or
// narrowed grant stops it). It returns the requested settings still to apply
// (blank where the operator passed their own) and the dropped field names.
func fedSpawnOverridesStillAllowed(peer string, groupID int64, o db.FederationSpawnOverrides, explicit fedSpawnApproveReq) (db.FederationSpawnOverrides, []string) {
	return fedSpawnOverridesApplied(o, fedSpawnOverridesAllowed(peer, groupID), explicit)
}

// fedSpawnOverridesApplied is fedSpawnOverridesStillAllowed for a given set
// of fields the peer may choose. The dashboard's approve dialog mirrors it
// (fleet-admin-spawns.js) to preview what an approval drops.
func fedSpawnOverridesApplied(o db.FederationSpawnOverrides, allowed map[string]bool, explicit fedSpawnApproveReq) (db.FederationSpawnOverrides, []string) {
	var dropped []string
	keep := func(field, value, operator string) string {
		if operator != "" {
			return "" // the operator's own value applies instead
		}
		if value == "" {
			return ""
		}
		if !allowed[field] {
			dropped = append(dropped, field)
			return ""
		}
		return value
	}
	out := db.FederationSpawnOverrides{
		Cwd:            keep(fedOverrideCwd, o.Cwd, explicit.Cwd),
		Harness:        keep(fedOverrideHarness, o.Harness, explicit.Harness),
		Model:          keep(fedOverrideModel, o.Model, explicit.Model),
		Effort:         keep(fedOverrideEffort, o.Effort, explicit.Effort),
		SandboxProfile: keep(fedOverrideSandbox, o.SandboxProfile, ""),
	}
	if len(o.Roles) > 0 {
		if allowed[fedOverrideRoles] {
			out.Roles = o.Roles
		} else {
			dropped = append(dropped, fedOverrideRoles)
		}
	}
	for _, field := range fedSpawnLaunchFields(o.Launch) {
		if !allowed[field] {
			dropped = append(dropped, field)
		}
	}
	for key, value := range o.Launch {
		if allowed[fedSpawnLaunchKeys[key]] {
			if out.Launch == nil {
				out.Launch = map[string]json.RawMessage{}
			}
			out.Launch[key] = value
		}
	}
	// The peer chose its model and effort for the harness it requested: when
	// the launch runs another one (the operator named a different harness,
	// or the requested harness is dropped), they go too unless the operator
	// set them.
	otherHarness := explicit.Harness != "" && explicit.Harness != o.Harness ||
		explicit.Harness == "" && o.Harness != "" && out.Harness == ""
	if otherHarness {
		if out.Model != "" {
			out.Model = ""
			dropped = append(dropped, fedOverrideModel)
		}
		if out.Effort != "" {
			out.Effort = ""
			dropped = append(dropped, fedOverrideEffort)
		}
	}
	return out, dropped
}

// fedSpawnOverridesAllowedList is fedSpawnOverridesAllowed as a sorted list,
// empty (not nil) when the peer may choose nothing.
func fedSpawnOverridesAllowedList(peer string, groupID int64) []string {
	out := []string{}
	for field, ok := range fedSpawnOverridesAllowed(peer, groupID) {
		if ok {
			out = append(out, field)
		}
	}
	slices.Sort(out)
	return out
}

// fedSpawnOverridesText lists requested launch settings for an operator
// notice, one per line, or "" when there are none.
func fedSpawnOverridesText(o db.FederationSpawnOverrides) string {
	out := ""
	var launch []string
	for key := range o.Launch {
		launch = append(launch, key)
	}
	slices.Sort(launch)
	for _, kv := range [][2]string{{"directory", o.Cwd}, {"harness", o.Harness}, {"model", o.Model}, {"effort", o.Effort}, {"roles", strings.Join(o.Roles, ", ")}, {"sandbox profile", o.SandboxProfile}, {"launch settings", strings.Join(launch, ", ")}} {
		if kv[1] != "" {
			out += fmt.Sprintf("\n  %s: %q", kv[0], kv[1])
		}
	}
	return out
}

// fedSpawnApprovalSettingsText is the requested-settings paragraph of a
// needs-approval notice, or "" when the request carries none.
func fedSpawnApprovalSettingsText(o db.FederationSpawnOverrides) string {
	if o.Empty() {
		return ""
	}
	return "\nRequested launch settings (approve applies those the peer may still choose, unless you pass your own):" + fedSpawnOverridesText(o)
}

// checkFedSpawnOverrides refuses a request that sets a launch setting the
// peer may not choose for this group, naming the first such field.
func checkFedSpawnOverrides(peer string, groupID int64, o db.FederationSpawnOverrides) error {
	if o.Empty() {
		return nil
	}
	if err := validateFedSpawnOverrides(o); err != nil {
		return err
	}
	allowed := fedSpawnOverridesAllowed(peer, groupID)
	for field, value := range map[string]string{fedOverrideCwd: o.Cwd, fedOverrideHarness: o.Harness, fedOverrideModel: o.Model, fedOverrideEffort: o.Effort, fedOverrideRoles: strings.Join(o.Roles, ","), fedOverrideSandbox: o.SandboxProfile} {
		if value != "" && !allowed[field] {
			return fmt.Errorf("this node does not let you choose the %s for this group", field)
		}
	}
	for _, field := range fedSpawnLaunchFields(o.Launch) {
		if !allowed[field] {
			return fmt.Errorf("this node does not let you choose the %s settings for this group", field)
		}
	}
	return nil
}
