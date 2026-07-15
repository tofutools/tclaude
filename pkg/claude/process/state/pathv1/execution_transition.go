package pathv1

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	legacy "github.com/tofutools/tclaude/pkg/claude/process/state"
)

// ExecutionTransition is a sealed exact successor. Its bytes can only be
// created by path-v1 planner/reducer constructors; callers cannot inject an
// aggregate into the persistence boundary.
type ExecutionTransition struct {
	pre       CheckpointBinding
	post      CheckpointBinding
	postBytes []byte
	kind      string
}

func (t *ExecutionTransition) PreBinding() CheckpointBinding {
	if t == nil {
		return CheckpointBinding{}
	}
	return t.pre
}

func (t *ExecutionTransition) PostBinding() CheckpointBinding {
	if t == nil {
		return CheckpointBinding{}
	}
	return t.post
}

func (t *ExecutionTransition) Kind() string {
	if t == nil {
		return ""
	}
	return t.kind
}

func newExecutionTransition(checkpoint, next *CheckpointV7, kind string) (*ExecutionTransition, error) {
	if err := ValidateCheckpointV7(checkpoint); err != nil {
		return nil, err
	}
	if err := ValidateCheckpointV7(next); err != nil {
		return nil, err
	}
	pre := CurrentCheckpointBinding(checkpoint)
	post := CurrentCheckpointBinding(next)
	if next.Execution == nil || next.Execution.PreviousDigest != pre.Digest || post.Generation != pre.Generation+1 || post.Digest == pre.Digest || kind == "" {
		return nil, fmt.Errorf("%w: invalid sealed execution successor", ErrMutationInvalid)
	}
	postBytes, err := EncodeCheckpointV7(next)
	if err != nil {
		return nil, err
	}
	return &ExecutionTransition{pre: pre, post: post, postBytes: bytes.Clone(postBytes), kind: kind}, nil
}

// ValidateExecutionTransitionForAppend revalidates a sealed transition against
// the exact coherently read checkpoint and template source. Detached bytes and
// checkpoint values are returned so the store never installs caller aliases.
func ValidateExecutionTransitionForAppend(ctx context.Context, currentBytes, templateSource []byte, transition *ExecutionTransition) (CheckpointBinding, []byte, *CheckpointV7, error) {
	if err := ctx.Err(); err != nil {
		return CheckpointBinding{}, nil, nil, err
	}
	if transition == nil || transition.kind == "" || len(transition.postBytes) == 0 {
		return CheckpointBinding{}, nil, nil, fmt.Errorf("%w: sealed execution transition is required", ErrMutationInvalid)
	}
	current, err := DecodeCheckpointV7(currentBytes)
	if err != nil {
		return CheckpointBinding{}, nil, nil, err
	}
	if _, err := VerifyExclusiveInput(ctx, currentBytes, templateSource); err != nil {
		return CheckpointBinding{}, nil, nil, err
	}
	pre := CurrentCheckpointBinding(current)
	postBytes := bytes.Clone(transition.postBytes)
	post, err := DecodeCheckpointV7(postBytes)
	if err != nil {
		return pre, nil, nil, err
	}
	if _, err := VerifyExclusiveInput(ctx, postBytes, templateSource); err != nil {
		return CurrentCheckpointBinding(current), nil, nil, err
	}
	if bytes.Equal(currentBytes, postBytes) {
		if pre != transition.post {
			return pre, nil, nil, fmt.Errorf("%w: exact transition bytes have a different post-binding", ErrMutationInconsistent)
		}
		return transition.pre, postBytes, post, nil
	}
	if pre != transition.pre {
		return pre, nil, nil, fmt.Errorf("%w: transition pre-binding differs from current checkpoint", ErrMutationInconsistent)
	}
	currentInitialize, currentInitializeDigest, err := CanonicalInitializationAnchor(current)
	if err != nil {
		return pre, nil, nil, err
	}
	postInitialize, postInitializeDigest, err := CanonicalInitializationAnchor(post)
	if err != nil {
		return pre, nil, nil, err
	}
	if !bytes.Equal(postInitialize, currentInitialize) || postInitializeDigest != currentInitializeDigest ||
		post.Execution == nil || post.Execution.Revision != CheckpointRevision(current)+1 ||
		post.Execution.PreviousDigest != pre.Digest || CurrentCheckpointBinding(post) != transition.post {
		return pre, nil, nil, fmt.Errorf("%w: transition post-state is not the exact next checkpoint", ErrMutationInvalid)
	}
	if !post.Execution.LogAdvanced && (post.Execution.LastLogSeq != CurrentLastLogSeq(current) || post.Execution.LogChecksum != CurrentLogChecksum(current)) {
		return pre, nil, nil, fmt.Errorf("%w: log-preserving transition changed completion anchors", ErrMutationInvalid)
	}
	if post.Execution.LogAdvanced {
		currentLogSeq := CurrentLastLogSeq(current)
		if post.Execution.LastLogSeq <= currentLogSeq || post.Execution.LastLogSeq-currentLogSeq > uint64(MaxRoutingLogEntries) {
			return pre, nil, nil, fmt.Errorf("%w: log-advancing transition has invalid logical sequence delta", ErrMutationInvalid)
		}
	}
	return pre, postBytes, post, nil
}

