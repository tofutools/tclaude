package pathv1

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

var (
	ErrExclusiveInputInvalid = errors.New("path-v1 exclusive input is invalid")
	ErrExclusiveUnsupported  = errors.New("path-v1 exclusive operation is unsupported")
	ErrExclusiveNotRoutable  = errors.New("path-v1 exclusive observation is not routable")
)

// VerifiedExclusiveInput is a sealed, detached pure-execution input. The only
// constructor accepts exact checkpoint and template-source bytes; callers
// cannot assemble or mutate its run, template, aggregate, or checkpoint
// authority after verification.
type VerifiedExclusiveInput struct {
	checkpointBytes []byte
	templateSource  []byte
	checkpoint      *CheckpointV7
	template        *model.Template
	binding         CheckpointBinding
}

// ExclusiveObservation is the already-reduced observation boundary used by
// the dormant pure planner. Adapter claim/dispatch/observation remains outside
// this package and is deliberately not wired by this API.
type ExclusiveObservation struct {
	SourcePathID     PathID
	Attempt          uint64
	Outcome          string
	ResolutionDigest string
	Actor            string
	EvidenceRef      string
	EvidenceHash     string
	ExternalRef      string
	Feedback         string
}

type ExclusiveDisposition string

const (
	ExclusiveRouteReady     ExclusiveDisposition = "route_ready"
	ExclusiveRetryPending   ExclusiveDisposition = "retry_pending"
	ExclusiveResolvedRetry  ExclusiveDisposition = "resolved_retry"
	ExclusiveResolvedSkip   ExclusiveDisposition = "resolved_skip"
	ExclusiveResolvedCancel ExclusiveDisposition = "resolved_cancel"
)

type ExclusiveCompletionInput struct {
	CheckpointJSON []byte
	RunStatus      string
	LastLogSeq     uint64
	LogChecksum    string
}

// ExclusiveProjection is the sealed result of replaying one pure routing
// command. Accessors return copies so the validated post-state cannot be
// changed through caller aliases.
type ExclusiveProjection struct {
	aggregate AggregateCheckpoint
	binding   CheckpointBinding
	command   CommandRecord
	dispose   ReplayDisposition
}

// Binding returns the original verified checkpoint replay basis. A projection
// is not an encoded checkpoint and therefore never invents checkpoint
// authority from its routing-state digest.
func (p *ExclusiveProjection) Binding() CheckpointBinding {
	if p == nil {
		return CheckpointBinding{}
	}
	return p.binding
}

func (p *ExclusiveProjection) Command() CommandRecord {
	if p == nil {
		return CommandRecord{}
	}
	return cloneCommandRecord(p.command)
}

func (p *ExclusiveProjection) ReplayDisposition() ReplayDisposition {
	if p == nil {
		return ""
	}
	return p.dispose
}

// Routing returns a deep copy intended for tests and later pure composition.
// It is not a persistence or active-runtime surface.
func (p *ExclusiveProjection) Routing() RoutingState {
	if p == nil {
		return RoutingState{}
	}
	return Clone(p.aggregate.Routing)
}

// VerifyExclusiveInput performs the complete pure gate: strict schema-7
// checkpoint validation, exact semantic/source template binding, aggregate
// authority validation, and exact-template reservation topology validation.
func VerifyExclusiveInput(ctx context.Context, checkpointBytes, templateSource []byte) (*VerifiedExclusiveInput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(checkpointBytes) == 0 || len(checkpointBytes) > MaxCheckpointBytes {
		return nil, fmt.Errorf("%w: checkpoint size %d", ErrExclusiveInputInvalid, len(checkpointBytes))
	}
	if len(templateSource) == 0 || len(templateSource) > MaxCheckpointBytes {
		return nil, fmt.Errorf("%w: template source size %d", ErrExclusiveInputInvalid, len(templateSource))
	}

	checkpointCopy := bytes.Clone(checkpointBytes)
	sourceCopy := bytes.Clone(templateSource)
	checkpoint, err := DecodeCheckpointV7(checkpointCopy)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExclusiveInputInvalid, err)
	}
	parsed, err := model.Parse(sourceCopy)
	if err != nil {
		return nil, fmt.Errorf("%w: parse exact template: %v", ErrExclusiveInputInvalid, err)
	}
	if parsed.Diagnostics.HasErrors() {
		return nil, fmt.Errorf("%w: exact template has blocking diagnostics", ErrExclusiveInputInvalid)
	}
	event := checkpoint.Initialize
	if parsed.Ref != event.UpgradeNeeded.TemplateRef || parsed.SemanticHash != event.TemplateHash || parsed.SourceHash != event.UpgradeNeeded.TemplateSourceHash {
		return nil, fmt.Errorf("%w: exact template ref/source binding mismatch", ErrExclusiveInputInvalid)
	}
	current, err := CurrentAggregateCheckpoint(checkpoint)
	if err != nil {
		return nil, fmt.Errorf("%w: current aggregate: %v", ErrExclusiveInputInvalid, err)
	}
	view := current.View()
	if view.RunID != event.UpgradeNeeded.RunID || view.TemplateRef != parsed.SemanticHash || view.TemplateSourceHash != parsed.SourceHash {
		return nil, fmt.Errorf("%w: exact run/template aggregate binding mismatch", ErrExclusiveInputInvalid)
	}
	if err := validateExclusiveMaterializedTopology(ctx, view, parsed.Template); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExclusiveInputInvalid, err)
	}
	return &VerifiedExclusiveInput{
		checkpointBytes: checkpointCopy,
		templateSource:  sourceCopy,
		checkpoint:      checkpoint,
		template:        parsed.Template,
		binding:         CurrentCheckpointBinding(checkpoint),
	}, nil
}

// PlanExclusiveRoute creates one canonical route_paths_v1 command. It performs
// no I/O and does not expose the supporting observation records it derives;
// ReduceExclusiveRoute deterministically recomputes and validates them.
func PlanExclusiveRoute(ctx context.Context, input *VerifiedExclusiveInput, observation ExclusiveObservation) (CommandRecord, error) {
	draft, err := buildExclusiveRouteDraft(ctx, input, observation)
	if err != nil {
		return CommandRecord{}, err
	}
	return cloneCommandRecord(draft.command), nil
}

// ClassifyExclusiveObservation performs the retry and audited block-resolution
// fold without inventing a command kind. Non-route dispositions deliberately
// leave the live token untouched for a later generation-bound integration.
func ClassifyExclusiveObservation(ctx context.Context, input *VerifiedExclusiveInput, observation ExclusiveObservation) (ExclusiveDisposition, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if input == nil || input.checkpoint == nil || input.template == nil {
		return "", fmt.Errorf("%w: sealed input is required", ErrExclusiveInputInvalid)
	}
	current, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return "", err
	}
	return classifyExclusiveObservation(current.View(), input.template, observation)
}

// ReduceExclusiveRoute is the sole apply path for a planned exclusive route.
// It delegates the routing mutation to ReplayRoutePaths, then validates token
// conservation and the complete aggregate before returning a detached result.
func ReduceExclusiveRoute(ctx context.Context, input *VerifiedExclusiveInput, observation ExclusiveObservation, command CommandRecord) (*ExclusiveProjection, error) {
	draft, err := buildExclusiveRouteDraft(ctx, input, observation)
	if err != nil {
		return nil, err
	}
	if !exactExclusiveCommand(draft.command, command) {
		return nil, fmt.Errorf("%w: supplied route command differs from deterministic plan", ErrMutationInvalid)
	}
	result, err := ReplayRoutePaths(draft.view, draft.command)
	if err != nil {
		return nil, err
	}
	post := draft.view.Aggregate
	post.Routing = &result.Routing
	if err := validateExclusiveConservation(post, draft); err != nil {
		return nil, err
	}
	report := ValidateAggregate(post)
	if !report.Valid() {
		return nil, fmt.Errorf("%w: projected aggregate has %d diagnostics", ErrMutationInconsistent, len(report.Diagnostics)+report.Suppressed)
	}
	aggregate, err := checkpointAggregate(post)
	if err != nil {
		return nil, err
	}
	return &ExclusiveProjection{
		aggregate: aggregate,
		binding:   input.binding,
		command:   cloneCommandRecord(draft.command),
		dispose:   result.Disposition,
	}, nil
}

// PlanExclusiveDeadPath plans the canonical closure of one impossible sibling
// at a local exclusive merge. The preceding route command is supplied exactly
// so this operation remains anchored to the same sealed checkpoint and does
// not infer predecessor identity from evidence or logs.
func PlanExclusiveDeadPath(ctx context.Context, input *VerifiedExclusiveInput, observation ExclusiveObservation, routeCommand CommandRecord) (CommandRecord, error) {
	draft, err := buildExclusiveClosureDraft(ctx, input, observation, routeCommand)
	if err != nil {
		return CommandRecord{}, err
	}
	return cloneCommandRecord(draft.command), nil
}

// ReduceExclusiveDeadPath replays the exact route and propagation commands
// through their canonical mutation reducers and returns a detached projection.
func ReduceExclusiveDeadPath(ctx context.Context, input *VerifiedExclusiveInput, observation ExclusiveObservation, routeCommand, closureCommand CommandRecord) (*ExclusiveProjection, error) {
	draft, err := buildExclusiveClosureDraft(ctx, input, observation, routeCommand)
	if err != nil {
		return nil, err
	}
	if !exactExclusiveCommand(draft.command, closureCommand) {
		return nil, fmt.Errorf("%w: supplied closure command differs from deterministic plan", ErrMutationInvalid)
	}
	result, err := ReplayPropagateClosure(draft.view, draft.command)
	if err != nil {
		return nil, err
	}
	post := draft.view.Aggregate
	post.Routing = &result.Routing
	if len(result.Routing.CandidateClosures) != draft.preClosureCount+1 {
		return nil, fmt.Errorf("%w: local dead-path elimination did not conserve one closure", ErrMutationInconsistent)
	}
	report := ValidateAggregate(post)
	if !report.Valid() {
		return nil, fmt.Errorf("%w: projected closure aggregate has %d diagnostics", ErrMutationInconsistent, len(report.Diagnostics)+report.Suppressed)
	}
	aggregate, err := checkpointAggregate(post)
	if err != nil {
		return nil, err
	}
	return &ExclusiveProjection{
		aggregate: aggregate,
		binding:   input.binding,
		command:   cloneCommandRecord(draft.command),
		dispose:   result.Disposition,
	}, nil
}

