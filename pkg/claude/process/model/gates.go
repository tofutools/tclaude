package model

import (
	"fmt"
	"strings"
)

// An agreement gate is a decision several deciders take together: each
// decider (an agent or a person) votes one of the gate's outcomes. When every
// vote is the agree outcome the gate takes that edge; otherwise it takes the
// first dissent's outcome, in decider order, and every decider's notes go on to
// whoever does the work again. The engine runs it as one vote per decider plus
// a tally of its own (engine.Prepare).

// MinGateDeciders and MaxGateDeciders bound a gate's deciders. One decider is a
// plain decision.
const (
	MinGateDeciders = 2
	MaxGateDeciders = 8
)

// IsGate reports whether node is an agreement gate.
func (n Node) IsGate() bool { return len(n.Deciders) > 0 }

// GateVoteID is the prepared id of the vote of the k-th decider (1-based) of
// gate gateID.
func GateVoteID(gateID string, k int) string { return fmt.Sprintf("%s.vote.%d", gateID, k) }

// GateTallyID is the prepared id of the engine-owned tally of gate gateID.
func GateTallyID(gateID string) string { return gateID + ".tally" }

// GateDerivedIDs are the ids an engine expansion of the gate takes.
func GateDerivedIDs(gateID string, node Node) []string {
	ids := make([]string, 0, len(node.Deciders)+1)
	for k := range node.Deciders {
		ids = append(ids, GateVoteID(gateID, k+1))
	}
	return append(ids, GateTallyID(gateID))
}

// validateGate checks a decision's deciders and agree outcome.
func validateGate(node Node, path string, diagnostics *templateDiagnosticCollector) bool {
	agree := strings.TrimSpace(node.Agree)
	if !node.IsGate() {
		if agree != "" {
			return diagnostics.Add(diagError("agree_without_deciders", path+".agree",
				"agree names the outcome all deciders must choose; it needs deciders"))
		}
		return true
	}
	if node.Performer != nil {
		if !diagnostics.Add(diagError("gate_performer", path+".performer",
			"an agreement gate is decided by its deciders; remove performer or deciders")) {
			return false
		}
	}
	if n := len(node.Deciders); n < MinGateDeciders || n > MaxGateDeciders {
		if !diagnostics.Add(diagError("gate_deciders", path+".deciders", fmt.Sprintf(
			"an agreement gate needs %d..%d deciders; got %d (one decider is a plain decision performer)",
			MinGateDeciders, MaxGateDeciders, n))) {
			return false
		}
	}
	for i, decider := range node.Deciders {
		deciderPath := fmt.Sprintf("%s.deciders[%d]", path, i)
		if decider.Kind != PerformerAgent && decider.Kind != PerformerHuman {
			if !diagnostics.Add(diagError("gate_decider_kind", deciderPath+".kind",
				"a decider is an agent or a person (human)")) {
				return false
			}
			continue
		}
		if !validatePerformer(decider, deciderPath, true, diagnostics) {
			return false
		}
	}
	switch _, routed := node.Next[agree]; {
	case agree == "":
		return diagnostics.Add(diagError("missing_agree", path+".agree",
			"an agreement gate names the outcome every decider must choose, for example agree: approve"))
	case !routed:
		return diagnostics.Add(diagError("agree_outcome", path+".agree",
			fmt.Sprintf("agree names outcome %q, which next does not route", agree)))
	case len(node.Next) < 2:
		return diagnostics.Add(diagError("gate_outcomes", path+".next",
			"an agreement gate needs an outcome besides agree for a decider to ask for changes"))
	}
	if _, looped := node.Loops[agree]; looped {
		if !diagnostics.Add(diagError("agree_loop", path+".loops."+agree,
			"the agree outcome moves the work on; it cannot send it back for another round")) {
			return false
		}
	}
	for _, outcome := range sortedKeys(node.Loops) {
		// Nobody chooses the gate's outcome by hand, so once the rounds run out
		// it needs somewhere to go.
		if strings.TrimSpace(node.Loops[outcome].OnExhausted) == "" {
			if !diagnostics.Add(diagError("gate_loop_exhausted", path+".loops."+outcome+".onExhausted",
				"a gate's loop needs onExhausted: nobody picks another outcome once its rounds run out")) {
				return false
			}
		}
	}
	return true
}
