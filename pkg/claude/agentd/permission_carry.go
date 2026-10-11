package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const PermAgentsReceivePermissions = "agents.receive.permissions"
const PermSelfTeleportPermissions = "self.teleport.permissions"

// Each decision is receiver-generated; source rows remain advisory until this
// policy is checked again immediately before the normal spawn path.
type carriedPermissionDecision struct {
	Slug        string          `json:"slug"`
	Effect      string          `json:"effect"`
	SourceScope json.RawMessage `json:"source_scope,omitempty"`
	Scope       json.RawMessage `json:"scope,omitempty"`
	Decision    string          `json:"decision"`
	Reason      string          `json:"reason"`
}

type permissionCarryPolicy struct {
	Peer           string
	GroupID        int64
	Group          string
	Enabled        bool
	AllowSensitive bool
}

func carrySensitiveSlug(slug string) bool {
	return strings.HasPrefix(slug, "permissions.") || strings.HasPrefix(slug, "human.") || strings.HasPrefix(slug, "federation.") || strings.HasPrefix(slug, "sandbox.") || slug == PermConfigImport || slug == PermAgentSandboxImplementation || slug == PermSandboxProfilesManage
}

// Permission seeds are historical launch configuration, not current standing
// authority. Match the central resolver's override > group > defaults order.
func carrySourceTier(source string) int {
	switch {
	case strings.HasPrefix(source, "profile:"), strings.HasPrefix(source, "role:"), strings.HasPrefix(source, "ownership:"):
		return -1
	case source == "defaults":
		return 0
	case strings.HasPrefix(source, "group:"):
		return 1
	default:
		return 2
	}
}
func standingCarryRows(rows []agentbundle.Permission) []agentbundle.Permission {
	tiers := map[string]int{}
	for _, row := range rows {
		tiers[row.Slug] = max(tiers[row.Slug], carrySourceTier(row.Source))
	}
	var result []agentbundle.Permission
	for _, row := range rows {
		if tier := carrySourceTier(row.Source); tier >= 0 && tier == tiers[row.Slug] {
			result = append(result, row)
		}
	}
	return result
}

