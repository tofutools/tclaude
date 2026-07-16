package view_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	processview "github.com/tofutools/tclaude/pkg/claude/process/view"
)

func TestProjectViewerV2DiscriminatesSchemaProtocolAndAbsence(t *testing.T) {
	t.Parallel()
	tmpl, ref, _, _ := viewerV2Fixture(t)

	legacy := processview.ProjectViewerV2(processview.ViewerV2Input{StateSchemaVersion: 6, ExactTemplateRef: ref, ExactTemplate: tmpl})
	assert.Equal(t, processview.LegacyV6PathProtocol, legacy.PathProtocol)
	assert.False(t, legacy.RoutingAvailable)
	assert.Equal(t, processview.RoutingUnavailableLegacySchema, legacy.RoutingUnavailableReason)
	require.NotNil(t, legacy.ExactTopology)
	assert.Nil(t, legacy.Routing)

	unknown := processview.ProjectViewerV2(processview.ViewerV2Input{StateSchemaVersion: 99, ExactTemplateRef: ref, ExactTemplate: tmpl})
	assert.Empty(t, unknown.PathProtocol)
	assert.Equal(t, processview.RoutingUnavailableUnsupportedSchema, unknown.RoutingUnavailableReason)
	assert.Nil(t, unknown.ExactTopology)

	absent := processview.ProjectViewerV2(processview.ViewerV2Input{StateSchemaVersion: processview.PathV1StateSchemaVersion, ExactTemplateRef: ref, ExactTemplate: tmpl})
	assert.Equal(t, pathv1.Protocol, absent.PathProtocol)
	assert.Equal(t, processview.RoutingUnavailableAbsent, absent.RoutingUnavailableReason)
	require.NotNil(t, absent.ExactTopology)
	assert.Nil(t, absent.Routing)
}

func TestProjectViewerV2ValidatesExactTopologyAndAggregateBeforeOverlay(t *testing.T) {
	t.Parallel()
	tmpl, ref, sourceHash, aggregate := viewerV2Fixture(t)
	input := processview.ViewerV2Input{
		RunID: aggregate.RunID, StateSchemaVersion: processview.PathV1StateSchemaVersion,
		ExactTemplateRef: ref, ExactTemplate: tmpl, TemplateSourceHash: sourceHash, Aggregate: &aggregate,
	}

	projected := processview.ProjectViewerV2(input)
	assert.True(t, projected.RoutingAvailable)
	assert.Empty(t, projected.RoutingUnavailableReason)
	require.NotNil(t, projected.Routing)
	assert.Equal(t, pathv1.Protocol, projected.Routing.Protocol)
	assert.Equal(t, pathv1.Encoding, projected.Routing.Encoding)
	require.Equal(t, []processview.RoutingEdgeOverlayV2{{
		EdgeID: aggregate.Routing.Paths[viewerV2OnlyEdgePathID(t, aggregate)].Edge.ID,
		State:  pathv1.PathArrived,
		Count:  1,
	}}, projected.Routing.Edges)
	assert.Equal(t, len(aggregate.Routing.Paths), projected.Routing.Aggregate.Paths)
	assert.Equal(t, len(aggregate.Routing.Scopes), projected.Routing.Aggregate.Scopes)
	require.NotNil(t, projected.ExactTopology)
	assert.Equal(t, ref, projected.ExactTopology.TemplateRef)
	assert.Equal(t, []string{"done", "work"}, []string{projected.ExactTopology.Nodes[0].ID, projected.ExactTopology.Nodes[1].ID})
	require.Len(t, projected.ExactTopology.Edges, 2)
	for _, edge := range projected.ExactTopology.Edges {
		hash, err := pathv1.EdgeIdentity(aggregate.TemplateRef, edge.From, edge.Outcome, edge.To)
		require.NoError(t, err)
		assert.Equal(t, hash, edge.ID)
	}

	wrongRef := input
	wrongRef.ExactTemplateRef = "other@sha256:" + strings.Repeat("c", 64)
	assertViewerV2Unavailable(t, processview.ProjectViewerV2(wrongRef), processview.RoutingUnavailableInconsistent)

	unknownProtocol := aggregate
	unknownProtocol.Routing = cloneRouting(aggregate.Routing)
	unknownProtocol.Routing.Protocol = "path_v9"
	input.Aggregate = &unknownProtocol
	assertViewerV2Unavailable(t, processview.ProjectViewerV2(input), processview.RoutingUnavailableUnsupportedProtocol)

	unknownEncoding := aggregate
	unknownEncoding.Routing = cloneRouting(aggregate.Routing)
	unknownEncoding.Routing.Encoding++
	input.Aggregate = &unknownEncoding
	assertViewerV2Unavailable(t, processview.ProjectViewerV2(input), processview.RoutingUnavailableUnsupportedProtocol)

	overBudget := aggregate
	overBudget.CheckpointBytes = pathv1.MaxCheckpointBytes + 1
	overBudget.RunID = "also-inconsistent"
	input.Aggregate = &overBudget
	assertViewerV2Unavailable(t, processview.ProjectViewerV2(input), processview.RoutingUnavailableOverBudget)

	inconsistent := aggregate
	inconsistent.RunID = "different-run"
	input.Aggregate = &inconsistent
	assertViewerV2Unavailable(t, processview.ProjectViewerV2(input), processview.RoutingUnavailableInconsistent)

	tamperedEdge := aggregate
	tamperedEdge.Routing = cloneRouting(aggregate.Routing)
	edgePathID := viewerV2OnlyEdgePathID(t, tamperedEdge)
	edgePath := tamperedEdge.Routing.Paths[edgePathID]
	edgePath.Edge.TemplateRef = strings.Repeat("d", 64)
	tamperedEdge.Routing.Paths[edgePathID] = edgePath
	input.Aggregate = &tamperedEdge
	assertViewerV2Unavailable(t, processview.ProjectViewerV2(input), processview.RoutingUnavailableInconsistent)
}

