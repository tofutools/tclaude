package model

import (
	"fmt"
	"slices"
	"strings"
)

// Bounded loops (tcl-eu70x9). A process graph is a DAG except where an author
// bounds an edge that goes back: a decision's outcome may route to a node
// earlier in the graph, starting another round of the work between the two,
// as long as the outcome is declared in the decision's loops with a round
// limit:
//
//	decide:
//	  type: decision
//	  next: {approved: merge, changes: implement, escalate: ask-owner}
//	  loops:
//	    changes: {maxIterations: 3, onExhausted: escalate}
//
// Every cycle must contain such an edge. The nodes on the paths from the edge's
// target to the decision are the loop BODY, and taking the edge runs the whole
// body again. To keep a round well defined the body is single-entry, it is left
// only through decisions, and it holds no join: any; two loops' bodies are
// nested or disjoint.
//
// The single entry is either the edge's target (the work comes first, then the
// decision) or the decision itself, a decision-first loop:
//
//	review:
//	  type: decision
//	  next: {approved: merge, changes: fix}
//	  loops:
//	    changes: {maxIterations: 3, onExhausted: ...}
//	fix:
//	  next: {done: review}
//
// Here the work arrives at review, and fix runs only when review asks for
// changes: every other node of the body is reached only through the edge that
// goes back. That is what lets two review loops run alongside each other, each
// with its own fix, and join once both approve. No other loop may sit inside a
// decision-first body.

// MinLoopIterations and MaxLoopIterations bound a loop's round limit. A
// limit of one would forbid the very round the edge exists for.
const (
	MinLoopIterations = 2
	MaxLoopIterations = 100
)

// LoopBound bounds one back edge: the work between its target and its
// decision runs at most MaxIterations rounds. When a round would go past the
// limit, the decision follows OnExhausted, another of its own outcomes,
// instead; with none, the outcome that goes back is refused and the decider
// has to choose another one.
type LoopBound struct {
	MaxIterations int    `json:"maxIterations" yaml:"maxIterations"`
	OnExhausted   string `json:"onExhausted,omitempty" yaml:"onExhausted,omitempty"`
}

// Loop is one bounded back edge and the body it repeats, in a single graph
// scope. Body and Exits are sorted, so a loop has one description.
type Loop struct {
	From          string
	Outcome       string
	To            string
	MaxIterations int
	OnExhausted   string
	// Body holds the nodes on the forward paths from To to From, both included.
	Body []string
	// Exits are the forward edges from a body node to a node outside it.
	Exits []Edge
	// DecisionFirst marks a loop entered at its decision (From) rather than at
	// its target: the rest of the body runs only once the decision sends the
	// work back. Validation proves the entry is one or the other.
	DecisionFirst bool
}

// Contains reports whether a node is in the loop's body.
func (l Loop) Contains(nodeID string) bool {
	_, found := slices.BinarySearch(l.Body, nodeID)
	return found
}

// IsBoundedLoopEdge reports whether an edge is a bounded back edge: its
// source declares a loop bound for its outcome.
func IsBoundedLoopEdge(tmpl *Template, edge Edge) bool {
	if tmpl == nil || edge.From == "" {
		return false
	}
	node, ok := tmpl.Nodes[edge.From]
	if !ok {
		return false
	}
	_, bounded := node.Loops[edge.Outcome]
	return bounded && node.Next[edge.Outcome] == edge.To
}

// isCycleExemptEdge reports the edges acyclicity ignores: bounded back edges
// and the poison-escalation retry edge.
func isCycleExemptEdge(tmpl *Template, edge Edge) bool {
	return IsBoundedLoopEdge(tmpl, edge) || IsPoisonEscalationRetryEdge(tmpl, edge)
}

// forwardAdjacency is the graph without its bounded back edges, which
// validation proves acyclic.
func forwardAdjacency(tmpl *Template, edges []Edge) (map[string][]string, map[string][]string) {
	out := map[string][]string{}
	in := map[string][]string{}
	for _, edge := range edges {
		if edge.From == "" || isCycleExemptEdge(tmpl, edge) {
			continue
		}
		if _, ok := tmpl.Nodes[edge.To]; !ok {
			continue
		}
		out[edge.From] = append(out[edge.From], edge.To)
		in[edge.To] = append(in[edge.To], edge.From)
	}
	return out, in
}

