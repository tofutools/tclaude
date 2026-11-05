package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// CreateAgentGroup inserts a new group. Returns the new group's ID.
func CreateAgentGroup(name, descr string) (int64, error) {
	return CreateAgentGroupWithParent(name, descr, "")
}

// CreateAgentGroupWithParent inserts a new group, optionally nested under an
// existing parent group. parentName == "" creates a top-level group.
func CreateAgentGroupWithParent(name, descr, parentName string) (int64, error) {
	return CreateAgentGroupWithParentAndAttachment(name, descr, parentName, "", "")
}

// CreateAgentGroupWithParentAndAttachment inserts a new group with its optional
// persistent reference in the same transaction, so callers never expose a
// successfully created group that is missing requested attachment metadata.
func CreateAgentGroupWithParentAndAttachment(name, descr, parentName, attachmentURL, attachmentLabel string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var parentID sql.NullInt64
	if parentName = strings.TrimSpace(parentName); parentName != "" {
		if parentName == name {
			return 0, ErrGroupParentCycle
		}
		if err := tx.QueryRow(`SELECT id FROM agent_groups WHERE name = ?`, parentName).Scan(&parentID.Int64); errors.Is(err, sql.ErrNoRows) {
			return 0, ErrGroupParentNotFound
		} else if err != nil {
			return 0, err
		}
		parentID.Valid = true
	}

	res, err := tx.Exec(`
		INSERT INTO agent_groups
			(name, descr, created_at, parent_id, attachment_url, attachment_label)
		VALUES (?, ?, ?, ?, ?, ?)`,
		name, descr, dbTime(time.Now()), parentID, attachmentURL, attachmentLabel)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// CreateAgentGroupFrom inserts a new group named `name` that carries
// every configurable setting from `src` — descr, default cwd / context
// / profile, attachment, live group permissions, tree parent, the max-members cap and the notify switch. created_at is
// stamped fresh and the new group comes up active (archived_at left
// zero) regardless of src's archived state, so cloning an archived
// group yields a live one. Returns the new group's ID.
//
// This is the full-fidelity sibling of CreateAgentGroup (which sets only
// name + descr): group-clone needs every column copied, not just descr,
// so a cloned group is configured identically to its source.
func CreateAgentGroupFrom(name string, src AgentGroup) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	environmentJSON, err := marshalEnvironmentColumn(src.Environment)
	if err != nil {
		return 0, err
	}
	defaultProfileID, err := registryIDByName(tx, "spawn_profiles", src.DefaultProfile)
	if err != nil {
		return 0, err
	}
	sourceTemplateID := sql.NullInt64{Int64: src.SourceTemplateID, Valid: src.SourceTemplateID > 0}
	if !sourceTemplateID.Valid {
		sourceTemplateID, err = registryIDByName(tx, "group_templates", src.SourceTemplate)
		if err != nil {
			return 0, err
		}
	}
	// The source row may have been loaded before a concurrent profile delete.
	// Re-resolve inside the clone transaction rather than resurrecting a stale
	// numeric ID/name snapshot on the new group.
	var sandboxProfileID sql.NullInt64
	sandboxProfileName := src.SandboxProfile
	if src.SandboxProfileID > 0 {
		var id int64
		if scanErr := tx.QueryRow(`SELECT id FROM sandbox_profiles WHERE id = ?`, src.SandboxProfileID).Scan(&id); scanErr == nil {
			sandboxProfileID = sql.NullInt64{Int64: id, Valid: true}
		} else if !errors.Is(scanErr, sql.ErrNoRows) {
			return 0, scanErr
		} else {
			sandboxProfileName = ""
		}
	} else {
		sandboxProfileID, err = registryIDByName(tx, "sandbox_profiles", src.SandboxProfile)
		if err != nil {
			return 0, err
		}
	}
	res, err := tx.Exec(`
		INSERT INTO agent_groups
			(name, descr, default_cwd, default_context, default_profile, default_profile_id,
			 sandbox_profile, sandbox_profile_id, max_members, notify_enabled, remote_control,
			 attachment_url, attachment_label, mission, mission_header, source_template, source_template_id,
			 parent_id, created_at, owner_scopes_json, environment_json, reinject_after_compact, default_prompts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		name, src.Descr, src.DefaultCwd, src.DefaultContext, src.DefaultProfile, defaultProfileID,
		sandboxProfileName, sandboxProfileID, src.MaxMembers, src.NotifyEnabled, boolPtrToNull(src.RemoteControl),
		src.AttachmentURL, src.AttachmentLabel, src.Mission, src.MissionHeader, src.SourceTemplate, sourceTemplateID,
		src.ParentGroupID,
		dbTime(time.Now()), src.OwnerScopesJSON, environmentJSON, src.ReinjectAfterCompact, MarshalPromptRefs(src.DefaultPrompts))
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	// Group permissions are live group configuration, so a config clone carries
	// them even when member agents are not cloned.
	if src.ID > 0 {
		if _, err := tx.Exec(`
			INSERT INTO agent_group_permissions (group_id, slug, granted_at, granted_by, scope_json)
			SELECT ?, slug, ?, 'group-clone', scope_json FROM agent_group_permissions WHERE group_id = ?`,
			id, dbTime(time.Now()), src.ID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// SetAgentGroupDescr sets (or, with descr == "", clears) the group's
// own one-line description — the text shown next to the group name on
// the dashboard, distinct from any per-member descr. Returns the
// number of rows affected — 0 means no group by that name, so the
// caller can answer 404.
func SetAgentGroupDescr(name, descr string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`UPDATE agent_groups SET descr = ? WHERE name = ?`, descr, name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetAgentGroupDeployMeta records a group's deployment provenance
// (JOH-245): the mission it was deployed against and the template it was
// instantiated/deployed from. Either may be "" (no mission / no source
// template). Returns the number of rows affected — 0 means no group by
// that name. Called best-effort right after CreateAgentGroup on the
// deploy / instantiate path, mirroring SetAgentGroupDefaultCwd.
func SetAgentGroupDeployMeta(name, mission, missionHeader, sourceTemplate string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	templateID, err := registryIDByNameDB(db, "group_templates", sourceTemplate)
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`UPDATE agent_groups SET mission = ?, mission_header = ?, source_template = ?, source_template_id = ? WHERE name = ?`,
		mission, missionHeader, sourceTemplate, templateID, name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetAgentGroupDefaultCwd sets (or, with cwd == "", clears) the
// default working directory pre-filled for agents spawned into the
// named group. Returns the number of rows affected — 0 means no
// group by that name, so the caller can answer 404.
func SetAgentGroupDefaultCwd(name, cwd string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`UPDATE agent_groups
		SET default_cwd = ?, default_spawn_group = CASE WHEN ? = '' THEN 0 ELSE default_spawn_group END
		WHERE name = ?`, cwd, cwd, name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetAgentGroupDefaultSpawn atomically selects (or clears) the one group used
// as a directory-auto-join tie-breaker. Selecting requires a configured cwd;
// the database's partial unique index independently enforces the single-row
// invariant.
func SetAgentGroupDefaultSpawn(name string, enabled bool) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var cwd string
	if err := tx.QueryRow(`SELECT default_cwd FROM agent_groups WHERE name = ?`, name).Scan(&cwd); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	if enabled && strings.TrimSpace(cwd) == "" {
		return 0, fmt.Errorf("group %q needs a default spawn directory before it can be the default auto-join group", name)
	}
	if enabled {
		if _, err := tx.Exec(`UPDATE agent_groups SET default_spawn_group = 0 WHERE default_spawn_group != 0`); err != nil {
			return 0, err
		}
	}
	res, err := tx.Exec(`UPDATE agent_groups SET default_spawn_group = ? WHERE name = ?`, enabled, name)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// SetAgentGroupDefaultContext sets (or, with context == "", clears)
// the shared startup context for the named group. Returns the number
// of rows affected — 0 means no group by that name, so the caller can
// answer 404.
func SetAgentGroupDefaultContext(name, context string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`UPDATE agent_groups SET default_context = ? WHERE name = ?`, context, name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetAgentGroupEnvironment replaces the common process environment inherited
// by new agents spawned into the group. Existing agents keep their frozen
// launch snapshot across lifecycle operations.
func SetAgentGroupEnvironment(name string, environment []sandboxpolicy.EnvironmentEntry) (int64, error) {
	raw, err := marshalEnvironmentColumn(environment)
	if err != nil {
		return 0, err
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`UPDATE agent_groups SET environment_json = ? WHERE name = ?`, raw, name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetAgentGroupOwnerScopes sets (or, with scopes == "", clears) the group's
// owner-scope map — the narrowing applied to the structural owner-implied
// permission bypass for THIS group. The value is stored verbatim; the caller
// is responsible for validating and canonicalizing it (agentd does so at the
// HTTP boundary, against the same registry that validates grant scopes).
// Returns the number of rows affected — 0 means no group by that name, so the
// caller can answer 404.
func SetAgentGroupOwnerScopes(name, scopes string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`UPDATE agent_groups SET owner_scopes_json = ? WHERE name = ?`, scopes, name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetAgentGroupDefaultProfile sets (or, with profile == "", clears) the
// name of the spawn profile whose launch fields fill blank spawn fields
// for agents spawned into the named group (JOH-210). Callers that stamp a
// name for immediate use validate the referenced profile exists first;
// callers that resolve the profile LIVE at spawn time (the scribe summon,
// JOH-371) may deliberately stamp a name whose profile can later vanish —
// a dangling reference self-heals to the no-profile path at read time
// (groupDefaultProfile → nil) rather than erroring here. Returns the
// number of rows affected — 0 means no group by that name, so the caller
// can answer 404.
func SetAgentGroupDefaultProfile(name, profile string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	profileID, err := registryIDByNameDB(db, "spawn_profiles", profile)
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`UPDATE agent_groups SET default_profile = ?, default_profile_id = ? WHERE name = ?`, profile, profileID, name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetAgentGroupDefaultPrompts replaces (or, with an empty set, clears) the
// ordered saved-prompt names a group's spawns compose into the initial message
// when the spawn and its named profile name none. Callers validate the names;
// a reference that later dangles fails the spawn that resolves it. Returns the
// number of rows affected — 0 means no group by that name.
func SetAgentGroupDefaultPrompts(name string, prompts []string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`UPDATE agent_groups SET default_prompts = ? WHERE name = ?`, MarshalPromptRefs(prompts), name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetAgentGroupMaxMembers sets the hard member-count cap for the named
// group. 0 means unlimited (the default). A spawn that would push the
// group's membership over a non-zero cap is refused — see the
// spawn-guardrail layer. Returns the number of rows affected — 0 means
// no group by that name, so the caller can answer 404. A negative max
// is clamped to 0 (unlimited) rather than rejected, so a careless CLI
// value never wedges a group.
func SetAgentGroupMaxMembers(name string, max int) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	if max < 0 {
		max = 0
	}
	res, err := db.Exec(`UPDATE agent_groups SET max_members = ? WHERE name = ?`, max, name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetAgentGroupNotifyEnabled flips the group's OS-notification switch.
// false mutes state-transition notifications for every member agent
// (a per-agent 'on' pref in agent_notify_prefs still overrides the
// mute). Returns the number of rows affected — 0 means no group by
// that name, so the caller can answer 404.
func SetAgentGroupNotifyEnabled(name string, enabled bool) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`UPDATE agent_groups SET notify_enabled = ? WHERE name = ?`, enabled, name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetAgentGroupRemoteControl sets the group's remote-control policy — the
// tri-state knob that OVERRIDES a spawn profile's remote-control default
// (JOH-262). policy is nil = inherit (clear the override, defer to the profile),
// false = actively deny (force Remote Access off for the group's agents), true =
// actively opt-in (force it on). Stored as a nullable INTEGER so "inherit" is a
// real state distinct from "off". Returns the number of rows affected — 0 means
// no group by that name, so the caller can answer 404.
func SetAgentGroupRemoteControl(name string, policy *bool) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`UPDATE agent_groups SET remote_control = ? WHERE name = ?`,
		boolPtrToNull(policy), name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Values of AgentGroup.ReinjectAfterCompact.
const (
	// ReinjectContexts re-injects identity plus the group and profile startup
	// context after a compaction (identity only after /clear). The default.
	ReinjectContexts = "contexts"
	// ReinjectIdentity re-injects only the agent's identity, after both
	// compaction and /clear.
	ReinjectIdentity = "identity"
	// ReinjectOff re-injects nothing.
	ReinjectOff = "off"
)

// ValidReinjectAfterCompact reports whether v is a storable value ("" is the
// default and is valid).
func ValidReinjectAfterCompact(v string) bool {
	switch v {
	case "", ReinjectContexts, ReinjectIdentity, ReinjectOff:
		return true
	}
	return false
}

// EffectiveReinjectAfterCompact resolves the stored value, mapping "" to the
// default.
func (g *AgentGroup) EffectiveReinjectAfterCompact() string {
	if g == nil || g.ReinjectAfterCompact == "" {
		return ReinjectContexts
	}
	return g.ReinjectAfterCompact
}

// SetAgentGroupReinjectAfterCompact sets the group's re-injection mode.
// Storing the default spells it "" so a later change of default reaches
// groups that never chose. Returns rows affected (0 = no such group).
func SetAgentGroupReinjectAfterCompact(name, mode string) (int64, error) {
	if !ValidReinjectAfterCompact(mode) {
		return 0, fmt.Errorf("invalid reinject_after_compact %q (want contexts, identity, or off)", mode)
	}
	if mode == ReinjectContexts {
		mode = ""
	}
	db, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`UPDATE agent_groups SET reinject_after_compact = ? WHERE name = ?`, mode, name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetAgentGroupAttachment sets (or, with url == "", clears) the group's
// persistent reference. Clearing the URL also clears the optional label so a
// stale label cannot outlive the attachment it described.
func SetAgentGroupAttachment(name, url, label string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errors.New("SetAgentGroupAttachment: group name required")
	}
	url = strings.TrimSpace(url)
	label = strings.TrimSpace(label)
	if url == "" {
		label = ""
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(
		`UPDATE agent_groups SET attachment_url = ?, attachment_label = ? WHERE name = ?`,
		url, label, name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteAgentGroup removes a group by name. Cascades to membership +
// ownership rows (ON DELETE CASCADE in schema) and, within the same
// transaction, rewrites the group's messages to group_id = 0 so the
// conversation history survives the group's deletion as direct
// messages. agent_messages no longer has a foreign key to
// agent_groups (the "universal inbox" change dropped it), so this is a
// data-retention choice rather than an FK workaround: a deleted group
// should not silently destroy what its members said to each other.
//
// No-op if the group doesn't exist.
func DeleteAgentGroup(name string) error {
	db, err := Open()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var gID int64
	if err := tx.QueryRow(`SELECT id FROM agent_groups WHERE name = ?`, name).Scan(&gID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if _, err := tx.Exec(`UPDATE agent_messages SET group_id = 0 WHERE group_id = ?`, gID); err != nil {
		return err
	}
	// Advisory process state (JOH-242) is keyed to the group by group_id in
	// sibling tables (unlike deploy meta, which lives on the agent_groups row
	// and dies with it), so sweep it explicitly in the same transaction.
	if _, err := tx.Exec(`DELETE FROM group_process_state WHERE group_id = ?`, gID); err != nil {
		return err
	}
	// Runtime process runs bound to the group park as orphaned for a human to
	// re-scope; they are never silently re-bound, and the flip must commit
	// with the delete because group ids can be reused.
	if err := orphanGroupProcessRunsTx(tx, gID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM group_process_transitions WHERE group_id = ?`, gID); err != nil {
		return err
	}
	// Staged-spawn choreography state (JOH-244) is keyed to the group by
	// group_id, so cancel any pending waves in the same transaction (the v92
	// process-state cleanup pattern). A wave runner that reads a now-missing
	// row simply drops it — self-healing.
	if _, err := tx.Exec(`DELETE FROM group_wave_choreography WHERE group_id = ?`, gID); err != nil {
		return err
	}
	// Group-target cron jobs (JOH-244) — including template-seeded rhythms —
	// target THIS group and become meaningless once it is gone, so remove them
	// here (agent_cron_runs cascade-clean via their job FK). Conv-target jobs
	// merely routed THROUGH the group still deliver to their conv and are left
	// alone. A fire against an already-deleted group also no-ops gracefully
	// (fireCronGroupJob resolves the group to nil → "no_target"), so this is
	// tidy-up, not correctness-critical.
	if _, err := tx.Exec(
		`DELETE FROM agent_cron_jobs WHERE target_kind = 'group' AND group_id = ?`, gID); err != nil {
		return err
	}
	// Group-target standing orders (TCL-841) are swept for the same reason and
	// on the same terms: they target THIS group and are meaningless once it is
	// gone, while a conv-target order merely routed through the group still has
	// a real recipient and is left alone. Their delivery-ledger rows go with
	// them — an evaluation history for an order that no longer exists answers
	// no question anyone can still ask.
	//
	// Unlike the cron sweep this is closer to correctness than tidy-up: a
	// group id is reused when a later group takes the same row, and a
	// surviving order would then start injecting text into whoever joins it.
	//
	// A reusable definition may also be activated on other groups. Promote
	// the lowest additional group to primary before the legacy sweep so
	// deleting one group does not delete an instruction still in use
	// elsewhere. This is an administrative target reshaping only: the
	// effective recipients do not change, so delivery revision stays put.
	//
	// First invalidate stale order editors for definitions where this group
	// is an additional activation. The foreign-key cascade will remove those
	// rows when the group goes away, but a cascade alone cannot advance the
	// parent definition's optimistic-concurrency token.
	scopeRemovalTime := formatStandingTime(time.Now())
	if _, err := tx.Exec(`
		UPDATE agent_standing_orders
		   SET row_version = row_version + 1, updated_at = ?
		 WHERE EXISTS (
			SELECT 1 FROM agent_standing_order_group_scopes scope
			 WHERE scope.order_id = agent_standing_orders.id
			   AND scope.group_id = ?
		 )`, scopeRemovalTime, gID); err != nil {
		return err
	}
	type standingOrderPromotion struct {
		orderID int64
		groupID int64
	}
	promotionRows, err := tx.Query(`
		SELECT orders.id, MIN(scope.group_id)
		  FROM agent_standing_orders orders
		  JOIN agent_standing_order_group_scopes scope ON scope.order_id = orders.id
		 WHERE orders.target_kind = 'group' AND orders.group_id = ?
		 GROUP BY orders.id`, gID)
	if err != nil {
		return err
	}
	var promotions []standingOrderPromotion
	for promotionRows.Next() {
		var promotion standingOrderPromotion
		if err := promotionRows.Scan(&promotion.orderID, &promotion.groupID); err != nil {
			_ = promotionRows.Close()
			return err
		}
		promotions = append(promotions, promotion)
	}
	if err := promotionRows.Close(); err != nil {
		return err
	}
	if err := promotionRows.Err(); err != nil {
		return err
	}
	promotionTime := formatStandingTime(time.Now())
	for _, promotion := range promotions {
		if _, err := tx.Exec(`
			UPDATE agent_standing_orders
			   SET group_id = ?, target_role = '',
			       row_version = row_version + 1, updated_at = ?
			 WHERE id = ? AND target_kind = 'group' AND group_id = ?`,
			promotion.groupID, promotionTime, promotion.orderID, gID); err != nil {
			return err
		}
		if _, err := tx.Exec(`
			DELETE FROM agent_standing_order_group_scopes
			 WHERE order_id = ? AND group_id = ?`,
			promotion.orderID, promotion.groupID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM agent_standing_order_deliveries
		WHERE order_id IN (
			SELECT id FROM agent_standing_orders WHERE target_kind = 'group' AND group_id = ?
		)`, gID); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`DELETE FROM agent_standing_orders WHERE target_kind = 'group' AND group_id = ?`, gID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM spawn_harness_rules WHERE group_id = ?`, gID); err != nil {
		return err
	}
	// Peer and inherited pool grants store stable group IDs in their scope.
	// Sweep them in the deletion transaction, like other group-owned authority.
	for _, table := range []string{"federation_peer_grants", "federation_node_group_grants"} {
		if _, err := tx.Exec(`DELETE FROM `+table+` WHERE scope = ?`, FederationGroupScope(gID)); err != nil {
			return err
		}
	}
	// Rebind every sandbox record naming the group: its members', and those of
	// agents that moved away but kept a setting on it.
	memberRows, err := tx.Query(`SELECT agent_id FROM agent_group_members WHERE group_id = ?
		UNION
		SELECT agent_id FROM agents WHERE json_valid(effective_sandbox_config) AND (
			json_extract(effective_sandbox_config, '$.resolution_group_id') = ?1 OR
			json_extract(effective_sandbox_config, '$.group_binding.environment_group_id') = ?1)`, gID)
	if err != nil {
		return err
	}
	var memberIDs []string
	for memberRows.Next() {
		var agentID string
		if err := memberRows.Scan(&agentID); err != nil {
			_ = memberRows.Close()
			return err
		}
		memberIDs = append(memberIDs, agentID)
	}
	if err := memberRows.Err(); err != nil {
		return err
	}
	if err := memberRows.Close(); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM agent_group_members WHERE group_id = ?`, gID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM agent_groups WHERE id = ?`, gID); err != nil {
		return err
	}
	// After the group row is gone, so a setting pinned to it is released too.
	for _, agentID := range memberIDs {
		if err := syncGroupSourcedSettings(tx, agentID, 0); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const agentGroupSelect = `SELECT id, name, descr, default_cwd, default_spawn_group, default_context,
	CASE WHEN default_profile_id IS NULL THEN default_profile
	     ELSE COALESCE((SELECT name FROM spawn_profiles WHERE id = default_profile_id), '') END,
	CASE WHEN sandbox_profile_id IS NULL THEN sandbox_profile
	     ELSE COALESCE((SELECT name FROM sandbox_profiles WHERE id = sandbox_profile_id), '') END,
	COALESCE(sandbox_profile_id, 0),
	max_members, notify_enabled, remote_control, attachment_url, attachment_label, mission, mission_header,
	CASE WHEN source_template_id IS NULL THEN source_template
	     ELSE COALESCE((SELECT name FROM group_templates WHERE id = source_template_id), source_template) END,
	COALESCE(source_template_id, 0),
	created_at, archived_at, parent_id, route_generation, owner_scopes_json, environment_json,
	reinject_after_compact, default_prompts FROM agent_groups`

const agentGroupAliasedSelect = `SELECT g.id, g.name, g.descr, g.default_cwd, g.default_spawn_group, g.default_context,
	CASE WHEN g.default_profile_id IS NULL THEN g.default_profile
	     ELSE COALESCE((SELECT name FROM spawn_profiles WHERE id = g.default_profile_id), '') END,
	CASE WHEN g.sandbox_profile_id IS NULL THEN g.sandbox_profile
	     ELSE COALESCE((SELECT name FROM sandbox_profiles WHERE id = g.sandbox_profile_id), '') END,
	COALESCE(g.sandbox_profile_id, 0),
	g.max_members, g.notify_enabled, g.remote_control, g.attachment_url, g.attachment_label, g.mission, g.mission_header,
	CASE WHEN g.source_template_id IS NULL THEN g.source_template
	     ELSE COALESCE((SELECT name FROM group_templates WHERE id = g.source_template_id), g.source_template) END,
	COALESCE(g.source_template_id, 0),
	g.created_at, g.archived_at, g.parent_id, g.route_generation, g.owner_scopes_json, g.environment_json,
	g.reinject_after_compact, g.default_prompts`

// GetAgentGroupByName returns the group with the given name, or nil if not
// found.
func GetAgentGroupByName(name string) (*AgentGroup, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	row := db.QueryRow(agentGroupSelect+` WHERE name = ?`, name)
	g, err := scanAgentGroup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return g, err
}

// GetAgentGroupByID returns the group with the given primary key, or
// nil if not found.
func GetAgentGroupByID(id int64) (*AgentGroup, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	row := db.QueryRow(agentGroupSelect+` WHERE id = ?`, id)
	g, err := scanAgentGroup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return g, err
}

// ListAgentGroups returns all groups, ordered by name.
func ListAgentGroups() ([]*AgentGroup, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(agentGroupSelect + ` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*AgentGroup
	for rows.Next() {
		g, err := scanAgentGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ErrGroupNameTaken signals that a rename target already exists.
var ErrGroupNameTaken = errors.New("group name already exists")

// RenameAgentGroup atomically renames the canonical name of a group +
// records an audit row. byConv may be empty for the human path.
//
// Returns:
//   - (nil, nil) when no group with oldName exists — caller chooses
//     whether to surface that as 404 or no-op.
//   - (nil, ErrGroupNameTaken) if newName collides with another group.
//   - (group, nil) on success — the returned group reflects the NEW
//     name + the same id/created_at/archived_at.
//
// All work happens in a single transaction so a failure mid-flight
// rolls back cleanly. The schema stores group references as integer
// foreign keys (group_id), so renaming is a single-row UPDATE — no
// cascades required.
func RenameAgentGroup(oldName, newName, byConv string) (*AgentGroup, error) {
	if newName == "" {
		return nil, fmt.Errorf("newName required")
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRow(
		agentGroupSelect+` WHERE name = ?`,
		oldName)
	g, err := scanAgentGroup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if oldName == newName {
		// No-op: idempotent same-name. Still record the audit row so a
		// human can see they tried — useful when chasing typos.
		if _, err := tx.Exec(
			`INSERT INTO agent_group_audit (group_id, old_name, new_name, by_conv, at, by_agent)
			 VALUES (?, ?, ?, ?, ?, `+agentForConvExpr+`)`,
			g.ID, oldName, newName, byConv, dbTime(time.Now()), byConv); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return g, nil
	}
	// Collision check before the UPDATE to surface a clear 409, since
	// the UNIQUE constraint would fire as a generic SQLITE_CONSTRAINT.
	var existing int64
	err = tx.QueryRow(`SELECT id FROM agent_groups WHERE name = ?`, newName).Scan(&existing)
	if err == nil {
		return nil, ErrGroupNameTaken
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE agent_groups SET name = ? WHERE id = ?`, newName, g.ID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(
		`INSERT INTO agent_group_audit (group_id, old_name, new_name, by_conv, at, by_agent)
		 VALUES (?, ?, ?, ?, ?, `+agentForConvExpr+`)`,
		g.ID, oldName, newName, byConv, dbTime(time.Now()), byConv); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	g.Name = newName
	return g, nil
}

// ListAgentGroupRenames returns the rename history for a single group,
// most recent first. Used for "what was this group called before?"
// lookups; not currently surfaced via CLI but cheap to expose later.
func ListAgentGroupRenames(groupID int64) ([]AgentGroupAudit, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return queryRows(d, func(rows rowScanner) (AgentGroupAudit, error) {
		var a AgentGroupAudit
		var at dbTimestamp
		if err := rows.Scan(&a.ID, &a.GroupID, &a.OldName, &a.NewName, &a.ByConv, &at, &a.ByAgent); err != nil {
			return AgentGroupAudit{}, err
		}
		a.At = exportTimestamp(at)
		return a, nil
	},
		`SELECT id, group_id, old_name, new_name, by_conv, at, by_agent
		 FROM agent_group_audit WHERE group_id = ? ORDER BY id DESC`, groupID)
}

// AgentGroupAudit is one row in agent_group_audit, recording a single
// rename event. ByConv is the conv-id snapshot of who renamed the group;
// ByAgent is the stable agent_id companion (dual-written via
// agentForConvExpr, v77-backfilled) — the durable actor, with ByConv kept
// as the point-in-time snapshot. ByAgent is "" for a human/un-enrolled
// renamer.
type AgentGroupAudit struct {
	ID      int64
	GroupID int64
	OldName string
	NewName string
	ByConv  string
	At      string
	ByAgent string
}

// ArchiveAgentGroup soft-deletes a group: stamps archived_at = now and
// leaves all rows intact. Mutating operations (groups.members.add, owners.*,
// message) refuse on archived groups; listing endpoints filter them out
// by default. Idempotent — re-archiving an already-archived group bumps
// the timestamp without erroring. Returns sql.ErrNoRows if the group
// doesn't exist (callers should treat as a clean miss).
func ArchiveAgentGroup(name string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE agent_groups SET archived_at = ? WHERE name = ?`,
		dbTime(time.Now().UTC()), name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UnarchiveAgentGroup reverses ArchiveAgentGroup: clears archived_at so
// the group is active again. Idempotent on already-active groups.
func UnarchiveAgentGroup(name string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE agent_groups SET archived_at = NULL WHERE name = ?`, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ErrGroupParentNotFound is returned by SetAgentGroupParent when the named
// parent group does not exist.
var ErrGroupParentNotFound = errors.New("parent group not found")

// ErrGroupParentCycle is returned by SetAgentGroupParent when the requested
// nesting would create a loop — a group cannot be its own parent, nor be
// nested under a group that is already one of its own descendants.
var ErrGroupParentCycle = errors.New("cannot nest a group under itself or one of its own descendants")

// SetAgentGroupParent nests the group childID under the group named
// parentName (n-level groups-in-groups, JOH-392). parentName == "" (after
// trimming) clears the parent, making the child top-level. Returns the
// updated child group.
//
// Cycle safety: rejects self-parenting and any edge that would close a loop.
// The check walks UP the prospective parent's ancestor chain; sighting the
// child means the child is already an ancestor of the parent, so the new
// edge would loop — refuse with ErrGroupParentCycle. A visited set bounds
// the walk so even a pre-existing corrupt cycle in the table cannot spin
// forever. Returns ErrGroupParentNotFound if parentName resolves to nothing,
// and sql.ErrNoRows if the child vanished mid-flight.
//
// All work is one transaction; the child row's parent_id is the only write.
// (Deleting a parent needs no code here — the column's ON DELETE SET NULL
// FK auto-orphans children back to top-level; see migrateV98toV99.)
func SetAgentGroupParent(childID int64, parentName string) (*AgentGroup, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var newParent sql.NullInt64
	if parentName = strings.TrimSpace(parentName); parentName != "" {
		var pid int64
		err := tx.QueryRow(`SELECT id FROM agent_groups WHERE name = ?`, parentName).Scan(&pid)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrGroupParentNotFound
		} else if err != nil {
			return nil, err
		}
		if pid == childID {
			return nil, ErrGroupParentCycle
		}
		// Walk up from the prospective parent. If we reach childID, the child
		// is already an ancestor of pid and this edge would loop.
		seen := map[int64]bool{}
		cur := sql.NullInt64{Int64: pid, Valid: true}
		for cur.Valid {
			if cur.Int64 == childID {
				return nil, ErrGroupParentCycle
			}
			if seen[cur.Int64] {
				break // pre-existing corrupt cycle in the table; stop walking
			}
			seen[cur.Int64] = true
			var next sql.NullInt64
			err := tx.QueryRow(`SELECT parent_id FROM agent_groups WHERE id = ?`, cur.Int64).Scan(&next)
			if errors.Is(err, sql.ErrNoRows) {
				break
			} else if err != nil {
				return nil, err
			}
			cur = next
		}
		newParent = sql.NullInt64{Int64: pid, Valid: true}
	}

	res, err := tx.Exec(`UPDATE agent_groups SET parent_id = ? WHERE id = ?`, newParent, childID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, sql.ErrNoRows
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return GetAgentGroupByID(childID)
}

// MissionBlock is the group's mission as the block every member is briefed
// with ("## <MissionHeader>\n\n<mission>"), or "" for a group with no mission
// or a legacy group whose mission is still folded into DefaultContext.
func (g *AgentGroup) MissionBlock() string {
	if g == nil {
		return ""
	}
	header := strings.TrimSpace(g.MissionHeader)
	mission := normalizeMissionText(g.Mission)
	if header == "" || mission == "" {
		return ""
	}
	return "## " + header + "\n\n" + mission
}

// normalizeMissionText trims a mission and normalizes its line endings, the
// same way a deploy normalizes the text it briefs members with.
func normalizeMissionText(mission string) string {
	mission = strings.TrimSpace(mission)
	mission = strings.ReplaceAll(mission, "\r\n", "\n")
	return strings.ReplaceAll(mission, "\r", "\n")
}

// GetDefaultContext is DefaultContext, nil-safe.
func (g *AgentGroup) GetDefaultContext() string {
	if g == nil {
		return ""
	}
	return g.DefaultContext
}
