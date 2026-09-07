package app

import (
	"math"

	"github.com/tofutools/tclaude/internal/backend/model"
)

func validateEditorLayout(draft DefinitionDraft) error {
	if draft.EditorLayout == nil {
		return nil
	}
	if draft.Kind != model.DefinitionProcess || draft.Process == nil {
		return fail(ErrInvalid, "graph layout requires a process definition")
	}
	nodes := make(map[model.WorkNodeID]bool, len(draft.Process.Graph.Nodes))
	for _, node := range draft.Process.Graph.Nodes {
		nodes[node.ID] = true
	}
	for id, position := range draft.EditorLayout.Nodes {
		if !nodes[id] {
			return fail(ErrInvalid, "layout references unknown node %s", id)
		}
		if math.IsNaN(position.X) || math.IsNaN(position.Y) || math.IsInf(position.X, 0) || math.IsInf(position.Y, 0) || math.Abs(position.X) > 1e6 || math.Abs(position.Y) > 1e6 {
			return fail(ErrInvalid, "node %s layout is outside the supported canvas", id)
		}
	}
	return nil
}
