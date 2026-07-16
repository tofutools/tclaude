package view

import (
	"cmp"
	"errors"
	"regexp"
	"slices"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
)

// Outcome labels are exact template authority, not lowercase node IDs.
// Preserve ASCII case while retaining the viewer's narrow safe charset.
var safeExactOutcomePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

const (
	ViewerV2Protocol         = "viewer_v2"
	LegacyV6PathProtocol     = "legacy_v6"
	PathV1StateSchemaVersion = 7
)

// RoutingUnavailableReason is a closed, stable explanation for why a
// checkpoint-derived path overlay was not projected.
type RoutingUnavailableReason string

const (
	RoutingUnavailableLegacySchema        RoutingUnavailableReason = "legacy_schema"
	RoutingUnavailableAbsent              RoutingUnavailableReason = "routing_absent"
	RoutingUnavailableUnsupportedSchema   RoutingUnavailableReason = "unsupported_schema"
	RoutingUnavailableUnsupportedProtocol RoutingUnavailableReason = "unsupported_protocol"
	RoutingUnavailableOverBudget          RoutingUnavailableReason = "over_budget"
	RoutingUnavailableInconsistent        RoutingUnavailableReason = "inconsistent"
)

// ViewerV2 is additive to the schema-v1 history report. RoutingAvailable is
// the sole authority for whether Routing contains a checkpoint-derived path
// overlay; callers must not infer one from legacy evidence or traversedEdges.
type ViewerV2 struct {
	Protocol                 string                   `json:"protocol"`
	StateSchemaVersion       int                      `json:"stateSchemaVersion"`
	PathProtocol             string                   `json:"pathProtocol,omitempty"`
	RoutingAvailable         bool                     `json:"routingAvailable"`
	RoutingUnavailableReason RoutingUnavailableReason `json:"routingUnavailableReason,omitempty"`
	ExactTopology            *ExactTopologyV2         `json:"exactTopology,omitempty"`
	Routing                  *RoutingOverlayV2        `json:"routing,omitempty"`
}

type ExactTopologyV2 struct {
	TemplateRef string           `json:"templateRef"`
	Start       string           `json:"start"`
	Nodes       []TopologyNodeV2 `json:"nodes"`
	Edges       []TopologyEdgeV2 `json:"edges"`
}

// ExactTopologyV2EncodedBytes gives the encoded topology ceiling its own unit
// so it cannot be confused with the viewer's node, edge, or routing-record
// cardinality budgets.
type ExactTopologyV2EncodedBytes uint64

// MaxExactTopologyV2EncodedBytes keeps one exact topology no larger than the
// path-v1 checkpoint whose routing it can describe.
const MaxExactTopologyV2EncodedBytes ExactTopologyV2EncodedBytes = pathv1.MaxCheckpointBytes

type TopologyNodeV2 struct {
	ID   string         `json:"id"`
	Type model.NodeType `json:"type,omitempty"`
}

type TopologyEdgeV2 struct {
	ID      string `json:"id"`
	From    string `json:"from,omitempty"`
	Outcome string `json:"outcome"`
	To      string `json:"to"`
}

type RoutingOverlayV2 struct {
	Protocol  string                    `json:"protocol"`
	Encoding  uint32                    `json:"encoding"`
	Edges     []RoutingEdgeOverlayV2    `json:"edges"`
	Scopes    []RoutingScopeOverlayV2   `json:"scopes,omitempty"`
	Joins     []RoutingJoinOverlayV2    `json:"joins,omitempty"`
	Closures  []RoutingClosureOverlayV2 `json:"closures,omitempty"`
	Aggregate RoutingAggregateOverlayV2 `json:"aggregate"`
}

// RoutingEdgeOverlayV2 deliberately collapses path records to an exact
// template edge, state, and count. It exposes no aggregate record, command,
// payload, completion basis, or evidence content.
type RoutingEdgeOverlayV2 struct {
	EdgeID string           `json:"edgeId"`
	State  pathv1.PathState `json:"state"`
	Count  int              `json:"count"`
}

type RoutingScopeOverlayV2 struct {
	ID                string                  `json:"id"`
	ParentScopeID     string                  `json:"parentScopeId,omitempty"`
	JoinReservationID string                  `json:"joinReservationId,omitempty"`
	State             pathv1.ScopeState       `json:"state"`
	CloseReason       pathv1.ScopeCloseReason `json:"closeReason,omitempty"`
}

