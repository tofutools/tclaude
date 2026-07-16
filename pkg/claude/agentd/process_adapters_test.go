package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/plan"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

func TestProcessSpawnParams_ExplicitFalseBlocksGlobalTrue(t *testing.T) {
	setupTestDB(t)
	off := false
	on := true
	explicit := &db.SpawnProfile{
		Name: "process-off", Harness: "codex", AutoReview: &off, TrustDir: &off,
	}
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "global-on", Harness: "codex", AutoReview: &on, TrustDir: &on,
	})
	require.NoError(t, err)
	require.NoError(t, db.SetDashboardPref(dashboardDefaultProfilePrefKey, "global-on"))

	p, err := processSpawnParams(explicit, processexec.Request{
		Command: plan.Command{ID: "cmd_0123456789abcdef01234567"},
		Input:   processexec.Input{RunID: "run", NodeID: "node"},
	})
	require.NoError(t, err)
	require.True(t, p.AutoReviewSet)
	require.True(t, p.TrustDirSet)
	require.Nil(t, applyDefaultProfile(nil, &p))
	assert.False(t, p.AutoReview)
	assert.False(t, p.TrustDir)
}

func TestProcessAgentSpawnParams_ResolvesProfileAlias(t *testing.T) {
	setupTestDB(t)
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "gpt5.6-sol-high", Aliases: []string{"codex-reviewer"}, Harness: "codex", Model: "gpt-5.6-sol",
	})
	require.NoError(t, err)
	p, err := processAgentSpawnParams(processexec.Request{
		Command:   plan.Command{ID: "cmd_0123456789abcdef01234567"},
		Input:     processexec.Input{RunID: "run", NodeID: "review"},
		Performer: model.Performer{Kind: model.PerformerAgent, Profile: "codex-reviewer", Prompt: "Review the diff"},
	})
	require.NoError(t, err)
	assert.Equal(t, "gpt-5.6-sol", p.Model)
}