// ExclusiveAttemptPlan is a sealed exact perform_attempt_v1 command plus the
// adapter request authority derived from the exact current template.
type ExclusiveAttemptPlan struct {
	command   CommandRecord
	nodeID    string
	performer *model.Performer
	params    map[string]string
}

func (p *ExclusiveAttemptPlan) Command() CommandRecord {
	if p == nil {
		return CommandRecord{}
	}
	return cloneCommandRecord(p.command)
}

func (p *ExclusiveAttemptPlan) NodeID() string {
	if p == nil {
		return ""
	}
	return p.nodeID
}

func (p *ExclusiveAttemptPlan) Performer() *model.Performer {
	if p == nil || p.performer == nil {
		return nil
	}
	return cloneExclusivePerformer(p.performer)
}

func (p *ExclusiveAttemptPlan) Params() map[string]string {
	if p == nil {
		return nil
	}
	return cloneExclusiveParams(p.params)
}

func PlanExclusiveAttempt(ctx context.Context, input *VerifiedExclusiveInput, sourcePathID PathID, attempt uint64, params map[string]string) (*ExclusiveAttemptPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input == nil || input.checkpoint == nil || input.template == nil || sourcePathID == "" || attempt == 0 {
		return nil, fmt.Errorf("%w: complete sealed attempt input is required", ErrExclusiveInputInvalid)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	view := aggregate.View()
	for _, command := range view.Commands {
		if command.State.Active() {
			return nil, fmt.Errorf("%w: active command %q must recover before planning", ErrMutationInconsistent, command.ID)
		}
	}
	source, ok := view.Routing.Paths[sourcePathID]
	if !ok || source.Kind != PathActivationOutput || source.State != PathLive {
		return nil, fmt.Errorf("%w: source path is not a live activation output", ErrMutationInconsistent)
	}
	activation, ok := view.Routing.Activations[source.SourceActivation.ID]
	if !ok || activation.Ref != source.SourceActivation {
		return nil, fmt.Errorf("%w: source activation is missing or mismatched", ErrMutationInconsistent)
	}
	reservation, ok := view.Routing.Reservations[activation.ReservationID]
	if !ok || reservation.State != ReservationActivated {
		return nil, fmt.Errorf("%w: source reservation is not activated", ErrMutationInconsistent)
	}
	node, ok := input.template.Nodes[reservation.NodeID]
	if !ok {
		return nil, fmt.Errorf("%w: source node is absent from exact template", ErrExclusiveInputInvalid)
	}
	if (node.Type != model.NodeTypeTask && node.Type != model.NodeTypeDecision) || node.Performer == nil || node.IsCompound() {
		return nil, fmt.Errorf("%w: node %q of type %q has no direct performer dispatch path", ErrExclusiveUnsupported, reservation.NodeID, node.Type)
	}
	wantAttempt := uint64(1)
	for _, command := range view.Commands {
		if command.Identity.Kind == CommandPerformAttempt && command.Identity.SourceActivationID == source.SourceActivation.ID && command.Identity.Attempt >= wantAttempt {
			if command.Identity.Attempt == math.MaxUint64 {
				return nil, &OverBudgetError{Limit: "attempt", Value: math.MaxInt64, Maximum: math.MaxInt64 - 1}
			}
			wantAttempt = command.Identity.Attempt + 1
		}
	}
	if attempt != wantAttempt {
		return nil, fmt.Errorf("%w: attempt %d is not exact next attempt %d", ErrMutationInvalid, attempt, wantAttempt)
	}
	performer := model.InterpolatePerformer(*node.Performer, params)
	payload, err := performCommandPayload(view, reservation.NodeID, source, attempt, &performer, params)
	if err != nil {
		return nil, err
	}
	identity := CommandIdentity{
		RunID: view.RunID, Kind: CommandPerformAttempt, PayloadSchema: 1,
		SourceActivationID: source.SourceActivation.ID, SourceGeneration: source.SourceActivation.Generation,
		Attempt: attempt, PlanDigest: payloadDigest(payload),
	}
	command, err := commandWithState(identity, payload, CommandIssued)
	if err != nil {
		return nil, err
	}
	return &ExclusiveAttemptPlan{command: command, nodeID: reservation.NodeID, performer: cloneExclusivePerformer(&performer), params: cloneExclusiveParams(params)}, nil
}

// ClaimExclusiveAttempt creates the exact durable self-only claim transition.
// It does not invoke an adapter and is safe to compute inside a coherent read.
func ClaimExclusiveAttempt(ctx context.Context, input *VerifiedExclusiveInput, plan *ExclusiveAttemptPlan) (*ExecutionTransition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input == nil || plan == nil {
		return nil, fmt.Errorf("%w: sealed input and attempt plan are required", ErrMutationInvalid)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	view := aggregate.View()
	activation := view.Routing.Activations[plan.command.Identity.SourceActivationID]
	if activation.ID == "" || activation.Ref.Generation != plan.command.Identity.SourceGeneration {
		return nil, fmt.Errorf("%w: attempt plan activation is not current", ErrMutationInvalid)
	}
	source := view.Routing.Paths[activation.OutputPathID]
	replanned, err := PlanExclusiveAttempt(ctx, input, source.ID, plan.command.Identity.Attempt, plan.params)
	if err != nil {
		return nil, err
	}
	if !exactExclusiveCommand(replanned.command, plan.command) || replanned.nodeID != plan.nodeID || !canonicalEqual(replanned.performer, plan.performer) || !canonicalEqual(replanned.params, plan.params) {
		return nil, fmt.Errorf("%w: supplied attempt differs from deterministic plan", ErrMutationInvalid)
	}
	aggregate.Commands[plan.command.ID] = cloneCommandRecord(plan.command)
	effect := SideEffectIdentity{Kind: SideEffectAttempt, RunID: aggregate.RunID, ActivationID: activation.ID, Attempt: plan.command.Identity.Attempt, State: "claimed"}
	effect.ID, err = AttemptIdentity(effect.RunID, effect.ActivationID, effect.Attempt)
	if err != nil {
		return nil, err
	}
	aggregate.SideEffects[effect.ID] = effect
	next, err := advanceCheckpointV7(input.checkpoint, aggregate, CurrentRunStatus(input.checkpoint))
	if err != nil {
		return nil, err
	}
	return newExecutionTransition(input.checkpoint, next, "claim_attempt")
}

// RecoverExclusiveAttempt returns the sole durable active performer request.
// It never converts a claimed command back into new-perform authority.
func RecoverExclusiveAttempt(ctx context.Context, input *VerifiedExclusiveInput) (*ExclusiveAttemptPlan, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if input == nil || input.checkpoint == nil || input.template == nil {
		return nil, false, fmt.Errorf("%w: sealed input is required", ErrExclusiveInputInvalid)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, false, err
	}
	var command CommandRecord
	for _, candidate := range aggregate.Commands {
		if !candidate.State.Active() || candidate.Identity.Kind != CommandPerformAttempt {
			continue
		}
		if command.ID != "" {
			return nil, false, fmt.Errorf("%w: multiple active performer commands", ErrMutationInconsistent)
		}
		command = cloneCommandRecord(candidate)
	}
	if command.ID == "" {
		return nil, false, nil
	}
	var payload performAttemptPayload
	if err := decodeExactPayload(command.Payload, &payload); err != nil {
		return nil, false, fmt.Errorf("%w: claimed performer payload: %v", ErrMutationInvalid, err)
	}
	view := aggregate.View()
	activation, ok := view.Routing.Activations[command.Identity.SourceActivationID]
	if !ok || activation.Ref.Generation != command.Identity.SourceGeneration || activation.OutputPathID == "" {
		return nil, false, fmt.Errorf("%w: claimed performer activation is unavailable", ErrMutationInconsistent)
	}
	source, ok := view.Routing.Paths[activation.OutputPathID]
	if !ok || source.State != PathLive {
		return nil, false, fmt.Errorf("%w: claimed performer source path is not live", ErrMutationInconsistent)
	}
	reservation, ok := view.Routing.Reservations[activation.ReservationID]
	if !ok || reservation.NodeID != payload.NodeID {
		return nil, false, fmt.Errorf("%w: claimed performer node binding differs", ErrMutationInconsistent)
	}
	node, ok := input.template.Nodes[reservation.NodeID]
	if !ok || node.Performer == nil || (node.Type != model.NodeTypeTask && node.Type != model.NodeTypeDecision) || node.IsCompound() {
		return nil, false, fmt.Errorf("%w: claimed node has no direct performer path", ErrExclusiveUnsupported)
	}
	wantPerformer := model.InterpolatePerformer(*node.Performer, payload.Params)
	if payload.TemplateRef != view.TemplateRef || payload.TemplateSourceHash != view.TemplateSourceHash ||
		payload.SourceActivationID != string(activation.ID) || payload.SourceGeneration != activation.Ref.Generation ||
		payload.Attempt != command.Identity.Attempt || !canonicalEqual(payload.Performer, &wantPerformer) {
		return nil, false, fmt.Errorf("%w: claimed performer request differs from exact template-bound payload", ErrMutationInvalid)
	}
	effectID, err := AttemptIdentity(view.RunID, activation.ID, command.Identity.Attempt)
	if err != nil {
		return nil, false, err
	}
	effect, ok := aggregate.SideEffects[effectID]
	if !ok || !ActiveSideEffect(effect) {
		return nil, false, fmt.Errorf("%w: claimed performer lacks active attempt evidence", ErrMutationInconsistent)
	}
	return &ExclusiveAttemptPlan{
		command: command, nodeID: reservation.NodeID, performer: cloneExclusivePerformer(payload.Performer), params: cloneExclusiveParams(payload.Params),
	}, true, nil
}

// ObserveExclusiveAttempt settles exactly one durable claim. recovered marks
// an observation obtained through adapter reconciliation after an ambiguous
// perform acknowledgement; it never authorizes another Perform call.
func ObserveExclusiveAttempt(ctx context.Context, input *VerifiedExclusiveInput, plan *ExclusiveAttemptPlan, observation ExclusiveObservation, recovered bool) (*ExecutionTransition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	current, found, err := RecoverExclusiveAttempt(ctx, input)
	if err != nil {
		return nil, err
	}
	if !found || plan == nil || !exactExclusiveCommand(current.command, plan.command) || !canonicalEqual(current.performer, plan.performer) || !canonicalEqual(current.params, plan.params) {
		return nil, fmt.Errorf("%w: observation does not settle the exact durable claim", ErrMutationInvalid)
	}
	actor := legacy.ActorRef(strings.TrimSpace(observation.Actor))
	if strings.TrimSpace(observation.Outcome) == "" || !legacy.ValidateActorRef(actor) || legacy.IsEngineActor(actor) {
		return nil, fmt.Errorf("%w: performer observation requires outcome and actor provenance", ErrMutationInvalid)
	}
	observation.Actor = string(actor)
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	view := aggregate.View()
	activation := view.Routing.Activations[plan.command.Identity.SourceActivationID]
	source := view.Routing.Paths[activation.OutputPathID]
	node := input.template.Nodes[plan.nodeID]
	observation.Outcome, err = canonicalExclusiveOutcome(node, observation.Outcome)
	if err != nil {
		return nil, err
	}
	outcome := observation.Outcome
	observation.SourcePathID = source.ID
	observation.Attempt = plan.command.Identity.Attempt
	disposition, err := classifyExclusiveObservation(view, input.template, observation)
	if err != nil {
		return nil, err
	}
	if disposition == ExclusiveRouteReady {
		outgoing, err := exactOutgoingEdges(view.TemplateRef, plan.nodeID, node.Next)
		if err != nil {
			return nil, err
		}
		if _, err := resolveExclusiveEdge(node, observation.Outcome, outgoing); err != nil {
			return nil, err
		}
	}
	observedPerform := cloneCommandRecord(plan.command)
	observedPerform.State = CommandObserved
	if recovered {
		observedPerform.State = CommandReconciled
	}
	if err := ValidateCommand(observedPerform); err != nil {
		return nil, err
	}
	aggregate.Commands[observedPerform.ID] = observedPerform
	settlePayload, err := json.Marshal(settleAttemptObservationPayload{
		TemplateRef: aggregate.TemplateRef, SourceCommandID: observedPerform.ID,
		SourceActivationID: activation.ID, SourceGeneration: activation.Ref.Generation,
		Attempt: plan.command.Identity.Attempt, ResultCode: outcome,
		Actor: observation.Actor, EvidenceRef: observation.EvidenceRef, EvidenceHash: observation.EvidenceHash,
		ResolutionDigest: observation.ResolutionDigest, ExternalRef: observation.ExternalRef, Feedback: observation.Feedback,
	})
	if err != nil {
		return nil, err
	}
	settleIdentity := CommandIdentity{
		RunID: aggregate.RunID, Kind: CommandSettleAttempt, PayloadSchema: 1,
		SourceActivationID: activation.ID, SourceGeneration: activation.Ref.Generation,
		Attempt: plan.command.Identity.Attempt, InputDigest: observedPerform.ID,
		PlanDigest: payloadDigest(settlePayload), ResultCode: outcome,
	}
	settle, err := observedCommand(settleIdentity, settlePayload)
	if err != nil {
		return nil, err
	}
	aggregate.Commands[settle.ID] = settle
	effectID, err := AttemptIdentity(aggregate.RunID, activation.ID, plan.command.Identity.Attempt)
	if err != nil {
		return nil, err
	}
	effect := aggregate.SideEffects[effectID]
	effect.State = "observed"
	aggregate.SideEffects[effectID] = effect
	if _, _, _, err := observedAttemptCommands(aggregate.View(), plan.nodeID, input.template.Nodes[plan.nodeID], source, observation); err != nil {
		return nil, err
	}
	next, err := advanceCheckpointV7(input.checkpoint, aggregate, CurrentRunStatus(input.checkpoint))
	if err != nil {
		return nil, err
	}
	return newExecutionTransition(input.checkpoint, next, "observe_attempt")
}

// PendingExclusiveObservation returns the sole routable observed settlement
// that still owns a live source path. Retry-pending settlements remain durable
// provenance but do not prevent planning their exact next attempt.
func PendingExclusiveObservation(ctx context.Context, input *VerifiedExclusiveInput) (ExclusiveObservation, bool, error) {
	if err := ctx.Err(); err != nil {
		return ExclusiveObservation{}, false, err
	}
	if input == nil || input.checkpoint == nil {
		return ExclusiveObservation{}, false, fmt.Errorf("%w: sealed input is required", ErrExclusiveInputInvalid)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return ExclusiveObservation{}, false, err
	}
	used := make(map[string]bool)
	for _, command := range aggregate.Commands {
		if command.Identity.Kind == CommandRoutePaths {
			used[command.Identity.InputDigest] = true
		}
	}
	var pending ExclusiveObservation
	found := false
	for _, command := range aggregate.Commands {
		if command.Identity.Kind != CommandSettleAttempt || (command.State != CommandObserved && command.State != CommandReconciled) || used[command.ID] {
			continue
		}
		activation, ok := aggregate.Routing.Activations[command.Identity.SourceActivationID]
		if !ok || activation.Ref.Generation != command.Identity.SourceGeneration {
			continue
		}
		source, ok := aggregate.Routing.Paths[activation.OutputPathID]
		if !ok || source.State != PathLive {
			continue
		}
		var payload settleAttemptObservationPayload
		if err := decodeExactPayload(command.Payload, &payload); err != nil {
			return ExclusiveObservation{}, false, fmt.Errorf("%w: pending settlement payload: %v", ErrMutationInvalid, err)
		}
		candidate := ExclusiveObservation{
			SourcePathID: source.ID, Attempt: command.Identity.Attempt, Outcome: payload.ResultCode,
			Actor: payload.Actor, EvidenceRef: payload.EvidenceRef, EvidenceHash: payload.EvidenceHash,
			ResolutionDigest: payload.ResolutionDigest, ExternalRef: payload.ExternalRef, Feedback: payload.Feedback,
		}
		disposition, err := classifyExclusiveObservation(aggregate.View(), input.template, candidate)
		if err != nil {
			return ExclusiveObservation{}, false, err
		}
		if disposition == ExclusiveRetryPending || disposition == ExclusiveResolvedRetry {
			continue
		}
		if disposition != ExclusiveRouteReady {
			return ExclusiveObservation{}, false, fmt.Errorf("%w: pending observation disposition %q requires explicit integration", ErrExclusiveUnsupported, disposition)
		}
		if found {
			return ExclusiveObservation{}, false, fmt.Errorf("%w: multiple pending exclusive observations", ErrMutationInconsistent)
		}
		pending = candidate
		found = true
	}
	return pending, found, nil
}

// AdvanceExclusiveRoute deterministically derives the sole pending durable
// observation from the verified checkpoint, then folds it through route,
// local DPE, activation, and a directly reached end node. Callers cannot
// supply observation authority to this persistence-authorizing constructor.
func AdvanceExclusiveRoute(ctx context.Context, input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
	observation, found, err := PendingExclusiveObservation(ctx, input)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%w: no pending durable observation is routable", ErrExclusiveNotRoutable)
	}
	return advanceExclusiveRoute(ctx, input, observation)
}

func advanceExclusiveRoute(ctx context.Context, input *VerifiedExclusiveInput, observation ExclusiveObservation) (*ExecutionTransition, error) {
	sequence, err := PlanExclusiveRouteSequence(ctx, input, observation)
	if err != nil {
		return nil, err
	}
	commands := sequence.Commands()
	projection := &ExclusiveProjection{
		aggregate: sequence.final, binding: input.binding,
		command: commands[len(commands)-1], dispose: ReplayApplied,
	}
	lastLogSeq, err := aggregateLogicalLastSeq(projection.aggregate)
	if err != nil {
		return nil, err
	}
	next, err := advanceCheckpointV7To(input.checkpoint, projection.aggregate, CurrentRunStatus(input.checkpoint), lastLogSeq)
	if err != nil {
		return nil, err
	}
	return newExecutionTransition(input.checkpoint, next, "route_observation")
}

func aggregateLogicalLastSeq(aggregate AggregateCheckpoint) (uint64, error) {
	var maximum int64
	add := func(sequence int64) error {
		if sequence < 0 {
			return fmt.Errorf("%w: negative aggregate event sequence", ErrMutationInconsistent)
		}
		if sequence > maximum {
			maximum = sequence
		}
		return nil
	}
	for _, path := range aggregate.Routing.Paths {
		for _, sequence := range []int64{path.CreatedSeq, path.UpdatedSeq, path.ArrivedSeq} {
			if err := add(sequence); err != nil {
				return 0, err
			}
		}
		if path.Disposition != nil {
			if err := add(path.Disposition.EventSeq); err != nil {
				return 0, err
			}
		}
		if path.DetachedSink != nil {
			if err := add(path.DetachedSink.EventSeq); err != nil {
				return 0, err
			}
		}
	}
	for _, scope := range aggregate.Routing.Scopes {
		if err := add(scope.EventSeq); err != nil {
			return 0, err
		}
	}
	for _, reservation := range aggregate.Routing.Reservations {
		if err := add(reservation.EventSeq); err != nil {
			return 0, err
		}
		if reservation.CloseReceipt != nil {
			if err := add(reservation.CloseReceipt.EventSeq); err != nil {
				return 0, err
			}
		}
	}
	for _, activation := range aggregate.Routing.Activations {
		if err := add(activation.EventSeq); err != nil {
			return 0, err
		}
		if err := add(activation.Receipt.EventSeq); err != nil {
			return 0, err
		}
	}
	for _, cause := range aggregate.Routing.CauseRecords {
		if err := add(cause.EventSeq); err != nil {
			return 0, err
		}
	}
	for _, closure := range aggregate.Routing.CandidateClosures {
		if err := add(closure.EventSeq); err != nil {
			return 0, err
		}
	}
	for _, detachment := range aggregate.Routing.Detachments {
		if err := add(detachment.EventSeq); err != nil {
			return 0, err
		}
		if err := add(detachment.ActivatedSeq); err != nil {
			return 0, err
		}
	}
	for _, propagation := range aggregate.Routing.Propagation {
		if err := add(propagation.EventSeq); err != nil {
			return 0, err
		}
	}
	for _, admin := range aggregate.AdminRecords {
		if err := add(admin.EventSeq); err != nil {
			return 0, err
		}
	}
	return uint64(maximum), nil
}

// AdvanceExclusiveStart routes an instantaneous start node without creating
// adapter dispatch authority.
func AdvanceExclusiveStart(ctx context.Context, input *VerifiedExclusiveInput, sourcePathID PathID) (*ExecutionTransition, error) {
	if input == nil || input.checkpoint == nil || input.template == nil {
		return nil, fmt.Errorf("%w: sealed input is required", ErrExclusiveInputInvalid)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	source, ok := aggregate.Routing.Paths[sourcePathID]
	activation := aggregate.Routing.Activations[source.SourceActivation.ID]
	reservation := aggregate.Routing.Reservations[activation.ReservationID]
	node, nodeOK := input.template.Nodes[reservation.NodeID]
	if !ok || source.State != PathLive || !nodeOK || node.Type != model.NodeTypeStart {
		return nil, fmt.Errorf("%w: source is not a live instantaneous start", ErrExclusiveUnsupported)
	}
	return advanceExclusiveRoute(ctx, input, ExclusiveObservation{SourcePathID: sourcePathID, Attempt: 1, Outcome: "pass"})
}

func completionReplayView(checkpoint *CheckpointV7, aggregate AggregateCheckpoint, selfCommandID string) (CompletionReplayView, error) {
	checkpointJSON, err := CompletionCheckpointJSON(checkpoint, selfCommandID)
	if err != nil {
		return CompletionReplayView{}, err
	}
	view := CompletionReplayView{
		Aggregate: aggregate.View(), CheckpointJSON: checkpointJSON,
		RunStatus: CurrentRunStatus(checkpoint), LastLogSeq: CurrentLastLogSeq(checkpoint), LogChecksum: CurrentLogChecksum(checkpoint),
		Checkpoint: CurrentCheckpointBinding(checkpoint),
	}
	basis, err := computeCompletionBasis(view, CompletionBasis{
		BasisRunStatus: view.RunStatus, BasisLastLogSeq: view.LastLogSeq, BasisLogChecksum: view.LogChecksum,
	}, selfCommandID)
	if err != nil {
		return CompletionReplayView{}, err
	}
	view.Checkpoint = completionBasisCheckpoint(basis)
	return view, nil
}

// ClaimExclusiveCompletion durably installs the sole completion self-command
// against the exact current checkpoint basis.
func ClaimExclusiveCompletion(ctx context.Context, input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	view, err := completionReplayView(input.checkpoint, aggregate, "")
	if err != nil {
		return nil, err
	}
	recovery, err := RecoverCompleteRun(view)
	if err != nil || recovery.Phase != CompletionReadyToClaim {
		return nil, fmt.Errorf("%w: completion is not ready to claim: %v", ErrMutationInconsistent, err)
	}
	aggregate.Commands[recovery.Command.ID] = cloneCommandRecord(recovery.Command)
	next, err := advanceCheckpointV7PreservingLog(input.checkpoint, aggregate, CurrentRunStatus(input.checkpoint))
	if err != nil {
		return nil, err
	}
	return newExecutionTransition(input.checkpoint, next, "claim_completion")
}

// ObserveExclusiveCompletion atomically marks the completion self-command and
// the run status from the same validated completion basis.
func ObserveExclusiveCompletion(ctx context.Context, input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	var self string
	for id, command := range aggregate.Commands {
		if command.Identity.Kind == CommandCompleteRun && command.State.Active() {
			if self != "" {
				return nil, fmt.Errorf("%w: multiple active completion commands", ErrMutationInconsistent)
			}
			self = id
		}
	}
	if self == "" {
		return nil, fmt.Errorf("%w: active completion command is absent", ErrMutationInconsistent)
	}
	view, err := completionReplayView(input.checkpoint, aggregate, self)
	if err != nil {
		return nil, err
	}
	recovery, err := RecoverCompleteRun(view)
	if err != nil || recovery.Phase != CompletionReadyToObserve {
		return nil, fmt.Errorf("%w: completion is not ready to observe: %v", ErrMutationInconsistent, err)
	}
	command := aggregate.Commands[self]
	command.State = CommandObserved
	aggregate.Commands[self] = command
	next, err := advanceCheckpointV7(input.checkpoint, aggregate, recovery.Result)
	if err != nil {
		return nil, err
	}
	return newExecutionTransition(input.checkpoint, next, "observe_completion")
}

func cloneExclusivePerformer(performer *model.Performer) *model.Performer {
	if performer == nil {
		return nil
	}
	clone := *performer
	clone.Choices = append([]string(nil), performer.Choices...)
	clone.Args = append([]string(nil), performer.Args...)
	if performer.ChoiceOutcomes != nil {
		clone.ChoiceOutcomes = make(map[string]string, len(performer.ChoiceOutcomes))
		for choice, outcome := range performer.ChoiceOutcomes {
			clone.ChoiceOutcomes[choice] = outcome
		}
	}
	if performer.Contact != nil {
		contact := *performer.Contact
		clone.Contact = &contact
	}
	return &clone
}

func cloneExclusiveParams(params map[string]string) map[string]string {
	if params == nil {
		return nil
	}
	clone := make(map[string]string, len(params))
	for key, value := range params {
		clone[key] = value
	}
	return clone
}

func performCommandPayload(view AggregateView, nodeID string, source PathRecord, attempt uint64, performer *model.Performer, params map[string]string) ([]byte, error) {
	return json.Marshal(performAttemptPayload{
		TemplateRef: view.TemplateRef, TemplateSourceHash: view.TemplateSourceHash, NodeID: nodeID,
		SourceActivationID: source.SourceActivation.ID, SourceGeneration: source.SourceActivation.Generation, Attempt: attempt,
		Performer: cloneExclusivePerformer(performer), Params: cloneExclusiveParams(params),
	})
}

func commandWithState(identity CommandIdentity, payload []byte, state CommandState) (CommandRecord, error) {
	command, err := observedCommand(identity, payload)
	if err != nil {
		return CommandRecord{}, err
	}
	command.State = state
	if err := ValidateCommand(command); err != nil {
		return CommandRecord{}, err
	}
	return command, nil
}
