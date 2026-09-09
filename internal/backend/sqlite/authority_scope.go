package sqlite

import (
	"context"

	"github.com/tofutools/tclaude/internal/backend/model"
)

// Scope values come from the actual effect and current durable identities.
// In particular an agent's memberships do not invent a group context for an
// agent-wide action. Unavailable dimensions and unresolved ancestry selectors
// cannot satisfy a constrained grant.
func grantScopeMatches(ctx context.Context, q queryer, scope model.PermissionScope, request model.AuthorityRequest) bool {
	if len(scope) == 0 {
		return true
	}
	var current model.PermissionContext
	switch request.Resource.Kind {
	case model.ResourceGroup, model.ResourceGroupPeers:
		if err := q.QueryRowContext(ctx, `SELECT name FROM groups WHERE id=? AND tombstoned=0`, request.Resource.GroupID).Scan(&current.Group); err != nil {
			return false
		}
	case model.ResourceAgent:
		current.TargetAgent = string(request.Resource.AgentID)
	case model.ResourceExecution:
		if err := q.QueryRowContext(ctx, `SELECT COALESCE(agent_id,'') FROM executions WHERE id=?`, request.Resource.ExecutionID).Scan(&current.TargetAgent); err != nil {
			return false
		}
	}
	return scope.Matches(current, nil)
}