type RoutingJoinOverlayV2 struct {
	ReservationID string                  `json:"reservationId"`
	NodeID        string                  `json:"nodeId"`
	ScopeID       string                  `json:"scopeId"`
	Policy        pathv1.JoinPolicy       `json:"policy"`
	State         pathv1.ReservationState `json:"state"`
	Arrived       int                     `json:"arrived"`
	Open          int                     `json:"open"`
	Impossible    int                     `json:"impossible"`
	Failed        int                     `json:"failed"`
	Skipped       int                     `json:"skipped"`
	Canceled      int                     `json:"canceled"`
}

type RoutingClosureOverlayV2 struct {
	ReservationID string              `json:"reservationId"`
	CandidateID   string              `json:"candidateId"`
	TerminalKind  pathv1.TerminalKind `json:"terminalKind"`
}

type RoutingAggregateOverlayV2 struct {
	Paths        int    `json:"paths"`
	Scopes       int    `json:"scopes"`
	Reservations int    `json:"reservations"`
	Activations  int    `json:"activations"`
	Closures     int    `json:"closures"`
	Propagation  int    `json:"propagation"`
	Settled      bool   `json:"settled"`
	Result       string `json:"result,omitempty"`
}

// ViewerV2Input is the complete input to the pure viewer-v2 projector.
// Evidence logs are intentionally absent: routing overlays can only come from
// the validated checkpoint aggregate.
type ViewerV2Input struct {
	RunID              string
	StateSchemaVersion int
	ExactTemplateRef   string
	ExactTemplate      *model.Template
	TemplateSourceHash string
	Aggregate          *pathv1.AggregateView
}

type exactTopologyProjection struct {
	topology     *ExactTopologyV2
	semanticHash string
	edges        map[string]TopologyEdgeV2
}

// ProjectViewerV2 derives an exact-template topology and, only for a valid
// schema-7 path-v1 checkpoint, a routing overlay. Every failure is closed and
// represented by one stable reason.
func ProjectViewerV2(input ViewerV2Input) ViewerV2 {
	result := ViewerV2{Protocol: ViewerV2Protocol, StateSchemaVersion: input.StateSchemaVersion}
	if input.StateSchemaVersion > 0 && input.StateSchemaVersion <= 6 {
		result.PathProtocol = LegacyV6PathProtocol
		result.RoutingUnavailableReason = RoutingUnavailableLegacySchema
		if topology, reason := projectExactTopology(input.ExactTemplateRef, input.ExactTemplate); reason == "" {
			result.ExactTopology = topology.topology
		}
		return result
	}
	if input.StateSchemaVersion != PathV1StateSchemaVersion {
		result.RoutingUnavailableReason = RoutingUnavailableUnsupportedSchema
		return result
	}
	result.PathProtocol = pathv1.Protocol

	topology, reason := projectExactTopology(input.ExactTemplateRef, input.ExactTemplate)
	if reason != "" {
		result.RoutingUnavailableReason = reason
		return result
	}
	result.ExactTopology = topology.topology
	aggregate := input.Aggregate
	if aggregate == nil || aggregate.Routing == nil {
		result.RoutingUnavailableReason = RoutingUnavailableAbsent
		return result
	}
	if aggregate.Routing.Protocol != pathv1.Protocol || aggregate.Routing.Encoding != pathv1.Encoding {
		result.RoutingUnavailableReason = RoutingUnavailableUnsupportedProtocol
		return result
	}
	usage, err := pathv1.MeasureAggregate(*aggregate)
	if err != nil {
		result.RoutingUnavailableReason = RoutingUnavailableInconsistent
		return result
	}
	if err := usage.Validate(); err != nil {
		var overBudget *pathv1.OverBudgetError
		if errors.As(err, &overBudget) {
			result.RoutingUnavailableReason = RoutingUnavailableOverBudget
		} else {
			result.RoutingUnavailableReason = RoutingUnavailableInconsistent
		}
		return result
	}
	if _, err := pathv1.Encode(aggregate.Routing); err != nil {
		var overBudget *pathv1.OverBudgetError
		if errors.As(err, &overBudget) {
			result.RoutingUnavailableReason = RoutingUnavailableOverBudget
		} else {
			result.RoutingUnavailableReason = RoutingUnavailableInconsistent
		}
		return result
	}
	if aggregate.RunID != input.RunID || aggregate.TemplateRef != topology.semanticHash || input.TemplateSourceHash == "" || aggregate.TemplateSourceHash != input.TemplateSourceHash {
		result.RoutingUnavailableReason = RoutingUnavailableInconsistent
		return result
	}
	if report := pathv1.ValidateAggregate(*aggregate); !report.Valid() {
		result.RoutingUnavailableReason = RoutingUnavailableInconsistent
		return result
	}

	overlay, ok := projectRoutingOverlay(*aggregate, topology.semanticHash, topology.edges)
	if !ok {
		result.RoutingUnavailableReason = RoutingUnavailableInconsistent
		return result
	}
	result.RoutingAvailable = true
	result.Routing = overlay
	return result
}

