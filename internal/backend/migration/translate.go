package migration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tofutools/tclaude/internal/backend/app"
	sourcev228 "github.com/tofutools/tclaude/internal/backend/migration/source/v228"
	"github.com/tofutools/tclaude/internal/backend/model"
)

const ImporterFormatVersion = 2
const TargetSchemaVersion = 1

type AttachmentPayload struct {
	SourceTable string
	SourceID    string
	SHA256      string
	Data        []byte
}

type TranslationOptions struct {
	MetadataOnlyAttachments bool
	CompletedAt             time.Time
}

// Translate converts a reviewed v228 plan into real replacement entities and
// immutable retained source evidence. It is pure: payload bytes are supplied
// by the bundle reader and it performs no source or target I/O.
func Translate(inspection Inspection, plan MigrationPlan, attachments []AttachmentPayload, options TranslationOptions) (app.ImportBatch, error) {
	if !conversionAllowed(plan, options.MetadataOnlyAttachments) {
		return app.ImportBatch{}, fmt.Errorf("offline import preflight has blocking diagnostics")
	}
	if plan.Source.DatabaseHash != inspection.Source.DatabaseHash || plan.PlanHash == "" {
		return app.ImportBatch{}, fmt.Errorf("migration plan does not match inspection")
	}
	completedAt := options.CompletedAt.UTC()
	if completedAt.IsZero() {
		completedAt = time.Now().UTC()
	}
	t := translator{
		inspection:   inspection,
		plan:         plan,
		ids:          make(map[string]string),
		payloads:     make(map[string]AttachmentPayload),
		profileNames: make(map[string]model.ConfigurationProfileRef),
		profileIDs:   make(map[string]model.ConfigurationProfileRef),
		revisionIDs:  make(map[string]string),
		options:      options,
	}
	for _, mapping := range plan.Identities {
		if strings.HasSuffix(mapping.TargetKind, "_revision") {
			t.revisionIDs[mapping.SourceTable+"\x1f"+mapping.SourceKey] = mapping.TargetID
		} else {
			t.ids[mapping.SourceTable+"\x1f"+mapping.SourceKey] = mapping.TargetID
		}
	}
	for _, payload := range attachments {
		t.payloads[payload.SourceTable+"\x1f"+payload.SourceID] = payload
	}
	batch := app.ImportBatch{}
	if err := t.translateEvidence(&batch); err != nil {
		return app.ImportBatch{}, err
	}
	t.translateProfiles(&batch)
	t.translateSandboxProfiles(&batch)
	t.translateAgents(&batch)
	if err := t.translateGroups(&batch); err != nil {
		return app.ImportBatch{}, err
	}
	if err := t.translateSandboxDefaults(&batch); err != nil {
		return app.ImportBatch{}, err
	}
	t.translateConversations(&batch)
	t.translateAuthoredOrchestration(&batch)
	t.translateHistoryAndWorkspaces(&batch)
	if err := t.translateMessages(&batch); err != nil {
		return app.ImportBatch{}, err
	}
	t.translateUsageAndActivity(&batch)
	batch.Receipt = model.ImportReceipt{
		ID:                      stableImportID("imp", inspection.Source.DatabaseHash, plan.PlanHash),
		SourceSchemaVersion:     inspection.Source.SchemaVersion,
		SourceDatabaseSHA256:    inspection.Source.DatabaseHash,
		ManifestSHA256:          inspection.Source.ManifestHash,
		ImporterFormatVersion:   ImporterFormatVersion,
		PlanFormatVersion:       plan.FormatVersion,
		TargetSchemaVersion:     TargetSchemaVersion,
		PlanSHA256:              plan.PlanHash,
		MetadataOnlyAttachments: options.MetadataOnlyAttachments,
		Counts:                  importCounts(batch),
		CompletedAt:             completedAt,
	}
	semantic, err := semanticDigest(batch)
	if err != nil {
		return app.ImportBatch{}, err
	}
	batch.Receipt.SemanticSHA256 = semantic
	return batch, nil
}

type translator struct {
	inspection   Inspection
	plan         MigrationPlan
	ids          map[string]string
	payloads     map[string]AttachmentPayload
	profileNames map[string]model.ConfigurationProfileRef
	profileIDs   map[string]model.ConfigurationProfileRef
	revisionIDs  map[string]string
	options      TranslationOptions
}

func conversionAllowed(plan MigrationPlan, metadataOnly bool) bool {
	for _, diagnostic := range plan.Diagnostics {
		if diagnostic.Severity != SeverityBlocking {
			continue
		}
		if metadataOnly && diagnostic.Code == "attachment_manifest_missing" {
			continue
		}
		return false
	}
	return true
}

