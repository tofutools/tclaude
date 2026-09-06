package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
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
			ids := recipientIDs(occurrence.Occurrence.Recipients)
			_, sendErr := s.SendMessage(ctx, SendMessageRequest{RequestContext: RequestContext{Principal: occurrence.Occurrence.Requester, RequestID: occurrence.Occurrence.RequestID}, RecipientAgentIDs: ids, Body: revision.Action.Message.Body})
			state := model.OccurrenceDelivered
			recipients := deliveredRecipients(occurrence.Occurrence.Recipients)
			if sendErr != nil {
				state, recipients = model.OccurrenceDenied, deniedRecipients(occurrence.Occurrence.Recipients, sendErr.Error())
			}
			if _, err = s.store.UpdateOccurrence(ctx, occurrence.Occurrence.ID, occurrence.Occurrence.Revision, state, "", "", "", recipients, now); err != nil {
				return touched, err
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
	ids := append([]model.AgentID(nil), action.Message.AgentIDs...)
	if action.Message.GroupID != "" {
		group, err := s.store.Group(ctx, action.Message.GroupID)
		if err != nil {
			return nil, err
		}
		ids = append(ids, group.Members...)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	result := make([]model.OccurrenceRecipient, 0, len(ids))
	for i, id := range ids {
		if i > 0 && id == ids[i-1] {
			continue
		}
		result = append(result, model.OccurrenceRecipient{AgentID: id, Disposition: model.RecipientPending})
	}
	return result, nil
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

func recipientIDs(recipients []model.OccurrenceRecipient) []model.AgentID {
	ids := make([]model.AgentID, len(recipients))
	for i := range recipients {
		ids[i] = recipients[i].AgentID
	}
	return ids
}

func deliveredRecipients(in []model.OccurrenceRecipient) []model.OccurrenceRecipient {
	out := append([]model.OccurrenceRecipient(nil), in...)
	for i := range out {
		out[i].Disposition = model.RecipientDelivered
	}
	return out
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
