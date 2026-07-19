package view

import (
	"cmp"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
)

// Outcome labels are exact template authority, not lowercase node IDs.
// Preserve ASCII case while retaining the viewer's narrow safe charset.
var safeExactOutcomePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Routing reasons are engine-authored codes. Exclusive impossibility reasons
// deliberately bind two full edge identities with slash separators, so they
// need a wider (still control-free) vocabulary than the short API code regex.
var safeRoutingReasonPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,511}$`)

const (
	ViewerV2Protocol         = "viewer_v2"
	LegacyV6PathProtocol     = "legacy_v6"
	PathV1StateSchemaVersion = 7
	DefaultRoutingPageLimit  = 50
	MaxRoutingPageLimit      = 100
)

// MaxRoutingOverlayV2EncodedBytes bounds the complete additive routing DTO.
// The exact topology has an independent ceiling because it is independently
// useful when a checkpoint overlay is unavailable.
const MaxRoutingOverlayV2EncodedBytes = pathv1.MaxCheckpointBytes

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
	ID   string           `json:"id"`
	Type model.NodeType   `json:"type,omitempty"`
	Join model.JoinPolicy `json:"join,omitempty"`
}

type TopologyEdgeV2 struct {
	ID      string `json:"id"`
	From    string `json:"from,omitempty"`
	Outcome string `json:"outcome"`
	To      string `json:"to"`
}

type RoutingOverlayV2 struct {
	Protocol    string                    `json:"protocol"`
	Encoding    uint32                    `json:"encoding"`
	Edges       []RoutingEdgeOverlayV2    `json:"edges"`
	Scopes      []RoutingScopeOverlayV2   `json:"scopes,omitempty"`
	Joins       []RoutingJoinOverlayV2    `json:"joins,omitempty"`
	Closures    []RoutingClosureOverlayV2 `json:"closures,omitempty"`
	StateCounts RoutingStateCountsV2      `json:"stateCounts"`
	Details     RoutingDetailsV2          `json:"details"`
	Aggregate   RoutingAggregateOverlayV2 `json:"aggregate"`
}

// RoutingPageRequestV2 applies one stable window to each rich detail table.
// Keeping aggregation and graph edges outside this window lets every page
// retain the same truthful overview while clients page one detail tab.
type RoutingPageRequestV2 struct {
	Offset int
	Limit  int
}

func (r RoutingPageRequestV2) normalized() RoutingPageRequestV2 {
	if r.Offset < 0 {
		r.Offset = 0
	}
	if r.Limit <= 0 {
		r.Limit = DefaultRoutingPageLimit
	}
	if r.Limit > MaxRoutingPageLimit {
		r.Limit = MaxRoutingPageLimit
	}
	return r
}

type RoutingPageV2 struct {
	Offset  int  `json:"offset"`
	Limit   int  `json:"limit"`
	Total   int  `json:"total"`
	HasMore bool `json:"hasMore"`
}

type RoutingDetailsV2 struct {
	Generations   RoutingGenerationPageV2   `json:"generations"`
	Scopes        RoutingScopePageV2        `json:"scopes"`
	Closures      RoutingClosurePageV2      `json:"closures"`
	CauseSets     RoutingCauseSetPageV2     `json:"causeSets"`
	Causes        RoutingCausePageV2        `json:"causes"`
	Detachments   RoutingDetachmentPageV2   `json:"detachments"`
	DetachedSinks RoutingDetachedSinkPageV2 `json:"detachedSinks"`
	Contacts      RoutingContactPageV2      `json:"contacts"`
}

type RoutingContactPageV2 struct {
	Page  RoutingPageV2             `json:"page"`
	Items []RoutingContactOverlayV2 `json:"items"`
}

// RoutingContactOverlayV2 mirrors the legacy viewer's Contact projection for
// schema-7 runs: reminder schedule and budget state only. Assignee and
// escalation target pass the same provenance funnel; no message bodies,
// prompts, or command payloads are exposed.
type RoutingContactOverlayV2 struct {
	NodeID           string      `json:"nodeId"`
	Attempt          uint64      `json:"attempt,omitempty"`
	State            string      `json:"state"`
	Kind             string      `json:"kind"`
	Assignee         *Provenance `json:"assignee,omitempty"`
	Cadence          string      `json:"cadence,omitempty"`
	LastContactAt    time.Time   `json:"lastContactAt,omitzero"`
	NextContactAt    time.Time   `json:"nextContactAt,omitzero"`
	BudgetUsed       int         `json:"budgetUsed"`
	BudgetMax        int         `json:"budgetMax"`
	EscalationTarget *Provenance `json:"escalationTarget,omitempty"`
	EscalatedAt      time.Time   `json:"escalatedAt,omitzero"`
	Paused           bool        `json:"paused"`
}

type RoutingGenerationPageV2 struct {
	Page  RoutingPageV2                `json:"page"`
	Items []RoutingGenerationOverlayV2 `json:"items"`
}

type RoutingScopePageV2 struct {
	Page  RoutingPageV2          `json:"page"`
	Items []RoutingScopeDetailV2 `json:"items"`
}

type RoutingClosurePageV2 struct {
	Page  RoutingPageV2            `json:"page"`
	Items []RoutingClosureDetailV2 `json:"items"`
}

type RoutingCauseSetPageV2 struct {
	Page  RoutingPageV2              `json:"page"`
	Items []RoutingCauseSetOverlayV2 `json:"items"`
}

type RoutingCausePageV2 struct {
	Page  RoutingPageV2           `json:"page"`
	Items []RoutingCauseOverlayV2 `json:"items"`
}

type RoutingDetachmentPageV2 struct {
	Page  RoutingPageV2                `json:"page"`
	Items []RoutingDetachmentOverlayV2 `json:"items"`
}

type RoutingDetachedSinkPageV2 struct {
	Page  RoutingPageV2                  `json:"page"`
	Items []RoutingDetachedSinkOverlayV2 `json:"items"`
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

type RoutingGenerationOverlayV2 struct {
	ReservationID    string                         `json:"reservationId"`
	ActivationID     string                         `json:"activationId,omitempty"`
	NodeID           string                         `json:"nodeId"`
	ScopeID          string                         `json:"scopeId"`
	Generation       uint64                         `json:"generation"`
	Policy           pathv1.JoinPolicy              `json:"policy"`
	ReservationState pathv1.ReservationState        `json:"reservationState"`
	ReceiptResult    pathv1.ActivationReceiptResult `json:"receiptResult,omitempty"`
	InputCount       int                            `json:"inputCount"`
	OutputPathID     string                         `json:"outputPathId,omitempty"`
	WinnerPathID     string                         `json:"winnerPathId,omitempty"`
}

type RoutingScopeDetailV2 struct {
	ID                    string                  `json:"id"`
	ParentScopeID         string                  `json:"parentScopeId,omitempty"`
	ParentBranchEdgeID    string                  `json:"parentBranchEdgeId,omitempty"`
	ForkActivationID      string                  `json:"forkActivationId,omitempty"`
	ForkOutputPathID      string                  `json:"forkOutputPathId,omitempty"`
	Generation            uint64                  `json:"generation"`
	ExpectedBranchEdgeIDs []string                `json:"expectedBranchEdgeIds"`
	JoinNodeID            string                  `json:"joinNodeId,omitempty"`
	JoinReservationID     string                  `json:"joinReservationId,omitempty"`
	State                 pathv1.ScopeState       `json:"state"`
	CloseReason           pathv1.ScopeCloseReason `json:"closeReason,omitempty"`
}

type RoutingJoinOverlayV2 struct {
	ReservationID string                  `json:"reservationId"`
	NodeID        string                  `json:"nodeId"`
	ScopeID       string                  `json:"scopeId"`
	Policy        pathv1.JoinPolicy       `json:"policy"`
	State         pathv1.ReservationState `json:"state"`
	Generation    uint64                  `json:"generation"`
	ActivationID  string                  `json:"activationId,omitempty"`
	WinnerPathID  string                  `json:"winnerPathId,omitempty"`
	Detached      int                     `json:"detached"`
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
	CauseDigest   string              `json:"causeDigest"`
}

type RoutingClosureDetailV2 = RoutingClosureOverlayV2

type RoutingCauseSetOverlayV2 struct {
	Digest   string   `json:"digest"`
	CauseIDs []string `json:"causeIds"`
}

type RoutingCauseOverlayV2 struct {
	ID                 string              `json:"id"`
	TerminalKind       pathv1.TerminalKind `json:"terminalKind"`
	DispositionReason  string              `json:"dispositionReason"`
	SourcePathID       string              `json:"sourcePathId,omitempty"`
	SourceActivationID string              `json:"sourceActivationId,omitempty"`
	EventSeq           int64               `json:"eventSeq"`
}

type RoutingDetachmentOverlayV2 struct {
	ID                       string `json:"id"`
	ReservationID            string `json:"reservationId"`
	CandidateID              string `json:"candidateId"`
	WinnerPathID             string `json:"winnerPathId"`
	JoinActivationID         string `json:"joinActivationId"`
	JoinActivationGeneration uint64 `json:"joinActivationGeneration"`
	ReasonCode               string `json:"reasonCode"`
	ActivatedSeq             int64  `json:"activatedSeq"`
}

type RoutingDetachedSinkOverlayV2 struct {
	PathID              string           `json:"pathId"`
	SourceActivationID  string           `json:"sourceActivationId"`
	SourceGeneration    uint64           `json:"sourceGeneration"`
	TargetReservationID string           `json:"targetReservationId"`
	CandidateID         string           `json:"candidateId"`
	DetachmentID        string           `json:"detachmentId"`
	ReasonCode          string           `json:"reasonCode"`
	State               pathv1.PathState `json:"state"`
	EventSeq            int64            `json:"eventSeq"`
}

type RoutingStateCountV2 struct {
	State string `json:"state"`
	Count int    `json:"count"`
}

type RoutingStateCountsV2 struct {
	Paths             []RoutingStateCountV2 `json:"paths"`
	Scopes            []RoutingStateCountV2 `json:"scopes"`
	Reservations      []RoutingStateCountV2 `json:"reservations"`
	Propagation       []RoutingStateCountV2 `json:"propagation"`
	DetachedPathCount int                   `json:"detachedPathCount"`
	DetachedSinkCount int                   `json:"detachedSinkCount"`
}

type RoutingAggregateOverlayV2 struct {
	Paths         int    `json:"paths"`
	Scopes        int    `json:"scopes"`
	Reservations  int    `json:"reservations"`
	Activations   int    `json:"activations"`
	Closures      int    `json:"closures"`
	Propagation   int    `json:"propagation"`
	CauseRecords  int    `json:"causeRecords"`
	CauseSets     int    `json:"causeSets"`
	Detachments   int    `json:"detachments"`
	DetachedSinks int    `json:"detachedSinks"`
	Settled       bool   `json:"settled"`
	Result        string `json:"result,omitempty"`
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
	Page               RoutingPageRequestV2
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

	overlay, reason := projectRoutingOverlay(*aggregate, topology.semanticHash, topology.edges, input.Page)
	if reason != "" {
		result.RoutingUnavailableReason = reason
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
	edges, cardinalityDiagnostics := model.NormalizeEdgesWithinBudget(tmpl)
	if cardinalityDiagnostics.HasErrors() {
		return exactTopologyProjection{}, RoutingUnavailableOverBudget
	}
	if diagnostics := model.Validate(tmpl, edges); diagnostics.HasErrors() {
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
		join := tmpl.Nodes[id].Join
		if join != "" && join != model.JoinAll && join != model.JoinAny {
			return exactTopologyProjection{}, RoutingUnavailableInconsistent
		}
		topology.Nodes = append(topology.Nodes, TopologyNodeV2{ID: id, Type: nodeType, Join: join})
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
		) {
			return false
		}
		if node.Join != "" && !b.add(exactTopologyV2LiteralBytes(`,"join":`), exactTopologyV2StringBytes(string(node.Join))) {
			return false
		}
		if !b.add(1) {
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

func projectRoutingOverlay(aggregate pathv1.AggregateView, semanticHash string, topologyEdges map[string]TopologyEdgeV2, request RoutingPageRequestV2) (*RoutingOverlayV2, RoutingUnavailableReason) {
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
			return nil, RoutingUnavailableInconsistent
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
	detachmentCounts := indexRoutingDetachmentsByReservation(routing.Detachments)
	joins, ok := projectRoutingJoins(routing, arrivals, detachmentCounts)
	if !ok {
		return nil, RoutingUnavailableInconsistent
	}
	overlay.Joins = joins
	for _, closure := range routing.CandidateClosures {
		overlay.Closures = append(overlay.Closures, RoutingClosureOverlayV2{ReservationID: closure.Key.ReservationID, CandidateID: closure.Key.CandidateID, TerminalKind: closure.TerminalKind, CauseDigest: closure.CauseDigest})
	}
	slices.SortFunc(overlay.Closures, func(a, b RoutingClosureOverlayV2) int {
		if n := cmp.Compare(a.ReservationID, b.ReservationID); n != 0 {
			return n
		}
		return cmp.Compare(a.CandidateID, b.CandidateID)
	})
	details, sinks, ok := projectRoutingDetails(routing, request)
	if !ok {
		return nil, RoutingUnavailableInconsistent
	}
	contacts, ok := projectRoutingContacts(aggregate, request)
	if !ok {
		return nil, RoutingUnavailableInconsistent
	}
	details.Contacts = contacts
	overlay.Details = details
	overlay.StateCounts = projectRoutingStateCounts(routing)
	overlay.Aggregate = RoutingAggregateOverlayV2{
		Paths: len(routing.Paths), Scopes: len(routing.Scopes), Reservations: len(routing.Reservations),
		Activations: len(routing.Activations), Closures: len(routing.CandidateClosures), Propagation: len(routing.Propagation),
		CauseRecords: len(routing.CauseRecords), CauseSets: len(routing.CauseSets),
		Detachments: len(routing.Detachments), DetachedSinks: sinks,
	}
	if completion, err := pathv1.AssessAggregateCompletion(aggregate); err == nil {
		overlay.Aggregate.Settled = true
		overlay.Aggregate.Result = completion.Result
	} else if !errors.Is(err, pathv1.ErrAggregateUnsettled) {
		return nil, RoutingUnavailableInconsistent
	}
	encoded, err := json.Marshal(overlay)
	if err != nil {
		return nil, RoutingUnavailableInconsistent
	}
	if len(encoded) > MaxRoutingOverlayV2EncodedBytes {
		return nil, RoutingUnavailableOverBudget
	}
	return overlay, ""
}

// projectRoutingContacts renders the schema-7 contact registry. A verified
// checkpoint cannot carry unresolvable references or noncanonical timestamps,
// so any such finding marks the overlay inconsistent rather than guessing.
func projectRoutingContacts(aggregate pathv1.AggregateView, request RoutingPageRequestV2) (RoutingContactPageV2, bool) {
	items := make([]RoutingContactOverlayV2, 0, len(aggregate.Contacts))
	for id, record := range aggregate.Contacts {
		marker, ok := aggregate.SideEffects[id]
		if !ok || marker.Kind != pathv1.SideEffectContact {
			return RoutingContactPageV2{}, false
		}
		activation, ok := aggregate.Routing.Activations[record.ActivationID]
		if !ok {
			return RoutingContactPageV2{}, false
		}
		reservation, ok := aggregate.Routing.Reservations[activation.ReservationID]
		if !ok {
			return RoutingContactPageV2{}, false
		}
		last, lastErr := pathv1.ParseCanonicalTimestamp(record.LastContactedAt)
		next, nextErr := pathv1.ParseCanonicalTimestamp(record.NextContactAt)
		escalated, escalatedErr := pathv1.ParseCanonicalTimestamp(record.EscalatedAt)
		if lastErr != nil || nextErr != nil || escalatedErr != nil {
			return RoutingContactPageV2{}, false
		}
		maxInt := uint64(int(^uint(0) >> 1))
		items = append(items, RoutingContactOverlayV2{
			NodeID: reservation.NodeID, Attempt: record.Attempt, State: marker.State, Kind: string(record.Kind),
			Assignee: safeProvenance(record.Assignee), Cadence: safeCadence(record.Cadence),
			LastContactAt: last, NextContactAt: next,
			BudgetUsed: int(min(record.Used, maxInt)), BudgetMax: int(min(record.Budget, maxInt)),
			EscalationTarget: safeProvenance(record.EscalationTarget),
			EscalatedAt:      escalated, Paused: marker.State == pathv1.ContactStatePaused,
		})
	}
	slices.SortFunc(items, func(a, b RoutingContactOverlayV2) int {
		if n := cmp.Compare(a.NodeID, b.NodeID); n != 0 {
			return n
		}
		return cmp.Compare(a.Attempt, b.Attempt)
	})
	return RoutingContactPageV2{Page: routingPage(len(items), request), Items: routingPageItems(items, request)}, true
}

func indexRoutingDetachmentsByReservation(detachments map[pathv1.DetachmentKey]pathv1.DetachmentRecord) map[pathv1.ReservationID]int {
	counts := make(map[pathv1.ReservationID]int, len(detachments))
	for _, detachment := range detachments {
		counts[detachment.ReservationID]++
	}
	return counts
}

func projectRoutingJoins(routing *pathv1.RoutingState, arrivals map[routingArrivalKey]struct{}, detachmentCounts map[pathv1.ReservationID]int) ([]RoutingJoinOverlayV2, bool) {
	joins := make([]RoutingJoinOverlayV2, 0)
	for _, reservation := range routing.Reservations {
		if reservation.JoinPolicy != pathv1.JoinAll && reservation.JoinPolicy != pathv1.JoinAny {
			continue
		}
		join := RoutingJoinOverlayV2{
			ReservationID: reservation.ID, NodeID: reservation.NodeID, ScopeID: reservation.ScopeID,
			Policy: reservation.JoinPolicy, State: reservation.State, Generation: reservation.Generation,
			Detached: detachmentCounts[reservation.ID],
		}
		if reservation.Activation != nil {
			activation, exists := routing.Activations[reservation.Activation.ID]
			if !exists || activation.Ref != *reservation.Activation || activation.ReservationID != reservation.ID {
				return nil, false
			}
			join.ActivationID = activation.ID
			if reservation.JoinPolicy == pathv1.JoinAny && activation.Receipt.Result == pathv1.ReceiptActivated {
				if len(activation.InputPathIDs) != 1 {
					return nil, false
				}
				join.WinnerPathID = activation.InputPathIDs[0]
			}
		}
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

func projectRoutingDetails(routing *pathv1.RoutingState, request RoutingPageRequestV2) (RoutingDetailsV2, int, bool) {
	request = request.normalized()
	generations := make([]RoutingGenerationOverlayV2, 0, len(routing.Reservations))
	for _, reservation := range routing.Reservations {
		item := RoutingGenerationOverlayV2{
			ReservationID: reservation.ID, NodeID: reservation.NodeID, ScopeID: reservation.ScopeID,
			Generation: reservation.Generation, Policy: reservation.JoinPolicy, ReservationState: reservation.State,
		}
		if reservation.Activation != nil {
			activation, ok := routing.Activations[reservation.Activation.ID]
			if !ok || activation.Ref != *reservation.Activation || activation.ReservationID != reservation.ID {
				return RoutingDetailsV2{}, 0, false
			}
			item.ActivationID = activation.ID
			item.ReceiptResult = activation.Receipt.Result
			item.InputCount = len(activation.InputPathIDs)
			item.OutputPathID = activation.OutputPathID
			if reservation.JoinPolicy == pathv1.JoinAny && activation.Receipt.Result == pathv1.ReceiptActivated {
				if len(activation.InputPathIDs) != 1 {
					return RoutingDetailsV2{}, 0, false
				}
				item.WinnerPathID = activation.InputPathIDs[0]
			}
		}
		generations = append(generations, item)
	}
	slices.SortFunc(generations, func(a, b RoutingGenerationOverlayV2) int {
		if n := cmp.Compare(a.NodeID, b.NodeID); n != 0 {
			return n
		}
		if n := cmp.Compare(a.Generation, b.Generation); n != 0 {
			return n
		}
		return cmp.Compare(a.ReservationID, b.ReservationID)
	})

	scopes := make([]RoutingScopeDetailV2, 0, len(routing.Scopes))
	for _, scope := range routing.Scopes {
		scopes = append(scopes, RoutingScopeDetailV2{
			ID: scope.ID, ParentScopeID: scope.ParentScopeID, ParentBranchEdgeID: scope.ParentBranchEdgeID,
			ForkActivationID: scope.ForkActivationID, ForkOutputPathID: scope.ForkOutputPathID,
			Generation: scope.Generation, ExpectedBranchEdgeIDs: append([]string(nil), scope.ExpectedBranchEdgeIDs...),
			JoinNodeID: scope.JoinNodeID, JoinReservationID: scope.JoinReservationID,
			State: scope.State, CloseReason: scope.CloseReason,
		})
	}
	slices.SortFunc(scopes, func(a, b RoutingScopeDetailV2) int { return cmp.Compare(a.ID, b.ID) })

	closures := make([]RoutingClosureDetailV2, 0, len(routing.CandidateClosures))
	for _, closure := range routing.CandidateClosures {
		closures = append(closures, RoutingClosureDetailV2{
			ReservationID: closure.Key.ReservationID, CandidateID: closure.Key.CandidateID,
			TerminalKind: closure.TerminalKind, CauseDigest: closure.CauseDigest,
		})
	}
	slices.SortFunc(closures, func(a, b RoutingClosureDetailV2) int {
		if n := cmp.Compare(a.ReservationID, b.ReservationID); n != 0 {
			return n
		}
		return cmp.Compare(a.CandidateID, b.CandidateID)
	})

	causeSets := make([]RoutingCauseSetOverlayV2, 0, len(routing.CauseSets))
	for _, set := range routing.CauseSets {
		for _, causeID := range set.CauseIDs {
			if _, ok := routing.CauseRecords[causeID]; !ok {
				return RoutingDetailsV2{}, 0, false
			}
		}
		causeSets = append(causeSets, RoutingCauseSetOverlayV2{Digest: set.Digest, CauseIDs: append([]string(nil), set.CauseIDs...)})
	}
	slices.SortFunc(causeSets, func(a, b RoutingCauseSetOverlayV2) int { return cmp.Compare(a.Digest, b.Digest) })

	causes := make([]RoutingCauseOverlayV2, 0, len(routing.CauseRecords))
	for _, cause := range routing.CauseRecords {
		if !safeRoutingReasonPattern.MatchString(cause.DispositionReason) {
			return RoutingDetailsV2{}, 0, false
		}
		causes = append(causes, RoutingCauseOverlayV2{
			ID: cause.ID, TerminalKind: cause.TerminalKind, DispositionReason: cause.DispositionReason,
			SourcePathID: cause.SourcePathID, SourceActivationID: cause.SourceActivationID, EventSeq: cause.EventSeq,
		})
	}
	slices.SortFunc(causes, func(a, b RoutingCauseOverlayV2) int { return cmp.Compare(a.ID, b.ID) })

	detachments := make([]RoutingDetachmentOverlayV2, 0, len(routing.Detachments))
	for _, detachment := range routing.Detachments {
		if !safeRoutingReasonPattern.MatchString(detachment.ReasonCode) {
			return RoutingDetailsV2{}, 0, false
		}
		detachments = append(detachments, RoutingDetachmentOverlayV2{
			ID: detachment.ID, ReservationID: detachment.ReservationID, CandidateID: detachment.CandidateID,
			WinnerPathID: detachment.WinnerPathID, JoinActivationID: detachment.JoinActivation.ID,
			JoinActivationGeneration: detachment.JoinActivation.Generation,
			ReasonCode:               detachment.ReasonCode, ActivatedSeq: detachment.ActivatedSeq,
		})
	}
	slices.SortFunc(detachments, func(a, b RoutingDetachmentOverlayV2) int { return cmp.Compare(a.ID, b.ID) })

	sinks := make([]RoutingDetachedSinkOverlayV2, 0)
	for _, path := range routing.Paths {
		if path.State != pathv1.PathDetachedSink {
			continue
		}
		if path.DetachedSink == nil || !safeRoutingReasonPattern.MatchString(path.DetachedSink.ReasonCode) {
			return RoutingDetailsV2{}, 0, false
		}
		sinks = append(sinks, RoutingDetachedSinkOverlayV2{
			PathID: path.ID, SourceActivationID: path.SourceActivation.ID,
			SourceGeneration: path.SourceActivation.Generation, TargetReservationID: path.TargetReservationID,
			CandidateID: path.CandidateID, DetachmentID: path.DetachedSink.DetachmentID,
			ReasonCode: path.DetachedSink.ReasonCode, State: path.State, EventSeq: path.DetachedSink.EventSeq,
		})
	}
	slices.SortFunc(sinks, func(a, b RoutingDetachedSinkOverlayV2) int { return cmp.Compare(a.PathID, b.PathID) })

	return RoutingDetailsV2{
		Generations:   RoutingGenerationPageV2{Page: routingPage(len(generations), request), Items: routingPageItems(generations, request)},
		Scopes:        RoutingScopePageV2{Page: routingPage(len(scopes), request), Items: routingPageItems(scopes, request)},
		Closures:      RoutingClosurePageV2{Page: routingPage(len(closures), request), Items: routingPageItems(closures, request)},
		CauseSets:     RoutingCauseSetPageV2{Page: routingPage(len(causeSets), request), Items: routingPageItems(causeSets, request)},
		Causes:        RoutingCausePageV2{Page: routingPage(len(causes), request), Items: routingPageItems(causes, request)},
		Detachments:   RoutingDetachmentPageV2{Page: routingPage(len(detachments), request), Items: routingPageItems(detachments, request)},
		DetachedSinks: RoutingDetachedSinkPageV2{Page: routingPage(len(sinks), request), Items: routingPageItems(sinks, request)},
	}, len(sinks), true
}

func routingPage(total int, request RoutingPageRequestV2) RoutingPageV2 {
	request = request.normalized()
	end := request.Offset + request.Limit
	if end < request.Offset || end > total {
		end = total
	}
	return RoutingPageV2{Offset: request.Offset, Limit: request.Limit, Total: total, HasMore: end < total}
}

func routingPageItems[T any](items []T, request RoutingPageRequestV2) []T {
	request = request.normalized()
	if request.Offset >= len(items) {
		return []T{}
	}
	end := request.Offset + request.Limit
	if end < request.Offset || end > len(items) {
		end = len(items)
	}
	return append([]T(nil), items[request.Offset:end]...)
}

func projectRoutingStateCounts(routing *pathv1.RoutingState) RoutingStateCountsV2 {
	pathCounts := make(map[string]int)
	scopeCounts := make(map[string]int)
	reservationCounts := make(map[string]int)
	propagationCounts := make(map[string]int)
	detachedPaths, detachedSinks := 0, 0
	for _, path := range routing.Paths {
		pathCounts[string(path.State)]++
		if path.DetachmentSetID != "" {
			detachedPaths++
		}
		if path.State == pathv1.PathDetachedSink {
			detachedSinks++
		}
	}
	for _, scope := range routing.Scopes {
		scopeCounts[string(scope.State)]++
	}
	for _, reservation := range routing.Reservations {
		reservationCounts[string(reservation.State)]++
	}
	for _, propagation := range routing.Propagation {
		propagationCounts[string(propagation.State)]++
	}
	return RoutingStateCountsV2{
		Paths: routingStateCounts(pathCounts), Scopes: routingStateCounts(scopeCounts),
		Reservations: routingStateCounts(reservationCounts), Propagation: routingStateCounts(propagationCounts),
		DetachedPathCount: detachedPaths, DetachedSinkCount: detachedSinks,
	}
}

func routingStateCounts(counts map[string]int) []RoutingStateCountV2 {
	states := make([]string, 0, len(counts))
	for state := range counts {
		states = append(states, state)
	}
	slices.Sort(states)
	result := make([]RoutingStateCountV2, 0, len(states))
	for _, state := range states {
		result = append(result, RoutingStateCountV2{State: state, Count: counts[state]})
	}
	return result
}
