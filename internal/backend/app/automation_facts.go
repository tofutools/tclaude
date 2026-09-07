package app

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

const (
	maxAutomationCausalDepth  = 8
	maxAutomationCollectPages = 4
	automationCollectPageSize = 64
)

// IngestTrustedAutomationFacts is intentionally not part of the public API.
// Only composition-owned read-only collectors receive this narrow interface.
func (s *Service) IngestTrustedAutomationFacts(ctx context.Context, source string, facts []model.NormalizedFact) error {
	if source == "" || source == model.AutomationSourceApplication || len(facts) > 256 {
		return ErrInvalid
	}
	normalized := make([]model.NormalizedFact, len(facts))
	for i, fact := range facts {
		fact.Source = source
		if err := validateNormalizedAutomationFact(fact, false); err != nil {
			return err
		}
		fact.Sequence, fact.Cursor, fact.ExecutionID, fact.ExecutionRevision, fact.ContextRevision = 0, "", "", 0, 0
		fact.ResourceRefs, fact.Payload = nil, nil
		normalized[i] = fact
	}
	return s.store.AppendAutomationFacts(ctx, source, normalized)
}

// ReportAutomationSourceFailure is a composition-only negative-evidence seam.
// It clears an in-progress dwell/debounce episode for the exact configured
// source and target without fabricating a positive fact or changing cooldown.
func (s *Service) ReportAutomationSourceFailure(ctx context.Context, source string, resource model.AutomationFactResource, observedAt time.Time) error {
	if source == "" || source == model.AutomationSourceApplication || observedAt.IsZero() || observedAt.After(s.now().UTC().Add(time.Minute)) {
		return ErrInvalid
	}
	if err := validateAutomationFactResource(resource); err != nil {
		return err
	}
	return s.resetAutomationTriggerEpisodes(ctx, source, resource, observedAt.UTC())
}

func (s *Service) resetAutomationTriggerEpisodes(ctx context.Context, source string, resource model.AutomationFactResource, observedAt time.Time) error {
	rules, err := s.store.ListAutomationRules(ctx, false)
	if err != nil {
		return err
	}
	var resetErrors []error
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		revision, readErr := s.store.AutomationRuleRevision(ctx, rule.HeadRevisionID)
		if readErr != nil {
			resetErrors = append(resetErrors, readErr)
			continue
		}
		condition := revision.Condition.Trigger
		if revision.Condition.Kind != model.AutomationTrigger || condition == nil || condition.SourceID != source || condition.Resource != resource {
			continue
		}
		state, stateErr := s.store.AutomationConditionState(ctx, rule.ID)
		if stateErr != nil {
			resetErrors = append(resetErrors, stateErr)
			continue
		}
		expected := state.Revision
		state.DwellEpisodeID = ""
		state.DwellSince, state.DebounceAt, state.DebouncePayload = nil, nil, nil
		state.ObservedAt = observedAt
		if _, _, advanceErr := s.store.AdvanceTrigger(ctx, state, nil, rule.Revision, expected); advanceErr != nil {
			resetErrors = append(resetErrors, advanceErr)
		}
	}
	return errors.Join(resetErrors...)
}

