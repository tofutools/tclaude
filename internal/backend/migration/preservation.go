package migration

import (
	"encoding/json"
	"fmt"
	"strings"

	sourcev228 "github.com/tofutools/tclaude/internal/backend/migration/source/v228"
)

// These edges are historical references, not permission to activate their
// owners. Empty/zero legacy sentinels carry no reference.
func init() {
	referenceSpecs = append(referenceSpecs, []referenceSpec{
		{"human_message_attachments", "message_id", "human_messages", "human_message", false},
		{"agent_lineage", "child_agent_id", "agents", "agent", false},
		{"agent_lineage", "parent_agent_id", "agents", "agent", false},
		{"agent_clone_history", "source_agent_id", "agents", "agent", false},

		{"agent_spawn_history", "spawner_agent_id", "agents", "agent", false},

		{"agent_conv_succession", "old_conv_id", "agent_conversations", "conversation", false},
		{"agent_conv_succession", "new_conv_id", "agent_conversations", "conversation", false},
		{"agent_conv_succession", "agent_id", "agents", "agent", true},
		{"agent_messages", "parent_id", "agent_messages", "message", true},
		{"agent_messages", "original_to_conv", "agent_conversations", "conversation", true},
		{"agent_messages", "group_id", "agent_groups", "group", true},
		{"human_messages", "process_run_id", "process_runs", "work_run", true},
		{"execution_operations", "agent_id", "agents", "agent", true},
		{"execution_operations", "recovery_agent_id", "agents", "agent", true},
		{"execution_operations", "conv_id", "agent_conversations", "conversation", true},
		{"execution_operations", "logical_conversation_id", "logical_conversations", "conversation", true},
		{"agent_cron_jobs", "owner_agent", "agents", "agent", true},
		{"agent_cron_jobs", "target_agent", "agents", "agent", true},
		{"agent_cron_jobs", "group_id", "agent_groups", "group", true},
		{"trigger_rules", "owner_agent", "agents", "agent", true},
		{"trigger_rules", "group_id", "agent_groups", "group", true},
		{"agent_standing_orders", "owner_agent", "agents", "agent", true},
		{"agent_standing_orders", "target_agent", "agents", "agent", true},
		{"audit_log", "actor_agent", "agents", "agent", true},
		{"audit_log", "target_agent", "agents", "agent", true},
		{"audit_log", "actor_conv", "agent_conversations", "conversation", true},
		{"audit_log", "target_conv", "agent_conversations", "conversation", true},
		{"agent_standing_orders", "group_id", "agent_groups", "group", true},
		{"agent_head_aliases", "anchor_conv_id", "agent_conversations", "conversation", false},
		{"agent_head_aliases", "anchor_agent_id", "agents", "agent", true},
		{"agent_workdir", "conv_id", "agent_conversations", "conversation", false},
		{"conv_branch_history", "conv_id", "agent_conversations", "conversation", false},
		{"conversation_resume_profiles", "conv_id", "agent_conversations", "conversation", false},
		{"agent_cron_runs", "job_id", "agent_cron_jobs", "automation_rule", false},
		{"agent_standing_order_group_scopes", "order_id", "agent_standing_orders", "automation_rule", false},
		{"agent_standing_order_group_scopes", "group_id", "agent_groups", "group", false},
		{"agent_standing_order_hook_selectors", "order_id", "agent_standing_orders", "automation_rule", false},
	}...)
}

func validConfig(data []byte) bool {
	var config struct {
		Agent struct {
			DefaultPermissions []string `json:"default_permissions"`
		} `json:"agent"`
	}
	return len(data) > 0 && strings.HasPrefix(strings.TrimSpace(string(data)), "{") && json.Unmarshal(data, &config) == nil
}

