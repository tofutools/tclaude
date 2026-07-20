package pathv1

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/plan"
	legacy "github.com/tofutools/tclaude/pkg/claude/process/state"
)

const (
	LegacyProjectionVersion   = 1
	LegacyProjectionAdminType = "progressed_history_projected"
)

type ProjectionRefusalCode string

const (
	ProjectionRefusalRunState       ProjectionRefusalCode = "run_state"
	ProjectionRefusalTopology       ProjectionRefusalCode = "topology"
	ProjectionRefusalHistory        ProjectionRefusalCode = "history"
	ProjectionRefusalAuthority      ProjectionRefusalCode = "authority"
	ProjectionRefusalFrontier       ProjectionRefusalCode = "frontier"
	ProjectionRefusalStateMismatch  ProjectionRefusalCode = "state_mismatch"
	ProjectionRefusalContactHistory ProjectionRefusalCode = "contact_history"
)

// ProjectionRefusal is a safe compatibility refusal, not corruption. Its
// unwrap keeps the production host's existing schema-6 fallback behavior.
type ProjectionRefusal struct {
	Code   ProjectionRefusalCode
	Detail string
}

func (e *ProjectionRefusal) Error() string {
	if e == nil {
		return ErrInitializationAmbiguous.Error()
	}
	return fmt.Sprintf("%v: progressed legacy projection refused (%s): %s", ErrInitializationAmbiguous, e.Code, e.Detail)
}

func (e *ProjectionRefusal) Unwrap() error { return ErrInitializationAmbiguous }

