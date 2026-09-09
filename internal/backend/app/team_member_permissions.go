package app

import (
	"reflect"
	"slices"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
)

// ValidateTeamMemberPermissions is also used at transactional publication.
func ValidateTeamMemberPermissions(permissions []model.TeamMemberPermission) error {
	seen := map[model.Action]bool{}
	for _, permission := range permissions {
		if !slices.Contains(allActions, permission.Action) || seen[permission.Action] {
			return fail(ErrInvalid, "member permissions require distinct known actions")
		}
		seen[permission.Action] = true
		if _, err := permission.Scope.Normalize(); err != nil {
			return fail(ErrInvalid, "%v", err)
		}
		if permission.Denied && len(permission.Scope) != 0 {
			return fail(ErrInvalid, "member denials cannot carry a scope")
		}
	}
	return nil
}

func teamMemberAuthority(principal model.Principal, deployment model.DeploymentID, team model.TeamDefinition, members map[string]model.AgentID, pins []model.TeamRolePin, now time.Time) ([]model.AuthorityGrant, []model.AuthorityDenial, error) {
	var grants []model.AuthorityGrant
	var denials []model.AuthorityDenial
	roles := map[model.RoleID]model.TeamRolePin{}
	for _, pin := range pins {
		roles[pin.RoleID] = pin
	}
	for _, member := range team.Members {
		permissions, err := resolveTeamRolePermissions(member, roles)
		if err != nil {
			return nil, nil, err
		}
		if len(permissions) == 0 {
			continue
		}
		if principal.Kind != model.PrincipalOperator {
			return nil, nil, ErrUnauthorized
		}
		if err := ValidateTeamMemberPermissions(permissions); err != nil {
			return nil, nil, err
		}
		subject := model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: members[member.Key]}
		for _, permission := range permissions {
			identity := string(deployment) + ":" + member.Key + ":" + string(permission.Action)
			if permission.Denied {
				denials = append(denials, model.AuthorityDenial{ID: model.DenialID(deterministicOrchestrationID("deny_", identity)), Subject: subject, Action: permission.Action, Revision: 1, CreatedAt: now, UpdatedAt: now})
			} else {
				scope, _ := permission.Scope.Normalize()
				grants = append(grants, model.AuthorityGrant{ID: model.GrantID(deterministicOrchestrationID("grant_", identity)), Subject: subject, Action: permission.Action, Resource: model.ResourceSelector{Kind: model.ResourceAll}, Scope: scope, Revision: 1, CreatedAt: now, UpdatedAt: now})
			}
		}
	}
	return grants, denials, nil
}

// Role conflicts are resolved before explicit member overrides, matching v1.
func resolveTeamRolePermissions(member model.TeamMemberSpec, roles map[model.RoleID]model.TeamRolePin) ([]model.TeamMemberPermission, error) {
	var out []model.TeamMemberPermission
	positions := map[model.Action]int{}
	for _, id := range member.Roles {
		if id == model.GroupOwnerRole {
			continue
		}
		role, ok := roles[id]
		if !ok {
			return nil, fail(ErrInvalid, "team role %s has no admitted definition", id)
		}
		for _, action := range role.Actions {
			scope, err := role.Scopes[action].Normalize()
			if err != nil {
				return nil, fail(ErrInvalid, "%v", err)
			}
			if index, exists := positions[action]; exists {
				if !reflect.DeepEqual(out[index].Scope, scope) {
					return nil, fail(ErrInvalid, "roles grant %s with incompatible scopes", action)
				}
				continue
			}
			positions[action] = len(out)
			out = append(out, model.TeamMemberPermission{Action: action, Scope: scope})
		}
	}
	for _, permission := range member.Permissions {
		if index, exists := positions[permission.Action]; exists {
			out[index] = permission
		} else {
			positions[permission.Action] = len(out)
			out = append(out, permission)
		}
	}
	return out, nil
}