func appendPreservationChecks(plan *MigrationPlan, snapshot sourcev228.Snapshot, lookup map[string]string) {
	add := func(code, table, detail string) {
		plan.Diagnostics = append(plan.Diagnostics, Diagnostic{Severity: SeverityBlocking, Code: code, Table: table, Count: 1, Detail: detail})
	}
	owners := map[string]string{}
	for _, row := range snapshot.Rows["agent_conversations"] {
		owners[sourcev228.String(row.Values["conv_id"])] = sourcev228.String(row.Values["agent_id"])
	}
	for _, row := range snapshot.Rows["agents"] {
		conv := sourcev228.String(row.Values["current_conv_id"])
		if owner := owners[conv]; owner != "" && owner != sourcev228.String(row.Values["agent_id"]) {
			add("current_association_owner_mismatch", "agents", "current association belongs to a different actor")
		}
	}
	if len(snapshot.Config) > 0 && !validConfig(snapshot.Config) {
		add("invalid_authored_config", "", "authored config has invalid field types")
	}
	// The two global preference keys are authored selection, not UI state.
	var selected string
	for _, row := range snapshot.Rows["dashboard_prefs"] {
		key, value := sourcev228.String(row.Values["key"]), sourcev228.String(row.Values["value"])
		if value == "" || value == "0" {
			continue
		}
		id := value
		if key == "tclaude.dash.default_profile" {
			id = ""
			for _, profile := range snapshot.Rows["spawn_profiles"] {
				if sourcev228.String(profile.Values["name"]) == value {
					id = sourcev228.String(profile.Values["id"])
				}
			}
		}
		target := lookup["spawn_profiles\x1f"+id]
		plan.References = append(plan.References, ReferenceMapping{SourceTable: "dashboard_prefs", SourceKey: key, Field: "value", TargetKind: "spawn_profile", TargetID: target, Resolved: target != ""})
		if target == "" {
			add("unresolved_global_profile", "dashboard_prefs", "authored global profile selection does not resolve")
		}
		if selected != "" && target != "" && selected != target {
			add("conflicting_global_profile", "dashboard_prefs", "global profile name and ID select different profiles")
		}
		if target != "" {
			selected = target
		}
	}
	for _, row := range snapshot.Rows["agent_messages"] {
		for _, spec := range []struct{ field, table, kind string }{
			{"to_recipients", "agent_conversations", "conversation"}, {"cc_recipients", "agent_conversations", "conversation"},
			{"to_recipient_agents", "agents", "agent"}, {"cc_recipient_agents", "agents", "agent"},
		} {
			raw := sourcev228.String(row.Values[spec.field])
			if raw == "" {
				continue
			}
			var recipients []string
			if json.Unmarshal([]byte(raw), &recipients) != nil {
				add("malformed_message_audience", "agent_messages", "message audience must be a JSON array of identity strings")
				continue
			}
			for index, value := range recipients {
				target := lookup[spec.table+"\x1f"+strings.TrimSpace(value)]
				plan.References = append(plan.References, ReferenceMapping{SourceTable: "agent_messages", SourceKey: row.Key, Field: fmt.Sprintf("%s[%d]", spec.field, index), TargetKind: spec.kind, TargetID: target, Resolved: target != ""})
				if target == "" {
					add("broken_dependency_reference", "agent_messages", "message audience reference does not resolve")
				}
			}
		}
	}
	var terminal int64
	for _, row := range snapshot.Rows["execution_operations"] {
		switch sourcev228.String(row.Values["state"]) {
		case "failed", "rejected", "cancelled", "exited", "succeeded":
			terminal++
		}
	}
	if terminal > 0 {
		plan.Diagnostics = append(plan.Diagnostics, Diagnostic{Severity: SeverityInfo, Code: "terminal_operation_history", Table: "execution_operations", Count: terminal, Detail: "terminal outcomes are retained as history without replay"})
	}
}

func terminalOperation(state string) bool {
	switch state {
	case "failed", "rejected", "cancelled", "exited", "succeeded":
		return true
	}
	return false
}
