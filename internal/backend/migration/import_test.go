package migration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestImportSnapshotSemanticRoundTripAndExactRetry(t *testing.T) {
	bundle := buildFixture(t, fixtureOptions{attachment: true})
	alterFixture(t, bundle, `
		ALTER TABLE spawn_profiles ADD COLUMN harness TEXT;
		ALTER TABLE spawn_profiles ADD COLUMN model TEXT;
		ALTER TABLE spawn_profiles ADD COLUMN approval TEXT;
		ALTER TABLE spawn_profiles ADD COLUMN sandbox TEXT;
		INSERT INTO spawn_profiles(id,name,permission_overrides,environment_json,role_refs,harness,model,approval,sandbox)
		VALUES('7','safe','[]','[]','[]','codex','gpt','supervised','workspace_write');
		ALTER TABLE dashboard_prefs ADD COLUMN value TEXT;
		INSERT INTO dashboard_prefs(key,value) VALUES('tclaude.dash.default_profile','safe');
		INSERT INTO group_templates(id,name,process,rhythms,owner_scopes_json) VALUES('4','imported-team','','','[]');
		ALTER TABLE session_cost_daily ADD COLUMN conv_id TEXT;
		INSERT INTO session_cost_daily(session_id,day,cost_usd,agent_id,harness,conv_id) VALUES('old-session','2026-08-31','1.2500','agt_fixture','codex','native-conv');
		ALTER TABLE audit_log ADD COLUMN target_conv TEXT;
		INSERT INTO audit_log(id,at,verb,detail,actor_agent,target_agent,target_conv) VALUES('9','2026-08-31T10:00:00Z','agent.stop','historical','agt_fixture','agt_fixture','native-conv');
		ALTER TABLE execution_operations ADD COLUMN requested_at TEXT;
		UPDATE execution_operations SET requested_at='2026-08-31T11:00:00Z';
		ALTER TABLE agent_messages ADD COLUMN cc_recipients TEXT;
		ALTER TABLE agent_messages ADD COLUMN cc_recipient_agents TEXT;
		INSERT INTO conv_index(conv_id,harness) VALUES('plain-history','claude');
		UPDATE agent_messages SET cc_recipients='["plain-history"]',cc_recipient_agents='[""]'`)
	destination := filepath.Join(t.TempDir(), "replacement.sqlite")
	result, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	require.False(t, result.Repeated)
	require.NotEmpty(t, result.Receipt.SemanticSHA256)

	inspection, err := Inspect(context.Background(), bundle)
	require.NoError(t, err)
	plan, err := Plan(inspection)
	require.NoError(t, err)
	agentID := model.AgentID(findIdentity(t, plan, "agents", "agt_fixture").TargetID)
	groupID := model.GroupID(findIdentity(t, plan, "agent_groups", "1").TargetID)

	store, err := backendsqlite.Open(destination)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	agent, err := store.Agent(context.Background(), agentID)
	require.NoError(t, err)
	require.Equal(t, "fixture", agent.Name)
	require.Equal(t, "https://tracker.invalid/T-1", agent.TaskReference)
	require.Empty(t, agent.PrimaryExecutionID)
	require.NotNil(t, agent.ConfigurationProfile)
	require.Equal(t, model.ConfigurationProfileID(findIdentity(t, plan, "spawn_profiles", "7").TargetID), agent.ConfigurationProfile.ProfileID)
	group, err := store.Group(context.Background(), groupID)
	require.NoError(t, err)
	require.Empty(t, group.OwnerAgentID, "ambiguous legacy owners must not become authority")
	require.Equal(t, []model.AgentID{agentID}, group.Members)
	messages, err := store.MessagesForAgent(context.Background(), agentID, false)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Equal(t, "message-secret", messages[0].Body)
	require.Len(t, messages[0].Attachments, 1)
	require.Equal(t, int64(len("attachment")), messages[0].Attachments[0].Size)
	records, err := store.ImportedSourceRecords(context.Background(), "trigger_rules")
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, string(Inactive), records[0].Class)
	defaults, err := store.ConfigurationDefaults(context.Background())
	require.NoError(t, err)
	require.NotNil(t, defaults.Global)
	definitionID := model.DefinitionID(findIdentity(t, plan, "group_templates", "4").TargetID)
	definition, err := store.Definition(context.Background(), definitionID)
	require.NoError(t, err)
	require.Equal(t, "legacy-v228-unsupported", definition.Head.CompilerVersion)
	require.Nil(t, definition.Head.Team)
	ruleID := model.AutomationRuleID(findIdentity(t, plan, "trigger_rules", "1").TargetID)
	rule, err := store.AutomationRule(context.Background(), ruleID)
	require.NoError(t, err)
	require.False(t, rule.Rule.Enabled)
	require.Empty(t, rule.Head.Action.Kind)
	activity, err := store.QueryActivity(context.Background(), app.ActivityFilter{Target: app.ActivityTarget{AgentID: agentID}}, model.AuthorityRequest{Principal: model.OperatorPrincipal(), Action: model.ActionReadActivity, Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: agentID}}, result.Receipt.CompletedAt)
	require.NoError(t, err)
	require.NotEmpty(t, activity.Records)
	var interrupted bool
	for _, record := range activity.Records {
		interrupted = interrupted || (record.Historical && record.Outcome == "interrupted_unresolved")
	}
	require.True(t, interrupted)
	conversationID := model.ConversationID(findIdentity(t, plan, "agent_conversations", "native-conv").TargetID)
	usage, err := store.QueryUsage(context.Background(), app.UsageFilter{Target: app.UsageTarget{ConversationID: conversationID}}, model.AuthorityRequest{Principal: model.OperatorPrincipal(), Action: model.ActionReadUsage, Resource: model.ResourceSelector{Kind: model.ResourceConversation, ConversationID: conversationID}}, result.Receipt.CompletedAt)
	require.NoError(t, err)
	require.Len(t, usage.Observations, 1)
	require.Equal(t, "1.2500", usage.Observations[0].Cost.Amount)
	report, err := store.ImportReport(context.Background())
	require.NoError(t, err)
	require.Equal(t, result.Receipt, report.Receipt)
	require.NotEmpty(t, report.IDMappings)
	require.NotEmpty(t, report.Records)
	require.NotEmpty(t, report.Diagnostics)
	require.NotEmpty(t, report.Records[0].SourcePath)
	require.NotEmpty(t, report.Records[0].PayloadSHA256)
	reportJSON, err := json.Marshal(report)
	require.NoError(t, err)
	require.NotContains(t, string(reportJSON), "message-secret", "default report must not expose retained source payloads")
	require.NotContains(t, string(reportJSON), "YXR0YWNobWVudA==", "default report must not expose attachment content")

	raw, err := sql.Open("sqlite", destination)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	var content []byte
	var availability string
	require.NoError(t, raw.QueryRow(`SELECT a.content,v.availability FROM attachments a JOIN imported_attachment_availability v ON v.attachment_id=a.id`).Scan(&content, &availability))
	require.Equal(t, []byte("attachment"), content)
	require.Equal(t, string(model.ImportedAttachmentAvailable), availability)
	importedAttachment, err := store.ImportedAttachment(context.Background(), messages[0].Attachments[0].ID)
	require.NoError(t, err)
	require.Equal(t, []byte("attachment"), importedAttachment.Content)
	beforeReport, err := os.ReadFile(destination)
	require.NoError(t, err)
	readOnlyReport, err := ReadImportReport(context.Background(), destination)
	require.NoError(t, err)
	require.Equal(t, report, readOnlyReport)
	afterReport, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, beforeReport, afterReport, "read-only report must not mutate the completed database")
	envelope, err := store.ImportedMessageEnvelope(context.Background(), messages[0].ID)
	require.NoError(t, err)
	var unresolvedPlain bool
	for _, address := range envelope.Addresses {
		unresolvedPlain = unresolvedPlain || (address.OriginalConversation == "plain-history" && !address.Resolved)
	}
	require.True(t, unresolvedPlain)
	var amount, currency, precision, provenance string
	require.NoError(t, raw.QueryRow(`SELECT cost_amount,cost_currency,attribution_precision,provenance FROM usage_observations`).Scan(&amount, &currency, &precision, &provenance))
	require.Equal(t, "1.2500", amount)
	require.Equal(t, "USD", currency)
	require.Equal(t, string(model.UsageAttributionConversation), precision)
	require.Contains(t, provenance, "session_cost_daily[")
	for _, table := range []string{"executions", "execution_accesses", "authority_grants", "role_assignments", "effect_permits", "workspace_uses", "automation_occurrences"} {
		var count int
		require.NoError(t, raw.QueryRow(`SELECT count(*) FROM `+table).Scan(&count))
		require.Zero(t, count, table+" must not be activated by import")
	}

	repeated, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	require.True(t, repeated.Repeated)
	require.Equal(t, result.Receipt, repeated.Receipt)
}

