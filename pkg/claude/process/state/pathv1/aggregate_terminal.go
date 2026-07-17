package pathv1

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
)

// settleAttemptObservationPayload is the narrow typed observation envelope
// needed by the dormant aggregate validator. The later mutation layer owns
// replacing this envelope with its richer typed plan before path-v1 enablement.
type settleAttemptObservationPayload struct {
	TemplateRef        string       `json:"templateRef"`
	SourceCommandID    string       `json:"sourceCommandId"`
	SourceActivationID ActivationID `json:"sourceActivationId"`
	SourceGeneration   uint64       `json:"sourceGeneration"`
	Attempt            uint64       `json:"attempt"`
	ResultCode         string       `json:"resultCode"`
	ReasonCode         string       `json:"reasonCode,omitempty"`
	Actor              string       `json:"actor,omitempty"`
	EvidenceRef        string       `json:"evidenceRef,omitempty"`
	EvidenceHash       string       `json:"evidenceHash,omitempty"`
	ResolutionDigest   string       `json:"resolutionDigest,omitempty"`
	ExternalRef        string       `json:"externalRef,omitempty"`
	Feedback           string       `json:"feedback,omitempty"`
}

type routeTerminalPayload struct {
	TemplateRef         string       `json:"templateRef"`
	SettlementCommandID string       `json:"settlementCommandId"`
	SourceActivationID  ActivationID `json:"sourceActivationId"`
	SourceGeneration    uint64       `json:"sourceGeneration"`
	SourcePathID        PathID       `json:"sourcePathId"`
	Attempt             uint64       `json:"attempt"`
	ResultCode          string       `json:"resultCode"`
	ReasonCode          string       `json:"reasonCode"`
	ProducedPathIDs     []PathID     `json:"producedPathIds"`
}

func decodeExactPayload[T any](payload []byte, value *T) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if !bytes.Equal(payload, canonical) {
		return fmt.Errorf("payload is not canonical typed JSON")
	}
	return nil
}

