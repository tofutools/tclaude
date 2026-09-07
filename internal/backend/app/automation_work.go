package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	cronv3 "github.com/robfig/cron/v3"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (s *Service) reconcileAutomation(ctx context.Context) ([]model.OccurrenceID, error) {
	now := s.now().UTC()
	rules, err := s.store.ListAutomationRules(ctx, false)
	if err != nil {
		return nil, err
	}
	var touched []model.OccurrenceID
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		revision, readErr := s.store.AutomationRuleRevision(ctx, rule.HeadRevisionID)
		if readErr != nil {
			return touched, readErr
		}
		if revision.Condition.Kind != model.AutomationSchedule || revision.Condition.Schedule == nil {
			continue
		}
		existing, listErr := s.store.OccurrencesForRule(ctx, rule.ID)
		if listErr != nil {
			return touched, listErr
		}
		if revision.Policy.Overlap != model.OverlapAllow && hasActiveOccurrence(existing) {
			continue
		}
		scheduled, due, scheduleErr := latestScheduleTick(revision, existing, now)
		if scheduleErr != nil {
			return touched, scheduleErr
		}
		if !due {
			continue
		}
		key := fmt.Sprintf("schedule:%d", scheduled.UnixNano())
		occurrenceID := model.OccurrenceID(deterministicOrchestrationID("occurrence_", string(revision.ID)+":"+key))
		requestID := model.RequestID(deterministicOrchestrationID("request_", string(revision.ID)+":"+key))
		requester := model.AutomationPrincipal(string(occurrenceID), revision.Owner, revision.Delegation)
		recipients, recipientErr := s.automationRecipients(ctx, revision.Action)
		if recipientErr != nil {
			return touched, recipientErr
		}
		occurrence := model.AutomationOccurrence{ID: occurrenceID, RuleID: rule.ID, RuleRevisionID: revision.ID, SourceOccurrenceKey: key, RequestID: requestID, Requester: requester, ScheduledAt: scheduled, EligibleAt: scheduled, ExpiresAt: scheduled.Add(revision.Policy.ExpiresAfter), State: model.OccurrencePending, Recipients: recipients, Revision: 1, CreatedAt: now, UpdatedAt: now}
		_, _, materializeErr := s.store.MaterializeOccurrence(ctx, occurrence, rule.Revision)
		if materializeErr != nil && !errors.Is(materializeErr, ErrConflict) {
			return touched, materializeErr
		}
		touched = append(touched, occurrenceID)
	}
	pending, err := s.store.PendingOccurrences(ctx)
	if err != nil {
		return touched, err
	}
	for _, occurrence := range pending {
		if occurrence.Occurrence.EligibleAt.After(now) {
			continue
		}
		if !occurrence.Occurrence.ExpiresAt.After(now) {
			if _, err = s.store.UpdateOccurrence(ctx, occurrence.Occurrence.ID, occurrence.Occurrence.Revision, model.OccurrenceExpired, occurrence.Occurrence.OperationID, occurrence.Occurrence.WorkRunID, occurrence.Occurrence.DeploymentID, expiredRecipients(occurrence.Occurrence.Recipients), now); err != nil {
				return touched, err
			}
			continue
		}
		revision, readErr := s.store.AutomationRuleRevision(ctx, occurrence.Occurrence.RuleRevisionID)
		if readErr != nil {
			return touched, readErr
		}
		if occurrence.Occurrence.WorkRunID != "" {
			work, workErr := s.store.WorkRun(ctx, occurrence.Occurrence.WorkRunID)
			if workErr != nil {
				return touched, workErr
			}
			state := occurrence.Occurrence.State
			switch work.Run.State {
			case model.WorkRunSucceeded:
				state = model.OccurrenceDelivered
			case model.WorkRunFailed, model.WorkRunCancelled:
				state = model.OccurrenceDenied
			case model.WorkRunUncertain:
				state = model.OccurrenceUncertain
			}
			if state != occurrence.Occurrence.State {
				if _, err = s.store.UpdateOccurrence(ctx, occurrence.Occurrence.ID, occurrence.Occurrence.Revision, state, occurrence.Occurrence.OperationID, work.Run.ID, occurrence.Occurrence.DeploymentID, occurrence.Occurrence.Recipients, now); err != nil {
					return touched, err
				}
			}
			continue
		}
		if occurrence.Occurrence.DeploymentID != "" {
			deployment, deploymentErr := s.store.TeamDeployment(ctx, occurrence.Occurrence.DeploymentID)
			if deploymentErr != nil {
				return touched, deploymentErr
			}
			if deployment.State == model.DeploymentDeploying {
				if _, workErr := s.store.WorkRun(ctx, deployment.WorkRunID); errors.Is(workErr, ErrNotFound) {
					definition, definitionErr := s.store.DefinitionRevision(ctx, deployment.Definition.RevisionID)
					if definitionErr != nil || definition.Team == nil || definition.DefinitionID != deployment.Definition.DefinitionID || definition.ContentHash != deployment.Definition.ContentHash {
						if definitionErr == nil {
							definitionErr = ErrConflict
						}
						return touched, definitionErr
					}
					if workErr = s.startTeamDeploymentProcess(ctx, deployment, *definition.Team, occurrence.Occurrence.Requester, occurrence.Occurrence.RuleID); workErr != nil {
						if _, updateErr := s.store.UpdateTeamDeployment(ctx, deployment.ID, deployment.Revision, model.DeploymentPartial, deployment.AdvisoryPhase, now); updateErr != nil {
							return touched, errors.Join(workErr, updateErr)
						}
						continue
					}
				} else if workErr != nil {
					return touched, workErr
				}
			}
			state := occurrence.Occurrence.State
			switch deployment.State {
			case model.DeploymentReady:
				state = model.OccurrenceDelivered
			case model.DeploymentPartial, model.DeploymentStopped:
				state = model.OccurrenceDenied
			}
			if state != occurrence.Occurrence.State {
				if _, err = s.store.UpdateOccurrence(ctx, occurrence.Occurrence.ID, occurrence.Occurrence.Revision, state, occurrence.Occurrence.OperationID, occurrence.Occurrence.WorkRunID, deployment.ID, occurrence.Occurrence.Recipients, now); err != nil {
					return touched, err
				}
			}
			continue
		}
		currentRule, ruleErr := s.store.AutomationRule(ctx, occurrence.Occurrence.RuleID)
		if ruleErr != nil {
			return touched, ruleErr
		}
		if occurrence.Occurrence.State == model.OccurrencePending && !currentRule.Rule.Enabled {
			if _, err = s.store.UpdateOccurrence(ctx, occurrence.Occurrence.ID, occurrence.Occurrence.Revision, model.OccurrenceDenied, "", "", "", occurrence.Occurrence.Recipients, now); err != nil {
				return touched, err
			}
			touched = append(touched, occurrence.Occurrence.ID)
			continue
		}
		switch revision.Action.Kind {
		case model.AutomationStartWork:
			start := *revision.Action.Work
			start.RequestID = occurrence.Occurrence.RequestID
			start.Scope.RuleID, start.Scope.OccurrenceID = occurrence.Occurrence.RuleID, occurrence.Occurrence.ID
			workID := model.WorkRunID(deterministicOrchestrationID("work_", string(occurrence.Occurrence.ID)))
			work, startErr := s.StartProcess(ctx, StartProcessRequest{Context: RequestContext{Principal: occurrence.Occurrence.Requester, RequestID: occurrence.Occurrence.RequestID}, ID: workID, Start: start})
			if startErr != nil {
				if _, err = s.store.UpdateOccurrence(ctx, occurrence.Occurrence.ID, occurrence.Occurrence.Revision, model.OccurrenceDenied, "", "", "", occurrence.Occurrence.Recipients, now); err != nil {
					return touched, err
				}
				continue
			}
			if _, err = s.store.UpdateOccurrence(ctx, occurrence.Occurrence.ID, occurrence.Occurrence.Revision, model.OccurrenceAdmitted, "", work.Run.ID, "", occurrence.Occurrence.Recipients, now); err != nil {
				return touched, err
			}
		case model.AutomationSendMessage:
			recipients := append([]model.OccurrenceRecipient(nil), occurrence.Occurrence.Recipients...)
			if !hasPendingRecipient(recipients) {
				continue
			}
			audience := automationMessageAudience(*revision.Action.Message)
			for i := range recipients {
				if recipients[i].Disposition != model.RecipientPending {
					continue
				}
				requestID := model.RequestID(deterministicOrchestrationID("request_", string(occurrence.Occurrence.ID)+":"+string(recipients[i].AgentID)))
				send := SendMessageRequest{RequestContext: RequestContext{Principal: occurrence.Occurrence.Requester, RequestID: requestID}, To: model.MessageAudience{AgentIDs: []model.AgentID{recipients[i].AgentID}}, RecipientEligibility: &audience, Body: revision.Action.Message.Body}
				digest, digestErr := authoredMessageRequestDigest(send)
				if digestErr != nil {
					return touched, digestErr
				}
				if repeated, ok, repeatErr := s.store.MessageByRequest(ctx, send.Principal, requestID, digest); repeatErr != nil {
					return touched, repeatErr
				} else if ok {
					recipients[i].OperationID = repeated.Operation.ID
					recipients[i].Disposition, recipients[i].Detail = automationMessageDisposition(repeated.Operation.ResultCode)
					continue
				}
				online, onlineErr := s.automationRecipientOnline(ctx, recipients[i].AgentID)
				if onlineErr != nil {
					recipients[i].Disposition, recipients[i].Detail = model.RecipientDenied, onlineErr.Error()
					continue
				}
				if !online && revision.Policy.OfflineDelivery == model.OfflineSkip {
					recipients[i].Disposition, recipients[i].Detail = model.RecipientSkipped, "recipient offline; skipped by policy"
					continue
				}
				send.AdmissionResultCode = "automation_delivered"
				if !online {
					send.AdmissionResultCode = "automation_queued"
				}
				sent, sendErr := s.SendMessage(ctx, send)
				if sendErr != nil {
					recipients[i].Disposition, recipients[i].Detail = model.RecipientDenied, sendErr.Error()
					continue
				}
				recipients[i].OperationID = sent.Operation.ID
				recipients[i].Disposition, recipients[i].Detail = automationMessageDisposition(sent.Operation.ResultCode)
			}
			state := automationMessageOccurrenceState(recipients)
			if _, err = s.store.UpdateOccurrence(ctx, occurrence.Occurrence.ID, occurrence.Occurrence.Revision, state, "", "", "", recipients, now); err != nil {
				return touched, err
			}
		case model.AutomationDeployTeam:
			deploymentID := model.DeploymentID(deterministicOrchestrationID("deployment_", string(occurrence.Occurrence.ID)))
			deployed, deployErr := s.DeployTeam(ctx, DeployTeamRequest{Context: RequestContext{Principal: occurrence.Occurrence.Requester, RequestID: occurrence.Occurrence.RequestID}, DeploymentID: deploymentID, Instantiation: *revision.Action.Team})
			if deployed.Deployment.ID == "" {
				if _, err = s.store.UpdateOccurrence(ctx, occurrence.Occurrence.ID, occurrence.Occurrence.Revision, model.OccurrenceDenied, "", "", "", occurrence.Occurrence.Recipients, now); err != nil {
					return touched, err
				}
			} else if deployErr != nil {
				// The deployment action was durably admitted before its child
				// process failed. Its normal deployment reconciliation owns the
				// resulting partial/denied disposition.
				continue
			}
		}
		touched = append(touched, occurrence.Occurrence.ID)
	}
	return uniqueOccurrenceIDs(touched), nil
}

