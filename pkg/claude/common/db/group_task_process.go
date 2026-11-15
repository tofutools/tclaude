package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// GroupTaskProcessNone is a task's choice to run no process, whatever its
// board's default way of working says.
const GroupTaskProcessNone = "none"

// GroupTaskProcess is a task's own choice of process: a template id,
// GroupTaskProcessNone, or "" when it follows its board's default.
func GroupTaskProcess(taskID string) (string, error) {
	d, err := Open()
	if err != nil {
		return "", err
	}
	var process string
	err = d.QueryRow(`SELECT process FROM group_task_process WHERE task_id = ?`, taskID).Scan(&process)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return process, err
}

// GroupTaskProcesses is GroupTaskProcess for many tasks at once: those of
// ids with a choice of their own, by id.
func GroupTaskProcesses(ids []string) (map[string]string, error) {
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(len(ids), start+500)]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := d.Query(`SELECT task_id, process FROM group_task_process WHERE task_id IN (?`+
			strings.Repeat(",?", len(chunk)-1)+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, process string
			if err := rows.Scan(&id, &process); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[id] = process
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SetGroupTaskProcess records a task's choice of process: a template id or
// GroupTaskProcessNone; "" clears it, so the task follows its board again.
func SetGroupTaskProcess(taskID, process string) error {
	if len(process) > 128 {
		return fmt.Errorf("process %q is too long", process)
	}
	d, err := Open()
	if err != nil {
		return err
	}
	if process == "" {
		_, err = d.Exec(`DELETE FROM group_task_process WHERE task_id = ?`, taskID)
		return err
	}
	_, err = d.Exec(`INSERT INTO group_task_process (task_id, process) VALUES (?, ?)
		ON CONFLICT(task_id) DO UPDATE SET process = excluded.process`, taskID, process)
	return err
}