func TestViewerV2JSONIsNarrowAndHasNoEvidenceFallback(t *testing.T) {
	t.Parallel()
	tmpl, ref, sourceHash, aggregate := viewerV2Fixture(t)
	projected := processview.ProjectViewerV2(processview.ViewerV2Input{
		RunID: aggregate.RunID, StateSchemaVersion: processview.PathV1StateSchemaVersion,
		ExactTemplateRef: ref, ExactTemplate: tmpl, TemplateSourceHash: sourceHash, Aggregate: &aggregate,
	})
	data, err := json.Marshal(projected)
	require.NoError(t, err)
	for _, forbidden := range []string{"commands", "payload", "completionBasis", "evidence", "DO_NOT_LEAK"} {
		assert.NotContains(t, string(data), forbidden)
	}

	// The projector's declared input has no evidence/log field. Removing the
	// checkpoint routing therefore closes the overlay instead of reconstructing
	// one from any historical surface.
	aggregate.Routing = nil
	projected = processview.ProjectViewerV2(processview.ViewerV2Input{
		RunID: aggregate.RunID, StateSchemaVersion: processview.PathV1StateSchemaVersion,
		ExactTemplateRef: ref, ExactTemplate: tmpl, TemplateSourceHash: sourceHash, Aggregate: &aggregate,
	})
	assertViewerV2Unavailable(t, projected, processview.RoutingUnavailableAbsent)
}

func TestProjectViewerV2ReturnsBoundedPageMetadataWithUnpagedAggregates(t *testing.T) {
	t.Parallel()
	tmpl, ref, sourceHash, aggregate := viewerV2Fixture(t)
	projected := processview.ProjectViewerV2(processview.ViewerV2Input{
		RunID: aggregate.RunID, StateSchemaVersion: processview.PathV1StateSchemaVersion,
		ExactTemplateRef: ref, ExactTemplate: tmpl, TemplateSourceHash: sourceHash, Aggregate: &aggregate,
		Page: processview.RoutingPageRequestV2{Offset: 1, Limit: 1},
	})
	require.True(t, projected.RoutingAvailable)
	require.NotNil(t, projected.Routing)
	assert.Equal(t, processview.RoutingPageV2{Offset: 1, Limit: 1, Total: 2}, projected.Routing.Details.Generations.Page)
	require.Len(t, projected.Routing.Details.Generations.Items, 1)
	assert.Equal(t, "work", projected.Routing.Details.Generations.Items[0].NodeID)
	assert.Equal(t, 2, projected.Routing.Aggregate.Reservations, "aggregate totals must not shrink with the detail page")
	assert.Equal(t, []processview.RoutingStateCountV2{{State: string(pathv1.PathArrived), Count: 1}, {State: string(pathv1.PathRouted), Count: 1}}, projected.Routing.StateCounts.Paths)
}

