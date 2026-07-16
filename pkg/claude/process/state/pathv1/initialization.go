package pathv1

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	legacy "github.com/tofutools/tclaude/pkg/claude/process/state"
)

const (
	CheckpointStateSchemaVersion = 7
	InitializeEventKind          = "routing_initialized_from_legacy_v1"
)

var (
	ErrInitializationInvalid      = errors.New("path-v1 initialization is invalid")
	ErrInitializationAmbiguous    = errors.New("path-v1 initialization is ambiguous")
	ErrInitializationInconsistent = errors.New("path-v1 initialization replay is inconsistent")
	ErrCheckpointSchemaNewer      = errors.New("path-v1 checkpoint schema is newer than this binary supports")
	ErrCheckpointSchemaInvalid    = errors.New("path-v1 checkpoint schema is invalid")
)

// CheckpointV7 is the indivisible schema-7 state. The complete initialization
// event is deliberately singular: schema 7 has no representation for a
// partial or multi-event v6-to-v7 transition.
type CheckpointV7 struct {
	StateSchemaVersion int             `json:"stateSchemaVersion"`
	Initialize         InitializeEvent `json:"initialize"`
	// Execution is the mutable, append-only-revision execution head. Older
	// installed schema-7 checkpoints omit it; their validated initialization
	// aggregate is revision zero. Initialize is never rewritten.
	Execution *ExecutionCheckpoint `json:"execution,omitempty"`
	Digest    string               `json:"digest"`
}

// InitializeEvent contains every durable record created by the transition.
// Its UpgradeNeeded value is the exact detached authority consumed while the
// legacy checkpoint was still protected by the append lock.
type InitializeEvent struct {
	Kind            string              `json:"kind"`
	EventSeq        int64               `json:"eventSeq"`
	UpgradeNeeded   UpgradeNeeded       `json:"upgradeNeeded"`
	TemplateHash    string              `json:"templateHash"`
	Command         CommandRecord       `json:"command"`
	AdminRecord     PathV1AdminRecord   `json:"adminRecord"`
	Aggregate       AggregateCheckpoint `json:"aggregate"`
	AggregateDigest string              `json:"aggregateDigest"`
}

// AggregateCheckpoint is the complete persisted path-v1 aggregate. Static
// exact-template authority remains outside RoutingState, but is included in
// the atomic schema-7 container so no partially authoritative state exists.
type AggregateCheckpoint struct {
	RunID              string                        `json:"runId"`
	TemplateRef        string                        `json:"templateRef"`
	TemplateSourceHash string                        `json:"templateSourceHash"`
	Authority          AggregateAuthorityCheckpoint  `json:"authority"`
	Routing            RoutingState                  `json:"routing"`
	Commands           map[string]CommandRecord      `json:"commands"`
	SideEffects        map[string]SideEffectIdentity `json:"sideEffects"`
	AdminRecords       map[string]PathV1AdminRecord  `json:"adminRecords"`
	AdminResolutions   map[string]BlockResolution    `json:"adminResolutions"`
}

type AggregateAuthorityCheckpoint struct {
	RunID              string                                 `json:"runId"`
	TemplateRef        string                                 `json:"templateRef"`
	TemplateSourceHash string                                 `json:"templateSourceHash"`
	Genesis            GenesisAuthority                       `json:"genesis"`
	Scopes             map[ScopeID]ScopeAuthority             `json:"scopes"`
	Reservations       map[ReservationID]ReservationAuthority `json:"reservations"`
}

type InitializeRoutingPayload struct {
	UpgradeNeeded   UpgradeNeeded    `json:"upgradeNeeded"`
	TemplateHash    string           `json:"templateHash"`
	Genesis         GenesisAuthority `json:"genesis"`
	AggregateDigest string           `json:"aggregateDigest"`
}

type InitializationDisposition string

const (
	InitializationApplied        InitializationDisposition = "applied"
	InitializationAlreadyApplied InitializationDisposition = "already_applied"
)

