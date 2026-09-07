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
		if revision.Condition.Kind == model.AutomationTrigger && revision.Condition.Trigger != nil {
			triggered, triggerErr := s.reconcileTriggerRule(ctx, rule, revision, now)
			if triggerErr != nil {
				return touched, triggerErr
			}
			touched = append(touched, triggered...)
			continue
		}
		if revision.Condition.Kind != model.AutomationSchedule || revision.Condition.Schedule == nil {
			continue
		}
		cursor, cursorRevision, cursorErr := s.store.ScheduleCursor(ctx, rule.ID)
		if cursorErr != nil {
			return touched, cursorErr
		}
		initial := cursor.IsZero()
		if initial {
			cursor = revision.CreatedAt.UTC()
		}
		scheduled, dueCount, scheduleErr := scheduleTicksAfter(revision, cursor, now, initial)
		if scheduleErr != nil {
			return touched, scheduleErr
		}
		if dueCount == 0 {
			continue
		}
		if revision.Policy.MissedTicks == model.MissedTickSkip && dueCount > 1 {
			if _, _, scheduleErr = s.store.AdvanceSchedule(ctx, nil, rule.ID, rule.Revision, cursorRevision, scheduled); scheduleErr != nil && !errors.Is(scheduleErr, ErrConflict) {
				return touched, scheduleErr
			}
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
		created, _, materializeErr := s.store.AdvanceSchedule(ctx, &occurrence, rule.ID, rule.Revision, cursorRevision, scheduled)
		if materializeErr != nil && !errors.Is(materializeErr, ErrConflict) {
			return touched, materializeErr
		}
		if materializeErr == nil {
			touched = append(touched, created.Occurrence.ID)
		}
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
		currentRule, ruleErr := s.store.AutomationRule(ctx, occurrence.Occurrence.RuleID)
		if ruleErr != nil {
			return touched, ruleErr
		}
		if (occurrence.Occurrence.State == model.OccurrencePending || occurrence.Occurrence.State == model.OccurrenceParked) && !currentRule.Rule.Enabled {
			if _, err = s.store.UpdateOccurrence(ctx, occurrence.Occurrence.ID, occurrence.Occurrence.Revision, model.OccurrenceDenied, occurrence.Occurrence.OperationID, occurrence.Occurrence.WorkRunID, occurrence.Occurrence.DeploymentID, deniedRecipients(occurrence.Occurrence.Recipients, "automation rule disabled"), now); err != nil {
				return touched, err
			}
			touched = append(touched, occurrence.Occurrence.ID)
			continue
		}
		if occurrence.Occurrence.State == model.OccurrenceParked {
			var ready bool
			occurrence, ready, readErr = s.prepareParkedOccurrence(ctx, occurrence, revision, now)
			if readErr != nil {
				return touched, readErr
			}
			if !ready {
				continue
			}
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
		switch revision.Action.Kind {
		case model.AutomationStartWork:
			start := *revision.Action.Work
			// Every occurrence owns its own fixed work deadline. The authored template
			// may predate this tick; response-loss retries must reuse the same deadline.
			start.Deadline = occurrence.Occurrence.EligibleAt.Add(revision.Policy.Deadline)
			if start.Deadline.After(occurrence.Occurrence.ExpiresAt) {
				start.Deadline = occurrence.Occurrence.ExpiresAt
			}
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
				send := SendMessageRequest{RequestContext: RequestContext{Principal: occurrence.Occurrence.Requester, RequestID: requestID}, Subject: "Message", To: model.MessageAudience{AgentIDs: []model.AgentID{recipients[i].AgentID}}, RecipientEligibility: &audience, Body: revision.Action.Message.Body}
				digest, digestErr := authoredMessageRequestDigest(send)
				if digestErr != nil {
					return touched, digestErr
				}
				if repeated, ok, repeatErr := s.store.MessageByRequest(ctx, send.Principal, requestID, digest); repeatErr != nil {
					return touched, fmt.Errorf("recover automation recipient %s: %w", recipients[i].AgentID, repeatErr)
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
				return touched, fmt.Errorf("record automation recipient dispositions: %w", err)
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

func (s *Service) prepareParkedOccurrence(ctx context.Context, current OccurrenceRecord, revision model.AutomationRuleRevision, now time.Time) (OccurrenceRecord, bool, error) {
	records, err := s.store.OccurrencesForRule(ctx, current.Occurrence.RuleID)
	if err != nil {
		return current, false, err
	}
	var active []OccurrenceRecord
	for _, candidate := range records {
		if candidate.Occurrence.ID != current.Occurrence.ID && occurrenceConsumesOverlapSlot(candidate.Occurrence.State) {
			active = append(active, candidate)
		}
	}
	if revision.Policy.Overlap == model.OverlapAllow {
		if uint32(len(active)) >= revision.Policy.MaxActive {
			return current, false, nil
		}
		updated, updateErr := s.store.UpdateOccurrence(ctx, current.Occurrence.ID, current.Occurrence.Revision, model.OccurrencePending, current.Occurrence.OperationID, current.Occurrence.WorkRunID, current.Occurrence.DeploymentID, current.Occurrence.Recipients, now)
		return updated, updateErr == nil, updateErr
	}
	if revision.Policy.Overlap != model.OverlapReplace {
		return current, false, nil
	}
	for _, candidate := range records {
		if candidate.Occurrence.ID == current.Occurrence.ID || candidate.Occurrence.State != model.OccurrenceParked {
			continue
		}
		candidateIsNewer := candidate.Occurrence.CreatedAt.After(current.Occurrence.CreatedAt) || candidate.Occurrence.CreatedAt.Equal(current.Occurrence.CreatedAt) && candidate.Occurrence.ID > current.Occurrence.ID
		if candidateIsNewer {
			updated, updateErr := s.store.UpdateOccurrence(ctx, current.Occurrence.ID, current.Occurrence.Revision, model.OccurrenceDenied, current.Occurrence.OperationID, current.Occurrence.WorkRunID, current.Occurrence.DeploymentID, deniedRecipients(current.Occurrence.Recipients, "superseded by "+string(candidate.Occurrence.ID)), now)
			return updated, false, updateErr
		}
		if _, updateErr := s.store.UpdateOccurrence(ctx, candidate.Occurrence.ID, candidate.Occurrence.Revision, model.OccurrenceDenied, candidate.Occurrence.OperationID, candidate.Occurrence.WorkRunID, candidate.Occurrence.DeploymentID, deniedRecipients(candidate.Occurrence.Recipients, "superseded by "+string(current.Occurrence.ID)), now); updateErr != nil && !errors.Is(updateErr, ErrConflict) {
			return current, false, updateErr
		}
	}
	for _, prior := range active {
		if prior.Occurrence.WorkRunID == "" {
			if prior.Occurrence.State == model.OccurrencePending || prior.Occurrence.State == model.OccurrenceParked {
				if _, err = s.store.UpdateOccurrence(ctx, prior.Occurrence.ID, prior.Occurrence.Revision, model.OccurrenceDenied, prior.Occurrence.OperationID, prior.Occurrence.WorkRunID, prior.Occurrence.DeploymentID, deniedRecipients(prior.Occurrence.Recipients, "replaced by "+string(current.Occurrence.ID)), now); err != nil && !errors.Is(err, ErrConflict) {
					return current, false, err
				}
			}
			continue
		}
		work, workErr := s.store.WorkRun(ctx, prior.Occurrence.WorkRunID)
		if workErr != nil {
			return current, false, workErr
		}
		if work.Run.State == model.WorkRunSucceeded || work.Run.State == model.WorkRunFailed || work.Run.State == model.WorkRunCancelled {
			continue
		}
		_, workErr = s.CancelWork(ctx, CancelWorkRequest{Context: RequestContext{Principal: current.Occurrence.Requester, RequestID: model.RequestID(deterministicOrchestrationID("request_", string(current.Occurrence.ID)+":replace:"+string(prior.Occurrence.ID)))}, WorkRunID: work.Run.ID, ExpectedRunRevision: work.Run.Revision, Reason: "replaced by occurrence " + string(current.Occurrence.ID)})
		if workErr != nil && !errors.Is(workErr, ErrConflict) {
			_, updateErr := s.store.UpdateOccurrence(ctx, current.Occurrence.ID, current.Occurrence.Revision, model.OccurrenceDenied, current.Occurrence.OperationID, current.Occurrence.WorkRunID, current.Occurrence.DeploymentID, deniedRecipients(current.Occurrence.Recipients, "replacement stop denied: "+workErr.Error()), now)
			return current, false, updateErr
		}
	}
	if len(active) != 0 {
		return current, false, nil
	}
	updated, err := s.store.UpdateOccurrence(ctx, current.Occurrence.ID, current.Occurrence.Revision, model.OccurrencePending, current.Occurrence.OperationID, current.Occurrence.WorkRunID, current.Occurrence.DeploymentID, current.Occurrence.Recipients, now)
	return updated, err == nil, err
}

func scheduleTicksAfter(revision model.AutomationRuleRevision, cursor, now time.Time, initial bool) (time.Time, uint64, error) {
	condition := revision.Condition.Schedule
	anchor := condition.Anchor.UTC()
	if anchor.IsZero() {
		anchor = revision.CreatedAt.UTC()
	}
	if now.Before(anchor) {
		return time.Time{}, 0, nil
	}
	if condition.Interval > 0 {
		first := anchor
		if cursor.After(anchor) || !initial {
			steps := cursor.Sub(anchor)/condition.Interval + 1
			first = anchor.Add(steps * condition.Interval)
		}
		if first.After(now) {
			return time.Time{}, 0, nil
		}
		count := uint64(now.Sub(first)/condition.Interval) + 1
		return first.Add(time.Duration(count-1) * condition.Interval), count, nil
	}
	location, err := time.LoadLocation(condition.Timezone)
	if err != nil {
		return time.Time{}, 0, fail(ErrInvalid, "schedule timezone is unavailable: %v", err)
	}
	schedule, err := cronv3.ParseStandard(condition.Cron)
	if err != nil {
		return time.Time{}, 0, fail(ErrInvalid, "cron schedule is invalid: %v", err)
	}
	cursor = cursor.In(location)
	var latest time.Time
	var count uint64
	// Standard cron cannot fire more than once per minute. Five leap-aware
	// years bounds cold-start catch-up while covering annual and Feb-29 rules.
	for range 5*366*24*60 + 1 {
		next := schedule.Next(cursor)
		if next.After(now.In(location)) {
			return latest.UTC(), count, nil
		}
		latest, cursor, count = next, next, count+1
	}
	return time.Time{}, 0, fail(ErrInvalid, "cron catch-up exceeds bounded five-year window")
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

func occurrenceConsumesOverlapSlot(state model.OccurrenceState) bool {
	switch state {
	case model.OccurrencePending, model.OccurrenceAdmitted, model.OccurrencePartial, model.OccurrenceUncertain:
		return true
	default:
		return false
	}
}

func deterministicOrchestrationID(prefix, source string) string {
	sum := sha256.Sum256([]byte(source))
	return prefix + hex.EncodeToString(sum[:12])
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

// deniedRecipients preserves already settled per-recipient effects during replacement.
func deniedRecipients(in []model.OccurrenceRecipient, detail string) []model.OccurrenceRecipient {
	out := append([]model.OccurrenceRecipient(nil), in...)
	for i := range out {
		if out[i].Disposition == model.RecipientPending {
			out[i].Disposition, out[i].Detail = model.RecipientDenied, detail
		}
	}
	return out
}