func reachableFrom(start string, adj map[string][]string) map[string]bool {
	seen := map[string]bool{start: true}
	stack := []string{start}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, next := range adj[id] {
			if !seen[next] {
				seen[next] = true
				stack = append(stack, next)
			}
		}
	}
	return seen
}

// Loops returns the template scope's bounded loops in (from, outcome) order.
// A bound whose edge does not go back, or names no outcome, yields no loop;
// validation reports it. The scope's forward graph must be acyclic.
func Loops(tmpl *Template, edges []Edge) []Loop {
	if tmpl == nil {
		return nil
	}
	out, in := forwardAdjacency(tmpl, edges)
	var loops []Loop
	for _, from := range sortedKeys(tmpl.Nodes) {
		node := tmpl.Nodes[from]
		for _, outcome := range sortedKeys(node.Loops) {
			to, ok := node.Next[outcome]
			if _, exists := tmpl.Nodes[to]; !ok || !exists {
				continue
			}
			ahead := reachableFrom(to, out)
			if !ahead[from] {
				continue
			}
			behind := reachableFrom(from, in)
			loop := Loop{
				From: from, Outcome: outcome, To: to,
				MaxIterations: node.Loops[outcome].MaxIterations,
				OnExhausted:   node.Loops[outcome].OnExhausted,
			}
			for id := range ahead {
				if behind[id] {
					loop.Body = append(loop.Body, id)
				}
			}
			slices.Sort(loop.Body)
			loop.DecisionFirst = loopEntries(tmpl, edges, loop)[from]
			loops = append(loops, loop)
		}
	}
	// A decision-first loop's repeated work is not on the forward paths through
	// its decision, but it belongs to every loop around that decision.
	for changed := true; changed; {
		changed = false
		for i := range loops {
			for _, inner := range loops {
				if !inner.DecisionFirst || !loops[i].Contains(inner.From) || loopInside(inner, loops[i]) {
					continue
				}
				loops[i].Body = append(loops[i].Body, inner.Body...)
				slices.Sort(loops[i].Body)
				loops[i].Body = slices.Compact(loops[i].Body)
				changed = true
			}
		}
	}
	for i := range loops {
		loop := &loops[i]
		// An outer loop's entries are judged on its body with the inner
		// decision-first bodies added: an edge from an inner loop's repeated
		// work into the outer body is not an entry from outside.
		loop.DecisionFirst = loopEntries(tmpl, edges, *loop)[loop.From]
		for _, edge := range edges {
			if edge.From == "" || !loop.Contains(edge.From) || loop.Contains(edge.To) || isCycleExemptEdge(tmpl, edge) {
				continue
			}
			loop.Exits = append(loop.Exits, edge)
		}
		slices.SortFunc(loop.Exits, compareEdge)
	}
	return loops
}