func payloadDigest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func (i *aggregateIndex) validateSettleAttemptTerminal(path string, p PathRecord, d DispositionReceipt, settle CommandRecord) {
	id := settle.Identity
	fail := func(format string, args ...any) {
		i.c.add("terminal_command_provenance", path+".disposition", format, args...)
	}
	if id.Attempt == 0 {
		fail("settlement attempt must be positive")
		return
	}
	source, ok := i.view.Commands[id.InputDigest]
	if !ok || source.Identity.Kind != CommandPerformAttempt || source.ID != id.InputDigest || source.Identity.RunID != id.RunID || source.Identity.SourceActivationID != id.SourceActivationID || source.Identity.SourceGeneration != id.SourceGeneration || source.Identity.Attempt != id.Attempt || (source.State != CommandObserved && source.State != CommandReconciled) {
		fail("settlement input does not name its exact settled perform-attempt command")
	}
	for otherID, other := range i.view.Commands {
		if otherID == source.ID || other.Identity.Kind != CommandPerformAttempt {
			continue
		}
		if other.Identity.RunID == id.RunID && other.Identity.SourceActivationID == id.SourceActivationID && other.Identity.SourceGeneration == id.SourceGeneration && other.Identity.Attempt == id.Attempt {
			fail("performer attempt has multiple source commands")
			break
		}
	}
	for otherID, other := range i.view.Commands {
		if otherID == settle.ID || other.Identity.Kind != CommandSettleAttempt {
			continue
		}
		otherIdentity := other.Identity
		if otherIdentity.RunID == id.RunID && otherIdentity.SourceActivationID == id.SourceActivationID && otherIdentity.SourceGeneration == id.SourceGeneration && (otherIdentity.Attempt == id.Attempt || otherIdentity.InputDigest == id.InputDigest) {
			fail("performer attempt has multiple settlement observations")
			break
		}
	}
	attemptID, err := AttemptIdentity(i.view.RunID, id.SourceActivationID, id.Attempt)
	effect, effectOK := i.view.SideEffects[attemptID]
	wantEffect := map[PathState]string{PathFailed: "failed", PathCanceled: "canceled", PathSkipped: "observed", PathEnded: "observed"}[p.State]
	if err != nil || !effectOK || effect.Kind != SideEffectAttempt || effect.ActivationID != id.SourceActivationID || effect.Attempt != id.Attempt || effect.State != wantEffect {
		fail("settlement lacks its exact terminal attempt lifecycle evidence")
	}
	for _, other := range i.view.SideEffects {
		if other.Kind == SideEffectAttempt && other.ActivationID == id.SourceActivationID && other.Attempt > id.Attempt {
			fail("settlement replays attempt %d after later attempt %d", id.Attempt, other.Attempt)
			break
		}
	}
	wantResult := string(p.State)
	if p.State == PathEnded {
		wantResult = "pass"
	}
	if id.ResultCode != wantResult {
		fail("settlement result %q cannot own terminal state %q", id.ResultCode, p.State)
	}
	var payload settleAttemptObservationPayload
	if err := decodeExactPayload(settle.Payload, &payload); err != nil {
		fail("settlement observation payload is invalid: %v", err)
		return
	}
	wantPayload := settleAttemptObservationPayload{TemplateRef: i.view.TemplateRef, SourceCommandID: source.ID, SourceActivationID: id.SourceActivationID, SourceGeneration: id.SourceGeneration, Attempt: id.Attempt, ResultCode: id.ResultCode}
	if p.State != PathEnded {
		wantPayload.ReasonCode = d.ReasonCode
	}
	if payload.TemplateRef != wantPayload.TemplateRef || payload.SourceCommandID != wantPayload.SourceCommandID ||
		payload.SourceActivationID != wantPayload.SourceActivationID || payload.SourceGeneration != wantPayload.SourceGeneration ||
		payload.Attempt != wantPayload.Attempt || payload.ResultCode != wantPayload.ResultCode || payload.ReasonCode != wantPayload.ReasonCode ||
		id.PlanDigest != payloadDigest(settle.Payload) || id.PlanDigest != settle.PayloadHash {
		fail("settlement observation digest does not bind the exact source, attempt, result, and reason")
	}
}

func (i *aggregateIndex) validateRouteTerminal(path string, p PathRecord, d DispositionReceipt, route CommandRecord) {
	id := route.Identity
	fail := func(format string, args ...any) {
		i.c.add("terminal_command_provenance", path+".disposition", format, args...)
	}
	settle, ok := i.view.Commands[id.InputDigest]
	if !ok || settle.Identity.Kind != CommandSettleAttempt || settle.ID != id.InputDigest || settle.Identity.SourceActivationID != id.SourceActivationID || settle.Identity.SourceGeneration != id.SourceGeneration || settle.Identity.Attempt != id.Attempt {
		fail("terminal route input does not name its exact settlement command")
		return
	}
	i.validateSettleAttemptTerminal(path, p, d, settle)
	outcome, exact := exactSettlementResult(id.ResultCode, false)
	if !exact || outcome != settle.Identity.ResultCode {
		fail("terminal route result %q does not conserve settlement result %q", id.ResultCode, settle.Identity.ResultCode)
	}
	payload, err := i.decodeRouteTerminalPayload(p, route, settle.Identity.ResultCode)
	if err != nil {
		fail("terminal route payload is invalid: %v", err)
		return
	}
	want := routeTerminalPayload{TemplateRef: i.view.TemplateRef, SettlementCommandID: settle.ID, SourceActivationID: id.SourceActivationID, SourceGeneration: id.SourceGeneration, SourcePathID: p.ID, Attempt: id.Attempt, ResultCode: id.ResultCode, ReasonCode: d.ReasonCode, ProducedPathIDs: append([]PathID(nil), p.ProducedPathIDs...)}
	if payload.TemplateRef != want.TemplateRef || payload.SettlementCommandID != want.SettlementCommandID || payload.SourceActivationID != want.SourceActivationID || payload.SourceGeneration != want.SourceGeneration || payload.SourcePathID != want.SourcePathID || payload.Attempt != want.Attempt || payload.ResultCode != want.ResultCode || payload.ReasonCode != want.ReasonCode || !slices.Equal(payload.ProducedPathIDs, want.ProducedPathIDs) || id.PlanDigest != payloadDigest(route.Payload) || id.PlanDigest != route.PayloadHash {
		fail("terminal route plan digest does not bind the exact settlement and terminal transition")
	}
}

