package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

// Birth permissions are published only for agents created in this transaction.
// They become ordinary editable grants/denials; deployment receipt replay never
// recreates them after a later revocation.
func publishTeamMemberAuthority(ctx context.Context, tx *sql.Tx, deployment model.TeamDeployment, group model.Group, agents []model.Agent, principal model.Principal) error {
	if len(deployment.MemberGrants)+len(deployment.MemberDenials) == 0 {
		return nil
	}
	if principal.Kind != model.PrincipalOperator {
		return app.ErrUnauthorized
	}
	created := map[model.AgentID]bool{}
	for _, agent := range agents {
		created[agent.ID] = true
	}
	members := map[model.AgentID]bool{}
	for _, agentID := range deployment.Members {
		members[agentID] = true
	}
	inGroup := map[model.AgentID]bool{}
	for _, id := range group.Members {
		inGroup[id] = true
	}
	validSubject := func(subject model.AuthoritySubject) bool {
		return subject.Kind == model.AuthorityAgent && subject.ExecutionID == "" && created[subject.AgentID] && members[subject.AgentID] && inGroup[subject.AgentID]
	}
	seen := map[model.AgentID]map[model.Action]bool{}
	validate := func(subject model.AuthoritySubject, permission model.TeamMemberPermission) error {
		if !validSubject(subject) {
			return app.ErrInvalid
		}
		if seen[subject.AgentID] == nil {
			seen[subject.AgentID] = map[model.Action]bool{}
		}
		if seen[subject.AgentID][permission.Action] {
			return app.ErrInvalid
		}
		seen[subject.AgentID][permission.Action] = true
		return app.ValidateTeamMemberPermissions([]model.TeamMemberPermission{permission})
	}
	for _, grant := range deployment.MemberGrants {
		if err := validate(grant.Subject, model.TeamMemberPermission{Action: grant.Action, Scope: grant.Scope}); err != nil {
			return err
		}
		if err := app.ValidateAuthorityGrant(grant); err != nil {
			return err
		}
		if grant.Resource != (model.ResourceSelector{Kind: model.ResourceAll}) || !reflect.DeepEqual(grant.Bounds, model.ConfigurationBounds{}) || grant.ExpiresAt != nil {
			return app.ErrInvalid
		}
		scope, _ := grant.Scope.Normalize()
		encoded, err := json.Marshal(scope)
		if err != nil {
			return err
		}
		bounds, err := json.Marshal(grant.Bounds)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO authority_grants(id,subject_kind,subject_id,action,resource_kind,resource_id,bounds_json,expires_at,revision,created_at,updated_at,scope_json) VALUES(?,?,?,?,?,'',?,NULL,1,?,?,?)`, grant.ID, model.AuthorityAgent, grant.Subject.AgentID, grant.Action, model.ResourceAll, bounds, nanos(grant.CreatedAt), nanos(grant.UpdatedAt), encoded); err != nil {
			return classify(err)
		}
	}
	for _, denial := range deployment.MemberDenials {
		if err := validate(denial.Subject, model.TeamMemberPermission{Action: denial.Action, Denied: true}); err != nil {
			return err
		}
		if err := app.ValidateAuthorityDenial(denial); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO authority_denials(id,subject_kind,subject_id,action,revision,created_at,updated_at) VALUES(?,?,?,?,1,?,?)`, denial.ID, model.AuthorityAgent, denial.Subject.AgentID, denial.Action, nanos(denial.CreatedAt), nanos(denial.UpdatedAt)); err != nil {
			return classify(err)
		}
	}
	return nil
}
