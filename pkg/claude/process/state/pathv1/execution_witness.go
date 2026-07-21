package pathv1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

const (
	ExecutionWitnessVersion  = 1
	MaxExecutionWitnessBytes = 64 << 10
)

type EmptyExecutionWitnessV1 struct{}
type SourceExecutionWitnessV1 struct {
	SourcePathID PathID `json:"sourcePathId"`
}
type ClaimWaitWitnessV1 struct {
	SourcePathID PathID `json:"sourcePathId"`
	Now          string `json:"now"`
	CommandID    string `json:"commandId"`
}
type ObserveWaitWitnessV1 struct {
	CommandID   string `json:"commandId"`
	Actor       string `json:"actor"`
	EvidenceRef string `json:"evidenceRef,omitempty"`
}
type ClaimAttemptWitnessV1 struct {
	SourcePathID PathID            `json:"sourcePathId"`
	Attempt      uint64            `json:"attempt"`
	Params       map[string]string `json:"params,omitempty"`
	CommandID    string            `json:"commandId"`
}
type ObserveAttemptWitnessV1 struct {
	CommandID   string               `json:"commandId"`
	Observation ExclusiveObservation `json:"observation"`
}
type RouteObservationWitnessV1 struct {
	Mode         string `json:"mode"`
	SourcePathID PathID `json:"sourcePathId,omitempty"`
}
type ScheduleContactWitnessV1 struct {
	Schedule ContactScheduleV7 `json:"schedule"`
	Now      string            `json:"now"`
}
type ContactAtWitnessV1 struct {
	ContactID string `json:"contactId"`
	At        string `json:"at"`
}
type ContactReasonWitnessV1 struct {
	ContactID string `json:"contactId"`
	Reason    string `json:"reason"`
}
type RecoverContactWitnessV1 struct {
	ContactID   string `json:"contactId"`
	RecoveredAt string `json:"recoveredAt"`
	Now         string `json:"now"`
}

// ExecutionWitnessV1 is a closed transition-input union. Exactly one member
// must be present and it must correspond to Kind.
type ExecutionWitnessV1 struct {
	Version          int                        `json:"version"`
	Kind             string                     `json:"kind"`
	Pre              CheckpointBinding          `json:"pre"`
	Post             CheckpointBinding          `json:"post"`
	ClaimWait        *ClaimWaitWitnessV1        `json:"claimWait,omitempty"`
	ObserveWait      *ObserveWaitWitnessV1      `json:"observeWait,omitempty"`
	ClaimAttempt     *ClaimAttemptWitnessV1     `json:"claimAttempt,omitempty"`
	ObserveAttempt   *ObserveAttemptWitnessV1   `json:"observeAttempt,omitempty"`
	RouteObservation *RouteObservationWitnessV1 `json:"routeObservation,omitempty"`
	Source           *SourceExecutionWitnessV1  `json:"source,omitempty"`
	Empty            *EmptyExecutionWitnessV1   `json:"empty,omitempty"`
	ScheduleContact  *ScheduleContactWitnessV1  `json:"scheduleContact,omitempty"`
	ContactAt        *ContactAtWitnessV1        `json:"contactAt,omitempty"`
	ContactReason    *ContactReasonWitnessV1    `json:"contactReason,omitempty"`
	RecoverContact   *RecoverContactWitnessV1   `json:"recoverContact,omitempty"`
	Settlement       *AuditedSettlementInput    `json:"settlement,omitempty"`
}

func cloneExecutionWitness(w *ExecutionWitnessV1) *ExecutionWitnessV1 {
	if w == nil {
		return nil
	}
	b, _ := json.Marshal(w)
	var out ExecutionWitnessV1
	_ = json.Unmarshal(b, &out)
	return &out
}