func (t *translator) translateEvidence(batch *app.ImportBatch) error {
	disposition := map[string]TableDisposition{}
	for _, value := range t.plan.Dispositions {
		if _, ok := disposition[value.Table]; !ok || value.Conversion == ConversionInterrupted {
			disposition[value.Table] = value
		}
	}
	tables := make([]string, 0, len(t.inspection.Snapshot.Rows))
	for table := range t.inspection.Snapshot.Rows {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		rows := append([]sourcev228.Row(nil), t.inspection.Snapshot.Rows[table]...)
		sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
		for _, row := range rows {
			// JSON replaces malformed UTF-8 in strings. Refuse conversion rather
			// than publish evidence that silently changes source text.
			if !utf8.ValidString(row.Key) {
				return fmt.Errorf("source text is not valid UTF-8; offline conversion refused")
			}
			for key, value := range row.Values {
				if !utf8.ValidString(key) {
					return fmt.Errorf("source text is not valid UTF-8; offline conversion refused")
				}
				if text, ok := value.(string); ok && !utf8.ValidString(text) {
					return fmt.Errorf("source text is not valid UTF-8; offline conversion refused")
				}
			}
			payload, err := json.Marshal(row.Values)
			if err != nil {
				return fmt.Errorf("source evidence cannot be encoded; offline conversion refused")
			}
			d := disposition[table]
			conversion := d.Conversion
			reason := d.ReasonCode
			if table == "execution_operations" && !terminalOperation(strings.ToLower(sourcev228.String(row.Values["state"]))) {
				conversion, reason = ConversionInterrupted, "unresolved_effect_without_replay"
			}
			if table == "process_runs" && !legacyTerminal(strings.ToLower(sourcev228.String(row.Values["status"]))) {
				conversion, reason = ConversionInterrupted, "unresolved_effect_without_replay"
			}
			batch.SourceRecords = append(batch.SourceRecords, model.ImportedSourceRecord{
				SourceTable: table, SourceKey: row.Key, SourcePath: sourcePath(table, row.Key),
				Class: string(d.Classification), Conversion: string(conversion), ReasonCode: reason,
				Payload: payload, PayloadSHA256: digest(payload),
			})
		}
	}
	if payload, ok := inactiveDefaultPermissions(t.inspection.Snapshot.Config); ok {
		batch.SourceRecords = append(batch.SourceRecords, model.ImportedSourceRecord{
			SourceTable: "authored_config", SourceKey: "agent.default_permissions", SourcePath: "config.agent.default_permissions",
			Class: string(Inactive), Conversion: string(ConversionReady), ReasonCode: "authority_preserved_inactive",
			Payload: append([]byte(nil), payload...), PayloadSHA256: digest(payload),
		})
		batch.Diagnostics = append(batch.Diagnostics, model.ImportedDiagnostic{
			Severity: string(SeverityWarning), Code: "authority_preserved_inactive", SourceTable: "authored_config",
			SourceKey: "agent.default_permissions", SourcePath: "config.agent.default_permissions",
			Detail: "legacy default permissions are retained as inactive authored evidence",
		})
	}
	if hasUnsupportedAuthoredConfig(t.inspection.Snapshot.Config) {
		batch.Diagnostics = append(batch.Diagnostics, model.ImportedDiagnostic{
			Severity: string(SeverityWarning), Code: "authored_config_fields_excluded", SourceTable: "authored_config",
			SourcePath: "config", Detail: "authored fields outside supported inactive configuration were excluded from retained values",
		})
	}
	for _, mapping := range t.plan.Identities {
		batch.IDMappings = append(batch.IDMappings, model.ImportIDMapping{SourceNamespace: "tclaude-v228", SourceTable: mapping.SourceTable, SourceKey: mapping.SourceKey, TargetKind: mapping.TargetKind, TargetID: mapping.TargetID})
	}
	for _, diagnostic := range t.plan.Diagnostics {
		batch.Diagnostics = append(batch.Diagnostics, model.ImportedDiagnostic{Severity: string(diagnostic.Severity), Code: diagnostic.Code, SourceTable: diagnostic.Table, SourcePath: sourcePath(diagnostic.Table, "*"), Detail: diagnostic.Detail})
	}
	sort.Slice(batch.SourceRecords, func(i, j int) bool {
		if batch.SourceRecords[i].SourceTable != batch.SourceRecords[j].SourceTable {
			return batch.SourceRecords[i].SourceTable < batch.SourceRecords[j].SourceTable
		}
		return batch.SourceRecords[i].SourceKey < batch.SourceRecords[j].SourceKey
	})
	sort.Slice(batch.IDMappings, func(i, j int) bool {
		a, b := batch.IDMappings[i], batch.IDMappings[j]
		if a.SourceNamespace != b.SourceNamespace {
			return a.SourceNamespace < b.SourceNamespace
		}
		if a.SourceTable != b.SourceTable {
			return a.SourceTable < b.SourceTable
		}
		if a.SourceKey != b.SourceKey {
			return a.SourceKey < b.SourceKey
		}
		return a.TargetKind < b.TargetKind
	})
	return nil
}

func (t *translator) translateAgents(batch *app.ImportBatch) {
	titles := map[string]sourcev228.Row{}
	for _, row := range t.inspection.Snapshot.Rows["conv_index"] {
		titles[sourcev228.String(row.Values["conv_id"])] = row
	}
	for _, row := range t.inspection.Snapshot.Rows["agents"] {
		id := model.AgentID(t.id("agents", sourcev228.String(row.Values["agent_id"])))
		created := timeValue(row.Values["created_at"])
		updated := created
		current := sourcev228.String(row.Values["current_conv_id"])
		catalog := titles[current]
		name := firstNonEmpty(sourcev228.String(catalog.Values["custom_title"]), sourcev228.String(row.Values["pending_name"]), sourcev228.String(catalog.Values["summary"]), sourcev228.String(catalog.Values["first_prompt"]), string(id))
		retiredAt := optionalTime(row.Values["retired_at"])
		lifecycle := model.AgentActive
		if retiredAt != nil {
			lifecycle = model.AgentRetired
			updated = *retiredAt
		}
		agent := model.Agent{
			ID: id, Name: name, TaskReference: sourcev228.String(row.Values["task_ref_url"]), Lifecycle: lifecycle,
			RetiredAt: retiredAt, RetirementReason: sourcev228.String(row.Values["retire_reason"]),
			Notifications: model.AgentNotificationPreferences{DirectMessage: model.NotificationIfAvailable},
			Desired:       desiredFromRow(row.Values), Revision: 1, CreatedAt: created, UpdatedAt: updated,
		}
		agent.Desired.Environment = t.launchEnvironment(batch, "agents", row)
		if model.ValidateEffort(agent.Desired.Effort) != nil {
			t.launchMetadataDiagnostic(batch, "agents", row.Key, "requested_effort_requires_review", "requested native effort is preserved verbatim and requires correction before new effects")
		}
		profileName := firstNonEmpty(sourcev228.String(row.Values["relaunch_profile"]), spawnProfileName(row.Values["initial_spawn_config"]))
		if ref, ok := t.profileNames[profileName]; ok && profileName != "" {
			copy := ref
			agent.ConfigurationProfile = &copy
		}
		if parent := sourcev228.String(row.Values["parent_agent_id"]); parent != "" {
			agent.ParentAgentID = model.AgentID(t.id("agents", parent))
		}
		if source := sourcev228.String(row.Values["clone_source_agent_id"]); source != "" {
			agent.CloneSourceAgentID = model.AgentID(t.id("agents", source))
		}
		batch.Agents = append(batch.Agents, agent)
	}
	sort.Slice(batch.Agents, func(i, j int) bool { return batch.Agents[i].ID < batch.Agents[j].ID })
}

