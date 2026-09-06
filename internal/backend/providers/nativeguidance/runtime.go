package nativeguidance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

const (
	MaxEventIDBytes     = 512
	MaxCorrelationBytes = 4096
	MaxPayloadBytes     = 1 << 20
	MaxGuidanceBytes    = 64 << 10
)

// Runtime owns the synchronous native-guidance effect sequence. It is bound
// to one exact execution attempt by its evaluator and evidence closure.
type Runtime struct {
	Evaluator   ports.NativeGuidanceEvaluator
	Evidence    func() (model.ProviderEvidence, error)
	Kinds       map[string]struct{}
	Correlation func(string) bool
	Now         func() time.Time
}

func (r Runtime) HandleNativeEvent(ctx context.Context, event ports.NormalizedNativeEvent, responder ports.NativeGuidanceResponder) (ports.NativeGuidanceSettlement, error) {
	if r.Evaluator == nil || responder == nil {
		return ports.NativeGuidanceSettlement{Disposition: ports.EffectUnsupported}, nil
	}
	if err := r.validate(event); err != nil {
		return ports.NativeGuidanceSettlement{Disposition: ports.EffectRefused}, err
	}
	admission, err := r.Evaluator.EvaluateNativeGuidance(ctx, event)
	if err != nil {
		return ports.NativeGuidanceSettlement{Disposition: ports.EffectUnknown}, fmt.Errorf("evaluate native guidance: %w", err)
	}
	if admission.IssuanceID == "" && admission.Permit == nil && strings.TrimSpace(admission.Guidance) == "" {
		return ports.NativeGuidanceSettlement{Disposition: ports.EffectRefused}, nil
	}
	if admission.Permit == nil || admission.IssuanceID == "" || admission.Permit.IssuanceID() != admission.IssuanceID ||
		admission.Permit.OperationID() == "" || strings.TrimSpace(admission.Guidance) == "" || len(admission.Guidance) > MaxGuidanceBytes {
		return ports.NativeGuidanceSettlement{IssuanceID: admission.IssuanceID, Disposition: ports.EffectRefused}, fmt.Errorf("native guidance admission is incomplete or invalid")
	}
	now := r.now()
	if admission.Deadline.IsZero() || !admission.Deadline.After(now) {
		return ports.NativeGuidanceSettlement{IssuanceID: admission.IssuanceID, Disposition: ports.EffectRefused}, fmt.Errorf("native guidance admission deadline has elapsed")
	}
	effectCtx, cancel := context.WithDeadline(ctx, admission.Deadline)
	defer cancel()
	if err := admission.Permit.Consume(effectCtx); err != nil {
		return ports.NativeGuidanceSettlement{IssuanceID: admission.IssuanceID, Disposition: ports.EffectRefused}, fmt.Errorf("consume native guidance permit: %w", err)
	}
	disposition, responseErr := responder.RespondNativeGuidance(effectCtx, admission.Guidance)
	if disposition == "" {
		disposition = ports.EffectUnknown
	}
	if responseErr != nil && disposition != ports.EffectUnknown {
		disposition = ports.EffectUnknown
	}
	settlement := ports.NativeGuidanceSettlement{IssuanceID: admission.IssuanceID, Disposition: disposition, SettledAt: r.now()}
	if r.Evidence == nil {
		responseErr = errors.Join(responseErr, fmt.Errorf("native guidance evidence is unavailable"))
		settlement.Disposition = ports.EffectUnknown
	} else {
		var evidenceErr error
		settlement.Evidence, evidenceErr = r.Evidence()
		if evidenceErr != nil {
			settlement.Disposition = ports.EffectUnknown
			responseErr = errors.Join(responseErr, fmt.Errorf("capture native guidance evidence: %w", evidenceErr))
		}
	}
	settleErr := r.Evaluator.SettleNativeGuidance(effectCtx, settlement)
	if settleErr != nil {
		settlement.Disposition = ports.EffectUnknown
	}
	return settlement, errors.Join(responseErr, settleErr)
}

func (r Runtime) validate(event ports.NormalizedNativeEvent) error {
	if _, ok := r.Kinds[event.Kind]; !ok || event.Timing != model.StandingOrderSameContinuation {
		return fmt.Errorf("unsupported native guidance event %q at timing %q", event.Kind, event.Timing)
	}
	if strings.TrimSpace(event.EventID) == "" || len(event.EventID) > MaxEventIDBytes ||
		strings.TrimSpace(event.NativeCorrelation) == "" || len(event.NativeCorrelation) > MaxCorrelationBytes ||
		len(event.Payload) == 0 || len(event.Payload) > MaxPayloadBytes || !json.Valid(event.Payload) ||
		event.ObservedAt.IsZero() {
		return fmt.Errorf("native guidance event is incomplete or exceeds bounds")
	}
	if r.Correlation == nil || !r.Correlation(event.NativeCorrelation) {
		return fmt.Errorf("native guidance event does not match the provider-native continuation")
	}
	return nil
}

func (r Runtime) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}
