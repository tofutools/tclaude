package agentd

import (
	"fmt"
	"slices"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Selectable profiles are separate from the receiver's pinned default profile.
// Canonical names keep aliases out of the wire contract and policy comparison.
func normalizeSelectableProfiles(slug string, policy *db.FederationSpawnPolicy) error {
	if len(policy.AllowedProfiles) > 64 {
		return fmt.Errorf("at most 64 selectable profiles are allowed")
	}
	if len(policy.AllowedProfiles) != 0 && slug != PermGroupsMembersSpawn {
		return fmt.Errorf("allowed_profiles requires groups.members.spawn")
	}
	for i, name := range policy.AllowedProfiles {
		profile, err := db.ResolveSpawnProfile(name)
		if err != nil || profile == nil || profile.Disabled || len(profile.Name) > 128 || validateGroupName(profile.Name) != nil {
			return fmt.Errorf("selectable profile %q is unavailable", name)
		}
		policy.AllowedProfiles[i] = profile.Name
	}
	slices.Sort(policy.AllowedProfiles)
	policy.AllowedProfiles = slices.Compact(policy.AllowedProfiles)
	return nil
}

func checkSelectableProfile(peer string, group int64, name string) error {
	if name == "" {
		return nil
	}
	if len(name) > 128 || validateGroupName(name) != nil || proto.StripControls(name) != name {
		return fmt.Errorf("invalid requested profile")
	}
	// Even unrestricted peers need an explicit profile allowlist. A broad trust
	// level must not expose the operator's entire local profile registry.
	grant := fedPeerExplicitGroupGrant(peer, group, PermGroupsMembersSpawn)
	if grant == nil || !slices.Contains(grant.SpawnPolicy.AllowedProfiles, name) {
		return fmt.Errorf("requested profile is not in this peer's spawn allowlist")
	}
	profile, err := db.GetSpawnProfile(name)
	if err != nil || profile == nil || profile.Disabled {
		return fmt.Errorf("requested profile is unavailable")
	}
	return nil
}

func selectableSpawnProfiles(peer string, group int64) []proto.CatalogSpawnProfile {
	grant := fedPeerExplicitGroupGrant(peer, group, PermGroupsMembersSpawn)
	if grant == nil {
		return nil
	}
	launchGrant := fedPeerGroupGrant(peer, group, PermGroupsMembersSpawn)
	if launchGrant == nil {
		return nil
	}
	var out []proto.CatalogSpawnProfile
	for _, name := range grant.SpawnPolicy.AllowedProfiles {
		profile, err := db.GetSpawnProfile(name)
		if err != nil || profile == nil || profile.Disabled {
			continue
		}
		out = append(out, proto.CatalogSpawnProfile{
			Name: profile.Name, Harness: harnessOrDefault(fedFirst(launchGrant.SpawnPolicy.Harness, profile.Harness)),
			Model: fedFirst(launchGrant.SpawnPolicy.Model, profile.Model), Effort: profile.Effort,
		})
	}
	return out
}

func catalogAllowsSpawnProfile(group proto.CatalogGroup, name string) bool {
	for _, profile := range group.SpawnProfiles {
		if profile.Name == name {
			return true
		}
	}
	return false
}