// validateLoops checks every declared loop bound and the shape of the body it
// repeats. Acyclicity of the remaining graph is validateAcyclic's.
func validateLoops(tmpl *Template, edges []Edge) Diagnostics {
	var diagnostics Diagnostics
	add := func(code, path, message string) {
		diagnostics = append(diagnostics, diagError(code, path, message))
	}
	for _, nodeID := range sortedKeys(tmpl.Nodes) {
		node := tmpl.Nodes[nodeID]
		if len(node.Loops) == 0 {
			continue
		}
		path := "nodes." + nodeID + ".loops"
		if node.Type != NodeTypeDecision {
			add("loop_source", path, "only a decision can send work back for another round; loops are valid on decision nodes")
			continue
		}
		free := 0
		for _, outcome := range sortedKeys(node.Next) {
			if _, bounded := node.Loops[outcome]; !bounded {
				free++
			}
		}
		for _, outcome := range sortedKeys(node.Loops) {
			bound := node.Loops[outcome]
			outcomePath := path + "." + outcome
			if _, ok := node.Next[outcome]; !ok {
				add("loop_outcome", outcomePath, fmt.Sprintf("loops names outcome %q, which next does not route", outcome))
				continue
			}
			if bound.MaxIterations < MinLoopIterations || bound.MaxIterations > MaxLoopIterations {
				add("loop_iterations", outcomePath+".maxIterations", fmt.Sprintf(
					"maxIterations is the most rounds the loop runs and must be %d..%d; got %d",
					MinLoopIterations, MaxLoopIterations, bound.MaxIterations))
			}
			switch exhausted := strings.TrimSpace(bound.OnExhausted); exhausted {
			case "":
				if free == 0 {
					add("loop_exhaustion", outcomePath, "with every outcome going back, a used-up loop has no way on; name onExhausted or add an outcome that leaves the loop")
				}
			case outcome:
				add("loop_exhaustion", outcomePath+".onExhausted", "onExhausted must name another outcome than the one that goes back")
			default:
				if _, ok := node.Next[exhausted]; !ok {
					add("loop_exhaustion", outcomePath+".onExhausted", fmt.Sprintf("onExhausted names outcome %q, which next does not route", exhausted))
				} else if _, bounded := node.Loops[exhausted]; bounded {
					add("loop_exhaustion", outcomePath+".onExhausted", fmt.Sprintf("onExhausted names %q, which goes back itself", exhausted))
				}
			}
		}
	}
	if len(diagnostics) > 0 {
		return diagnostics
	}
	// A bound is only meaningful on an edge that closes a cycle.
	for _, nodeID := range sortedKeys(tmpl.Nodes) {
		node := tmpl.Nodes[nodeID]
		for _, outcome := range sortedKeys(node.Loops) {
			to := node.Next[outcome]
			if _, ok := tmpl.Nodes[to]; !ok {
				continue
			}
			if to == nodeID {
				add("loop_not_back", "nodes."+nodeID+".loops."+outcome, fmt.Sprintf(
					"outcome %q routes %q back to itself; a loop goes back to earlier work", outcome, nodeID))
				continue
			}
			out, _ := forwardAdjacency(tmpl, edges)
			if !reachableFrom(to, out)[nodeID] {
				add("loop_not_back", "nodes."+nodeID+".loops."+outcome, fmt.Sprintf(
					"outcome %q goes forward to %q; only an edge back to earlier work can be bounded", outcome, to))
			}
		}
	}
	if len(diagnostics) > 0 || validateAcyclic(tmpl, edges).HasErrors() {
		return diagnostics
	}
	loops := Loops(tmpl, edges)
	for _, loop := range loops {
		path := "nodes." + loop.From + ".loops." + loop.Outcome
		// The body is entered at its target, or only at its decision.
		entry := loop.To
		if loop.DecisionFirst {
			entry = loop.From
		}
		if loop.Contains(tmpl.Start) && tmpl.Start != entry {
			add("loop_entry", path, fmt.Sprintf("the process starts at %q inside the loop body; a loop is entered only at %q or at its decision %q", tmpl.Start, loop.To, loop.From))
		}
		for _, edge := range edges {
			if edge.From == "" || isCycleExemptEdge(tmpl, edge) || loop.Contains(edge.From) || !loop.Contains(edge.To) || edge.To == entry {
				continue
			}
			add("loop_entry", "nodes."+edge.From+".next."+edge.Outcome, fmt.Sprintf(
				"%q enters the loop body at %q; a loop back to %q is entered only at %q, or only at its decision %q",
				edge.From, edge.To, loop.To, loop.To, loop.From))
		}
		for _, exit := range loop.Exits {
			if tmpl.Nodes[exit.From].Type != NodeTypeDecision {
				// Plain words, as the editor's problems line shows them: which
				// step, which route, and the two ways to fix it.
				add("loop_exit", "nodes."+exit.From+".next."+exit.Outcome, fmt.Sprintf(
					"step %q is inside the loop that %q sends back to %q, but its %q route goes to %q, outside the loop. "+
						"Only the decision %q may lead out of the loop: point %q at a step inside the loop, "+
						"or remove that route (a %q with no route fails the run)",
					exit.From, loop.From, loop.To, exit.Outcome, exit.To, loop.From, exit.Outcome, exit.Outcome))
			}
		}
		for _, id := range loop.Body {
			if tmpl.Nodes[id].Join == JoinAny {
				add("loop_join_any", "nodes."+id+".join", fmt.Sprintf(
					"join: any inside the loop back to %q would leave losing branches running into the next round", loop.To))
			}
		}
	}
	if cycle := decisionFirstOrderCycle(tmpl, edges, loops); cycle != "" {
		add("loop_decision_first_order", "nodes."+cycle, fmt.Sprintf(
			"%q is reached from a loop that starts at a decision and also leads into one, so neither can run first; route it through the decision of its own loop", cycle))
	}
	for i := range loops {
		for j := range loops {
			if i != j && loops[i].DecisionFirst && loopInside(loops[j], loops[i]) {
				add("loop_decision_first_nested", "nodes."+loops[j].From+".loops."+loops[j].Outcome, fmt.Sprintf(
					"the loop back to %q sits inside the loop that starts at decision %q; nothing may loop inside a loop entered at its decision",
					loops[j].To, loops[i].From))
			}
		}
		for j := i + 1; j < len(loops); j++ {
			if !loopsNestOrDisjoint(loops[i], loops[j]) {
				add("loop_overlap", "nodes."+loops[j].From+".loops."+loops[j].Outcome, fmt.Sprintf(
					"the loop back to %q overlaps the loop from %q back to %q without one containing the other",
					loops[j].To, loops[i].From, loops[i].To))
			}
		}
	}
	return diagnostics
}