func latestScheduleTick(revision model.AutomationRuleRevision, existing []OccurrenceRecord, now time.Time) (time.Time, bool, error) {
	condition := revision.Condition.Schedule
	anchor := condition.Anchor.UTC()
	if anchor.IsZero() {
		anchor = revision.CreatedAt.UTC()
	}
	if now.Before(anchor) {
		return time.Time{}, false, nil
	}
	if condition.Interval > 0 {
		return anchor.Add(time.Duration(now.Sub(anchor)/condition.Interval) * condition.Interval), true, nil
	}
	location, err := time.LoadLocation(condition.Timezone)
	if err != nil {
		return time.Time{}, false, fail(ErrInvalid, "schedule timezone is unavailable: %v", err)
	}
	schedule, err := cronv3.ParseStandard(condition.Cron)
	if err != nil {
		return time.Time{}, false, fail(ErrInvalid, "cron schedule is invalid: %v", err)
	}
	cursor := anchor.In(location)
	for _, occurrence := range existing {
		if occurrence.Occurrence.ScheduledAt.After(cursor) {
			cursor = occurrence.Occurrence.ScheduledAt.In(location)
		}
	}
	var latest time.Time
	// Standard cron cannot fire more than once per minute. Five leap-aware
	// years bounds cold-start catch-up while covering annual and Feb-29 rules.
	for range 5*366*24*60 + 1 {
		next := schedule.Next(cursor)
		if next.After(now.In(location)) {
			return latest.UTC(), !latest.IsZero(), nil
		}
		latest, cursor = next, next
	}
	return time.Time{}, false, fail(ErrInvalid, "cron catch-up exceeds bounded five-year window")
}

