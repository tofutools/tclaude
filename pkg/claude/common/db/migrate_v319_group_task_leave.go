package db

import "database/sql"

// migrateV318toV319 unassigns an agent's live tasks on a group's board when
// the agent leaves that group (tcl-nl18ib), whichever path removes the
// membership, and records it in the task's history.
//
// Membership is agent-keyed and reincarnation keeps the agent id
// (RotateAgentConv never touches agent_group_members), so a reincarnated
// agent keeps its tasks. Re-adding a member (AddAgentGroupMember's INSERT OR
// REPLACE) does not fire the trigger either: SQLite runs no delete triggers
// for a REPLACE while recursive_triggers is off, as it is here. Retirement
// unassigns with its own "assignee retired" detail before it drops the
// memberships, so this trigger finds nothing left to do there.
func migrateV318toV319(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TRIGGER IF NOT EXISTS group_tasks_unassign_on_leave
		AFTER DELETE ON agent_group_members
		BEGIN
			INSERT INTO group_task_events (task_id, at, actor, kind, detail)
				SELECT id, CAST((julianday('now') - 2440587.5) * 86400000000000 AS INTEGER),
					'system:membership', 'unassigned', 'assignee left the group'
				FROM group_tasks
				WHERE group_id = OLD.group_id AND assignee_id = OLD.agent_id AND status IN ('open','blocked');
			UPDATE group_tasks SET assignee_id = NULL, assigned_by = '', assigned_at = NULL,
					updated_at = CAST((julianday('now') - 2440587.5) * 86400000000000 AS INTEGER)
				WHERE group_id = OLD.group_id AND assignee_id = OLD.agent_id AND status IN ('open','blocked');
		END`,
		`UPDATE schema_version SET version = 319`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