func projectExactTopology(ref string, tmpl *model.Template) (exactTopologyProjection, RoutingUnavailableReason) {
	if tmpl == nil || ref == "" {
		return exactTopologyProjection{}, RoutingUnavailableInconsistent
	}
	edgeCount := 1
	for _, node := range tmpl.Nodes {
		if len(node.Next) > pathv1.MaxRoutingList || edgeCount > pathv1.MaxRoutingRecords-len(node.Next) {
			return exactTopologyProjection{}, RoutingUnavailableOverBudget
		}
		edgeCount += len(node.Next)
	}
	if len(tmpl.Nodes) > pathv1.MaxRoutingRecords || edgeCount > pathv1.MaxRoutingRecords-len(tmpl.Nodes) {
		return exactTopologyProjection{}, RoutingUnavailableOverBudget
	}
	hash, err := model.SemanticHash(tmpl)
	if err != nil || model.TemplateRef(tmpl.ID, hash) != ref {
		return exactTopologyProjection{}, RoutingUnavailableInconsistent
	}
	edges := model.NormalizeEdges(tmpl)
	if diagnostics := model.Validate(tmpl, edges); diagnostics.HasErrors() {
		return exactTopologyProjection{}, RoutingUnavailableInconsistent
	}

	nodeIDs := make([]string, 0, len(tmpl.Nodes))
	for id := range tmpl.Nodes {
		if !safeIDPattern.MatchString(id) {
			return exactTopologyProjection{}, RoutingUnavailableInconsistent
		}
		nodeIDs = append(nodeIDs, id)
	}
	slices.Sort(nodeIDs)
	topology := &ExactTopologyV2{TemplateRef: ref, Start: tmpl.Start, Nodes: make([]TopologyNodeV2, 0, len(nodeIDs)), Edges: make([]TopologyEdgeV2, 0, len(edges))}
	for _, id := range nodeIDs {
		nodeType := tmpl.Nodes[id].Type
		if !validNodeType(nodeType) {
			return exactTopologyProjection{}, RoutingUnavailableInconsistent
		}
		topology.Nodes = append(topology.Nodes, TopologyNodeV2{ID: id, Type: nodeType})
	}
	edgesByID := make(map[string]TopologyEdgeV2, len(edges))
	for _, edge := range edges {
		if (edge.From != "" && !safeIDPattern.MatchString(edge.From)) || !safeExactOutcomePattern.MatchString(edge.Outcome) || !safeIDPattern.MatchString(edge.To) {
			return exactTopologyProjection{}, RoutingUnavailableInconsistent
		}
		id, err := pathv1.EdgeIdentity(hash, edge.From, edge.Outcome, edge.To)
		if err != nil {
			return exactTopologyProjection{}, RoutingUnavailableInconsistent
		}
		projected := TopologyEdgeV2{ID: id, From: edge.From, Outcome: edge.Outcome, To: edge.To}
		if _, exists := edgesByID[id]; exists {
			return exactTopologyProjection{}, RoutingUnavailableInconsistent
		}
		edgesByID[id] = projected
		topology.Edges = append(topology.Edges, projected)
	}
	slices.SortFunc(topology.Edges, func(a, b TopologyEdgeV2) int {
		if n := cmp.Compare(a.From, b.From); n != 0 {
			return n
		}
		if n := cmp.Compare(a.Outcome, b.Outcome); n != 0 {
			return n
		}
		if n := cmp.Compare(a.To, b.To); n != 0 {
			return n
		}
		return cmp.Compare(a.ID, b.ID)
	})
	if !exactTopologyV2FitsEncodedByteBudget(topology) {
		return exactTopologyProjection{}, RoutingUnavailableOverBudget
	}
	return exactTopologyProjection{topology: topology, semanticHash: hash, edges: edgesByID}, ""
}

