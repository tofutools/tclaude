package db

import (
	"database/sql"
	"errors"
	"time"
)

// QueuedSpawn is a prepared, not-yet-launched spawn (queued_spawns, schema
// v268). Draft is the dashboard spawn dialog's form state as JSON; the daemon
// never interprets it beyond the group it targets.
type QueuedSpawn struct {
	ID        int64
	Group     string
	Draft     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ErrQueuedSpawnNotFound reports an update or delete of an absent row.
var ErrQueuedSpawnNotFound = errors.New("queued spawn not found")

// ListQueuedSpawns returns every queued spawn, oldest first.
func ListQueuedSpawns() ([]QueuedSpawn, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id, group_name, draft, created_at, updated_at FROM queued_spawns ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []QueuedSpawn{}
	for rows.Next() {
		var q QueuedSpawn
		var created, updated int64
		if err := rows.Scan(&q.ID, &q.Group, &q.Draft, &created, &updated); err != nil {
			return nil, err
		}
		q.CreatedAt, q.UpdatedAt = time.Unix(0, created), time.Unix(0, updated)
		out = append(out, q)
	}
	return out, rows.Err()
}

// GetQueuedSpawn returns one queued spawn, or nil when absent.
func GetQueuedSpawn(id int64) (*QueuedSpawn, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var q QueuedSpawn
	var created, updated int64
	err = d.QueryRow(`SELECT id, group_name, draft, created_at, updated_at FROM queued_spawns WHERE id = ?`, id).
		Scan(&q.ID, &q.Group, &q.Draft, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	q.CreatedAt, q.UpdatedAt = time.Unix(0, created), time.Unix(0, updated)
	return &q, nil
}

// CreateQueuedSpawn stores a new queued spawn and returns its id.
func CreateQueuedSpawn(group, draft string, now time.Time) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`INSERT INTO queued_spawns (group_name, draft, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		group, draft, now.UnixNano(), now.UnixNano())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateQueuedSpawn replaces a queued spawn's draft, keeping its queue position.
func UpdateQueuedSpawn(id int64, group, draft string, now time.Time) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE queued_spawns SET group_name = ?, draft = ?, updated_at = ? WHERE id = ?`,
		group, draft, now.UnixNano(), id)
	if err != nil {
		return err
	}
	return requireQueuedSpawnRow(res)
}

// DeleteQueuedSpawn removes a queued spawn.
func DeleteQueuedSpawn(id int64) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`DELETE FROM queued_spawns WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return requireQueuedSpawnRow(res)
}

func requireQueuedSpawnRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrQueuedSpawnNotFound
	}
	return nil
}
