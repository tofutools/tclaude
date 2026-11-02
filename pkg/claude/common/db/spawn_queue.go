package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Spawn queue states. queued and launching are active; the rest are final.
const (
	SpawnQueueQueued    = "queued"
	SpawnQueueLaunching = "launching"
	SpawnQueueLaunched  = "launched"
	SpawnQueueFailed    = "failed"
	SpawnQueueCancelled = "cancelled"
)

// Who asked for a queued spawn, and so who the launch runs as.
const (
	SpawnRequesterAgent     = "agent"
	SpawnRequesterHuman     = "human"
	SpawnRequesterDashboard = "dashboard"
	// SpawnRequesterFederation is a remote peer's spawn request, held in
	// federation_spawn_requests (FedRequestID) rather than as a request body.
	SpawnRequesterFederation = "federation"
)

// ErrSpawnQueueFull is returned when capacity.max_queued entries already wait.
var ErrSpawnQueueFull = errors.New("spawn queue is full")

// SpawnQueueEntry is one waiting (or finished) capacity-queued spawn. The
// request body is deliberately not a field: it is read only by the launcher,
// through SpawnQueueBody, so no listing can carry it.
type SpawnQueueEntry struct {
	ID              int64
	GroupID         int64
	GroupName       string
	Name            string
	RequesterKind   string
	RequesterAgent  string
	RequesterName   string
	RequesterMember bool
	// ProofDirs are the launch directories the requester proved it can write
	// when the spawn was queued; the launch re-checks them as canonical paths.
	ProofDirs     []string
	State         string
	Reason        string
	ReservedAgent string
	// FedRequestID is the federation spawn request a remote entry holds a
	// place for; zero for local entries.
	FedRequestID int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
	// Position is the 1-based place among active entries; zero once final.
	Position int
}

// Active reports whether the entry still waits or is being launched.
func (e *SpawnQueueEntry) Active() bool {
	return e.State == SpawnQueueQueued || e.State == SpawnQueueLaunching
}