func planCarriedPermissions(rows []agentbundle.Permission, p permissionCarryPolicy) ([]carriedPermissionDecision, map[string]db.PermissionOverride) {
	decisions := make([]carriedPermissionDecision, 0, len(rows))
	overrides := map[string]db.PermissionOverride{}
	tiers := map[string]int{}
	for _, row := range rows {
		tiers[row.Slug] = max(tiers[row.Slug], carrySourceTier(row.Source))
	}
	unrestricted := p.Peer == "" || db.FederationPeerUnrestricted(p.Peer)
	var receive *db.FederationPeerGrant
	if p.Peer != "" {
		receive = fedPeerGroupGrant(p.Peer, p.GroupID, PermAgentsReceivePermissions)
	}
	for _, row := range rows {
		d := carriedPermissionDecision{Slug: row.Slug, Effect: row.Effect, SourceScope: row.Scope, Decision: "drop"}
		switch {
		case !p.Enabled:
			d.Reason = "permission carry was not requested"
		case !IsKnownPermSlug(row.Slug):
			d.Reason = "unknown permission on receiver"
		case strings.HasPrefix(row.Source, "ownership:"):
			d.Reason = "source ownership does not travel"
		case carrySourceTier(row.Source) < 0:
			d.Reason = "spawn-time profile and role authority does not travel"
		case carrySourceTier(row.Source) < tiers[row.Slug]:
			d.Reason = "shadowed by current source permission tier"
		case row.Effect == "deny":
			d.Decision, d.Reason = "apply", "deny only narrows receiver authority"
		case row.Effect != "grant":
			d.Reason = "invalid permission effect"
		case !unrestricted && receive == nil:
			d.Reason = "receiver has not granted agents.receive.permissions"
		case receive != nil && len(receive.SpawnPolicy.PermissionSlugs) > 0 && !slices.Contains(receive.SpawnPolicy.PermissionSlugs, row.Slug):
			d.Reason = "outside receiver permission allowlist"
		case carrySensitiveSlug(row.Slug) && (!unrestricted || !p.AllowSensitive):
			d.Reason = "sensitive permission needs unrestricted trust and explicit receiver opt-in"
		default:
			scope, _, err := parsePermissionScope(row.Scope)
			if err != nil || validatePermissionScopeForSlug(row.Slug, scope) != nil {
				d.Reason = "invalid source scope"
				break
			}
			unsafe := false
			for dim := range scope {
				if dim != ScopeDimGroup {
					unsafe = true
				}
			}
			if unsafe {
				d.Reason = "source-local scope has no receiver mapping"
				break
			}
			d.Decision, d.Reason = "apply", "authorized by receiver"
			if len(scope) > 0 || (!unrestricted || strings.HasPrefix(row.Source, "group:")) && containsScopeDim(permissionScopeDimsForSlug(row.Slug), ScopeDimGroup) {
				if p.Group == "" {
					d.Decision, d.Reason = "drop", "no landing group for scope remap"
					break
				}
				if scope == nil {
					scope = PermissionScope{}
				}
				scope[ScopeDimGroup] = []string{p.Group}
				raw, _ := json.Marshal(scope)
				d.Scope = raw
				d.Decision, d.Reason = "remap", "group scope remapped to landing group"
			}
		}
		decisions = append(decisions, d)
	}
	// A deny wins over every source grant. Represent only safe, exact unions;
	// one stored per-agent override cannot express unrelated AND combinations.
	bySlug := map[string][]int{}
	for i, d := range decisions {
		if d.Decision != "drop" {
			bySlug[d.Slug] = append(bySlug[d.Slug], i)
		}
	}
	slugs := make([]string, 0, len(bySlug))
	for slug := range bySlug {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	for _, slug := range slugs {
		ids := bySlug[slug]
		deny, unscoped := false, false
		scopes := map[string]bool{}
		for _, i := range ids {
			d := decisions[i]
			deny = deny || d.Effect == "deny"
			unscoped = unscoped || len(d.Scope) == 0
			scopes[string(d.Scope)] = true
		}
		if deny {
			overrides[slug] = db.Deny()
			for _, i := range ids {
				if decisions[i].Effect != "deny" {
					decisions[i].Decision, decisions[i].Reason = "drop", "source deny wins"
				}
			}
		} else if unscoped {
			overrides[slug] = db.Grant()
		} else if len(scopes) == 1 {
			overrides[slug] = db.ScopedOverride("grant", string(decisions[ids[0]].Scope))
		} else {
			for _, i := range ids {
				decisions[i].Decision, decisions[i].Reason = "drop", "combined source scopes cannot be represented safely"
			}
		}
	}
	return decisions, overrides
}

type permissionCarryContextKey struct{}
type permissionCarryLaunchContextKey struct{}
type permissionCarryLaunch struct {
	Policy   permissionCarryPolicy
	Rows     []agentbundle.Permission
	Expected map[string]db.PermissionOverride
}

func (p permissionCarryLaunch) check() error {
	_, current := planCarriedPermissions(p.Rows, p.Policy)
	if !reflect.DeepEqual(current, p.Expected) {
		return errors.New("receiver permission carry policy changed; preview the import again")
	}
	return nil
}

func permissionCarrySummary(decisions []carriedPermissionDecision) string {
	counts := map[string]int{}
	var dropped []string
	for _, d := range decisions {
		counts[d.Decision]++
		if d.Decision == "drop" {
			dropped = append(dropped, d.Slug+": "+d.Reason)
		}
	}
	summary := fmt.Sprintf("Permission carry: %d applied, %d remapped, %d dropped. Receiver policy decides the visiting copy's grants; no source ownership or sudo leases travel.", counts["apply"], counts["remap"], counts["drop"])
	if len(dropped) > 0 {
		sort.Strings(dropped)
		summary += " Dropped: " + strings.Join(dropped[:min(len(dropped), 8)], "; ")
	}
	return summary
}
