package transport

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestProcessDurationCompanionRetainsNestedExactValuesWithoutChangingModel(t *testing.T) {
	const exact time.Duration = 9007199254740993
	p := &model.ProcessDefinition{Graph: model.WorkGraph{Nodes: []model.WorkNode{{
		Retry: model.RetryPolicy{Backoff: exact, AttemptBudget: 15}, Wait: &model.WaitPolicy{Duration: exact}, Decision: &model.DecisionNode{ExpiresAfter: exact},
		Stages: &model.TaskStages{Plan: &model.TaskStage{Retry: model.RetryPolicy{Backoff: exact}}, Review: &model.TaskStage{Retry: model.RetryPolicy{AttemptBudget: exact}}, Checks: []model.TaskStage{{Retry: model.RetryPolicy{Backoff: exact}}}, PlanApproval: &model.DecisionNode{ExpiresAfter: exact}},
	}}}}
	before, err := json.Marshal(p)
	require.NoError(t, err)
	got := processDurationProjection(p)
	require.Equal(t, map[string]string{"0.Retry.Backoff": "9007199254740993", "0.Wait.Duration": "9007199254740993", "0.Decision.ExpiresAfter": "9007199254740993", "0.Stages.Plan.Retry.Backoff": "9007199254740993", "0.Stages.Review.Retry.AttemptBudget": "9007199254740993", "0.Stages.Checks.0.Retry.Backoff": "9007199254740993", "0.Stages.PlanApproval.ExpiresAfter": "9007199254740993"}, got)
	after, err := json.Marshal(p)
	require.NoError(t, err)
	require.Equal(t, before, after)
}
