package epochv8

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// EligibilityMatrixVersion is deliberately independent of the moving legacy
// and schema-7 host selectors. Schema 8 admits exactly the production path-v1
// shapes reviewed for TCL-602; a later production expansion must consciously
// version this matrix rather than silently changing old checkpoint meaning.
const EligibilityMatrixVersion = "path_v1_production_2026_07"

type EligibilityStatus string

const (
	EligibilitySupported   EligibilityStatus = "supported"
	EligibilityUnsupported EligibilityStatus = "unsupported"
)

type EligibilityReason string

const (
	EligibilityReasonSupported       EligibilityReason = "supported"
	EligibilityReasonInvalidTemplate EligibilityReason = "invalid_template"
	EligibilityReasonCompound        EligibilityReason = "compound_not_supported"
	EligibilityReasonProgram         EligibilityReason = "program_not_supported"
	EligibilityReasonPerformer       EligibilityReason = "performer_not_supported"
	EligibilityReasonContact         EligibilityReason = "contact_not_supported"
	EligibilityReasonChoiceOutcomes  EligibilityReason = "choice_outcomes_not_supported"
	EligibilityReasonEnd             EligibilityReason = "end_result_not_supported"
	EligibilityReasonWait            EligibilityReason = "wait_not_supported"
	EligibilityReasonParallel        EligibilityReason = "parallel_topology_not_supported"
	EligibilityReasonNodeType        EligibilityReason = "node_type_not_supported"
)

type TemplateClassification struct {
	MatrixVersion        string
	Status               EligibilityStatus
	Reason               EligibilityReason
	RequiredCapabilities []Capability
	candidate            *TemplateCandidate
}

// Candidate returns an immutable candidate only for supported input. An
// unsupported classification is a routing fact for S2; it does not say that
// the template is globally uninstantiable or choose its later legacy route.
func (c TemplateClassification) Candidate() *TemplateCandidate {
	if c.Status != EligibilitySupported || c.candidate == nil {
		return nil
	}
	copy := *c.candidate
	copy.epoch = cloneEpoch(copy.epoch)
	return &copy
}

// TemplateCandidate contains only canonical digests and a normalized graph.
// Source bytes are consumed during classification and never retained.
type TemplateCandidate struct{ epoch TemplateEpoch }

func (c *TemplateCandidate) TemplateRef() string {
	if c == nil {
		return ""
	}
	return c.epoch.TemplateRef
}

func (c *TemplateCandidate) SourceDigest() string {
	if c == nil {
		return ""
	}
	return c.epoch.TemplateSourceDigest
}

func (c *TemplateCandidate) GraphTotals() (nodes, edges int) {
	if c == nil {
		return 0, 0
	}
	return len(c.epoch.Graph.Nodes), len(c.epoch.Graph.Edges)
}

// ClassifyTemplateSource is the stable S1 classifier for untrusted exact
// source. Invalid, compound, program, or otherwise ineligible input returns an
// unsupported classification rather than selecting or disabling a fallback.
func ClassifyTemplateSource(source []byte) (TemplateClassification, error) {
	classification := TemplateClassification{
		MatrixVersion: EligibilityMatrixVersion,
		Status:        EligibilityUnsupported,
		Reason:        EligibilityReasonInvalidTemplate,
	}
	if len(source) == 0 || len(source) > model.MaxProcessTemplateSourceBytes {
		return classification, nil
	}
	parsed, err := model.ParseExactSource(source)
	if err != nil {
		return classification, nil
	}
	if parsed == nil || parsed.Template == nil || parsed.Diagnostics.HasErrors() ||
		!canonicalTemplateRef(parsed.Ref) || !canonicalDigest(parsed.SourceHash) {
		return classification, nil
	}
	reason := classifyProductionPathV1(parsed.Template)
	capabilities := requiredCapabilities(parsed.Template)
	classification.RequiredCapabilities = capabilities
	if reason != EligibilityReasonSupported {
		classification.Reason = reason
		return classification, nil
	}
	graph, err := buildEpochGraph(parsed.Template, parsed.Edges)
	if err != nil {
		return classification, fmt.Errorf("%w: build candidate graph: %v", ErrInvalid, err)
	}
	epoch := TemplateEpoch{
		TemplateRef:          parsed.Ref,
		TemplateSourceDigest: parsed.SourceHash,
		RequiredCapabilities: capabilities,
		Graph:                graph,
	}
	classification.Status = EligibilitySupported
	classification.Reason = EligibilityReasonSupported
	classification.candidate = &TemplateCandidate{epoch: epoch}
	return classification, nil
}