func TestProjectViewerV2ExactTopologyEncodedByteBudget(t *testing.T) {
	atBound, atBoundRef, adjustableSource, adjustableOutcome, atBoundSourceBytes := denseViewerV2TemplateAtEncodedByteBudget(t)
	projected := processview.ProjectViewerV2(processview.ViewerV2Input{
		StateSchemaVersion: processview.PathV1StateSchemaVersion,
		ExactTemplateRef:   atBoundRef,
		ExactTemplate:      atBound,
	})
	assert.Equal(t, processview.RoutingUnavailableAbsent, projected.RoutingUnavailableReason)
	require.NotNil(t, projected.ExactTopology)
	encoded, err := json.Marshal(projected.ExactTopology)
	require.NoError(t, err)
	assert.Len(t, encoded, int(processview.MaxExactTopologyV2EncodedBytes))
	assert.Greater(t, len(encoded), atBoundSourceBytes)

	adjustedNode := atBound.Nodes[adjustableSource]
	target := adjustedNode.Next[adjustableOutcome]
	delete(adjustedNode.Next, adjustableOutcome)
	adjustedNode.Next[adjustableOutcome+"x"] = target
	overBoundRef := viewerV2TemplateRef(t, atBound)
	overBoundSourceBytes := assertViewerV2ExactTemplateInputWithinByteBudget(t, atBound)
	projected = processview.ProjectViewerV2(processview.ViewerV2Input{
		StateSchemaVersion: processview.PathV1StateSchemaVersion,
		ExactTemplateRef:   overBoundRef,
		ExactTemplate:      atBound,
	})
	assert.Equal(t, int(processview.MaxExactTopologyV2EncodedBytes)+1, len(encoded)+1)
	assert.Equal(t, atBoundSourceBytes+1, overBoundSourceBytes)
	assertViewerV2Unavailable(t, projected, processview.RoutingUnavailableOverBudget)
	assert.Nil(t, projected.ExactTopology)

	legacy := processview.ProjectViewerV2(processview.ViewerV2Input{
		StateSchemaVersion: 6,
		ExactTemplateRef:   overBoundRef,
		ExactTemplate:      atBound,
	})
	assert.Equal(t, processview.RoutingUnavailableLegacySchema, legacy.RoutingUnavailableReason)
	assert.Nil(t, legacy.ExactTopology)

	wrongRef := processview.ProjectViewerV2(processview.ViewerV2Input{
		StateSchemaVersion: processview.PathV1StateSchemaVersion,
		ExactTemplateRef:   "other@sha256:" + strings.Repeat("c", 64),
		ExactTemplate:      atBound,
	})
	assertViewerV2Unavailable(t, wrongRef, processview.RoutingUnavailableInconsistent)
}

func assertViewerV2Unavailable(t *testing.T, got processview.ViewerV2, reason processview.RoutingUnavailableReason) {
	t.Helper()
	assert.False(t, got.RoutingAvailable)
	assert.Equal(t, reason, got.RoutingUnavailableReason)
	assert.Nil(t, got.Routing)
}