func (s *Service) collectAutomationFacts(ctx context.Context) error {
	if len(s.automationFacts) == 0 {
		return nil
	}
	sources := make(map[string]ports.AutomationFactSource, len(s.automationFacts))
	for _, source := range s.automationFacts {
		if source != nil && strings.TrimSpace(source.SourceID()) != "" {
			if _, exists := sources[source.SourceID()]; !exists {
				sources[source.SourceID()] = source
			}
		}
	}
	rules, err := s.store.ListAutomationRules(ctx, false)
	if err != nil {
		return err
	}
	type target struct {
		source   string
		resource model.AutomationFactResource
	}
	targets := make(map[target]struct{})
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		revision, readErr := s.store.AutomationRuleRevision(ctx, rule.HeadRevisionID)
		if readErr != nil {
			return readErr
		}
		condition := revision.Condition.Trigger
		if revision.Condition.Kind == model.AutomationTrigger && condition != nil && condition.SourceID != model.AutomationSourceApplication {
			targets[target{source: condition.SourceID, resource: condition.Resource}] = struct{}{}
		}
	}
	var collectionErrors []error
	for configured := range targets {
		source := sources[configured.source]
		if source == nil {
			if resetErr := s.resetAutomationTriggerEpisodes(ctx, configured.source, configured.resource, s.now().UTC()); resetErr != nil {
				collectionErrors = append(collectionErrors, resetErr)
			}
			continue
		}
		cursor := ""
		seenCursor := map[string]bool{"": true}
		collected := false
		failed := false
		complete := false
		for page := 0; page < maxAutomationCollectPages; page++ {
			batch, collectErr := source.CollectAutomationFacts(ctx, ports.AutomationFactCollectRequest{Resource: configured.resource, Cursor: cursor, Limit: automationCollectPageSize, Now: s.now().UTC()})
			if collectErr != nil {
				failed = true
				break
			}
			if len(batch.Facts) > automationCollectPageSize {
				collectionErrors = append(collectionErrors, fail(ErrInvalid, "automation fact source %s exceeded its bounded page", configured.source))
				failed = true
				break
			}
			for _, fact := range batch.Facts {
				if fact.Resource != configured.resource {
					collectionErrors = append(collectionErrors, fail(ErrInvalid, "automation fact source %s returned an unconfigured resource", configured.source))
					failed = true
					break
				}
			}
			if failed {
				break
			}
			if len(batch.Facts) > 0 {
				collected = true
				if ingestErr := s.IngestTrustedAutomationFacts(ctx, configured.source, batch.Facts); ingestErr != nil {
					collectionErrors = append(collectionErrors, ingestErr)
					failed = true
					break
				}
			}
			if batch.NextCursor == "" {
				complete = true
				break
			}
			if seenCursor[batch.NextCursor] {
				collectionErrors = append(collectionErrors, fail(ErrInvalid, "automation fact source %s repeated its cursor", configured.source))
				failed = true
				break
			}
			seenCursor[batch.NextCursor] = true
			cursor = batch.NextCursor
		}
		if !complete && !failed {
			collectionErrors = append(collectionErrors, fail(ErrConflict, "automation fact source %s exceeded bounded catch-up", configured.source))
			failed = true
		}
		if failed || !collected {
			if resetErr := s.resetAutomationTriggerEpisodes(ctx, configured.source, configured.resource, s.now().UTC()); resetErr != nil {
				collectionErrors = append(collectionErrors, resetErr)
			}
		}
	}
	return errors.Join(collectionErrors...)
}

func validateNormalizedAutomationFact(fact model.NormalizedFact, application bool) error {
	if strings.TrimSpace(fact.Source) == "" || strings.TrimSpace(fact.EventID) == "" || fact.OccurredAt.IsZero() || fact.ObservedAt.IsZero() || fact.ObservedAt.Before(fact.OccurredAt) || fact.CausalDepth > maxAutomationCausalDepth {
		return fail(ErrInvalid, "automation fact identity, causal depth and timestamps are invalid")
	}
	if !application && (fact.ParentOccurrenceID != "" || fact.CausalDepth != 0) {
		return fail(ErrInvalid, "external automation facts must be causal roots")
	}
	if err := validateAutomationFactResource(fact.Resource); err != nil {
		return err
	}
	allowed := false
	if application {
		switch fact.Kind {
		case model.FactOperationSucceeded:
			allowed = fact.Resource.Kind == model.FactResourceOperation && fact.Value == "succeeded"
		case model.FactOperationFailed:
			allowed = fact.Resource.Kind == model.FactResourceOperation && fact.Value == "failed"
		case model.FactMessageDelivered:
			allowed = fact.Resource.Kind == model.FactResourceMessage && fact.Value == "delivered"
		case model.FactMessageDenied:
			allowed = fact.Resource.Kind == model.FactResourceMessage && fact.Value == "denied"
		case model.FactWorkSucceeded:
			allowed = fact.Resource.Kind == model.FactResourceWork && fact.Value == "succeeded"
		case model.FactWorkFailed:
			allowed = fact.Resource.Kind == model.FactResourceWork && fact.Value == "failed"
		case model.FactWorkCancelled:
			allowed = fact.Resource.Kind == model.FactResourceWork && fact.Value == "cancelled"
		case model.FactAgentAwaitingInput:
			allowed = fact.Resource.Kind == model.FactResourceAgent && (fact.Value == "true" || fact.Value == "false" || fact.Value == "unknown")
		case model.FactAgentIdle:
			allowed = fact.Resource.Kind == model.FactResourceAgent && (fact.Value == "true" || fact.Value == "false" || fact.Value == "unknown")
		}
	} else if fact.Resource.Kind == model.FactResourceRepositoryPullReq {
		switch fact.Kind {
		case model.FactPullRequestChanged:
			allowed = containsString([]string{"open", "draft", "closed", "merged"}, fact.Value)
		case model.FactCICompleted:
			allowed = containsString([]string{"succeeded", "failed", "pending", "unknown"}, fact.Value)
		}
	}
	if !allowed {
		return fail(ErrInvalid, "automation fact kind, value and resource do not form a supported fact")
	}
	return nil
}

