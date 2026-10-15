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
// body again. To keep a round well defined the body is single-entry (only its
// target is entered from outside), it is left only through decisions, and it
// holds no join: any; two loops' bodies are nested or disjoint.

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
			for _, edge := range edges {
				if edge.From == "" || !loop.Contains(edge.From) || loop.Contains(edge.To) || isCycleExemptEdge(tmpl, edge) {
					continue
				}
				loop.Exits = append(loop.Exits, edge)
			}
			slices.SortFunc(loop.Exits, compareEdge)
			loops = append(loops, loop)
		}
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
			switch exhausted := strings.TrimSpace(bound.OnExhausted); {
			case exhausted == "":
				if free == 0 {
					add("loop_exhaustion", outcomePath, "with every outcome going back, a used-up loop has no way on; name onExhausted or add an outcome that leaves the loop")
				}
			case exhausted == outcome:
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
		if loop.Contains(tmpl.Start) && loop.To != tmpl.Start {
			add("loop_entry", path, fmt.Sprintf("the process starts at %q inside the loop body; a loop is entered only at %q", tmpl.Start, loop.To))
		}
		for _, edge := range edges {
			if edge.From == "" || isCycleExemptEdge(tmpl, edge) || loop.Contains(edge.From) || !loop.Contains(edge.To) || edge.To == loop.To {
				continue
			}
			add("loop_entry", "nodes."+edge.From+".next."+edge.Outcome, fmt.Sprintf(
				"%q enters the loop body at %q; a loop back to %q is entered only there", edge.From, edge.To, loop.To))
		}
		for _, exit := range loop.Exits {
			if tmpl.Nodes[exit.From].Type != NodeTypeDecision {
				add("loop_exit", "nodes."+exit.From+".next."+exit.Outcome, fmt.Sprintf(
					"%q leaves the loop back to %q for %q; only a decision may leave a loop", exit.From, loop.To, exit.To))
			}
		}
		for _, id := range loop.Body {
			if tmpl.Nodes[id].Join == JoinAny {
				add("loop_join_any", "nodes."+id+".join", fmt.Sprintf(
					"join: any inside the loop back to %q would leave losing branches running into the next round", loop.To))
			}
		}
	}
	for i := range loops {
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

func loopsNestOrDisjoint(a, b Loop) bool {
	shared := 0
	for _, id := range a.Body {
		if b.Contains(id) {
			shared++
		}
	}
	return shared == 0 || shared == len(a.Body) || shared == len(b.Body)
}
