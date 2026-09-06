package migration

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	sourcev228 "github.com/tofutools/tclaude/internal/backend/migration/source/v228"
)

func TestInspectAndPlanDeterministicWithoutMutatingSource(t *testing.T) {
	bundle := buildFixture(t, fixtureOptions{config: `{"operator_token":"raw-secret","agent":{"default_permissions":["groups.members.spawn"]}}`})
	databasePath := filepath.Join(bundle.Root, "snapshot.sqlite")
	before, err := os.ReadFile(databasePath)
	require.NoError(t, err)

	inspection, err := Inspect(context.Background(), bundle)
	require.NoError(t, err)
	require.True(t, inspection.Valid, inspection.Diagnostics)
	require.Equal(t, SourceSchemaVersion, inspection.Source.SchemaVersion)
	require.NotEmpty(t, inspection.Snapshot.Rows["agent_messages"])

	first, err := Plan(inspection)
	require.NoError(t, err)
	second, err := Plan(inspection)
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.True(t, first.PreflightValid)
	require.False(t, first.ExecutableConversion)
	require.NotEmpty(t, first.PlanHash)
	requireDiagnostic(t, first.Diagnostics, "authority_preserved_inactive")
	requireDiagnostic(t, first.Diagnostics, "automation_imported_disabled")
	requireDiagnostic(t, first.Diagnostics, "operation_interrupted_unresolved")

	agent := findIdentity(t, first, "agents", "agt_fixture")
	require.True(t, agent.Retained)
	require.Equal(t, "agt_fixture", agent.TargetID)
	group := findIdentity(t, first, "agent_groups", "1")
	require.False(t, group.Retained)
	require.True(t, strings.HasPrefix(group.TargetID, "grp_"))

	report, err := inspection.MarshalReport()
	require.NoError(t, err)
	require.NotContains(t, string(report), "raw-secret")
	require.NotContains(t, string(report), "message-secret")
	planReport, err := json.Marshal(first)
	require.NoError(t, err)
	require.NotContains(t, string(planReport), "raw-secret")
	require.NotContains(t, string(planReport), "message-secret")
	after, err := os.ReadFile(databasePath)
	require.NoError(t, err)
	require.Equal(t, before, after, "immutable inspection changed source bytes")
}

func TestInspectReportsNonSelfContainedSQLiteSnapshot(t *testing.T) {
	bundle := buildFixture(t, fixtureOptions{})
	require.NoError(t, os.WriteFile(filepath.Join(bundle.Root, "snapshot.sqlite-wal"), []byte("not-a-backup"), 0o600))
	inspection, err := Inspect(context.Background(), bundle)
	require.NoError(t, err)
	require.False(t, inspection.Valid)
	requireDiagnostic(t, inspection.Diagnostics, "database_sidecar_present")
}

func TestPlanReportsBrokenIdentityReference(t *testing.T) {
	bundle := buildFixture(t, fixtureOptions{brokenReference: true})
	inspection, err := Inspect(context.Background(), bundle)
	require.NoError(t, err)
	require.False(t, inspection.Valid)
	requireDiagnostic(t, inspection.Diagnostics, "unresolved_identity_reference")
	plan, err := Plan(inspection)
	require.NoError(t, err)
	requireDiagnostic(t, plan.Diagnostics, "broken_dependency_reference")
}

func TestInspectRejectsFutureSchemaAndMalformedAuthoredJSON(t *testing.T) {
	bundle := buildFixture(t, fixtureOptions{schemaVersion: 229, malformedProcessJSON: true})
	inspection, err := Inspect(context.Background(), bundle)
	require.NoError(t, err)
	require.False(t, inspection.Valid)
	requireDiagnostic(t, inspection.Diagnostics, "unsupported_source_schema")
	requireDiagnostic(t, inspection.Diagnostics, "malformed_or_oversize_json")
	plan, err := Plan(inspection)
	require.NoError(t, err)
	require.False(t, plan.PreflightValid)
	require.False(t, plan.ExecutableConversion)
}

func TestInspectAttachmentContainmentAndCompleteness(t *testing.T) {
	bundle := buildFixture(t, fixtureOptions{attachment: true})
	inspection, err := Inspect(context.Background(), bundle)
	require.NoError(t, err)
	require.True(t, inspection.Valid, inspection.Diagnostics)

	manifestBytes, err := os.ReadFile(bundle.ManifestPath)
	require.NoError(t, err)
	var manifest Manifest
	require.NoError(t, json.Unmarshal(manifestBytes, &manifest))
	manifest.Attachments[0].Path = "../outside.bin"
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(bundle.Root), "outside.bin"), []byte("attachment"), 0o600))
	writeManifest(t, bundle.ManifestPath, manifest)
	inspection, err = Inspect(context.Background(), bundle)
	require.NoError(t, err)
	require.False(t, inspection.Valid)
	requireDiagnostic(t, inspection.Diagnostics, "attachment_validation_failed")
}