func TestImportSnapshotMetadataOnlyLossIsExplicit(t *testing.T) {
	bundle := buildFixture(t, fixtureOptions{attachment: true})
	var manifest Manifest
	data, err := os.ReadFile(bundle.ManifestPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &manifest))
	manifest.Attachments = nil
	writeManifest(t, bundle.ManifestPath, manifest)
	destination := filepath.Join(t.TempDir(), "metadata.sqlite")
	result, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: destination, MetadataOnlyAttachments: true})
	require.NoError(t, err)
	require.True(t, result.Receipt.MetadataOnlyAttachments)
	db, err := sql.Open("sqlite", destination)
	require.NoError(t, err)
	defer db.Close()
	var availability, reason string
	var content []byte
	require.NoError(t, db.QueryRow(`SELECT v.availability,v.loss_reason,a.content FROM imported_attachment_availability v JOIN attachments a ON a.id=v.attachment_id`).Scan(&availability, &reason, &content))
	require.Equal(t, string(model.ImportedAttachmentMissing), availability)
	require.Equal(t, "operator_selected_metadata_only_loss", reason)
	require.Empty(t, content)
}

func TestImportSnapshotPreservesOperatorMessageDirectionAndCanonicalFanoutCopy(t *testing.T) {
	bundle := buildFixture(t, fixtureOptions{})
	alterFixture(t, bundle, `
		INSERT INTO agents(agent_id,current_conv_id,created_at,initial_spawn_config) VALUES('agt_second','second',1,'{}');
		INSERT INTO agent_conversations(conv_id,agent_id,linked_at) VALUES('second','agt_second',1);
		ALTER TABLE agent_messages ADD COLUMN to_recipients TEXT;
		ALTER TABLE agent_messages ADD COLUMN to_recipient_agents TEXT;
		UPDATE agent_messages SET from_agent='',from_conv='',to_recipients='["native-conv","second"]',to_recipient_agents='["agt_fixture","agt_second"]';
		INSERT INTO operator_agent_messages(message_id) VALUES('1');
		INSERT INTO agent_messages(id,from_conv,to_conv,body,created_at,from_agent,to_agent,to_recipients,to_recipient_agents)
		VALUES('2','native-conv','second','fanout-copy',1,'agt_fixture','agt_second','["native-conv","second"]','["agt_fixture","agt_second"]')`)
	inspection, err := Inspect(context.Background(), bundle)
	require.NoError(t, err)
	plan, err := Plan(inspection)
	require.NoError(t, err)
	destination := filepath.Join(t.TempDir(), "messages.sqlite")
	_, err = ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	store, err := backendsqlite.Open(destination)
	require.NoError(t, err)
	defer store.Close()
	first := model.AgentID(findIdentity(t, plan, "agents", "agt_fixture").TargetID)
	second := model.AgentID(findIdentity(t, plan, "agents", "agt_second").TargetID)
	firstMessages, err := store.MessagesForAgent(context.Background(), first, false)
	require.NoError(t, err)
	require.Len(t, firstMessages, 1, "operator authorship must not reverse the canonical recipient")
	require.Equal(t, model.PrincipalOperator, firstMessages[0].Sender.Kind)
	secondMessages, err := store.MessagesForAgent(context.Background(), second, false)
	require.NoError(t, err)
	require.Len(t, secondMessages, 1, "display audience must not expand each delivery copy")
}