// ValidateUnambiguousLegacyInitialization limits the schema-7 release
// to the one legacy checkpoint whose complete path-v1 meaning is unique: the
// pristine, newly instantiated exclusive run. Progressed-but-quiescent legacy
// histories remain schema 6 for the later parity migrator.
func ValidateUnambiguousLegacyInitialization(st *legacy.State, tmpl *model.Template) error {
	if st == nil || tmpl == nil {
		return fmt.Errorf("%w: legacy state and exact template are required", ErrInitializationInvalid)
	}
	if st.StateSchemaVersion <= 0 || st.StateSchemaVersion > LegacyMaxSchemaVersion {
		return fmt.Errorf("%w: legacy schema is outside 1-%d", ErrInitializationInvalid, LegacyMaxSchemaVersion)
	}
	if st.RunID == "" || st.Status != legacy.RunStatusRunning || st.Pause != nil || st.TemplateDivergence != nil ||
		st.OriginalTemplateRef != st.CurrentTemplateRef || st.LastLogSeq != 0 || st.LogChecksum != "" {
		return fmt.Errorf("%w: legacy run is not a pristine running checkpoint", ErrInitializationAmbiguous)
	}
	if len(st.OutstandingCommands) != 0 || len(st.Waits) != 0 || len(st.Timers) != 0 ||
		len(st.Obligations) != 0 || len(st.Contacts) != 0 || len(st.AdminRecords) != 0 {
		return fmt.Errorf("%w: legacy checkpoint contains history or side effects", ErrInitializationAmbiguous)
	}
	if len(st.Nodes) != len(tmpl.Nodes) {
		return fmt.Errorf("%w: legacy node set differs from the exact template", ErrInitializationAmbiguous)
	}
	for nodeID, node := range tmpl.Nodes {
		want := legacy.NodeState{Type: node.Type, Status: legacy.NodeStatusPending}
		if nodeID == tmpl.Start {
			want.Status = legacy.NodeStatusReady
		}
		got, ok := st.Nodes[nodeID]
		if !ok || !reflect.DeepEqual(got, want) {
			return fmt.Errorf("%w: legacy node %q has progressed or ambiguous state", ErrInitializationAmbiguous, nodeID)
		}
	}
	return nil
}

