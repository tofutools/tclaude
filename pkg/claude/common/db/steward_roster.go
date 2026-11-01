package db

import (
	"database/sql"
	"errors"
	"time"
)

// Seats on the Steward roster.
const (
	StewardSeatActive  = "active"
	StewardSeatStandby = "standby"
)

// StewardRosterEntry is one agent on the roster.
type StewardRosterEntry struct {
	AgentID string
	Seat    string
	Since   time.Time
	SetBy   string
}

// ActiveStewardAgent returns the agent in the active seat, "" when none is.
func ActiveStewardAgent() (string, error) {
	d, err := Open()
	if err != nil {
		return "", err
	}
	var agentID string
	err = d.QueryRow(`SELECT agent_id FROM steward_roster WHERE seat = ?`, StewardSeatActive).Scan(&agentID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return agentID, err
}

// ListStewardRoster returns the roster, longest-held seat first.
func ListStewardRoster() ([]StewardRosterEntry, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT agent_id, seat, since, set_by FROM steward_roster ORDER BY since, agent_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []StewardRosterEntry
	for rows.Next() {
		var entry StewardRosterEntry
		var since int64
		if err := rows.Scan(&entry.AgentID, &entry.Seat, &since, &entry.SetBy); err != nil {
			return nil, err
		}
		entry.Since = time.Unix(0, since)
		out = append(out, entry)
	}
	return out, rows.Err()
}

// PauseStewardStandbyJobs pauses what a Steward going on the roster owns: its
// enabled cron jobs and standing orders, together, as a retirement does. Only
// enabled rows take the marker, so one the human disabled stays disabled when
// ResumeStewardStandbyJobs runs. It returns how many it paused.
func PauseStewardStandbyJobs(agentID string) (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	jobs, err := tx.Exec(`UPDATE agent_cron_jobs SET enabled = 0, disabled_reason = ?
		WHERE owner_agent = ? AND enabled = 1`, CronDisabledReasonStewardStandby, agentID)
	if err != nil {
		return 0, err
	}
	orders, err := tx.Exec(`UPDATE agent_standing_orders
		SET enabled = 0, disabled_reason = ?, row_version = row_version + 1, updated_at = ?
		WHERE owner_agent = ? AND enabled = 1`,
		StandingDisabledReasonStewardStandby, formatStandingTime(time.Now()), agentID)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	paused, _ := jobs.RowsAffected()
	pausedOrders, _ := orders.RowsAffected()
	return int(paused + pausedOrders), nil
}

// ResumeStewardStandbyJobs undoes PauseStewardStandbyJobs for an agent back in
// the seat. The marker is the crux: only what that call paused runs again. The
// last run is left alone, so a long pause does not fire a flood of catch-ups.
func ResumeStewardStandbyJobs(agentID string) (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	jobs, err := tx.Exec(`UPDATE agent_cron_jobs SET enabled = 1, disabled_reason = ''
		WHERE owner_agent = ? AND disabled_reason = ?`, agentID, CronDisabledReasonStewardStandby)
	if err != nil {
		return 0, err
	}
	orders, err := tx.Exec(`UPDATE agent_standing_orders
		SET enabled = 1, disabled_reason = '', row_version = row_version + 1, updated_at = ?
		WHERE owner_agent = ? AND disabled_reason = ?`,
		formatStandingTime(time.Now()), agentID, StandingDisabledReasonStewardStandby)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	resumed, _ := jobs.RowsAffected()
	resumedOrders, _ := orders.RowsAffected()
	return int(resumed + resumedOrders), nil
}

// SetActiveSteward seats an agent and takes the seat's last holder off the
// roster, all at once. The caller says what became of that holder.
func SetActiveSteward(agentID, by string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM steward_roster WHERE seat = ? OR agent_id = ?`, StewardSeatActive, agentID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO steward_roster (agent_id, seat, since, set_by) VALUES (?, ?, ?, ?)`,
		agentID, StewardSeatActive, dbTime(time.Now()), by); err != nil {
		return err
	}
	return tx.Commit()
}

// SetStewardStandby puts an agent on the roster, out of the active seat.
func SetStewardStandby(agentID, by string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO steward_roster (agent_id, seat, since, set_by) VALUES (?, ?, ?, ?)
		ON CONFLICT(agent_id) DO UPDATE SET seat = excluded.seat, since = excluded.since, set_by = excluded.set_by`,
		agentID, StewardSeatStandby, dbTime(time.Now()), by)
	return err
}

// RemoveFromStewardRoster takes an agent off the roster, whatever its seat.
func RemoveFromStewardRoster(agentID string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM steward_roster WHERE agent_id = ?`, agentID)
	return err
}

// ForgetSteward drops everything held about an agent as a Steward: retiring ends
// that for good, and a reinstated agent starts over.
func ForgetSteward(agentID string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM steward_roster WHERE agent_id = ?`, agentID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM steward_offered_permissions WHERE agent_id = ?`, agentID); err != nil {
		return err
	}
	return tx.Commit()
}

// CloseStewardRecord ends an agent's time as a Steward: the record no longer
// counts as open, and the given slugs may be offered again if it returns. A slug
// left in the record is one the operator took off, which stays off.
func CloseStewardRecord(agentID string, slugs ...string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, slug := range append(slugs, StewardRecordOpened, StewardRecordPromoted) {
		if _, err := tx.Exec(`DELETE FROM steward_offered_permissions WHERE agent_id = ? AND slug = ?`, agentID, slug); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// EndInheritedStewardSudo revokes the temporary grants an agent holds only by
// having been a Steward. The stored grants behind them stay, for later Stewards.
func EndInheritedStewardSudo(agentID, inheritedBy string) ([]string, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	now := dbTime(time.Now())
	rows, err := tx.Query(`SELECT slug FROM agent_sudo_grants
		WHERE agent_id = ? AND granted_by = ? AND revoked_at IS NULL AND expires_at > ?`, agentID, inheritedBy, now)
	if err != nil {
		return nil, err
	}
	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			_ = rows.Close()
			return nil, err
		}
		slugs = append(slugs, slug)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE agent_sudo_grants SET revoked_at = ?
		WHERE agent_id = ? AND granted_by = ? AND revoked_at IS NULL`, now, agentID, inheritedBy); err != nil {
		return nil, err
	}
	return slugs, tx.Commit()
}
