package app

import "github.com/tofutools/tclaude/internal/backend/model"

// Legacy ordinary nodes settle their sole authored route regardless of its
// spelling. Multiple routes are retained for authoring, but cannot be executed
// as an implicit fan-out. Check the authored graph before stage expansion.
func executableProcessRoutes(graph model.WorkGraph) error {
	degree := map[model.WorkNodeID]int{}
	for _, edge := range graph.Edges {
		degree[edge.From]++
	}
	for _, node := range graph.Nodes {
		if node.RoutingMode == "" {
			continue
		}
		if node.RoutingMode != "single-route-v1" {
			return fail(ErrInvalid, "unknown process node routing mode")
		}
		switch node.Kind {
		case model.WorkNodeDecision, model.WorkNodeFork, model.WorkNodeEnd:
		default:
			if degree[node.ID] != 1 {
				return fail(ErrUnsupported, "node %s requires one ordinary route in imported single-route execution; retained alternatives must be resolved before starting", node.ID)
			}
		}
	}
	return nil
}