// BuildInitialization constructs and validates the complete deterministic
// transition. It performs no I/O and does not authorize a caller to persist
// the result without rechecking UpgradeNeeded under the append lock.
func BuildInitialization(ctx context.Context, needed UpgradeNeeded, tmpl *model.Template) (*CheckpointV7, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ValidateUpgradeNeeded(needed); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInitializationInvalid, err)
	}
	if needed.Reason != UpgradeMigrationRequired || len(needed.ActiveLegacyIDs) != 0 || len(needed.CheckpointAdminRecords) != 0 {
		return nil, fmt.Errorf("%w: legacy drain is required", ErrInitializationInvalid)
	}
	if tmpl == nil || tmpl.Start == "" || tmpl.Nodes[tmpl.Start].Type == "" {
		return nil, fmt.Errorf("%w: exact template has no valid start node", ErrInitializationInvalid)
	}
	edges, cardinalityDiagnostics := model.NormalizeEdgesWithinBudget(tmpl)
	if cardinalityDiagnostics.HasErrors() {
		return nil, fmt.Errorf("%w: exact template is invalid", ErrInitializationInvalid)
	}
	if diagnostics := model.Validate(tmpl, edges); diagnostics.HasErrors() {
		return nil, fmt.Errorf("%w: exact template is invalid", ErrInitializationInvalid)
	}
	id, templateHash, ok := splitExactTemplateRef(needed.TemplateRef)
	if !ok || id != tmpl.ID {
		return nil, fmt.Errorf("%w: exact template ref mismatch", ErrInitializationInvalid)
	}
	semanticHash, err := model.SemanticHash(tmpl)
	if err != nil || semanticHash != templateHash {
		return nil, fmt.Errorf("%w: exact template semantic hash mismatch", ErrInitializationInvalid)
	}

	const generation uint64 = 1
	eventSeq := int64(needed.Checkpoint.Generation + 1)
	rootScopeID, err := ScopeIdentity(needed.RunID, "", "", "", "", generation)
	if err != nil {
		return nil, err
	}
	reservationID, err := ReservationIdentity(needed.RunID, tmpl.Start, rootScopeID, "", generation)
	if err != nil {
		return nil, err
	}
	inputDigest, err := InputSetIdentity(nil)
	if err != nil {
		return nil, err
	}
	activationID, err := ActivationIdentity(needed.RunID, reservationID, generation, inputDigest)
	if err != nil {
		return nil, err
	}
	outputPathID, err := ActivationOutputIdentity(activationID, generation)
	if err != nil {
		return nil, err
	}
	genesis := GenesisAuthority{
		RootScopeID: rootScopeID, StartNodeID: tmpl.Start, ReservationID: reservationID,
		ActivationID: activationID, OutputPathID: outputPathID, Generation: generation,
	}

	routing := NewRoutingState()
	authority := AggregateAuthorityCheckpoint{
		RunID: needed.RunID, TemplateRef: templateHash, TemplateSourceHash: needed.TemplateSourceHash,
		Genesis: genesis,
		Scopes: map[ScopeID]ScopeAuthority{rootScopeID: {
			ID: rootScopeID, Generation: generation, ExpectedBranchEdgeIDs: []EdgeID{},
		}},
		Reservations: map[ReservationID]ReservationAuthority{reservationID: {
			ID: reservationID, NodeID: tmpl.Start, ScopeID: rootScopeID, Generation: generation,
			JoinPolicy: JoinExclusive, Candidates: []CandidateRecord{}, PossibleSlots: []PossibleSlotRecord{},
		}},
	}

	// The digest is defined by the existing aggregate fold contract. Genesis
	// has one live path and activated reservation and no propagation/effects.
	pathDigest, err := PathFoldIdentity([]PathFoldEntry{{PathID: outputPathID, State: PathLive, UpdatedSeq: uint64(eventSeq)}})
	if err != nil {
		return nil, err
	}
	reservationDigest, err := ReservationFoldIdentity([]ReservationFoldEntry{{ReservationID: reservationID, State: ReservationActivated, EventSeq: uint64(eventSeq)}})
	if err != nil {
		return nil, err
	}
	propagationDigest, err := PropagationFoldIdentity(nil)
	if err != nil {
		return nil, err
	}
	sideEffectDigest, err := SideEffectFoldIdentity(nil)
	if err != nil {
		return nil, err
	}
	terminalCauseDigest, err := CauseSetIdentity(nil)
	if err != nil {
		return nil, err
	}
	aggregateDigest, err := AggregateIdentity(needed.RunID, templateHash, needed.Checkpoint.Digest, pathDigest, reservationDigest, propagationDigest, sideEffectDigest, terminalCauseDigest)
	if err != nil {
		return nil, err
	}
	payload := InitializeRoutingPayload{UpgradeNeeded: cloneUpgradeNeeded(needed), TemplateHash: templateHash, Genesis: genesis, AggregateDigest: aggregateDigest}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	identity := CommandIdentity{
		RunID: needed.RunID, Kind: CommandInitializeRouting, PayloadSchema: 1,
		InputDigest: needed.Checkpoint.Digest, PlanDigest: aggregateDigest,
	}
	commandID, err := CommandIdentityDigest(identity)
	if err != nil {
		return nil, err
	}
	payloadHash := sha256.Sum256(payloadJSON)
	command := CommandRecord{
		ID: commandID, IdempotencyKey: CommandIdempotencyKey(identity.Kind, commandID), Identity: identity,
		Payload: payloadJSON, PayloadHash: hex.EncodeToString(payloadHash[:]), State: CommandObserved,
	}
	admin := PathV1AdminRecord{
		RunID: needed.RunID, EventSeq: eventSeq, AdminType: InitializeEventKind,
		Actor: "system:migration", ReasonCode: "upgrade_needed", EvidenceRef: commandID,
	}
	admin.ID, err = AdminRecordIdentity(admin)
	if err != nil {
		return nil, err
	}
	activationRef := ActivationRef{ID: activationID, Generation: generation}
	receiptID, err := ActivationReceiptIdentity(activationID, reservationID, inputDigest, outputPathID, commandID, uint64(eventSeq))
	if err != nil {
		return nil, err
	}
	receipt := ActivationReceipt{
		ID: receiptID, ActivationID: activationID, ReservationID: reservationID, InputSetDigest: inputDigest,
		OutputPathID: outputPathID, ScopeID: rootScopeID, JoinPolicy: JoinExclusive,
		Result: ReceiptActivated, CommandID: commandID, EventSeq: eventSeq,
	}
	routing.Scopes[rootScopeID] = ScopeRecord{
		ID: rootScopeID, RunID: needed.RunID, Generation: generation,
		ExpectedBranchEdgeIDs: []EdgeID{}, State: ScopeOpen, EventSeq: eventSeq,
	}
	routing.Reservations[reservationID] = ActivationReservation{
		ID: reservationID, RunID: needed.RunID, NodeID: tmpl.Start, ScopeID: rootScopeID,
		Generation: generation, JoinPolicy: JoinExclusive, Candidates: []CandidateRecord{},
		PossibleSlots: []PossibleSlotRecord{}, State: ReservationActivated,
		Activation: &activationRef, CommandID: commandID, EventSeq: eventSeq,
	}
	routing.Activations[activationID] = ActivationRecord{
		ID: activationID, RunID: needed.RunID, Ref: activationRef, ReservationID: reservationID,
		InputPathIDs: []PathID{}, InputSetDigest: inputDigest, OutputPathID: outputPathID,
		Receipt: receipt, CommandID: commandID, EventSeq: eventSeq,
	}
	routing.Paths[outputPathID] = PathRecord{
		ID: outputPathID, Kind: PathActivationOutput, State: PathLive, SourceActivation: activationRef,
		ScopeID: rootScopeID, CandidateLineage: []CandidateLineageFrame{}, CreatedSeq: eventSeq, UpdatedSeq: eventSeq,
	}
	aggregate := AggregateCheckpoint{
		RunID: needed.RunID, TemplateRef: templateHash, TemplateSourceHash: needed.TemplateSourceHash,
		Authority: authority, Routing: routing,
		Commands: map[string]CommandRecord{commandID: command}, SideEffects: map[string]SideEffectIdentity{},
		AdminRecords: map[string]PathV1AdminRecord{admin.ID: admin}, AdminResolutions: map[string]BlockResolution{},
	}
	event := InitializeEvent{
		Kind: InitializeEventKind, EventSeq: eventSeq, UpgradeNeeded: cloneUpgradeNeeded(needed), TemplateHash: templateHash,
		Command: command, AdminRecord: admin,
		Aggregate: aggregate, AggregateDigest: aggregateDigest,
	}
	digest, err := initializeEventDigest(event)
	if err != nil {
		return nil, err
	}
	checkpoint := &CheckpointV7{StateSchemaVersion: CheckpointStateSchemaVersion, Initialize: event, Digest: digest}
	if err := ValidateCheckpointV7(checkpoint); err != nil {
		return nil, err
	}
	return checkpoint, nil
}