// denseViewerV2TemplateAtEncodedByteBudget builds a valid acyclic chain at the
// 2,048 normalized-node ceiling. Its 2,000 long source IDs are repeated across
// two projected edge objects; branch targets are sharded across 48 sinks.
// This keeps the exact template input well below its existing file ceiling
// while projection overhead supplies most of the encoded topology size. One
// safe outcome is then extended just enough to land exactly on the bound.
func denseViewerV2TemplateAtEncodedByteBudget(t *testing.T) (*model.Template, string, string, string, int) {
	t.Helper()
	const outgoing = 2
	const sinks = 48
	sourceIDs := make([]string, model.MaxNormalizedNodes-sinks)
	for i := range sourceIDs {
		sourceIDs[i] = fmt.Sprintf("source-%04d-%s", i, strings.Repeat("s", 52))
	}
	nodes := make(map[string]model.Node, len(sourceIDs)+sinks)
	for sink := 0; sink < sinks; sink++ {
		nodes[fmt.Sprintf("end-%02d", sink)] = model.Node{Type: model.NodeTypeEnd}
	}
	for i, sourceID := range sourceIDs {
		next := make(model.Next, outgoing)
		for branch := 0; branch < outgoing-1; branch++ {
			next[fmt.Sprintf("branch-%02d", branch)] = fmt.Sprintf("end-%02d", (i*(outgoing-1)+branch)%sinks)
		}
		if i+1 < len(sourceIDs) {
			next["advance"] = sourceIDs[i+1]
		} else {
			next["advance"] = "end-00"
		}
		nodes[sourceID] = model.Node{
			Type:      model.NodeTypeDecision,
			Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "Choose an outcome"},
			Next:      next,
		}
	}
	tmpl := &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         "viewer-v2-dense-budget",
		Start:      sourceIDs[0],
		Nodes:      nodes,
	}
	edges := model.NormalizeEdges(tmpl)
	require.Len(t, edges, len(sourceIDs)*outgoing+1)
	require.LessOrEqual(t, len(nodes)+len(edges), pathv1.MaxRoutingRecords)

	ref := viewerV2TemplateRef(t, tmpl)
	baseline := processview.ProjectViewerV2(processview.ViewerV2Input{
		StateSchemaVersion: processview.PathV1StateSchemaVersion,
		ExactTemplateRef:   ref,
		ExactTemplate:      tmpl,
	})
	require.Equal(t, processview.RoutingUnavailableAbsent, baseline.RoutingUnavailableReason)
	require.NotNil(t, baseline.ExactTopology)
	baselineJSON, err := json.Marshal(baseline.ExactTopology)
	require.NoError(t, err)

	adjustableOutcome := "branch-00"
	adjustableSource := sourceIDs[0]
	fillerBytes := int(processview.MaxExactTopologyV2EncodedBytes) - len(baselineJSON)
	require.Positive(t, fillerBytes)
	adjustedNode := nodes[adjustableSource]
	target := adjustedNode.Next[adjustableOutcome]
	delete(adjustedNode.Next, adjustableOutcome)
	adjustableOutcome += strings.Repeat("x", fillerBytes)
	adjustedNode.Next[adjustableOutcome] = target

	ref = viewerV2TemplateRef(t, tmpl)
	sourceBytes := assertViewerV2ExactTemplateInputWithinByteBudget(t, tmpl)
	return tmpl, ref, adjustableSource, adjustableOutcome, sourceBytes
}

func assertViewerV2ExactTemplateInputWithinByteBudget(t *testing.T, tmpl *model.Template) int {
	t.Helper()
	encoded, err := json.Marshal(tmpl)
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), pathv1.MaxCheckpointBytes)
	canonical, err := model.CanonicalSemanticJSON(tmpl)
	require.NoError(t, err)
	require.LessOrEqual(t, len(canonical), pathv1.MaxCheckpointBytes)
	return len(encoded)
}

func viewerV2TemplateRef(t *testing.T, tmpl *model.Template) string {
	t.Helper()
	hash, err := model.SemanticHash(tmpl)
	require.NoError(t, err)
	return model.TemplateRef(tmpl.ID, hash)
}