// PlanExclusiveDeadReservation plans the canonical no-activation close for a
// fully impossible sibling reservation. A zero command is returned when the
// impossible candidate belongs to the selected local merge reservation.
func PlanExclusiveDeadReservation(ctx context.Context, input *VerifiedExclusiveInput, observation ExclusiveObservation, routeCommand, closureCommand CommandRecord) (CommandRecord, error) {
	draft, required, err := buildExclusiveDeadReservationDraft(ctx, input, observation, routeCommand, closureCommand)
	if err != nil {
		return CommandRecord{}, err
	}
	if !required {
		return CommandRecord{}, nil
	}
	return cloneCommandRecord(draft.command), nil
}

func ReduceExclusiveDeadReservation(ctx context.Context, input *VerifiedExclusiveInput, observation ExclusiveObservation, routeCommand, closureCommand, deadReservationCommand CommandRecord) (*ExclusiveProjection, error) {
	draft, required, err := buildExclusiveDeadReservationDraft(ctx, input, observation, routeCommand, closureCommand)
	if err != nil {
		return nil, err
	}
	if !required || !exactExclusiveCommand(draft.command, deadReservationCommand) {
		return nil, fmt.Errorf("%w: supplied dead-reservation command differs from deterministic plan", ErrMutationInvalid)
	}
	result, err := ReplayActivateGeneration(draft.view, draft.command)
	if err != nil {
		return nil, err
	}
	post := draft.view.Aggregate
	post.Routing = &result.Routing
	if report := ValidateAggregate(post); !report.Valid() {
		return nil, fmt.Errorf("%w: projected dead-reservation aggregate has %d diagnostics", ErrMutationInconsistent, len(report.Diagnostics)+report.Suppressed)
	}
	aggregate, err := checkpointAggregate(post)
	if err != nil {
		return nil, err
	}
	return &ExclusiveProjection{aggregate: aggregate, binding: input.binding, command: cloneCommandRecord(draft.command), dispose: result.Disposition}, nil
}

// PlanExclusiveActivation plans the local exclusive merge reached by an exact
// route. closureCommand may be zero when the selected reservation has no dead
// sibling candidate; otherwise it must be the exact preceding DPE command.
func PlanExclusiveActivation(ctx context.Context, input *VerifiedExclusiveInput, observation ExclusiveObservation, routeCommand, closureCommand, deadReservationCommand CommandRecord) (CommandRecord, error) {
	draft, err := buildExclusiveActivationDraft(ctx, input, observation, routeCommand, closureCommand, deadReservationCommand)
	if err != nil {
		return CommandRecord{}, err
	}
	return cloneCommandRecord(draft.command), nil
}

func ReduceExclusiveActivation(ctx context.Context, input *VerifiedExclusiveInput, observation ExclusiveObservation, routeCommand, closureCommand, deadReservationCommand, activationCommand CommandRecord) (*ExclusiveProjection, error) {
	draft, err := buildExclusiveActivationDraft(ctx, input, observation, routeCommand, closureCommand, deadReservationCommand)
	if err != nil {
		return nil, err
	}
	if !exactExclusiveCommand(draft.command, activationCommand) {
		return nil, fmt.Errorf("%w: supplied activation command differs from deterministic plan", ErrMutationInvalid)
	}
	result, err := ReplayActivateGeneration(draft.view, draft.command)
	if err != nil {
		return nil, err
	}
	post := draft.view.Aggregate
	post.Routing = &result.Routing
	if report := ValidateAggregate(post); !report.Valid() {
		return nil, fmt.Errorf("%w: projected activation aggregate has %d diagnostics", ErrMutationInconsistent, len(report.Diagnostics)+report.Suppressed)
	}
	aggregate, err := checkpointAggregate(post)
	if err != nil {
		return nil, err
	}
	return &ExclusiveProjection{aggregate: aggregate, binding: input.binding, command: cloneCommandRecord(draft.command), dispose: result.Disposition}, nil
}

func PlanExclusiveEnd(ctx context.Context, input *VerifiedExclusiveInput, observation ExclusiveObservation, routeCommand, closureCommand, deadReservationCommand, activationCommand CommandRecord) (CommandRecord, error) {
	draft, err := buildExclusiveEndDraft(ctx, input, observation, routeCommand, closureCommand, deadReservationCommand, activationCommand)
	if err != nil {
		return CommandRecord{}, err
	}
	return cloneCommandRecord(draft.command), nil
}

func ReduceExclusiveEnd(ctx context.Context, input *VerifiedExclusiveInput, observation ExclusiveObservation, routeCommand, closureCommand, deadReservationCommand, activationCommand, endCommand CommandRecord) (*ExclusiveProjection, error) {
	draft, err := buildExclusiveEndDraft(ctx, input, observation, routeCommand, closureCommand, deadReservationCommand, activationCommand)
	if err != nil {
		return nil, err
	}
	if !exactExclusiveCommand(draft.command, endCommand) {
		return nil, fmt.Errorf("%w: supplied end command differs from deterministic plan", ErrMutationInvalid)
	}
	result, err := ReplayRoutePaths(draft.view, draft.command)
	if err != nil {
		return nil, err
	}
	post := draft.view.Aggregate
	post.Routing = &result.Routing
	if report := ValidateAggregate(post); !report.Valid() {
		return nil, fmt.Errorf("%w: projected end aggregate has %d diagnostics", ErrMutationInconsistent, len(report.Diagnostics)+report.Suppressed)
	}
	aggregate, err := checkpointAggregate(post)
	if err != nil {
		return nil, err
	}
	return &ExclusiveProjection{aggregate: aggregate, binding: input.binding, command: cloneCommandRecord(draft.command), dispose: result.Disposition}, nil
}

func PlanExclusiveCompletion(ctx context.Context, input *VerifiedExclusiveInput, observation ExclusiveObservation, routeCommand, closureCommand, deadReservationCommand, activationCommand, endCommand CommandRecord, completion ExclusiveCompletionInput) (CommandRecord, error) {
	view, err := buildExclusiveCompletionView(ctx, input, observation, routeCommand, closureCommand, deadReservationCommand, activationCommand, endCommand, completion)
	if err != nil {
		return CommandRecord{}, err
	}
	return PlanCompleteRun(view)
}

func ReduceExclusiveCompletion(ctx context.Context, input *VerifiedExclusiveInput, observation ExclusiveObservation, routeCommand, closureCommand, deadReservationCommand, activationCommand, endCommand, completionCommand CommandRecord, completion ExclusiveCompletionInput) (CompletionRecovery, error) {
	view, err := buildExclusiveCompletionView(ctx, input, observation, routeCommand, closureCommand, deadReservationCommand, activationCommand, endCommand, completion)
	if err != nil {
		return CompletionRecovery{}, err
	}
	planned, err := PlanCompleteRun(view)
	if err != nil {
		return CompletionRecovery{}, err
	}
	if !exactExclusiveCommand(planned, completionCommand) {
		return CompletionRecovery{}, fmt.Errorf("%w: supplied completion command differs from deterministic basis", ErrMutationInvalid)
	}
	return RecoverCompleteRun(view)
}

type exclusiveRouteDraft struct {
	view          MutationReplayView
	command       CommandRecord
	eventSeq      int64
	sourcePathID  PathID
	outgoing      []EdgeKey
	selectedEdge  EdgeID
	producedPaths []PathID
}

type exclusiveClosureDraft struct {
	view            MutationReplayView
	command         CommandRecord
	eventSeq        int64
	preClosureCount int
}

type exclusiveActivationDraft struct {
	view     MutationReplayView
	command  CommandRecord
	eventSeq int64
}

type exclusiveDeadReservationDraft struct {
	view     MutationReplayView
	command  CommandRecord
	eventSeq int64
}

type exclusiveEndDraft struct {
	view     MutationReplayView
	command  CommandRecord
	eventSeq int64
}

type performAttemptPayload struct {
	TemplateRef        string            `json:"templateRef"`
	TemplateSourceHash string            `json:"templateSourceHash"`
	NodeID             string            `json:"nodeId"`
	SourceActivationID string            `json:"sourceActivationId"`
	SourceGeneration   uint64            `json:"sourceGeneration"`
	Attempt            uint64            `json:"attempt"`
	Performer          *model.Performer  `json:"performer,omitempty"`
	Params             map[string]string `json:"params,omitempty"`
}

