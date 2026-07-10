package agentd

import (
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/claude/process/worklist"
)

func TestBindWorklistBlockResolutionRejectsStaleItemGeneration(t *testing.T) {
	snapshot := store.Snapshot{
		Run: store.RunRecord{ID: "run"},
		State: &state.State{RunID: "run", Nodes: map[string]state.NodeState{
			"implement": {
				Status: state.NodeStatusBlocked, Children: []string{"implement.test.tests"},
				BlockedAttempt: 2, BlockedNodeID: "implement.test.tests", BlockedReason: "new poison", BlockedOwner: "human:operator",
			},
			"implement.test.tests": {
				Status: state.NodeStatusBlocked, Parent: "implement", Attempt: 2,
				BlockedAttempt: 2, BlockedNodeID: "implement.test.tests", BlockedReason: "new poison", BlockedOwner: "human:operator",
			},
		}},
	}
	stale := worklist.Item{Run: "run", Node: "implement.test.tests", Attempt: 1}
	_, err := bindWorklistBlockResolution(snapshot, stale, processWorklistActionRequest{
		Action: "cancel", Comment: "cancel the old poison", IdempotencyKey: "stale-1",
	}, "human:operator", "worklist-action:sha256:test")
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale work item was rebound to the current generation: %v", err)
	}
}
