package migration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	sourcev228 "github.com/tofutools/tclaude/internal/backend/migration/source/v228"
)

type dispositionRule struct {
	class      Classification
	conversion ConversionState
	reason     string
}

var tableRules = rules(
	[]string{"agents", "agent_conversations", "logical_conversations", "conversation_attempt_bindings", "conversation_reference_bindings", "conv_index", "agent_workspace", "agent_workdir", "conv_branch_history", "agent_prs", "agent_messages", "operator_agent_messages", "human_messages", "agent_message_attachments", "human_message_attachments", "agent_lineage", "agent_conv_succession", "agent_clone_history", "agent_spawn_history", "agent_head_aliases", "agent_group_audit", "agent_route_audit", "agent_transfer_log", "audit_log", "access_requests", "agent_recovery", "export_jobs", "session_cost_daily", "subscription_usage_samples", "subscription_usage_windows", "process_run_events", "agent_cron_runs", "trigger_pr_events", "trigger_pr_observations", "trigger_firings", "trigger_action_outcomes", "trigger_workers", "agent_standing_order_deliveries", "agent_standing_order_messages", "agent_cron_messages", "daemon_spawn_history"}, dispositionRule{Preserve, ConversionPending, "preserved_pending_target_schema"},
	[]string{"agent_groups", "agent_group_members", "agent_group_owners", "agent_group_links", "agent_tags", "agent_permissions", "agent_group_permissions", "agent_sudo_grants", "roles", "spawn_profiles", "spawn_profile_aliases", "sandbox_profiles", "sandbox_profile_global_assignment", "spawn_harness_rules", "codex_native_permission_profiles", "group_templates", "group_template_agents", "process_snippets", "process_snippet_library", "group_process_state", "group_process_transitions", "group_wave_choreography", "process_runs", "agent_cron_jobs", "trigger_rules", "agent_standing_orders", "agent_standing_order_group_scopes", "agent_standing_order_hook_selectors", "agent_standing_order_turn_origins", "agent_standing_order_debounce", "trigger_dwell_states", "agent_routes", "conversation_resume_profiles", "pending_spawns"}, dispositionRule{Inactive, ConversionPending, "preserved_inactive_pending_validation"},
	[]string{"execution_operations", "session_execution_boundaries"}, dispositionRule{Preserve, ConversionInterrupted, "historical_effect_preserved_without_replay"},
	[]string{"sessions", "agent_route_leases", "darwin_route_launches", "darwin_route_slot_claims", "notify_state", "browser_notifications", "dashboard_session_grace", "ask_threads", "agentd_idempotency", "opencode_runtimes", "opencode_agent_state_allocations", "copilot_api_runtimes", "codex_app_server_runtimes", "codex_app_server_capabilities"}, dispositionRule{Reset, ConversionNotApplicable, "runtime_fact_or_claim_reset"},
	[]string{"conv_embeddings", "usage_cache", "git_cache", "codex_usage_cache", "codex_telemetry_checkpoints", "copilot_usage_snapshots", "opencode_usage_activity", "opencode_usage_step_removals", "copilot_model_catalog", "dashboard_prefs", "agent_notify_prefs", "dashboard_session_grace"}, dispositionRule{Rebuild, ConversionNotApplicable, "derived_or_presentation_state_rebuilt"},
)

func rules(groups ...any) map[string]dispositionRule {
	out := map[string]dispositionRule{}
	for i := 0; i < len(groups); i += 2 {
		for _, table := range groups[i].([]string) {
			out[table] = groups[i+1].(dispositionRule)
		}
	}
	out["schema_version"] = dispositionRule{Preserve, ConversionNotApplicable, "source_version_evidence"}
	return out
}

type identitySpec struct {
	table, key, kind, prefix string
	retain                   bool
}

var identitySpecs = []identitySpec{
	{"agents", "agent_id", "agent", "agt", true},
	{"agent_groups", "id", "group", "grp", false},
	{"roles", "id", "role", "role", false},
	{"spawn_profiles", "id", "spawn_profile", "sp", false},
	{"sandbox_profiles", "id", "sandbox_profile", "sb", false},
	{"group_templates", "id", "definition", "def", false},
	{"logical_conversations", "id", "conversation", "cvn", true},
	{"agent_messages", "id", "message", "msg", false},
	{"human_messages", "id", "human_message", "hmsg", false},
	{"process_snippets", "id", "snippet", "snip", true},
	{"process_runs", "id", "work_run", "work", true},
	{"agent_cron_jobs", "id", "automation_rule", "rule", false},
	{"trigger_rules", "id", "automation_rule", "rule", false},
	{"agent_standing_orders", "id", "automation_rule", "rule", false},
	{"execution_operations", "id", "operation", "op", true},
}