func classifyProductionPathV1(tmpl *model.Template) EligibilityReason {
	hasParallel := false
	nodeIDs := make([]string, 0, len(tmpl.Nodes))
	for nodeID, node := range tmpl.Nodes {
		nodeIDs = append(nodeIDs, nodeID)
		if node.Type == model.NodeTypeParallel {
			hasParallel = true
		}
	}
	slices.Sort(nodeIDs)
	for _, nodeID := range nodeIDs {
		node := tmpl.Nodes[nodeID]
		if node.IsCompound() {
			return EligibilityReasonCompound
		}
		switch node.Type {
		case model.NodeTypeTask, model.NodeTypeDecision:
			if node.Performer == nil {
				return EligibilityReasonPerformer
			}
			if node.Performer.Kind == model.PerformerProgram {
				return EligibilityReasonProgram
			}
			if node.Performer.Kind != model.PerformerHuman && node.Performer.Kind != model.PerformerAgent {
				return EligibilityReasonPerformer
			}
			if !frozenProductionContactEligible(*node.Performer) {
				return EligibilityReasonContact
			}
			if node.Type == model.NodeTypeTask && len(node.Performer.ChoiceOutcomes) != 0 {
				return EligibilityReasonChoiceOutcomes
			}
		case model.NodeTypeEnd:
			if nodeID == tmpl.Start {
				return EligibilityReasonEnd
			}
			switch strings.ToLower(strings.TrimSpace(node.Result)) {
			case "", "pass", "passed", "success", "succeeded", "complete", "completed", "done", "ok",
				"fail", "failed", "failure", "error":
			default:
				return EligibilityReasonEnd
			}
		case model.NodeTypeWait:
			if !productionPathV1WaitEligible(node.Wait) {
				return EligibilityReasonWait
			}
		case model.NodeTypeStart:
		case model.NodeTypeParallel:
			if len(node.Next) < 2 {
				return EligibilityReasonParallel
			}
		default:
			return EligibilityReasonNodeType
		}
	}
	if hasParallel && !productionPathV1ParallelTopologyEligible(tmpl) {
		return EligibilityReasonParallel
	}
	return EligibilityReasonSupported
}

// frozenProductionContactEligible copies the exact bounded contact-admission
// rule present when EligibilityMatrixVersion was reviewed. Do not replace it
// with the moving host helper: parity drift must fail an explicit test and
// force a conscious matrix-version decision.
func frozenProductionContactEligible(performer model.Performer) bool {
	const (
		maxContactFieldBytes   = 256
		maxContactCadenceBytes = 32
	)
	if performer.Contact != nil {
		cadence, err := time.ParseDuration(strings.TrimSpace(performer.Contact.Cadence))
		if err != nil || cadence <= 0 || len(cadence.String()) > maxContactCadenceBytes || performer.Contact.Budget <= 0 {
			return false
		}
		escalation := strings.TrimSpace(performer.Contact.EscalationTarget)
		if escalation == "" || len(escalation) > maxContactFieldBytes {
			return false
		}
	}
	if performer.Kind != model.PerformerHuman {
		return true
	}
	assignee := strings.TrimSpace(performer.Assignee)
	if assignee == "" {
		assignee = strings.TrimSpace(performer.Profile)
	}
	if assignee == "" {
		assignee = "human:operator"
	} else if !strings.HasPrefix(assignee, "human:") && !strings.HasPrefix(assignee, "role:") {
		assignee = "human:" + assignee
	}
	return len(assignee) <= maxContactFieldBytes
}

