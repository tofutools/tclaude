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
	// The override allowlist is the other per-request spawn choice a grant opens.
	return normalizeSpawnOverrideAllowlist(slug, policy)
}

func checkSelectableProfile(peer string, group int64, name string) error {
	if name == "" {
		return nil
	}
	if len(name) > 128 || validateGroupName(name) != nil || proto.StripControls(name) != name {
		return fmt.Errorf("invalid requested profile")
	}
	// One answer for a missing, disabled or unlisted profile, so a peer
	// cannot probe which profile names exist here.
	if !slices.ContainsFunc(selectableSpawnProfiles(peer, group), func(p proto.CatalogSpawnProfile) bool { return p.Name == name }) {
		return fmt.Errorf("requested profile is not in this peer's spawn allowlist")
	}
	return nil
}

func selectableSpawnProfiles(peer string, group int64) []proto.CatalogSpawnProfile {
	launchGrant := fedPeerGroupGrant(peer, group, PermGroupsMembersSpawn)
	if launchGrant == nil {
		return nil
	}
	// An explicit grant names its selectable profiles. Unrestricted trust,
	// which already reads the whole registry over node.spawn.read, may also
	// pick any enabled profile that is not operator-only (a spawn request may
	// come from the peer's agents, not only its operator).
	var profiles []*db.SpawnProfile
	allowed := map[string]bool{}
	if grant := fedPeerExplicitGroupGrant(peer, group, PermGroupsMembersSpawn); grant != nil {
		for _, name := range grant.SpawnPolicy.AllowedProfiles {
			if profile, err := db.GetSpawnProfile(name); err == nil && profile != nil {
				allowed[profile.Name] = true
				profiles = append(profiles, profile)
			}
		}
	}
	if db.FederationPeerUnrestricted(peer) {
		all, _ := db.ListSpawnProfiles()
		for _, profile := range all {
			if profile != nil && !profile.OperatorOnly && !allowed[profile.Name] {
				profiles = append(profiles, profile)
			}
		}
	}
	var out []proto.CatalogSpawnProfile
	for _, profile := range profiles {
		if profile == nil || profile.Disabled {
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
