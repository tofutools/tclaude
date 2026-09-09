package v228

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type TableSpec struct {
	Columns []string
	Key     []string
}

// RequiredTables is intentionally about preservation-critical schema shape,
// not every cache or transient column ever present in v228.
var RequiredTables = map[string]TableSpec{
	"schema_version":                  {Columns: []string{"version"}},
	"agents":                          {Columns: []string{"agent_id", "current_conv_id", "created_at", "initial_spawn_config", "task_ref_url", "relaunch_profile"}, Key: []string{"agent_id"}},
	"agent_conversations":             {Columns: []string{"conv_id", "agent_id", "role", "reason", "linked_at"}, Key: []string{"conv_id"}},
	"agent_groups":                    {Columns: []string{"id", "name", "owner_scopes_json", "source_template_id", "default_profile_id", "default_cwd"}, Key: []string{"id"}},
	"agent_group_members":             {Columns: []string{"group_id", "agent_id", "role", "joined_at"}, Key: []string{"group_id", "agent_id"}},
	"agent_group_owners":              {Columns: []string{"group_id", "agent_id", "granted_at", "granted_by"}, Key: []string{"group_id", "agent_id"}},
	"agent_permissions":               {Columns: []string{"agent_id", "slug", "effect", "scope_json"}, Key: []string{"agent_id", "slug"}},
	"agent_group_permissions":         {Columns: []string{"group_id", "slug", "scope_json"}, Key: []string{"group_id", "slug"}},
	"agent_sudo_grants":               {Columns: []string{"id", "agent_id", "slug", "expires_at", "revoked_at", "scope_json"}, Key: []string{"id"}},
	"roles":                           {Columns: []string{"id", "name", "permissions", "spawn_profile_id"}, Key: []string{"id"}},
	"spawn_profiles":                  {Columns: []string{"id", "name", "permission_overrides", "environment_json", "role_refs", "disabled", "disabled_reason", "fast_mode", "auto_review", "auto_memory", "peer_messaging", "trust_dir", "ask_user_question_timeout", "auto_compact_window", "operator_only"}, Key: []string{"id"}},
	"spawn_profile_aliases":           {Columns: []string{"alias", "profile_id"}, Key: []string{"alias"}},
	"sandbox_profiles":                {Columns: []string{"id", "name", "filesystem_json", "environment_json", "includes_json"}, Key: []string{"id"}},
	"group_templates":                 {Columns: []string{"id", "name", "process", "rhythms", "owner_scopes_json"}, Key: []string{"id"}},
	"group_template_agents":           {Columns: []string{"id", "template_id", "name", "permissions", "profile_inline"}, Key: []string{"id"}},
	"agent_messages":                  {Columns: []string{"id", "from_conv", "to_conv", "body", "created_at", "from_agent", "to_agent"}, Key: []string{"id"}},
	"agent_message_attachments":       {Columns: []string{"id", "message_id", "ordinal", "filename", "size_bytes", "storage_path"}, Key: []string{"id"}},
	"human_messages":                  {Columns: []string{"id", "from_conv", "body", "created_at", "from_agent"}, Key: []string{"id"}},
	"human_message_attachments":       {Columns: []string{"id", "message_id", "seq", "filename", "size_bytes", "storage_path"}, Key: []string{"id"}},
	"logical_conversations":           {Columns: []string{"id", "created_at"}, Key: []string{"id"}},
	"conversation_attempt_bindings":   {Columns: []string{"execution_id", "conversation_id", "external_ref", "revision", "harness", "namespace"}, Key: []string{"execution_id"}},
	"conversation_reference_bindings": {Columns: []string{"execution_id", "revision", "conversation_id", "external_ref", "harness", "namespace"}, Key: []string{"execution_id", "revision"}},
	"conv_index":                      {Columns: []string{"conv_id", "full_path", "custom_title", "harness"}, Key: []string{"conv_id"}},
	"agent_workspace":                 {Columns: []string{"conv_id", "cwd", "branch", "repo_url", "agent_id"}, Key: []string{"conv_id"}},
	"process_snippets":                {Columns: []string{"id", "envelope_json", "revision"}, Key: []string{"id"}},
	"process_runs":                    {Columns: []string{"id", "template_snapshot_json", "params_json", "status", "checkpoint_json", "program_authorizations_json"}, Key: []string{"id"}},
	"process_run_events":              {Columns: []string{"run_id", "sequence", "kind", "payload_json"}, Key: []string{"run_id", "sequence"}},
	"agent_cron_jobs":                 {Columns: []string{"id", "owner_agent", "enabled", "action_kind"}, Key: []string{"id"}},
	"trigger_rules":                   {Columns: []string{"id", "revision", "owner_agent", "enabled", "actions_json"}, Key: []string{"id"}},
	"agent_standing_orders":           {Columns: []string{"id", "revision", "owner_agent", "enabled"}, Key: []string{"id"}},
	"execution_operations":            {Columns: []string{"id", "kind", "agent_id", "state", "launch_phase"}, Key: []string{"id"}},
	"audit_log":                       {Columns: []string{"id", "at", "verb", "detail", "actor_agent", "target_agent"}, Key: []string{"id"}},
	"session_cost_daily":              {Columns: []string{"session_id", "day", "cost_usd", "agent_id", "harness"}, Key: []string{"session_id", "day"}},
}