func (s *Service) automationRecipients(ctx context.Context, action model.AutomationAction) ([]model.OccurrenceRecipient, error) {
	if action.Message == nil {
		return nil, nil
	}
	ids, err := s.store.ResolveMessageAudience(ctx, automationMessageAudience(*action.Message))
	if err != nil {
		return nil, err
	}
	result := make([]model.OccurrenceRecipient, 0, len(ids))
	for _, id := range ids {
		result = append(result, model.OccurrenceRecipient{AgentID: id, Disposition: model.RecipientPending})
	}
	return result, nil
}

func automationMessageAudience(action model.AutomationMessageAction) model.MessageAudience {
	return model.MessageAudience{AgentIDs: append([]model.AgentID(nil), action.AgentIDs...), GroupID: action.GroupID, RoleID: action.RoleID}
}

func (s *Service) automationRecipientOnline(ctx context.Context, id model.AgentID) (bool, error) {
	agent, err := s.store.Agent(ctx, id)
	if err != nil {
		return false, err
	}
	if agent.Lifecycle != model.AgentActive || agent.PrimaryExecutionID == "" {
		return false, nil
	}
	execution, err := s.store.Execution(ctx, agent.PrimaryExecutionID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return execution.State == model.ExecutionReleased || execution.State == model.ExecutionRunning, nil
}

func automationMessageDisposition(resultCode string) (model.RecipientDisposition, string) {
	if resultCode == "automation_queued" {
		return model.RecipientQueued, "recipient offline; durable message queued"
	}
	return model.RecipientDelivered, "message admitted"
}

func hasPendingRecipient(recipients []model.OccurrenceRecipient) bool {
	for _, recipient := range recipients {
		if recipient.Disposition == model.RecipientPending {
			return true
		}
	}
	return false
}

func automationMessageOccurrenceState(recipients []model.OccurrenceRecipient) model.OccurrenceState {
	var delivered, queued, denied, skipped int
	for _, recipient := range recipients {
		switch recipient.Disposition {
		case model.RecipientDelivered:
			delivered++
		case model.RecipientQueued:
			queued++
		case model.RecipientDenied:
			denied++
		case model.RecipientSkipped:
			skipped++
		}
	}
	if queued > 0 && denied == 0 && skipped == 0 {
		return model.OccurrenceAdmitted
	}
	if denied == len(recipients) || skipped == len(recipients) || denied+skipped == len(recipients) {
		return model.OccurrenceDenied
	}
	if delivered == len(recipients) {
		return model.OccurrenceDelivered
	}
	return model.OccurrencePartial
}

func hasActiveOccurrence(records []OccurrenceRecord) bool {
	for _, record := range records {
		switch record.Occurrence.State {
		case model.OccurrencePending, model.OccurrenceAdmitted, model.OccurrencePartial, model.OccurrenceUncertain:
			return true
		}
	}
	return false
}

func deterministicOrchestrationID(prefix, source string) string {
	sum := sha256.Sum256([]byte(source))
	return prefix + hex.EncodeToString(sum[:12])
}

func deniedRecipients(in []model.OccurrenceRecipient, detail string) []model.OccurrenceRecipient {
	out := append([]model.OccurrenceRecipient(nil), in...)
	for i := range out {
		out[i].Disposition, out[i].Detail = model.RecipientDenied, detail
	}
	return out
}

func expiredRecipients(in []model.OccurrenceRecipient) []model.OccurrenceRecipient {
	out := append([]model.OccurrenceRecipient(nil), in...)
	for i := range out {
		if out[i].Disposition == model.RecipientPending || out[i].Disposition == model.RecipientQueued {
			out[i].Disposition = model.RecipientExpired
		}
	}
	return out
}

func uniqueOccurrenceIDs(ids []model.OccurrenceID) []model.OccurrenceID {
	seen := make(map[model.OccurrenceID]bool, len(ids))
	result := ids[:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
	}
	return result
}
