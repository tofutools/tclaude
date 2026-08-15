package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV211toV212SeedsExistingPRsWithoutReplay(t *testing.T) {
	setupTestDB(t)
	agent, _, err := EnsureAgentForConv("conv-existing-pr", "test")
	require.NoError(t, err)
	row, err := UpsertAgentPR(agent, "https://github.com/o/r/pull/1", "old", "open")
	require.NoError(t, err)
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`DELETE FROM trigger_pr_events`)
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE schema_version SET version=211`)
	require.NoError(t, err)
	require.NoError(t, migrateV211toV212(d))
	var status string
	require.NoError(t, d.QueryRow(`SELECT status FROM trigger_pr_events WHERE agent_pr_id=?`, row.ID).Scan(&status))
	assert.Equal(t, TriggerEventPreexisting, status)
}

func TestDeleteTriggerRulePreservesFiringAndWorkerHistory(t *testing.T) {
	setupTestDB(t)
	ruleID, err := InsertTriggerRule(&TriggerRule{Name: "history", Enabled: true, OperatorAuthored: true,
		ScopeKind: TriggerScopeGlobal, Source: TriggerSourcePROpened, DraftFilter: TriggerDraftInclude,
		Actions: []TriggerAction{{Type: TriggerActionSpawn, Spawn: &TriggerSpawnAction{Profile: "reviewer", InstructionTemplate: "review", MaxLiveWorkers: 1}}}})
	require.NoError(t, err)
	agent, _, err := EnsureAgentForConv("conv-history", "test")
	require.NoError(t, err)
	_, err = UpsertAgentPR(agent, "https://github.com/o/r/pull/7", "ready", "open")
	require.NoError(t, err)
	events, err := ListPendingTriggerPREvents(10)
	require.NoError(t, err)
	require.Len(t, events, 1)
	firingID, inserted, err := InsertTriggerFiring(ruleID, 1, events[0].ID, events[0].EventRef, "ok", "", time.Now())
	require.NoError(t, err)
	require.True(t, inserted)
	require.NoError(t, FinishTriggerFiring(firingID, "ok", "", time.Now()))
	_, err = InsertTriggerWorker(&TriggerWorker{RuleID: ruleID, FiringID: firingID, ActionIndex: 0, AgentID: NewAgentID(), State: "live", CreatedAt: time.Now()})
	require.NoError(t, err)
	require.NoError(t, DeleteTriggerRule(ruleID, 1))
	d, err := Open()
	require.NoError(t, err)
	var firings, workers, orphanedRules int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM trigger_firings`).Scan(&firings))
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM trigger_workers`).Scan(&workers))
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM trigger_workers WHERE rule_id IS NULL`).Scan(&orphanedRules))
	assert.Equal(t, 1, firings)
	assert.Equal(t, 1, workers)
	assert.Equal(t, 1, orphanedRules)
}

func TestUpsertAgentPRQueuesOneDurableOpenEdge(t *testing.T) {
	setupTestDB(t)
	agent, _, err := EnsureAgentForConv("conv-new-pr", "test")
	require.NoError(t, err)
	_, err = UpsertAgentPRDetails(agent, "https://github.com/o/r/pull/42", "new", "open", "topic", false)
	require.NoError(t, err)
	_, err = UpsertAgentPRDetails(agent, "https://github.com/o/r/pull/42", "edited", "open", "topic-2", false)
	require.NoError(t, err)
	events, err := ListPendingTriggerPREvents(10)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, 42, events[0].PRNumber)
	assert.Equal(t, "topic-2", events[0].PRBranch)
}