type referenceSpec struct {
	table, field, targetTable, kind string
	allowEmpty                      bool
}

var referenceSpecs = []referenceSpec{
	{"agents", "current_conv_id", "agent_conversations", "conversation", false},
	{"agent_conversations", "agent_id", "agents", "agent", false},
	{"agent_group_members", "group_id", "agent_groups", "group", false},
	{"agent_group_members", "agent_id", "agents", "agent", false},
	{"agent_group_owners", "group_id", "agent_groups", "group", false},
	{"agent_group_owners", "agent_id", "agents", "agent", false},
	{"agent_permissions", "agent_id", "agents", "agent", false},
	{"agent_group_permissions", "group_id", "agent_groups", "group", false},
	{"agent_sudo_grants", "agent_id", "agents", "agent", false},
	{"agent_groups", "parent_id", "agent_groups", "group", true},
	{"agent_groups", "source_template_id", "group_templates", "definition", true},
	{"agent_groups", "default_profile_id", "spawn_profiles", "spawn_profile", true},
	{"roles", "spawn_profile_id", "spawn_profiles", "spawn_profile", true},
	{"conversation_attempt_bindings", "conversation_id", "logical_conversations", "conversation", false},
	{"conversation_reference_bindings", "conversation_id", "logical_conversations", "conversation", false},
	{"agent_messages", "from_agent", "agents", "agent", true},
	{"agent_messages", "to_agent", "agents", "agent", true},
	{"agent_messages", "from_conv", "agent_conversations", "conversation", true},
	{"agent_messages", "to_conv", "agent_conversations", "conversation", true},
	{"human_messages", "from_agent", "agents", "agent", true},
	{"human_messages", "from_conv", "agent_conversations", "conversation", true},
	{"agent_workspace", "agent_id", "agents", "agent", true},
	{"agent_workspace", "conv_id", "agent_conversations", "conversation", true},
	{"agent_message_attachments", "message_id", "agent_messages", "message", false},
	{"group_template_agents", "template_id", "group_templates", "definition", false},
	{"group_template_agents", "spawn_profile_id", "spawn_profiles", "spawn_profile", true},
	{"spawn_profile_aliases", "profile_id", "spawn_profiles", "spawn_profile", false},
	{"process_run_events", "run_id", "process_runs", "work_run", false},
}

var stableID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