// SnapshotTables adds preservation/history tables whose complete shape is
// retained for later writers but whose columns are not part of the minimum
// preflight signature. Unknown additions remain visible in TableCounts and are
// quarantined by the planner rather than read opportunistically.
var SnapshotTables = func() map[string][]string {
	out := map[string][]string{}
	for table, spec := range RequiredTables {
		if table != "schema_version" {
			out[table] = spec.Key
		}
	}
	for table, key := range map[string][]string{
		"agent_tags": {"agent_id", "tag"}, "agent_head_aliases": {"handle"},
		"agent_group_links": {"id"},
		"access_requests":   {"id"}, "agent_recovery": {"agent_id"},
		"export_jobs": {"id"}, "pending_spawns": {"label"},
		"codex_native_permission_profiles":  {"generation"},
		"spawn_harness_rules":               {"group_id", "source_harness", "target_harness"},
		"sandbox_profile_global_assignment": {"id"},
		"agent_lineage":                     {"child_agent_id"}, "agent_conv_succession": {"old_conv_id"},
		"agent_clone_history": {"source_agent_id", "cloned_at"}, "agent_spawn_history": {"spawner_agent_id", "spawned_at"},
		"agent_group_audit": {"id"}, "agent_route_audit": {"id"}, "agent_transfer_log": {"id"},
		"agent_workdir": {"conv_id"}, "conv_branch_history": {"conv_id", "repo_dir", "branch"}, "agent_prs": {"id"},
		"operator_agent_messages": {"message_id"}, "agent_cron_messages": {"message_id"}, "agent_standing_order_messages": {"message_id"},
		"conversation_resume_profiles": {"conv_id"}, "process_snippet_library": {"id"},
		"group_process_state": {"group_id"}, "group_process_transitions": {"id"}, "group_wave_choreography": {"group_id"},
		"agent_cron_runs":   {"id"},
		"trigger_pr_events": {"id"}, "trigger_pr_observations": {"agent_pr_id"}, "trigger_firings": {"id"},
		"trigger_action_outcomes": {"id"}, "trigger_workers": {"id"}, "trigger_dwell_states": {"rule_id", "agent_id"},
		"agent_standing_order_deliveries": {"id"}, "agent_standing_order_turn_origins": {"target_agent"},
		"agent_standing_order_debounce": {"order_id", "target_agent"}, "agent_standing_order_group_scopes": {"order_id", "group_id"},
		"agent_standing_order_hook_selectors": {"order_id", "harness", "event"}, "daemon_spawn_history": {"principal", "spawned_at"},
		"agent_routes": {"id"}, "subscription_usage_samples": {"id"}, "subscription_usage_windows": {"sample_id", "window_name"},
		"session_execution_boundaries": {"session_id"},
		"dashboard_prefs":              {"key"}, "copilot_usage_snapshots": {"session_id"}, "opencode_usage_activity": {"session_id", "message_id"},
	} {
		out[table] = key
	}
	return out
}()

type JSONSpec struct {
	Limit      int
	AllowEmpty bool
}

var JSONColumns = map[string]map[string]JSONSpec{
	"agents":                  {"initial_spawn_config": {262144, true}},
	"agent_cron_jobs":         {"spawn_role_refs_json": {262144, true}},
	"agent_groups":            {"owner_scopes_json": {262144, true}, "environment_json": {262144, false}},
	"agent_permissions":       {"scope_json": {262144, true}},
	"agent_group_permissions": {"scope_json": {262144, true}},
	"agent_sudo_grants":       {"scope_json": {262144, true}},
	"roles":                   {"permissions": {262144, false}},
	"spawn_profiles":          {"permission_overrides": {262144, true}, "role_refs": {262144, false}, "environment_json": {262144, false}},
	"sandbox_profiles":        {"filesystem_json": {262144, false}, "environment_json": {262144, false}, "includes_json": {262144, false}, "network_json": {262144, true}, "resource_limits_json": {262144, false}},
	"group_templates":         {"owner_scopes_json": {262144, true}},
	"group_template_agents":   {"permissions": {262144, false}, "profile_inline": {262144, true}},
	"process_snippets":        {"envelope_json": {4194304, false}},
	"process_runs":            {"template_snapshot_json": {4194304, false}, "params_json": {262144, false}, "checkpoint_json": {4194304, false}, "program_authorizations_json": {262144, false}},
	"process_run_events":      {"payload_json": {262144, false}},
	"trigger_rules":           {"actions_json": {262144, false}},
}

