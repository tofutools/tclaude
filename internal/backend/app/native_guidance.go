package app

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type nativeGuidanceEvaluator struct {
	service   *Service
	execution model.Execution
	mu        sync.Mutex
	issued    map[model.WorkIssuanceID]model.OperationID
}

func (s *Service) boundNativeGuidance(execution model.Execution) ports.NativeGuidanceEvaluator {
	if s.callbackIngress == nil {
		return nil
	}
	return &nativeGuidanceEvaluator{service: s, execution: execution, issued: make(map[model.WorkIssuanceID]model.OperationID)}
}

func (s *Service) requireNativeGuidanceComposition(ctx context.Context, execution model.Execution) error {
	if s.callbackIngress != nil || execution.AgentID == "" {
		return nil
	}
	rules, err := s.store.ListAutomationRules(ctx, false)
	if err != nil {
		return err
	}
	evaluator := &nativeGuidanceEvaluator{service: s, execution: execution}
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		revision, readErr := s.store.AutomationRuleRevision(ctx, rule.HeadRevisionID)
		if readErr != nil {
			return readErr
		}
		if revision.Condition.Kind != model.AutomationStandingOrder || revision.Action.Kind != model.AutomationSendMessage || revision.Action.Message == nil {
			continue
		}
		eligible, eligibilityErr := evaluator.targetsExecution(ctx, *revision.Action.Message)
		if eligibilityErr != nil {
			return eligibilityErr
		}
		if eligible {
			return fail(ErrUnavailable, "native callback ingress is required for targeted standing guidance")
		}
	}
	return nil
}

func (e *nativeGuidanceEvaluator) EvaluateNativeGuidance(ctx context.Context, event ports.NormalizedNativeEvent) (ports.NativeGuidanceAdmission, error) {
	if strings.TrimSpace(event.EventID) == "" || strings.TrimSpace(event.Kind) == "" || event.ObservedAt.IsZero() {
		return ports.NativeGuidanceAdmission{}, fail(ErrInvalid, "native event identity, kind and observation time are required")
	}
	rules, err := e.service.store.ListAutomationRules(ctx, false)
	if err != nil {
		return ports.NativeGuidanceAdmission{}, err
	}
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		revision, readErr := e.service.store.AutomationRuleRevision(ctx, rule.HeadRevisionID)
		if readErr != nil {
			return ports.NativeGuidanceAdmission{}, readErr
		}
		condition := revision.Condition.StandingOrder
		if revision.Condition.Kind != model.AutomationStandingOrder || condition == nil || condition.FactKind != event.Kind || condition.Timing != event.Timing || revision.Action.Kind != model.AutomationSendMessage || revision.Action.Message == nil {
			continue
		}
		matched, matchErr := regexp.MatchString(condition.Pattern, string(event.Payload))
		if matchErr != nil || !matched {
			continue
		}
		key := string(revision.ID) + ":" + string(e.execution.ID) + ":" + event.EventID + ":" + event.NativeCorrelation
		issuanceID := model.WorkIssuanceID(deterministicOrchestrationID("issuance_", key))
		operationID := model.OperationID(deterministicOrchestrationID("op_", key))
		eligible := true
		if _, priorErr := e.service.store.OperationResult(ctx, operationID); errors.Is(priorErr, ErrNotFound) {
			var eligibilityErr error
			eligible, eligibilityErr = e.targetsExecution(ctx, *revision.Action.Message)
			if eligibilityErr != nil {
				return ports.NativeGuidanceAdmission{}, eligibilityErr
			}
			if !eligible {
				continue
			}
		} else if priorErr != nil {
			return ports.NativeGuidanceAdmission{}, priorErr
		}
		deadline := event.ObservedAt.Add(condition.DispatchDeadline)
		if !deadline.After(e.service.now().UTC()) {
			return ports.NativeGuidanceAdmission{}, fail(ErrConflict, "native guidance deadline elapsed")
		}
		principal := model.AutomationPrincipal(string(issuanceID), revision.Owner, revision.Delegation)
		now := e.service.now().UTC()
		operation := model.Operation{ID: operationID, RequestID: model.RequestID(deterministicOrchestrationID("request_", key)), Kind: model.OperationInteract, Principal: principal, ExecutionID: e.execution.ID, State: model.OperationAdmitted, Revision: 1, CreatedAt: now, UpdatedAt: now}
		authority := model.AuthorityRequest{Principal: principal, Action: model.ActionInteract, Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: e.execution.AgentID}}
		audience := automationMessageAudience(*revision.Action.Message)
		admitted, admitErr := e.service.store.AdmitExecutionOperation(ctx, ExecutionOperationAdmission{Operation: operation, Authority: authority, Eligibility: &audience})
		if admitErr != nil {
			if !eligible && errors.Is(admitErr, ErrConflict) {
				continue
			}
			return ports.NativeGuidanceAdmission{}, admitErr
		}
		if admitted.Repeated && admitted.Operation.State != model.OperationAdmitted {
			return ports.NativeGuidanceAdmission{}, fail(ErrConflict, "native guidance issuance is already settled")
		}
		e.mu.Lock()
		e.issued[issuanceID] = operationID
		e.mu.Unlock()
		return ports.NativeGuidanceAdmission{IssuanceID: issuanceID, Guidance: revision.Action.Message.Body, Deadline: deadline, Permit: &nativeGuidancePermit{store: e.service.store, issuanceID: issuanceID, operationID: operationID, now: e.service.now}}, nil
	}
	return ports.NativeGuidanceAdmission{}, fail(ErrNotFound, "no standing order matched native event")
}

func (e *nativeGuidanceEvaluator) targetsExecution(ctx context.Context, action model.AutomationMessageAction) (bool, error) {
	ids, err := e.service.store.ResolveMessageAudience(ctx, automationMessageAudience(action))
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		if id == e.execution.AgentID {
			return true, nil
		}
	}
	return false, nil
}

func (e *nativeGuidanceEvaluator) SettleNativeGuidance(ctx context.Context, settlement ports.NativeGuidanceSettlement) error {
	e.mu.Lock()
	operationID, ok := e.issued[settlement.IssuanceID]
	if ok {
		delete(e.issued, settlement.IssuanceID)
	}
	e.mu.Unlock()
	if !ok {
		return ErrConflict
	}
	operationState := model.OperationFailed
	switch settlement.Disposition {
	case ports.EffectAccepted:
		operationState = model.OperationSucceeded
	case ports.EffectRefused, ports.EffectUnsupported:
		operationState = model.OperationRefused
	case ports.EffectUnknown:
		operationState = model.OperationUncertain
	}
	_, err := e.service.store.CompleteOperation(context.WithoutCancel(ctx), OperationCompletion{OperationID: operationID, OperationState: operationState, ResultCode: "native_guidance", ExecutionID: e.execution.ID, Evidence: settlement.Evidence, At: settlement.SettledAt})
	return err
}

type nativeGuidancePermit struct {
	store       Store
	issuanceID  model.WorkIssuanceID
	operationID model.OperationID
	now         func() time.Time
	mu          sync.Mutex
	consumed    bool
}

func (p *nativeGuidancePermit) IssuanceID() model.WorkIssuanceID { return p.issuanceID }
func (p *nativeGuidancePermit) OperationID() model.OperationID   { return p.operationID }
func (p *nativeGuidancePermit) Consume(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.consumed {
		return ErrConflict
	}
	if err := p.store.ConsumeExecutionEffect(ctx, p.operationID, p.now().UTC()); err != nil {
		return err
	}
	p.consumed = true
	return nil
}
