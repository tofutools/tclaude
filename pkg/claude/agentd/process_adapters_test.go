package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/plan"
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
