package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

func TestClipProjectionText(t *testing.T) {
	assert.Equal(t, "short", clipProjectionText("  short \n", 8))
	assert.Equal(t, "abcdefg…", clipProjectionText("abcdefghij", 8))
	assert.Equal(t, "åäöåäöå…", clipProjectionText("åäöåäöåäöå", 8), "clipping counts runes, not bytes")
}

// A subprocess body runs under namespaced ids ("box/gate"); its live state
// lifts onto the subprocess node, which keeps kind "subprocess", exactly as a
// compound stage lifts onto its task.
func TestProcessProjectionLiftsSubprocessBodyOntoItsNode(t *testing.T) {
	skeleton := newProcessProjectionSkeleton(&model.Template{
		ID: "boxed", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{"next": "box"}},
			"box":   {Type: model.NodeType("subprocess"), Next: model.Next{"next": "end"}},
			"end":   {Type: model.NodeTypeEnd},
		},
	})
	projection := skeleton.project(processRunView{
		ID: "run_boxed",
		Checkpoint: engine.Checkpoint{Nodes: map[string]engine.NodeStatus{
			"start": engine.NodeDone, "box": engine.NodeRunning, "box/lint": engine.NodeDone,
			"box/gate": engine.NodeReady, "box/ok": engine.NodePending, "end": engine.NodePending,
		}},
		AwaitingDecisions: []processRunDecisionView{{NodeID: "box/gate"}},
	})
	byID := map[string]processProjectionNode{}
	for _, node := range projection.Nodes {
		byID[node.ID] = node
	}
	assert.Len(t, projection.Nodes, 3, "body nodes are not projected one by one")
	assert.Equal(t, "subprocess", byID["box"].Kind)
	assert.Equal(t, "awaiting", byID["box"].State, "a decision awaited inside the body lifts onto the node")
	assert.Equal(t, []string{"box"}, projection.Current)
}