func TestPlanQuarantinesUnclassifiedNonemptyTable(t *testing.T) {
	bundle := buildFixture(t, fixtureOptions{extraTable: true})
	inspection, err := Inspect(context.Background(), bundle)
	require.NoError(t, err)
	plan, err := Plan(inspection)
	require.NoError(t, err)
	require.False(t, plan.PreflightValid)
	requireDiagnostic(t, plan.Diagnostics, "unclassified_source_table")
}

func TestPlanOrchestrationQuarantineDiagnostics(t *testing.T) {
	bundle := buildFixture(t, fixtureOptions{})
	inspection, err := Inspect(context.Background(), bundle)
	require.NoError(t, err)
	run := inspection.Snapshot.Rows["process_runs"][0]
	run.Values = cloneValues(run.Values)
	run.Values["template_snapshot_json"] = `{"performer":{"kind":"program"}}`
	inspection.Snapshot.Rows["process_runs"] = append(inspection.Snapshot.Rows["process_runs"], run)
	inspection.Snapshot.Rows["trigger_action_outcomes"] = []sourcev228.Row{
		{Key: "1", Values: map[string]any{"firing_id": int64(9), "action_index": int64(0)}},
		{Key: "2", Values: map[string]any{"firing_id": int64(9), "action_index": int64(0)}},
	}
	plan, err := Plan(inspection)
	require.NoError(t, err)
	require.False(t, plan.PreflightValid)
	requireDiagnostic(t, plan.Diagnostics, "revision_hash_disagreement")
	requireDiagnostic(t, plan.Diagnostics, "missing_performer_profile_revision")
	requireDiagnostic(t, plan.Diagnostics, "ambiguous_occurrence_issuance")
}