func buildExclusiveRouteDraft(ctx context.Context, input *VerifiedExclusiveInput, observation ExclusiveObservation) (exclusiveRouteDraft, error) {
	if err := ctx.Err(); err != nil {
		return exclusiveRouteDraft{}, err
	}
	if input == nil || input.checkpoint == nil || input.template == nil {
		return exclusiveRouteDraft{}, fmt.Errorf("%w: sealed input is required", ErrExclusiveInputInvalid)
	}
	if observation.SourcePathID == "" || observation.Attempt == 0 || strings.TrimSpace(observation.Outcome) == "" {
		return exclusiveRouteDraft{}, fmt.Errorf("%w: incomplete exclusive observation", ErrMutationInvalid)
	}

	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return exclusiveRouteDraft{}, err
	}
	view := aggregate.View()
	source, ok := view.Routing.Paths[observation.SourcePathID]
	if !ok || source.Kind != PathActivationOutput || source.State != PathLive {
		return exclusiveRouteDraft{}, fmt.Errorf("%w: source path is not a live activation output", ErrMutationInconsistent)
	}
	activation, ok := view.Routing.Activations[source.SourceActivation.ID]
	if !ok || activation.Ref != source.SourceActivation {
		return exclusiveRouteDraft{}, fmt.Errorf("%w: source activation is missing or mismatched", ErrMutationInconsistent)
	}
	sourceReservation, ok := view.Routing.Reservations[activation.ReservationID]
	if !ok || sourceReservation.State != ReservationActivated {
		return exclusiveRouteDraft{}, fmt.Errorf("%w: source reservation is not activated", ErrMutationInconsistent)
	}
	node, ok := input.template.Nodes[sourceReservation.NodeID]
	if !ok {
		return exclusiveRouteDraft{}, fmt.Errorf("%w: source node is absent from exact template", ErrExclusiveInputInvalid)
	}
	observation.Outcome, err = canonicalExclusiveOutcome(node, observation.Outcome)
	if err != nil {
		return exclusiveRouteDraft{}, err
	}
	disposition, err := classifyExclusiveObservation(view, input.template, observation)
	if err != nil {
		return exclusiveRouteDraft{}, err
	}
	if disposition != ExclusiveRouteReady {
		return exclusiveRouteDraft{}, fmt.Errorf("%w: %s", ErrExclusiveNotRoutable, disposition)
	}
	outgoing, err := exactOutgoingEdges(view.TemplateRef, sourceReservation.NodeID, node.Next)
	if err != nil {
		return exclusiveRouteDraft{}, err
	}
	if len(outgoing) > 2 {
		return exclusiveRouteDraft{}, fmt.Errorf("%w: exclusive fan-out %d requires bounded multi-way DPE", ErrExclusiveUnsupported, len(outgoing))
	}
	if _, err := MutationCountExclusive(len(outgoing)); err != nil {
		return exclusiveRouteDraft{}, err
	}
	selectedIndex, err := resolveExclusiveEdge(node, observation.Outcome, outgoing)
	if err != nil {
		return exclusiveRouteDraft{}, err
	}

	if CurrentLastLogSeq(input.checkpoint) >= math.MaxInt64 {
		return exclusiveRouteDraft{}, &OverBudgetError{Limit: "log_entries", Value: math.MaxInt64, Maximum: math.MaxInt64 - 1}
	}
	eventSeq := int64(CurrentLastLogSeq(input.checkpoint) + 1)
	before := Clone(*view.Routing)
	after := Clone(before)
	authority := cloneExclusiveAuthority(view.Authority)
	produced := make([]PathID, 0, len(outgoing))
	allCauseIDs := make([]CauseID, 0, max(0, len(outgoing)-1))
	for index, edge := range outgoing {
		if err := ctx.Err(); err != nil {
			return exclusiveRouteDraft{}, err
		}
		reservation, created, err := exactExclusiveReservation(input.template, view, authority, after, edge.ToNodeID, eventSeq)
		if err != nil {
			return exclusiveRouteDraft{}, err
		}
		if created {
			authority.Reservations[reservation.ID] = reservationAuthority(reservation)
			after.Reservations[reservation.ID] = reservation
		}
		candidate, ok := candidateForEdge(reservation, edge.ID)
		if !ok {
			return exclusiveRouteDraft{}, fmt.Errorf("%w: target reservation lacks exact edge candidate", ErrExclusiveInputInvalid)
		}
		lineage, lineageID, err := AppendCandidateLineage(source, reservation.ID, candidate.ID)
		if err != nil {
			return exclusiveRouteDraft{}, err
		}
		if index == selectedIndex {
			pathID, err := EdgePathIdentity(source.SourceActivation.ID, source.ID, edge.ID, reservation.ID, candidate.ID)
			if err != nil {
				return exclusiveRouteDraft{}, err
			}
			arrivalID, err := ArrivalIdentity(pathID, reservation.ID, candidate.ID)
			if err != nil {
				return exclusiveRouteDraft{}, err
			}
			after.Paths[pathID] = PathRecord{
				ID: pathID, Kind: PathEdge, State: PathArrived, ParentPathID: source.ID,
				SourceActivation: source.SourceActivation, Edge: cloneEdge(&edge), TargetReservationID: reservation.ID,
				CandidateID: candidate.ID, ScopeID: source.ScopeID, BranchEdgeID: source.BranchEdgeID,
				CandidateLineage: lineage, CandidateLineageID: lineageID, LineageDepth: uint32(len(lineage)),
				ArrivalID: arrivalID, ArrivedSeq: eventSeq, CreatedSeq: eventSeq, UpdatedSeq: eventSeq,
			}
			produced = append(produced, pathID)
			continue
		}

		reason := "exclusive_unselected/" + outgoing[selectedIndex].ID
		causeID, err := CauseIdentity("", TerminalImpossible, reason, "", MutationCommandPlaceholder, "", uint64(eventSeq))
		if err != nil {
			return exclusiveRouteDraft{}, err
		}
		causeDigest, err := CauseSetIdentity([]CauseID{causeID})
		if err != nil {
			return exclusiveRouteDraft{}, err
		}
		after.CauseRecords[causeID] = CauseRecord{ID: causeID, TerminalKind: TerminalImpossible, DispositionReason: reason, SourceCommandID: MutationCommandPlaceholder, EventSeq: eventSeq}
		after.CauseSets[causeDigest] = CauseSetRecord{Digest: causeDigest, CauseIDs: []CauseID{causeID}}
		pathID, err := ImpossibleEdgePathIdentity(causeDigest, edge.ID, reservation.ID)
		if err != nil {
			return exclusiveRouteDraft{}, err
		}
		after.Paths[pathID] = PathRecord{
			ID: pathID, Kind: PathImpossibleEdge, State: PathImpossible, ParentPathID: source.ID,
			SourceActivation: source.SourceActivation, Edge: cloneEdge(&edge), TargetReservationID: reservation.ID,
			CandidateID: candidate.ID, ScopeID: source.ScopeID, BranchEdgeID: source.BranchEdgeID,
			CandidateLineage: lineage, CandidateLineageID: lineageID, LineageDepth: uint32(len(lineage)),
			ImpossibleCauseDigest: causeDigest, CreatedSeq: eventSeq, UpdatedSeq: eventSeq,
		}
		produced = append(produced, pathID)
		allCauseIDs = append(allCauseIDs, causeID)
	}
	slices.Sort(produced)
	slices.Sort(allCauseIDs)
	causeDigest, err := CauseSetIdentity(allCauseIDs)
	if err != nil {
		return exclusiveRouteDraft{}, err
	}
	parent := after.Paths[source.ID]
	parent.State = PathRouted
	parent.ProducedPathIDs = append([]PathID(nil), produced...)
	parent.UpdatedSeq = eventSeq
	dispositionID, err := DispositionReceiptIdentity(parent.ID, PathLive, PathRouted, "exclusive_route", MutationCommandPlaceholder, "", uint64(eventSeq))
	if err != nil {
		return exclusiveRouteDraft{}, err
	}
	parent.Disposition = &DispositionReceipt{ID: dispositionID, PathID: parent.ID, FromState: PathLive, ToState: PathRouted, ReasonCode: "exclusive_route", CommandID: MutationCommandPlaceholder, EventSeq: eventSeq}
	after.Paths[parent.ID] = parent

	batch, err := NewMutationBatch(&before, &after, eventSeq)
	if err != nil {
		return exclusiveRouteDraft{}, err
	}
	view.Authority = authority
	view.Commands = cloneMap(view.Commands)
	view.SideEffects = cloneMap(view.SideEffects)
	perform, settle, attemptEffect, err := observedAttemptCommands(view, sourceReservation.NodeID, node, source, observation)
	if err != nil {
		return exclusiveRouteDraft{}, err
	}
	if err := insertExactCommand(view.Commands, perform); err != nil {
		return exclusiveRouteDraft{}, err
	}
	if err := insertExactCommand(view.Commands, settle); err != nil {
		return exclusiveRouteDraft{}, err
	}
	view.SideEffects[attemptEffect.ID] = attemptEffect
	view.Routing = &before
	replayView := MutationReplayView{Aggregate: view, Checkpoint: input.binding}
	plan := RoutePathsPlan{
		SettlementCommandID: settle.ID, SourceActivationID: source.SourceActivation.ID,
		SourceGeneration: source.SourceActivation.Generation, SourcePathID: source.ID,
		Attempt: observation.Attempt, CauseDigest: causeDigest, ResultCode: "exclusive/" + observation.Outcome,
		ProducedPathIDs: produced, Batch: batch,
	}
	payload, err := EncodeRoutePathsPayload(replayView, plan)
	if err != nil {
		return exclusiveRouteDraft{}, err
	}
	identity := CommandIdentity{
		RunID: view.RunID, Kind: CommandRoutePaths, PayloadSchema: 1,
		SourceActivationID: source.SourceActivation.ID, SourceGeneration: source.SourceActivation.Generation,
		SourcePathID: source.ID, Attempt: observation.Attempt, InputDigest: settle.ID,
		CauseDigest: causeDigest, PlanDigest: payloadDigest(payload), ResultCode: plan.ResultCode,
	}
	command, err := observedCommand(identity, payload)
	if err != nil {
		return exclusiveRouteDraft{}, err
	}
	if err := insertExactCommand(replayView.Aggregate.Commands, command); err != nil {
		return exclusiveRouteDraft{}, err
	}
	if _, err := ReplayRoutePaths(replayView, command); err != nil {
		return exclusiveRouteDraft{}, err
	}
	return exclusiveRouteDraft{
		view: replayView, command: command, eventSeq: eventSeq, sourcePathID: source.ID,
		outgoing: outgoing, selectedEdge: outgoing[selectedIndex].ID, producedPaths: produced,
	}, nil
}