func validateExecutionWitness(w *ExecutionWitnessV1) error {
	if w == nil || w.Version != ExecutionWitnessVersion || w.Kind == "" || w.Pre.Digest == "" || w.Post.Digest == "" {
		return fmt.Errorf("%w: execution witness envelope is invalid", ErrMutationInvalid)
	}
	count := 0
	for _, present := range []bool{w.ClaimWait != nil, w.ObserveWait != nil, w.ClaimAttempt != nil, w.ObserveAttempt != nil, w.RouteObservation != nil, w.Source != nil, w.Empty != nil, w.ScheduleContact != nil, w.ContactAt != nil, w.ContactReason != nil, w.RecoverContact != nil, w.Settlement != nil} {
		if present {
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("%w: execution witness union is invalid", ErrMutationInvalid)
	}
	valid := false
	switch w.Kind {
	case TransitionClaimWait:
		valid = w.ClaimWait != nil
	case TransitionObserveWait:
		valid = w.ObserveWait != nil
	case TransitionClaimAttempt:
		valid = w.ClaimAttempt != nil
	case TransitionObserveAttempt:
		valid = w.ObserveAttempt != nil
	case TransitionRouteObservation:
		valid = w.RouteObservation != nil && (w.RouteObservation.Mode == "pending_route" || w.RouteObservation.Mode == "start_route" && w.RouteObservation.SourcePathID != "")
	case TransitionParallelSplit, TransitionParallelEnd:
		valid = w.Source != nil && w.Source.SourcePathID != ""
	case TransitionParallelAll, TransitionParallelAny, TransitionParallelRoute, TransitionParallelExclusiveArrival, TransitionParallelPropagation, TransitionParallelPropagationSeed, TransitionParallelTerminalClosure, TransitionParallelDetachedSink, TransitionParallelDetachmentIntern, TransitionClaimCompletion, TransitionObserveCompletion:
		valid = w.Empty != nil
	case TransitionScheduleContact:
		valid = w.ScheduleContact != nil
	case TransitionMarkContactDue, TransitionNudgeContact, TransitionEscalateContact, TransitionLatchContactHuman:
		valid = w.ContactAt != nil
	case TransitionPauseContact, TransitionClearContactHumanLatch:
		valid = w.ContactReason != nil
	case TransitionRecoverContact:
		valid = w.RecoverContact != nil
	case TransitionAuditedSettlement:
		valid = w.Settlement != nil
	}
	if !valid {
		return fmt.Errorf("%w: execution witness kind/variant mismatch", ErrMutationInvalid)
	}
	encoded, err := json.Marshal(w)
	if err != nil || len(encoded) > MaxExecutionWitnessBytes {
		return &OverBudgetError{Limit: "execution_witness_bytes", Value: len(encoded), Maximum: MaxExecutionWitnessBytes}
	}
	return nil
}

func parseWitnessTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || CanonicalTimestamp(t) != value {
		return time.Time{}, fmt.Errorf("%w: witness timestamp is not canonical", ErrMutationInvalid)
	}
	return t, nil
}

