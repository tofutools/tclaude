package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

func TestMigrateV212toV213PreservesRuleEventAndFiringLedger(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, config.Save(&config.Config{Features: &config.FeaturesConfig{Triggers: true}}))
	ruleID, err := InsertTriggerRule(&TriggerRule{Name: "migrated-open", Enabled: true, OperatorAuthored: true,
		ScopeKind: TriggerScopeGlobal, Source: TriggerSourcePROpened, DraftFilter: TriggerDraftInclude,
		Actions: []TriggerAction{{Type: TriggerActionMessage, Message: &TriggerMessageAction{BodyTemplate: "review"}}}})
	require.NoError(t, err)
	agentID, _, err := EnsureAgentForConv("v213-ledger-author", "test")
	require.NoError(t, err)
	pr, err := UpsertAgentPRDetails(agentID, "https://github.com/o/r/pull/213", "migration", "open", "migrated-topic", false)
	require.NoError(t, err)
	events, err := ListPendingTriggerPREvents(10)
	require.NoError(t, err)
	require.Len(t, events, 1)
	firingID, inserted, err := InsertTriggerFiring(ruleID, 1, events[0].ID, events[0].EventRef, "ok", "", time.Now())
	require.NoError(t, err)
	require.True(t, inserted)
	require.NoError(t, FinishTriggerFiring(firingID, "ok", "", time.Now()))

	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`DROP TABLE trigger_pr_observations; DROP TABLE trigger_dwell_states; UPDATE schema_version SET version=212`)
	require.NoError(t, err)
	require.NoError(t, migrateV212toV213(d))
	// Restore today's additive dwell columns before exercising today's read
	// helpers. This test deliberately rolls only the trigger tables back to the
	// v212 shape; the unrelated cron-managed-worker schema is already v214.
	require.NoError(t, migrateV214toV215(d))
	require.NoError(t, migrateV215toV216(d))

	rule, err := GetTriggerRule(ruleID)
	require.NoError(t, err)
	require.NotNil(t, rule)
	assert.Equal(t, TriggerSourcePROpened, rule.Source)
	rows, err := ListTriggerFirings(ruleID, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, events[0].EventRef, rows[0].EventRef)
	assert.Equal(t, TriggerSourcePROpened, rows[0].Source)
	var branch string
	require.NoError(t, d.QueryRow(`SELECT branch_context FROM trigger_pr_observations WHERE agent_pr_id=?`, pr.ID).Scan(&branch))
	assert.Equal(t, "migrated-topic", branch)
	var violations int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations))
	assert.Zero(t, violations)
}