type exactTopologyV2EncodedByteBudget struct {
	used ExactTopologyV2EncodedBytes
}

func (b *exactTopologyV2EncodedByteBudget) add(parts ...ExactTopologyV2EncodedBytes) bool {
	for _, part := range parts {
		if part > MaxExactTopologyV2EncodedBytes-b.used {
			return false
		}
		b.used += part
	}
	return true
}

// exactTopologyV2FitsEncodedByteBudget counts the exact encoding/json object
// shape without materializing another copy of the topology. All strings have
// already passed the topology's safe ASCII patterns, so their JSON encoding is
// exactly their byte length plus the two quote bytes.
func exactTopologyV2FitsEncodedByteBudget(topology *ExactTopologyV2) bool {
	if topology == nil {
		return false
	}
	b := exactTopologyV2EncodedByteBudget{}
	if !b.add(
		exactTopologyV2LiteralBytes(`{"templateRef":`), exactTopologyV2StringBytes(topology.TemplateRef),
		exactTopologyV2LiteralBytes(`,"start":`), exactTopologyV2StringBytes(topology.Start),
		exactTopologyV2LiteralBytes(`,"nodes":[`),
	) {
		return false
	}
	for i, node := range topology.Nodes {
		if i > 0 && !b.add(1) {
			return false
		}
		if !b.add(
			exactTopologyV2LiteralBytes(`{"id":`), exactTopologyV2StringBytes(node.ID),
			exactTopologyV2LiteralBytes(`,"type":`), exactTopologyV2StringBytes(string(node.Type)),
			1,
		) {
			return false
		}
	}
	if !b.add(exactTopologyV2LiteralBytes(`],"edges":[`)) {
		return false
	}
	for i, edge := range topology.Edges {
		if i > 0 && !b.add(1) {
			return false
		}
		if !b.add(exactTopologyV2LiteralBytes(`{"id":`), exactTopologyV2StringBytes(edge.ID)) {
			return false
		}
		if edge.From != "" && !b.add(exactTopologyV2LiteralBytes(`,"from":`), exactTopologyV2StringBytes(edge.From)) {
			return false
		}
		if !b.add(
			exactTopologyV2LiteralBytes(`,"outcome":`), exactTopologyV2StringBytes(edge.Outcome),
			exactTopologyV2LiteralBytes(`,"to":`), exactTopologyV2StringBytes(edge.To),
			1,
		) {
			return false
		}
	}
	return b.add(2)
}

func exactTopologyV2LiteralBytes(value string) ExactTopologyV2EncodedBytes {
	return ExactTopologyV2EncodedBytes(len(value))
}

func exactTopologyV2StringBytes(value string) ExactTopologyV2EncodedBytes {
	return ExactTopologyV2EncodedBytes(len(value)) + 2
}

type routingArrivalKey struct {
	reservationID pathv1.ReservationID
	candidateID   pathv1.CandidateID
}

