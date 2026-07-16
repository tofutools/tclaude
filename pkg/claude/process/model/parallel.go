package model

import (
	"fmt"
	"slices"
	"strings"
)

func validateJoinAndDegree(tmpl *Template, edges []Edge) Diagnostics {
	if tmpl == nil {
		return nil
	}
	inbound := make(map[string]int, len(tmpl.Nodes))
	for _, edge := range edges {
		if edge.From != "" {
			inbound[edge.To]++
		}
	}
	var diagnostics Diagnostics
	for _, nodeID := range sortedKeys(tmpl.Nodes) {
		node := tmpl.Nodes[nodeID]
		path := "nodes." + nodeID
		if len(node.Next) > MaxNormalizedDegree {
			diagnostics = append(diagnostics, diagError("normalized_degree_limit", path+".next",
				fmt.Sprintf("normalized outgoing degree %d exceeds %d", len(node.Next), MaxNormalizedDegree)))
		}
		if inbound[nodeID] > MaxNormalizedDegree {
			diagnostics = append(diagnostics, diagError("normalized_inbound_limit", path+".join",
				fmt.Sprintf("normalized inbound candidate count %d exceeds %d", inbound[nodeID], MaxNormalizedDegree)))
		}
		switch node.Join {
		case "":
		case JoinAll, JoinAny:
			if inbound[nodeID] < 2 {
				diagnostics = append(diagnostics, diagError("join_degree", path+".join", "join requires at least two normalized inbound edges"))
			}
		default:
			diagnostics = append(diagnostics, diagError("invalid_join", path+".join", fmt.Sprintf("join must be all or any; got %q", node.Join)))
		}
	}
	return diagnostics
}

// A scope signature is the static counterpart of the runtime ScopeRecord
// lineage. Branch is the complete normalized edge tuple; tuple comparison is
// sufficient here and avoids making authoring validation depend on a semantic
// hash that is computed only after validation.
type scopeFrame struct {
	Fork   string
	Branch Edge
}

type scopeSignature []scopeFrame

func cloneSignature(in scopeSignature) scopeSignature {
	return append(scopeSignature(nil), in...)
}