func buildExclusiveClosureDraft(ctx context.Context, input *VerifiedExclusiveInput, observation ExclusiveObservation, routeCommand CommandRecord) (exclusiveClosureDraft, error) {
	if err := ctx.Err(); err != nil {
		return exclusiveClosureDraft{}, err
	}
	route, err := buildExclusiveRouteDraft(ctx, input, observation)
	if err != nil {
		return exclusiveClosureDraft{}, err
	}
	if !exactExclusiveCommand(route.command, routeCommand) {
		return exclusiveClosureDraft{}, fmt.Errorf("%w: supplied predecessor route differs from deterministic plan", ErrMutationInvalid)
	}
	routed, err := ReplayRoutePaths(route.view, route.command)
	if err != nil {
		return exclusiveClosureDraft{}, err
	}
	post := route.view.Aggregate
	post.Routing = &routed.Routing

	parent := routed.Routing.Paths[route.sourcePathID]
	var arrived, impossible PathRecord
	for _, childID := range parent.ProducedPathIDs {
		child := routed.Routing.Paths[childID]
		switch child.Kind {
		case PathEdge:
			if child.State == PathArrived {
				if arrived.ID != "" {
					return exclusiveClosureDraft{}, fmt.Errorf("%w: local merge has multiple selected arrivals", ErrExclusiveUnsupported)
				}
				arrived = child
			}
		case PathImpossibleEdge:
			if child.State == PathImpossible {
				if impossible.ID != "" {
					return exclusiveClosureDraft{}, fmt.Errorf("%w: first DPE slice requires exactly one impossible sibling", ErrExclusiveUnsupported)
				}
				impossible = child
			}
		}
	}
	if arrived.ID == "" {
		return exclusiveClosureDraft{}, fmt.Errorf("%w: exclusive route lacks selected arrival", ErrExclusiveUnsupported)
	}
	if impossible.ID == "" {
		return exclusiveClosureDraft{}, fmt.Errorf("%w: selected route has no impossible sibling", ErrExclusiveUnsupported)
	}
	reservation, ok := routed.Routing.Reservations[impossible.TargetReservationID]
	if !ok || reservation.JoinPolicy != JoinExclusive || reservation.State != ReservationOpen {
		return exclusiveClosureDraft{}, fmt.Errorf("%w: local exclusive reservation is absent or closed", ErrMutationInconsistent)
	}
	candidate, ok := candidateForID(reservation, impossible.CandidateID)
	if !ok {
		return exclusiveClosureDraft{}, fmt.Errorf("%w: impossible sibling candidate is not reserved", ErrMutationInconsistent)
	}
	set, ok := routed.Routing.CauseSets[impossible.ImpossibleCauseDigest]
	if !ok || len(set.CauseIDs) == 0 {
		return exclusiveClosureDraft{}, fmt.Errorf("%w: impossible sibling lacks complete cause provenance", ErrMutationInconsistent)
	}
	kinds := make([]TerminalKind, len(set.CauseIDs))
	for index, causeID := range set.CauseIDs {
		cause, exists := routed.Routing.CauseRecords[causeID]
		if !exists {
			return exclusiveClosureDraft{}, fmt.Errorf("%w: impossible cause %q is absent", ErrMutationInconsistent, causeID)
		}
		kinds[index] = cause.TerminalKind
	}
	settled := make(map[PossibleSlotID]SlotSettlement, len(candidate.PossibleSlotIDs))
	for _, slotID := range candidate.PossibleSlotIDs {
		settled[slotID] = SlotSettlement{CauseIDs: cloneSlice(set.CauseIDs), CauseKinds: cloneSlice(kinds)}
	}
	entry, causeIDs, terminal, err := FoldCandidateSlots(reservation.ID, candidate, settled, false)
	if err != nil {
		return exclusiveClosureDraft{}, err
	}
	if entry.FoldKind == CandidateFoldOpen || entry.FoldKind == "arrived" || !slices.Equal(causeIDs, set.CauseIDs) {
		return exclusiveClosureDraft{}, fmt.Errorf("%w: impossible candidate does not fold to its exact complete causes", ErrMutationInconsistent)
	}

	eventSeq := route.eventSeq + 1
	before := Clone(routed.Routing)
	after := Clone(before)
	closureKey, err := CandidateClosureKeyIdentity(reservation.ID, candidate.ID)
	if err != nil {
		return exclusiveClosureDraft{}, err
	}
	after.CandidateClosures[closureKey] = CandidateClosure{
		ID:           entry.PathOrClosureID,
		Key:          CandidateClosureKeyRecord{ID: closureKey, ReservationID: reservation.ID, CandidateID: candidate.ID},
		TerminalKind: terminal, CauseDigest: set.Digest, CommandID: MutationCommandPlaceholder, EventSeq: eventSeq,
	}
	frontier := []CandidateClosureKey{closureKey}
	planDigest, err := PropagationPlanIdentity(reservation.ID, candidate.ID, set.Digest, 0, frontier)
	if err != nil {
		return exclusiveClosureDraft{}, err
	}
	intentID, err := PropagationIntentIdentity(set.Digest, 0, planDigest)
	if err != nil {
		return exclusiveClosureDraft{}, err
	}
	intent := PropagationIntent{
		ID: intentID, RootReservationID: reservation.ID, RootCandidateID: candidate.ID, RootCauseDigest: set.Digest,
		Shard: 0, Cursor: 1, Frontier: frontier, PlanDigest: planDigest, State: PropagationComplete,
		CommandID: MutationCommandPlaceholder, EventSeq: eventSeq,
	}
	after.Propagation[intent.ID] = intent
	batch, err := NewMutationBatch(&before, &after, eventSeq)
	if err != nil {
		return exclusiveClosureDraft{}, err
	}
	post.Routing = &before
	replayView := MutationReplayView{Aggregate: post, Checkpoint: input.binding}
	plan := PropagateClosurePlan{
		SourcePathID: impossible.ID, TargetReservationID: reservation.ID, TargetGeneration: reservation.Generation,
		InputDigest: planDigest, CauseDigest: set.Digest,
		RootReservationID: reservation.ID, RootCandidateID: candidate.ID, RootCauseDigest: set.Digest,
		Intents: []PropagationIntent{intent}, Batch: batch,
	}
	payload, err := EncodePropagateClosurePayload(replayView, plan)
	if err != nil {
		return exclusiveClosureDraft{}, err
	}
	identity := CommandIdentity{
		RunID: post.RunID, Kind: CommandPropagateCandidateClosure, PayloadSchema: 1,
		SourcePathID: impossible.ID, TargetReservationID: reservation.ID, TargetGeneration: reservation.Generation,
		InputDigest: planDigest, CauseDigest: set.Digest, PlanDigest: payloadDigest(payload),
	}
	command, err := observedCommand(identity, payload)
	if err != nil {
		return exclusiveClosureDraft{}, err
	}
	if err := insertExactCommand(replayView.Aggregate.Commands, command); err != nil {
		return exclusiveClosureDraft{}, err
	}
	if _, err := ReplayPropagateClosure(replayView, command); err != nil {
		return exclusiveClosureDraft{}, err
	}
	return exclusiveClosureDraft{view: replayView, command: command, eventSeq: eventSeq, preClosureCount: len(before.CandidateClosures)}, nil
}

func buildExclusiveDeadReservationDraft(ctx context.Context, input *VerifiedExclusiveInput, observation ExclusiveObservation, routeCommand, closureCommand CommandRecord) (exclusiveDeadReservationDraft, bool, error) {
	closure, err := buildExclusiveClosureDraft(ctx, input, observation, routeCommand)
	if err != nil {
		return exclusiveDeadReservationDraft{}, false, err
	}
	if !exactExclusiveCommand(closure.command, closureCommand) {
		return exclusiveDeadReservationDraft{}, false, fmt.Errorf("%w: supplied predecessor closure differs from deterministic plan", ErrMutationInvalid)
	}
	closed, err := ReplayPropagateClosure(closure.view, closure.command)
	if err != nil {
		return exclusiveDeadReservationDraft{}, false, err
	}
	post := closure.view.Aggregate
	post.Routing = &closed.Routing
	parent := closed.Routing.Paths[observation.SourcePathID]
	var arrived, impossible PathRecord
	for _, childID := range parent.ProducedPathIDs {
		child := closed.Routing.Paths[childID]
		if child.Kind == PathEdge && child.State == PathArrived {
			arrived = child
		}
		if child.Kind == PathImpossibleEdge && child.State == PathImpossible {
			impossible = child
		}
	}
	if arrived.ID == "" || impossible.ID == "" {
		return exclusiveDeadReservationDraft{}, false, fmt.Errorf("%w: routed sibling set is incomplete", ErrMutationInconsistent)
	}
	if arrived.TargetReservationID == impossible.TargetReservationID {
		return exclusiveDeadReservationDraft{}, false, nil
	}
	reservation := closed.Routing.Reservations[impossible.TargetReservationID]
	if reservation.State != ReservationOpen || reservation.JoinPolicy != JoinExclusive {
		return exclusiveDeadReservationDraft{}, false, fmt.Errorf("%w: dead sibling reservation is unavailable", ErrMutationInconsistent)
	}
	for _, candidate := range reservation.Candidates {
		key, err := CandidateClosureKeyIdentity(reservation.ID, candidate.ID)
		if err != nil {
			return exclusiveDeadReservationDraft{}, false, err
		}
		if _, ok := closed.Routing.CandidateClosures[key]; !ok {
			return exclusiveDeadReservationDraft{}, false, fmt.Errorf("%w: dead sibling reservation retains an open candidate", ErrExclusiveNotRoutable)
		}
	}
	fold, arrivals, leafDigest, err := activationFold(closed.Routing, reservation)
	if err != nil {
		return exclusiveDeadReservationDraft{}, false, err
	}
	if len(arrivals) != 0 {
		return exclusiveDeadReservationDraft{}, false, fmt.Errorf("%w: dead sibling reservation has an arrival", ErrMutationInconsistent)
	}
	leafSet, ok := closed.Routing.CauseSets[leafDigest]
	if !ok || len(leafSet.CauseIDs) == 0 {
		return exclusiveDeadReservationDraft{}, false, fmt.Errorf("%w: dead sibling fold lacks complete causes", ErrMutationInconsistent)
	}
	kinds := make([]TerminalKind, 0, len(leafSet.CauseIDs))
	for _, causeID := range leafSet.CauseIDs {
		kinds = append(kinds, closed.Routing.CauseRecords[causeID].TerminalKind)
	}
	terminal, err := FoldTerminalKinds(kinds)
	if err != nil {
		return exclusiveDeadReservationDraft{}, false, err
	}
	eventSeq := closure.eventSeq + 1
	joinCauseID, err := CauseIdentity("", terminal, "join_all_impossible", "", MutationCommandPlaceholder, "", uint64(eventSeq))
	if err != nil {
		return exclusiveDeadReservationDraft{}, false, err
	}
	finalCauseIDs := append(cloneSlice(leafSet.CauseIDs), joinCauseID)
	slices.Sort(finalCauseIDs)
	finalDigest, err := CauseSetIdentity(finalCauseIDs)
	if err != nil {
		return exclusiveDeadReservationDraft{}, false, err
	}
	inputDigest, err := InputSetIdentity(nil)
	if err != nil {
		return exclusiveDeadReservationDraft{}, false, err
	}
	receiptID, err := ActivationReceiptIdentity("", reservation.ID, inputDigest, "", MutationCommandPlaceholder, uint64(eventSeq))
	if err != nil {
		return exclusiveDeadReservationDraft{}, false, err
	}
	before := Clone(closed.Routing)
	after := Clone(before)
	after.CauseRecords[joinCauseID] = CauseRecord{
		ID: joinCauseID, TerminalKind: terminal, DispositionReason: "join_all_impossible",
		SourceCommandID: MutationCommandPlaceholder, EventSeq: eventSeq,
	}
	after.CauseSets[finalDigest] = CauseSetRecord{Digest: finalDigest, CauseIDs: finalCauseIDs}
	dead := after.Reservations[reservation.ID]
	dead.State = ReservationClosedNoActivation
	dead.CloseReceipt = &ActivationReceipt{
		ID: receiptID, ReservationID: reservation.ID, InputSetDigest: inputDigest,
		ScopeID: reservation.ScopeID, BranchEdgeID: reservation.BranchEdgeID,
		JoinPolicy: reservation.JoinPolicy, Result: ReceiptClosedNoActivation,
		CauseDigest: finalDigest, CommandID: MutationCommandPlaceholder, EventSeq: eventSeq,
	}
	dead.ClosedReason = string(ScopeCloseAllImpossible)
	dead.CauseDigest = finalDigest
	dead.CommandID = MutationCommandPlaceholder
	dead.EventSeq = eventSeq
	after.Reservations[dead.ID] = dead
	batch, err := NewMutationBatch(&before, &after, eventSeq)
	if err != nil {
		return exclusiveDeadReservationDraft{}, false, err
	}
	post.Routing = &before
	replayView := MutationReplayView{Aggregate: post, Checkpoint: input.binding}
	plan := ActivateGenerationPlan{
		ReservationID: reservation.ID, Generation: reservation.Generation, InputDigest: fold, CauseDigest: leafDigest,
		JoinPolicy: JoinExclusive, InputPathIDs: []PathID{}, Candidates: cloneCandidates(reservation.Candidates),
		PossibleSlots: cloneSlice(reservation.PossibleSlots), Batch: batch,
	}
	payload, err := EncodeActivateGenerationPayload(replayView, plan)
	if err != nil {
		return exclusiveDeadReservationDraft{}, false, err
	}
	identity := CommandIdentity{
		RunID: post.RunID, Kind: CommandActivateGeneration, PayloadSchema: 1,
		TargetReservationID: reservation.ID, TargetGeneration: reservation.Generation,
		InputDigest: fold, CauseDigest: leafDigest, PlanDigest: payloadDigest(payload),
	}
	command, err := observedCommand(identity, payload)
	if err != nil {
		return exclusiveDeadReservationDraft{}, false, err
	}
	if err := insertExactCommand(replayView.Aggregate.Commands, command); err != nil {
		return exclusiveDeadReservationDraft{}, false, err
	}
	if _, err := ReplayActivateGeneration(replayView, command); err != nil {
		return exclusiveDeadReservationDraft{}, false, err
	}
	return exclusiveDeadReservationDraft{view: replayView, command: command, eventSeq: eventSeq}, true, nil
}

