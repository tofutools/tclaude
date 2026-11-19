package engine

import (
	"fmt"
	"slices"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// An agreement gate (model/gates.go) is prepared as three things:
//
//   - the gate's own id, a parallel fork, so every edge into it and every loop
//     back to it is unchanged;
//   - one vote per decider, G.vote.<k>: an ordinary decision (a person's, or
//     an agent's command) with the gate's outcomes, every one of them an edge
//     to the tally;
//   - the tally, G.tally: an engine-owned join: all of the votes. It holds the
//     gate's authored edges and loops and takes the verdict they add up to.
//
// So a vote is decided, recorded, and sent back for another round exactly as
// any decision is, and only the tally is new.

// prepareGate appends gate index's votes and tally right after it.
func prepareGate(definition *Definition, index int, node model.Node, params map[string]string, scope int,
	prefix string, appendNode func(definitionNode) (int, error), nexts map[int]model.Next) error {
	gateID := definition.nodes[index].id
	outcomes := sortedOutcomes(node.Next)
	tallyID := model.GateTallyID(gateID)
	fork := make(model.Next, len(node.Deciders))
	voters := make([]int, 0, len(node.Deciders))
	for k, decider := range node.Deciders {
		voteID := model.GateVoteID(gateID, k+1)
		vote := definitionNode{
			id: voteID, kind: definitionDecision, verdicts: outcomes,
			parent: -1, doAnchor: -1, planAnchor: -1, approvalGate: -1,
			scope: scope, scopeEnd: -1, entry: -1,
		}
		if decider.Kind == model.PerformerHuman {
			vote.ask = humanAskOf(decider, params)
		}
		if decider.Kind == model.PerformerAgent {
			agent, err := bindAgent(voteID, decider, nil, params, definition.fallback)
			if err != nil {
				return err
			}
			constrainAgent(agent, decider, prefix)
			vote.agent = agent
			vote.maxAttempts = 1
		}
		voteIndex, err := appendNode(vote)
		if err != nil {
			return err
		}
		toTally := make(model.Next, len(outcomes))
		for _, outcome := range outcomes {
			toTally[outcome] = tallyID
		}
		nexts[voteIndex] = toTally
		fork[fmt.Sprintf("vote.%d", k+1)] = voteID
		voters = append(voters, voteIndex)
	}
	// Deciders are independent: no agent casts two votes at one gate.
	for _, voter := range voters {
		agent := definition.nodes[voter].agent
		if agent == nil {
			continue
		}
		for _, other := range voters {
			if other != voter && definition.nodes[other].agent != nil &&
				!slices.Contains(agent.DifferentFrom, definition.nodes[other].id) {
				agent.DifferentFrom = append(agent.DifferentFrom, definition.nodes[other].id)
			}
		}
	}
	tallyIndex, err := appendNode(definitionNode{
		id: tallyID, kind: definitionTally, verdicts: outcomes, joinAll: true,
		agree: node.Agree, voters: voters,
		parent: -1, doAnchor: -1, planAnchor: -1, approvalGate: -1,
		scope: scope, scopeEnd: -1, entry: -1,
	})
	if err != nil {
		return err
	}
	nexts[index] = fork
	nexts[tallyIndex] = namespacedNext(node.Next, prefix)
	if definition.gates == nil {
		definition.gates = map[string]int{}
	}
	definition.gates[gateID] = tallyIndex
	return nil
}

// gateLoops moves a scope's loops off its gates and onto their tallies, which
// hold the gates' edges: a loop from a gate leaves from its tally, and a loop
// whose body holds a gate holds its votes and tally too.
func gateLoops(tmpl *model.Template, prefix string, loops []pendingLoop) []pendingLoop {
	gateOf := func(id string) (string, model.Node, bool) {
		local := id[len(prefix):]
		node, ok := tmpl.Nodes[local]
		return local, node, ok && node.Type == model.NodeTypeDecision && node.IsGate()
	}
	for i := range loops {
		loop := &loops[i]
		if local, _, ok := gateOf(loop.from); ok {
			loop.from = prefix + model.GateTallyID(local)
		}
		for j, exit := range loop.exits {
			if local, _, ok := gateOf(exit[0]); ok {
				loop.exits[j][0] = prefix + model.GateTallyID(local)
			}
		}
		body := slices.Clone(loop.body)
		for _, id := range loop.body {
			if local, node, ok := gateOf(id); ok {
				for _, derived := range model.GateDerivedIDs(local, node) {
					body = append(body, prefix+derived)
				}
			}
		}
		slices.Sort(body)
		loop.body = slices.Compact(body)
	}
	return loops
}

// tallyVerdict is what a tally's votes add up to: its agree outcome when every
// vote chose it, else the first other outcome in decider order.
func tallyVerdict(checkpoint Checkpoint, definition *Definition, tally definitionNode) (string, error) {
	dissent := ""
	for _, voter := range tally.voters {
		chosen, ok := voteOf(checkpoint, definition, voter)
		if !ok {
			return "", fmt.Errorf("%w: tally %q is ready before vote %q is in",
				ErrInvalidTransition, tally.id, definition.nodes[voter].id)
		}
		if chosen != tally.agree && dissent == "" {
			dissent = chosen
		}
	}
	if dissent == "" {
		return tally.agree, nil
	}
	return dissent, nil
}

// voteOf is the outcome a vote chose this round, if it has voted.
func voteOf(checkpoint Checkpoint, definition *Definition, voter int) (string, bool) {
	for _, edgeIndex := range definition.nodes[voter].outgoing {
		edge := definition.edges[edgeIndex]
		if checkpoint.Edges[edge.from][edge.outcome] == EdgeArrived {
			return edge.outcome, true
		}
	}
	return "", false
}

// GateVote is one decider's vote on an agreement gate this round.
type GateVote struct {
	// NodeID is the vote's prepared node, G.vote.<k>.
	NodeID string
	// Verdict is the outcome it chose, empty while it has not voted.
	Verdict string
	// Human says a person casts it.
	Human bool
}

// Gate describes an agreement gate of the run: its tally node, the outcome
// every decider must choose, and each vote this round in decider order.
type Gate struct {
	NodeID  string
	TallyID string
	Agree   string
	Votes   []GateVote
}

// Gates lists the run's agreement gates as checkpoint has them, in prepared
// order.
func (d *Definition) Gates(checkpoint Checkpoint) []Gate {
	if len(d.gates) == 0 {
		return nil
	}
	var gates []Gate
	for index := range d.nodes {
		tally := d.nodes[index]
		if tally.kind != definitionTally {
			continue
		}
		gate := Gate{TallyID: tally.id, Agree: tally.agree}
		for id, tallyIndex := range d.gates {
			if tallyIndex == index {
				gate.NodeID = id
			}
		}
		for _, voter := range tally.voters {
			verdict, _ := voteOf(checkpoint, d, voter)
			gate.Votes = append(gate.Votes, GateVote{NodeID: d.nodes[voter].id, Verdict: verdict,
				Human: d.nodes[voter].agent == nil})
		}
		gates = append(gates, gate)
	}
	return gates
}

// GateOfTally returns the gate whose tally is nodeID.
func (d *Definition) GateOfTally(nodeID string) (string, bool) {
	index, ok := d.index[nodeID]
	if !ok || d.nodes[index].kind != definitionTally {
		return "", false
	}
	for id, tallyIndex := range d.gates {
		if tallyIndex == index {
			return id, true
		}
	}
	return "", false
}

// GateSettlement is what one commit did at an agreement gate: the verdict its
// votes added up to, and the edge that took the gate on from its tally.
type GateSettlement struct {
	GateID  string
	TallyID string
	Verdict string
}

// GateSettlements derives the gates a commit settled from the checkpoints
// around it, for evidence: a tally that went done, or one whose loop started
// another round.
func (d *Definition) GateSettlements(before, after Checkpoint) []GateSettlement {
	if len(d.gates) == 0 {
		return nil
	}
	var settled []GateSettlement
	for _, gate := range d.Gates(before) {
		index := d.index[gate.TallyID]
		tally := d.nodes[index]
		settlement := GateSettlement{GateID: gate.NodeID, TallyID: tally.id}
		iterated := false
		for _, li := range tally.sourceOf {
			loop := d.loops[li]
			if iterationsTaken(after, d, loop) > iterationsTaken(before, d, loop) {
				// The round's votes were reset with it: the verdict is the
				// loop's outcome.
				settlement.Verdict, iterated = loop.outcome, true
			}
		}
		switch {
		case iterated:
		case before.Nodes[tally.id] != NodeDone && after.Nodes[tally.id] == NodeDone:
			verdict, err := tallyVerdict(after, d, tally)
			if err != nil {
				continue
			}
			settlement.Verdict = verdict
		default:
			continue
		}
		settled = append(settled, settlement)
	}
	return settled
}
