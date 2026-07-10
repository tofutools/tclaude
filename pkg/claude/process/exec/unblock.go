package processexec

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	processverify "github.com/tofutools/tclaude/pkg/claude/process/verify"
)

// BlockResolutionRequest is the shared CLI/engine input for resolving a
// poison-blocked stage. Validation and normalization intentionally live here
// so adapters and commands cannot acquire different decision semantics.
type BlockResolutionRequest struct {
	RunID          string
	NodeID         string
	BlockedAttempt int
	Decision       state.BlockDecision
	Actor          state.ActorRef
	Reason         string
	EvidenceRef    string
}

// BindBlockResolution pins a decision intent to the currently blocked child
// and attempt. Callers persist/pass the returned request to ResolveBlocked;
// replaying it after a later poison generation is then rejected as stale.
func BindBlockResolution(snapshot store.Snapshot, request BlockResolutionRequest) (BlockResolutionRequest, error) {
	normalized, err := normalizeBlockResolution(snapshot, request, time.Time{}, false)
	if err != nil {
		return BlockResolutionRequest{}, err
	}
	return normalized.request, nil
}

type normalizedBlockResolution struct {
	request         BlockResolutionRequest
	childID         string
	parentID        string
	resolution      state.BlockResolution
	childStatus     state.NodeStatus
	runStatus       state.RunStatus
	alreadyResolved bool
}

// ResolveBlocked records one explicit decision and clears the poisoned stage
// child plus its parent mirror in one append batch. CAS retries re-normalize
// against the latest snapshot; an identical replay is idempotent.
func (e *Executor) ResolveBlocked(ctx context.Context, request BlockResolutionRequest) (*state.State, error) {
	if e == nil || e.Store == nil {
		return nil, fmt.Errorf("process executor store is required")
	}
	request.RunID = strings.TrimSpace(request.RunID)
	for attempt := 0; attempt < maxObservationCASAttempts; attempt++ {
		snapshot, err := e.Store.LoadRun(ctx, request.RunID)
		if err != nil {
			return nil, err
		}
		if report := processverify.StoreRun(ctx, e.Store, request.RunID); report.HasErrors() {
			for _, diagnostic := range report.Diagnostics {
				if diagnostic.Severity == model.SeverityError {
					return nil, fmt.Errorf("process run %q failed verification (%s at %s): %s", request.RunID, diagnostic.Code, diagnostic.Path, diagnostic.Message)
				}
			}
		}
		normalized, err := normalizeBlockResolution(snapshot, request, e.now(), true)
		if err != nil {
			return nil, err
		}
		if normalized.alreadyResolved {
			return snapshot.State, nil
		}
		at := normalized.resolution.Timestamp
		entries := []evidence.LogEntry{
			runEntry(state.Event{
				Type:        state.EventBlockResolutionRecorded,
				Actor:       normalized.resolution.Actor,
				Reason:      normalized.resolution.Reason,
				EvidenceRef: normalized.resolution.EvidenceRef,
				Resolution:  &normalized.resolution,
			}, normalized.resolution.EvidenceRef, at),
			nodeEntry(normalized.childID, state.Event{
				Type:       state.EventNodeUnblocked,
				NodeStatus: normalized.childStatus,
				Resolution: &normalized.resolution,
			}, normalized.resolution.EvidenceRef, at),
			nodeEntry(normalized.parentID, state.Event{
				Type:       state.EventNodeUnblocked,
				NodeStatus: state.NodeStatusRunning,
				Resolution: &normalized.resolution,
			}, normalized.resolution.EvidenceRef, at),
		}
		if normalized.runStatus != "" {
			entries = append(entries, runEntry(state.Event{
				Type:      state.EventRunStatusSet,
				RunStatus: normalized.runStatus,
			}, normalized.resolution.EvidenceRef, at))
		}
		appended, err := e.Store.Append(ctx, normalized.request.RunID, snapshot.State.LastLogSeq, entries)
		if err == nil {
			return appended.State, nil
		}
		if !store.IsConflict(err) {
			return nil, fmt.Errorf("resolve blocked node %q: %w", normalized.childID, err)
		}
	}
	return nil, fmt.Errorf("resolve blocked node %q: exceeded %d CAS attempts", request.NodeID, maxObservationCASAttempts)
}