func buildExclusiveActivationDraft(ctx context.Context, input *VerifiedExclusiveInput, observation ExclusiveObservation, routeCommand, closureCommand, deadReservationCommand CommandRecord) (exclusiveActivationDraft, error) {
	if err := ctx.Err(); err != nil {
		return exclusiveActivationDraft{}, err
	}
	route, err := buildExclusiveRouteDraft(ctx, input, observation)
	if err != nil {
		return exclusiveActivationDraft{}, err
	}
	if !exactExclusiveCommand(route.command, routeCommand) {
		return exclusiveActivationDraft{}, fmt.Errorf("%w: supplied predecessor route differs from deterministic plan", ErrMutationInvalid)
	}
	routed, err := ReplayRoutePaths(route.view, route.command)
	if err != nil {
		return exclusiveActivationDraft{}, err
	}
	post := route.view.Aggregate
	post.Routing = &routed.Routing
	eventSeq := route.eventSeq + 1
	if closureCommand.ID != "" {
		closure, err := buildExclusiveClosureDraft(ctx, input, observation, routeCommand)
		if err != nil {
			return exclusiveActivationDraft{}, err
		}
		if !exactExclusiveCommand(closure.command, closureCommand) {
			return exclusiveActivationDraft{}, fmt.Errorf("%w: supplied predecessor closure differs from deterministic plan", ErrMutationInvalid)
		}
		closed, err := ReplayPropagateClosure(closure.view, closure.command)
		if err != nil {
			return exclusiveActivationDraft{}, err
		}
		post = closure.view.Aggregate
		post.Routing = &closed.Routing
		eventSeq = closure.eventSeq + 1
		dead, required, err := buildExclusiveDeadReservationDraft(ctx, input, observation, routeCommand, closureCommand)
		if err != nil {
			return exclusiveActivationDraft{}, err
		}
		if required {
			if !exactExclusiveCommand(dead.command, deadReservationCommand) {
				return exclusiveActivationDraft{}, fmt.Errorf("%w: required dead-reservation command differs from deterministic plan", ErrMutationInvalid)
			}
			settled, err := ReplayActivateGeneration(dead.view, dead.command)
			if err != nil {
				return exclusiveActivationDraft{}, err
			}
			post = dead.view.Aggregate
			post.Routing = &settled.Routing
			eventSeq = dead.eventSeq + 1
		} else if deadReservationCommand.ID != "" {
			return exclusiveActivationDraft{}, fmt.Errorf("%w: unexpected dead-reservation command", ErrMutationInvalid)
		}
	} else if deadReservationCommand.ID != "" {
		return exclusiveActivationDraft{}, fmt.Errorf("%w: dead-reservation command lacks closure predecessor", ErrMutationInvalid)
	}

	parent := post.Routing.Paths[route.sourcePathID]
	var selected PathRecord
	for _, childID := range parent.ProducedPathIDs {
		child := post.Routing.Paths[childID]
		if child.Kind == PathEdge && child.State == PathArrived {
			if selected.ID != "" {
				return exclusiveActivationDraft{}, fmt.Errorf("%w: exclusive route has multiple selected arrivals", ErrMutationInconsistent)
			}
			selected = child
		}
	}
	if selected.ID == "" {
		return exclusiveActivationDraft{}, fmt.Errorf("%w: exclusive route has no selected arrival", ErrMutationInconsistent)
	}
	reservation, ok := post.Routing.Reservations[selected.TargetReservationID]
	if !ok || reservation.State != ReservationOpen || reservation.JoinPolicy != JoinExclusive {
		return exclusiveActivationDraft{}, fmt.Errorf("%w: selected exclusive reservation is unavailable", ErrMutationInconsistent)
	}
	fold, arrivals, causeDigest, err := activationFold(*post.Routing, reservation)
	if err != nil {
		return exclusiveActivationDraft{}, err
	}
	if len(arrivals) != 1 || arrivals[0] != selected.ID {
		return exclusiveActivationDraft{}, fmt.Errorf("%w: exclusive activation must conserve exactly the selected arrival", ErrMutationInconsistent)
	}
	for _, candidate := range reservation.Candidates {
		if candidate.ID == selected.CandidateID {
			continue
		}
		key, err := CandidateClosureKeyIdentity(reservation.ID, candidate.ID)
		if err != nil {
			return exclusiveActivationDraft{}, err
		}
		if _, ok := post.Routing.CandidateClosures[key]; !ok {
			return exclusiveActivationDraft{}, fmt.Errorf("%w: exclusive merge retains an open candidate", ErrExclusiveNotRoutable)
		}
	}
	inputDigest, err := InputSetIdentity(arrivals)
	if err != nil {
		return exclusiveActivationDraft{}, err
	}
	activationID, err := ActivationIdentity(post.RunID, reservation.ID, reservation.Generation, inputDigest)
	if err != nil {
		return exclusiveActivationDraft{}, err
	}
	outputID, err := ActivationOutputIdentity(activationID, reservation.Generation)
	if err != nil {
		return exclusiveActivationDraft{}, err
	}
	ref := ActivationRef{ID: activationID, Generation: reservation.Generation}
	before := Clone(*post.Routing)
	after := Clone(before)
	frames, lineageID, err := PopConsumedLineage([]PathRecord{selected}, reservation.ID)
	if err != nil {
		return exclusiveActivationDraft{}, err
	}
	consumed := after.Paths[selected.ID]
	consumed.State = PathConsumed
	consumed.ConsumedBy = &ref
	consumed.UpdatedSeq = eventSeq
	dispositionID, err := DispositionReceiptIdentity(consumed.ID, PathArrived, PathConsumed, "exclusive_input", MutationCommandPlaceholder, "", uint64(eventSeq))
	if err != nil {
		return exclusiveActivationDraft{}, err
	}
	consumed.Disposition = &DispositionReceipt{ID: dispositionID, PathID: consumed.ID, FromState: PathArrived, ToState: PathConsumed, ReasonCode: "exclusive_input", CommandID: MutationCommandPlaceholder, EventSeq: eventSeq}
	after.Paths[consumed.ID] = consumed
	after.Paths[outputID] = PathRecord{
		ID: outputID, Kind: PathActivationOutput, State: PathLive, SourceActivation: ref,
		ScopeID: reservation.ScopeID, BranchEdgeID: reservation.BranchEdgeID,
		CandidateLineage: frames, CandidateLineageID: lineageID, LineageDepth: uint32(len(frames)),
		CreatedSeq: eventSeq, UpdatedSeq: eventSeq,
	}
	receiptID, err := ActivationReceiptIdentity(activationID, reservation.ID, inputDigest, outputID, MutationCommandPlaceholder, uint64(eventSeq))
	if err != nil {
		return exclusiveActivationDraft{}, err
	}
	receipt := ActivationReceipt{
		ID: receiptID, ActivationID: activationID, ReservationID: reservation.ID, InputSetDigest: inputDigest,
		OutputPathID: outputID, ScopeID: reservation.ScopeID, BranchEdgeID: reservation.BranchEdgeID,
		JoinPolicy: JoinExclusive, Result: ReceiptActivated, CommandID: MutationCommandPlaceholder, EventSeq: eventSeq,
	}
	after.Activations[activationID] = ActivationRecord{
		ID: activationID, RunID: post.RunID, Ref: ref, ReservationID: reservation.ID,
		InputPathIDs: cloneSlice(arrivals), InputSetDigest: inputDigest, OutputPathID: outputID,
		Receipt: receipt, CommandID: MutationCommandPlaceholder, EventSeq: eventSeq,
	}
	activated := after.Reservations[reservation.ID]
	activated.State = ReservationActivated
	activated.Activation = &ref
	activated.CommandID = MutationCommandPlaceholder
	activated.EventSeq = eventSeq
	after.Reservations[activated.ID] = activated
	batch, err := NewMutationBatch(&before, &after, eventSeq)
	if err != nil {
		return exclusiveActivationDraft{}, err
	}
	post.Routing = &before
	replayView := MutationReplayView{Aggregate: post, Checkpoint: input.binding}
	plan := ActivateGenerationPlan{
		ReservationID: reservation.ID, Generation: reservation.Generation, InputDigest: fold, CauseDigest: causeDigest,
		JoinPolicy: JoinExclusive, InputPathIDs: cloneSlice(arrivals),
		Candidates: cloneCandidates(reservation.Candidates), PossibleSlots: cloneSlice(reservation.PossibleSlots), Batch: batch,
	}
	payload, err := EncodeActivateGenerationPayload(replayView, plan)
	if err != nil {
		return exclusiveActivationDraft{}, err
	}
	identity := CommandIdentity{
		RunID: post.RunID, Kind: CommandActivateGeneration, PayloadSchema: 1,
		TargetReservationID: reservation.ID, TargetGeneration: reservation.Generation,
		InputDigest: fold, CauseDigest: causeDigest, PlanDigest: payloadDigest(payload),
	}
	command, err := observedCommand(identity, payload)
	if err != nil {
		return exclusiveActivationDraft{}, err
	}
	if err := insertExactCommand(replayView.Aggregate.Commands, command); err != nil {
		return exclusiveActivationDraft{}, err
	}
	if _, err := ReplayActivateGeneration(replayView, command); err != nil {
		return exclusiveActivationDraft{}, err
	}
	return exclusiveActivationDraft{view: replayView, command: command, eventSeq: eventSeq}, nil
}

