package processcmd

import (
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

func TestPlanAdvanceRefusesFailedNode(t *testing.T) {
	snapshot := store.Snapshot{
		State: &state.State{
			Nodes: map[string]state.NodeState{
				"implement": {
					Type:    model.NodeTypeTask,
					Status:  state.NodeStatusFailed,
					Attempt: 1,
				},
			},
		},
	}
	tmpl := &model.Template{
		Nodes: map[string]model.Node{
			"implement": {Type: model.NodeTypeTask},
		},
	}

	_, err := planAdvance(snapshot, tmpl, "implement", "pass", "human:johan", "")
	if err == nil || !strings.Contains(err.Error(), "failed and cannot be advanced") {
		t.Fatalf("expected failed-node refusal, got %v", err)
	}
}

func TestPlanAdvanceRefusesPendingNode(t *testing.T) {
	snapshot := store.Snapshot{
		State: &state.State{
			Nodes: map[string]state.NodeState{
				"decide": {
					Type:   model.NodeTypeDecision,
					Status: state.NodeStatusPending,
				},
			},
		},
	}
	tmpl := &model.Template{
		Nodes: map[string]model.Node{
			"decide": {Type: model.NodeTypeDecision, Next: model.Next{"approve": "end"}},
		},
	}

	_, err := planAdvance(snapshot, tmpl, "decide", "approve", "human:johan", "")
	if err == nil || !strings.Contains(err.Error(), "only ready nodes can be advanced") {
		t.Fatalf("expected pending-node refusal, got %v", err)
	}
}

func TestPlanTaskFailDoesNotUsePassFallback(t *testing.T) {
	snapshot := store.Snapshot{
		State: &state.State{
			Nodes: map[string]state.NodeState{
				"implement": {
					Type:   model.NodeTypeTask,
					Status: state.NodeStatusReady,
				},
				"finish": {
					Type:   model.NodeTypeEnd,
					Status: state.NodeStatusPending,
				},
			},
		},
	}
	tmpl := &model.Template{
		Nodes: map[string]model.Node{
			"implement": {Type: model.NodeTypeTask, Next: model.Next{"pass": "finish"}},
			"finish":    {Type: model.NodeTypeEnd},
		},
	}

	entries, err := planAdvance(snapshot, tmpl, "implement", "fail", "human:johan", "")
	if err != nil {
		t.Fatal(err)
	}
	if hasNodeStatusEvent(entries, "finish") {
		t.Fatalf("failed task activated pass target: %#v", entries)
	}
	if !hasRunStatusEvent(entries, state.RunStatusFailed) {
		t.Fatalf("failed task without fail edge did not fail run: %#v", entries)
	}
}

func hasNodeStatusEvent(entries []evidence.LogEntry, nodeID string) bool {
	for _, entry := range entries {
		if entry.Event != nil && entry.Event.Type == state.EventNodeStatusSet && entry.Event.NodeID == nodeID {
			return true
		}
	}
	return false
}

func hasRunStatusEvent(entries []evidence.LogEntry, status state.RunStatus) bool {
	for _, entry := range entries {
		if entry.Event != nil && entry.Event.Type == state.EventRunStatusSet && entry.Event.RunStatus == status {
			return true
		}
	}
	return false
}