func (c AggregateCheckpoint) View() AggregateView {
	authority := AggregateAuthority{
		RunID: c.Authority.RunID, TemplateRef: c.Authority.TemplateRef, TemplateSourceHash: c.Authority.TemplateSourceHash,
		Genesis: c.Authority.Genesis, Scopes: c.Authority.Scopes, Reservations: c.Authority.Reservations,
	}
	routing := c.Routing
	return AggregateView{
		RunID: c.RunID, TemplateRef: c.TemplateRef, TemplateSourceHash: c.TemplateSourceHash,
		Authority: &authority, Routing: &routing, Commands: c.Commands, SideEffects: c.SideEffects,
		AdminRecords: c.AdminRecords, AdminResolutions: c.AdminResolutions,
	}
}

func EncodeCheckpointV7(checkpoint *CheckpointV7) ([]byte, error) {
	if err := ValidateCheckpointV7(checkpoint); err != nil {
		return nil, err
	}
	// Keep command payload bytes exact. MarshalIndent rewrites RawMessage
	// whitespace, which would invalidate the payload hash carried by the same
	// atomic event.
	data, err := json.Marshal(checkpoint)
	if err != nil {
		return nil, fmt.Errorf("encode path-v1 checkpoint: %w", err)
	}
	data = append(data, '\n')
	if len(data) > MaxCheckpointBytes {
		return nil, &OverBudgetError{Limit: "checkpoint_bytes", Value: len(data), Maximum: MaxCheckpointBytes}
	}
	return data, nil
}

