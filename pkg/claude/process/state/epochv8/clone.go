package epochv8

import (
	"encoding/json"
	"slices"

	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
)

func cloneGenesisWitness(value *pathv1.RuntimeGenesisWitnessV1) *pathv1.RuntimeGenesisWitnessV1 {
	if value == nil {
		return nil
	}
	data, _ := json.Marshal(value)
	var result pathv1.RuntimeGenesisWitnessV1
	_ = json.Unmarshal(data, &result)
	return &result
}

func cloneExecutionWitness(value *pathv1.ExecutionWitnessV1) *pathv1.ExecutionWitnessV1 {
	if value == nil {
		return nil
	}
	data, _ := json.Marshal(value)
	var result pathv1.ExecutionWitnessV1
	_ = json.Unmarshal(data, &result)
	return &result
}

func cloneGraph(graph EpochGraph) EpochGraph {
	graph.Nodes = slices.Clone(graph.Nodes)
	for i := range graph.Nodes {
		graph.Nodes[i].RequiredCapabilities = slices.Clone(graph.Nodes[i].RequiredCapabilities)
	}
	graph.Edges = slices.Clone(graph.Edges)
	return graph
}

func cloneEpoch(epoch TemplateEpoch) TemplateEpoch {
	epoch.RequiredCapabilities = slices.Clone(epoch.RequiredCapabilities)
	epoch.Graph = cloneGraph(epoch.Graph)
	return epoch
}

func cloneAuthority(authority AuthorityRecord) AuthorityRecord {
	authority.DependsOn = slices.Clone(authority.DependsOn)
	return authority
}

func cloneAuthorities(authorities []AuthorityRecord) []AuthorityRecord {
	result := make([]AuthorityRecord, len(authorities))
	for i := range authorities {
		result[i] = cloneAuthority(authorities[i])
	}
	return result
}

func cloneDiff(diff Diff) Diff {
	diff.AddedNodes = slices.Clone(diff.AddedNodes)
	diff.RemovedNodes = slices.Clone(diff.RemovedNodes)
	diff.ChangedNodes = slices.Clone(diff.ChangedNodes)
	diff.AddedEdges = slices.Clone(diff.AddedEdges)
	diff.RemovedEdges = slices.Clone(diff.RemovedEdges)
	return diff
}

func cloneHandoff(handoff Handoff) Handoff {
	if handoff.Target != nil {
		target := cloneAuthority(*handoff.Target)
		handoff.Target = &target
	}
	return handoff
}

func cloneHandoffs(handoffs []Handoff) []Handoff {
	result := make([]Handoff, len(handoffs))
	for i := range handoffs {
		result[i] = cloneHandoff(handoffs[i])
	}
	return result
}

func cloneApplyCore(core applyCore) applyCore {
	core.CandidateEpoch = cloneEpoch(core.CandidateEpoch)
	core.Diff = cloneDiff(core.Diff)
	core.Protected = cloneAuthorities(core.Protected)
	core.HandoffSet = cloneHandoffs(core.HandoffSet)
	return core
}

func cloneHistory(events []HistoryEvent) []HistoryEvent {
	result := make([]HistoryEvent, len(events))
	for i, event := range events {
		result[i] = event
		if event.Apply != nil {
			apply := *event.Apply
			apply.applyCore = cloneApplyCore(apply.applyCore)
			result[i].Apply = &apply
		}
		if event.Finish != nil {
			finish := *event.Finish
			result[i].Finish = &finish
		}
		if event.Runtime != nil {
			runtime := *event.Runtime
			runtime.Before = cloneAuthorities(runtime.Before)
			runtime.After = cloneAuthorities(runtime.After)
			runtime.GenesisWitness = cloneGenesisWitness(runtime.GenesisWitness)
			runtime.ExecutionWitness = cloneExecutionWitness(runtime.ExecutionWitness)
			result[i].Runtime = &runtime
		}
	}
	return result
}

func cloneWire(wire checkpointWire) checkpointWire {
	wire.Anchor.Capabilities = slices.Clone(wire.Anchor.Capabilities)
	wire.Anchor.OriginalEpoch = cloneEpoch(wire.Anchor.OriginalEpoch)
	wire.Anchor.InitialAuthorities = cloneAuthorities(wire.Anchor.InitialAuthorities)
	epochs := wire.Epochs
	wire.Epochs = make([]TemplateEpoch, len(epochs))
	for i := range epochs {
		wire.Epochs[i] = cloneEpoch(epochs[i])
	}
	wire.History = cloneHistory(wire.History)
	wire.Authorities = cloneAuthorities(wire.Authorities)
	return wire
}
