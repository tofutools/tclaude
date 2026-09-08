package app

import "github.com/tofutools/tclaude/internal/backend/model"

// possibleGraphEdges excludes only routes disproved by a settled decision's
// durable answer. Waiting, uncertain and missing answers remain possible. The
// graph is the admitted DAG; retries of a decision use its latest attempt.
func possibleGraphEdges(graph model.WorkGraph, attempts []model.WorkNodeAttempt, windows []model.DecisionWindow, current model.WorkAttemptRef, verdict string) []model.WorkEdge {
	latest := map[model.WorkNodeID]map[model.WorkActivationID]model.WorkNodeAttempt{}
	for _, a := range attempts {
		if latest[a.Ref.NodeID] == nil {
			latest[a.Ref.NodeID] = map[model.WorkActivationID]model.WorkNodeAttempt{}
		}
		prior, ok := latest[a.Ref.NodeID][a.Ref.ActivationID]
		if !ok || a.Ref.Attempt > prior.Ref.Attempt {
			latest[a.Ref.NodeID][a.Ref.ActivationID] = a
		}
	}
	answers := map[model.WorkAttemptRef]string{}
	for _, w := range windows {
		if w.State == model.DecisionAnswered && w.SubmittedAnswer != nil {
			answers[w.Attempt] = *w.SubmittedAnswer
		}
	}
	if verdict != "" {
		answers[current] = verdict
	}
	allowed := map[model.WorkNodeID]map[model.WorkNodeID]bool{}
	for _, n := range graph.Nodes {
		if n.Kind != model.WorkNodeDecision || len(latest[n.ID]) == 0 {
			continue
		}
		selected := map[model.WorkNodeID]bool{}
		known := true
		for _, a := range latest[n.ID] {
			answer, ok := answers[a.Ref]
			if !ok || a.State != model.NodeAttemptSucceeded && a.State != model.NodeAttemptWaived {
				known = false
				break
			}
			for _, target := range outgoingNodesForVerdict(graph, n.ID, answer) {
				selected[target] = true
			}
		}
		if known {
			allowed[n.ID] = selected
		}
	}
	adjacency := map[model.WorkNodeID][]model.WorkEdge{}
	for _, e := range graph.Edges {
		if selected, known := allowed[e.From]; known && !selected[e.To] {
			continue
		}
		adjacency[e.From] = append(adjacency[e.From], e)
	}
	reachable := map[model.WorkNodeID]bool{graph.EntryNodeID: true}
	queue := []model.WorkNodeID{graph.EntryNodeID}
	var result []model.WorkEdge
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, e := range adjacency[id] {
			result = append(result, e)
			if !reachable[e.To] {
				reachable[e.To] = true
				queue = append(queue, e.To)
			}
		}
	}
	return result
}

func allJoinArrivals(edges []model.WorkEdge, attempts []model.WorkNodeAttempt, id model.WorkNodeID) bool {
	arrived := false
	for _, e := range edges {
		if e.To != id {
			continue
		}
		if !nodeConcludedSuccessfully(attempts, e.From) {
			return false
		}
		arrived = true
	}
	return arrived
}