func projectRoutingOverlay(aggregate pathv1.AggregateView, semanticHash string, topologyEdges map[string]TopologyEdgeV2) (*RoutingOverlayV2, bool) {
	routing := aggregate.Routing
	type overlayKey struct {
		edgeID string
		state  pathv1.PathState
	}
	counts := make(map[overlayKey]int)
	arrivals := make(map[routingArrivalKey]struct{})
	for _, path := range routing.Paths {
		if path.Edge == nil {
			continue
		}
		edge, ok := topologyEdges[path.Edge.ID]
		if !ok || path.Edge.TemplateRef != semanticHash || path.Edge.FromNodeID != edge.From || path.Edge.Outcome != edge.Outcome || path.Edge.ToNodeID != edge.To || !path.State.Valid() {
			return nil, false
		}
		counts[overlayKey{edgeID: edge.ID, state: path.State}]++
		if path.Kind == pathv1.PathEdge && (path.State == pathv1.PathArrived || path.State == pathv1.PathConsumed) {
			arrivals[routingArrivalKey{reservationID: path.TargetReservationID, candidateID: path.CandidateID}] = struct{}{}
		}
	}
	overlay := &RoutingOverlayV2{Protocol: pathv1.Protocol, Encoding: pathv1.Encoding, Edges: make([]RoutingEdgeOverlayV2, 0, len(counts))}
	for key, count := range counts {
		overlay.Edges = append(overlay.Edges, RoutingEdgeOverlayV2{EdgeID: key.edgeID, State: key.state, Count: count})
	}
	slices.SortFunc(overlay.Edges, func(a, b RoutingEdgeOverlayV2) int {
		if n := cmp.Compare(a.EdgeID, b.EdgeID); n != 0 {
			return n
		}
		return cmp.Compare(a.State, b.State)
	})
	for _, scope := range routing.Scopes {
		overlay.Scopes = append(overlay.Scopes, RoutingScopeOverlayV2{ID: scope.ID, ParentScopeID: scope.ParentScopeID, JoinReservationID: scope.JoinReservationID, State: scope.State, CloseReason: scope.CloseReason})
	}
	slices.SortFunc(overlay.Scopes, func(a, b RoutingScopeOverlayV2) int { return cmp.Compare(a.ID, b.ID) })
	joins, ok := projectRoutingJoins(routing, arrivals)
	if !ok {
		return nil, false
	}
	overlay.Joins = joins
	for _, closure := range routing.CandidateClosures {
		overlay.Closures = append(overlay.Closures, RoutingClosureOverlayV2{ReservationID: closure.Key.ReservationID, CandidateID: closure.Key.CandidateID, TerminalKind: closure.TerminalKind})
	}
	slices.SortFunc(overlay.Closures, func(a, b RoutingClosureOverlayV2) int {
		if n := cmp.Compare(a.ReservationID, b.ReservationID); n != 0 {
			return n
		}
		return cmp.Compare(a.CandidateID, b.CandidateID)
	})
	overlay.Aggregate = RoutingAggregateOverlayV2{Paths: len(routing.Paths), Scopes: len(routing.Scopes), Reservations: len(routing.Reservations), Activations: len(routing.Activations), Closures: len(routing.CandidateClosures), Propagation: len(routing.Propagation)}
	if completion, err := pathv1.AssessAggregateCompletion(aggregate); err == nil {
		overlay.Aggregate.Settled = true
		overlay.Aggregate.Result = completion.Result
	} else if !errors.Is(err, pathv1.ErrAggregateUnsettled) {
		return nil, false
	}
	return overlay, true
}

func projectRoutingJoins(routing *pathv1.RoutingState, arrivals map[routingArrivalKey]struct{}) ([]RoutingJoinOverlayV2, bool) {
	joins := make([]RoutingJoinOverlayV2, 0)
	for _, reservation := range routing.Reservations {
		if reservation.JoinPolicy != pathv1.JoinAll && reservation.JoinPolicy != pathv1.JoinAny {
			continue
		}
		join := RoutingJoinOverlayV2{ReservationID: reservation.ID, NodeID: reservation.NodeID, ScopeID: reservation.ScopeID, Policy: reservation.JoinPolicy, State: reservation.State}
		for _, candidate := range reservation.Candidates {
			if _, arrived := arrivals[routingArrivalKey{reservationID: reservation.ID, candidateID: candidate.ID}]; arrived {
				join.Arrived++
				continue
			}
			key, err := pathv1.CandidateClosureKeyIdentity(reservation.ID, candidate.ID)
			if err != nil {
				return nil, false
			}
			closure, ok := routing.CandidateClosures[key]
			if !ok {
				join.Open++
				continue
			}
			switch closure.TerminalKind {
			case pathv1.TerminalImpossible:
				join.Impossible++
			case pathv1.TerminalFailed:
				join.Failed++
			case pathv1.TerminalSkipped:
				join.Skipped++
			case pathv1.TerminalCanceled:
				join.Canceled++
			default:
				return nil, false
			}
		}
		joins = append(joins, join)
	}
	slices.SortFunc(joins, func(a, b RoutingJoinOverlayV2) int { return cmp.Compare(a.ReservationID, b.ReservationID) })
	return joins, true
}