func TestTranslateKeepsMessageMarkerNamespacesSeparate(t *testing.T) {
	bundle := buildFixture(t, fixtureOptions{})
	alterFixture(t, bundle, `
		UPDATE agent_messages SET from_agent='',from_conv='';
		INSERT INTO operator_agent_messages(message_id) VALUES('1');
		INSERT INTO human_messages(id,from_conv,from_agent,body,created_at) VALUES('1','native-conv','agt_fixture','notification',1)`)
	inspection, err := Inspect(context.Background(), bundle)
	require.NoError(t, err)
	plan, err := Plan(inspection)
	require.NoError(t, err)
	batch, err := Translate(inspection, plan, nil, TranslationOptions{})
	require.NoError(t, err)
	humanID := model.MessageID(findIdentity(t, plan, "human_messages", "1").TargetID)
	for _, message := range batch.Messages {
		if message.ID == humanID {
			require.Equal(t, model.PrincipalAgent, message.Sender.Kind)
			require.Equal(t, model.AgentID("agt_fixture"), message.Sender.AgentID)
			return
		}
	}
	t.Fatal("translated human message not found")
}

func TestImportSnapshotRetainsInactiveConfigAndCatalogOnlyUsage(t *testing.T) {
	bundle := buildFixture(t, fixtureOptions{config: `{"agent":{"default_permissions":["message.direct"]},"runtime_secret":"excluded"}`})
	alterFixture(t, bundle, `
		INSERT INTO conv_index(conv_id,harness) VALUES('plain-history','codex');
		ALTER TABLE session_cost_daily ADD COLUMN conv_id TEXT;
		INSERT INTO session_cost_daily(session_id,day,conv_id,cost_usd,agent_id,harness) VALUES('s','2026-08-31','plain-history','2.5','','codex')`)
	destination := filepath.Join(t.TempDir(), "history.sqlite")
	result, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.Receipt.Counts["usage"])
	store, err := backendsqlite.Open(destination)
	require.NoError(t, err)
	defer store.Close()
	records, err := store.ImportedSourceRecords(context.Background(), "authored_config")
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.JSONEq(t, `["message.direct"]`, string(records[0].Payload))
	require.NotContains(t, string(records[0].Payload), "runtime_secret")
}