func viewerV2Fixture(t *testing.T) (*model.Template, string, string, pathv1.AggregateView) {
	t.Helper()
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "viewer-v2", Start: "work",
		Nodes: map[string]model.Node{
			"work": {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Profile: "dev", Prompt: "DO_NOT_LEAK"}, Next: model.Next{"pass": "done"}},
			"done": {Type: model.NodeTypeEnd},
		},
	}
	semanticHash, err := model.SemanticHash(tmpl)
	require.NoError(t, err)
	ref := model.TemplateRef(tmpl.ID, semanticHash)
	sourceHash := strings.Repeat("b", 64)
	runID := "run-viewer-v2"
	root, err := pathv1.ScopeIdentity(runID, "", "", "", "", 1)
	require.NoError(t, err)
	reservationID, err := pathv1.ReservationIdentity(runID, tmpl.Start, root, "", 1)
	require.NoError(t, err)
	inputDigest, err := pathv1.InputSetIdentity(nil)
	require.NoError(t, err)
	activationID, err := pathv1.ActivationIdentity(runID, reservationID, 1, inputDigest)
	require.NoError(t, err)
	outputID, err := pathv1.ActivationOutputIdentity(activationID, 1)
	require.NoError(t, err)
	identity := pathv1.CommandIdentity{RunID: runID, Kind: pathv1.CommandInitializeRouting, PayloadSchema: 1, InputDigest: "legacy", PlanDigest: "routing"}
	commandID, err := pathv1.CommandIdentityDigest(identity)
	require.NoError(t, err)
	payload := json.RawMessage(`{"genesis":true}`)
	sum := sha256.Sum256(payload)
	command := pathv1.CommandRecord{ID: commandID, IdempotencyKey: pathv1.CommandIdempotencyKey(identity.Kind, commandID), Identity: identity, Payload: payload, PayloadHash: hex.EncodeToString(sum[:]), State: pathv1.CommandObserved}
	activationRef := pathv1.ActivationRef{ID: activationID, Generation: 1}
	receiptID, err := pathv1.ActivationReceiptIdentity(activationID, reservationID, inputDigest, outputID, commandID, 1)
	require.NoError(t, err)
	receipt := pathv1.ActivationReceipt{ID: receiptID, ActivationID: activationID, ReservationID: reservationID, InputSetDigest: inputDigest, OutputPathID: outputID, ScopeID: root, JoinPolicy: pathv1.JoinExclusive, Result: pathv1.ReceiptActivated, CommandID: commandID, EventSeq: 1}
	routing := pathv1.NewRoutingState()
	routing.Scopes[root] = pathv1.ScopeRecord{ID: root, RunID: runID, Generation: 1, ExpectedBranchEdgeIDs: []string{}, State: pathv1.ScopeOpen, EventSeq: 1}
	routing.Reservations[reservationID] = pathv1.ActivationReservation{ID: reservationID, RunID: runID, NodeID: tmpl.Start, ScopeID: root, Generation: 1, JoinPolicy: pathv1.JoinExclusive, Candidates: []pathv1.CandidateRecord{}, PossibleSlots: []pathv1.PossibleSlotRecord{}, State: pathv1.ReservationActivated, Activation: &activationRef, CommandID: commandID, EventSeq: 1}
	routing.Activations[activationID] = pathv1.ActivationRecord{ID: activationID, RunID: runID, Ref: activationRef, ReservationID: reservationID, InputPathIDs: []string{}, InputSetDigest: inputDigest, OutputPathID: outputID, Receipt: receipt, CommandID: commandID, EventSeq: 1}
	routing.Paths[outputID] = pathv1.PathRecord{ID: outputID, Kind: pathv1.PathActivationOutput, State: pathv1.PathEnded, SourceActivation: activationRef, ScopeID: root, CandidateLineage: []pathv1.CandidateLineageFrame{}, CreatedSeq: 1, UpdatedSeq: 1}
	authority := &pathv1.AggregateAuthority{
		RunID: runID, TemplateRef: semanticHash, TemplateSourceHash: sourceHash,
		Genesis:      pathv1.GenesisAuthority{RootScopeID: root, StartNodeID: tmpl.Start, ReservationID: reservationID, ActivationID: activationID, OutputPathID: outputID, Generation: 1},
		Scopes:       map[string]pathv1.ScopeAuthority{root: {ID: root, Generation: 1, ExpectedBranchEdgeIDs: []string{}}},
		Reservations: map[string]pathv1.ReservationAuthority{reservationID: {ID: reservationID, NodeID: tmpl.Start, ScopeID: root, Generation: 1, JoinPolicy: pathv1.JoinExclusive, Candidates: []pathv1.CandidateRecord{}, PossibleSlots: []pathv1.PossibleSlotRecord{}}},
	}
	aggregate := pathv1.AggregateView{
		RunID: runID, TemplateRef: semanticHash, TemplateSourceHash: sourceHash, Authority: authority, Routing: &routing,
		Commands: map[string]pathv1.CommandRecord{commandID: command}, SideEffects: map[string]pathv1.SideEffectIdentity{}, AdminRecords: map[string]pathv1.PathV1AdminRecord{}, AdminResolutions: map[string]pathv1.BlockResolution{},
	}
	addViewerV2Arrival(t, &aggregate, "work", "pass", "done")
	return tmpl, ref, sourceHash, aggregate
}