func refuseProjection(code ProjectionRefusalCode, format string, args ...any) error {
	return &ProjectionRefusal{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// LegacyProjectionMetadata is the compact checkpoint-bound anchor for the
// unchanged legacy evidence. It contains no prompt, payload, or evidence body.
type LegacyProjectionMetadata struct {
	Version                uint64 `json:"version"`
	ID                     string `json:"id"`
	LegacyCheckpointDigest string `json:"legacyCheckpointDigest"`
	LegacyLastLogSeq       uint64 `json:"legacyLastLogSeq"`
	LegacyLogChecksum      string `json:"legacyLogChecksum"`
	ProjectionKind         string `json:"projectionKind"`
	ProjectedAggregateHash string `json:"projectedAggregateHash"`
}

type legacyProjectionIdentityInput struct {
	Version                uint64 `json:"version"`
	LegacyCheckpointDigest string `json:"legacyCheckpointDigest"`
	LegacyLastLogSeq       uint64 `json:"legacyLastLogSeq"`
	LegacyLogChecksum      string `json:"legacyLogChecksum"`
	ProjectionKind         string `json:"projectionKind"`
}

func buildLegacyProjectionMetadata(needed UpgradeNeeded, st *legacy.State) (LegacyProjectionMetadata, error) {
	if st == nil || st.LastLogSeq < 0 {
		return LegacyProjectionMetadata{}, fmt.Errorf("%w: legacy projection metadata lacks state", ErrInitializationInvalid)
	}
	value := legacyProjectionIdentityInput{
		Version: LegacyProjectionVersion, LegacyCheckpointDigest: needed.Checkpoint.Digest,
		LegacyLastLogSeq: uint64(st.LastLogSeq), LegacyLogChecksum: st.LogChecksum,
		ProjectionKind: "exclusive_current_state_v1",
	}
	data, err := canonicalJSON(value)
	if err != nil {
		return LegacyProjectionMetadata{}, err
	}
	sum := sha256.Sum256(data)
	return LegacyProjectionMetadata{
		Version: value.Version, ID: hex.EncodeToString(sum[:]),
		LegacyCheckpointDigest: value.LegacyCheckpointDigest,
		LegacyLastLogSeq:       value.LegacyLastLogSeq, LegacyLogChecksum: value.LegacyLogChecksum,
		ProjectionKind: value.ProjectionKind,
	}, nil
}

func validateLegacyProjectionMetadata(value *LegacyProjectionMetadata, needed UpgradeNeeded) error {
	if value == nil {
		return nil
	}
	if value.Version != LegacyProjectionVersion || value.ProjectionKind != "exclusive_current_state_v1" ||
		value.LegacyCheckpointDigest != needed.Checkpoint.Digest || value.LegacyLastLogSeq != needed.Checkpoint.Generation ||
		value.LegacyLogChecksum == "" || !canonicalDigest(value.ProjectedAggregateHash) {
		return fmt.Errorf("%w: legacy projection anchor is invalid", ErrInitializationInvalid)
	}
	want, err := buildLegacyProjectionMetadata(needed, &legacy.State{LastLogSeq: int64(value.LegacyLastLogSeq), LogChecksum: value.LegacyLogChecksum})
	if err != nil || want.ID != value.ID {
		return fmt.Errorf("%w: legacy projection identity mismatch", ErrInitializationInvalid)
	}
	return nil
}

func validateLegacyProjectionProvenance(value *LegacyProjectionMetadata, aggregate AggregateView, eventSeq int64) error {
	matching := 0
	for _, record := range aggregate.AdminRecords {
		if record.AdminType != LegacyProjectionAdminType {
			continue
		}
		matching++
		if value == nil || record.Actor != "system:migration" || record.ReasonCode != "legacy_projection" ||
			record.EvidenceRef != value.ID || record.EventSeq != eventSeq {
			return fmt.Errorf("%w: legacy projection provenance mismatch", ErrInitializationInvalid)
		}
	}
	if (value == nil && matching != 0) || (value != nil && matching != 1) {
		return fmt.Errorf("%w: legacy projection requires exactly one matching provenance record", ErrInitializationInvalid)
	}
	return nil
}

func validateLegacyProjectionAggregate(value *LegacyProjectionMetadata, execution *ExecutionCheckpoint) error {
	if value == nil || execution == nil || execution.Revision != 1 {
		return nil
	}
	data, err := canonicalJSON(execution.Aggregate)
	if err != nil {
		return fmt.Errorf("%w: projected aggregate cannot be canonicalized", ErrInitializationInvalid)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != value.ProjectedAggregateHash {
		return fmt.Errorf("%w: projected aggregate hash mismatch", ErrInitializationInvalid)
	}
	return nil
}

func cloneLegacyProjectionMetadata(value *LegacyProjectionMetadata) *LegacyProjectionMetadata {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func currentLegacyProjection(checkpoint *CheckpointV7) *LegacyProjectionMetadata {
	if checkpoint == nil || checkpoint.Execution == nil {
		return nil
	}
	return checkpoint.Execution.LegacyProjection
}

// LegacyProjectionInput is valid only after the caller has obtained one
// coherent, locked execution view. This pure constructor re-verifies the
// evidence/state anchor before materializing anything.
type LegacyProjectionInput struct {
	UpgradeNeeded        UpgradeNeeded
	Template             *model.Template
	LegacyState          *legacy.State
	LegacyCheckpointJSON []byte
	Manifest             []evidence.ManifestEntry
	NodeLogs             []evidence.NodeLog
}

// BuildProgressedInitialization deterministically materializes the bounded
// exclusive progressed-history slice without invoking an adapter or mutating
// any caller-owned value.
func BuildProgressedInitialization(ctx context.Context, input LegacyProjectionInput) (*CheckpointV7, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateProjectionInput(ctx, input); err != nil {
		return nil, err
	}
	entries, err := orderedLegacyEntries(input.Manifest, input.NodeLogs)
	if err != nil {
		return nil, err
	}
	checkpoint, err := BuildInitialization(ctx, input.UpgradeNeeded, input.Template)
	if err != nil {
		return nil, err
	}
	projector := legacyProjector{
		ctx: ctx, tmpl: input.Template, legacy: input.LegacyState, entries: entries,
		eventSeq: checkpoint.Initialize.EventSeq, checkpoint: checkpoint,
	}
	aggregate, err := projector.project()
	if err != nil {
		return nil, err
	}
	metadata, err := buildLegacyProjectionMetadata(input.UpgradeNeeded, input.LegacyState)
	if err != nil {
		return nil, err
	}
	admin := PathV1AdminRecord{
		RunID: input.LegacyState.RunID, EventSeq: checkpoint.Initialize.EventSeq,
		AdminType: LegacyProjectionAdminType, Actor: "system:migration",
		ReasonCode: "legacy_projection", EvidenceRef: metadata.ID,
	}
	admin.ID, err = AdminRecordIdentity(admin)
	if err != nil {
		return nil, err
	}
	aggregate.AdminRecords[admin.ID] = admin
	projectedJSON, err := canonicalJSON(aggregate)
	if err != nil {
		return nil, err
	}
	projectedSum := sha256.Sum256(projectedJSON)
	metadata.ProjectedAggregateHash = hex.EncodeToString(projectedSum[:])
	if report := ValidateAggregate(aggregate.View()); !report.Valid() {
		return nil, fmt.Errorf("%w: projected aggregate diagnostics=%v (%d suppressed)", ErrInitializationInconsistent, report.Diagnostics, report.Suppressed)
	}
	execution := &ExecutionCheckpoint{
		Revision: 1, PreviousDigest: checkpoint.Digest, Status: string(input.LegacyState.Status),
		LastLogSeq: uint64(checkpoint.Initialize.EventSeq), LogChecksum: checkpoint.Digest,
		LegacyProjection: &metadata, Aggregate: aggregate,
	}
	checkpoint.Execution = execution
	genesisDigest, err := initializeEventDigest(checkpoint.Initialize)
	if err != nil {
		return nil, err
	}
	checkpoint.Digest, err = executionCheckpointDigest(genesisDigest, execution)
	if err != nil {
		return nil, err
	}
	if err := ValidateCheckpointV7(checkpoint); err != nil {
		return nil, err
	}
	return checkpoint, nil
}

func validateProjectionInput(ctx context.Context, input LegacyProjectionInput) error {
	if input.Template == nil || input.LegacyState == nil || len(input.LegacyCheckpointJSON) == 0 {
		return fmt.Errorf("%w: complete legacy projection input is required", ErrInitializationInvalid)
	}
	if err := ValidateUpgradeNeeded(input.UpgradeNeeded); err != nil {
		return fmt.Errorf("%w: %v", ErrInitializationInvalid, err)
	}
	if input.UpgradeNeeded.Reason != UpgradeMigrationRequired || len(input.UpgradeNeeded.ActiveLegacyIDs) != 0 || len(input.UpgradeNeeded.CheckpointAdminRecords) != 0 {
		return fmt.Errorf("%w: legacy drain authority is required", ErrInitializationInvalid)
	}
	st := input.LegacyState
	if st.Status != legacy.RunStatusRunning || st.Pause != nil || st.TemplateDivergence != nil || len(st.AdminRecords) != 0 {
		return refuseProjection(ProjectionRefusalRunState, "run is terminal, paused, divergent, or administratively modified")
	}
	if st.LastLogSeq < 0 || uint64(st.LastLogSeq) != input.UpgradeNeeded.Checkpoint.Generation {
		return fmt.Errorf("%w: legacy log anchor differs from upgrade proof", ErrInitializationInconsistent)
	}
	if st.LastLogSeq == 0 && st.LogChecksum == "" {
		return refuseProjection(ProjectionRefusalAuthority, "progressed state lacks reconstruction evidence")
	}
	if st.LogChecksum == "" {
		return fmt.Errorf("%w: progressed legacy checkpoint lacks its log checksum", ErrInitializationInconsistent)
	}
	encoded, err := legacy.Encode(st)
	if err != nil {
		return err
	}
	if !bytes.Equal(encoded, input.LegacyCheckpointJSON) {
		return fmt.Errorf("%w: legacy checkpoint bytes differ from supplied state", ErrInitializationInconsistent)
	}
	digest, err := CheckpointIdentity(string(st.Status), uint64(st.LastLogSeq), st.LogChecksum, input.LegacyCheckpointJSON)
	if err != nil || digest != input.UpgradeNeeded.Checkpoint.Digest {
		return fmt.Errorf("%w: legacy checkpoint digest differs from upgrade proof", ErrInitializationInconsistent)
	}
	_, replayedJSON, replayErr := ReplayLegacyProjectionEvidence(ctx, st.RunID, st.OriginalTemplateRef, st.CurrentTemplateRef, input.Template, input.Manifest, input.NodeLogs)
	if replayErr != nil {
		return replayErr
	}
	if !bytes.Equal(replayedJSON, encoded) {
		return refuseProjection(ProjectionRefusalStateMismatch, "verified evidence does not reconstruct the retained checkpoint")
	}
	if err := validateProjectionTopology(input.Template, st); err != nil {
		return err
	}
	return nil
}

// ReplayLegacyProjectionEvidence reconstructs the canonical schema-6 state
// from bounded, manifest-ordered evidence and the exact pinned template. It is
// read-only and performs no adapter, timer, signal, or contact action.
func ReplayLegacyProjectionEvidence(
	ctx context.Context,
	runID, originalTemplateRef, currentTemplateRef string,
	tmpl *model.Template,
	manifest []evidence.ManifestEntry,
	logs []evidence.NodeLog,
) (legacy.State, []byte, error) {
	if tmpl == nil || runID == "" || originalTemplateRef == "" || currentTemplateRef == "" {
		return legacy.State{}, nil, fmt.Errorf("%w: legacy evidence replay lacks its exact run/template binding", ErrInitializationInvalid)
	}
	entries, err := orderedLegacyEntries(manifest, logs)
	if err != nil {
		return legacy.State{}, nil, err
	}
	nodeIDs := make([]string, 0, len(tmpl.Nodes))
	for id := range tmpl.Nodes {
		nodeIDs = append(nodeIDs, id)
	}
	slices.Sort(nodeIDs)
	nodes := make([]legacy.NodeInit, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		status := legacy.NodeStatusPending
		if id == tmpl.Start {
			status = legacy.NodeStatusReady
		}
		nodes = append(nodes, legacy.NodeInit{ID: id, Type: tmpl.Nodes[id].Type, Status: status})
	}
	replayed := legacy.New(runID, originalTemplateRef, currentTemplateRef, nodes)
	replayed.Status = legacy.RunStatusRunning
	manifestBySeq := make(map[int64]evidence.ManifestEntry, len(manifest))
	for _, item := range manifest {
		manifestBySeq[item.Seq] = item
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return legacy.State{}, nil, err
		}
		manifestEntry := manifestBySeq[entry.Seq]
		if entry.Event != nil {
			event := *entry.Event
			event.Seq = entry.Seq
			event.At = entry.At
			event.LogChecksum = manifestEntry.Checksum
			replayed, err = legacy.Apply(replayed, event)
			if err != nil {
				return legacy.State{}, nil, fmt.Errorf("%w: replay legacy evidence seq %d: %v", ErrInitializationInconsistent, entry.Seq, err)
			}
		} else {
			replayed.LastLogSeq = entry.Seq
			replayed.LogChecksum = manifestEntry.Checksum
		}
	}
	data, err := legacy.Encode(&replayed)
	if err != nil {
		return legacy.State{}, nil, err
	}
	return replayed, data, nil
}

// VerifyMigratedLegacyEvidence proves that bounded, unchanged schema-6
// evidence reconstructs the migration anchor retained by a schema-7
// checkpoint. The returned legacy state is historical report input only; it
// is never current routing or status authority.
func VerifyMigratedLegacyEvidence(
	ctx context.Context,
	checkpoint *CheckpointV7,
	tmpl *model.Template,
	manifest []evidence.ManifestEntry,
	logs []evidence.NodeLog,
) (*legacy.State, error) {
	if err := ValidateCheckpointV7(checkpoint); err != nil {
		return nil, err
	}
	if checkpoint.Execution == nil || checkpoint.Execution.LegacyProjection == nil {
		return nil, fmt.Errorf("%w: checkpoint has no migrated legacy projection", ErrInitializationInvalid)
	}
	needed := checkpoint.Initialize.UpgradeNeeded
	replayed, replayedJSON, err := ReplayLegacyProjectionEvidence(
		ctx, needed.RunID, needed.TemplateRef, needed.TemplateRef, tmpl, manifest, logs,
	)
	if err != nil {
		return nil, err
	}
	rebuilt, err := BuildProgressedInitialization(ctx, LegacyProjectionInput{
		UpgradeNeeded: needed, Template: tmpl, LegacyState: &replayed,
		LegacyCheckpointJSON: replayedJSON, Manifest: manifest, NodeLogs: logs,
	})
	if err != nil {
		return nil, err
	}
	if rebuilt.Execution == nil || !reflect.DeepEqual(rebuilt.Execution.LegacyProjection, checkpoint.Execution.LegacyProjection) {
		return nil, fmt.Errorf("%w: migrated legacy evidence differs from the retained projection anchor", ErrInitializationInconsistent)
	}
	return &replayed, nil
}

func orderedLegacyEntries(manifest []evidence.ManifestEntry, logs []evidence.NodeLog) ([]evidence.LogEntry, error) {
	if diagnostics := evidence.VerifySequence(manifest, logs); diagnostics.HasErrors() {
		return nil, fmt.Errorf("%w: legacy evidence diagnostics=%v", ErrInitializationInconsistent, diagnostics.Errors())
	}
	bySeq := make(map[int64]evidence.LogEntry, len(manifest))
	for _, log := range logs {
		for _, entry := range log.Entries {
			bySeq[entry.Seq] = entry
		}
	}
	entries := make([]evidence.LogEntry, 0, len(manifest))
	for _, item := range manifest {
		entry, ok := bySeq[item.Seq]
		if !ok {
			return nil, fmt.Errorf("%w: manifest seq %d lacks a log entry", ErrInitializationInconsistent, item.Seq)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func validateProjectionTopology(tmpl *model.Template, st *legacy.State) error {
	if tmpl == nil || st == nil || tmpl.Start == "" || len(st.Nodes) != len(tmpl.Nodes) {
		return refuseProjection(ProjectionRefusalTopology, "template/state node sets differ")
	}
	for id, node := range tmpl.Nodes {
		if node.IsCompound() || node.Type == model.NodeTypeParallel || node.Join != "" {
			return refuseProjection(ProjectionRefusalTopology, "node %q is compound or parallel", id)
		}
		legacyNode, ok := st.Nodes[id]
		if !ok || legacyNode.Parent != "" || len(legacyNode.Children) != 0 || legacyNode.BlockResolution != nil || legacyNode.Status == legacy.NodeStatusBlocked {
			return refuseProjection(ProjectionRefusalTopology, "node %q has unsupported expanded, blocked, or missing state", id)
		}
	}
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var walk func(string) bool
	walk = func(id string) bool {
		if visiting[id] {
			return true
		}
		if visited[id] {
			return false
		}
		visiting[id] = true
		for _, next := range tmpl.Nodes[id].Next {
			if walk(next) {
				return true
			}
		}
		visiting[id] = false
		visited[id] = true
		return false
	}
	if walk(tmpl.Start) {
		return refuseProjection(ProjectionRefusalTopology, "template contains a reachable cycle")
	}
	return nil
}

type legacyProjector struct {
	ctx        context.Context
	tmpl       *model.Template
	legacy     *legacy.State
	entries    []evidence.LogEntry
	eventSeq   int64
	checkpoint *CheckpointV7
}

func (p *legacyProjector) project() (AggregateCheckpoint, error) {
	aggregate, err := CurrentAggregateCheckpoint(p.checkpoint)
	if err != nil {
		return AggregateCheckpoint{}, err
	}
	seen := map[string]bool{}
	for {
		if err := p.ctx.Err(); err != nil {
			return AggregateCheckpoint{}, err
		}
		nodeID, sourcePath, err := liveProjectedNode(aggregate)
		if err != nil {
			return AggregateCheckpoint{}, err
		}
		if seen[nodeID] {
			return AggregateCheckpoint{}, refuseProjection(ProjectionRefusalTopology, "projected route revisits node %q", nodeID)
		}
		seen[nodeID] = true
		node := p.tmpl.Nodes[nodeID]
		legacyNode := p.legacy.Nodes[nodeID]
		if node.Type == model.NodeTypeEnd {
			return AggregateCheckpoint{}, refuseProjection(ProjectionRefusalFrontier, "legacy nonterminal run reached end node %q", nodeID)
		}
		if legacyNode.Status == legacy.NodeStatusReady {
			if err := compareProjectedFrontier(aggregate, p.legacy, seen, nodeID); err != nil {
				return AggregateCheckpoint{}, err
			}
			return aggregate, nil
		}
		var observation ExclusiveObservation
		switch node.Type {
		case model.NodeTypeStart:
			if legacyNode.Status != legacy.NodeStatusCompleted {
				return AggregateCheckpoint{}, refuseProjection(ProjectionRefusalFrontier, "start node is %q", legacyNode.Status)
			}
			observation = ExclusiveObservation{SourcePathID: sourcePath, Attempt: 1, Outcome: "pass"}
		case model.NodeTypeTask, model.NodeTypeDecision:
			observation, aggregate, err = p.projectPerformerNode(aggregate, nodeID, sourcePath)
		case model.NodeTypeWait:
			observation, aggregate, err = p.projectWaitNode(aggregate, nodeID, sourcePath)
		default:
			err = refuseProjection(ProjectionRefusalTopology, "node %q type %q is unsupported", nodeID, node.Type)
		}
		if err != nil {
			return AggregateCheckpoint{}, err
		}
		aggregate, err = p.routeAggregate(aggregate, observation)
		if err != nil {
			return AggregateCheckpoint{}, err
		}
	}
}

func (p *legacyProjector) routeAggregate(aggregate AggregateCheckpoint, observation ExclusiveObservation) (AggregateCheckpoint, error) {
	input := p.projectedInput(aggregate)
	sequence, err := PlanExclusiveRouteSequence(p.ctx, input, observation)
	if err != nil {
		return AggregateCheckpoint{}, refuseProjection(ProjectionRefusalHistory, "route observation cannot be reconstructed: %v", err)
	}
	projection, err := ReduceExclusiveRouteSequence(p.ctx, input, observation, sequence.Commands())
	if err != nil {
		return AggregateCheckpoint{}, fmt.Errorf("%w: reduce projected route: %v", ErrInitializationInconsistent, err)
	}
	return projection.aggregate, nil
}

func (p *legacyProjector) projectedInput(aggregate AggregateCheckpoint) *VerifiedExclusiveInput {
	checkpoint := *p.checkpoint
	checkpoint.Execution = &ExecutionCheckpoint{Aggregate: aggregate, Status: "running", LastLogSeq: uint64(p.eventSeq), LogChecksum: p.checkpoint.Digest}
	return &VerifiedExclusiveInput{
		checkpoint: &checkpoint, template: p.tmpl, binding: CurrentCheckpointBinding(p.checkpoint),
		projectionEventSeq: p.eventSeq,
	}
}

type legacyAttemptEvidence struct {
	attempt     uint64
	command     plan.Command
	observation ExclusiveObservation
	contact     *legacy.ContactState
	scheduledAt time.Time
}

func (p *legacyProjector) projectPerformerNode(aggregate AggregateCheckpoint, nodeID string, sourcePath PathID) (ExclusiveObservation, AggregateCheckpoint, error) {
	attempts, err := p.performerEvidence(nodeID)
	if err != nil {
		return ExclusiveObservation{}, AggregateCheckpoint{}, err
	}
	if len(attempts) == 0 {
		return ExclusiveObservation{}, AggregateCheckpoint{}, refuseProjection(ProjectionRefusalHistory, "node %q has no settled performer evidence", nodeID)
	}
	for index, item := range attempts {
		item.observation.SourcePathID = sourcePath
		attempts[index].observation.SourcePathID = sourcePath
		input := p.projectedInput(aggregate)
		planned, planErr := PlanExclusiveAttempt(p.ctx, input, sourcePath, item.attempt, item.command.Params)
		if planErr != nil {
			return ExclusiveObservation{}, AggregateCheckpoint{}, refuseProjection(ProjectionRefusalAuthority, "node %q attempt %d cannot be rebound: %v", nodeID, item.attempt, planErr)
		}
		if item.command.ParamsBound && (!reflect.DeepEqual(planned.Params(), item.command.Params) || !reflect.DeepEqual(planned.Performer(), item.command.Performer)) {
			return ExclusiveObservation{}, AggregateCheckpoint{}, refuseProjection(ProjectionRefusalAuthority, "node %q attempt %d payload differs from exact template", nodeID, item.attempt)
		}
		perform := planned.Command()
		perform.State = CommandObserved
		aggregate.Commands[perform.ID] = perform
		view := aggregate.View()
		source := view.Routing.Paths[sourcePath]
		performRecord, settle, effect, buildErr := observedAttemptCommands(view, nodeID, p.tmpl.Nodes[nodeID], source, item.observation, false)
		if buildErr != nil {
			return ExclusiveObservation{}, AggregateCheckpoint{}, refuseProjection(ProjectionRefusalHistory, "node %q attempt %d observation is invalid: %v", nodeID, item.attempt, buildErr)
		}
		aggregate.Commands[performRecord.ID] = performRecord
		aggregate.Commands[settle.ID] = settle
		aggregate.SideEffects[effect.ID] = effect
		if item.contact != nil {
			if err := projectLegacyContact(&aggregate, performRecord, *item.contact, item.scheduledAt, p.eventSeq); err != nil {
				return ExclusiveObservation{}, AggregateCheckpoint{}, err
			}
		}
		disposition, classifyErr := classifyExclusiveObservation(aggregate.View(), p.tmpl, item.observation, false)
		last := index == len(attempts)-1
		if !last && (classifyErr != nil || disposition != ExclusiveRetryPending) {
			return ExclusiveObservation{}, AggregateCheckpoint{}, refuseProjection(ProjectionRefusalHistory, "node %q attempt %d is not an exact retry", nodeID, item.attempt)
		}
		if last && (classifyErr != nil || disposition != ExclusiveRouteReady) {
			return ExclusiveObservation{}, AggregateCheckpoint{}, refuseProjection(ProjectionRefusalHistory, "node %q final attempt is not routable", nodeID)
		}
	}
	return attempts[len(attempts)-1].observation, aggregate, nil
}

func (p *legacyProjector) performerEvidence(nodeID string) ([]legacyAttemptEvidence, error) {
	starts := map[int]legacy.Event{}
	settles := map[int]legacy.Event{}
	settleSources := map[int]string{}
	issued := map[string]legacy.OutstandingCommand{}
	observed := map[string]legacy.Event{}
	var pendingSettles []plan.Command
	contacts := map[string][]struct {
		state legacy.ContactState
		at    time.Time
	}{}
	for _, entry := range p.entries {
		event := entry.Event
		if event == nil {
			continue
		}
		switch event.Type {
		case legacy.EventCommandIssued:
			if event.Command != nil {
				issued[event.Command.ID] = *event.Command
			}
		case legacy.EventCommandObserved:
			observed[event.CommandID] = *event
			if command, ok := issued[event.CommandID]; ok && command.Kind == legacy.CommandKindSettleAttempt {
				var planned plan.Command
				if err := json.Unmarshal(command.Payload, &planned); err != nil || planned.ID != command.ID ||
					planned.Kind != plan.CommandKindSettleAttempt || planned.NodeID != command.NodeID || planned.Attempt <= 0 || planned.SourceCommandID == "" {
					return nil, refuseProjection(ProjectionRefusalAuthority, "node %q has an inconsistent settle command payload", command.NodeID)
				}
				pendingSettles = append(pendingSettles, planned)
			}
		case legacy.EventNodeAttemptStarted:
			if event.NodeID == nodeID {
				starts[event.Attempt] = *event
			}
		case legacy.EventNodeAttemptSettled:
			if event.NodeID == nodeID {
				attempt := event.Attempt
				if attempt <= 0 {
					if len(pendingSettles) == 0 {
						return nil, refuseProjection(ProjectionRefusalAuthority, "node %q settlement lacks observed settle-command authority", nodeID)
					}
					settle := pendingSettles[len(pendingSettles)-1]
					pendingSettles = pendingSettles[:len(pendingSettles)-1]
					if settle.NodeID != nodeID {
						return nil, refuseProjection(ProjectionRefusalAuthority, "node %q settlement follows settle command for %q", nodeID, settle.NodeID)
					}
					attempt = settle.Attempt
					settleSources[attempt] = settle.SourceCommandID
				}
				if _, duplicate := settles[attempt]; duplicate {
					return nil, refuseProjection(ProjectionRefusalHistory, "node %q attempt %d has duplicate settlements", nodeID, attempt)
				}
				settles[attempt] = *event
			}
		case legacy.EventContactScheduled:
			if event.Contact != nil {
				contacts[event.Contact.CommandID] = append(contacts[event.Contact.CommandID], struct {
					state legacy.ContactState
					at    time.Time
				}{*event.Contact, entry.At})
			}
		}
	}
	if p.tmpl.Nodes[nodeID].Type == model.NodeTypeDecision {
		return p.decisionEvidence(nodeID, issued, observed, contacts)
	}
	attemptNumbers := make([]int, 0, len(settles))
	for attempt := range settles {
		attemptNumbers = append(attemptNumbers, attempt)
	}
	slices.Sort(attemptNumbers)
	result := make([]legacyAttemptEvidence, 0, len(attemptNumbers))
	for index, attempt := range attemptNumbers {
		if attempt != index+1 {
			return nil, refuseProjection(ProjectionRefusalHistory, "node %q attempt sequence is not contiguous", nodeID)
		}
		start, ok := starts[attempt]
		if !ok || start.CommandID == "" {
			return nil, refuseProjection(ProjectionRefusalAuthority, "node %q attempt %d lacks start authority", nodeID, attempt)
		}
		if sourceID := settleSources[attempt]; sourceID != "" && sourceID != start.CommandID {
			return nil, refuseProjection(ProjectionRefusalAuthority, "node %q attempt %d settlement names source command %q, want %q", nodeID, attempt, sourceID, start.CommandID)
		}
		command, ok := issued[start.CommandID]
		if !ok || command.Kind != legacy.CommandKindStartAttempt || len(command.Payload) == 0 {
			return nil, refuseProjection(ProjectionRefusalAuthority, "node %q attempt %d lacks issued command payload", nodeID, attempt)
		}
		var planned plan.Command
		if err := json.Unmarshal(command.Payload, &planned); err != nil || planned.ID != command.ID || planned.NodeID != nodeID || planned.Attempt != attempt {
			return nil, refuseProjection(ProjectionRefusalAuthority, "node %q attempt %d command payload is inconsistent", nodeID, attempt)
		}
		observationEvent, ok := observed[command.ID]
		settled := settles[attempt]
		if !ok || strings.TrimSpace(observationEvent.Outcome) == "" || settled.Outcome != observationEvent.Outcome || settled.Actor != observationEvent.Actor || settled.EvidenceRef != observationEvent.EvidenceRef || settled.EvidenceHash != observationEvent.EvidenceHash {
			return nil, refuseProjection(ProjectionRefusalAuthority, "node %q attempt %d observation/settlement authority differs", nodeID, attempt)
		}
		item := legacyAttemptEvidence{
			attempt: uint64(attempt), command: planned,
			observation: ExclusiveObservation{SourcePathID: "", Attempt: uint64(attempt), Outcome: settled.Outcome, Actor: string(settled.Actor), EvidenceRef: settled.EvidenceRef, EvidenceHash: settled.EvidenceHash, ExternalRef: observationEvent.ExternalRef, Feedback: settled.Feedback},
		}
		if current, ok := p.legacy.Contacts[command.ID]; ok {
			schedules := contacts[command.ID]
			if len(schedules) != 1 {
				return nil, refuseProjection(ProjectionRefusalContactHistory, "contact %q has %d schedule events", command.ID, len(schedules))
			}
			item.contact = &current
			item.scheduledAt = schedules[0].at
		}
		result = append(result, item)
	}
	return result, nil
}

func (p *legacyProjector) decisionEvidence(nodeID string, issued map[string]legacy.OutstandingCommand, observed map[string]legacy.Event, contacts map[string][]struct {
	state legacy.ContactState
	at    time.Time
}) ([]legacyAttemptEvidence, error) {
	var decision *legacy.Event
	for _, entry := range p.entries {
		if entry.Event != nil && entry.Event.Type == legacy.EventDecisionRecorded && entry.Event.NodeID == nodeID {
			if decision != nil {
				return nil, refuseProjection(ProjectionRefusalHistory, "decision %q has duplicate verdict evidence", nodeID)
			}
			copy := *entry.Event
			decision = &copy
		}
	}
	if decision == nil {
		return nil, refuseProjection(ProjectionRefusalHistory, "decision %q lacks verdict evidence", nodeID)
	}
	var source legacy.OutstandingCommand
	var sourceObservation legacy.Event
	for id, command := range issued {
		if command.NodeID != nodeID || command.Kind != legacy.CommandKindRecordDecision {
			continue
		}
		if source.ID != "" {
			return nil, refuseProjection(ProjectionRefusalAuthority, "decision %q has duplicate commands", nodeID)
		}
		source = command
		sourceObservation = observed[id]
	}
	if source.ID == "" || sourceObservation.CommandID == "" || sourceObservation.Outcome != decision.Outcome || len(source.Payload) == 0 {
		return nil, refuseProjection(ProjectionRefusalAuthority, "decision %q lacks exact command observation", nodeID)
	}
	var command plan.Command
	if err := json.Unmarshal(source.Payload, &command); err != nil || command.ID != source.ID || command.NodeID != nodeID {
		return nil, refuseProjection(ProjectionRefusalAuthority, "decision %q command payload is inconsistent", nodeID)
	}
	item := legacyAttemptEvidence{
		attempt: 1, command: command,
		observation: ExclusiveObservation{Attempt: 1, Outcome: decision.Outcome, Actor: string(decision.Actor), EvidenceRef: decision.EvidenceRef, EvidenceHash: sourceObservation.EvidenceHash, ExternalRef: sourceObservation.ExternalRef},
	}
	if current, ok := p.legacy.Contacts[source.ID]; ok {
		schedules := contacts[source.ID]
		if len(schedules) != 1 {
			return nil, refuseProjection(ProjectionRefusalContactHistory, "contact %q has %d schedule events", source.ID, len(schedules))
		}
		item.contact = &current
		item.scheduledAt = schedules[0].at
	}
	return []legacyAttemptEvidence{item}, nil
}

func (p *legacyProjector) projectWaitNode(aggregate AggregateCheckpoint, nodeID string, sourcePath PathID) (ExclusiveObservation, AggregateCheckpoint, error) {
	var created, satisfied *evidence.LogEntry
	for index := range p.entries {
		entry := &p.entries[index]
		if entry.Event == nil || entry.Event.NodeID != nodeID {
			continue
		}
		switch entry.Event.Type {
		case legacy.EventWaitCreated, legacy.EventTimerCreated:
			if created != nil {
				return ExclusiveObservation{}, AggregateCheckpoint{}, refuseProjection(ProjectionRefusalHistory, "wait %q has duplicate creation evidence", nodeID)
			}
			created = entry
		case legacy.EventWaitSatisfied, legacy.EventTimerSatisfied:
			if satisfied != nil {
				return ExclusiveObservation{}, AggregateCheckpoint{}, refuseProjection(ProjectionRefusalHistory, "wait %q has duplicate satisfaction evidence", nodeID)
			}
			satisfied = entry
		}
	}
	if created == nil || satisfied == nil {
		return ExclusiveObservation{}, AggregateCheckpoint{}, refuseProjection(ProjectionRefusalHistory, "wait %q lacks complete satisfied evidence", nodeID)
	}
	input := p.projectedInput(aggregate)
	planned, err := PlanExclusiveWait(p.ctx, input, sourcePath, created.At)
	if err != nil {
		return ExclusiveObservation{}, AggregateCheckpoint{}, refuseProjection(ProjectionRefusalAuthority, "wait %q cannot be rebound: %v", nodeID, err)
	}
	if created.Event.Timer != nil && (!planned.DueAt().Equal(created.Event.Timer.DueAt) || !created.Event.Timer.CreatedAt.Equal(created.At)) {
		return ExclusiveObservation{}, AggregateCheckpoint{}, refuseProjection(ProjectionRefusalAuthority, "wait %q timer schedule differs", nodeID)
	}
	command := planned.Command()
	command.State = CommandObserved
	aggregate.Commands[command.ID] = command
	observation := ExclusiveObservation{SourcePathID: sourcePath, Attempt: 1, Outcome: "satisfied", Actor: "engine:migration", EvidenceRef: created.Event.EvidenceRef}
	perform, settle, effect, err := observedAttemptCommands(aggregate.View(), nodeID, p.tmpl.Nodes[nodeID], aggregate.Routing.Paths[sourcePath], observation, false)
	if err != nil {
		return ExclusiveObservation{}, AggregateCheckpoint{}, refuseProjection(ProjectionRefusalHistory, "wait %q observation is invalid: %v", nodeID, err)
	}
	aggregate.Commands[perform.ID] = perform
	aggregate.Commands[settle.ID] = settle
	aggregate.SideEffects[effect.ID] = effect
	return observation, aggregate, nil
}

func projectLegacyContact(aggregate *AggregateCheckpoint, perform CommandRecord, source legacy.ContactState, scheduledAt time.Time, eventSeq int64) error {
	if aggregate == nil || scheduledAt.IsZero() || source.Budget <= 0 || source.Used < 0 {
		return refuseProjection(ProjectionRefusalContactHistory, "legacy contact lacks a complete schedule")
	}
	kind, ok := contactKindForAssignee(source.Assignee)
	if !ok {
		return refuseProjection(ProjectionRefusalContactHistory, "legacy contact assignee %q is unsupported", source.Assignee)
	}
	record := ContactRecordV7{
		RunID: aggregate.RunID, ActivationID: perform.Identity.SourceActivationID, Attempt: perform.Identity.Attempt,
		SourceCommandID: perform.ID, Assignee: source.Assignee, Kind: kind,
		Provenance: ContactProvenanceLegacyProjection, Cadence: source.Cadence,
		Budget: uint64(source.Budget), Used: uint64(source.Used), EscalationTarget: source.EscalationTarget,
		ScheduledAt: CanonicalTimestamp(scheduledAt), LastContactedAt: CanonicalTimestamp(source.LastContactedAt),
		NextContactAt: CanonicalTimestamp(source.NextContactAt), LastRecoveredAt: CanonicalTimestamp(source.LastRecoveredAt),
		EscalatedAt: CanonicalTimestamp(source.EscalatedAt), LegacyPauseReason: source.PauseReason,
		HumanInteractedAt: CanonicalTimestamp(source.HumanInteractedAt), EventSeq: eventSeq,
	}
	record.ID, _ = ContactIdentity(record.RunID, record.ActivationID, record.Attempt, record.Assignee)
	if err := ValidateContactRecord(record); err != nil {
		return refuseProjection(ProjectionRefusalContactHistory, "%v", err)
	}
	if aggregate.Contacts == nil {
		aggregate.Contacts = map[string]ContactRecordV7{}
	}
	aggregate.Contacts[record.ID] = record
	aggregate.SideEffects[record.ID] = SideEffectIdentity{
		Kind: SideEffectContact, ID: record.ID, RunID: record.RunID, ActivationID: record.ActivationID,
		Attempt: record.Attempt, Assignee: record.Assignee,
		State: ContactStateCompleted,
	}
	return nil
}

func liveProjectedNode(aggregate AggregateCheckpoint) (string, PathID, error) {
	var nodeID string
	var pathID PathID
	for id, path := range aggregate.Routing.Paths {
		if path.Kind != PathActivationOutput || path.State != PathLive {
			continue
		}
		activation, ok := aggregate.Routing.Activations[path.SourceActivation.ID]
		if !ok {
			return "", "", fmt.Errorf("%w: live path lacks activation", ErrInitializationInconsistent)
		}
		reservation, ok := aggregate.Routing.Reservations[activation.ReservationID]
		if !ok || nodeID != "" {
			return "", "", refuseProjection(ProjectionRefusalFrontier, "projected aggregate has an ambiguous live frontier")
		}
		nodeID, pathID = reservation.NodeID, id
	}
	if nodeID == "" {
		return "", "", refuseProjection(ProjectionRefusalFrontier, "projected aggregate has no live frontier")
	}
	return nodeID, pathID, nil
}

func compareProjectedFrontier(aggregate AggregateCheckpoint, st *legacy.State, visited map[string]bool, frontier string) error {
	for id, node := range st.Nodes {
		switch {
		case id == frontier:
			if node.Status != legacy.NodeStatusReady {
				return refuseProjection(ProjectionRefusalStateMismatch, "frontier node %q is %q", id, node.Status)
			}
		case visited[id]:
			if node.Status != legacy.NodeStatusCompleted && node.Status != legacy.NodeStatusFailed {
				return refuseProjection(ProjectionRefusalStateMismatch, "visited node %q is %q", id, node.Status)
			}
		default:
			if node.Status != legacy.NodeStatusPending {
				return refuseProjection(ProjectionRefusalStateMismatch, "unvisited node %q is %q", id, node.Status)
			}
		}
	}
	return nil
}