func validateAutomationFactResource(resource model.AutomationFactResource) error {
	switch resource.Kind {
	case model.FactResourceRepositoryPullReq:
		if strings.TrimSpace(resource.Repository) == "" || resource.PullRequest == 0 || resource.ID != "" {
			return fail(ErrInvalid, "repository fact requires exact repository and pull request")
		}
	case model.FactResourceOperation, model.FactResourceMessage, model.FactResourceWork, model.FactResourceAgent:
		if strings.TrimSpace(resource.ID) == "" || resource.Repository != "" || resource.PullRequest != 0 {
			return fail(ErrInvalid, "application fact requires one exact resource id")
		}
	default:
		return fail(ErrInvalid, "automation fact resource kind is unsupported")
	}
	return nil
}

func internalAutomationFact(kind, value string, resource model.AutomationFactResource, eventID string, occurred time.Time, parent model.OccurrenceID, depth uint32) model.NormalizedFact {
	return model.NormalizedFact{Source: model.AutomationSourceApplication, EventID: eventID, Kind: kind, Value: value, Resource: resource, OccurredAt: occurred, ObservedAt: occurred, ParentOccurrenceID: parent, CausalDepth: depth}
}

func (s *Service) reconcileTriggerRule(ctx context.Context, rule model.AutomationRule, revision model.AutomationRuleRevision, now time.Time) ([]model.OccurrenceID, error) {
	condition := revision.Condition.Trigger
	state, err := s.store.AutomationConditionState(ctx, rule.ID)
	if err != nil {
		return nil, err
	}
	after, _ := strconv.ParseUint(state.SourceCursor, 10, 64)
	facts, err := s.store.AutomationFactsAfter(ctx, condition.SourceID, condition.Resource, after, 256)
	if err != nil {
		return nil, err
	}
	var touched []model.OccurrenceID
	for _, fact := range facts {
		expected := state.Revision
		state.SourceCursor = strconv.FormatUint(fact.Sequence, 10)
		matchingKind := fact.Kind == condition.FactKind
		matchingValue := containsString(condition.Values, fact.Value)
		fresh := !fact.ObservedAt.After(now.Add(time.Minute)) && now.Sub(fact.ObservedAt.UTC()) <= condition.Freshness
		if matchingKind {
			state.ObservedAt = fact.ObservedAt.UTC()
		}
		if matchingKind && (!matchingValue || !fresh) {
			state.DwellSince, state.DebounceAt, state.DebouncePayload = nil, nil, nil
			state.DwellEpisodeID = ""
		} else if matchingKind && matchingValue && (state.CooldownUntil == nil || !fact.ObservedAt.Before(*state.CooldownUntil)) {
			if condition.Dwell > 0 {
				if state.DwellEpisodeID == "" {
					start := fact.ObservedAt.UTC()
					state.DwellEpisodeID, state.DwellSince = fact.EventID+":"+start.Format(time.RFC3339Nano), &start
				}
				if state.DwellSince != nil {
					fireAt := state.DwellSince.Add(condition.Dwell)
					if trailing := fact.ObservedAt.Add(condition.Debounce); trailing.After(fireAt) {
						fireAt = trailing
					}
					state.DebounceAt = &fireAt
					state.DebouncePayload, _ = json.Marshal(fact)
				}
			} else {
				fireAt := fact.ObservedAt.Add(condition.Debounce)
				state.DebounceAt = &fireAt
				state.DebouncePayload, _ = json.Marshal(fact)
			}
		}
		occurrence, dueErr := s.dueTriggerOccurrence(ctx, rule, revision, &state, now)
		if dueErr != nil {
			return touched, dueErr
		}
		created, repeated, advanceErr := s.store.AdvanceTrigger(ctx, state, occurrence, rule.Revision, expected)
		if advanceErr != nil {
			return touched, advanceErr
		}
		state.Revision = expected + 1
		if occurrence != nil && !repeated {
			touched = append(touched, created.Occurrence.ID)
		}
	}
	if len(facts) == 0 && state.DebounceAt != nil {
		expected := state.Revision
		occurrence, dueErr := s.dueTriggerOccurrence(ctx, rule, revision, &state, now)
		if dueErr != nil {
			return touched, dueErr
		}
		if occurrence != nil || state.DebounceAt == nil {
			created, repeated, advanceErr := s.store.AdvanceTrigger(ctx, state, occurrence, rule.Revision, expected)
			if advanceErr != nil {
				return touched, advanceErr
			}
			if occurrence != nil && !repeated {
				touched = append(touched, created.Occurrence.ID)
			}
		}
	}
	return touched, nil
}