func (t *translator) translateGroups(batch *app.ImportBatch) error {
	type member struct {
		id     model.AgentID
		joined time.Time
		key    string
	}
	members := map[string][]member{}
	for _, row := range t.inspection.Snapshot.Rows["agent_group_members"] {
		groupKey := sourcev228.String(row.Values["group_id"])
		members[groupKey] = append(members[groupKey], member{id: model.AgentID(t.id("agents", sourcev228.String(row.Values["agent_id"]))), joined: timeValue(row.Values["joined_at"]), key: row.Key})
	}
	for _, row := range t.inspection.Snapshot.Rows["agent_groups"] {
		key := sourcev228.String(row.Values["id"])
		created := timeValue(row.Values["created_at"])
		items := members[key]
		sort.Slice(items, func(i, j int) bool {
			if !items[i].joined.Equal(items[j].joined) {
				return items[i].joined.Before(items[j].joined)
			}
			return items[i].key < items[j].key
		})
		group := model.Group{ID: model.GroupID(t.id("agent_groups", key)), Name: firstNonEmpty(sourcev228.String(row.Values["name"]), key), Revision: 1, CreatedAt: created, UpdatedAt: firstTime(timeValue(row.Values["archived_at"]), created)}
		if raw := row.Values["max_members"]; raw != nil {
			if cap, ok := sourcev228.Int64(raw); ok && model.ValidGroupCapacity(cap) {
				group.MaxActiveMembers = cap
			} else {
				t.launchMetadataDiagnostic(batch, "agent_groups", row.Key, "group_capacity_retained_unmapped", "unsupported legacy capacity remains in exact retained source evidence; no capacity activated")
			}
		}
		details := model.GroupDetails{Description: sourcev228.String(row.Values["descr"]), Mission: sourcev228.String(row.Values["mission"]), LinkURL: sourcev228.String(row.Values["attachment_url"]), LinkLabel: sourcev228.String(row.Values["attachment_label"])}
		supported := model.GroupDetails{}
		if model.ValidateGroupDetails(model.GroupDetails{Description: details.Description}) == nil {
			supported.Description = details.Description
		}
		if model.ValidateGroupDetails(model.GroupDetails{Mission: details.Mission}) == nil {
			supported.Mission = details.Mission
		}
		if model.ValidateGroupDetails(model.GroupDetails{LinkURL: details.LinkURL}) == nil {
			supported.LinkURL = details.LinkURL
		}
		if model.ValidateGroupDetails(model.GroupDetails{LinkURL: supported.LinkURL, LinkLabel: details.LinkLabel}) == nil {
			supported.LinkLabel = details.LinkLabel
		}
		if supported != (model.GroupDetails{}) {
			group.Details = &supported
		}
		if supported != details {
			t.launchMetadataDiagnostic(batch, "agent_groups", row.Key, "group_details_retained_unmapped", "unsupported group detail fields remain in the exact retained source record; supported fields were preserved")
		}

		if parent := sourcev228.String(row.Values["parent_id"]); parent != "" {
			group.ParentGroupID = model.GroupID(t.id("agent_groups", parent))
			if group.ParentGroupID == "" {
				return fmt.Errorf("import group parent identity is missing")
			}
		}
		for _, item := range items {
			group.Members = append(group.Members, item.id)
		}
		t.translateGroupConfiguration(batch, row, group)
		batch.Groups = append(batch.Groups, group)
	}
	sort.Slice(batch.GroupConfigurations, func(i, j int) bool {
		return batch.GroupConfigurations[i].GroupID < batch.GroupConfigurations[j].GroupID
	})
	sort.Slice(batch.Groups, func(i, j int) bool { return batch.Groups[i].ID < batch.Groups[j].ID })
	return model.ValidateGroupHierarchy(batch.Groups)
}

func (t *translator) translateConversations(batch *app.ImportBatch) {
	created := map[string]time.Time{}
	for _, row := range t.inspection.Snapshot.Rows["logical_conversations"] {
		created[t.id("logical_conversations", sourcev228.String(row.Values["id"]))] = timeValue(row.Values["created_at"])
	}
	for _, row := range t.inspection.Snapshot.Rows["conv_index"] {
		created[t.id("conv_index", sourcev228.String(row.Values["conv_id"]))] = timeValue(row.Values["created"])
	}
	seen := map[string]bool{}
	for _, mapping := range t.plan.Identities {
		if mapping.TargetKind != "conversation" || seen[mapping.TargetID] {
			continue
		}
		seen[mapping.TargetID] = true
		at := created[mapping.TargetID]
		batch.Conversations = append(batch.Conversations, model.Conversation{ID: model.ConversationID(mapping.TargetID), Revision: 1, CreatedAt: at, UpdatedAt: at})
	}
	current := map[string]string{}
	for _, row := range t.inspection.Snapshot.Rows["agents"] {
		current[sourcev228.String(row.Values["agent_id"])] = sourcev228.String(row.Values["current_conv_id"])
	}
	for _, row := range t.inspection.Snapshot.Rows["agent_conversations"] {
		agentKey, convKey := sourcev228.String(row.Values["agent_id"]), sourcev228.String(row.Values["conv_id"])
		at := timeValue(row.Values["linked_at"])
		batch.ConversationLinks = append(batch.ConversationLinks, model.ConversationAssociation{AgentID: model.AgentID(t.id("agents", agentKey)), ConversationID: model.ConversationID(t.id("agent_conversations", convKey)), Current: current[agentKey] == convKey, Revision: 1, AssociatedAt: at})
	}
	sort.Slice(batch.Conversations, func(i, j int) bool { return batch.Conversations[i].ID < batch.Conversations[j].ID })
	sort.Slice(batch.ConversationLinks, func(i, j int) bool {
		if batch.ConversationLinks[i].AgentID != batch.ConversationLinks[j].AgentID {
			return batch.ConversationLinks[i].AgentID < batch.ConversationLinks[j].AgentID
		}
		return batch.ConversationLinks[i].ConversationID < batch.ConversationLinks[j].ConversationID
	})
}