func signaturesEqual(a, b scopeSignature) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func validateParallelScopePlan(tmpl *Template, edges []Edge) Diagnostics {
	if tmpl == nil {
		return nil
	}
	actual := make([]Edge, 0, len(edges))
	inbound := make(map[string][]Edge, len(tmpl.Nodes))
	outbound := make(map[string][]Edge, len(tmpl.Nodes))
	indegree := make(map[string]int, len(tmpl.Nodes))
	for nodeID := range tmpl.Nodes {
		indegree[nodeID] = 0
	}
	for _, edge := range edges {
		if edge.From == "" || isPoisonEscalationRetryEdge(tmpl, edge) {
			continue
		}
		if _, fromOK := tmpl.Nodes[edge.From]; !fromOK {
			continue
		}
		if _, toOK := tmpl.Nodes[edge.To]; !toOK {
			continue
		}
		actual = append(actual, edge)
		inbound[edge.To] = append(inbound[edge.To], edge)
		outbound[edge.From] = append(outbound[edge.From], edge)
		indegree[edge.To]++
	}
	for nodeID := range inbound {
		slices.SortFunc(inbound[nodeID], compareEdge)
	}
	for nodeID := range outbound {
		slices.SortFunc(outbound[nodeID], compareEdge)
	}

	ready := make([]string, 0, len(tmpl.Nodes))
	for nodeID, degree := range indegree {
		if degree == 0 {
			ready = append(ready, nodeID)
		}
	}
	slices.Sort(ready)
	order := make([]string, 0, len(tmpl.Nodes))
	for len(ready) > 0 {
		nodeID := ready[0]
		ready = ready[1:]
		order = append(order, nodeID)
		for _, edge := range outbound[nodeID] {
			indegree[edge.To]--
			if indegree[edge.To] == 0 {
				ready = append(ready, edge.To)
				slices.Sort(ready)
			}
		}
	}
	if len(order) != len(tmpl.Nodes) {
		// validateAcyclic owns the diagnostic for unsupported cycles. Avoid
		// deriving scope claims from an incomplete topological observation.
		return nil
	}

	outputs := make(map[string]scopeSignature, len(tmpl.Nodes))
	reachable := make(map[string]bool, len(tmpl.Nodes))
	reducers := make(map[string]string)
	var diagnostics Diagnostics
	for _, nodeID := range order {
		incoming := inbound[nodeID]
		incomingSignatures := make([]scopeSignature, 0, len(incoming))
		if nodeID == tmpl.Start {
			reachable[nodeID] = true
		}
		for _, edge := range incoming {
			if !reachable[edge.From] {
				continue
			}
			signature := cloneSignature(outputs[edge.From])
			if tmpl.Nodes[edge.From].Type == NodeTypeParallel {
				signature = append(signature, scopeFrame{Fork: edge.From, Branch: edge})
			}
			incomingSignatures = append(incomingSignatures, signature)
			reachable[nodeID] = true
		}
		if !reachable[nodeID] {
			continue
		}

		var output scopeSignature
		switch len(incomingSignatures) {
		case 0:
			output = nil
		case 1:
			output = cloneSignature(incomingSignatures[0])
		default:
			if allSignaturesEqual(incomingSignatures) {
				output = cloneSignature(incomingSignatures[0])
				break
			}
			prefix, fork, branches, ok := oneScopeReduction(incomingSignatures)
			if !ok || !completeForkBranches(tmpl, fork, branches) {
				diagnostics = append(diagnostics, crossScopeDiagnostic(nodeID,
					"multiple inbound edges must be a local merge or a complete one-scope reduction"))
				continue
			}
			if previous := reducers[fork]; previous != "" && previous != nodeID {
				diagnostics = append(diagnostics, crossScopeDiagnostic(nodeID,
					fmt.Sprintf("parallel fork %q already reduces at node %q", fork, previous)))
				continue
			}
			reducers[fork] = nodeID
			output = prefix
		}
		outputs[nodeID] = output
		if len(outbound[nodeID]) == 0 && len(output) > 0 {
			diagnostics = append(diagnostics, crossScopeDiagnostic(nodeID,
				fmt.Sprintf("branch escapes open parallel scope from fork %q", output[len(output)-1].Fork)))
		}
	}

	for _, nodeID := range sortedKeys(tmpl.Nodes) {
		if tmpl.Nodes[nodeID].Type == NodeTypeParallel && reachable[nodeID] && reducers[nodeID] == "" {
			diagnostics = append(diagnostics, diagError("cross_scope_join_v1", "nodes."+nodeID+".next",
				"parallel fork branches must have one complete structural reducer before leaving their scope"))
		}
	}
	_ = actual // retained as the normalized scope-plan input in debugger views.
	return diagnostics
}

func compareEdge(a, b Edge) int {
	if c := strings.Compare(a.From, b.From); c != 0 {
		return c
	}
	if c := strings.Compare(a.Outcome, b.Outcome); c != 0 {
		return c
	}
	return strings.Compare(a.To, b.To)
}

func allSignaturesEqual(signatures []scopeSignature) bool {
	for i := 1; i < len(signatures); i++ {
		if !signaturesEqual(signatures[0], signatures[i]) {
			return false
		}
	}
	return true
}

func oneScopeReduction(signatures []scopeSignature) (scopeSignature, string, map[Edge]struct{}, bool) {
	if len(signatures) < 2 || len(signatures[0]) == 0 {
		return nil, "", nil, false
	}
	depth := len(signatures[0])
	prefix := signatures[0][:depth-1]
	fork := signatures[0][depth-1].Fork
	branches := make(map[Edge]struct{}, len(signatures))
	for _, signature := range signatures {
		if len(signature) != depth || signature[depth-1].Fork != fork || !signaturesEqual(prefix, signature[:depth-1]) {
			return nil, "", nil, false
		}
		branches[signature[depth-1].Branch] = struct{}{}
	}
	return cloneSignature(prefix), fork, branches, true
}

func completeForkBranches(tmpl *Template, fork string, branches map[Edge]struct{}) bool {
	node, ok := tmpl.Nodes[fork]
	if !ok || node.Type != NodeTypeParallel || len(branches) != len(node.Next) {
		return false
	}
	for outcome, target := range node.Next {
		if _, ok := branches[Edge{From: fork, Outcome: outcome, To: target}]; !ok {
			return false
		}
	}
	return true
}

func crossScopeDiagnostic(nodeID, message string) Diagnostic {
	return diagError("cross_scope_join_v1", "nodes."+nodeID+".join", message)
}
