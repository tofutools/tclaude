package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tofutools/tclaude/internal/backend/model"
)

func teamRhythmPolicy() model.OccurrencePolicy {
	return model.OccurrencePolicy{MissedTicks: model.MissedTickSkip, OfflineDelivery: model.OfflineSkip, Overlap: model.OverlapForbid, MaxActive: 1, ExpiresAfter: time.Hour, Deadline: time.Hour}
}
func teamRhythmCondition(r model.TeamRhythm) (model.AutomationCondition, error) {
	var interval time.Duration
	var err error
	if r.Interval != "" {
		interval, err = time.ParseDuration(strings.TrimSpace(r.Interval))
		if err != nil || interval <= 0 {
			return model.AutomationCondition{}, fail(ErrInvalid, "rhythm interval must be a positive duration")
		}
	}
	return model.AutomationCondition{Kind: model.AutomationSchedule, Schedule: &model.ScheduleCondition{Interval: interval, Cron: r.Cron, Timezone: r.Timezone}}, nil
}
func validateTeamRhythms(rhythms []model.TeamRhythm) error {
	if len(rhythms) > 64 {
		return fail(ErrInvalid, "a team permits at most 64 rhythms")
	}
	names := map[string]bool{}
	for _, r := range rhythms {
		name := strings.ToLower(strings.TrimSpace(r.Name))
		if name == "" || names[name] || len(r.Name) > 256 || len(r.Subject) > 1024 || len(r.Body) > 32768 || !utf8.ValidString(r.Name+r.Subject+r.Body) || strings.ContainsRune(r.Name+r.Subject+r.Body, 0) {
			return fail(ErrInvalid, "rhythms require unique bounded names and valid message text")
		}
		names[name] = true
		if r.RoleLabel != "" {
			if strings.TrimSpace(r.RoleLabel) == "" || r.RoleID != "" {
				return fail(ErrInvalid, "rhythm display role must be nonempty and separate from permission-role filters")
			}
			if err := (model.AgentLabels{Role: r.RoleLabel}).Validate(); err != nil {
				return fail(ErrInvalid, "%v", err)
			}
		}
		if r.RoleID != "" && r.RoleID.Validate() != nil {
			return fail(ErrInvalid, "rhythm role is invalid")
		}
		condition, err := teamRhythmCondition(r)
		if err != nil {
			return err
		}
		if err = validateAutomation(condition, model.AutomationAction{Kind: model.AutomationSendMessage, Message: &model.AutomationMessageAction{Body: r.Body}}, teamRhythmPolicy()); err != nil {
			return err
		}
	}
	return nil
}
func teamRhythmID(deployment model.DeploymentID, index int) model.AutomationRuleID {
	return model.AutomationRuleID(deterministicOrchestrationID("rule_", fmt.Sprintf("%s:authored-rhythm:%d", deployment, index)))
}
func (s *Service) materializeAuthoredTeamRhythms(ctx context.Context, deployment model.TeamDeployment, rhythms []model.TeamRhythm, request RequestContext) error {
	if len(rhythms) == 0 {
		return nil
	}
	owner := request.Principal.Authority
	switch request.Principal.Kind {
	case model.PrincipalOperator:
		owner = model.AuthoritySubject{Kind: model.AuthorityOperator}
	case model.PrincipalAgent:
		owner = model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: request.Principal.AgentID}
	}
	delegation, err := teamRhythmDelegation(request.Principal, deployment.GroupID)
	if err != nil {
		return err
	}
	var creationAuthority *model.AuthorityRequest
	if request.Principal.Kind == model.PrincipalAutomation {
		occurrence, err := s.store.Occurrence(ctx, model.OccurrenceID(request.Principal.AutomationRun))
		if err != nil {
			return err
		}
		creationAuthority = &model.AuthorityRequest{Principal: request.Principal, Action: model.ActionRunAutomation, Resource: model.ResourceSelector{Kind: model.ResourceAutomationRule, AutomationRuleID: occurrence.Occurrence.RuleID}}
	}
	for i, r := range rhythms {
		condition, err := teamRhythmCondition(r)
		if err != nil {
			return err
		}
		// Stable deployment creation time keeps an interrupted materialization retry exact.
		condition.Schedule.Anchor = deployment.CreatedAt.Add(condition.Schedule.Interval)
		id := teamRhythmID(deployment.ID, i)
		_, err = s.saveAutomationRuleAuthorized(ctx, SaveAutomationRuleRequest{Context: RequestContext{Principal: request.Principal, RequestID: model.RequestID(deterministicOrchestrationID("request_", string(id)))}, ID: id, RevisionID: model.AutomationRuleRevisionID(deterministicOrchestrationID("rule_revision_", string(id))), Name: r.Name, Owner: owner, Delegation: delegation, Condition: condition, Action: model.AutomationAction{Kind: model.AutomationSendMessage, Message: &model.AutomationMessageAction{Subject: r.Subject, Body: r.Body, GroupID: deployment.GroupID, RoleID: r.RoleID, RoleLabel: r.RoleLabel}}, Policy: teamRhythmPolicy()}, deployment.ID, creationAuthority)
		if err != nil {
			return err
		}
	}
	return nil
}

// Generated schedules cannot widen an automation parent's accepted message scope
// or lifetime. Operator deployments explicitly create ongoing group nudges.
func teamRhythmDelegation(principal model.Principal, group model.GroupID) (model.AutomationDelegation, error) {
	delegation := model.AutomationDelegation{NoExpiry: true, Actions: []model.Action{model.ActionSendMessage}, Resources: []model.ResourceSelector{{Kind: model.ResourceGroupPeers, GroupID: group}}}
	if principal.Kind == model.PrincipalOperator {
		return delegation, nil
	}
	parent := principal.Delegation
	if principal.Kind != model.PrincipalAutomation || parent == nil || !slices.Contains(parent.Actions, model.ActionSendMessage) {
		return delegation, fail(ErrUnauthorized, "team nudges require delegated message.send and target group members")
	}
	allowed := false
	for _, resource := range parent.Resources {
		if resource.Kind == model.ResourceGroupPeers && resource.GroupID == group {
			allowed = true
		}
	}
	if !allowed {
		return delegation, fail(ErrUnauthorized, "team nudges require delegated target group members")
	}
	delegation.NoExpiry, delegation.ExpiresAt = parent.NoExpiry, parent.ExpiresAt
	return delegation, nil
}
