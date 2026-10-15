package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
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

// A compound task's stages are dispatched under their stage ids
// ("work.do", "work.review"); the dispatch lifts onto the authored task, and
// the open attempt wins over a closed earlier stage.
func TestProcessProjectionLiftsStageDispatchOntoItsTask(t *testing.T) {
	skeleton := newProcessProjectionSkeleton(&model.Template{
		ID: "staged", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{"next": "work"}},
			"work": {Type: model.NodeTypeTask, Next: model.Next{"next": "end"},
				Performer: &model.Performer{Kind: model.PerformerAgent, Profile: "dev"}},
			"end": {Type: model.NodeTypeEnd},
		},
	})
	projection := skeleton.project(processRunView{
		ID:         "run_staged",
		Checkpoint: engine.Checkpoint{Nodes: map[string]engine.NodeStatus{"start": engine.NodeDone, "work.review": engine.NodeRunning}},
		Performers: []processRunPerformerView{
			{NodeID: "work.review", Attempt: 1, State: db.ProcessDispatchDelivered, AgentID: "agt_reviewer"},
			{NodeID: "work.do", Attempt: 1, State: db.ProcessDispatchClosed, AgentID: "agt_coder"},
		},
	})
	var work processProjectionNode
	for _, node := range projection.Nodes {
		if node.ID == "work" {
			work = node
		}
	}
	if assert.NotNil(t, work.Dispatch) {
		assert.Equal(t, "agt_reviewer", work.Dispatch.AgentID)
		assert.Equal(t, "work.review", work.Dispatch.NodeID, "acting on the attempt names the stage")
	}
	assert.Equal(t, "agt_reviewer", work.Performer.Ref)
}
