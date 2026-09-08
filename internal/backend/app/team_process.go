package app

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/tofutools/tclaude/internal/backend/model"
)

func validateTeamPhases(team model.TeamDefinition) error {
	if len(team.AdvisoryProcess) != 0 && len(team.AdvisoryPhases) != 0 {
		return fail(ErrInvalid, "choose either structured advisory phases or legacy phase names")
	}
	phases := team.ProcessPhases()
	if len(phases) > 64 {
		return fail(ErrInvalid, "a team permits at most 64 advisory phases")
	}
	names := make(map[string]bool, len(phases))
	validText := func(text string, limit int) bool {
		return len(text) <= limit && utf8.ValidString(text) && !strings.ContainsRune(text, 0)
	}
	for _, phase := range phases {
		name := strings.ToLower(strings.TrimSpace(phase.Name))
		if name == "" || names[name] || !validText(phase.Name, 256) || strings.ContainsAny(phase.Name, "\r\n") {
			return fail(ErrInvalid, "advisory phase names require bounded unique single-line text")
		}
		names[name] = true
		if !validText(phase.Criteria, 8192) || len(phase.Roles) > 128 {
			return fail(ErrInvalid, "advisory phase guidance exceeds text or role limits")
		}
		for _, role := range phase.Roles {
			if strings.TrimSpace(role) == "" || !validText(role, 256) || strings.ContainsAny(role, "\r\n") {
				return fail(ErrInvalid, "advisory phase roles require bounded single-line labels")
			}
		}
	}
	return nil
}

func teamProcessBrief(team model.TeamDefinition) string {
	phases := team.ProcessPhases()
	if len(phases) == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteString("## Team process\n\nThis process is advisory. Coordinate with the team; phases do not change permissions or gate work.\n")
	for i, phase := range phases {
		roles := strings.Join(phase.Roles, ", ")
		if roles == "" {
			roles = "no specific roles"
		}
		fmt.Fprintf(&out, "\n%d. %s — active roles: %s\n", i+1, phase.Name, roles)
		if phase.Criteria != "" {
			out.WriteString(phase.Criteria + "\n")
		}
	}
	return strings.TrimSpace(out.String())
}

func (s *Service) teamDeploymentView(ctx context.Context, deployment model.TeamDeployment) (TeamDeploymentResult, error) {
	revision, err := s.store.DefinitionRevision(ctx, deployment.Definition.RevisionID)
	if err != nil {
		return TeamDeploymentResult{}, err
	}
	result := TeamDeploymentResult{Deployment: deployment}
	if revision.Team != nil {
		result.Phases = revision.Team.ProcessPhases()
	}
	return result, nil
}

// notifyTeamPhase uses the normal message/notification queue. It is an internal
// consequence of group management, not a public way to choose message authority.
func (s *Service) notifyTeamPhase(ctx context.Context, req AdvanceAdvisoryPhaseRequest, deployment model.TeamDeployment, from string, phase model.TeamPhase) int {
	if err := s.validateMessageSender(ctx, req.Context.Principal); err != nil {
		return 0
	}
	group, err := s.store.Group(ctx, deployment.GroupID)
	if err != nil {
		return 0
	}
	state, err := s.store.AuthorityState(ctx)
	if err != nil {
		return 0
	}
	var audiences []model.MessageAudience
	for _, label := range phase.Roles {
		if strings.EqualFold(strings.TrimSpace(label), "all") {
			audiences = []model.MessageAudience{{GroupID: group.ID}}
			break
		}
		for _, role := range state.Roles {
			if strings.EqualFold(strings.TrimSpace(label), strings.TrimSpace(role.Name)) {
				audiences = append(audiences, model.MessageAudience{GroupID: group.ID, RoleID: role.ID})
			}
		}
	}
	targets := map[model.AgentID]bool{}
	for _, audience := range audiences {
		ids, err := s.store.ResolveMessageAudience(ctx, audience)
		if err != nil {
			continue
		}
		for _, id := range ids {
			if id != req.Context.Principal.AgentID {
				targets[id] = true
			}
		}
	}
	body := fmt.Sprintf("The group %q advanced its advisory process from %q to %q — your role is active in this phase.", group.Name, from, phase.Name)
	if phase.Criteria != "" {
		body += "\n\nCriteria:\n" + phase.Criteria
	}
	count := 0
	for id := range targets {
		recipients, _, err := s.resolveRecipients(ctx, req.Context.Principal, model.MessageAudience{AgentIDs: []model.AgentID{id}}, model.MessageAudience{})
		if err != nil {
			continue
		}
		now := s.now().UTC()
		requestID := model.RequestID(deterministicOrchestrationID("request_", string(req.Context.RequestID)+":phase:"+string(deployment.ID)+":"+string(id)))
		subject := "[process] phase: " + phase.Name
		digest := contentHash(struct {
			Deployment    model.DeploymentID
			Expected      model.Revision
			Recipient     model.AgentID
			Subject, Body string
		}{deployment.ID, req.ExpectedRevision, id, subject, body})
		_, err = s.store.CreateMessage(ctx, MessageAdmission{Message: model.Message{ID: model.MessageID(s.newID("msg_")), Sender: req.Context.Principal, Subject: subject, Body: body, Recipients: recipients, CreatedAt: now}, RequestID: requestID, RequestDigest: digest, OperationID: model.OperationID(s.newID("op_")), Authority: []model.AuthorityRequest{{Principal: req.Context.Principal, Action: model.ActionManageMembership, Resource: model.ResourceSelector{Kind: model.ResourceGroup, GroupID: group.ID}}}, Eligibility: audiences, ResultCode: "committed"})
		if err == nil {
			count++
		}
	}
	return count
}
