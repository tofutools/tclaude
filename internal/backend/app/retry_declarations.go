package app

import "github.com/tofutools/tclaude/internal/backend/model"

func executableRetryDeclarations(graph model.WorkGraph) error {
	for _, node := range graph.Nodes {
		if node.Retry.OnFail == "feedback-same-session" {
			return fail(ErrUnsupported, "node %s requests feedback-same-session retry; retained-session retry execution is unavailable", node.ID)
		}
		if node.Stages == nil {
			continue
		}
		if node.Stages.Plan != nil && node.Stages.Plan.Retry.OnFail == "feedback-same-session" {
			return fail(ErrUnsupported, "task %s plan requests feedback-same-session retry; retained-session retry execution is unavailable", node.ID)
		}
		stages := append([]model.TaskStage(nil), node.Stages.Checks...)
		if node.Stages.Review != nil {
			stages = append(stages, *node.Stages.Review)
		}
		for _, stage := range stages {
			if retryDeclared(stage.Retry) {
				return fail(ErrUnsupported, "task %s stage %s declares independent retries; independent check/review retry execution is unavailable", node.ID, stage.ID)
			}
		}
	}
	return nil
}

func retryDeclared(retry model.RetryPolicy) bool {
	return retry.MaxAttempts != 0 || retry.Backoff != 0 || len(retry.Retryable) != 0 || retry.OnFail != ""
}