func (t *translator) translateProfiles(batch *app.ImportBatch) {
	for _, row := range t.inspection.Snapshot.Rows["spawn_profiles"] {
		key := sourcev228.String(row.Values["id"])
		id := model.ConfigurationProfileID(t.id("spawn_profiles", key))
		payload, _ := json.Marshal(row.Values)
		revisionID := model.ConfigurationProfileRevisionID(stableImportID("cpr", t.inspection.Source.DatabaseHash, "spawn_profiles\x00"+key+"\x00"+digest(payload)))
		at := timeValue(row.Values["created_at"])
		ref := model.ConfigurationProfileRef{ProfileID: id, RevisionID: revisionID, ContentHash: digest(payload)}
		t.profileNames[sourcev228.String(row.Values["name"])] = ref
		t.profileIDs[key] = ref
		startup := &model.ProfileStartup{AgentName: sourcev228.String(row.Values["agent_name"]), Context: sourcev228.String(row.Values["startup_context"]), InitialMessage: sourcev228.String(row.Values["initial_message"])}
		if *startup == (model.ProfileStartup{}) {
			startup = nil
		} else if model.ValidateProfileStartup(*startup) != nil {
			startup = nil
			t.launchMetadataDiagnostic(batch, "spawn_profiles", row.Key, "profile_startup_retained_unmapped", "startup suggestions exceed target text constraints; exact original fields remain in the source record")
		}
		disabled, validDisabled := sourcev228.Int64(row.Values["disabled"])
		archived := disabled != 0
		if row.Values["disabled"] != nil && !validDisabled {
			archived = true
			t.launchMetadataDiagnostic(batch, "spawn_profiles", row.Key, "profile_disabled_unrecognized", "unrecognized disabled state is retained as archived pending explicit operator review")
		}
		desired := desiredFromRow(row.Values)
		desired.Environment = t.launchEnvironment(batch, "spawn_profiles", row)
		if model.ValidateEffort(desired.Effort) != nil {
			t.launchMetadataDiagnostic(batch, "spawn_profiles", row.Key, "requested_effort_requires_review", "requested native effort is preserved verbatim and requires correction before new effects")
		}
		batch.ConfigurationProfiles = append(batch.ConfigurationProfiles, app.ConfigurationProfileResult{
			Profile:  model.ConfigurationProfile{Archived: archived, ID: id, Name: firstNonEmpty(sourcev228.String(row.Values["name"]), key), CurrentRevisionID: revisionID, Revision: 1, CreatedAt: at, UpdatedAt: at},
			Revision: model.ConfigurationProfileRevision{Ref: ref, Desired: desired, Startup: startup, CreatedAt: at},
		})
	}
	for _, row := range t.inspection.Snapshot.Rows["spawn_profile_aliases"] {
		if ref, ok := t.profileIDs[sourcev228.String(row.Values["profile_id"])]; ok {
			alias := sourcev228.String(row.Values["alias"])
			if _, named := t.profileNames[alias]; !named {
				t.profileNames[alias] = ref
			}
		}
	}
	// Preserve the namespace of the authored reference. Numeric profile names
	// remain names; only the stable-ID preference selects by source row ID.
globalDefault:
	for _, key := range []string{"tclaude.dash.default_profile_id", "tclaude.dash.default_profile"} {
		for _, row := range t.inspection.Snapshot.Rows["dashboard_prefs"] {
			if sourcev228.String(row.Values["key"]) != key {
				continue
			}
			value := sourcev228.String(row.Values["value"])
			// Preflight treats empty and zero stable IDs as absent.
			if key == "tclaude.dash.default_profile_id" && (value == "" || value == "0") {
				continue
			}
			ref, ok := t.profileIDs[value]
			if key == "tclaude.dash.default_profile" {
				ref, ok = t.profileNames[value]
			}
			if ok && value != "" && (key != "tclaude.dash.default_profile_id" || value != "0") {
				copy := ref
				batch.ConfigurationDefaults = &model.ConfigurationDefaults{Global: &copy, Harnesses: map[string]model.ConfigurationProfileRef{}, Revision: 1, UpdatedAt: timeValue(row.Values["updated_at"])}
			}
			// An explicit stable-ID selection never falls back to a coincidental name.
			break globalDefault
		}
	}
	sort.Slice(batch.ConfigurationProfiles, func(i, j int) bool {
		return batch.ConfigurationProfiles[i].Profile.ID < batch.ConfigurationProfiles[j].Profile.ID
	})
}

func (t *translator) launchMetadataDiagnostic(batch *app.ImportBatch, table, key, code, detail string) {
	batch.Diagnostics = append(batch.Diagnostics, model.ImportedDiagnostic{Severity: string(SeverityWarning), Code: code, SourceTable: table, SourceKey: key, SourcePath: sourcePath(table, key), Detail: detail})
}

func (t *translator) translateAuthoredOrchestration(batch *app.ImportBatch) {
	for _, row := range t.inspection.Snapshot.Rows["group_templates"] {
		key := sourcev228.String(row.Values["id"])
		id := model.DefinitionID(t.id("group_templates", key))
		payload := authoredEnvelope(row, t.inspection.Snapshot.Rows["group_template_agents"], "template_id", key)
		hash := digest(payload)
		revisionID := model.DefinitionRevisionID(t.revisionIDs["group_templates\x1f"+row.Key])
		at := timeValue(row.Values["created_at"])
		definition := model.Definition{ID: id, Name: firstNonEmpty(sourcev228.String(row.Values["name"]), key), Kind: model.DefinitionTeam, HeadRevisionID: revisionID, Revision: 1, CreatedAt: at, UpdatedAt: firstTime(timeValue(row.Values["updated_at"]), at)}
		revision := model.DefinitionRevision{ID: revisionID, DefinitionID: id, Number: 1, ContentHash: hash, SchemaVersion: SourceSchemaVersion, CompilerVersion: "legacy-v228-unsupported", Source: string(payload), Author: model.OperatorPrincipal(), RequestID: model.RequestID(stableImportID("req", t.inspection.Source.DatabaseHash, "definition\x00"+key)), CreatedAt: at}
		batch.Definitions = append(batch.Definitions, app.DefinitionRecord{Definition: definition, Head: revision})
	}
	for _, table := range []string{"agent_cron_jobs", "trigger_rules", "agent_standing_orders"} {
		for _, row := range t.inspection.Snapshot.Rows[table] {
			key := sourcev228.String(row.Values["id"])
			id := model.AutomationRuleID(t.id(table, key))
			payload, _ := json.Marshal(row.Values)
			hash := digest(payload)
			revisionID := model.AutomationRuleRevisionID(t.revisionIDs[table+"\x1f"+row.Key])
			at := timeValue(row.Values["created_at"])
			condition := model.AutomationCondition{}
			switch table {
			case "agent_cron_jobs":
				condition.Kind = model.AutomationSchedule
			case "trigger_rules":
				condition.Kind = model.AutomationTrigger
			case "agent_standing_orders":
				condition.Kind = model.AutomationStandingOrder
			}
			owner := model.AuthoritySubject{}
			if agentID := t.id("agents", sourcev228.String(row.Values["owner_agent"])); agentID != "" {
				owner = model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: model.AgentID(agentID)}
			}
			rule := model.AutomationRule{ID: id, Name: firstNonEmpty(sourcev228.String(row.Values["name"]), table+"-"+key), HeadRevisionID: revisionID, Enabled: false, Revision: 1, CreatedAt: at, UpdatedAt: firstTime(timeValue(row.Values["updated_at"]), at)}
			revision := model.AutomationRuleRevision{ID: revisionID, RuleID: id, Number: 1, ContentHash: hash, Owner: owner, Condition: condition, Author: model.OperatorPrincipal(), RequestID: model.RequestID(stableImportID("req", t.inspection.Source.DatabaseHash, "automation\x00"+table+"\x00"+key)), CreatedAt: at}
			batch.AutomationRules = append(batch.AutomationRules, app.AutomationRuleRecord{Rule: rule, Head: revision})
			batch.Diagnostics = append(batch.Diagnostics, model.ImportedDiagnostic{Severity: string(SeverityWarning), Code: "imported_automation_requires_revalidation", SourceTable: table, SourceKey: row.Key, SourcePath: sourcePath(table, row.Key), Detail: "authored rule is retained as a disabled non-runnable revision"})
		}
	}
	sort.Slice(batch.Definitions, func(i, j int) bool { return batch.Definitions[i].Definition.ID < batch.Definitions[j].Definition.ID })
	sort.Slice(batch.AutomationRules, func(i, j int) bool { return batch.AutomationRules[i].Rule.ID < batch.AutomationRules[j].Rule.ID })
}

