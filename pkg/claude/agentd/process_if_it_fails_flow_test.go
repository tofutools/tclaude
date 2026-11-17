package agentd_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// TestProcessRuntimeIfItFailsRunsAsTheInlineEditorSays runs a command step
// that always fails under each choice of the inline editor's "If it fails"
// row (process-step-list.js setStepFailure), with the retry policy that
// choice saves:
//
//   - fail the run (no policy): one attempt, the run fails.
//   - hold it for me (maxAttempts 1): one attempt, the step is held (blocked)
//     for the operator and the run stays open.
//   - retry 2×, then hold it for me (maxAttempts 3): three attempts, then held.
//
// A held step is the operator's to retry, skip or cancel
// (process_blocked_flow_test.go covers those).
func TestProcessRuntimeIfItFailsRunsAsTheInlineEditorSays(t *testing.T) {
	for _, choice := range []struct {
		name     string
		retry    *model.RetryPolicy
		attempts int
		held     bool
	}{
		{name: "fail the run", retry: nil, attempts: 1},
		{name: "hold it for me", retry: &model.RetryPolicy{MaxAttempts: 1}, attempts: 1, held: true},
		{name: "retry 2x then hold it for me", retry: &model.RetryPolicy{MaxAttempts: 3}, attempts: 3, held: true},
	} {
		t.Run(choice.name, func(t *testing.T) {
			f, root := processRuntimeFlow(t)
			tmpl := processRuntimeTemplate("if-it-fails", 1)
			task := tmpl.Nodes["task-01"]
			task.Retry = choice.retry
			tmpl.Nodes["task-01"] = task
			putProcessRuntimeTemplate(t, root, tmpl)

			log := &processAttemptLog{}
			t.Cleanup(agentd.SetProcessProgramPerformForTest(func(ctx context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
				command := dispatch.Command()
				log.record(ctx, command)
				return processObservation(command, engine.ProgramFailed, 1), nil
			}))

			created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
				"templateId": "if-it-fails", "authorizeProgramProfiles": []string{"safe"},
			})
			require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
			var run processRuntimeRunView
			testharness.DecodeJSON(t, created, &run)
			agentd.WaitForProcessRunRuntimeForTest()

			dispatched, _ := log.snapshot()
			require.Len(t, dispatched, choice.attempts, "attempts, the first included")

			show := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+run.ID, nil)
			require.Equal(t, http.StatusOK, show.Code, show.Body.String())
			var shown processRuntimeRunView
			testharness.DecodeJSON(t, show, &shown)
			if !choice.held {
				assert.Equal(t, engine.RunFailed, shown.Checkpoint.Status, "the run fails")
				assert.Equal(t, engine.NodeFailed, shown.Checkpoint.Nodes["task-01"])
				assert.Empty(t, shown.Blocked)
				return
			}
			assert.Equal(t, engine.RunRunning, shown.Checkpoint.Status, "the run waits")
			assert.Equal(t, engine.NodeBlocked, shown.Checkpoint.Nodes["task-01"], "the step is held")
			assert.Equal(t, "blocked", shown.Action)
			require.Len(t, shown.Blocked, 1)
			assert.Equal(t, choice.attempts, shown.Blocked[0].Attempt)
		})
	}
}