func (i *aggregateIndex) decodeRouteTerminalPayload(current PathRecord, route CommandRecord, settlementResult string) (routeTerminalPayload, error) {
	var envelope mutationPayload[RoutePathsPlan]
	typedErr := decodeExactPayload(route.Payload, &envelope)
	if typedErr == nil {
		plan := envelope.Plan
		if envelope.TemplateRef != i.view.TemplateRef || envelope.TemplateSourceHash != i.view.TemplateSourceHash {
			return routeTerminalPayload{}, fmt.Errorf("typed envelope template binding mismatch")
		}
		if err := envelope.Checkpoint.Validate(); err != nil {
			return routeTerminalPayload{}, fmt.Errorf("typed envelope checkpoint binding: %w", err)
		}
		if err := plan.Validate(); err != nil {
			return routeTerminalPayload{}, fmt.Errorf("typed route plan: %w", err)
		}
		id := route.Identity
		if plan.SettlementCommandID != id.InputDigest || plan.SourceActivationID != id.SourceActivationID || plan.SourceGeneration != id.SourceGeneration || plan.SourcePathID != id.SourcePathID || plan.Attempt != id.Attempt || plan.CauseDigest != id.CauseDigest || plan.ResultCode != id.ResultCode {
			return routeTerminalPayload{}, fmt.Errorf("typed route plan differs from command identity")
		}
		if err := validateHistoricalMutationPost(i.view.Routing, plan.Batch, route.ID); err != nil {
			return routeTerminalPayload{}, fmt.Errorf("typed route plan does not prove the complete durable transition: %w", err)
		}
		materialized, err := plan.Batch.materialize(route.ID)
		if err != nil {
			return routeTerminalPayload{}, fmt.Errorf("typed route plan materialization: %w", err)
		}
		mutation, ok := findMutation(MutationBatch{Mutations: materialized}, MutationPath, plan.SourcePathID)
		if !ok || len(mutation.Before) == 0 || len(mutation.After) == 0 {
			return routeTerminalPayload{}, fmt.Errorf("typed route plan lacks exact source transition")
		}
		var after PathRecord
		if err := decodeExactPayload(mutation.After, &after); err != nil {
			return routeTerminalPayload{}, fmt.Errorf("typed route source transition: %w", err)
		}
		if !canonicalEqual(after, current) || after.Disposition == nil {
			return routeTerminalPayload{}, fmt.Errorf("typed route source transition differs from durable terminal path")
		}
		pre, err := historicalMutationPre(i.view.Routing, plan.Batch, route.ID)
		if err != nil {
			return routeTerminalPayload{}, fmt.Errorf("typed route plan historical pre-state: %w", err)
		}
		if err := validateRouteTransitionAuthority(pre, plan, settlementResult); err != nil {
			return routeTerminalPayload{}, fmt.Errorf("typed route plan transition authority: %w", err)
		}
		return routeTerminalPayload{
			TemplateRef:         envelope.TemplateRef,
			SettlementCommandID: plan.SettlementCommandID,
			SourceActivationID:  plan.SourceActivationID,
			SourceGeneration:    plan.SourceGeneration,
			SourcePathID:        plan.SourcePathID,
			Attempt:             plan.Attempt,
			ResultCode:          plan.ResultCode,
			ReasonCode:          after.Disposition.ReasonCode,
			ProducedPathIDs:     append([]PathID(nil), plan.ProducedPathIDs...),
		}, nil
	}

	var legacy routeTerminalPayload
	if legacyErr := decodeExactPayload(route.Payload, &legacy); legacyErr == nil {
		return legacy, nil
	} else {
		return routeTerminalPayload{}, fmt.Errorf("neither strict typed envelope (%v) nor strict legacy envelope (%v)", typedErr, legacyErr)
	}
}