func SchemaVersionValue(ctx context.Context, db *sql.DB) (version int, rows int64, err error) {
	if err := db.QueryRowContext(ctx, `SELECT count(*), COALESCE(min(version),0) FROM schema_version`).Scan(&rows, &version); err != nil {
		return 0, 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, rows, nil
}

func ValidateSignatures(ctx context.Context, db *sql.DB) ([]string, error) {
	names := make([]string, 0, len(SnapshotTables)+1)
	for name := range SnapshotTables {
		names = append(names, name)
	}
	names = append(names, "schema_version")
	sort.Strings(names)
	var missing []string
	for _, name := range names {
		rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+quoteIdentifier(name)+`)`)
		if err != nil {
			return nil, err
		}
		found := map[string]bool{}
		for rows.Next() {
			var cid, notnull, pk int
			var column, kind string
			var defaultValue any
			if err := rows.Scan(&cid, &column, &kind, &notnull, &defaultValue, &pk); err != nil {
				rows.Close()
				return nil, err
			}
			found[column] = true
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		columns := append([]string(nil), RequiredTables[name].Columns...)
		columns = append(columns, SnapshotTables[name]...)
		seenRequired := map[string]bool{}
		for _, column := range columns {
			if seenRequired[column] {
				continue
			}
			seenRequired[column] = true
			if !found[column] {
				missing = append(missing, name+"."+column)
			}
		}
	}
	return missing, nil
}

func TableCounts(ctx context.Context, db *sql.DB) (map[string]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(names))
	for _, name := range names {
		var count int64
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+quoteIdentifier(name)).Scan(&count); err != nil {
			return nil, err
		}
		out[name] = count
	}
	return out, nil
}

func ReadSnapshot(ctx context.Context, db *sql.DB) (Snapshot, error) {
	out := Snapshot{Rows: map[string][]Row{}, Malformed: map[string]int64{}}
	available, err := availableTables(ctx, db)
	if err != nil {
		return Snapshot{}, err
	}
	names := make([]string, 0, len(SnapshotTables))
	for name := range SnapshotTables {
		if available[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		rows, err := db.QueryContext(ctx, `SELECT * FROM `+quoteIdentifier(name))
		if err != nil {
			return Snapshot{}, fmt.Errorf("read %s: %w", name, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			return Snapshot{}, err
		}
		for rows.Next() {
			values, ptrs := make([]any, len(columns)), make([]any, len(columns))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return Snapshot{}, err
			}
			mapped := make(map[string]any, len(columns))
			for i, column := range columns {
				mapped[column] = normalize(values[i])
			}
			if name == "dashboard_prefs" && String(mapped["key"]) != "tclaude.dash.default_profile" && String(mapped["key"]) != "tclaude.dash.default_profile_id" {
				continue
			}
			out.Rows[name] = append(out.Rows[name], Row{Key: rowKey(mapped, SnapshotTables[name]), Values: mapped})
		}
		if err := rows.Close(); err != nil {
			return Snapshot{}, err
		}
	}
	return out, nil
}

func availableTables(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

func ValidateJSON(snapshot *Snapshot) {
	for table, rows := range snapshot.Rows {
		for _, row := range rows {
			for column, value := range row.Values {
				spec, authored := JSONColumns[table][column]
				if !authored && strings.HasSuffix(column, "_json") {
					spec, authored = JSONSpec{4194304, true}, true
				}
				if !authored || value == nil {
					continue
				}
				text, ok := value.(string)
				if !ok || len(text) > spec.Limit || (text == "" && !spec.AllowEmpty) || (text != "" && !validStructuredJSON(text, table, column)) {
					snapshot.Malformed[table+"."+column]++
				}
			}
		}
	}
}

func validStructuredJSON(text, table, column string) bool {
	trimmed := strings.TrimSpace(text)
	if !json.Valid([]byte(trimmed)) {
		return false
	}
	// Authored configuration/checkpoints are structured values, never scalars.
	if trimmed == "null" {
		return true
	} // legacy optional defaults
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return false
	}
	if table == "agents" && column == "initial_spawn_config" {
		return strings.HasPrefix(trimmed, "{")
	}
	switch column {
	case "role_refs", "spawn_role_refs_json", "includes_json":
		var refs []string
		return json.Unmarshal([]byte(trimmed), &refs) == nil
	}
	return true
}

func rowKey(values map[string]any, columns []string) string {
	parts := make([]string, len(columns))
	for i, column := range columns {
		parts[i] = fmt.Sprint(values[column])
	}
	return strings.Join(parts, "\x1f")
}

func normalize(value any) any {
	switch value := value.(type) {
	case []byte:
		return string(value)
	case int64:
		return value
	case float64:
		return value
	case nil:
		return nil
	default:
		return fmt.Sprint(value)
	}
}

func Int64(value any) (int64, bool) {
	switch value := value.(type) {
	case int64:
		return value, true
	case string:
		n, err := strconv.ParseInt(value, 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}

func String(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func quoteIdentifier(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }
