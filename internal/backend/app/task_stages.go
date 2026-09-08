package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"

	"github.com/tofutools/tclaude/internal/backend/model"
)

// compileTaskStages accepts only authored graphs. Generated identities are stable
// across validation, admission and exact retries of a pinned definition.
func compileTaskStages(authored model.WorkGraph) (model.WorkGraph, error) {
	if len(authored.TaskGroups) != 0 || len(authored.EscalationRetries) != 0 {
		return model.WorkGraph{}, fail(ErrInvalid, "task groups are compiler-owned")
	}
	for _, node := range authored.Nodes {
		if node.Decision != nil && node.Decision.QuestionResolved || node.Stages != nil && node.Stages.PlanApproval != nil && node.Stages.PlanApproval.QuestionResolved {
			return model.WorkGraph{}, fail(ErrInvalid, "resolved decision questions are compiler-owned")
		}
	}
	if err := validateWorkGraph(authored); err != nil {
		return model.WorkGraph{}, err
	}
	retries, err := processEscalationRetries(authored)
	if err != nil {
		return model.WorkGraph{}, err
	}
	graph := authored
	graph.EscalationRetries = retries
	graph.Nodes = nil
	graph.Edges = slices.Clone(authored.Edges)
	entries := map[model.WorkNodeID]model.WorkNodeID{}
	for _, node := range authored.Nodes {
		if node.Kind == model.WorkNodeTaskComplete {
			return model.WorkGraph{}, fail(ErrInvalid, "task completion nodes are compiler-owned")
		}
		if node.Stages == nil {
			graph.Nodes = append(graph.Nodes, node)
			continue
		}
		if node.Kind != model.WorkNodeTask {
			return model.WorkGraph{}, fail(ErrInvalid, "only tasks can own stages")
		}
		if err := validateWorkNode(node); err != nil {
			return model.WorkGraph{}, err
		}
		if err := validateRetryPolicy(node); err != nil {
			return model.WorkGraph{}, err
		}
		stages := node.Stages
		if stages.Plan == nil && stages.PlanApproval == nil && len(stages.Checks) == 0 && stages.Review == nil {
			node.Stages = nil
			graph.Nodes = append(graph.Nodes, node)
			continue
		}
		if len(stages.Checks) > 64 || stages.Plan == nil && stages.PlanApproval != nil {
			return model.WorkGraph{}, fail(ErrInvalid, "bounded checks and a plan before approval are required")
		}
		group := model.CompiledTaskGroup{ID: node.ID}
		chain := []model.WorkNodeID{}
		seen := map[string]bool{}
		addStage := func(stage model.TaskStage, role string) (model.WorkNodeID, error) {
			if stage.ID == "" || model.ValidateStableID("stage", stage.ID) != nil || seen[stage.ID] {
				return "", fail(ErrInvalid, "stage IDs must be stable and unique within a task")
			}
			seen[stage.ID] = true
			if role != "plan" && (stage.Retry.MaxAttempts != 0 || stage.Retry.Backoff != 0 || len(stage.Retry.Retryable) != 0) {
				return "", fail(ErrInvalid, "checks and review share the task work retry budget")
			}
			id := taskStageID(node.ID, role, stage.ID)
			performer := stage.Performer
			graph.Nodes = append(graph.Nodes, model.WorkNode{ID: id, Kind: model.WorkNodeTask, Name: stage.Name, Description: stage.Description, Doc: stage.Doc, Performer: &performer, Retry: stage.Retry})
			chain = append(chain, id)
			return id, nil
		}
		var err error
		if stages.Plan != nil {
			group.Plan, err = addStage(*stages.Plan, "plan")
			if err != nil {
				return model.WorkGraph{}, err
			}
		}
		if stages.PlanApproval != nil {
			approval := *stages.PlanApproval
			if len(approval.PermittedAnswers) != 2 || !slices.Contains(approval.PermittedAnswers, "approve") || !slices.Contains(approval.PermittedAnswers, "rework") {
				return model.WorkGraph{}, fail(ErrInvalid, "plan approval answers must be approve and rework")
			}
			group.Approval = taskStageID(node.ID, "approval", "")
			graph.Nodes = append(graph.Nodes, model.WorkNode{ID: group.Approval, Kind: model.WorkNodeDecision, Name: node.Name + ": approve plan", Decision: &approval})
			chain = append(chain, group.Approval)
		}
		work := node
		work.ID, work.Stages = taskStageID(node.ID, "work", ""), nil
		group.Work = work.ID
		graph.Nodes = append(graph.Nodes, work)
		chain = append(chain, work.ID)
		for _, check := range stages.Checks {
			id, stageErr := addStage(check, "check")
			if stageErr != nil {
				return model.WorkGraph{}, stageErr
			}
			group.Checks = append(group.Checks, id)
		}
		if stages.Review != nil {
			group.Review, err = addStage(*stages.Review, "review")
			if err != nil {
				return model.WorkGraph{}, err
			}
		}
		graph.Nodes = append(graph.Nodes, model.WorkNode{ID: node.ID, Kind: model.WorkNodeTaskComplete, Name: node.Name, Description: node.Description, Doc: node.Doc})
		chain = append(chain, node.ID)
		group.Entry = chain[0]
		entries[node.ID] = group.Entry
		graph.TaskGroups = append(graph.TaskGroups, group)
		for i := 1; i < len(chain); i++ {
			verdict := ""
			if chain[i-1] == group.Approval {
				verdict = "approve"
			}
			graph.Edges = append(graph.Edges, model.WorkEdge{From: chain[i-1], To: chain[i], Verdict: verdict})
		}
	}
	// Only authored incoming edges move to the first stage. Generated final edges
	// still terminate at the authored task identity, which represents completion.
	for i := range authored.Edges {
		if entry := entries[graph.Edges[i].To]; entry != "" {
			graph.Edges[i].To = entry
		}
	}
	kept := graph.Edges[:0]
	for i, edge := range graph.Edges {
		if i < len(authored.Edges) && slices.Contains(retries, authored.Edges[i]) {
			continue
		}
		kept = append(kept, edge)
	}
	graph.Edges = kept
	if entry := entries[graph.EntryNodeID]; entry != "" {
		graph.EntryNodeID = entry
	}
	if err := validateWorkGraph(graph); err != nil {
		return model.WorkGraph{}, err
	}
	return graph, nil
}

func taskStageID(parent model.WorkNodeID, role, key string) model.WorkNodeID {
	data, _ := json.Marshal([]string{string(parent), role, key})
	hash := sha256.Sum256(data)
	return model.WorkNodeID("stage_" + hex.EncodeToString(hash[:16]))
}

func taskGroup(graph model.WorkGraph, id model.WorkNodeID) (model.CompiledTaskGroup, bool) {
	for _, group := range graph.TaskGroups {
		if id == group.ID || id == group.Plan || id == group.Approval || id == group.Work || id == group.Review || slices.Contains(group.Checks, id) {
			return group, true
		}
	}
	return model.CompiledTaskGroup{}, false
}
