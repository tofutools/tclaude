package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/groupexport"
)

// ErrGroupNotFound is returned by CollectGroupExport when the named
// group does not exist.
var ErrGroupNotFound = errors.New("group not found")

// group_export.go is the DB half of per-group export / import. It owns
// the raw SQL that reads every group- and conv-scoped table into a
// groupexport.Export, and the single transaction that writes one back.
//
// The .jsonl files and the on-disk container are NOT this file's
// concern — the daemon's groups_export.go handles file I/O, the conv-id
// collision/remap decision, and path rewriting. This file is given a
// fully-resolved GroupImportPlan and applies it transactionally.
//
// Tables carried (every table keyed to the group or to a member's
// conv-id): agent_groups, agent_group_members, agent_group_owners,
// agent_group_audit, agent_permissions, agents (as enrollments), agent_workdir,
// agent_sudo_grants, agent_head_aliases, agent_conv_succession,
// agent_spawn_history, agent_clone_history, agent_cron_jobs,
// agent_cron_runs, agent_messages. Deliberately NOT carried: conv_index
// and conv_embeddings (rebuilt from the .jsonl on scan), sessions and
// notify_state (live tmux runtime), usage_cache / git_cache (DB-global
// caches), agent_group_links (cross-group — deferred to a future
// multi-group export).

// CollectGroupExport gathers every DB row belonging to the named group
// into a groupexport.Export. The returned Convs carry ConvID, SourceCwd
// and Title only — the daemon fills in the .jsonl Content. Returns an
// error wrapping ErrGroupNotFound when no such group exists.
func CollectGroupExport(name string) (*groupexport.Export, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	g, err := GetAgentGroupByName(name)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, fmt.Errorf("%w: %q", ErrGroupNotFound, name)
	}

	home, _ := os.UserHomeDir()
	exp := &groupexport.Export{
		FormatVersion: groupexport.FormatVersion,
		ExportedAt:    time.Now().UTC().Format(time.RFC3339Nano),
		SchemaVersion: schemaVersion(d),
		SourceGroup:   g.Name,
		SourceHome:    home,
		SourceOS:      runtime.GOOS,
	}

	// --- the group row ---
	// default_model is intentionally NOT selected: the vestigial column was
	// dropped (JOH-220), so a v2 export omits it. exp.Group.DefaultModel
	// stays "" and is left out of the manifest (omitempty).
	if err := d.QueryRow(`
		SELECT descr, default_context, max_members, created_at, archived_at
		FROM agent_groups WHERE id = ?`, g.ID).Scan(
		&exp.Group.Descr, &exp.Group.DefaultContext,
		&exp.Group.MaxMembers, &exp.Group.CreatedAt, &exp.Group.ArchivedAt); err != nil {
		return nil, fmt.Errorf("collect group row: %w", err)
	}

	// --- group-scoped tables ---
	if exp.Members, err = collectMembers(d, g.ID); err != nil {
		return nil, err
	}
	if exp.Owners, err = collectOwners(d, g.ID); err != nil {
		return nil, err
	}
	if exp.Audit, err = collectAudit(d, g.ID); err != nil {
		return nil, err
	}
	if exp.Messages, err = collectMessages(d, g.ID); err != nil {
		return nil, err
	}
	cronJobs, cronJobIDs, err := collectCronJobs(d, g.ID)
	if err != nil {
		return nil, err
	}
	exp.CronJobs = cronJobs
	if exp.CronRuns, err = collectCronRuns(d, cronJobIDs); err != nil {
		return nil, err
	}

	// --- conv-scoped tables (keyed to the member conv-ids) ---
	convIDs := make([]string, len(exp.Members))
	for i, m := range exp.Members {
		convIDs[i] = m.ConvID
	}
	if exp.Permissions, err = collectPermissions(d, convIDs); err != nil {
		return nil, err
	}
	if exp.Enrollments, err = collectEnrollments(d, convIDs); err != nil {
		return nil, err
	}
	if exp.Workdirs, err = collectWorkdirs(d, convIDs); err != nil {
		return nil, err
	}
	if exp.SudoGrants, err = collectSudoGrants(d, convIDs); err != nil {
		return nil, err
	}
	if exp.HeadAliases, err = collectHeadAliases(d, convIDs); err != nil {
		return nil, err
	}
	if exp.Successions, err = collectSuccessions(d, convIDs); err != nil {
		return nil, err
	}
	if exp.SpawnHist, err = collectSpawnHist(d, convIDs); err != nil {
		return nil, err
	}
	if exp.CloneHist, err = collectCloneHist(d, convIDs); err != nil {
		return nil, err
	}

	// --- conv stubs (the daemon fills Content / Missing) ---
	exp.Convs = collectConvStubs(d, convIDs)

	return exp, nil
}