func authoredEnvelope(parent sourcev228.Row, children []sourcev228.Row, foreignKey, parentKey string) []byte {
	selected := make([]map[string]any, 0)
	for _, child := range children {
		if sourcev228.String(child.Values[foreignKey]) == parentKey {
			selected = append(selected, child.Values)
		}
	}
	encoded, _ := json.Marshal(struct {
		Parent   map[string]any   `json:"parent"`
		Children []map[string]any `json:"children"`
	}{Parent: parent.Values, Children: selected})
	return encoded
}

func (t *translator) translateHistoryAndWorkspaces(batch *app.ImportBatch) {
	seenHistory := map[model.ConversationID]bool{}
	for _, row := range t.inspection.Snapshot.Rows["conv_index"] {
		id := model.ConversationID(t.id("conv_index", sourcev228.String(row.Values["conv_id"])))
		if seenHistory[id] {
			continue
		}
		seenHistory[id] = true
		modified := timeValue(row.Values["modified"])
		batch.History = append(batch.History, model.HistoryCatalogEntry{ConversationID: id, Harness: sourcev228.String(row.Values["harness"]), Title: firstNonEmpty(sourcev228.String(row.Values["custom_title"]), sourcev228.String(row.Values["summary"]), sourcev228.String(row.Values["first_prompt"])), WorkspaceHint: sourcev228.String(row.Values["project_path"]), Archived: optionalTime(row.Values["archived_at"]) != nil, Availability: model.HistoryMetadataOnly, Coverage: model.HistoryCoverage{Metadata: model.HistoryCoverageComplete, Content: model.HistoryCoverageUnknown, SourceRevision: digest([]byte(row.Key)), RefreshedAt: modified}, ModifiedAt: modified, Revision: 1})
	}
	seenWorkspace := map[model.WorkspaceID]bool{}
	for _, row := range t.inspection.Snapshot.Rows["agent_workspace"] {
		id := model.WorkspaceID(stableImportID("wsp", t.inspection.Source.DatabaseHash, row.Key))
		if seenWorkspace[id] {
			continue
		}
		seenWorkspace[id] = true
		at := timeValue(row.Values["updated_at"])
		batch.Workspaces = append(batch.Workspaces, model.Workspace{ID: id, Intent: model.WorkspaceIntent{Repository: sourcev228.String(row.Values["repo_url"]), IntendedPath: sourcev228.String(row.Values["cwd"]), Branch: sourcev228.String(row.Values["branch"]), Provenance: model.WorkspaceRegistered, Ownership: model.WorkspaceExternal}, State: model.WorkspaceUncertain, Revision: 1, CreatedAt: at, UpdatedAt: at})
	}
	sort.Slice(batch.History, func(i, j int) bool { return batch.History[i].ConversationID < batch.History[j].ConversationID })
	sort.Slice(batch.Workspaces, func(i, j int) bool { return batch.Workspaces[i].ID < batch.Workspaces[j].ID })
}

