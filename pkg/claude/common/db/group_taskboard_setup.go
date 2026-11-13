package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/groupexport"
)

// A board's setup (groupexport.Taskboard): its options, custom columns and
// column states, never its tasks. A group export, a clone and a group
// template carry it; the daemon checks a setup from outside (a file, a
// template) before it is applied here.

// GroupTaskboardSetup reads group's board setup; nil when the board sets
// nothing (no options, no custom column, no hidden built-in column).
func GroupTaskboardSetup(groupID int64) (*groupexport.Taskboard, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return groupTaskboardSetupTx(d, groupID)
}

func groupTaskboardSetupTx(q taskTx, groupID int64) (*groupexport.Taskboard, error) {
	opts, err := getGroupTaskboardOptionsTx(q, groupID)
	if err != nil {
		return nil, err
	}
	cols, err := listGroupTaskColumnsTx(q, groupID)
	if err != nil {
		return nil, err
	}
	states, err := listGroupTaskColumnStatesTx(q, groupID)
	if err != nil {
		return nil, err
	}
	tb := &groupexport.Taskboard{}
	if raw := strings.TrimSpace(opts.Options); raw != "" && raw != "{}" {
		tb.Options = json.RawMessage(raw)
	}
	for _, c := range cols {
		tb.Columns = append(tb.Columns, groupexport.TaskboardColumn{Slug: c.Slug, Label: c.Label, State: states[c.Slug]})
	}
	for _, slug := range []string{GroupTaskColumnIdeas, GroupTaskColumnDone} {
		if st := states[slug]; st != "" {
			if tb.BuiltinStates == nil {
				tb.BuiltinStates = map[string]string{}
			}
			tb.BuiltinStates[slug] = st
		}
	}
	if tb.Options == nil && tb.Columns == nil && tb.BuiltinStates == nil {
		return nil, nil
	}
	return tb, nil
}

// ApplyGroupTaskboardSetup gives group's board the setup tb: its columns
// (after the board's own, up to the cap; one the board already has is
// kept as it is), their states, and its options in place of the board's.
// The setup must already be checked against the board (the daemon's
// sanitizeTaskboardSetup), its options against the columns it will have.
func ApplyGroupTaskboardSetup(groupID int64, tb *groupexport.Taskboard, actor string) error {
	if tb == nil {
		return nil
	}
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := applyGroupTaskboardSetupTx(tx, groupID, tb, actor, time.Now()); err != nil {
		return err
	}
	return tx.Commit()
}

func applyGroupTaskboardSetupTx(tx *sql.Tx, groupID int64, tb *groupexport.Taskboard, actor string, now time.Time) error {
	if tb == nil {
		return nil
	}
	cols, err := listGroupTaskColumnsTx(tx, groupID)
	if err != nil {
		return err
	}
	for _, c := range tb.Columns {
		if columnIndex(cols, c.Slug) < 0 && len(cols) < GroupTaskColumnsCap {
			if _, err := tx.Exec(`INSERT INTO group_task_columns (group_id, slug, label, position, created_by, created_at)
				VALUES (?, ?, ?, ?, ?, ?)`, groupID, c.Slug, c.Label, len(cols), actor, dbTime(now)); err != nil {
				return fmt.Errorf("board column %s: %w", c.Slug, err)
			}
			cols = append(cols, GroupTaskColumn{GroupID: groupID, Slug: c.Slug, Label: c.Label})
		}
		if c.State != "" && c.State != GroupTaskColumnShown && columnIndex(cols, c.Slug) >= 0 {
			if err := setColumnStateTx(tx, groupID, c.Slug, c.State); err != nil {
				return err
			}
		}
	}
	for slug, st := range tb.BuiltinStates {
		if st != "" && st != GroupTaskColumnShown {
			if err := setColumnStateTx(tx, groupID, slug, st); err != nil {
				return err
			}
		}
	}
	if len(tb.Options) == 0 {
		return nil
	}
	if _, err := tx.Exec(`INSERT INTO group_taskboard_options (group_id, options, version, updated_by, updated_at)
		VALUES (?, ?, 1, ?, ?)
		ON CONFLICT (group_id) DO UPDATE SET options = excluded.options, version = group_taskboard_options.version + 1,
			updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		groupID, string(tb.Options), actor, dbTime(now)); err != nil {
		return fmt.Errorf("board options: %w", err)
	}
	return nil
}

func setColumnStateTx(tx *sql.Tx, groupID int64, slug, state string) error {
	_, err := tx.Exec(`INSERT INTO group_task_column_states (group_id, slug, state) VALUES (?, ?, ?)
		ON CONFLICT (group_id, slug) DO UPDATE SET state = excluded.state`, groupID, slug, state)
	if err != nil {
		return fmt.Errorf("board column state %s: %w", slug, err)
	}
	return nil
}
