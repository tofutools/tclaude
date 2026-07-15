package view

import (
	"cmp"
	"errors"
	"slices"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
)

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
	Protocol string                 `json:"protocol"`
	Encoding uint32                 `json:"encoding"`
	Edges    []RoutingEdgeOverlayV2 `json:"edges"`
}

// RoutingEdgeOverlayV2 deliberately collapses path records to an exact
// template edge, state, and count. It exposes no aggregate record, command,
// payload, completion basis, or evidence content.
type RoutingEdgeOverlayV2 struct {
	EdgeID string           `json:"edgeId"`
	State  pathv1.PathState `json:"state"`
	Count  int              `json:"count"`
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

	overlay, ok := projectRoutingOverlay(aggregate.Routing, topology.semanticHash, topology.edges)
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
		if (edge.From != "" && !safeIDPattern.MatchString(edge.From)) || !safeIDPattern.MatchString(edge.Outcome) || !safeIDPattern.MatchString(edge.To) {
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
	return exactTopologyProjection{topology: topology, semanticHash: hash, edges: edgesByID}, ""
}

func projectRoutingOverlay(routing *pathv1.RoutingState, semanticHash string, topologyEdges map[string]TopologyEdgeV2) (*RoutingOverlayV2, bool) {
	type overlayKey struct {
		edgeID string
		state  pathv1.PathState
	}
	counts := make(map[overlayKey]int)
	for _, path := range routing.Paths {
		if path.Edge == nil {
			continue
		}
		edge, ok := topologyEdges[path.Edge.ID]
		if !ok || path.Edge.TemplateRef != semanticHash || path.Edge.FromNodeID != edge.From || path.Edge.Outcome != edge.Outcome || path.Edge.ToNodeID != edge.To || !path.State.Valid() {
			return nil, false
		}
		counts[overlayKey{edgeID: edge.ID, state: path.State}]++
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
	return overlay, true
}