func (t *translator) translateMessages(batch *app.ImportBatch) error {
	operatorMessages := map[string]bool{}
	for _, row := range t.inspection.Snapshot.Rows["operator_agent_messages"] {
		operatorMessages[sourcev228.String(row.Values["message_id"])] = true
	}
	origins := map[string]string{}
	for _, spec := range []struct{ table, origin string }{{"agent_cron_messages", "cron"}, {"agent_standing_order_messages", "standing_order"}} {
		for _, row := range t.inspection.Snapshot.Rows[spec.table] {
			origins[sourcev228.String(row.Values["message_id"])] = spec.origin
		}
	}
	messageBySource := map[string]model.MessageID{}
	for _, row := range t.inspection.Snapshot.Rows["agent_messages"] {
		messageBySource["agent_messages\x1f"+sourcev228.String(row.Values["id"])] = model.MessageID(t.id("agent_messages", sourcev228.String(row.Values["id"])))
	}
	for _, row := range t.inspection.Snapshot.Rows["human_messages"] {
		messageBySource["human_messages\x1f"+sourcev228.String(row.Values["id"])] = model.MessageID(t.id("human_messages", sourcev228.String(row.Values["id"])))
	}
	for _, table := range []string{"agent_messages", "human_messages"} {
		for _, row := range t.inspection.Snapshot.Rows[table] {
			key := sourcev228.String(row.Values["id"])
			id := messageBySource[table+"\x1f"+key]
			senderID := model.AgentID(t.id("agents", sourcev228.String(row.Values["from_agent"])))
			sender := model.OperatorPrincipal()
			if senderID != "" && (table != "agent_messages" || !operatorMessages[key]) {
				sender = model.AgentPrincipal(senderID)
			}
			origin := ""
			if table == "agent_messages" {
				origin = origins[key]
			}
			message := model.Message{ID: id, Sender: sender, SenderConversationID: t.conversationID(sourcev228.String(row.Values["from_conv"])), Subject: firstNonEmpty(sourcev228.String(row.Values["subject"]), "Message"), Body: sourcev228.String(row.Values["body"]), CreatedAt: timeValue(row.Values["created_at"])}
			envelope := model.ImportedMessageEnvelope{MessageID: id, SourceTable: table, SourceKey: row.Key, OriginalParent: sourcev228.String(row.Values["parent_id"]), OriginalGroup: sourcev228.String(row.Values["group_id"]), DeliveredAt: optionalTime(row.Values["delivered_at"]), ReadAt: optionalTime(row.Values["read_at"]), ProcessedAt: optionalTime(row.Values["processed_at"]), NudgeAttemptedAt: optionalTime(firstNonNil(row.Values["nudge_sent_at"], row.Values["nudge_attempted_at"])), Origin: origin}
			if parent := sourcev228.String(row.Values["parent_id"]); parent != "" {
				message.ParentMessageID = messageBySource["agent_messages\x1f"+parent]
			}
			if table == "human_messages" {
				message.Recipients = append(message.Recipients, recipientFor(t.inspection.Source.DatabaseHash, id, model.MessageAddressOperator, "", model.MessageAudienceTo, row))
				envelope.Addresses = append(envelope.Addresses, model.ImportedMessageAddress{
					AddressKind:   model.MessageAddressOperator,
					Audience:      model.MessageAudienceTo,
					OriginalAgent: "operator",
					Resolved:      true,
				})
			} else {
				envelope.Addresses = append(envelope.Addresses, t.envelopeAddresses(row, model.MessageAudienceTo, "to_recipients", "to_recipient_agents", "to_conv", "to_agent")...)
				envelope.Addresses = append(envelope.Addresses, t.envelopeAddresses(row, model.MessageAudienceCC, "cc_recipients", "cc_recipient_agents", "", "")...)
				agentKey := sourcev228.String(row.Values["to_agent"])
				if agentKey == "" {
					agentKey = t.conversationOwner(sourcev228.String(row.Values["to_conv"]))
				}
				if target := model.AgentID(t.id("agents", agentKey)); target != "" {
					message.Recipients = append(message.Recipients, recipientFor(t.inspection.Source.DatabaseHash, id, model.MessageAddressAgent, target, canonicalAudience(row), row))
				}
			}
			batch.Messages = append(batch.Messages, message)
			batch.MessageEnvelopes = append(batch.MessageEnvelopes, envelope)
		}
	}
	root := map[model.MessageID]model.MessageID{}
	for i := range batch.Messages {
		root[batch.Messages[i].ID] = batch.Messages[i].ID
	}
	for range batch.Messages {
		for i := range batch.Messages {
			if parent := batch.Messages[i].ParentMessageID; parent != "" && root[parent] != "" {
				root[batch.Messages[i].ID] = root[parent]
			}
		}
	}
	for i := range batch.Messages {
		batch.Messages[i].ThreadID = root[batch.Messages[i].ID]
	}
	sort.Slice(batch.Messages, func(i, j int) bool { return batch.Messages[i].ID < batch.Messages[j].ID })
	for _, table := range []string{"agent_message_attachments", "human_message_attachments"} {
		for _, row := range t.inspection.Snapshot.Rows[table] {
			key := sourcev228.String(row.Values["id"])
			messageTable := "agent_messages"
			if table == "human_message_attachments" {
				messageTable = "human_messages"
			}
			messageID := messageBySource[messageTable+"\x1f"+sourcev228.String(row.Values["message_id"])]
			payload, available := t.payloads[table+"\x1f"+key]
			if !available && !t.options.MetadataOnlyAttachments {
				return fmt.Errorf("attachment %s has no verified payload", sourcePath(table, key))
			}
			size, _ := sourcev228.Int64(row.Values["size_bytes"])
			sha := payload.SHA256
			availability, loss := model.ImportedAttachmentAvailable, ""
			if !available {
				availability, loss = model.ImportedAttachmentMissing, "operator_selected_metadata_only_loss"
				sha = sourcev228.String(row.Values["sha256"])
			}
			position, _ := sourcev228.Int64(firstNonNil(row.Values["ordinal"], row.Values["seq"]))
			attachmentID := model.AttachmentID(stableImportID("att", t.inspection.Source.DatabaseHash, table+"\x00"+key))
			attachment := model.Attachment{ID: attachmentID, Filename: sourcev228.String(row.Values["filename"]), MediaType: firstNonEmpty(sourcev228.String(row.Values["content_type"]), "application/octet-stream"), Size: size, SHA256: sha, CreatedAt: messageCreatedAt(batch.Messages, messageID)}
			batch.ImportedAttachments = append(batch.ImportedAttachments, model.ImportedAttachment{AttachmentID: attachmentID, SourceTable: table, SourceKey: key, MessageID: messageID, Position: int(position), Availability: availability, LossReason: loss, Attachment: attachment, Content: append([]byte{}, payload.Data...)})
			for i := range batch.Messages {
				if batch.Messages[i].ID == messageID {
					batch.Messages[i].Attachments = append(batch.Messages[i].Attachments, attachment)
				}
			}
		}
	}
	sort.Slice(batch.ImportedAttachments, func(i, j int) bool {
		return batch.ImportedAttachments[i].AttachmentID < batch.ImportedAttachments[j].AttachmentID
	})
	return nil
}

func (t *translator) envelopeAddresses(row sourcev228.Row, audience model.MessageAudienceKind, conversationsField, agentsField, fallbackConversation, fallbackAgent string) []model.ImportedMessageAddress {
	conversations, agents := audienceAgents(row, conversationsField), audienceAgents(row, agentsField)
	if len(conversations) == 0 && fallbackConversation != "" {
		conversations = []string{sourcev228.String(row.Values[fallbackConversation])}
	}
	if len(agents) == 0 && fallbackAgent != "" {
		agents = []string{sourcev228.String(row.Values[fallbackAgent])}
	}
	count := len(conversations)
	if len(agents) > count {
		count = len(agents)
	}
	out := make([]model.ImportedMessageAddress, 0, count)
	for index := 0; index < count; index++ {
		var conversation, agent string
		if index < len(conversations) {
			conversation = conversations[index]
		}
		if index < len(agents) {
			agent = agents[index]
		}
		if agent == "" {
			agent = t.conversationOwner(conversation)
		}
		target := model.AgentID(t.id("agents", agent))
		out = append(out, model.ImportedMessageAddress{
			AddressKind:          model.MessageAddressAgent,
			Audience:             audience,
			OriginalConversation: conversation,
			OriginalAgent:        agent,
			TargetAgentID:        target,
			Resolved:             target != "",
		})
	}
	return out
}