// inClause builds an "IN (?, ?, …)" fragment plus the matching []any
// args for a slice of conv-ids. Returns ("IN (NULL)", nil) for an empty
// slice — a clause that matches nothing, so a memberless group collects
// cleanly with no special-casing at every call site.
func inClause(ids []string) (string, []any) {
	if len(ids) == 0 {
		return "IN (NULL)", nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return "IN (" + strings.Repeat("?, ", len(ids)-1) + "?)", args
}

func collectMembers(d *sql.DB, groupID int64) ([]groupexport.Member, error) {
	// Membership is agent-keyed (JOH-26); export each member by its actor's
	// current conv so re-import re-adds it by conv (which re-resolves to an
	// actor on the target).
	rows, err := d.Query(`
		SELECT ag.current_conv_id, m.role, m.descr, m.joined_at
		FROM agent_group_members m JOIN agents ag ON ag.agent_id = m.agent_id
		WHERE m.group_id = ? ORDER BY ag.current_conv_id`, groupID)
	if err != nil {
		return nil, fmt.Errorf("collect members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []groupexport.Member
	for rows.Next() {
		var m groupexport.Member
		if err := rows.Scan(&m.ConvID, &m.Role, &m.Descr, &m.JoinedAt); err != nil {
			return nil, fmt.Errorf("scan member: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func collectOwners(d *sql.DB, groupID int64) ([]groupexport.Owner, error) {
	rows, err := d.Query(`
		SELECT ag.current_conv_id, o.granted_at, o.granted_by
		FROM agent_group_owners o JOIN agents ag ON ag.agent_id = o.agent_id
		WHERE o.group_id = ? ORDER BY ag.current_conv_id`, groupID)
	if err != nil {
		return nil, fmt.Errorf("collect owners: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []groupexport.Owner
	for rows.Next() {
		var o groupexport.Owner
		if err := rows.Scan(&o.ConvID, &o.GrantedAt, &o.GrantedBy); err != nil {
			return nil, fmt.Errorf("scan owner: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func collectAudit(d *sql.DB, groupID int64) ([]groupexport.AuditEntry, error) {
	rows, err := d.Query(`
		SELECT old_name, new_name, by_conv, at
		FROM agent_group_audit WHERE group_id = ? ORDER BY id`, groupID)
	if err != nil {
		return nil, fmt.Errorf("collect audit: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []groupexport.AuditEntry
	for rows.Next() {
		var a groupexport.AuditEntry
		if err := rows.Scan(&a.OldName, &a.NewName, &a.ByConv, &a.At); err != nil {
			return nil, fmt.Errorf("scan audit: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func collectMessages(d *sql.DB, groupID int64) ([]groupexport.Message, error) {
	rows, err := d.Query(`
		SELECT id, from_conv, to_conv, subject, body, created_at, delivered_at,
		       read_at, parent_id, to_recipients, cc_recipients, original_to_conv
		FROM agent_messages WHERE group_id = ? ORDER BY id`, groupID)
	if err != nil {
		return nil, fmt.Errorf("collect messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []groupexport.Message
	for rows.Next() {
		var m groupexport.Message
		if err := rows.Scan(&m.ID, &m.FromConv, &m.ToConv, &m.Subject, &m.Body,
			&m.CreatedAt, &m.DeliveredAt, &m.ReadAt, &m.ParentID,
			&m.ToRecipients, &m.CcRecipients, &m.OriginalToConv); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func collectCronJobs(d *sql.DB, groupID int64) ([]groupexport.CronJob, []string, error) {
	// owner/target are agent-keyed (JOH-26 PR3a); resolve each back to its
	// actor's CURRENT conv so the export stays conv-portable. LEFT JOIN +
	// COALESCE keeps a group-target job (target_agent '') or an owner-less job
	// rather than dropping it.
	rows, err := d.Query(`
		SELECT j.id, j.name, j.target_kind, COALESCE(ow.current_conv_id, ''), COALESCE(tg.current_conv_id, ''),
		       j.interval_seconds, j.subject, j.body,
		       j.enabled, j.created_at, j.last_run_at, j.last_run_status
		FROM agent_cron_jobs j
		LEFT JOIN agents ow ON ow.agent_id = j.owner_agent
		LEFT JOIN agents tg ON tg.agent_id = j.target_agent
		WHERE j.group_id = ? ORDER BY j.id`, groupID)
	if err != nil {
		return nil, nil, fmt.Errorf("collect cron jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []groupexport.CronJob
	var ids []string
	for rows.Next() {
		var j groupexport.CronJob
		if err := rows.Scan(&j.ID, &j.Name, &j.TargetKind, &j.OwnerConv, &j.TargetConv,
			&j.IntervalSeconds, &j.Subject, &j.Body, &j.Enabled, &j.CreatedAt,
			&j.LastRunAt, &j.LastRunStatus); err != nil {
			return nil, nil, fmt.Errorf("scan cron job: %w", err)
		}
		out = append(out, j)
		ids = append(ids, fmt.Sprintf("%d", j.ID))
	}
	return out, ids, rows.Err()
}

func collectCronRuns(d *sql.DB, jobIDs []string) ([]groupexport.CronRun, error) {
	if len(jobIDs) == 0 {
		return nil, nil
	}
	clause, args := inClause(jobIDs)
	rows, err := d.Query(`
		SELECT job_id, fired_at, status, error_msg
		FROM agent_cron_runs WHERE job_id `+clause+` ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("collect cron runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []groupexport.CronRun
	for rows.Next() {
		var r groupexport.CronRun
		if err := rows.Scan(&r.JobID, &r.FiredAt, &r.Status, &r.ErrorMsg); err != nil {
			return nil, fmt.Errorf("scan cron run: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func collectPermissions(d *sql.DB, convIDs []string) ([]groupexport.Permission, error) {
	clause, args := inClause(convIDs)
	rows, err := d.Query(`
		SELECT ag.current_conv_id, p.slug, p.effect, p.granted_at, p.granted_by
		FROM agent_permissions p JOIN agents ag ON ag.agent_id = p.agent_id
		WHERE ag.current_conv_id `+clause+` ORDER BY ag.current_conv_id, p.slug`, args...)
	if err != nil {
		return nil, fmt.Errorf("collect permissions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []groupexport.Permission
	for rows.Next() {
		var p groupexport.Permission
		if err := rows.Scan(&p.ConvID, &p.Slug, &p.Effect, &p.GrantedAt, &p.GrantedBy); err != nil {
			return nil, fmt.Errorf("scan permission: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// collectEnrollments emits one Enrollment record per exported member conv,
// sourced from the actor layer (JOH-26 PR3c removed agent_enrollment). The
// archive keeps the legacy `enrollments` shape for cross-version compatibility:
// an older importer still reads it into agent_enrollment, and a current importer
// translates it back onto the actor. Actor facts (created_via, retire state,
// pending_name) are carried on the actor's CURRENT conv.
func collectEnrollments(d *sql.DB, convIDs []string) ([]groupexport.Enrollment, error) {
	clause, args := inClause(convIDs)
	rows, err := d.Query(`
		SELECT ac.conv_id, ag.created_at, ag.created_via, ag.retired_at,
		       ag.retired_by, ag.retire_reason, ag.pending_name
		FROM agent_conversations ac
		JOIN agents ag ON ag.agent_id = ac.agent_id
		WHERE ac.conv_id `+clause+` ORDER BY ac.conv_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("collect enrollments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []groupexport.Enrollment
	for rows.Next() {
		var e groupexport.Enrollment
		if err := rows.Scan(&e.ConvID, &e.EnrolledAt, &e.EnrolledVia, &e.RetiredAt,
			&e.RetiredBy, &e.RetireReason, &e.PendingName); err != nil {
			return nil, fmt.Errorf("scan enrollment: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func collectWorkdirs(d *sql.DB, convIDs []string) ([]groupexport.Workdir, error) {
	clause, args := inClause(convIDs)
	rows, err := d.Query(`
		SELECT conv_id, updated_at
		FROM agent_workdir WHERE conv_id `+clause+` ORDER BY conv_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("collect workdirs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []groupexport.Workdir
	for rows.Next() {
		var w groupexport.Workdir
		if err := rows.Scan(&w.ConvID, &w.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan workdir: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func collectSudoGrants(d *sql.DB, convIDs []string) ([]groupexport.SudoGrant, error) {
	clause, args := inClause(convIDs)
	rows, err := d.Query(`
		SELECT ag.current_conv_id, s.slug, s.granted_at, s.expires_at, s.granted_by, s.reason, s.revoked_at
		FROM agent_sudo_grants s JOIN agents ag ON ag.agent_id = s.agent_id
		WHERE ag.current_conv_id `+clause+` ORDER BY s.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("collect sudo grants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []groupexport.SudoGrant
	for rows.Next() {
		var s groupexport.SudoGrant
		if err := rows.Scan(&s.ConvID, &s.Slug, &s.GrantedAt, &s.ExpiresAt,
			&s.GrantedBy, &s.Reason, &s.RevokedAt); err != nil {
			return nil, fmt.Errorf("scan sudo grant: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func collectHeadAliases(d *sql.DB, convIDs []string) ([]groupexport.HeadAlias, error) {
	clause, args := inClause(convIDs)
	rows, err := d.Query(`
		SELECT handle, anchor_conv_id, created_at, by_conv
		FROM agent_head_aliases WHERE anchor_conv_id `+clause+` ORDER BY handle`, args...)
	if err != nil {
		return nil, fmt.Errorf("collect head aliases: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []groupexport.HeadAlias
	for rows.Next() {
		var h groupexport.HeadAlias
		if err := rows.Scan(&h.Handle, &h.AnchorConvID, &h.CreatedAt, &h.ByConv); err != nil {
			return nil, fmt.Errorf("scan head alias: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func collectSuccessions(d *sql.DB, convIDs []string) ([]groupexport.Succession, error) {
	clause, args := inClause(convIDs)
	// A succession is in scope if EITHER endpoint is a member conv —
	// the member may be the reincarnated-from or the reincarnated-to.
	rows, err := d.Query(`
		SELECT old_conv_id, new_conv_id, reason, succeeded_at
		FROM agent_conv_succession
		WHERE old_conv_id `+clause+` OR new_conv_id `+clause+`
		ORDER BY old_conv_id`, append(args, args...)...)
	if err != nil {
		return nil, fmt.Errorf("collect successions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []groupexport.Succession
	for rows.Next() {
		var s groupexport.Succession
		if err := rows.Scan(&s.OldConvID, &s.NewConvID, &s.Reason, &s.SucceededAt); err != nil {
			return nil, fmt.Errorf("scan succession: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func collectSpawnHist(d *sql.DB, convIDs []string) ([]groupexport.SpawnHist, error) {
	clause, args := inClause(convIDs)
	// spawner is agent-keyed (JOH-26 PR3a); resolve to the actor's current conv
	// and filter against the member conv set (current convs), mirroring
	// collectPermissions.
	rows, err := d.Query(`
		SELECT ag.current_conv_id, h.spawned_at
		FROM agent_spawn_history h JOIN agents ag ON ag.agent_id = h.spawner_agent_id
		WHERE ag.current_conv_id `+clause+` ORDER BY h.spawned_at`, args...)
	if err != nil {
		return nil, fmt.Errorf("collect spawn history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []groupexport.SpawnHist
	for rows.Next() {
		var s groupexport.SpawnHist
		if err := rows.Scan(&s.SpawnerConvID, &s.SpawnedAt); err != nil {
			return nil, fmt.Errorf("scan spawn history: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func collectCloneHist(d *sql.DB, convIDs []string) ([]groupexport.CloneHist, error) {
	clause, args := inClause(convIDs)
	// source is agent-keyed (JOH-26 PR3a); resolve to the actor's current conv
	// and filter against the member conv set, mirroring collectPermissions.
	rows, err := d.Query(`
		SELECT ag.current_conv_id, h.cloned_at
		FROM agent_clone_history h JOIN agents ag ON ag.agent_id = h.source_agent_id
		WHERE ag.current_conv_id `+clause+` ORDER BY h.cloned_at`, args...)
	if err != nil {
		return nil, fmt.Errorf("collect clone history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []groupexport.CloneHist
	for rows.Next() {
		var c groupexport.CloneHist
		if err := rows.Scan(&c.SourceConvID, &c.ClonedAt); err != nil {
			return nil, fmt.Errorf("scan clone history: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// collectConvStubs builds one Conv per member conv-id, filling SourceCwd
// and Title from conv_index where a row exists. Content and Missing are
// left for the daemon, which locates and reads the .jsonl. A member with
// no conv_index row still gets a stub (empty SourceCwd / Title) — the
// daemon resolves it from disk.
func collectConvStubs(d *sql.DB, convIDs []string) []groupexport.Conv {
	out := make([]groupexport.Conv, 0, len(convIDs))
	for _, id := range convIDs {
		c := groupexport.Conv{ConvID: id}
		var cwd, title string
		if err := d.QueryRow(`
			SELECT project_path, custom_title FROM conv_index WHERE conv_id = ?`, id).
			Scan(&cwd, &title); err == nil {
			c.SourceCwd = cwd
			c.Title = title
		}
		out = append(out, c)
	}
	return out
}

// GroupImportPlan is the fully-resolved input to ImportGroup. The daemon
// builds it: it has decided the target group name, resolved the target
// directory, and detected which member conv-ids collide locally — so
// ConvRemap maps EVERY source member conv-id to its final id (identity
// when there is no collision, a freshly minted id when there is).
type GroupImportPlan struct {
	Export     *groupexport.Export
	TargetName string            // resolved group name (--as value, or the source name)
	TargetCwd  string            // absolute --into path; every imported path column is set to this
	ConvRemap  map[string]string // source member conv-id → final conv-id
	ByConv     string            // caller conv-id for the audit log ("" = human)
}

// GroupImportResult summarises a completed import.
type GroupImportResult struct {
	GroupID            int64
	GroupName          string
	AgentCount         int
	MessageCount       int
	HeadAliasesSkipped []string // handles skipped because they already existed locally
}

// ImportGroup applies a GroupImportPlan in a single transaction: it
// creates the group and inserts every carried row, remapping conv-ids
// through plan.ConvRemap and setting every path column to plan.TargetCwd.
// The agent_transfer_log row is written inside the same transaction, so a
// rollback leaves no trace — no group, no rows, no log entry.
//
// It does NOT touch the filesystem; the daemon stages and moves the
// .jsonl files around this call.
func ImportGroup(plan GroupImportPlan) (*GroupImportResult, error) {
	if plan.Export == nil {
		return nil, errors.New("ImportGroup: nil export")
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	c := &importCtx{tx: tx, plan: plan, exp: plan.Export}
	if err := c.run(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("import commit: %w", err)
	}
	committed = true
	return &GroupImportResult{
		GroupID:            c.newGroupID,
		GroupName:          plan.TargetName,
		AgentCount:         len(plan.Export.Members),
		MessageCount:       len(plan.Export.Messages),
		HeadAliasesSkipped: c.skippedAliases,
	}, nil
}

// importCtx threads the open transaction, the plan, and the running
// group-id through the per-table insert steps.
type importCtx struct {
	tx             *sql.Tx
	plan           GroupImportPlan
	exp            *groupexport.Export
	newGroupID     int64
	skippedAliases []string
}

// rc remaps a single conv-id through the plan (identity when absent).
func (c *importCtx) rc(id string) string {
	if v, ok := c.plan.ConvRemap[id]; ok {
		return v
	}
	return id
}

// rl remaps every conv-id occurrence inside a free-form string —
// recipient lists (to_recipients / cc_recipients) and the like. conv-ids
// are 36-char UUIDs, so a plain substring replace cannot mis-hit, and
// this stays agnostic to the list's delimiter.
func (c *importCtx) rl(s string) string {
	for old, fresh := range c.plan.ConvRemap {
		if old != fresh && old != "" {
			s = strings.ReplaceAll(s, old, fresh)
		}
	}
	return s
}

func (c *importCtx) run() error {
	// Re-check the target name inside the transaction — closes the
	// window between the daemon's pre-flight check and this insert.
	var exists int
	if err := c.tx.QueryRow(`SELECT COUNT(*) FROM agent_groups WHERE name = ?`,
		c.plan.TargetName).Scan(&exists); err != nil {
		return fmt.Errorf("import: check name: %w", err)
	}
	if exists > 0 {
		return fmt.Errorf("%w: %q", ErrGroupNameTaken, c.plan.TargetName)
	}

	if err := c.group(); err != nil {
		return err
	}
	for _, step := range []func() error{
		c.members, c.owners, c.audit, c.permissions, c.enrollments,
		c.workdirs, c.sudoGrants, c.headAliases, c.successions,
		c.spawnHist, c.cloneHist, c.cronJobsAndRuns, c.messages,
		c.transferLog,
	} {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

func (c *importCtx) group() error {
	g := c.exp.Group
	// default_model is not inserted: the vestigial column was dropped
	// (JOH-220). A pre-v2 archive that still carried one is handled by
	// legacyDefaultModelProfile below, which synthesizes a default spawn
	// profile from it rather than resurrecting the column.
	res, err := c.tx.Exec(`
		INSERT INTO agent_groups
			(name, descr, default_cwd, default_context, max_members, created_at, archived_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.plan.TargetName, g.Descr, c.plan.TargetCwd, g.DefaultContext,
		g.MaxMembers, g.CreatedAt, g.ArchivedAt)
	if err != nil {
		return fmt.Errorf("import: create group: %w", err)
	}
	if c.newGroupID, err = res.LastInsertId(); err != nil {
		return fmt.Errorf("import: group id: %w", err)
	}
	return c.legacyDefaultModelProfile()
}

// legacyDefaultModelProfile preserves the spawn default of a PRE-v2
// (format v1) archive. Such an archive carries the retired per-group
// default_model (JOH-220); the current schema has no column for it, so the
// importer turns a non-empty value into a synthesized claude spawn profile
// — mirroring the v62 forward migration — and points the freshly imported
// group's default_profile at it, so the older export's effective spawn
// default does not silently regress. A v2 export leaves DefaultModel "",
// making this a no-op. The synthesized profile name is deduped against
// existing profiles, so a re-import (or a name already taken by a real
// profile) takes a numeric suffix instead of colliding.
func (c *importCtx) legacyDefaultModelProfile() error {
	model := strings.TrimSpace(c.exp.Group.DefaultModel)
	if model == "" {
		return nil
	}
	name, err := uniqueSpawnProfileName(c.tx, "group-default-"+c.plan.TargetName)
	if err != nil {
		return fmt.Errorf("import: pick legacy default-model profile name: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// harness 'claude': a legacy default_model passed the Claude-only model
	// gate by construction. Only name/harness/model are set; every other
	// field takes its column default.
	if _, err := c.tx.Exec(
		`INSERT INTO spawn_profiles (name, harness, model, created_at, updated_at)
		 VALUES (?, 'claude', ?, ?, ?)`,
		name, model, now, now); err != nil {
		return fmt.Errorf("import: synthesize legacy default-model profile: %w", err)
	}
	if _, err := c.tx.Exec(
		`UPDATE agent_groups SET default_profile = ? WHERE id = ?`,
		name, c.newGroupID); err != nil {
		return fmt.Errorf("import: point group at legacy default-model profile: %w", err)
	}
	return nil
}

func (c *importCtx) members() error {
	for _, m := range c.exp.Members {
		conv := c.rc(m.ConvID)
		agentID, err := ensureAgentForConvTx(c.tx, conv, "import")
		if err != nil {
			return fmt.Errorf("import: member %s actor: %w", m.ConvID, err)
		}
		if _, err := c.tx.Exec(`
			INSERT INTO agent_group_members (group_id, agent_id, role, descr, joined_at)
			VALUES (?, ?, ?, ?, ?)`,
			c.newGroupID, agentID, m.Role, m.Descr, m.JoinedAt); err != nil {
			return fmt.Errorf("import: member %s: %w", m.ConvID, err)
		}
	}
	return nil
}

func (c *importCtx) owners() error {
	for _, o := range c.exp.Owners {
		conv := c.rc(o.ConvID)
		agentID, err := ensureAgentForConvTx(c.tx, conv, "import")
		if err != nil {
			return fmt.Errorf("import: owner %s actor: %w", o.ConvID, err)
		}
		if _, err := c.tx.Exec(`
			INSERT INTO agent_group_owners (group_id, agent_id, granted_at, granted_by)
			VALUES (?, ?, ?, ?)`,
			c.newGroupID, agentID, o.GrantedAt, o.GrantedBy); err != nil {
			return fmt.Errorf("import: owner %s: %w", o.ConvID, err)
		}
	}
	return nil
}

func (c *importCtx) audit() error {
	for _, a := range c.exp.Audit {
		if _, err := c.tx.Exec(`
			INSERT INTO agent_group_audit (group_id, old_name, new_name, by_conv, at)
			VALUES (?, ?, ?, ?, ?)`,
			c.newGroupID, a.OldName, a.NewName, c.rc(a.ByConv), a.At); err != nil {
			return fmt.Errorf("import: audit row: %w", err)
		}
	}
	return nil
}

func (c *importCtx) permissions() error {
	for _, p := range c.exp.Permissions {
		conv := c.rc(p.ConvID)
		agentID, err := ensureAgentForConvTx(c.tx, conv, "import")
		if err != nil {
			return fmt.Errorf("import: permission %s/%s actor: %w", p.ConvID, p.Slug, err)
		}
		if _, err := c.tx.Exec(`
			INSERT INTO agent_permissions (agent_id, slug, effect, granted_at, granted_by)
			VALUES (?, ?, ?, ?, ?)`,
			agentID, p.Slug, p.Effect, p.GrantedAt, p.GrantedBy); err != nil {
			return fmt.Errorf("import: permission %s/%s: %w", p.ConvID, p.Slug, err)
		}
	}
	return nil
}

// enrollments translates the archive's legacy enrollment records onto the actor
// layer (JOH-26 PR3c). Each becomes an ensure-actor plus a carry of the actor
// facts the membership/permission imports don't cover: the spawn-time
// pending_name and, defensively, any retire state. ensureAgentForConvTx is
// idempotent, so it composes with the actor the members/permissions imports
// already created for this conv.
func (c *importCtx) enrollments() error {
	for _, e := range c.exp.Enrollments {
		conv := c.rc(e.ConvID)
		via := e.EnrolledVia
		if via == "" {
			via = "import"
		}
		agentID, err := ensureAgentForConvTx(c.tx, conv, via)
		if err != nil {
			return fmt.Errorf("import: enrollment %s actor: %w", e.ConvID, err)
		}
		// Restore the original creation timestamp (ensureAgentForConvTx — and the
		// members import before it — stamp `now` for a freshly-minted actor) so
		// the round-tripped agent keeps its birth time. Import is transactional
		// to a remapped conv-id on a clean target, so the actor is always freshly
		// created in this transaction; the archived timestamp is authoritative.
		if e.EnrolledAt != "" {
			if _, err := c.tx.Exec(`UPDATE agents SET created_at = ? WHERE agent_id = ?`,
				e.EnrolledAt, agentID); err != nil {
				return fmt.Errorf("import: enrollment %s created_at: %w", e.ConvID, err)
			}
		}
		if e.PendingName != "" {
			if _, err := c.tx.Exec(`UPDATE agents SET pending_name = ? WHERE agent_id = ?`,
				e.PendingName, agentID); err != nil {
				return fmt.Errorf("import: enrollment %s pending_name: %w", e.ConvID, err)
			}
		}
		if e.RetiredAt != "" {
			if _, err := c.tx.Exec(`UPDATE agents
				SET retired_at = ?, retired_by = ?, retire_reason = ? WHERE agent_id = ?`,
				e.RetiredAt, e.RetiredBy, e.RetireReason, agentID); err != nil {
				return fmt.Errorf("import: enrollment %s retire: %w", e.ConvID, err)
			}
		}
	}
	return nil
}

func (c *importCtx) workdirs() error {
	// Every imported agent's working directory is the import target —
	// the source machine's path is deliberately discarded. worktree_root
	// and branch are cleared: the import does not recreate worktrees.
	for _, w := range c.exp.Workdirs {
		if _, err := c.tx.Exec(`
			INSERT INTO agent_workdir (conv_id, dir, updated_at, worktree_root, branch)
			VALUES (?, ?, ?, '', '')`,
			c.rc(w.ConvID), c.plan.TargetCwd, w.UpdatedAt); err != nil {
			return fmt.Errorf("import: workdir %s: %w", w.ConvID, err)
		}
	}
	return nil
}

func (c *importCtx) sudoGrants() error {
	for _, s := range c.exp.SudoGrants {
		conv := c.rc(s.ConvID)
		agentID, err := ensureAgentForConvTx(c.tx, conv, "import")
		if err != nil {
			return fmt.Errorf("import: sudo grant %s/%s actor: %w", s.ConvID, s.Slug, err)
		}
		if _, err := c.tx.Exec(`
			INSERT INTO agent_sudo_grants
				(agent_id, slug, granted_at, expires_at, granted_by, reason, revoked_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			agentID, s.Slug, s.GrantedAt, s.ExpiresAt, s.GrantedBy,
			s.Reason, s.RevokedAt); err != nil {
			return fmt.Errorf("import: sudo grant %s/%s: %w", s.ConvID, s.Slug, err)
		}
	}
	return nil
}

func (c *importCtx) headAliases() error {
	// agent_head_aliases.handle is a global primary key and a
	// human-meaningful name. Per the import's naming rule — conv-ids are
	// mechanical (silently remapped), names are not — a handle that
	// already exists locally is NOT silently renamed: the row is skipped
	// (INSERT OR IGNORE) and reported. The imported agent works fine
	// without its handle alias; the human can re-point it.
	for _, h := range c.exp.HeadAliases {
		res, err := c.tx.Exec(`
			INSERT OR IGNORE INTO agent_head_aliases
				(handle, anchor_conv_id, created_at, by_conv)
			VALUES (?, ?, ?, ?)`,
			h.Handle, c.rc(h.AnchorConvID), h.CreatedAt, c.rc(h.ByConv))
		if err != nil {
			return fmt.Errorf("import: head alias %s: %w", h.Handle, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			c.skippedAliases = append(c.skippedAliases, h.Handle)
		}
	}
	return nil
}

func (c *importCtx) successions() error {
	// old_conv_id is the table's primary key; a non-remapped endpoint
	// could already have a succession row locally — INSERT OR IGNORE
	// keeps the import all-or-nothing-safe without a hard failure.
	for _, s := range c.exp.Successions {
		if _, err := c.tx.Exec(`
			INSERT OR IGNORE INTO agent_conv_succession
				(old_conv_id, new_conv_id, reason, succeeded_at)
			VALUES (?, ?, ?, ?)`,
			c.rc(s.OldConvID), c.rc(s.NewConvID), s.Reason, s.SucceededAt); err != nil {
			return fmt.Errorf("import: succession %s: %w", s.OldConvID, err)
		}
	}
	return nil
}

func (c *importCtx) spawnHist() error {
	// History is keyed on the spawner's actor (JOH-26 PR3a); resolve the
	// remapped conv to its agent, mirroring members()/permissions().
	for _, s := range c.exp.SpawnHist {
		agentID, err := ensureAgentForConvTx(c.tx, c.rc(s.SpawnerConvID), "import")
		if err != nil {
			return fmt.Errorf("import: spawn history actor: %w", err)
		}
		if _, err := c.tx.Exec(`
			INSERT INTO agent_spawn_history (spawner_agent_id, spawned_at)
			VALUES (?, ?)`, agentID, s.SpawnedAt); err != nil {
			return fmt.Errorf("import: spawn history: %w", err)
		}
	}
	return nil
}

func (c *importCtx) cloneHist() error {
	// History is keyed on the source's actor (JOH-26 PR3a); resolve the
	// remapped conv to its agent.
	for _, h := range c.exp.CloneHist {
		agentID, err := ensureAgentForConvTx(c.tx, c.rc(h.SourceConvID), "import")
		if err != nil {
			return fmt.Errorf("import: clone history actor: %w", err)
		}
		if _, err := c.tx.Exec(`
			INSERT INTO agent_clone_history (source_agent_id, cloned_at)
			VALUES (?, ?)`, agentID, h.ClonedAt); err != nil {
			return fmt.Errorf("import: clone history: %w", err)
		}
	}
	return nil
}

// rcToAgent remaps an exported conv (c.rc) then resolves it to its owning
// actor, returning "" for an empty conv (a group-target or owner-less cron ref)
// so the agent-keyed column stays empty rather than minting a bogus actor for
// "". Used by the cron import to populate owner_agent / target_agent (JOH-26
// PR3a).
func (c *importCtx) rcToAgent(convID string) (string, error) {
	conv := c.rc(convID)
	if conv == "" {
		return "", nil
	}
	return ensureAgentForConvTx(c.tx, conv, "import")
}

func (c *importCtx) cronJobsAndRuns() error {
	// Cron jobs carry an autoincrement id that cron_runs reference; the
	// import assigns fresh ids, so old→new is captured here and applied
	// to the run rows.
	jobIDMap := make(map[int64]int64, len(c.exp.CronJobs))
	for _, j := range c.exp.CronJobs {
		ownerAgent, err := c.rcToAgent(j.OwnerConv)
		if err != nil {
			return fmt.Errorf("import: cron job %q owner actor: %w", j.Name, err)
		}
		targetAgent, err := c.rcToAgent(j.TargetConv)
		if err != nil {
			return fmt.Errorf("import: cron job %q target actor: %w", j.Name, err)
		}
		// Preserve the conv/group discriminator so a group fan-out job doesn't
		// round-trip as a (broken, empty-target) conv job. Older archives carry
		// no target_kind — default it to the v41 column default ("conv").
		kind := j.TargetKind
		if kind == "" {
			kind = CronTargetConv
		}
		res, err := c.tx.Exec(`
			INSERT INTO agent_cron_jobs
				(name, target_kind, owner_agent, target_agent, group_id, interval_seconds,
				 subject, body, enabled, created_at, last_run_at, last_run_status)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			j.Name, kind, ownerAgent, targetAgent, c.newGroupID,
			j.IntervalSeconds, j.Subject, j.Body, j.Enabled, j.CreatedAt,
			j.LastRunAt, j.LastRunStatus)
		if err != nil {
			return fmt.Errorf("import: cron job %q: %w", j.Name, err)
		}
		newID, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("import: cron job id: %w", err)
		}
		jobIDMap[j.ID] = newID
	}
	for _, r := range c.exp.CronRuns {
		newJobID, ok := jobIDMap[r.JobID]
		if !ok {
			continue // run for a job not in this export — skip
		}
		if _, err := c.tx.Exec(`
			INSERT INTO agent_cron_runs (job_id, fired_at, status, error_msg)
			VALUES (?, ?, ?, ?)`, newJobID, r.FiredAt, r.Status, r.ErrorMsg); err != nil {
			return fmt.Errorf("import: cron run: %w", err)
		}
	}
	return nil
}

func (c *importCtx) messages() error {
	// agent_messages.id is autoincrement and parent_id chains replies by
	// it. Insert every message first, capturing old→new ids, then a
	// second pass rewrites parent_id; a parent outside the exported set
	// collapses to 0 ("top of thread").
	msgIDMap := make(map[int64]int64, len(c.exp.Messages))
	for _, m := range c.exp.Messages {
		// Derive the actor refs from the REMAPPED convs, the same COALESCE
		// join InsertAgentMessage and migrateV75toV76 use — so an imported
		// message agrees with a freshly-sent one. agent_conversations is
		// already populated for this import: enrollments() (+ successions())
		// run before messages() and ensureAgentForConvTx every imported conv,
		// so a message whose remapped conv is an actor resolves here; a
		// non-actor conv falls through COALESCE to ''.
		res, err := c.tx.Exec(`
			INSERT INTO agent_messages
				(group_id, from_conv, to_conv, from_agent, to_agent, subject, body, created_at,
				 delivered_at, read_at, parent_id, to_recipients, cc_recipients,
				 original_to_conv)
			VALUES (?, ?, ?,
			 COALESCE((SELECT agent_id FROM agent_conversations WHERE conv_id = ?), ''),
			 COALESCE((SELECT agent_id FROM agent_conversations WHERE conv_id = ?), ''),
			 ?, ?, ?, ?, ?, 0, ?, ?, ?)`,
			c.newGroupID, c.rc(m.FromConv), c.rc(m.ToConv), c.rc(m.FromConv), c.rc(m.ToConv),
			m.Subject, m.Body, m.CreatedAt, m.DeliveredAt, m.ReadAt, c.rl(m.ToRecipients),
			c.rl(m.CcRecipients), c.rc(m.OriginalToConv))
		if err != nil {
			return fmt.Errorf("import: message %d: %w", m.ID, err)
		}
		newID, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("import: message id: %w", err)
		}
		msgIDMap[m.ID] = newID
	}
	for _, m := range c.exp.Messages {
		if m.ParentID == 0 {
			continue
		}
		newParent, ok := msgIDMap[m.ParentID]
		if !ok {
			continue // parent outside the export — leave parent_id 0
		}
		if _, err := c.tx.Exec(`UPDATE agent_messages SET parent_id = ? WHERE id = ?`,
			newParent, msgIDMap[m.ID]); err != nil {
			return fmt.Errorf("import: message parent link: %w", err)
		}
	}
	return nil
}

func (c *importCtx) transferLog() error {
	remaps := map[string]string{}
	for old, fresh := range c.plan.ConvRemap {
		if old != fresh {
			remaps[old] = fresh
		}
	}
	remapJSON := "{}"
	if len(remaps) > 0 {
		if b, err := json.Marshal(remaps); err == nil {
			remapJSON = string(b)
		}
	}
	_, err := insertTransferLog(c.tx, TransferLogEntry{
		Kind:          TransferKindImport,
		At:            time.Now().UTC(),
		FormatVersion: c.exp.FormatVersion,
		SourceGroup:   c.exp.SourceGroup,
		SourceHome:    c.exp.SourceHome,
		SourceOS:      c.exp.SourceOS,
		ResultGroup:   c.plan.TargetName,
		TargetDir:     c.plan.TargetCwd,
		ConvRemaps:    remapJSON,
		AgentCount:    len(c.exp.Members),
		MessageCount:  len(c.exp.Messages),
		ByConv:        c.plan.ByConv,
	})
	if err != nil {
		return fmt.Errorf("import: transfer log: %w", err)
	}
	return nil
}
