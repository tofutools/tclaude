package agentd

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

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
//   - groups.members.spawn.all: every field above.
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

	PermGroupsMembersSpawnCwd     = "groups.members.spawn.cwd"
	PermGroupsMembersSpawnHarness = "groups.members.spawn.harness"
	PermGroupsMembersSpawnModel   = "groups.members.spawn.model"
	PermGroupsMembersSpawnEffort  = "groups.members.spawn.effort"
	PermGroupsMembersSpawnProfile = "groups.members.spawn.profile"
	PermGroupsMembersSpawnRoles   = "groups.members.spawn.roles"
	PermGroupsMembersSpawnSandbox = "groups.members.spawn.sandbox"
	PermGroupsMembersSpawnAll     = "groups.members.spawn.all"
)

var fedSpawnOverrideFields = []string{fedOverrideCwd, fedOverrideHarness, fedOverrideModel, fedOverrideEffort, fedOverrideProfile, fedOverrideRoles, fedOverrideSandbox}

// fedSpawnFieldSlugs maps each field to the grant that opens it.
var fedSpawnFieldSlugs = map[string]string{
	fedOverrideCwd:     PermGroupsMembersSpawnCwd,
	fedOverrideHarness: PermGroupsMembersSpawnHarness,
	fedOverrideModel:   PermGroupsMembersSpawnModel,
	fedOverrideEffort:  PermGroupsMembersSpawnEffort,
	fedOverrideProfile: PermGroupsMembersSpawnProfile,
	fedOverrideRoles:   PermGroupsMembersSpawnRoles,
	fedOverrideSandbox: PermGroupsMembersSpawnSandbox,
}

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
	return nil
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
	for _, kv := range [][2]string{{"directory", o.Cwd}, {"harness", o.Harness}, {"model", o.Model}, {"effort", o.Effort}, {"roles", strings.Join(o.Roles, ", ")}, {"sandbox profile", o.SandboxProfile}} {
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
	return nil
}