func normalizeBlockResolution(snapshot store.Snapshot, request BlockResolutionRequest, at time.Time, requireBinding bool) (normalizedBlockResolution, error) {
	request.RunID = strings.TrimSpace(request.RunID)
	request.NodeID = strings.TrimSpace(request.NodeID)
	request.Decision = state.BlockDecision(strings.ToLower(strings.TrimSpace(string(request.Decision))))
	request.Actor = state.ActorRef(strings.TrimSpace(string(request.Actor)))
	request.Reason = strings.TrimSpace(request.Reason)
	request.EvidenceRef = strings.TrimSpace(request.EvidenceRef)
	if request.RunID == "" || request.RunID != snapshot.Run.ID || request.RunID != snapshot.State.RunID {
		return normalizedBlockResolution{}, fmt.Errorf("block resolution run id %q does not match loaded run %q", request.RunID, snapshot.Run.ID)
	}
	if request.NodeID == "" {
		return normalizedBlockResolution{}, fmt.Errorf("block resolution node id is required")
	}
	if request.BlockedAttempt < 0 {
		return normalizedBlockResolution{}, fmt.Errorf("block resolution attempt must not be negative")
	}
	if requireBinding && request.BlockedAttempt == 0 {
		return normalizedBlockResolution{}, fmt.Errorf("block resolution request is not generation-bound; call BindBlockResolution first")
	}
	if !request.Decision.IsValid() {
		return normalizedBlockResolution{}, fmt.Errorf("--decision must be retry, skip, or cancel")
	}
	if !state.ValidateActorRef(request.Actor) || state.IsEngineActor(request.Actor) {
		return normalizedBlockResolution{}, fmt.Errorf("block resolution actor %q must be a non-engine actor ref", request.Actor)
	}
	if request.Reason == "" {
		return normalizedBlockResolution{}, fmt.Errorf("block resolution reason is required")
	}
	if request.EvidenceRef == "" {
		return normalizedBlockResolution{}, fmt.Errorf("block resolution evidence ref is required")
	}

	selected, ok := snapshot.State.Nodes[request.NodeID]
	if !ok {
		return normalizedBlockResolution{}, fmt.Errorf("node %q is not in run state", request.NodeID)
	}
	if selected.BlockResolution != nil && selected.Status != state.NodeStatusBlocked {
		if resolutionMatchesRequest(*selected.BlockResolution, request) {
			request.NodeID = selected.BlockResolution.NodeID
			request.BlockedAttempt = selected.BlockResolution.BlockedAttempt
			return normalizedBlockResolution{request: request, alreadyResolved: true}, nil
		}
		return normalizedBlockResolution{}, fmt.Errorf("node %q was already unblocked with decision %q", request.NodeID, selected.BlockResolution.Decision)
	}
	if selected.Status != state.NodeStatusBlocked {
		return normalizedBlockResolution{}, fmt.Errorf("node %q is %s; only blocked nodes can be resolved", request.NodeID, selected.Status)
	}

	childID, parentID := request.NodeID, selected.Parent
	if parentID == "" {
		parentID = request.NodeID
		childID = ""
		for _, candidateID := range selected.Children {
			if candidate := snapshot.State.Nodes[candidateID]; candidate.Status == state.NodeStatusBlocked {
				if childID != "" {
					return normalizedBlockResolution{}, fmt.Errorf("blocked parent %q has multiple blocked children; resolve a child explicitly", request.NodeID)
				}
				childID = candidateID
			}
		}
		if childID == "" {
			return normalizedBlockResolution{}, fmt.Errorf("blocked parent %q has no blocked child", request.NodeID)
		}
	}
	child := snapshot.State.Nodes[childID]
	parent := snapshot.State.Nodes[parentID]
	if child.Status != state.NodeStatusBlocked || parent.Status != state.NodeStatusBlocked {
		return normalizedBlockResolution{}, fmt.Errorf("blocked mirror for child %q and parent %q is inconsistent", childID, parentID)
	}
	if child.BlockedReason != parent.BlockedReason || child.BlockedOwner != parent.BlockedOwner {
		return normalizedBlockResolution{}, fmt.Errorf("blocked mirror for child %q and parent %q has different reason or owner", childID, parentID)
	}
	blockedAttempt := child.BlockedAttempt
	if blockedAttempt <= 0 {
		blockedAttempt = child.Attempt
	}
	if blockedAttempt <= 0 {
		return normalizedBlockResolution{}, fmt.Errorf("blocked child %q has no attempt generation", childID)
	}
	if request.BlockedAttempt > 0 && request.BlockedAttempt != blockedAttempt {
		return normalizedBlockResolution{}, fmt.Errorf("block resolution for node %q is stale: approved attempt %d, current blocked attempt %d", childID, request.BlockedAttempt, blockedAttempt)
	}
	if parent.BlockedAttempt > 0 && parent.BlockedAttempt != blockedAttempt {
		return normalizedBlockResolution{}, fmt.Errorf("blocked mirror for child %q and parent %q has different attempt generations", childID, parentID)
	}
	if child.BlockedNodeID != "" && child.BlockedNodeID != childID || parent.BlockedNodeID != "" && parent.BlockedNodeID != childID {
		return normalizedBlockResolution{}, fmt.Errorf("blocked mirror for child %q and parent %q names a different poisoned child", childID, parentID)
	}
	resolution := state.BlockResolution{
		NodeID: childID, BlockedAttempt: blockedAttempt, Decision: request.Decision,
		Actor: request.Actor, Reason: request.Reason, EvidenceRef: request.EvidenceRef, Timestamp: at,
	}
	request.NodeID = childID
	request.BlockedAttempt = blockedAttempt
	normalized := normalizedBlockResolution{
		request: request, childID: childID, parentID: parentID,
		resolution: resolution,
	}
	switch request.Decision {
	case state.BlockDecisionRetry:
		normalized.childStatus = state.NodeStatusReady
	case state.BlockDecisionSkip:
		normalized.childStatus = state.NodeStatusSkipped
	case state.BlockDecisionCancel:
		normalized.childStatus = state.NodeStatusSkipped
		normalized.runStatus = state.RunStatusCanceled
	}
	return normalized, nil
}

func resolutionMatchesRequest(resolution state.BlockResolution, request BlockResolutionRequest) bool {
	return (request.BlockedAttempt == 0 || resolution.BlockedAttempt == request.BlockedAttempt) && resolution.Decision == request.Decision && resolution.Actor == request.Actor &&
		resolution.Reason == request.Reason && resolution.EvidenceRef == request.EvidenceRef
}
