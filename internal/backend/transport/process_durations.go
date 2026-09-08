package transport

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
)

// The companion preserves exact browser authoring values without changing the
// existing numeric API fields, immutable definition bytes, or their hashes.
func processDurationProjection(process *model.ProcessDefinition) map[string]string {
	if process == nil {
		return nil
	}
	return nodeDurationProjection(process.Graph.Nodes)
}
func nodeDurationProjection(nodes []model.WorkNode) map[string]string {
	result := map[string]string{}
	put := func(path string, value time.Duration) {
		if value > 9007199254740991 || value < -9007199254740991 {
			result[path] = strconv.FormatInt(int64(value), 10)
		}
	}
	retry := func(path string, r model.RetryPolicy) {
		put(path+".Backoff", r.Backoff)
		put(path+".AttemptBudget", r.AttemptBudget)
	}
	decision := func(path string, d *model.DecisionNode) {
		if d != nil {
			put(path+".ExpiresAfter", d.ExpiresAfter)
		}
	}
	for i, n := range nodes {
		path := strconv.Itoa(i)
		retry(path+".Retry", n.Retry)
		decision(path+".Decision", n.Decision)
		if n.Wait != nil {
			put(path+".Wait.Duration", n.Wait.Duration)
		}
		if n.Stages != nil {
			if n.Stages.Plan != nil {
				retry(path+".Stages.Plan.Retry", n.Stages.Plan.Retry)
			}
			if n.Stages.Review != nil {
				retry(path+".Stages.Review.Retry", n.Stages.Review.Retry)
			}
			decision(path+".Stages.PlanApproval", n.Stages.PlanApproval)
			for j, c := range n.Stages.Checks {
				retry(path+".Stages.Checks."+strconv.Itoa(j)+".Retry", c.Retry)
			}
		}
	}
	return result
}

type processSnippetView struct {
	model.ProcessSnippet
	ProcessDurationNS map[string]string
}

func projectProcessSnippet(snippet model.ProcessSnippet) processSnippetView {
	var selection model.ProcessSelection
	view := processSnippetView{ProcessSnippet: snippet}
	if snippet.Available && json.Unmarshal(snippet.Selection, &selection) == nil {
		view.ProcessDurationNS = nodeDurationProjection(selection.Nodes)
	}
	return view
}