func Plan(inspection Inspection) (MigrationPlan, error) {
	if inspection.Source.DatabaseHash == "" {
		return MigrationPlan{}, errors.New("inspection has no source database hash")
	}
	plan := MigrationPlan{FormatVersion: PlanFormatVersion, Source: inspection.Source, PreflightValid: inspection.Valid, Diagnostics: append([]Diagnostic(nil), inspection.Diagnostics...)}

	names := make([]string, 0, len(inspection.Counts))
	for table := range inspection.Counts {
		names = append(names, table)
	}
	sort.Strings(names)
	for _, table := range names {
		rule, ok := tableRules[table]
		if !ok {
			rule = dispositionRule{Quarantine, ConversionPending, "unclassified_source_table"}
			plan.Diagnostics = append(plan.Diagnostics, Diagnostic{Severity: SeverityBlocking, Code: "unclassified_source_table", Table: table, Count: inspection.Counts[table], Detail: "source table has no reviewed preservation rule"})
		}
		plan.Dispositions = append(plan.Dispositions, TableDisposition{Table: table, Rows: inspection.Counts[table], Classification: rule.class, Conversion: rule.conversion, ReasonCode: rule.reason})
	}

	lookup := map[string]string{}
	used := map[string]string{}
	for _, spec := range identitySpecs {
		for _, row := range inspection.Snapshot.Rows[spec.table] {
			sourceKey := sourcev228.String(row.Values[spec.key])
			if sourceKey == "" {
				plan.Diagnostics = append(plan.Diagnostics, Diagnostic{Severity: SeverityBlocking, Code: "missing_identity_key", Table: spec.table, Count: 1, Detail: "identity row has an empty source key"})
				continue
			}
			target, retained := sourceKey, spec.retain && stableID.MatchString(sourceKey)
			if !retained {
				target = deterministicID(spec.prefix, inspection.Source.DatabaseHash, spec.table, sourceKey)
			}
			usedKey := spec.kind + "\x1f" + target
			if prior, exists := used[usedKey]; exists && prior != spec.table+"\x1f"+sourceKey {
				plan.Diagnostics = append(plan.Diagnostics, Diagnostic{Severity: SeverityBlocking, Code: "target_identity_collision", Table: spec.table, Count: 1, Detail: "two source identities map to the same target identity"})
				continue
			}
			used[usedKey], lookup[spec.table+"\x1f"+sourceKey] = spec.table+"\x1f"+sourceKey, target
			plan.Identities = append(plan.Identities, IdentityMapping{SourceTable: spec.table, SourceKey: sourceKey, TargetKind: spec.kind, TargetID: target, Retained: retained})
		}
	}
	mapLegacyConversations(&plan, inspection.Snapshot, inspection.Source.DatabaseHash, lookup)
	sort.Slice(plan.Identities, func(i, j int) bool {
		a, b := plan.Identities[i], plan.Identities[j]
		if a.TargetKind != b.TargetKind {
			return a.TargetKind < b.TargetKind
		}
		if a.SourceTable != b.SourceTable {
			return a.SourceTable < b.SourceTable
		}
		return a.SourceKey < b.SourceKey
	})

	for _, spec := range referenceSpecs {
		for _, row := range inspection.Snapshot.Rows[spec.table] {
			value := sourcev228.String(row.Values[spec.field])
			if value == "" && spec.allowEmpty {
				continue
			}
			target, ok := lookup[spec.targetTable+"\x1f"+value]
			plan.References = append(plan.References, ReferenceMapping{SourceTable: spec.table, SourceKey: row.Key, Field: spec.field, TargetKind: spec.kind, TargetID: target, Resolved: ok})
			if !ok {
				plan.Diagnostics = append(plan.Diagnostics, Diagnostic{Severity: SeverityBlocking, Code: "broken_dependency_reference", Table: spec.table, Count: 1, Detail: "source dependency does not resolve to a planned target identity"})
			}
		}
	}
	sort.Slice(plan.References, func(i, j int) bool {
		a, b := plan.References[i], plan.References[j]
		if a.SourceTable != b.SourceTable {
			return a.SourceTable < b.SourceTable
		}
		if a.SourceKey != b.SourceKey {
			return a.SourceKey < b.SourceKey
		}
		return a.Field < b.Field
	})

	appendMeaningDiagnostics(&plan, inspection.Snapshot)
	sortDiagnostics(plan.Diagnostics)
	plan.PreflightValid = plan.PreflightValid && !hasBlocking(plan.Diagnostics)
	// The preflight component preserves and diagnoses rows, but target schemas
	// and exact policy compilers are intentionally not part of this package.
	plan.ExecutableConversion = false
	encoded, err := json.Marshal(plan)
	if err != nil {
		return MigrationPlan{}, fmt.Errorf("encode deterministic plan: %w", err)
	}
	plan.PlanHash = digest(encoded)
	return plan, nil
}

func mapLegacyConversations(plan *MigrationPlan, snapshot sourcev228.Snapshot, databaseHash string, lookup map[string]string) {
	byReference := map[string]map[string]bool{}
	for _, table := range []string{"conversation_attempt_bindings", "conversation_reference_bindings"} {
		for _, row := range snapshot.Rows[table] {
			reference := sourcev228.String(row.Values["external_ref"])
			logical := sourcev228.String(row.Values["conversation_id"])
			if reference == "" || logical == "" {
				continue
			}
			if byReference[reference] == nil {
				byReference[reference] = map[string]bool{}
			}
			byReference[reference][logical] = true
		}
	}
	for _, row := range snapshot.Rows["agent_conversations"] {
		reference := sourcev228.String(row.Values["conv_id"])
		if reference == "" {
			continue
		}
		var target string
		if candidates := byReference[reference]; len(candidates) == 1 {
			for logical := range candidates {
				target = lookup["logical_conversations\x1f"+logical]
			}
		} else if len(candidates) > 1 {
			plan.Diagnostics = append(plan.Diagnostics, Diagnostic{Severity: SeverityBlocking, Code: "ambiguous_conversation_reference", Table: "agent_conversations", Count: 1, Detail: "native conversation reference maps to multiple logical conversations"})
		}
		if target == "" {
			target = deterministicID("cvn", databaseHash, "agent_conversations", reference)
		}
		lookup["agent_conversations\x1f"+reference] = target
		plan.Identities = append(plan.Identities, IdentityMapping{SourceTable: "agent_conversations", SourceKey: reference, TargetKind: "conversation", TargetID: target, Retained: false})
	}
}