func (s *Service) dueTriggerOccurrence(ctx context.Context, rule model.AutomationRule, revision model.AutomationRuleRevision, state *model.AutomationConditionState, now time.Time) (*model.AutomationOccurrence, error) {
	if state.DebounceAt == nil || state.DebounceAt.After(now) || len(state.DebouncePayload) == 0 {
		return nil, nil
	}
	condition := revision.Condition.Trigger
	if state.ObservedAt.IsZero() || now.Sub(state.ObservedAt) > condition.Freshness {
		state.DwellSince, state.DebounceAt, state.DebouncePayload = nil, nil, nil
		state.DwellEpisodeID = ""
		return nil, nil
	}
	var fact model.NormalizedFact
	if err := json.Unmarshal(state.DebouncePayload, &fact); err != nil {
		return nil, fail(ErrInvalid, "decode pending trigger fact: %v", err)
	}
	keyID := fact.EventID
	if condition.Dwell > 0 {
		keyID = state.DwellEpisodeID
	}
	key := "trigger:" + fact.Source + ":" + keyID
	id := model.OccurrenceID(deterministicOrchestrationID("occurrence_", string(revision.ID)+":"+key))
	requestID := model.RequestID(deterministicOrchestrationID("request_", string(revision.ID)+":"+key))
	recipients, err := s.automationRecipients(ctx, revision.Action)
	if err != nil {
		return nil, err
	}
	occurrenceState := model.OccurrencePending
	if fact.CausalDepth > maxAutomationCausalDepth {
		occurrenceState = model.OccurrenceDenied
		for i := range recipients {
			recipients[i].Disposition, recipients[i].Detail = model.RecipientDenied, "automation causal depth exceeded"
		}
	}
	occurrence := &model.AutomationOccurrence{ID: id, RuleID: rule.ID, RuleRevisionID: revision.ID, SourceOccurrenceKey: key, RequestID: requestID, Requester: model.AutomationPrincipal(string(id), revision.Owner, revision.Delegation), ParentOccurrenceID: fact.ParentOccurrenceID, CausalDepth: fact.CausalDepth, EventAt: fact.OccurredAt.UTC(), EligibleAt: now, ExpiresAt: now.Add(revision.Policy.ExpiresAfter), State: occurrenceState, Recipients: recipients, Revision: 1, CreatedAt: now, UpdatedAt: now}
	state.DebounceAt, state.DebouncePayload = nil, nil
	if condition.Dwell > 0 {
		// A continuous true observation fires once. The episode identifier is
		// retained until negative/stale evidence resets it; nil DwellSince marks
		// the current episode as already emitted.
		state.DwellSince = nil
	}
	cooldown := now.Add(condition.Cooldown)
	state.CooldownUntil = &cooldown
	return occurrence, nil
}