func TestV228SignatureMatchesRepositorySchema(t *testing.T) {
	schema, err := os.ReadFile(filepath.Join("..", "..", "..", "pkg", "claude", "common", "db", "schema.sql"))
	require.NoError(t, err)
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.Exec(string(schema))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO schema_version(version) VALUES(?)`, SourceSchemaVersion)
	require.NoError(t, err)
	missing, err := sourcev228.ValidateSignatures(context.Background(), db)
	require.NoError(t, err)
	require.Empty(t, missing)
	counts, err := sourcev228.TableCounts(context.Background(), db)
	require.NoError(t, err)
	for table := range counts {
		_, classified := tableRules[table]
		require.Truef(t, classified, "v228 table %s has no disposition", table)
	}
	for table, rule := range tableRules {
		if table == "schema_version" || (rule.class != Preserve && rule.class != Inactive) {
			continue
		}
		_, retained := sourcev228.SnapshotTables[table]
		require.Truef(t, retained, "preserved v228 table %s is not retained in Snapshot", table)
	}
}

type fixtureOptions struct {
	schemaVersion        int
	malformedProcessJSON bool
	attachment           bool
	extraTable           bool
	config               string
	brokenReference      bool
}

func buildFixture(t *testing.T, options fixtureOptions) Bundle {
	t.Helper()
	root := t.TempDir()
	databasePath := filepath.Join(root, "snapshot.sqlite")
	db, err := sql.Open("sqlite", databasePath)
	require.NoError(t, err)
	version := options.schemaVersion
	if version == 0 {
		version = SourceSchemaVersion
	}
	names := make([]string, 0, len(sourcev228.SnapshotTables)+1)
	for name := range sourcev228.RequiredTables {
		names = append(names, name)
	}
	for name := range sourcev228.SnapshotTables {
		if _, required := sourcev228.RequiredTables[name]; !required {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		spec := sourcev228.RequiredTables[name]
		columns := append([]string(nil), spec.Columns...)
		columns = append(columns, sourcev228.SnapshotTables[name]...)
		columns = uniqueStrings(columns)
		if name == "schema_version" {
			_, err = db.Exec(`CREATE TABLE schema_version(version INTEGER NOT NULL)`)
		} else {
			defs := make([]string, len(columns))
			for i, column := range columns {
				defs[i] = quoteTestIdentifier(column) + ` TEXT`
			}
			_, err = db.Exec(`CREATE TABLE ` + quoteTestIdentifier(name) + `(` + strings.Join(defs, ",") + `)`)
		}
		require.NoError(t, err, name)
	}
	_, err = db.Exec(`INSERT INTO schema_version(version) VALUES(?)`, version)
	require.NoError(t, err)
	insertFixtureRows(t, db, options)
	if options.extraTable {
		_, err = db.Exec(`CREATE TABLE new_authored_feature(id TEXT); INSERT INTO new_authored_feature VALUES('one')`)
		require.NoError(t, err)
	}
	require.NoError(t, db.Close())

	manifest := Manifest{FormatVersion: BundleFormatVersion, Database: manifestFile(t, root, "snapshot.sqlite")}
	if options.config != "" {
		require.NoError(t, os.WriteFile(filepath.Join(root, "config.json"), []byte(options.config), 0o600))
		config := manifestFile(t, root, "config.json")
		manifest.Config = &config
	}
	if options.attachment {
		require.NoError(t, os.Mkdir(filepath.Join(root, "attachments"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(root, "attachments", "one.bin"), []byte("attachment"), 0o600))
		file := manifestFile(t, root, "attachments/one.bin")
		manifest.Attachments = []Attachment{{SourceTable: "agent_message_attachments", SourceID: "1", Path: file.Path, Size: file.Size, SHA256: file.SHA256}}
	}
	manifestPath := filepath.Join(root, "manifest.json")
	writeManifest(t, manifestPath, manifest)
	return Bundle{Root: root, ManifestPath: manifestPath}
}

func insertFixtureRows(t *testing.T, db *sql.DB, options fixtureOptions) {
	t.Helper()
	exec := func(query string, args ...any) { _, err := db.Exec(query, args...); require.NoError(t, err, query) }
	exec(`INSERT INTO agents(agent_id,current_conv_id,created_at,initial_spawn_config,task_ref_url,relaunch_profile) VALUES('agt_fixture','native-conv',1,'{}','https://tracker.invalid/T-1','safe')`)
	exec(`INSERT INTO logical_conversations(id,created_at) VALUES('cvn_fixture',1)`)
	exec(`INSERT INTO agent_conversations(conv_id,agent_id,role,reason,linked_at) VALUES('native-conv','agt_fixture','engineer','created',1)`)
	exec(`INSERT INTO agent_groups(id,name,owner_scopes_json,source_template_id,default_profile_id) VALUES('1','team','[]',NULL,NULL)`)
	memberAgent := "agt_fixture"
	if options.brokenReference {
		memberAgent = "agt_missing"
	}
	exec(`INSERT INTO agent_group_members(group_id,agent_id,role,joined_at) VALUES('1',?,'engineer',1)`, memberAgent)
	exec(`INSERT INTO agent_group_owners(group_id,agent_id,granted_at,granted_by) VALUES('1','agt_fixture',1,'operator')`)
	exec(`INSERT INTO agent_permissions(agent_id,slug,effect,scope_json) VALUES('agt_fixture','groups.members.spawn','grant','{}')`)
	exec(`INSERT INTO conversation_attempt_bindings(execution_id,conversation_id,external_ref,revision) VALUES('exe_fixture','cvn_fixture','bound-native','1')`)
	exec(`INSERT INTO conversation_reference_bindings(execution_id,revision,conversation_id,external_ref) VALUES('exe_fixture','1','cvn_fixture','bound-native')`)
	exec(`INSERT INTO conv_index(conv_id,full_path,custom_title,harness) VALUES('native-conv','/private/history','fixture','codex')`)
	exec(`INSERT INTO agent_messages(id,from_conv,to_conv,body,created_at,from_agent,to_agent) VALUES('1','native-conv','native-conv','message-secret',1,'agt_fixture','agt_fixture')`)
	exec(`INSERT INTO trigger_rules(id,revision,owner_agent,enabled,actions_json) VALUES('1','1','agt_fixture','1','[]')`)
	processJSON := `{"nodes":[]}`
	if options.malformedProcessJSON {
		processJSON = `{broken`
	}
	exec(`INSERT INTO process_runs(id,template_snapshot_json,params_json,status,checkpoint_json,program_authorizations_json) VALUES('run.fixture',?,'{}','running','{}','[]')`, processJSON)
	exec(`INSERT INTO execution_operations(id,kind,agent_id,state,launch_phase) VALUES('op_fixture','launch','agt_fixture','released','released')`)
	if options.attachment {
		exec(`INSERT INTO agent_message_attachments(id,message_id,ordinal,filename,size_bytes,storage_path) VALUES('1','1','0','one.bin','10','/legacy/private/one.bin')`)
	}
}

func manifestFile(t *testing.T, root, relative string) ManifestFile {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	info, err := os.Stat(path)
	require.NoError(t, err)
	return ManifestFile{Path: relative, Size: info.Size(), SHA256: fileDigest(path)}
}

func writeManifest(t *testing.T, path string, manifest Manifest) {
	t.Helper()
	data, err := json.MarshalIndent(manifest, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func requireDiagnostic(t *testing.T, diagnostics []Diagnostic, code string) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return
		}
	}
	t.Fatalf("diagnostic %q missing from %#v", code, diagnostics)
}

func findIdentity(t *testing.T, plan MigrationPlan, table, key string) IdentityMapping {
	t.Helper()
	for _, identity := range plan.Identities {
		if identity.SourceTable == table && identity.SourceKey == key {
			return identity
		}
	}
	t.Fatalf("identity %s/%s missing", table, key)
	return IdentityMapping{}
}

func cloneValues(source map[string]any) map[string]any {
	out := make(map[string]any, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func quoteTestIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}