// ReplayExecutionWitnessV1 reconstructs one exact successor by invoking only
// existing typed path-v1 planners/reducers against the reconstructed lineage.
func ReplayExecutionWitnessV1(ctx context.Context, checkpoint *CheckpointV7, tmpl *model.Template, sourceHash string, witness *ExecutionWitnessV1) (*ExecutionTransition, error) {
	if err := validateExecutionWitness(witness); err != nil {
		return nil, err
	}
	if CurrentCheckpointBinding(checkpoint) != witness.Pre {
		return nil, fmt.Errorf("%w: execution witness pre-binding mismatch", ErrMutationInvalid)
	}
	input, err := verifyExecutionInputFromTemplate(ctx, checkpoint, tmpl, sourceHash)
	if err != nil {
		return nil, err
	}
	var got *ExecutionTransition
	switch witness.Kind {
	case TransitionClaimWait:
		now, e := parseWitnessTime(witness.ClaimWait.Now)
		if e != nil {
			return nil, e
		}
		plan, e := PlanExclusiveWait(ctx, input, witness.ClaimWait.SourcePathID, now)
		if e != nil {
			return nil, e
		}
		if plan.Command().ID != witness.ClaimWait.CommandID {
			return nil, fmt.Errorf("%w: wait command mismatch", ErrMutationInvalid)
		}
		got, e = ClaimExclusiveWait(ctx, input, plan)
		err = e
	case TransitionObserveWait:
		plans, e := RecoverExclusiveWaits(ctx, input)
		if e != nil {
			return nil, e
		}
		var plan *ExclusiveWaitPlan
		for _, p := range plans {
			if p.Command().ID == witness.ObserveWait.CommandID {
				plan = p
			}
		}
		got, err = ObserveExclusiveWait(ctx, input, plan, witness.ObserveWait.Actor, witness.ObserveWait.EvidenceRef)
	case TransitionClaimAttempt:
		plan, e := PlanExclusiveAttempt(ctx, input, witness.ClaimAttempt.SourcePathID, witness.ClaimAttempt.Attempt, witness.ClaimAttempt.Params)
		if e != nil {
			return nil, e
		}
		if plan.Command().ID != witness.ClaimAttempt.CommandID {
			return nil, fmt.Errorf("%w: attempt command mismatch", ErrMutationInvalid)
		}
		got, err = ClaimExclusiveAttempt(ctx, input, plan)
	case TransitionObserveAttempt:
		plan, found, e := RecoverExclusiveAttempt(ctx, input)
		if e != nil {
			return nil, e
		}
		if !found || plan.Command().ID != witness.ObserveAttempt.CommandID {
			return nil, fmt.Errorf("%w: observed attempt mismatch", ErrMutationInvalid)
		}
		for _, recovered := range []bool{false, true} {
			candidate, e := ObserveExclusiveAttempt(ctx, input, plan, witness.ObserveAttempt.Observation, recovered)
			if e == nil && candidate.PostBinding() == witness.Post {
				got = candidate
				break
			}
		}
		if got == nil {
			return nil, fmt.Errorf("%w: observation recovery mode has no exact successor", ErrMutationInvalid)
		}
	case TransitionRouteObservation:
		if witness.RouteObservation.Mode == "pending_route" {
			got, err = AdvanceExclusiveRoute(ctx, input)
		} else {
			got, err = AdvanceExclusiveStart(ctx, input, witness.RouteObservation.SourcePathID)
		}
	case TransitionClaimCompletion:
		got, err = ClaimExclusiveCompletion(ctx, input)
	case TransitionObserveCompletion:
		got, err = ObserveExclusiveCompletion(ctx, input)
	case TransitionParallelSplit:
		got, err = AdvanceParallelSplit(ctx, input, witness.Source.SourcePathID)
	case TransitionParallelAll:
		got, err = AdvanceParallelAll(ctx, input)
	case TransitionParallelAny:
		got, err = AdvanceParallelAny(ctx, input)
	case TransitionParallelRoute:
		got, err = AdvanceParallelRoute(ctx, input)
	case TransitionParallelExclusiveArrival:
		got, err = AdvanceParallelExclusiveArrival(ctx, input)
	case TransitionParallelEnd:
		got, err = AdvanceParallelEnd(ctx, input, witness.Source.SourcePathID)
	case TransitionParallelPropagation:
		got, err = AdvanceParallelPropagation(ctx, input)
	case TransitionParallelPropagationSeed:
		aggregate, aggregateErr := CurrentAggregateCheckpoint(input.checkpoint)
		if aggregateErr != nil {
			return nil, aggregateErr
		}
		got, err = advanceParallelPropagationSeed(ctx, input, aggregate)
	case TransitionParallelTerminalClosure:
		got, err = AdvanceParallelTerminalClosure(ctx, input)
	case TransitionParallelDetachedSink:
		got, err = AdvanceParallelDetachedSink(ctx, input)
	case TransitionParallelDetachmentIntern:
		var ok bool
		got, ok, err = advanceReducerDetachmentIntern(ctx, input)
		if err == nil && !ok {
			err = ErrParallelAnyNotReady
		}
	case TransitionScheduleContact:
		now, e := parseWitnessTime(witness.ScheduleContact.Now)
		if e != nil {
			return nil, e
		}
		got, err = ScheduleExclusiveContact(ctx, input, witness.ScheduleContact.Schedule, now)
	case TransitionMarkContactDue, TransitionNudgeContact, TransitionEscalateContact, TransitionLatchContactHuman:
		at, e := parseWitnessTime(witness.ContactAt.At)
		if e != nil {
			return nil, e
		}
		switch witness.Kind {
		case TransitionMarkContactDue:
			got, err = MarkExclusiveContactDue(ctx, input, witness.ContactAt.ContactID, at)
		case TransitionNudgeContact:
			got, err = NudgeExclusiveContact(ctx, input, witness.ContactAt.ContactID, at)
		case TransitionEscalateContact:
			got, err = EscalateExclusiveContact(ctx, input, witness.ContactAt.ContactID, at)
		default:
			got, err = LatchExclusiveContactHumanInteraction(ctx, input, witness.ContactAt.ContactID, at)
		}
	case TransitionPauseContact:
		got, err = PauseExclusiveContact(ctx, input, witness.ContactReason.ContactID, witness.ContactReason.Reason)
	case TransitionClearContactHumanLatch:
		got, err = ClearExclusiveContactHumanLatch(ctx, input, witness.ContactReason.ContactID, witness.ContactReason.Reason)
	case TransitionRecoverContact:
		recovered, e := parseWitnessTime(witness.RecoverContact.RecoveredAt)
		if e != nil {
			return nil, e
		}
		now, e := parseWitnessTime(witness.RecoverContact.Now)
		if e != nil {
			return nil, e
		}
		got, err = RecoverExclusiveContactReset(ctx, input, witness.RecoverContact.ContactID, recovered, now)
	case TransitionAuditedSettlement:
		got, err = SettleExclusiveAttempt(ctx, input, *witness.Settlement)
	}
	if err != nil {
		return nil, err
	}
	if got == nil || got.Kind() != witness.Kind || got.PreBinding() != witness.Pre || got.PostBinding() != witness.Post {
		return nil, fmt.Errorf("%w: witness replay successor mismatch", ErrMutationInvalid)
	}
	return got, nil
}

