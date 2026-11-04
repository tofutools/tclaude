package agentd_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A worker refused by a disabled or operator-only spawn profile, its own or a
// default tier's, is a stopped automation: the refusal is recorded as a failed
// run naming the profile and why, and the first one is announced to the
// automation's owner and to the operator (tcl-a2e791 slice 4).

const refusalAuthor = "refusal-author-conv"

// refusalTrigger stores a PR-opened spawn trigger on group alpha, owned by
// ownerAgent ("" for an operator-authored rule).
func refusalTrigger(t *testing.T, f *testharness.Flow, ownerAgent string) int64 {
	t.Helper()
	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	f.HaveConvWithTitle(refusalAuthor, "author")
	f.HaveMember("alpha", refusalAuthor)
	ruleID, err := db.InsertTriggerRule(&db.TriggerRule{Name: "reviews", Enabled: true,
		OwnerAgent: ownerAgent, OperatorAuthored: ownerAgent == "",
		ScopeKind: db.TriggerScopeGroup, GroupID: g.ID, Source: db.TriggerSourcePROpened,
		DraftFilter: db.TriggerDraftExclude, Actions: []db.TriggerAction{{Type: db.TriggerActionSpawn,
			Spawn: &db.TriggerSpawnAction{Profile: "reviewer", NameTemplate: "review-{{pr.number}}",
				InstructionTemplate: "Review {{pr.url}}.", MaxLiveWorkers: 5}}}})
	require.NoError(t, err)
	return ruleID
}

// openPR opens PR n by the trigger's author and runs one trigger tick,
// returning the newest firing's single action outcome.
func openPR(t *testing.T, ruleID int64, n int) db.TriggerActionOutcome {
	t.Helper()
	authorAgent, err := db.AgentIDForConv(refusalAuthor)
	require.NoError(t, err)
	_, err = db.UpsertAgentPRDetails(authorAgent, fmt.Sprintf("https://github.com/o/r/pull/%d", n), "ready", "open", "topic", false)
	require.NoError(t, err)
	agentd.RunTriggerTickForTest(time.Now().Add(time.Duration(n) * time.Second))
	agentd.WaitForBackgroundForTest()
	rows, err := db.ListTriggerFirings(ruleID, 10)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	require.Len(t, rows[0].Actions, 1)
	return rows[0].Actions[0]
}

func operatorNotices(t *testing.T) []*db.HumanMessage {
	t.Helper()
	msgs, err := db.ListHumanMessagesPage(db.HumanMessageFilter{}, 0, 50)
	require.NoError(t, err)
	return msgs
}

func TestManagedProfileRefusal_Trigger_DisabledGroupDefaultFailsLoudlyOnce(t *testing.T) {
	f := uniformTriggerGroup(t)
	setGroupDefault(t, "alpha", &db.SpawnProfile{Name: "group-default", Harness: "claude",
		Disabled: true, DisabledReason: "retired"})
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude"})
	require.NoError(t, err)
	ruleID := refusalTrigger(t, f, "")

	first := openPR(t, ruleID, 9)
	assert.Equal(t, "profile_disabled", first.Outcome)
	assert.Contains(t, first.Detail, `spawn profile "group-default" is disabled: retired`)
	assert.Contains(t, first.Detail, "the default spawn profile of group alpha")
	rows, err := db.ListTriggerFirings(ruleID, 10)
	require.NoError(t, err)
	assert.Equal(t, "partial_failure", rows[0].Outcome, "the firing is recorded as failed")

	notices := operatorNotices(t)
	require.Len(t, notices, 1, "the operator hears about the first refusal")
	assert.Contains(t, notices[0].Subject, `trigger "reviews" stopped: a spawn profile is disabled`)
	assert.Contains(t, notices[0].Body, `"group-default"`)

	second := openPR(t, ruleID, 10)
	assert.Equal(t, "profile_disabled", second.Outcome, "every firing is refused the same way")
	assert.Len(t, operatorNotices(t), 1, "a repeated refusal is recorded, not announced again")
}