func productionPathV1WaitEligible(wait *model.WaitConfig) bool {
	if wait == nil {
		return false
	}
	duration := strings.TrimSpace(wait.Duration)
	until := strings.TrimSpace(wait.Until)
	signal := strings.TrimSpace(wait.Signal)
	configured := 0
	for _, value := range []string{duration, until, signal} {
		if value != "" {
			configured++
		}
	}
	if configured != 1 {
		return false
	}
	if signal != "" {
		return true
	}
	if until != "" {
		instant, err := model.ParseRFC3339(until)
		return err == nil && !instant.IsZero()
	}
	parsed, err := time.ParseDuration(duration)
	return err == nil && parsed > 0
}

// This intentionally freezes the released poison-propagation exception from
// the current production selector. It is duplicated, not shared with host
// dispatch, so later host widening cannot mutate schema-8 eligibility.
func productionPathV1ParallelTopologyEligible(tmpl *model.Template) bool {
	for outerID, outer := range tmpl.Nodes {
		if outer.Type != model.NodeTypeParallel {
			continue
		}
		type visit struct {
			nodeID   string
			fallible bool
		}
		stack := make([]visit, 0, len(outer.Next))
		for _, nodeID := range outer.Next {
			stack = append(stack, visit{nodeID: nodeID})
		}
		seen := make(map[visit]struct{})
		for len(stack) > 0 {
			current := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if _, ok := seen[current]; ok {
				continue
			}
			seen[current] = struct{}{}
			node, ok := tmpl.Nodes[current.nodeID]
			if !ok {
				continue
			}
			if current.nodeID != outerID && node.Type == model.NodeTypeParallel && current.fallible {
				return false
			}
			fallible := current.fallible || (node.Type == model.NodeTypeTask && model.FailTarget(node.Next) == "")
			for _, nextID := range node.Next {
				stack = append(stack, visit{nodeID: nextID, fallible: fallible})
			}
		}
	}
	return true
}

func requiredCapabilities(tmpl *model.Template) []Capability {
	set := map[Capability]struct{}{CapabilityFoundationV1: {}}
	for _, node := range tmpl.Nodes {
		if node.Type == model.NodeTypeParallel {
			set[CapabilityParallelAllV1] = struct{}{}
		}
		if node.Join == model.JoinAny {
			set[CapabilityParallelAllV1] = struct{}{}
			set[CapabilityParallelAnyV1] = struct{}{}
		}
	}
	result := make([]Capability, 0, len(set))
	for capability := range set {
		result = append(result, capability)
	}
	slices.Sort(result)
	return result
}

func buildEpochGraph(tmpl *model.Template, edges []model.Edge) (EpochGraph, error) {
	graph := EpochGraph{Nodes: make([]GraphNode, 0, len(tmpl.Nodes)), Edges: make([]GraphEdge, 0, len(edges))}
	for id, node := range tmpl.Nodes {
		semanticDigest, err := digestValue("template-node/v1", node)
		if err != nil {
			return EpochGraph{}, err
		}
		caps := []Capability{CapabilityFoundationV1}
		if node.Type == model.NodeTypeParallel || node.Join != "" {
			caps = append(caps, CapabilityParallelAllV1)
		}
		if node.Join == model.JoinAny {
			caps = append(caps, CapabilityParallelAnyV1)
		}
		graph.Nodes = append(graph.Nodes, GraphNode{
			ID: id, Type: string(node.Type), Join: string(node.Join), SemanticDigest: semanticDigest,
			RequiredCapabilities: caps,
		})
	}
	slices.SortFunc(graph.Nodes, func(a, b GraphNode) int { return cmp.Compare(a.ID, b.ID) })
	for _, edge := range edges {
		graph.Edges = append(graph.Edges, GraphEdge{From: edge.From, Outcome: edge.Outcome, To: edge.To})
	}
	slices.SortFunc(graph.Edges, compareGraphEdge)
	digest, err := graphDigest(graph)
	if err != nil {
		return EpochGraph{}, err
	}
	graph.Digest = digest
	return graph, nil
}

func compareGraphEdge(a, b GraphEdge) int {
	if value := cmp.Compare(a.From, b.From); value != 0 {
		return value
	}
	if value := cmp.Compare(a.Outcome, b.Outcome); value != 0 {
		return value
	}
	return cmp.Compare(a.To, b.To)
}
