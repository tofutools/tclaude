package engine

import "github.com/tofutools/tclaude/pkg/claude/process/model"

// A run that spawns an agent for a step retires it per the step's fallback
// lifetime (model.PerformerFallback.Lifetime). The daemon does the retiring;
// these say what the definition and checkpoint decide.

// SpawnLifetime is the lifetime of an agent spawned for nodeID: its authored
// fallback lifetime, else the default, which for a gate's vote is
// model.DefaultDeciderLifetime. False for a node no agent performs.
func (d *Definition) SpawnLifetime(nodeID string) (string, bool) {
	node, ok := definitionNodeByID(d, nodeID)
	if !ok || node.agent == nil {
		return "", false
	}
	if node.agent.FallbackLifetime != "" {
		return node.agent.FallbackLifetime, true
	}
	if d.isGateVote(nodeID) {
		return model.DefaultDeciderLifetime(), true
	}
	return model.DefaultLifetime(), true
}

// isGateVote reports whether nodeID is one of a gate's votes.
func (d *Definition) isGateVote(nodeID string) bool {
	for _, tally := range d.gates {
		for _, voter := range d.nodes[tally].voters {
			if d.nodes[voter].id == nodeID {
				return true
			}
		}
	}
	return false
}

// StepFinished reports whether nodeID is done for good in checkpoint: settled
// (done, failed, skipped or canceled, so past every retry), and every loop
// around it has exited, so no round will run it again. An agent with
// lifetime "step" is retired then.
func (d *Definition) StepFinished(checkpoint Checkpoint, nodeID string) bool {
	node, ok := definitionNodeByID(d, nodeID)
	if !ok || !settledStatus(checkpoint.Nodes[nodeID]) {
		return false
	}
	// A loop's body runs again while its decision can still send it back: it
	// has exited once its decision has settled and the body is not reset.
	for _, loop := range node.loops {
		if !settledStatus(checkpoint.Nodes[d.nodes[d.loops[loop].from].id]) {
			return false
		}
	}
	return true
}

func settledStatus(status NodeStatus) bool {
	switch status {
	case NodeDone, NodeFailed, NodeSkipped, NodeCanceled:
		return true
	}
	return false
}
