package agentd

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Remote spawn launch overrides: the launch settings a peer may choose per
// spawn request instead of leaving them to this node's grant policy and group
// defaults. Choosing a directory on this machine is the sensitive one, so
// every field is closed by default: unrestricted trust opens all of them, an
// explicit groups.members.spawn grant opens the ones it lists
// (allowed_overrides), and a request without a spawn grant (operator
// approval) may set none. The ordinary group spawn path still applies every
// launch guardrail to the result.
const (
	fedOverrideCwd     = "cwd"
	fedOverrideHarness = "harness"
	fedOverrideModel   = "model"
	fedOverrideEffort  = "effort"
)

var fedSpawnOverrideFields = []string{fedOverrideCwd, fedOverrideHarness, fedOverrideModel, fedOverrideEffort}

var (
	// No leading '-': a model reaches harness command lines, where it must
	// never read as a flag.
	fedSpawnModelRE  = regexp.MustCompile(`^[A-Za-z0-9._:/@\[\]+][A-Za-z0-9._:/@\[\]+-]{0,127}$`)
	fedSpawnEffortRE = regexp.MustCompile(`^[a-z]{1,32}$`)
)

// normalizeSpawnOverrideAllowlist validates a grant's allowed_overrides.
func normalizeSpawnOverrideAllowlist(slug string, policy *db.FederationSpawnPolicy) error {
	if len(policy.AllowedOverrides) == 0 {
		policy.AllowedOverrides = nil
		return nil
	}
	if slug != PermGroupsMembersSpawn {
		return fmt.Errorf("allowed_overrides requires groups.members.spawn")
	}
	for _, field := range policy.AllowedOverrides {
		if !slices.Contains(fedSpawnOverrideFields, field) {
			return fmt.Errorf("unknown spawn override %q (allowed: cwd, harness, model, effort)", field)
		}
	}
	slices.Sort(policy.AllowedOverrides)
	policy.AllowedOverrides = slices.Compact(policy.AllowedOverrides)
	return nil
}

// fedSpawnOverridesAllowed reports which launch settings peer may choose for
// a spawn into group. Only a grant that also auto-approves (or unrestricted
// trust) opens any: an operator-approved request carries none.
func fedSpawnOverridesAllowed(peer string, groupID int64) map[string]bool {
	out := map[string]bool{}
	if db.FederationPeerUnrestricted(peer) {
		if fedPeerGroupGrant(peer, groupID, PermGroupsMembersSpawn) != nil {
			for _, field := range fedSpawnOverrideFields {
				out[field] = true
			}
		}
		return out
	}
	grant := fedPeerExplicitGroupGrant(peer, groupID, PermGroupsMembersSpawn)
	if grant == nil {
		return out
	}
	for _, field := range grant.SpawnPolicy.AllowedOverrides {
		if slices.Contains(fedSpawnOverrideFields, field) {
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
		return fmt.Errorf("profile %q runs harness %q; choose that harness or leave the harness blank", profile.Name, pinned)
	}
	return nil
}

// fedSpawnOverridesStillAllowed is what an operator approval launches with:
// a requested setting the operator did not set explicitly applies only while
// the current grant or trust still lets the peer choose it (a revoked or
// narrowed grant stops it). It returns the kept settings and the dropped
// field names.
func fedSpawnOverridesStillAllowed(peer string, groupID int64, o db.FederationSpawnOverrides, explicit fedSpawnApproveReq) (db.FederationSpawnOverrides, []string) {
	allowed := fedSpawnOverridesAllowed(peer, groupID)
	var dropped []string
	keep := func(field, value, operator string) string {
		if value == "" || operator != "" {
			return value
		}
		if !allowed[field] {
			dropped = append(dropped, field)
			return ""
		}
		return value
	}
	return db.FederationSpawnOverrides{
		Cwd:     keep(fedOverrideCwd, o.Cwd, explicit.Cwd),
		Harness: keep(fedOverrideHarness, o.Harness, explicit.Harness),
		Model:   keep(fedOverrideModel, o.Model, explicit.Model),
		Effort:  keep(fedOverrideEffort, o.Effort, explicit.Effort),
	}, dropped
}

// fedSpawnOverridesText lists requested launch settings for an operator
// notice, one per line, or "" when there are none.
func fedSpawnOverridesText(o db.FederationSpawnOverrides) string {
	out := ""
	for _, kv := range [][2]string{{"directory", o.Cwd}, {"harness", o.Harness}, {"model", o.Model}, {"effort", o.Effort}} {
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
	for field, value := range map[string]string{fedOverrideCwd: o.Cwd, fedOverrideHarness: o.Harness, fedOverrideModel: o.Model, fedOverrideEffort: o.Effort} {
		if value != "" && !allowed[field] {
			return fmt.Errorf("this node does not let you choose the %s for this group", field)
		}
	}
	return nil
}