func (t *translator) translateUsageAndActivity(batch *app.ImportBatch) {
	for _, row := range t.inspection.Snapshot.Rows["session_cost_daily"] {
		conversationID := t.conversationID(sourcev228.String(row.Values["conv_id"]))
		if conversationID == "" {
			continue
		}
		day, err := time.Parse("2006-01-02", sourcev228.String(row.Values["day"]))
		if err != nil {
			continue
		}
		appendCost := func(field, suffix string, kind model.UsageCostKind) {
			amount := exactDecimal(row.Values[field])
			if decimalZero(amount) {
				return
			}
			key := row.Key + suffix
			id := model.UsageObservationID(stableImportID("use", t.inspection.Source.DatabaseHash, key))
			batch.Usage = append(batch.Usage, app.HistoricalUsageWrite{Observation: model.UsageObservation{ID: id, Attribution: model.UsageAttribution{AgentID: model.AgentID(t.id("agents", sourcev228.String(row.Values["agent_id"]))), ConversationID: conversationID, Precision: model.UsageAttributionConversation}, Harness: sourcev228.String(row.Values["harness"]), Source: "legacy_session_cost_daily", SourceRevision: digest([]byte(key)), ObservedAt: day, CollectedAt: day, Cost: &model.UsageCost{Amount: amount, Currency: "USD", Kind: kind}, Coverage: model.UsageCoverage{Counters: model.UsageCoverageUnsupported, Cost: model.UsageCoverageComplete, Reason: "legacy daily cost aggregate"}, Historical: true, Provenance: sourcePath("session_cost_daily", row.Key) + suffix}, SourceKey: key})
		}
		appendCost("cost_usd", "#reported", model.UsageCostNativeReported)
		appendCost("virtual_cost_usd", "#estimate", model.UsageCostHistoricalEstimate)
	}
	for _, row := range t.inspection.Snapshot.Rows["audit_log"] {
		at := timeValue(row.Values["at"])
		if at.IsZero() {
			continue
		}
		actorID := model.AgentID(t.id("agents", sourcev228.String(row.Values["actor_agent"])))
		kind := model.PrincipalOperator
		if actorID != "" {
			kind = model.PrincipalAgent
		}
		batch.Activity = append(batch.Activity, app.HistoricalActivityWrite{Record: model.ActivityRecord{ID: stableImportID("act", t.inspection.Source.DatabaseHash, row.Key), Kind: model.ActivityHistorical, Actor: model.ActivityActor{Kind: kind, AgentID: actorID}, AgentID: model.AgentID(t.id("agents", sourcev228.String(row.Values["target_agent"]))), ConversationID: t.conversationID(sourcev228.String(row.Values["target_conv"])), Outcome: strconv.FormatInt(mustInt64(row.Values["status"]), 10), Reason: sourcev228.String(row.Values["verb"]), StartedAt: at, Historical: true, Provenance: sourcePath("audit_log", row.Key)}, SourceKey: row.Key, SourceRevision: digest([]byte(row.Key))})
	}
	for _, row := range t.inspection.Snapshot.Rows["execution_operations"] {
		at := timeValue(row.Values["requested_at"])
		if at.IsZero() {
			continue
		}
		state := strings.ToLower(sourcev228.String(row.Values["state"]))
		if !terminalOperation(state) {
			state = "interrupted_unresolved"
		}
		batch.Activity = append(batch.Activity, app.HistoricalActivityWrite{Record: model.ActivityRecord{ID: stableImportID("act", t.inspection.Source.DatabaseHash, "execution_operations\x00"+row.Key), Kind: model.ActivityHistorical, AgentID: model.AgentID(t.id("agents", sourcev228.String(row.Values["agent_id"]))), ConversationID: t.conversationID(sourcev228.String(row.Values["conv_id"])), Outcome: state, Reason: sourcev228.String(row.Values["kind"]), StartedAt: at, Historical: true, Provenance: sourcePath("execution_operations", row.Key)}, SourceKey: "execution_operations\x1f" + row.Key, SourceRevision: digest([]byte(row.Key))})
	}
	sort.Slice(batch.Usage, func(i, j int) bool { return batch.Usage[i].Observation.ID < batch.Usage[j].Observation.ID })
	sort.Slice(batch.Activity, func(i, j int) bool { return batch.Activity[i].Record.ID < batch.Activity[j].Record.ID })
}

func (t *translator) id(table, key string) string {
	if key == "" {
		return ""
	}
	return t.ids[table+"\x1f"+key]
}

func (t *translator) conversationID(key string) model.ConversationID {
	for _, table := range []string{"agent_conversations", "conv_index", "logical_conversations"} {
		if id := t.id(table, key); id != "" {
			return model.ConversationID(id)
		}
	}
	return ""
}

func (t *translator) conversationOwner(conv string) string {
	for _, row := range t.inspection.Snapshot.Rows["agent_conversations"] {
		if sourcev228.String(row.Values["conv_id"]) == conv {
			return sourcev228.String(row.Values["agent_id"])
		}
	}
	return ""
}

func desiredFromRow(values map[string]any) model.DesiredConfiguration {
	desired := model.DesiredConfiguration{Harness: sourcev228.String(values["harness"]), Model: sourcev228.String(values["model"]), Effort: sourcev228.String(values["effort"]), WorkingDirectory: firstNonEmpty(sourcev228.String(values["working_directory"]), sourcev228.String(values["cwd"]))}
	if raw := sourcev228.String(values["initial_spawn_config"]); raw != "" {
		var config map[string]any
		if json.Unmarshal([]byte(raw), &config) == nil {
			desired.Harness = firstNonEmpty(stringMap(config, "harness"), desired.Harness)
			desired.Model = firstNonEmpty(stringMap(config, "model"), desired.Model)
			desired.Effort = stringMap(config, "effort")
			desired.WorkingDirectory = firstNonEmpty(stringMap(config, "working_directory"), stringMap(config, "cwd"), desired.WorkingDirectory)
			values = config
		}
	}
	switch strings.ReplaceAll(strings.ToLower(firstNonEmpty(stringMap(values, "approval"), sourcev228.String(values["approval"]))), "-", "_") {
	case "supervised":
		desired.Approval = model.ApprovalSupervised
	case "automatic":
		desired.Approval = model.ApprovalAutomatic
	}
	switch strings.ReplaceAll(strings.ToLower(firstNonEmpty(stringMap(values, "sandbox"), sourcev228.String(values["sandbox"]))), "-", "_") {
	case "read_only":
		desired.Sandbox = model.SandboxReadOnly
	case "workspace_write":
		desired.Sandbox = model.SandboxWorkspaceWrite
	case "unconfined":
		desired.Sandbox = model.SandboxUnconfined
	}
	return desired
}