// historicalMutationPre reconstructs the route-time pre-state for the exact
// records owned by a durable batch while preserving unrelated records that
// may have advanced later. Materialized keys are reversed because command
// ownership can transitively change create-only cause, cause-set, and path
// identities. The result is used only by the canonical route mutation-set
// authority validator; whole-state replay classification remains the live
// replay path's responsibility.
func historicalMutationPre(current *RoutingState, batch MutationBatch, commandID string) (RoutingState, error) {
	if current == nil {
		return RoutingState{}, fmt.Errorf("%w: routing state is absent", ErrMutationInconsistent)
	}
	materialized, err := batch.materialize(commandID)
	if err != nil {
		return RoutingState{}, err
	}
	pre := Clone(*current)
	for index := len(materialized) - 1; index >= 0; index-- {
		mutation := materialized[index]
		reverse := RecordMutation{Kind: mutation.Kind, Key: mutation.Key, After: mutation.Before}
		if err := applyRecordMutation(&pre, reverse); err != nil {
			return RoutingState{}, err
		}
	}
	return pre, nil
}

// validateHistoricalMutationPost proves the records owned by an earlier
// terminal batch without requiring the entire aggregate to remain frozen at
// that event. Immutable batch outputs must still be byte-exact; mutable
// non-source records may have advanced under independently validated later
// commands. The terminal source transition itself is checked byte-exact by
// decodeRouteTerminalPayload below.
func validateHistoricalMutationPost(current *RoutingState, batch MutationBatch, commandID string) error {
	if current == nil {
		return fmt.Errorf("%w: routing state is absent", ErrMutationInconsistent)
	}
	materialized, err := batch.materialize(commandID)
	if err != nil {
		return err
	}
	for _, mutation := range materialized {
		actual, exists, err := mutationRecordBytes(current, mutation.Kind, mutation.Key)
		if err != nil {
			return err
		}
		if len(mutation.After) == 0 {
			if exists {
				return fmt.Errorf("%w: deleted batch record %s/%s is present", ErrMutationInconsistent, mutation.Kind, mutation.Key)
			}
			continue
		}
		if !exists {
			return fmt.Errorf("%w: batch record %s/%s is absent", ErrMutationInconsistent, mutation.Kind, mutation.Key)
		}
		if historicalMutableKind(mutation.Kind) {
			continue
		}
		if !bytes.Equal(actual, mutation.After) {
			return fmt.Errorf("%w: immutable batch record %s/%s differs", ErrMutationInconsistent, mutation.Kind, mutation.Key)
		}
	}
	return nil
}

func historicalMutableKind(kind MutationRecordKind) bool {
	switch kind {
	case MutationPath, MutationScope, MutationReservation, MutationPropagation:
		return true
	default:
		return false
	}
}

func mutationRecordBytes(current *RoutingState, kind MutationRecordKind, key string) ([]byte, bool, error) {
	var value any
	var ok bool
	switch kind {
	case MutationPath:
		value, ok = current.Paths[key]
	case MutationScope:
		value, ok = current.Scopes[key]
	case MutationReservation:
		value, ok = current.Reservations[key]
	case MutationActivation:
		value, ok = current.Activations[key]
	case MutationCandidateClosure:
		value, ok = current.CandidateClosures[key]
	case MutationCauseRecord:
		value, ok = current.CauseRecords[key]
	case MutationCauseSet:
		value, ok = current.CauseSets[key]
	case MutationDetachmentSet:
		value, ok = current.DetachmentSets[key]
	case MutationDetachment:
		value, ok = current.Detachments[key]
	case MutationPropagation:
		value, ok = current.Propagation[key]
	default:
		return nil, false, fmt.Errorf("%w: invalid mutation kind %q", ErrMutationInvalid, kind)
	}
	if !ok {
		return nil, false, nil
	}
	encoded, err := json.Marshal(value)
	return encoded, true, err
}