func TestProcessAgentSpawnParams_RejectsDisabledProfile(t *testing.T) {
	setupTestDB(t)
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "paused", Disabled: true, DisabledReason: "provider maintenance", Harness: "codex",
	})
	require.NoError(t, err)
	_, err = processAgentSpawnParams(processexec.Request{
		Command:   plan.Command{ID: "cmd_0123456789abcdef01234567"},
		Input:     processexec.Input{RunID: "run", NodeID: "review"},
		Performer: model.Performer{Kind: model.PerformerAgent, Profile: "paused", Prompt: "Review the diff"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `spawn profile "paused" is disabled: provider maintenance`)
}

func TestProcessHumanAdapterBlockedContactIsActionable(t *testing.T) {
	setupTestDB(t)
	request := processexec.Request{
		Command: plan.Command{
			ID: "cmd_0123456789abcdef01234567", Kind: plan.CommandKindBlockNode,
			Reason: "tests exhausted their retry budget",
		},
		Input: processexec.Input{RunID: "release-42", NodeID: "implement.test.tests"},
		Performer: model.Performer{
			Kind: model.PerformerHuman, Assignee: "human:operator", Ask: "tests exhausted their retry budget",
		},
	}
	adapter := processHumanAdapter{}
	require.NoError(t, adapter.Contact(t.Context(), request, false))
	require.NoError(t, adapter.Contact(t.Context(), request, true))

	reminder, err := db.FindHumanMessageForProcessCommand(request.Command.ID, "Process blocked reminder")
	require.NoError(t, err)
	require.NotNil(t, reminder)
	assert.Equal(t, "Process blocked reminder", reminder.Subject)
	assert.Contains(t, reminder.Body, "Process release-42 node implement.test.tests is blocked: tests exhausted their retry budget")
	assert.Contains(t, reminder.Body, "tclaude process unblock release-42 implement.test.tests")
	assert.Contains(t, reminder.Body, "--decision <retry|skip|cancel>")

	escalation, err := db.FindHumanMessageForProcessCommand(request.Command.ID, "Process blocked escalation")
	require.NoError(t, err)
	require.NotNil(t, escalation)
	assert.Equal(t, "Process blocked escalation", escalation.Subject)
	assert.Contains(t, escalation.Body, "tests exhausted their retry budget")
	assert.Contains(t, escalation.Body, "tclaude process unblock release-42 implement.test.tests")
	assert.Contains(t, escalation.Body, "--decision <retry|skip|cancel>")
	assert.Contains(t, escalation.Body, "Escalation target: human:operator.")

	performerRequest := request
	performerRequest.Command = plan.Command{ID: "cmd_1123456789abcdef01234567", Kind: plan.CommandKindStartAttempt}
	require.NoError(t, adapter.Contact(t.Context(), performerRequest, false))
	performerReminder, err := db.FindHumanMessageForProcessCommand(performerRequest.Command.ID, "Process reminder")
	require.NoError(t, err)
	require.NotNil(t, performerReminder)
	assert.Equal(t, "Waiting on process release-42 node implement.test.tests (command cmd_1123456789abcdef01234567).", performerReminder.Body)
}

func TestProcessHumanAdapterCrashBeforeDispatchRedispatchesDurableClaim(t *testing.T) {
	setupTestDB(t)
	fs, err := store.NewFS(t.TempDir())
	require.NoError(t, err)
	performer := &model.Performer{Kind: model.PerformerHuman, Ask: "Approve the release?"}
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "human-recovery", Start: "work",
		Nodes: map[string]model.Node{
			"work":   {Type: model.NodeTypeTask, Performer: performer, Next: model.Next{"pass": "done", "fail": "failed"}},
			"done":   {Type: model.NodeTypeEnd, Result: "completed"},
			"failed": {Type: model.NodeTypeEnd, Result: "failed"},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	require.NoError(t, err)
	const runID = "human-crash-before-dispatch"
	checkpoint := state.New(runID, record.Ref, record.Ref, []state.NodeInit{
		{ID: "work", Type: model.NodeTypeTask, Status: state.NodeStatusReady},
		{ID: "done", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
		{ID: "failed", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	checkpoint.Status = state.RunStatusRunning
	_, err = fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: record.Ref}, checkpoint)
	require.NoError(t, err)
	proof, err := fs.UpgradeNeeded(t.Context(), runID)
	require.NoError(t, err)
	_, err = fs.InitializePathV1(t.Context(), runID, proof)
	require.NoError(t, err)

	var claim *pathv1.ExecutionTransition
	require.NoError(t, fs.WithPathV1ExecutionView(t.Context(), runID, func(view store.PathV1ExecutionView) error {
		aggregate, aggregateErr := pathv1.CurrentAggregateCheckpoint(view.Checkpoint)
		if aggregateErr != nil {
			return aggregateErr
		}
		attempt, planErr := pathv1.PlanExclusiveAttempt(t.Context(), view.Input, aggregate.Authority.Genesis.OutputPathID, 1, view.Run.Params)
		if planErr != nil {
			return planErr
		}
		claim, planErr = pathv1.ClaimExclusiveAttempt(t.Context(), view.Input, attempt)
		return planErr
	}))
	_, err = fs.AppendPathV1(t.Context(), runID, claim)
	require.NoError(t, err, "the durable claim models a crash before adapter dispatch")

	executor := processexec.NewExclusiveV7(fs, map[model.PerformerKind]processexec.Adapter{
		model.PerformerHuman: processHumanAdapter{},
	})
	_, err = executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	messages, err := db.ListHumanMessages()
	require.NoError(t, err)
	require.Len(t, messages, 1, "missing production obligation must be redispatched")
	assert.Equal(t, "Process obligation", messages[0].Subject)
	assert.Equal(t, runID, messages[0].ProcessRunID)

	_, err = executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	messages, err = db.ListHumanMessages()
	require.NoError(t, err)
	assert.Len(t, messages, 1, "discoverable obligation must remain in flight without duplication")
}
