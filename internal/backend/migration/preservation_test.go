package migration

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	sourcev228 "github.com/tofutools/tclaude/internal/backend/migration/source/v228"
)

func alterFixture(t *testing.T, bundle Bundle, query string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(bundle.Root, "snapshot.sqlite"))
	require.NoError(t, err)
	_, err = db.Exec(query)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	var m Manifest
	b, err := os.ReadFile(bundle.ManifestPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &m))
	m.Database = manifestFile(t, bundle.Root, "snapshot.sqlite")
	writeManifest(t, bundle.ManifestPath, m)
}
func inspectPlan(t *testing.T, b Bundle) MigrationPlan {
	t.Helper()
	i, e := Inspect(context.Background(), b)
	require.NoError(t, e)
	p, e := Plan(i)
	require.NoError(t, e)
	return p
}

func TestPreflightRejectsLostIntentAndBrokenReferences(t *testing.T) {
	for _, tc := range []struct{ name, sql, code string }{
		{"owner", `UPDATE trigger_rules SET owner_agent='missing'`, "broken_dependency_reference"},
		{"association", `INSERT INTO agents(agent_id,current_conv_id,initial_spawn_config) VALUES('agt_other','native-conv','{}')`, "current_association_owner_mismatch"},
		{"spawn intent", `UPDATE agents SET initial_spawn_config='{broken'`, "malformed_or_oversize_json"},
		{"lineage", `ALTER TABLE agent_lineage ADD COLUMN parent_agent_id TEXT; INSERT INTO agent_lineage VALUES('agt_fixture','missing')`, "broken_dependency_reference"},
		{"audience", `ALTER TABLE agent_messages ADD COLUMN cc_recipient_agents TEXT; UPDATE agent_messages SET cc_recipient_agents='["missing"]'`, "broken_dependency_reference"},
		{"parent", `ALTER TABLE agent_messages ADD COLUMN parent_id TEXT; UPDATE agent_messages SET parent_id='999'`, "broken_dependency_reference"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := buildFixture(t, fixtureOptions{})
			alterFixture(t, b, tc.sql)
			p := inspectPlan(t, b)
			require.False(t, p.PreflightValid)
			requireDiagnostic(t, p.Diagnostics, tc.code)
		})
	}
	p := inspectPlan(t, buildFixture(t, fixtureOptions{config: `{"agent":{"default_permissions":42}}`}))
	require.False(t, p.PreflightValid)
	requireDiagnostic(t, p.Diagnostics, "invalid_authored_config")
}

func TestPreflightQualifiedNativeIdentityAndCatalogOnlyHistory(t *testing.T) {
	b := buildFixture(t, fixtureOptions{})
	alterFixture(t, b, `UPDATE conversation_attempt_bindings SET external_ref='native-conv',harness='claude',namespace='claude-root'; UPDATE conversation_reference_bindings SET external_ref='native-conv',harness='claude',namespace='claude-root'; INSERT INTO conv_index(conv_id,harness) VALUES('historical-only','copilot')`)
	p := inspectPlan(t, b)
	require.True(t, p.PreflightValid, p.Diagnostics)
	require.NotEqual(t, findIdentity(t, p, "logical_conversations", "cvn_fixture").TargetID, findIdentity(t, p, "agent_conversations", "native-conv").TargetID)
	require.NotEmpty(t, findIdentity(t, p, "conv_index", "historical-only").TargetID)
	alterFixture(t, b, `UPDATE conversation_attempt_bindings SET harness='codex'`)
	p = inspectPlan(t, b)
	require.False(t, p.PreflightValid)
	requireDiagnostic(t, p.Diagnostics, "unproven_conversation_namespace")
}

