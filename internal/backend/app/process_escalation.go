package app

import (
	"slices"

	"github.com/tofutools/tclaude/internal/backend/model"
)

func compoundTask(node model.WorkNode) bool {
	s := node.Stages
	return node.Kind == model.WorkNodeTask && s != nil && (s.Plan != nil || s.PlanApproval != nil || len(s.Checks) != 0 || s.Review != nil)
}

func escalationFailure(verdict string) bool {
	return slices.Contains([]string{"fail", "failed", "failure", "error"}, verdict)
}

// Authored escalation loops remain declarations, not ordinary executable cycles.
func processEscalationRetries(graph model.WorkGraph) ([]model.WorkEdge, error) {
	nodes := make(map[model.WorkNodeID]model.WorkNode, len(graph.Nodes))
	for _, node := range graph.Nodes {
		nodes[node.ID] = node
	}
	decisions := map[model.WorkNodeID]model.WorkNodeID{}
	sources := map[model.WorkNodeID]model.WorkNodeID{}
	for _, edge := range graph.Edges {
		if !compoundTask(nodes[edge.From]) || !escalationFailure(edge.Verdict) || nodes[edge.To].Kind != model.WorkNodeDecision {
			continue
		}
		if prior, ok := decisions[edge.To]; ok && prior != edge.From {
			return nil, fail(ErrInvalid, "escalation decision must belong to one compound task")
		}
		if prior, ok := sources[edge.From]; ok && prior != edge.To {
			return nil, fail(ErrInvalid, "compound task must have one escalation decision")
		}
		decisions[edge.To], sources[edge.From] = edge.From, edge.To
	}
	var retries []model.WorkEdge
	for _, node := range graph.Nodes {
		source, escalation := decisions[node.ID]
		if !escalation {
			continue
		}
		if node.ID == graph.EntryNodeID || node.Decision == nil || node.Decision.Decider != nil || !validEscalationAudience(node.Decision.Audience) || len(node.Decision.PermittedAnswers) != 2 || !slices.Contains(node.Decision.PermittedAnswers, "retry") || !slices.Contains(node.Decision.PermittedAnswers, "cancel") {
			return nil, fail(ErrInvalid, "escalation requires a non-entry decision with exactly retry and cancel answers")
		}
		var retry, cancel *model.WorkEdge
		count := 0
		for _, edge := range graph.Edges {
			if edge.To == node.ID && (edge.From != source || !escalationFailure(edge.Verdict)) {
				return nil, fail(ErrInvalid, "escalation decision may only receive its compound task failure")
			}
			if edge.From != node.ID {
				continue
			}
			count++
			switch edge.Verdict {
			case "retry":
				value := edge
				retry = &value
			case "cancel":
				value := edge
				cancel = &value
			}
		}
		if count != 2 || retry == nil || cancel == nil || retry.To != source {
			return nil, fail(ErrInvalid, "escalation retry must return to its compound task and have one cancel route")
		}
		end := nodes[cancel.To]
		if end.Kind != model.WorkNodeEnd || end.End == nil || end.End.Outcome != model.WorkOutcomeCancelled {
			return nil, fail(ErrInvalid, "escalation cancel must target a cancelled end")
		}
		retries = append(retries, *retry)
	}
	return retries, nil
}

// An authored loop needs an audience identity, not an empty placeholder.
// Current grants and role eligibility remain execution-time checks.
func validEscalationAudience(audience []model.DecisionAudience) bool {
	if len(audience) == 0 {
		return false
	}
	for _, entry := range audience {
		subject := entry.Subject
		if entry.RoleID != "" {
			if entry.RoleID.Validate() != nil || (entry.GroupID != "" && entry.GroupID.Validate() != nil) || subject != (model.AuthoritySubject{}) {
				return false
			}
			continue
		}
		if entry.GroupID != "" {
			return false
		}
		switch subject.Kind {
		case model.AuthorityOperator:
			if subject.AgentID != "" || subject.ExecutionID != "" {
				return false
			}
		case model.AuthorityAgent:
			if subject.AgentID.Validate() != nil || subject.ExecutionID != "" {
				return false
			}
		case model.AuthorityExecution:
			if subject.ExecutionID.Validate() != nil || subject.AgentID != "" {
				return false
			}
		default:
			return false
		}
	}
	return true
}