func buildExclusiveEndDraft(ctx context.Context, input *VerifiedExclusiveInput, observation ExclusiveObservation, routeCommand, closureCommand, deadReservationCommand, activationCommand CommandRecord) (exclusiveEndDraft, error) {
	activation, err := buildExclusiveActivationDraft(ctx, input, observation, routeCommand, closureCommand, deadReservationCommand)
	if err != nil {
		return exclusiveEndDraft{}, err
	}
	if !exactExclusiveCommand(activation.command, activationCommand) {
		return exclusiveEndDraft{}, fmt.Errorf("%w: supplied predecessor activation differs from deterministic plan", ErrMutationInvalid)
	}
	activated, err := ReplayActivateGeneration(activation.view, activation.command)
	if err != nil {
		return exclusiveEndDraft{}, err
	}
	post := activation.view.Aggregate
	post.Routing = &activated.Routing
	reservationID := activation.command.Identity.TargetReservationID
	reservation := activated.Routing.Reservations[reservationID]
	node, ok := input.template.Nodes[reservation.NodeID]
	if !ok || node.Type != model.NodeTypeEnd {
		return exclusiveEndDraft{}, fmt.Errorf("%w: activated target is not an end node", ErrExclusiveUnsupported)
	}
	result := strings.ToLower(strings.TrimSpace(node.Result))
	if result != "" && result != "pass" && result != "success" && result != "completed" && result != "complete" {
		return exclusiveEndDraft{}, fmt.Errorf("%w: terminal result %q requires non-success authority outside this slice", ErrExclusiveUnsupported, node.Result)
	}
	activationRecord := activated.Routing.Activations[reservation.Activation.ID]
	output := activated.Routing.Paths[activationRecord.OutputPathID]
	if output.Kind != PathActivationOutput || output.State != PathLive {
		return exclusiveEndDraft{}, fmt.Errorf("%w: end activation output is not live", ErrMutationInconsistent)
	}
	eventSeq := activation.eventSeq + 1
	before := Clone(activated.Routing)
	after := Clone(before)
	ended := after.Paths[output.ID]
	ended.State = PathEnded
	ended.UpdatedSeq = eventSeq
	dispositionID, err := DispositionReceiptIdentity(ended.ID, PathLive, PathEnded, "completed", MutationCommandPlaceholder, "", uint64(eventSeq))
	if err != nil {
		return exclusiveEndDraft{}, err
	}
	ended.Disposition = &DispositionReceipt{ID: dispositionID, PathID: ended.ID, FromState: PathLive, ToState: PathEnded, ReasonCode: "completed", CommandID: MutationCommandPlaceholder, EventSeq: eventSeq}
	after.Paths[ended.ID] = ended
	batch, err := NewMutationBatch(&before, &after, eventSeq)
	if err != nil {
		return exclusiveEndDraft{}, err
	}
	endObservation := ExclusiveObservation{SourcePathID: output.ID, Attempt: 1, Outcome: "pass"}
	post.Commands = cloneMap(post.Commands)
	post.SideEffects = cloneMap(post.SideEffects)
	perform, settle, effect, err := observedAttemptCommands(post, reservation.NodeID, node, output, endObservation)
	if err != nil {
		return exclusiveEndDraft{}, err
	}
	if err := insertExactCommand(post.Commands, perform); err != nil {
		return exclusiveEndDraft{}, err
	}
	if err := insertExactCommand(post.Commands, settle); err != nil {
		return exclusiveEndDraft{}, err
	}
	post.SideEffects[effect.ID] = effect
	post.Routing = &before
	replayView := MutationReplayView{Aggregate: post, Checkpoint: input.binding}
	emptyCause, err := CauseSetIdentity(nil)
	if err != nil {
		return exclusiveEndDraft{}, err
	}
	plan := RoutePathsPlan{
		SettlementCommandID: settle.ID, SourceActivationID: output.SourceActivation.ID,
		SourceGeneration: output.SourceActivation.Generation, SourcePathID: output.ID,
		Attempt: 1, CauseDigest: emptyCause, ResultCode: "pass", ProducedPathIDs: []PathID{}, Batch: batch,
	}
	payload, err := EncodeRoutePathsPayload(replayView, plan)
	if err != nil {
		return exclusiveEndDraft{}, err
	}
	identity := CommandIdentity{
		RunID: post.RunID, Kind: CommandRoutePaths, PayloadSchema: 1,
		SourceActivationID: output.SourceActivation.ID, SourceGeneration: output.SourceActivation.Generation,
		SourcePathID: output.ID, Attempt: 1, InputDigest: settle.ID,
		CauseDigest: emptyCause, PlanDigest: payloadDigest(payload), ResultCode: "pass",
	}
	command, err := observedCommand(identity, payload)
	if err != nil {
		return exclusiveEndDraft{}, err
	}
	if err := insertExactCommand(replayView.Aggregate.Commands, command); err != nil {
		return exclusiveEndDraft{}, err
	}
	if _, err := ReplayRoutePaths(replayView, command); err != nil {
		return exclusiveEndDraft{}, err
	}
	return exclusiveEndDraft{view: replayView, command: command, eventSeq: eventSeq}, nil
}

func buildExclusiveCompletionView(ctx context.Context, input *VerifiedExclusiveInput, observation ExclusiveObservation, routeCommand, closureCommand, deadReservationCommand, activationCommand, endCommand CommandRecord, completion ExclusiveCompletionInput) (CompletionReplayView, error) {
	if err := ctx.Err(); err != nil {
		return CompletionReplayView{}, err
	}
	end, err := buildExclusiveEndDraft(ctx, input, observation, routeCommand, closureCommand, deadReservationCommand, activationCommand)
	if err != nil {
		return CompletionReplayView{}, err
	}
	if !exactExclusiveCommand(end.command, endCommand) {
		return CompletionReplayView{}, fmt.Errorf("%w: supplied predecessor end differs from deterministic plan", ErrMutationInvalid)
	}
	ended, err := ReplayRoutePaths(end.view, end.command)
	if err != nil {
		return CompletionReplayView{}, err
	}
	aggregate := end.view.Aggregate
	aggregate.Routing = &ended.Routing
	view := CompletionReplayView{
		Aggregate: aggregate, CheckpointJSON: bytes.Clone(completion.CheckpointJSON),
		RunStatus: completion.RunStatus, LastLogSeq: completion.LastLogSeq, LogChecksum: completion.LogChecksum,
	}
	basis, err := computeCompletionBasis(view, CompletionBasis{
		BasisRunStatus: completion.RunStatus, BasisLastLogSeq: completion.LastLogSeq, BasisLogChecksum: completion.LogChecksum,
	}, "")
	if err != nil {
		return CompletionReplayView{}, err
	}
	view.Checkpoint = completionBasisCheckpoint(basis)
	return view, nil
}

func classifyExclusiveObservation(view AggregateView, tmpl *model.Template, observation ExclusiveObservation) (ExclusiveDisposition, error) {
	if tmpl == nil || observation.SourcePathID == "" || observation.Attempt == 0 || strings.TrimSpace(observation.Outcome) == "" {
		return "", fmt.Errorf("%w: incomplete exclusive observation", ErrMutationInvalid)
	}
	source, ok := view.Routing.Paths[observation.SourcePathID]
	if !ok || source.Kind != PathActivationOutput || source.State != PathLive {
		return "", fmt.Errorf("%w: source path is not a live activation output", ErrMutationInconsistent)
	}
	activation, ok := view.Routing.Activations[source.SourceActivation.ID]
	if !ok || activation.Ref != source.SourceActivation {
		return "", fmt.Errorf("%w: source activation is missing or mismatched", ErrMutationInconsistent)
	}
	reservation, ok := view.Routing.Reservations[activation.ReservationID]
	if !ok {
		return "", fmt.Errorf("%w: source reservation is missing", ErrMutationInconsistent)
	}
	node, ok := tmpl.Nodes[reservation.NodeID]
	if !ok {
		return "", fmt.Errorf("%w: source node is absent from exact template", ErrExclusiveInputInvalid)
	}
	if observation.ResolutionDigest != "" {
		var matched *BlockResolution
		for recordID, record := range view.AdminRecords {
			if record.ResolutionDigest != observation.ResolutionDigest {
				continue
			}
			resolution, exists := view.AdminResolutions[recordID]
			if !exists || matched != nil {
				return "", fmt.Errorf("%w: block resolution authority is missing or ambiguous", ErrMutationInconsistent)
			}
			copy := resolution
			matched = &copy
		}
		if matched == nil {
			return "", fmt.Errorf("%w: block resolution digest is absent", ErrMutationInconsistent)
		}
		digest, err := ValidateBlockResolution(*matched)
		if err != nil || digest != observation.ResolutionDigest || matched.NodeID != reservation.NodeID || matched.BlockedAttempt != observation.Attempt {
			return "", fmt.Errorf("%w: block resolution is not bound to this node generation", ErrMutationInvalid)
		}
		blockID, err := BlockIdentity(view.RunID, activation.ID, observation.Attempt)
		if err != nil {
			return "", err
		}
		effect, ok := view.SideEffects[blockID]
		if !ok || effect.Kind != SideEffectBlock || effect.State != "resolved_"+matched.Decision {
			return "", fmt.Errorf("%w: block resolution lacks exact aggregate side-effect outcome", ErrMutationInconsistent)
		}
		switch matched.Decision {
		case "retry":
			return ExclusiveResolvedRetry, nil
		case "skip":
			return ExclusiveResolvedSkip, nil
		case "cancel":
			return ExclusiveResolvedCancel, nil
		}
	}
	if node.Type == model.NodeTypeWait && strings.ToLower(strings.TrimSpace(observation.Outcome)) != "satisfied" {
		return "", fmt.Errorf("%w: wait observation is not satisfied", ErrExclusiveNotRoutable)
	}
	if node.Type == model.NodeTypeTask && isFailOutcome(strings.ToLower(strings.TrimSpace(observation.Outcome))) && observation.Attempt < uint64(model.RetryBudget(node.Retry)) {
		return ExclusiveRetryPending, nil
	}
	return ExclusiveRouteReady, nil
}

func resolveExclusiveEdge(node model.Node, outcome string, outgoing []EdgeKey) (int, error) {
	if node.Type == model.NodeTypeDecision {
		for index := range outgoing {
			if outgoing[index].Outcome == outcome {
				return index, nil
			}
		}
		return -1, fmt.Errorf("%w: decision verdict %q has no exact edge", ErrExclusiveUnsupported, outcome)
	}
	trimmed := strings.ToLower(strings.TrimSpace(outcome))
	for index := range outgoing {
		if strings.ToLower(outgoing[index].Outcome) == trimmed {
			return index, nil
		}
	}
	target, err := resolveExclusiveTarget(node, trimmed)
	if err != nil {
		return -1, err
	}
	selected := -1
	for index := range outgoing {
		if outgoing[index].ToNodeID != target {
			continue
		}
		if selected >= 0 {
			return -1, fmt.Errorf("%w: fallback outcome has ambiguous outgoing authority", ErrExclusiveUnsupported)
		}
		selected = index
	}
	if selected < 0 {
		return -1, fmt.Errorf("%w: selected target is absent from exact outgoing topology", ErrExclusiveInputInvalid)
	}
	return selected, nil
}