func TestImportSnapshotRejectsChangedCompletedDestination(t *testing.T) {
	bundle := buildFixture(t, fixtureOptions{attachment: true})
	destination := filepath.Join(t.TempDir(), "changed.sqlite")
	_, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	db, err := sql.Open("sqlite", destination)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE messages SET body='changed'; UPDATE attachments SET content=x'00'; DELETE FROM imported_source_records`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	_, err = ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: destination})
	require.Error(t, err, "a receipt alone must not qualify a changed destination as an exact retry")
}

func TestImportSnapshotExactRetryRejectsSemanticAndActiveTargetChanges(t *testing.T) {
	for name, query := range map[string]string{
		"operation semantics": `UPDATE operations SET state='failed',result_code='changed'`,
		"active authority":    `INSERT INTO authority_grants(id,subject_kind,subject_id,action,resource_kind,resource_id,bounds_json,revision,created_at,updated_at) VALUES('grant_added','agent','agt_fixture','execution.launch','agent','agt_fixture','{}',1,1,1)`,
		"wal message":         `PRAGMA journal_mode=WAL; UPDATE messages SET body='changed in WAL'`,
	} {
		t.Run(name, func(t *testing.T) {
			bundle := buildFixture(t, fixtureOptions{})
			destination := filepath.Join(t.TempDir(), "target.sqlite")
			_, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: destination})
			require.NoError(t, err)
			db, err := sql.Open("sqlite", destination)
			require.NoError(t, err)
			_, err = db.Exec(query)
			require.NoError(t, err)
			if name != "wal message" {
				require.NoError(t, db.Close())
			} else {
				defer db.Close()
				var body string
				require.NoError(t, db.QueryRow(`SELECT body FROM messages LIMIT 1`).Scan(&body))
				require.Equal(t, "changed in WAL", body)
			}
			_, err = ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: destination})
			require.Error(t, err, "a changed target must not qualify as an exact retry")
		})
	}
}

func TestImportSnapshotDoesNotSwallowPostLinkSyncFailure(t *testing.T) {
	bundle := buildFixture(t, fixtureOptions{})
	destination := filepath.Join(t.TempDir(), "sync-failure.sqlite")
	originalSync := syncPublishedDirectory
	failed := false
	syncPublishedDirectory = func(path string) error {
		if !failed {
			failed = true
			return errors.New("injected directory sync failure")
		}
		return originalSync(path)
	}
	t.Cleanup(func() { syncPublishedDirectory = originalSync })
	_, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: destination})
	require.ErrorContains(t, err, "injected directory sync failure")
	_, reportErr := ReadImportReport(context.Background(), destination)
	require.NoError(t, reportErr, "post-link failure may expose only the complete verified database")
	syncPublishedDirectory = originalSync
	repeated, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	require.True(t, repeated.Repeated)
}

func TestReadImportReportIsRedactedAndNonMutating(t *testing.T) {
	bundle := buildFixture(t, fixtureOptions{attachment: true, config: `{"agent":{"default_permissions":["message.direct"]},"operator_token":"report-secret"}`})
	directory := t.TempDir()
	destination := filepath.Join(directory, "target.sqlite")
	_, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: destination})
	require.NoError(t, err)
	before, err := os.ReadFile(destination)
	require.NoError(t, err)
	report, err := ReadImportReport(context.Background(), destination)
	require.NoError(t, err)
	after, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, before, after)
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	for _, secret := range []string{"report-secret", "message-secret", "message.direct", "YXR0YWNobWVudA=="} {
		require.NotContains(t, string(encoded), secret)
	}
	require.EqualValues(t, 1, report.Receipt.Counts["attachments_available"])
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	require.Len(t, entries, 1, "read-only reporting must not create SQLite sidecars")
	missing := filepath.Join(directory, "missing.sqlite")
	_, err = ReadImportReport(context.Background(), missing)
	require.Error(t, err)
	_, err = os.Stat(missing)
	require.True(t, os.IsNotExist(err))
}

func TestImportSnapshotConcurrentPublicationAndReplacementRefusal(t *testing.T) {
	bundle := buildFixture(t, fixtureOptions{})
	destination := filepath.Join(t.TempDir(), "race.sqlite")
	results := make(chan ImportResult, 2)
	errors := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: destination})
			results <- result
			errors <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	var fresh, repeated int
	for result := range results {
		if result.Repeated {
			repeated++
		} else {
			fresh++
		}
	}
	require.Equal(t, 1, fresh)
	require.Equal(t, 1, repeated)

	foreign := filepath.Join(t.TempDir(), "foreign.sqlite")
	require.NoError(t, os.WriteFile(foreign, []byte("operator-owned"), 0o600))
	_, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: foreign})
	require.Error(t, err)
	unchanged, readErr := os.ReadFile(foreign)
	require.NoError(t, readErr)
	require.Equal(t, []byte("operator-owned"), unchanged)
}

func TestApplyImportFailureRollsBackReceiptAndEntities(t *testing.T) {
	bundle := buildFixture(t, fixtureOptions{})
	inspection, err := Inspect(context.Background(), bundle)
	require.NoError(t, err)
	plan, err := Plan(inspection)
	require.NoError(t, err)
	batch, err := Translate(inspection, plan, nil, TranslationOptions{})
	require.NoError(t, err)
	batch.Agents = append(batch.Agents, batch.Agents[0])
	path := filepath.Join(t.TempDir(), "rollback.sqlite")
	store, err := backendsqlite.Open(path)
	require.NoError(t, err)
	require.Error(t, store.ApplyImport(context.Background(), batch))
	require.Error(t, store.VerifyImport(context.Background(), batch))
	require.NoError(t, store.Close())
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	for _, table := range []string{"agents", "import_receipts", "import_id_map", "imported_source_records"} {
		var count int
		err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count)
		if table != "agents" && err != nil {
			// DDL is transactional too, so importer-owned tables may not exist.
			continue
		}
		require.NoError(t, err)
		require.Zero(t, count)
	}
}