// InsertSpawnQueueEntry appends e with its request body, refusing past
// maxQueued active entries. It returns the new id and 1-based position.
func InsertSpawnQueueEntry(e *SpawnQueueEntry, body string, maxQueued int) (int64, int, error) {
	d, err := Open()
	if err != nil {
		return 0, 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	id, position, err := insertSpawnQueueEntryTx(tx, e, body, maxQueued)
	if err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	NotifyStatusChanged()
	return id, position, nil
}

// InsertQueuedFederationSpawnRequest stores a remote spawn request with status
// queued and its queue entry together, so neither exists without the other.
// The request's errors are InsertFederationSpawnRequest's; the queue's,
// InsertSpawnQueueEntry's. It returns the request id, entry id and position.
func InsertQueuedFederationSpawnRequest(r *FederationSpawnRequest, pendingLimit int, e *SpawnQueueEntry, maxQueued int) (int64, int64, int, error) {
	d, err := Open()
	if err != nil {
		return 0, 0, 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	reqID, err := insertFederationSpawnRequestTx(tx, r, FedSpawnQueued, pendingLimit)
	if err != nil {
		return 0, 0, 0, err
	}
	e.RequesterKind, e.FedRequestID = SpawnRequesterFederation, reqID
	id, position, err := insertSpawnQueueEntryTx(tx, e, "", maxQueued)
	if err != nil {
		return 0, 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, 0, err
	}
	NotifyStatusChanged()
	return reqID, id, position, nil
}

func insertSpawnQueueEntryTx(tx *sql.Tx, e *SpawnQueueEntry, body string, maxQueued int) (int64, int, error) {
	var active int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM spawn_queue WHERE state IN (?, ?)`,
		SpawnQueueQueued, SpawnQueueLaunching).Scan(&active); err != nil {
		return 0, 0, err
	}
	if maxQueued > 0 && active >= maxQueued {
		return 0, 0, ErrSpawnQueueFull
	}
	dirs := ""
	if len(e.ProofDirs) > 0 {
		raw, _ := json.Marshal(e.ProofDirs)
		dirs = string(raw)
	}
	now := time.Now()
	var stored any = body
	if e.FedRequestID != 0 {
		stored = nil
	}
	res, err := tx.Exec(`INSERT INTO spawn_queue (group_id, name, requester_kind, requester_agent, requester_name,
			requester_member, request_body, proof_dirs, state, fed_request_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.GroupID, e.Name, e.RequesterKind, e.RequesterAgent, e.RequesterName, e.RequesterMember,
		stored, dirs, SpawnQueueQueued, e.FedRequestID, dbTime(now), dbTime(now))
	if err != nil {
		return 0, 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, 0, err
	}
	return id, active + 1, nil
}

const spawnQueueColumns = `q.id, q.group_id, COALESCE(g.name, ''), q.name, q.requester_kind, q.requester_agent,
	q.requester_name, q.requester_member, q.proof_dirs, q.state, q.reason, q.reserved_agent, q.fed_request_id, q.created_at, q.updated_at`

func scanSpawnQueueEntry(row interface{ Scan(...any) error }) (*SpawnQueueEntry, error) {
	var e SpawnQueueEntry
	var dirs string
	var created, updated dbTimestamp
	if err := row.Scan(&e.ID, &e.GroupID, &e.GroupName, &e.Name, &e.RequesterKind, &e.RequesterAgent,
		&e.RequesterName, &e.RequesterMember, &dirs, &e.State, &e.Reason, &e.ReservedAgent, &e.FedRequestID, &created, &updated); err != nil {
		return nil, err
	}
	if dirs != "" {
		if err := json.Unmarshal([]byte(dirs), &e.ProofDirs); err != nil {
			return nil, err
		}
	}
	e.CreatedAt, e.UpdatedAt = created.Time(), updated.Time()
	return &e, nil
}

// ListSpawnQueue returns active entries in queue order, with positions, and
// with includeFinal also the final ones still kept, newest last.
func ListSpawnQueue(includeFinal bool) ([]*SpawnQueueEntry, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	where := `WHERE q.state IN ('queued', 'launching')`
	if includeFinal {
		where = ""
	}
	rows, err := d.Query(`SELECT ` + spawnQueueColumns + `
		FROM spawn_queue q LEFT JOIN agent_groups g ON g.id = q.group_id ` + where + ` ORDER BY q.id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*SpawnQueueEntry
	position := 0
	for rows.Next() {
		e, err := scanSpawnQueueEntry(rows)
		if err != nil {
			return nil, err
		}
		if e.Active() {
			position++
			e.Position = position
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetSpawnQueueEntry returns the entry with its position, or nil.
func GetSpawnQueueEntry(id int64) (*SpawnQueueEntry, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	e, err := scanSpawnQueueEntry(d.QueryRow(`SELECT `+spawnQueueColumns+`
		FROM spawn_queue q LEFT JOIN agent_groups g ON g.id = q.group_id WHERE q.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if e.Active() {
		if err := d.QueryRow(`SELECT COUNT(*) FROM spawn_queue WHERE state IN ('queued', 'launching') AND id <= ?`,
			id).Scan(&e.Position); err != nil {
			return nil, err
		}
	}
	return e, nil
}

// ActiveSpawnQueueEntryForFederationRequest returns the active entry holding a
// place for remote spawn request reqID, with its position, or nil.
func ActiveSpawnQueueEntryForFederationRequest(reqID int64) (*SpawnQueueEntry, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var id int64
	err = d.QueryRow(`SELECT id FROM spawn_queue WHERE fed_request_id = ? AND state IN ('queued', 'launching')`, reqID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return GetSpawnQueueEntry(id)
}

// CountActiveSpawnQueue counts entries that are waiting or being launched.
func CountActiveSpawnQueue() (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	var n int
	err = d.QueryRow(`SELECT COUNT(*) FROM spawn_queue WHERE state IN ('queued', 'launching')`).Scan(&n)
	return n, err
}

// SpawnQueueBody returns the stored request body of an active entry.
func SpawnQueueBody(id int64) (string, error) {
	d, err := Open()
	if err != nil {
		return "", err
	}
	var body sql.NullString
	err = d.QueryRow(`SELECT request_body FROM spawn_queue WHERE id = ?`, id).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return body.String, err
}

// ClaimSpawnQueueEntry moves a queued entry to launching with the agent id its
// launch will use. False when another claim, a cancel or a finish got there first.
func ClaimSpawnQueueEntry(id int64, reservedAgent string) (bool, error) {
	return spawnQueueTransition(`UPDATE spawn_queue SET state = 'launching', reserved_agent = ?, updated_at = ?
		WHERE id = ? AND state = 'queued'`, reservedAgent, dbTime(time.Now()), id)
}

// RequeueSpawnQueueEntry puts a launching entry back in its place, for a launch
// that found capacity gone or never reached the spawner.
func RequeueSpawnQueueEntry(id int64) (bool, error) {
	return spawnQueueTransition(`UPDATE spawn_queue SET state = 'queued', reserved_agent = '', updated_at = ?
		WHERE id = ? AND state = 'launching'`, dbTime(time.Now()), id)
}

// FinishSpawnQueueEntry gives an active entry its final state and drops its
// request body. False when the entry was already final.
func FinishSpawnQueueEntry(id int64, state, reason string) (bool, error) {
	return spawnQueueTransition(`UPDATE spawn_queue SET state = ?, reason = ?, request_body = NULL, updated_at = ?
		WHERE id = ? AND state IN ('queued', 'launching')`, state, reason, dbTime(time.Now()), id)
}

// CancelSpawnQueueEntry cancels a waiting entry and drops its request body.
// An entry being launched cannot be cancelled. False when it no longer waits.
func CancelSpawnQueueEntry(id int64) (bool, error) {
	return spawnQueueTransition(`UPDATE spawn_queue SET state = 'cancelled', reason = 'cancelled', request_body = NULL, updated_at = ?
		WHERE id = ? AND state = 'queued'`, dbTime(time.Now()), id)
}

// PruneSpawnQueue deletes final entries last changed before cutoff.
func PruneSpawnQueue(cutoff time.Time) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM spawn_queue WHERE state NOT IN ('queued', 'launching') AND updated_at < ?`, dbTime(cutoff))
	return err
}

func spawnQueueTransition(query string, args ...any) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(query, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n > 0 {
		NotifyStatusChanged()
	}
	return n > 0, nil
}