// loopEntries is the set of body nodes the work enters from outside the body:
// the start when it is in the body, and every target of a forward edge from
// outside.
func loopEntries(tmpl *Template, edges []Edge, loop Loop) map[string]bool {
	entries := map[string]bool{}
	if loop.Contains(tmpl.Start) {
		entries[tmpl.Start] = true
	}
	for _, edge := range edges {
		if edge.From == "" || isCycleExemptEdge(tmpl, edge) || loop.Contains(edge.From) || !loop.Contains(edge.To) {
			continue
		}
		entries[edge.To] = true
	}
	return entries
}

// decisionFirstOrderCycle checks the order a run executes in: a
// decision-first loop's decision runs before its repeated work, so its bounded
// edge orders and the edges from its body back into the decision do not. That
// order must be acyclic (the engine prepares nodes in it); it returns a node
// on a cycle, or "".
func decisionFirstOrderCycle(tmpl *Template, edges []Edge, loops []Loop) string {
	adj := map[string][]string{}
	for _, edge := range edges {
		if edge.From == "" {
			continue
		}
		if _, ok := tmpl.Nodes[edge.To]; !ok {
			continue
		}
		orders := !isCycleExemptEdge(tmpl, edge)
		for _, loop := range loops {
			if !loop.DecisionFirst {
				continue
			}
			if edge.From == loop.From && edge.Outcome == loop.Outcome {
				orders = true
			} else if edge.To == loop.From && edge.From != loop.From && loop.Contains(edge.From) {
				orders = false
			}
		}
		if orders {
			adj[edge.From] = append(adj[edge.From], edge.To)
		}
	}
	state := map[string]int{}
	var found string
	var visit func(id string) bool
	visit = func(id string) bool {
		state[id] = 1
		for _, next := range adj[id] {
			if state[next] == 1 || state[next] == 0 && visit(next) {
				if found == "" {
					found = next
				}
				return true
			}
		}
		state[id] = 2
		return false
	}
	for _, id := range sortedKeys(tmpl.Nodes) {
		if state[id] == 0 && visit(id) {
			return found
		}
	}
	return ""
}

// loopInside reports whether inner's body lies within outer's.
func loopInside(inner, outer Loop) bool {
	for _, id := range inner.Body {
		if !outer.Contains(id) {
			return false
		}
	}
	return true
}

func loopsNestOrDisjoint(a, b Loop) bool {
	shared := 0
	for _, id := range a.Body {
		if b.Contains(id) {
			shared++
		}
	}
	return shared == 0 || shared == len(a.Body) || shared == len(b.Body)
}