func appendMeaningDiagnostics(plan *MigrationPlan, snapshot sourcev228.Snapshot) {
	authority := int64(len(snapshot.Rows["agent_permissions"]) + len(snapshot.Rows["agent_group_permissions"]) + len(snapshot.Rows["agent_sudo_grants"]) + len(snapshot.Rows["agent_group_owners"]))
	var config struct {
		Agent struct {
			DefaultPermissions []string `json:"default_permissions"`
		} `json:"agent"`
	}
	if len(snapshot.Config) > 0 && json.Unmarshal(snapshot.Config, &config) == nil {
		authority += int64(len(config.Agent.DefaultPermissions))
	}
	if authority > 0 {
		plan.Diagnostics = append(plan.Diagnostics, Diagnostic{Severity: SeverityWarning, Code: "authority_preserved_inactive", Count: authority, Detail: "legacy ownership and grants require exact deny-aware target conversion"})
	}
	var denies int64
	for _, row := range snapshot.Rows["agent_permissions"] {
		if sourcev228.String(row.Values["effect"]) == "deny" {
			denies++
		}
	}
	if denies > 0 {
		plan.Diagnostics = append(plan.Diagnostics, Diagnostic{Severity: SeverityWarning, Code: "legacy_deny_preserved", Table: "agent_permissions", Count: denies, Detail: "explicit denies block activation until target precedence is proven exact"})
	}
	for _, table := range []string{"agent_cron_jobs", "trigger_rules", "agent_standing_orders"} {
		if count := int64(len(snapshot.Rows[table])); count > 0 {
			plan.Diagnostics = append(plan.Diagnostics, Diagnostic{Severity: SeverityWarning, Code: "automation_imported_disabled", Table: table, Count: count, Detail: "authored automation is preserved but cannot be enabled by import"})
		}
	}
	var uncertain int64
	for _, row := range snapshot.Rows["execution_operations"] {
		state := strings.ToLower(sourcev228.String(row.Values["state"]))
		if state != "ready" && state != "failed" && state != "exited" && state != "succeeded" {
			uncertain++
		}
	}
	for _, row := range snapshot.Rows["process_runs"] {
		state := strings.ToLower(sourcev228.String(row.Values["status"]))
		if state != "succeeded" && state != "failed" && state != "cancelled" && state != "completed" {
			uncertain++
		}
	}
	if uncertain > 0 {
		plan.Diagnostics = append(plan.Diagnostics, Diagnostic{Severity: SeverityWarning, Code: "operation_interrupted_unresolved", Count: uncertain, Detail: "nonterminal legacy effects are preserved as uncertain and are never replayed"})
	}
	validateOrchestrationShape(plan, snapshot)
}

func validateOrchestrationShape(plan *MigrationPlan, snapshot sourcev228.Snapshot) {
	seen := map[string]string{}
	for _, row := range snapshot.Rows["process_runs"] {
		id := sourcev228.String(row.Values["id"])
		if !stableID.MatchString(id) {
			plan.Diagnostics = append(plan.Diagnostics, Diagnostic{Severity: SeverityWarning, Code: "malformed_graph_identity", Table: "process_runs", Count: 1, Detail: "legacy run identity requires deterministic remapping before graph conversion"})
		}
		payload := sourcev228.String(row.Values["template_snapshot_json"])
		sum := sha256.Sum256([]byte(payload))
		hash := hex.EncodeToString(sum[:])
		if prior, ok := seen[id]; ok && prior != hash {
			plan.Diagnostics = append(plan.Diagnostics, Diagnostic{Severity: SeverityBlocking, Code: "revision_hash_disagreement", Table: "process_runs", Count: 1, Detail: "duplicate revision identity has different authored content"})
		}
		seen[id] = hash
		if strings.Contains(payload, `"performer"`) && !strings.Contains(payload, `"profile"`) {
			plan.Diagnostics = append(plan.Diagnostics, Diagnostic{Severity: SeverityWarning, Code: "missing_performer_profile_revision", Table: "process_runs", Count: 1, Detail: "pinned performer profile revision is not provable from the legacy snapshot"})
		}
	}
	// Multiple action rows for one firing/action index are ambiguous issuance
	// correlation even if an old database omitted an enforcing unique index.
	correlations := map[string]bool{}
	for _, row := range snapshot.Rows["trigger_action_outcomes"] {
		key := fmt.Sprint(row.Values["firing_id"], "\x1f", row.Values["action_index"])
		if correlations[key] {
			plan.Diagnostics = append(plan.Diagnostics, Diagnostic{Severity: SeverityBlocking, Code: "ambiguous_occurrence_issuance", Table: "trigger_action_outcomes", Count: 1, Detail: "one occurrence action has multiple legacy outcome correlations"})
		}
		correlations[key] = true
	}
}

func deterministicID(prefix, databaseHash, table, key string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("migration-plan-v%d\x00%s\x00%s\x00%s", PlanFormatVersion, databaseHash, table, key)))
	return prefix + "_" + hex.EncodeToString(sum[:16])
}