func resolveExclusiveTarget(node model.Node, outcome string) (string, error) {
	outcome = strings.ToLower(strings.TrimSpace(outcome))
	switch node.Type {
	case model.NodeTypeStart, model.NodeTypeWait:
		return resolvePassTarget(node.Next, "pass")
	case model.NodeTypeTask:
		if isFailOutcome(outcome) {
			target := model.FailTarget(node.Next)
			if target == "" {
				return "", fmt.Errorf("%w: terminal task failure is outside the first routing slice", ErrExclusiveUnsupported)
			}
			return target, nil
		}
		return resolvePassTarget(node.Next, outcome)
	case model.NodeTypeDecision:
		return "", fmt.Errorf("%w: decision verdict %q has no exact edge", ErrExclusiveUnsupported, outcome)
	default:
		return "", fmt.Errorf("%w: node type %q", ErrExclusiveUnsupported, node.Type)
	}
}

func resolvePassTarget(next model.Next, outcome string) (string, error) {
	keys := append([]string{outcome, strings.ToLower(strings.TrimSpace(outcome))}, model.PassOutcomeLabels()...)
	for _, key := range keys {
		if target := next[key]; target != "" {
			return target, nil
		}
	}
	if len(next) == 1 {
		for _, target := range next {
			return target, nil
		}
	}
	return "", fmt.Errorf("%w: no unambiguous pass edge", ErrExclusiveUnsupported)
}

func isFailOutcome(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "fail", "failed", "failure", "error", "timeout", "rejected", "cancel", "canceled", "cancelled":
		return true
	default:
		return false
	}
}

func canonicalExclusiveOutcome(node model.Node, outcome string) (string, error) {
	outcome = strings.TrimSpace(outcome)
	if outcome == "" {
		return "", fmt.Errorf("%w: empty exclusive outcome", ErrMutationInvalid)
	}
	if node.Type == model.NodeTypeDecision {
		return outcome, nil
	}
	return strings.ToLower(outcome), nil
}

func exactOutgoingEdges(templateRef, nodeID string, next model.Next) ([]EdgeKey, error) {
	if len(next) == 0 || len(next) > MaxOutgoingOrAllCandidates {
		return nil, fmt.Errorf("%w: outgoing degree %d", ErrExclusiveInputInvalid, len(next))
	}
	edges := make([]EdgeKey, 0, len(next))
	for outcome, target := range next {
		edge := EdgeKey{TemplateRef: templateRef, FromNodeID: nodeID, Outcome: outcome, ToNodeID: target}
		var err error
		edge.ID, err = EdgeIdentity(templateRef, nodeID, outcome, target)
		if err != nil {
			return nil, err
		}
		edges = append(edges, edge)
	}
	slices.SortFunc(edges, func(a, b EdgeKey) int { return strings.Compare(a.ID, b.ID) })
	for index := 1; index < len(edges); index++ {
		if edges[index-1].ID == edges[index].ID {
			return nil, fmt.Errorf("%w: duplicate outgoing edge identity", ErrExclusiveInputInvalid)
		}
	}
	return edges, nil
}

func exactExclusiveReservation(tmpl *model.Template, view AggregateView, authority *AggregateAuthority, routing RoutingState, nodeID string, eventSeq int64) (ActivationReservation, bool, error) {
	root := authority.Genesis.RootScopeID
	reservationID, err := ReservationIdentity(view.RunID, nodeID, root, "", 1)
	if err != nil {
		return ActivationReservation{}, false, err
	}
	want, err := deriveExclusiveReservation(tmpl, view.RunID, view.TemplateRef, root, nodeID, eventSeq)
	if err != nil {
		return ActivationReservation{}, false, err
	}
	if current, ok := routing.Reservations[reservationID]; ok {
		static := current
		static.State, static.Activation, static.CloseReceipt, static.ClosedReason, static.CauseDigest, static.CommandID = ReservationOpen, nil, nil, "", "", ""
		static.EventSeq = eventSeq
		if !canonicalEqual(static, want) {
			return ActivationReservation{}, false, fmt.Errorf("%w: materialized reservation differs from exact template authority", ErrExclusiveInputInvalid)
		}
		return current, false, nil
	}
	return want, true, nil
}