func stringMap(values map[string]any, key string) string {
	for candidate, value := range values {
		if strings.EqualFold(candidate, key) {
			return sourcev228.String(value)
		}
	}
	return ""
}

func spawnProfileName(value any) string {
	var config map[string]any
	if json.Unmarshal([]byte(sourcev228.String(value)), &config) != nil {
		return ""
	}
	return firstNonEmpty(stringMap(config, "profile"), stringMap(config, "profile_name"), stringMap(config, "spawn_profile"))
}

func recipientFor(sourceHash string, messageID model.MessageID, address model.MessageAddressKind, agentID model.AgentID, audience model.MessageAudienceKind, row sourcev228.Row) model.MessageRecipient {
	key := string(address) + "\x00" + string(agentID)
	recipient := model.MessageRecipient{ID: model.RecipientID(stableImportID("rcp", sourceHash, string(messageID)+"\x00"+key)), AddressKind: address, AgentID: agentID, Audience: audience, NotificationIntent: model.NotificationNone, NotificationOutcome: model.NotificationNotRequested}
	if readAt := optionalTime(row.Values["read_at"]); readAt != nil {
		recipient.ReadAt = readAt
	}
	if notified := optionalTime(firstNonNil(row.Values["nudge_sent_at"], row.Values["delivered_at"])); notified != nil {
		recipient.NotificationIntent, recipient.NotificationOutcome, recipient.NotifiedAt = model.NotificationIfAvailable, model.NotificationDelivered, notified
	}
	return recipient
}

func audienceAgents(row sourcev228.Row, field string) []string {
	var values []string
	_ = json.Unmarshal([]byte(sourcev228.String(row.Values[field])), &values)
	return values
}

func canonicalAudience(row sourcev228.Row) model.MessageAudienceKind {
	conversation := sourcev228.String(row.Values["to_conv"])
	agent := sourcev228.String(row.Values["to_agent"])
	ccConversations, ccAgents := audienceAgents(row, "cc_recipients"), audienceAgents(row, "cc_recipient_agents")
	for index := 0; index < len(ccConversations) || index < len(ccAgents); index++ {
		if (index < len(ccConversations) && ccConversations[index] == conversation) || (agent != "" && index < len(ccAgents) && ccAgents[index] == agent) {
			return model.MessageAudienceCC
		}
	}
	return model.MessageAudienceTo
}

func sourcePath(table, key string) string {
	if table == "" {
		return "snapshot"
	}
	return table + "[" + key + "]"
}

func stableImportID(prefix, sourceHash, key string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("offline-import-v%d\x00%s\x00%s", ImporterFormatVersion, sourceHash, key)))
	return prefix + "_" + hex.EncodeToString(sum[:16])
}

func timeValue(value any) time.Time {
	if value == nil {
		return time.Time{}
	}
	if n, ok := sourcev228.Int64(value); ok && n != 0 {
		if n > 1_000_000_000_000_000 {
			return time.Unix(0, n).UTC()
		}
		return time.Unix(n, 0).UTC()
	}
	text := strings.TrimSpace(sourcev228.String(value))
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, text); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func optionalTime(value any) *time.Time {
	at := timeValue(value)
	if at.IsZero() {
		return nil
	}
	return &at
}

func firstTime(candidate, fallback time.Time) time.Time {
	if candidate.IsZero() {
		return fallback
	}
	return candidate
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil && sourcev228.String(value) != "" {
			return value
		}
	}
	return nil
}

func mustInt64(value any) int64 {
	n, _ := sourcev228.Int64(value)
	return n
}

func exactDecimal(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(value, 10)
	default:
		return "0"
	}
}

func decimalZero(value string) bool {
	number, err := strconv.ParseFloat(value, 64)
	return err != nil || number == 0
}

func legacyTerminal(state string) bool {
	switch state {
	case "succeeded", "failed", "cancelled", "completed":
		return true
	}
	return false
}

func messageCreatedAt(messages []model.Message, id model.MessageID) time.Time {
	for _, message := range messages {
		if message.ID == id {
			return message.CreatedAt
		}
	}
	return time.Time{}
}

func importCounts(batch app.ImportBatch) map[string]int64 {
	var availableAttachments, metadataOnlyAttachments int64
	for _, attachment := range batch.ImportedAttachments {
		if attachment.Availability == model.ImportedAttachmentAvailable {
			availableAttachments++
		} else {
			metadataOnlyAttachments++
		}
	}
	counts := map[string]int64{
		"agents": int64(len(batch.Agents)), "groups": int64(len(batch.Groups)),
		"conversations": int64(len(batch.Conversations)), "messages": int64(len(batch.Messages)),
		"message_envelopes": int64(len(batch.MessageEnvelopes)),
		"attachments_total": int64(len(batch.ImportedAttachments)), "attachments_available": availableAttachments,
		"attachments_metadata_only": metadataOnlyAttachments, "configuration_profiles": int64(len(batch.ConfigurationProfiles)),
		"definitions": int64(len(batch.Definitions)), "automation_rules": int64(len(batch.AutomationRules)),
		"workspaces": int64(len(batch.Workspaces)), "usage": int64(len(batch.Usage)),
		"activity": int64(len(batch.Activity)), "retained_source_records": int64(len(batch.SourceRecords)),
	}
	if batch.SandboxDefaults != nil {
		counts["sandbox_defaults"] = 1
	}
	if len(batch.SandboxProfiles) > 0 {
		counts["sandbox_profiles"] = int64(len(batch.SandboxProfiles))
	}
	if len(batch.GroupConfigurations) > 0 {
		counts["group_configurations"] = int64(len(batch.GroupConfigurations))
	}
	return counts
}

func semanticDigest(batch app.ImportBatch) (string, error) {
	copy := batch
	copy.Receipt = model.ImportReceipt{}
	encoded, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	return digest(encoded), nil
}