func TestPreflightPreservesAuthoredDefaultAndUsage(t *testing.T) {
	b := buildFixture(t, fixtureOptions{})
	alterFixture(t, b, `ALTER TABLE dashboard_prefs ADD COLUMN value TEXT; INSERT INTO spawn_profiles(id,name,role_refs,environment_json) VALUES('1','safe','[]','[]'); INSERT INTO dashboard_prefs VALUES('tclaude.dash.default_profile','safe'),('tclaude.dash.default_profile_id','1'),('theme','dark'); INSERT INTO copilot_usage_snapshots VALUES('old-session'); INSERT INTO opencode_usage_activity VALUES('old-session','message')`)
	i, e := Inspect(context.Background(), b)
	require.NoError(t, e)
	require.Len(t, i.Snapshot.Rows["dashboard_prefs"], 2)
	require.Len(t, i.Snapshot.Rows["copilot_usage_snapshots"], 1)
	p, e := Plan(i)
	require.NoError(t, e)
	require.True(t, p.PreflightValid, p.Diagnostics)
	n := 0
	for _, r := range p.References {
		if r.SourceTable == "dashboard_prefs" {
			require.True(t, r.Resolved)
			n++
		}
	}
	require.Equal(t, 2, n)
	for _, d := range p.Dispositions {
		if d.Table == "copilot_usage_snapshots" || d.Table == "opencode_usage_activity" {
			require.Equal(t, Preserve, d.Classification)
		}
	}
}

func TestPreflightReadyIsUnresolvedAndTerminalRemainsHistory(t *testing.T) {
	b := buildFixture(t, fixtureOptions{})
	alterFixture(t, b, `UPDATE execution_operations SET state='ready'; UPDATE process_runs SET status='succeeded'; INSERT INTO execution_operations(id,state) VALUES('op_terminal','failed')`)
	p := inspectPlan(t, b)
	requireDiagnostic(t, p.Diagnostics, "operation_interrupted_unresolved")
	requireDiagnostic(t, p.Diagnostics, "terminal_operation_history")
	for _, d := range p.Diagnostics {
		if d.Code == "operation_interrupted_unresolved" {
			require.EqualValues(t, 1, d.Count)
		}
	}
}

func TestInvalidPreflightHashIsDeterministic(t *testing.T) {
	b := buildFixture(t, fixtureOptions{})
	alterFixture(t, b, `INSERT INTO spawn_profiles(id,name,environment_json,permission_overrides,role_refs) VALUES('1','a','{broken','{broken','[]'),('2','b','{broken','{}','[]')`)
	hash := ""
	for range 60 {
		p := inspectPlan(t, b)
		require.False(t, p.PreflightValid)
		if hash == "" {
			hash = p.PlanHash
		}
		require.Equal(t, hash, p.PlanHash)
	}
}

func TestJSONSpawnIntentEmptyLegacyDefault(t *testing.T) {
	s := sourcev228.Snapshot{Rows: map[string][]sourcev228.Row{"agents": {{Values: map[string]any{"initial_spawn_config": ""}}}}, Malformed: map[string]int64{}}
	sourcev228.ValidateJSON(&s)
	require.Empty(t, s.Malformed)
}

func TestPreflightMessageActorSentinelsAndAttribution(t *testing.T) {
	b := buildFixture(t, fixtureOptions{})
	alterFixture(t, b, `ALTER TABLE agent_messages ADD COLUMN cc_recipients TEXT; ALTER TABLE agent_messages ADD COLUMN cc_recipient_agents TEXT; INSERT INTO conv_index(conv_id,harness) VALUES('plain-conv','claude'); UPDATE agent_messages SET cc_recipients='["plain-conv"]',cc_recipient_agents='[""]'`)
	p := inspectPlan(t, b)
	require.True(t, p.PreflightValid, p.Diagnostics)
	alterFixture(t, b, `INSERT INTO agents(agent_id,current_conv_id,initial_spawn_config) VALUES('agt_other','other-conv','{}'); INSERT INTO agent_conversations(conv_id,agent_id) VALUES('other-conv','agt_other'); UPDATE agent_messages SET from_agent='agt_other'`)
	p = inspectPlan(t, b)
	require.False(t, p.PreflightValid)
	requireDiagnostic(t, p.Diagnostics, "conflicting_message_attribution")
	alterFixture(t, b, `UPDATE agent_messages SET from_agent='agt_fixture',cc_recipients='["native-conv"]',cc_recipient_agents='["agt_other"]'`)
	p = inspectPlan(t, b)
	require.False(t, p.PreflightValid)
	requireDiagnostic(t, p.Diagnostics, "conflicting_message_attribution")
}