// RuntimeGenesisWitnessV1 is the canonical semantic base for one runtime
// lineage. Source bytes are deliberately absent; the raw source digest stays
// independently bound by the epoch.
type RuntimeGenesisWitnessV1 struct {
	Version              int             `json:"version"`
	InternalRunID        string          `json:"internalRunId"`
	TemplateRef          string          `json:"templateRef"`
	TemplateSourceDigest string          `json:"templateSourceDigest"`
	NodeID               string          `json:"nodeId"`
	Template             json.RawMessage `json:"template"`
}

func BuildRuntimeGenesisWitness(ctx context.Context, runID string, source []byte, nodeID string) (*CheckpointV7, *RuntimeGenesisWitnessV1, error) {
	checkpoint, err := BuildRuntimeGenesis(ctx, runID, source, nodeID)
	if err != nil {
		return nil, nil, err
	}
	parsed, err := model.ParseExactSource(source)
	if err != nil || parsed == nil || parsed.Template == nil {
		return nil, nil, fmt.Errorf("%w: runtime witness template is invalid", ErrInitializationInvalid)
	}
	semantic, err := model.CanonicalSemanticJSON(parsed.Template)
	if err != nil {
		return nil, nil, err
	}
	w := &RuntimeGenesisWitnessV1{Version: 1, InternalRunID: runID, TemplateRef: parsed.Ref, TemplateSourceDigest: parsed.SourceHash, NodeID: nodeID, Template: semantic}
	return checkpoint, w, nil
}

func ReplayRuntimeGenesisWitness(ctx context.Context, w *RuntimeGenesisWitnessV1) (*CheckpointV7, *model.Template, error) {
	if w == nil || w.Version != 1 || strings.TrimSpace(w.InternalRunID) == "" || w.NodeID == "" {
		return nil, nil, fmt.Errorf("%w: runtime genesis witness is invalid", ErrInitializationInvalid)
	}
	decoder := json.NewDecoder(bytes.NewReader(w.Template))
	decoder.DisallowUnknownFields()
	var tmpl model.Template
	if err := decoder.Decode(&tmpl); err != nil {
		return nil, nil, fmt.Errorf("%w: runtime genesis semantic preimage decode: %v", ErrInitializationInvalid, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("%w: runtime genesis semantic preimage has trailing data", ErrInitializationInvalid)
	}
	canonical, err := model.CanonicalSemanticJSON(&tmpl)
	if err != nil || !bytes.Equal(canonical, w.Template) {
		return nil, nil, fmt.Errorf("%w: runtime genesis semantic preimage is noncanonical", ErrInitializationInvalid)
	}
	hash, err := model.SemanticHash(&tmpl)
	if err != nil || model.TemplateRef(tmpl.ID, hash) != w.TemplateRef {
		return nil, nil, fmt.Errorf("%w: runtime genesis semantic mismatch", ErrInitializationInvalid)
	}
	checkpoint, err := buildRuntimeGenesisFromTemplate(ctx, w.InternalRunID, &tmpl, w.TemplateRef, w.TemplateSourceDigest, w.NodeID)
	if err != nil {
		return nil, nil, err
	}
	return checkpoint, &tmpl, nil
}