func DecodeCheckpointV7(data []byte) (*CheckpointV7, error) {
	if len(data) > MaxCheckpointBytes {
		return nil, &OverBudgetError{Limit: "checkpoint_bytes", Value: len(data), Maximum: MaxCheckpointBytes}
	}
	var header struct {
		StateSchemaVersion int `json:"stateSchemaVersion"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, fmt.Errorf("decode path-v1 checkpoint header: %w", err)
	}
	if header.StateSchemaVersion <= 0 {
		return nil, fmt.Errorf("%w: %d", ErrCheckpointSchemaInvalid, header.StateSchemaVersion)
	}
	if header.StateSchemaVersion > CheckpointStateSchemaVersion {
		return nil, fmt.Errorf("%w: got %d, supported %d", ErrCheckpointSchemaNewer, header.StateSchemaVersion, CheckpointStateSchemaVersion)
	}
	if header.StateSchemaVersion != CheckpointStateSchemaVersion {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrCheckpointSchemaInvalid, header.StateSchemaVersion, CheckpointStateSchemaVersion)
	}
	if _, err := parseJCS(data); err != nil {
		return nil, fmt.Errorf("decode path-v1 checkpoint strict JSON: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var checkpoint CheckpointV7
	if err := dec.Decode(&checkpoint); err != nil {
		return nil, fmt.Errorf("decode path-v1 checkpoint: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, fmt.Errorf("decode path-v1 checkpoint trailing JSON: %w", err)
		}
		return nil, fmt.Errorf("decode path-v1 checkpoint: multiple values")
	}
	if err := ValidateCheckpointV7(&checkpoint); err != nil {
		return nil, err
	}
	return &checkpoint, nil
}

func ValidateCheckpointV7(checkpoint *CheckpointV7) error {
	if checkpoint == nil || checkpoint.StateSchemaVersion != CheckpointStateSchemaVersion {
		return fmt.Errorf("%w: schema-7 checkpoint is required", ErrInitializationInvalid)
	}
	event := checkpoint.Initialize
	if event.Kind != InitializeEventKind || event.EventSeq <= 0 {
		return fmt.Errorf("%w: initialize event kind or sequence is invalid", ErrInitializationInvalid)
	}
	if err := ValidateUpgradeNeeded(event.UpgradeNeeded); err != nil || event.UpgradeNeeded.Reason != UpgradeMigrationRequired ||
		len(event.UpgradeNeeded.ActiveLegacyIDs) != 0 || len(event.UpgradeNeeded.CheckpointAdminRecords) != 0 {
		return fmt.Errorf("%w: initialize event has invalid upgrade authority", ErrInitializationInvalid)
	}
	if event.EventSeq != int64(event.UpgradeNeeded.Checkpoint.Generation+1) {
		return fmt.Errorf("%w: initialize event sequence is not checkpoint-coupled", ErrInitializationInvalid)
	}
	_, templateHash, ok := splitExactTemplateRef(event.UpgradeNeeded.TemplateRef)
	if !ok || templateHash != event.TemplateHash || !canonicalDigest(event.TemplateHash) {
		return fmt.Errorf("%w: initialize template hash mismatch", ErrInitializationInvalid)
	}
	view := event.Aggregate.View()
	if view.RunID != event.UpgradeNeeded.RunID || view.TemplateRef != event.TemplateHash ||
		view.TemplateSourceHash != event.UpgradeNeeded.TemplateSourceHash {
		return fmt.Errorf("%w: aggregate anchor mismatch", ErrInitializationInvalid)
	}
	if report := ValidateAggregate(view); !report.Valid() {
		return fmt.Errorf("%w: aggregate diagnostics=%v (%d suppressed)", ErrInitializationInvalid, report.Diagnostics, report.Suppressed)
	}
	command, ok := view.Commands[event.Command.ID]
	if !ok || !reflect.DeepEqual(command, event.Command) || command.State != CommandObserved ||
		command.Identity.Kind != CommandInitializeRouting || command.Identity.InputDigest != event.UpgradeNeeded.Checkpoint.Digest ||
		command.Identity.PlanDigest != event.AggregateDigest {
		return fmt.Errorf("%w: initialize command provenance mismatch", ErrInitializationInvalid)
	}
	if hash := sha256.Sum256(command.Payload); command.PayloadHash != hex.EncodeToString(hash[:]) {
		return fmt.Errorf("%w: initialize command payload hash mismatch", ErrInitializationInvalid)
	}
	var payload InitializeRoutingPayload
	if err := decodeExactJSON(command.Payload, &payload); err != nil || !equalUpgradeNeeded(payload.UpgradeNeeded, event.UpgradeNeeded) ||
		payload.TemplateHash != event.TemplateHash || payload.Genesis != view.Authority.Genesis || payload.AggregateDigest != event.AggregateDigest {
		return fmt.Errorf("%w: initialize command payload mismatch", ErrInitializationInvalid)
	}
	admin, ok := view.AdminRecords[event.AdminRecord.ID]
	if !ok || !reflect.DeepEqual(admin, event.AdminRecord) || admin.EventSeq != event.EventSeq ||
		admin.AdminType != InitializeEventKind || admin.EvidenceRef != command.ID {
		return fmt.Errorf("%w: initialize admin provenance mismatch", ErrInitializationInvalid)
	}
	if err := ValidateAdminRecord(admin, false, nil); err != nil {
		return fmt.Errorf("%w: %v", ErrInitializationInvalid, err)
	}
	wantAggregate, err := initializationAggregateDigest(view, event.UpgradeNeeded.Checkpoint.Digest)
	if err != nil || wantAggregate != event.AggregateDigest {
		return fmt.Errorf("%w: aggregate digest mismatch", ErrInitializationInvalid)
	}
	genesisDigest, err := initializeEventDigest(event)
	if err != nil {
		return fmt.Errorf("%w: checkpoint genesis digest cannot be computed", ErrInitializationInvalid)
	}
	if checkpoint.Execution == nil {
		if checkpoint.Digest != genesisDigest {
			return fmt.Errorf("%w: checkpoint digest mismatch", ErrInitializationInvalid)
		}
	} else if err := validateExecutionCheckpoint(checkpoint, genesisDigest); err != nil {
		return err
	}
	return nil
}

func ExactInitializationReplay(checkpoint *CheckpointV7, needed UpgradeNeeded) error {
	if err := ValidateCheckpointV7(checkpoint); err != nil {
		return fmt.Errorf("%w: %v", ErrInitializationInconsistent, err)
	}
	if err := ValidateUpgradeNeeded(needed); err != nil || !equalUpgradeNeeded(checkpoint.Initialize.UpgradeNeeded, needed) {
		return fmt.Errorf("%w: supplied upgrade proof differs from installed initialization", ErrInitializationInconsistent)
	}
	return nil
}

// RequireExactUpgradeNeeded rejects any semantic difference between detached
// caller authority and the proof freshly derived in the append-locked view.
func RequireExactUpgradeNeeded(provided, derived UpgradeNeeded) error {
	if err := ValidateUpgradeNeeded(provided); err != nil {
		return fmt.Errorf("%w: supplied upgrade proof is invalid: %v", ErrInitializationInvalid, err)
	}
	if err := ValidateUpgradeNeeded(derived); err != nil {
		return fmt.Errorf("%w: derived upgrade proof is invalid: %v", ErrInitializationInconsistent, err)
	}
	if !equalUpgradeNeeded(provided, derived) {
		return fmt.Errorf("%w: supplied upgrade proof differs from append-locked authority", ErrInitializationInconsistent)
	}
	return nil
}

func initializationAggregateDigest(view AggregateView, checkpointDigest string) (string, error) {
	paths := make([]PathFoldEntry, 0, len(view.Routing.Paths))
	for _, path := range view.Routing.Paths {
		seq, err := eventUint(path.UpdatedSeq)
		if err != nil {
			return "", err
		}
		paths = append(paths, PathFoldEntry{PathID: path.ID, State: path.State, UpdatedSeq: seq})
	}
	reservations := make([]ReservationFoldEntry, 0, len(view.Routing.Reservations))
	for _, reservation := range view.Routing.Reservations {
		seq, err := eventUint(reservation.EventSeq)
		if err != nil {
			return "", err
		}
		reservations = append(reservations, ReservationFoldEntry{ReservationID: reservation.ID, State: reservation.State, EventSeq: seq})
	}
	propagation := make([]PropagationFoldEntry, 0, len(view.Routing.Propagation))
	for _, intent := range view.Routing.Propagation {
		propagation = append(propagation, PropagationFoldEntry{IntentID: intent.ID, State: intent.State, Cursor: uint64(intent.Cursor)})
	}
	effects := make([]SideEffectFoldEntry, 0, len(view.SideEffects))
	for _, effect := range view.SideEffects {
		effects = append(effects, SideEffectFoldEntry{Kind: effect.Kind, ID: effect.ID, State: effect.State})
	}
	terminalIDs := make([]CauseID, 0)
	for _, path := range view.Routing.Paths {
		if path.State.TerminalNonSuccess() {
			terminalIDs = append(terminalIDs, path.TerminalCauseID)
		}
	}
	pathDigest, err := PathFoldIdentity(paths)
	if err != nil {
		return "", err
	}
	reservationDigest, err := ReservationFoldIdentity(reservations)
	if err != nil {
		return "", err
	}
	propagationDigest, err := PropagationFoldIdentity(propagation)
	if err != nil {
		return "", err
	}
	effectDigest, err := SideEffectFoldIdentity(effects)
	if err != nil {
		return "", err
	}
	terminalDigest, err := CauseSetIdentity(terminalIDs)
	if err != nil {
		return "", err
	}
	return AggregateIdentity(view.RunID, view.TemplateRef, checkpointDigest, pathDigest, reservationDigest, propagationDigest, effectDigest, terminalDigest)
}

func initializeEventDigest(event InitializeEvent) (string, error) {
	data, err := canonicalJSON(event)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func canonicalJSON(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	parsed, err := parseJCS(data)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := writeJCS(&out, parsed); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func decodeExactJSON(data []byte, target any) error {
	if _, err := parseJCS(data); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return fmt.Errorf("multiple JSON values")
	}
	return nil
}

func splitExactTemplateRef(ref string) (string, string, bool) {
	id, digest, ok := strings.Cut(ref, "@sha256:")
	return id, digest, ok && id != "" && canonicalDigest(digest)
}

func cloneUpgradeNeeded(in UpgradeNeeded) UpgradeNeeded {
	out := in
	if in.ActiveLegacyIDs != nil {
		out.ActiveLegacyIDs = make([]LegacyActiveID, len(in.ActiveLegacyIDs))
		copy(out.ActiveLegacyIDs, in.ActiveLegacyIDs)
	}
	if in.CheckpointAdminRecords != nil {
		out.CheckpointAdminRecords = make([]CheckpointLegacyAdminRecord, len(in.CheckpointAdminRecords))
		copy(out.CheckpointAdminRecords, in.CheckpointAdminRecords)
	}
	for i := range out.CheckpointAdminRecords {
		out.CheckpointAdminRecords[i].Record = in.CheckpointAdminRecords[i].Record
		if in.CheckpointAdminRecords[i].Resolution != nil {
			resolution := *in.CheckpointAdminRecords[i].Resolution
			out.CheckpointAdminRecords[i].Resolution = &resolution
		}
	}
	return out
}

func equalUpgradeNeeded(a, b UpgradeNeeded) bool {
	return a.Reason == b.Reason && a.RunID == b.RunID && a.LegacyStateSchema == b.LegacyStateSchema &&
		a.Checkpoint == b.Checkpoint && a.TemplateRef == b.TemplateRef && a.TemplateSourceHash == b.TemplateSourceHash &&
		slices.Equal(a.ActiveLegacyIDs, b.ActiveLegacyIDs) && slices.EqualFunc(a.CheckpointAdminRecords, b.CheckpointAdminRecords, equalCheckpointAdminRecord)
}

func equalCheckpointAdminRecord(a, b CheckpointLegacyAdminRecord) bool {
	if a.ID != b.ID || a.LegacyID != b.LegacyID || a.Record != b.Record {
		return false
	}
	if a.Resolution == nil || b.Resolution == nil {
		return a.Resolution == nil && b.Resolution == nil
	}
	return *a.Resolution == *b.Resolution
}
