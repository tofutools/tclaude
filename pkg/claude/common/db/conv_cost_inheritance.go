package db

import (
	"database/sql"
	"errors"
	"time"
)

// SetConvCostInheritedUntil records that convID's transcript carries copied
// history whose usage, up to and including until, belongs to the conversation
// it was copied from. Repeated writes keep the latest instant: everything a
// later copy brought in is inherited too.
func SetConvCostInheritedUntil(convID string, until time.Time) error {
	if convID == "" {
		return errors.New("conv cost inheritance: empty conv id")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO conv_cost_inheritance (conv_id, inherited_until) VALUES (?, ?)
		ON CONFLICT(conv_id) DO UPDATE SET
			inherited_until = MAX(conv_cost_inheritance.inherited_until, excluded.inherited_until)`,
		convID, dbTime(until))
	return err
}

// ConvCostInheritedUntil returns the instant recorded by
// SetConvCostInheritedUntil, or the zero time when convID's whole transcript
// is its own.
func ConvCostInheritedUntil(convID string) (time.Time, error) {
	if convID == "" {
		return time.Time{}, nil
	}
	d, err := Open()
	if err != nil {
		return time.Time{}, err
	}
	var ns int64
	err = d.QueryRow(`SELECT inherited_until FROM conv_cost_inheritance WHERE conv_id = ?`, convID).Scan(&ns)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return timeFromUnixNano(ns), nil
}

// DeleteConvCostInheritance drops convID's record; for an import rolled back
// before the conversation was ever used.
func DeleteConvCostInheritance(convID string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM conv_cost_inheritance WHERE conv_id = ?`, convID)
	return err
}