func TestManagedProfileRefusal_Trigger_OperatorOnlyNotifiesOwnerAndOperator(t *testing.T) {
	f := uniformTriggerGroup(t)
	setGroupDefault(t, "alpha", &db.SpawnProfile{Name: "group-default", Harness: "claude", OperatorOnly: true})
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude"})
	require.NoError(t, err)
	const owner = "refusal-owner-conv"
	f.HaveConvWithTitle(owner, "owner")
	f.HaveMember("alpha", owner)
	ownerAgent, err := db.AgentIDForConv(owner)
	require.NoError(t, err)
	require.NoError(t, db.GrantAgentPermission(owner, agentd.PermGroupsTriggersManage, "test"))
	ruleID := refusalTrigger(t, f, ownerAgent)

	outcome := openPR(t, ruleID, 9)
	assert.Equal(t, "profile_operator_only", outcome.Outcome)
	assert.Contains(t, outcome.Detail, `spawn profile "group-default" is restricted to human/operator spawns`)

	inbox, err := db.ListAgentMessagesForConv(owner, 20)
	require.NoError(t, err)
	var subjects []string
	for _, m := range inbox {
		subjects = append(subjects, m.Subject)
	}
	assert.Contains(t, subjects, `trigger "reviews" stopped: a spawn profile is operator-only`, "the owner hears about it")
	require.Len(t, operatorNotices(t), 1, "and so does the operator")
}

// A spawn from the same automation re-arms the notice: the next refusal is
// a new stop and is announced again.
func TestManagedProfileRefusal_Trigger_SuccessRearmsTheNotice(t *testing.T) {
	f := uniformTriggerGroup(t)
	worker := &db.SpawnProfile{Name: "reviewer", Harness: "claude", Disabled: true}
	_, err := db.CreateSpawnProfile(worker)
	require.NoError(t, err)
	ruleID := refusalTrigger(t, f, "")

	assert.Equal(t, "profile_disabled", openPR(t, ruleID, 9).Outcome, "the worker's own profile refuses too")
	require.Len(t, operatorNotices(t), 1)

	stored, err := db.GetSpawnProfile("reviewer")
	require.NoError(t, err)
	stored.Disabled = false
	require.NoError(t, db.UpdateSpawnProfile(stored))
	assert.Equal(t, "spawned", openPR(t, ruleID, 10).Outcome)

	stored.Disabled = true
	require.NoError(t, db.UpdateSpawnProfile(stored))
	assert.Equal(t, "profile_disabled", openPR(t, ruleID, 11).Outcome)
	assert.Len(t, operatorNotices(t), 2, "a refusal after a spawn is announced again")
}

// A process node whose performer spawn a profile refuses fails with the
// reason instead of waiting for a member that will never come.
func TestManagedProfileRefusal_Process_DisabledGroupDefaultFailsTheNode(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	_, err := db.SetAgentGroupDefaultCwd("crew", t.TempDir())
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "dev", Harness: "claude"})
	require.NoError(t, err)
	setGroupDefault(t, "crew", &db.SpawnProfile{Name: "group-default", Harness: "claude",
		Disabled: true, DisabledReason: "paused"})
	performer := devPerformer()
	performer.Spawn = model.SpawnAllowed
	view := startPerformerRun(t, f, root, "run_refused", performerTemplate("refused", performer, nil),
		agentd.ProcessRunScopeForTest{Kind: "group", GroupID: g.ID})
	agentd.WaitForBackgroundForTest()

	assert.Equal(t, engine.RunFailed, view.Status, "%+v", view)
	require.Len(t, view.Performers, 1)
	assert.Equal(t, "fallback_failed", view.Performers[0].Resolution)
	assert.Contains(t, view.Performers[0].Note, `spawn profile "group-default" is disabled: paused`)

	notices := operatorNotices(t)
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0].Subject, "process run run_refused")
	assert.Equal(t, "run_refused", notices[0].ProcessRunID, "the notice links to its run")
}