func deriveExclusiveReservation(tmpl *model.Template, runID, templateRef, scopeID, nodeID string, eventSeq int64) (ActivationReservation, error) {
	if _, ok := tmpl.Nodes[nodeID]; !ok {
		return ActivationReservation{}, fmt.Errorf("%w: target node %q missing", ErrExclusiveInputInvalid, nodeID)
	}
	reservationID, err := ReservationIdentity(runID, nodeID, scopeID, "", 1)
	if err != nil {
		return ActivationReservation{}, err
	}
	inbound := make([]EdgeKey, 0)
	for fromID, node := range tmpl.Nodes {
		for outcome, target := range node.Next {
			if target != nodeID {
				continue
			}
			edge := EdgeKey{TemplateRef: templateRef, FromNodeID: fromID, Outcome: outcome, ToNodeID: target}
			edge.ID, err = EdgeIdentity(templateRef, fromID, outcome, target)
			if err != nil {
				return ActivationReservation{}, err
			}
			inbound = append(inbound, edge)
		}
	}
	if len(inbound) == 0 || len(inbound) > MaxOutgoingOrAllCandidates {
		return ActivationReservation{}, fmt.Errorf("%w: target %q inbound degree %d", ErrExclusiveInputInvalid, nodeID, len(inbound))
	}
	slices.SortFunc(inbound, func(a, b EdgeKey) int { return strings.Compare(a.ID, b.ID) })
	candidates := make([]CandidateRecord, 0, len(inbound))
	slots := make([]PossibleSlotRecord, 0, len(inbound))
	for _, edge := range inbound {
		candidateID, err := CandidateIdentity(reservationID, CandidateInboundEdge, edge.ID)
		if err != nil {
			return ActivationReservation{}, err
		}
		slotID, err := PossibleSlotIdentity(reservationID, candidateID, edge.FromNodeID, edge.ID, scopeID, "", 1)
		if err != nil {
			return ActivationReservation{}, err
		}
		candidates = append(candidates, CandidateRecord{ID: candidateID, Kind: CandidateInboundEdge, MemberID: edge.ID, PossibleSlotIDs: []PossibleSlotID{slotID}})
		slots = append(slots, PossibleSlotRecord{ID: slotID, ReservationID: reservationID, CandidateID: candidateID, SourceNodeID: edge.FromNodeID, SourceEdgeID: edge.ID, SourceScopeID: scopeID, Generation: 1})
	}
	slices.SortFunc(candidates, func(a, b CandidateRecord) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(slots, func(a, b PossibleSlotRecord) int { return strings.Compare(a.ID, b.ID) })
	return ActivationReservation{
		ID: reservationID, RunID: runID, NodeID: nodeID, ScopeID: scopeID, Generation: 1,
		JoinPolicy: JoinExclusive, Candidates: candidates, PossibleSlots: slots,
		State: ReservationOpen, EventSeq: eventSeq,
	}, nil
}

func reservationAuthority(value ActivationReservation) ReservationAuthority {
	return ReservationAuthority{
		ID: value.ID, NodeID: value.NodeID, ScopeID: value.ScopeID, BranchEdgeID: value.BranchEdgeID,
		Generation: value.Generation, JoinPolicy: value.JoinPolicy, IsReducing: value.IsReducing,
		ReducesScopeID: value.ReducesScopeID, Candidates: cloneCandidates(value.Candidates), PossibleSlots: cloneSlice(value.PossibleSlots),
	}
}

func candidateForEdge(reservation ActivationReservation, edgeID EdgeID) (CandidateRecord, bool) {
	for _, candidate := range reservation.Candidates {
		if candidate.Kind == CandidateInboundEdge && candidate.MemberID == edgeID {
			return candidate, true
		}
	}
	return CandidateRecord{}, false
}

func candidateForID(reservation ActivationReservation, candidateID CandidateID) (CandidateRecord, bool) {
	for _, candidate := range reservation.Candidates {
		if candidate.ID == candidateID {
			return candidate, true
		}
	}
	return CandidateRecord{}, false
}

func observedAttemptCommands(view AggregateView, nodeID string, node model.Node, source PathRecord, observation ExclusiveObservation) (CommandRecord, CommandRecord, SideEffectIdentity, error) {
	var existingPerform, existingSettle CommandRecord
	for _, candidate := range view.Commands {
		identity := candidate.Identity
		if identity.SourceActivationID != source.SourceActivation.ID || identity.SourceGeneration != source.SourceActivation.Generation || identity.Attempt != observation.Attempt {
			continue
		}
		switch identity.Kind {
		case CommandPerformAttempt:
			if existingPerform.ID != "" {
				return CommandRecord{}, CommandRecord{}, SideEffectIdentity{}, fmt.Errorf("%w: performer attempt has multiple source commands", ErrMutationInconsistent)
			}
			existingPerform = cloneCommandRecord(candidate)
		case CommandSettleAttempt:
			if existingSettle.ID != "" {
				return CommandRecord{}, CommandRecord{}, SideEffectIdentity{}, fmt.Errorf("%w: performer attempt has multiple settlement commands", ErrMutationInconsistent)
			}
			existingSettle = cloneCommandRecord(candidate)
		}
	}
	performPayload, err := json.Marshal(performAttemptPayload{
		TemplateRef: view.TemplateRef, TemplateSourceHash: view.TemplateSourceHash, NodeID: nodeID,
		SourceActivationID: source.SourceActivation.ID, SourceGeneration: source.SourceActivation.Generation, Attempt: observation.Attempt,
	})
	if err != nil {
		return CommandRecord{}, CommandRecord{}, SideEffectIdentity{}, err
	}
	performIdentity := CommandIdentity{
		RunID: view.RunID, Kind: CommandPerformAttempt, PayloadSchema: 1,
		SourceActivationID: source.SourceActivation.ID, SourceGeneration: source.SourceActivation.Generation,
		Attempt: observation.Attempt, PlanDigest: payloadDigest(performPayload),
	}
	perform := existingPerform
	if perform.ID == "" {
		perform, err = observedCommand(performIdentity, performPayload)
		if err != nil {
			return CommandRecord{}, CommandRecord{}, SideEffectIdentity{}, err
		}
	} else if perform.State != CommandObserved && perform.State != CommandReconciled {
		return CommandRecord{}, CommandRecord{}, SideEffectIdentity{}, fmt.Errorf("%w: performer attempt remains active", ErrMutationInconsistent)
	}
	outcome, err := canonicalExclusiveOutcome(node, observation.Outcome)
	if err != nil {
		return CommandRecord{}, CommandRecord{}, SideEffectIdentity{}, err
	}
	settlePayload, err := json.Marshal(settleAttemptObservationPayload{
		TemplateRef: view.TemplateRef, SourceCommandID: perform.ID, SourceActivationID: source.SourceActivation.ID,
		SourceGeneration: source.SourceActivation.Generation, Attempt: observation.Attempt, ResultCode: outcome,
		Actor: observation.Actor, EvidenceRef: observation.EvidenceRef, EvidenceHash: observation.EvidenceHash,
		ResolutionDigest: observation.ResolutionDigest, ExternalRef: observation.ExternalRef, Feedback: observation.Feedback,
	})
	if err != nil {
		return CommandRecord{}, CommandRecord{}, SideEffectIdentity{}, err
	}
	settleIdentity := CommandIdentity{
		RunID: view.RunID, Kind: CommandSettleAttempt, PayloadSchema: 1,
		SourceActivationID: source.SourceActivation.ID, SourceGeneration: source.SourceActivation.Generation,
		Attempt: observation.Attempt, InputDigest: perform.ID, PlanDigest: payloadDigest(settlePayload), ResultCode: outcome,
	}
	settle := existingSettle
	if settle.ID == "" {
		settle, err = observedCommand(settleIdentity, settlePayload)
		if err != nil {
			return CommandRecord{}, CommandRecord{}, SideEffectIdentity{}, err
		}
	} else if settle.State != CommandObserved && settle.State != CommandReconciled {
		return CommandRecord{}, CommandRecord{}, SideEffectIdentity{}, fmt.Errorf("%w: settlement remains active", ErrMutationInconsistent)
	}
	effect := SideEffectIdentity{Kind: SideEffectAttempt, RunID: view.RunID, ActivationID: source.SourceActivation.ID, Attempt: observation.Attempt, State: "observed"}
	if node.Type == model.NodeTypeWait {
		effect.Kind = SideEffectWait
		effect.WaitKind = exactWaitKind(node.Wait)
		effect.State = "satisfied"
	}
	var effectID string
	if effect.Kind == SideEffectWait {
		effectID, err = WaitIdentity(view.RunID, source.SourceActivation.ID, observation.Attempt, effect.WaitKind)
	} else {
		effectID, err = AttemptIdentity(view.RunID, source.SourceActivation.ID, observation.Attempt)
	}
	if err != nil {
		return CommandRecord{}, CommandRecord{}, SideEffectIdentity{}, err
	}
	effect.ID = effectID
	return perform, settle, effect, nil
}

func exactWaitKind(wait *model.WaitConfig) string {
	if wait == nil {
		return "satisfied"
	}
	if strings.TrimSpace(wait.Signal) != "" {
		return "signal"
	}
	if strings.TrimSpace(wait.Duration) != "" {
		return "duration"
	}
	if strings.TrimSpace(wait.Until) != "" {
		return "until"
	}
	return "satisfied"
}

func observedCommand(identity CommandIdentity, payload []byte) (CommandRecord, error) {
	id, err := CommandIdentityDigest(identity)
	if err != nil {
		return CommandRecord{}, err
	}
	sum := sha256.Sum256(payload)
	command := CommandRecord{
		ID: id, IdempotencyKey: CommandIdempotencyKey(identity.Kind, id), Identity: identity,
		Payload: bytes.Clone(payload), PayloadHash: hex.EncodeToString(sum[:]), State: CommandObserved,
	}
	if err := ValidateCommand(command); err != nil {
		return CommandRecord{}, err
	}
	return command, nil
}

func insertExactCommand(commands map[string]CommandRecord, command CommandRecord) error {
	if current, ok := commands[command.ID]; ok && !exactExclusiveCommand(current, command) {
		return fmt.Errorf("%w: command identity collision", ErrMutationInconsistent)
	}
	commands[command.ID] = cloneCommandRecord(command)
	return nil
}

func exactExclusiveCommand(left, right CommandRecord) bool {
	return left.ID == right.ID &&
		left.IdempotencyKey == right.IdempotencyKey &&
		left.Identity == right.Identity &&
		bytes.Equal(left.Payload, right.Payload) &&
		left.PayloadHash == right.PayloadHash &&
		left.State == right.State
}

func validateExclusiveConservation(post AggregateView, draft exclusiveRouteDraft) error {
	parent, ok := post.Routing.Paths[draft.sourcePathID]
	if !ok || parent.State != PathRouted || !slices.Equal(parent.ProducedPathIDs, materializedProducedIDs(post, draft.sourcePathID)) {
		return fmt.Errorf("%w: routed parent does not own exact materialized children", ErrMutationInconsistent)
	}
	selected, impossible := 0, 0
	seenEdges := make(map[EdgeID]struct{}, len(draft.outgoing))
	for _, childID := range parent.ProducedPathIDs {
		child, ok := post.Routing.Paths[childID]
		if !ok || child.ParentPathID != parent.ID || child.Edge == nil {
			return fmt.Errorf("%w: routed child backlink is incomplete", ErrMutationInconsistent)
		}
		if _, duplicate := seenEdges[child.Edge.ID]; duplicate {
			return fmt.Errorf("%w: routed edge is duplicated", ErrMutationInconsistent)
		}
		seenEdges[child.Edge.ID] = struct{}{}
		switch child.Kind {
		case PathEdge:
			selected++
			if child.Edge.ID != draft.selectedEdge || child.State != PathArrived {
				return fmt.Errorf("%w: selected edge differs from exact plan", ErrMutationInconsistent)
			}
		case PathImpossibleEdge:
			impossible++
			if child.Edge.ID == draft.selectedEdge || child.State != PathImpossible || child.ImpossibleCauseDigest == "" {
				return fmt.Errorf("%w: impossible sibling lacks exact closure provenance", ErrMutationInconsistent)
			}
		default:
			return fmt.Errorf("%w: exclusive route produced child kind %q", ErrMutationInconsistent, child.Kind)
		}
	}
	if len(seenEdges) != len(draft.outgoing) || selected != 1 || impossible != len(draft.outgoing)-1 {
		return fmt.Errorf("%w: exclusive token count selected=%d impossible=%d outgoing=%d", ErrMutationInconsistent, selected, impossible, len(draft.outgoing))
	}
	return nil
}

func materializedProducedIDs(post AggregateView, parentID PathID) []PathID {
	ids := make([]PathID, 0)
	for id, path := range post.Routing.Paths {
		if path.ParentPathID == parentID {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

func validateExclusiveMaterializedTopology(ctx context.Context, view AggregateView, tmpl *model.Template) error {
	if report := ValidateAggregate(view); !report.Valid() {
		return fmt.Errorf("aggregate has %d diagnostics", len(report.Diagnostics)+report.Suppressed)
	}
	for id, reservation := range view.Routing.Reservations {
		if err := ctx.Err(); err != nil {
			return err
		}
		if id == view.Authority.Genesis.ReservationID {
			continue
		}
		if reservation.ScopeID != view.Authority.Genesis.RootScopeID || reservation.BranchEdgeID != "" || reservation.Generation != 1 || reservation.JoinPolicy != JoinExclusive || reservation.IsReducing {
			return fmt.Errorf("reservation %q is outside exclusive root authority", id)
		}
		want, err := deriveExclusiveReservation(tmpl, view.RunID, view.TemplateRef, reservation.ScopeID, reservation.NodeID, reservation.EventSeq)
		if err != nil {
			return err
		}
		static := reservation
		static.State, static.Activation, static.CloseReceipt, static.ClosedReason, static.CauseDigest, static.CommandID = ReservationOpen, nil, nil, "", "", ""
		if !canonicalEqual(reservationAuthority(static), reservationAuthority(want)) {
			return fmt.Errorf("reservation %q differs from normalized exact-template authority", id)
		}
	}
	return nil
}

func checkpointAggregate(view AggregateView) (AggregateCheckpoint, error) {
	if view.Authority == nil || view.Routing == nil {
		return AggregateCheckpoint{}, fmt.Errorf("%w: incomplete projected aggregate", ErrMutationInconsistent)
	}
	value := AggregateCheckpoint{
		RunID: view.RunID, TemplateRef: view.TemplateRef, TemplateSourceHash: view.TemplateSourceHash,
		Authority: AggregateAuthorityCheckpoint{
			RunID: view.Authority.RunID, TemplateRef: view.Authority.TemplateRef, TemplateSourceHash: view.Authority.TemplateSourceHash,
			Genesis: view.Authority.Genesis, Scopes: cloneMap(view.Authority.Scopes), Reservations: cloneMap(view.Authority.Reservations),
		},
		Routing: Clone(*view.Routing), Commands: cloneCommands(view.Commands), SideEffects: cloneMap(view.SideEffects),
		AdminRecords: cloneMap(view.AdminRecords), AdminResolutions: cloneMap(view.AdminResolutions),
	}
	return cloneAggregateCheckpoint(value)
}

func cloneAggregateCheckpoint(value AggregateCheckpoint) (AggregateCheckpoint, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return AggregateCheckpoint{}, err
	}
	var out AggregateCheckpoint
	if err := json.Unmarshal(data, &out); err != nil {
		return AggregateCheckpoint{}, err
	}
	return out, nil
}

func cloneExclusiveAuthority(value *AggregateAuthority) *AggregateAuthority {
	if value == nil {
		return nil
	}
	return &AggregateAuthority{
		RunID: value.RunID, TemplateRef: value.TemplateRef, TemplateSourceHash: value.TemplateSourceHash,
		Genesis: value.Genesis, Scopes: cloneMap(value.Scopes), Reservations: cloneMap(value.Reservations),
	}
}

func cloneCommands(values map[string]CommandRecord) map[string]CommandRecord {
	out := make(map[string]CommandRecord, len(values))
	for id, command := range values {
		out[id] = cloneCommandRecord(command)
	}
	return out
}

func cloneCommandRecord(value CommandRecord) CommandRecord {
	value.Payload = bytes.Clone(value.Payload)
	return value
}

func cloneCandidates(values []CandidateRecord) []CandidateRecord {
	out := cloneSlice(values)
	for index := range out {
		out[index].PossibleSlotIDs = cloneSlice(out[index].PossibleSlotIDs)
	}
	return out
}

func cloneEdge(value *EdgeKey) *EdgeKey {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}