func addViewerV2Arrival(t *testing.T, aggregate *pathv1.AggregateView, from, outcome, to string) {
	t.Helper()
	root := aggregate.Authority.Genesis.RootScopeID
	edgeID, err := pathv1.EdgeIdentity(aggregate.TemplateRef, from, outcome, to)
	require.NoError(t, err)
	reservationID, err := pathv1.ReservationIdentity(aggregate.RunID, to, root, "", 1)
	require.NoError(t, err)
	candidateID, err := pathv1.CandidateIdentity(reservationID, pathv1.CandidateInboundEdge, edgeID)
	require.NoError(t, err)
	slotID, err := pathv1.PossibleSlotIdentity(reservationID, candidateID, from, edgeID, root, "", 1)
	require.NoError(t, err)
	slot := pathv1.PossibleSlotRecord{ID: slotID, ReservationID: reservationID, CandidateID: candidateID, SourceNodeID: from, SourceEdgeID: edgeID, SourceScopeID: root, Generation: 1}
	candidate := pathv1.CandidateRecord{ID: candidateID, Kind: pathv1.CandidateInboundEdge, MemberID: edgeID, PossibleSlotIDs: []string{slotID}}
	aggregate.Authority.Reservations[reservationID] = pathv1.ReservationAuthority{ID: reservationID, NodeID: to, ScopeID: root, Generation: 1, JoinPolicy: pathv1.JoinExclusive, Candidates: []pathv1.CandidateRecord{candidate}, PossibleSlots: []pathv1.PossibleSlotRecord{slot}}
	aggregate.Routing.Reservations[reservationID] = pathv1.ActivationReservation{ID: reservationID, RunID: aggregate.RunID, NodeID: to, ScopeID: root, Generation: 1, JoinPolicy: pathv1.JoinExclusive, Candidates: []pathv1.CandidateRecord{candidate}, PossibleSlots: []pathv1.PossibleSlotRecord{slot}, State: pathv1.ReservationOpen}

	parentID := aggregate.Authority.Genesis.OutputPathID
	parent := aggregate.Routing.Paths[parentID]
	edge := pathv1.EdgeKey{TemplateRef: aggregate.TemplateRef, ID: edgeID, FromNodeID: from, Outcome: outcome, ToNodeID: to}
	pathID, err := pathv1.EdgePathIdentity(parent.SourceActivation.ID, parent.ID, edge.ID, reservationID, candidateID)
	require.NoError(t, err)
	arrivalID, err := pathv1.ArrivalIdentity(pathID, reservationID, candidateID)
	require.NoError(t, err)
	lineageID, err := pathv1.CandidateLineageIdentity("", reservationID, candidateID)
	require.NoError(t, err)
	identity := pathv1.CommandIdentity{RunID: aggregate.RunID, Kind: pathv1.CommandRoutePaths, PayloadSchema: 1, SourceActivationID: parent.SourceActivation.ID, SourceGeneration: parent.SourceActivation.Generation, SourcePathID: parent.ID, InputDigest: "settled", CauseDigest: "cause", PlanDigest: "exclusive", ResultCode: outcome}
	commandID, err := pathv1.CommandIdentityDigest(identity)
	require.NoError(t, err)
	payload := json.RawMessage(`{"plan":true}`)
	sum := sha256.Sum256(payload)
	aggregate.Commands[commandID] = pathv1.CommandRecord{ID: commandID, IdempotencyKey: pathv1.CommandIdempotencyKey(identity.Kind, commandID), Identity: identity, Payload: payload, PayloadHash: hex.EncodeToString(sum[:]), State: pathv1.CommandObserved}
	parent.State = pathv1.PathRouted
	parent.ProducedPathIDs = []string{pathID}
	parent.UpdatedSeq = 2
	dispositionID, err := pathv1.DispositionReceiptIdentity(parent.ID, pathv1.PathLive, pathv1.PathRouted, "route", commandID, "", 2)
	require.NoError(t, err)
	parent.Disposition = &pathv1.DispositionReceipt{ID: dispositionID, PathID: parent.ID, FromState: pathv1.PathLive, ToState: pathv1.PathRouted, ReasonCode: "route", CommandID: commandID, EventSeq: 2}
	aggregate.Routing.Paths[parent.ID] = parent
	aggregate.Routing.Paths[pathID] = pathv1.PathRecord{
		ID: pathID, Kind: pathv1.PathEdge, State: pathv1.PathArrived, ParentPathID: parent.ID,
		SourceActivation: parent.SourceActivation, Edge: &edge, TargetReservationID: reservationID, CandidateID: candidateID, ScopeID: root,
		CandidateLineage: []pathv1.CandidateLineageFrame{{ID: lineageID, ReservationID: reservationID, CandidateID: candidateID}}, CandidateLineageID: lineageID, LineageDepth: 1,
		ArrivalID: arrivalID, ArrivedSeq: 2, CreatedSeq: 2, UpdatedSeq: 2,
	}
}

func viewerV2OnlyEdgePathID(t *testing.T, aggregate pathv1.AggregateView) string {
	t.Helper()
	var id string
	for pathID, path := range aggregate.Routing.Paths {
		if path.Edge == nil {
			continue
		}
		require.Empty(t, id)
		id = pathID
	}
	require.NotEmpty(t, id)
	return id
}

func cloneRouting(in *pathv1.RoutingState) *pathv1.RoutingState {
	if in == nil {
		return nil
	}
	clone := pathv1.Clone(*in)
	return &clone
}
