package db

import (
	"database/sql"
	"errors"
	"time"

	platformexec "github.com/tofutools/tclaude/pkg/claude/platform/execution"
)

// BackgroundProjectionRef identifies the exact session-row attempt whose
// background ledgers were observed. CreatedAt remains part of the fence for
// legacy rows without an ExecutionID; MainPID prevents an old sample from
// crossing a main-process replacement within an otherwise unchanged row.
type BackgroundProjectionRef struct {
	Attempt          platformexec.AttemptRef
	SessionCreatedAt time.Time
	MainPID          int
}

// ProjectSessionBackgroundLedgers conditionally replaces both background
// ledgers as one projection. Both input ledgers participate in the same CAS,
// so a concurrent hook addition to either ledger makes the whole projection a
// no-op. Attempt, row lifetime, and main PID fence same-ID row reuse.
//
// A projection that changes neither ledger performs only the identity/input
// check; periodic reconciliation must not manufacture a database write merely
// to prove that its input is still current.
func ProjectSessionBackgroundLedgers(
	ref BackgroundProjectionRef,
	previousShells, previousMonitors, nextShells, nextMonitors string,
) (bool, error) {
	if ref.Attempt.LegacySessionID == "" {
		return false, nil
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	args := []any{
		ref.Attempt.LegacySessionID,
		ref.Attempt.ExecutionID,
		dbTime(ref.SessionCreatedAt),
		ref.MainPID,
		previousShells,
		previousMonitors,
	}
	if previousShells == nextShells && previousMonitors == nextMonitors {
		var present int
		err := d.QueryRow(`SELECT 1 FROM sessions
			WHERE id = ? AND exit_callback_generation = ? AND created_at = ? AND pid = ?
			  AND bg_shells_json = ? AND monitors_json = ?`, args...).Scan(&present)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil && present == 1, err
	}
	res, err := d.Exec(`UPDATE sessions SET bg_shells_json = ?, monitors_json = ?
		WHERE id = ? AND exit_callback_generation = ? AND created_at = ? AND pid = ?
		  AND bg_shells_json = ? AND monitors_json = ?`,
		nextShells, nextMonitors,
		args[0], args[1], args[2], args[3], args[4], args[5])
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// SetSessionStatusFromBackgroundProjection conditionally updates the idle
// projection while the exact attempt and the projector's resulting ledger
// snapshot remain current. A hook racing after ledger projection therefore
// wins both its ledger addition and its status transition.
func SetSessionStatusFromBackgroundProjection(
	ref BackgroundProjectionRef,
	observedStatus string,
	observedUpdatedAt time.Time,
	projectedShells, projectedMonitors string,
	status, detail string,
	at time.Time,
) (bool, error) {
	if ref.Attempt.LegacySessionID == "" {
		return false, nil
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE sessions SET status = ?, status_detail = ?, updated_at = ?
		WHERE id = ? AND exit_callback_generation = ? AND created_at = ? AND pid = ?
		  AND status = ? AND updated_at = ?
		  AND bg_shells_json = ? AND monitors_json = ?`,
		status, detail, dbTime(at),
		ref.Attempt.LegacySessionID, ref.Attempt.ExecutionID,
		dbTime(ref.SessionCreatedAt), ref.MainPID,
		observedStatus, dbTime(observedUpdatedAt),
		projectedShells, projectedMonitors)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
