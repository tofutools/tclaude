package db

import (
	"database/sql"
	"errors"
)

// SetConvLossReport stores the losses.json document for convID, replacing any
// earlier one. The caller chains the earlier report into it.
func SetConvLossReport(convID string, report []byte) error {
	if convID == "" {
		return errors.New("conv loss report: empty conv id")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO conv_loss_reports (conv_id, report) VALUES (?, ?)
		ON CONFLICT(conv_id) DO UPDATE SET report = excluded.report`, convID, string(report))
	return err
}

// ConvLossReport returns convID's stored losses.json document, or nil when the
// conversation was never converted.
func ConvLossReport(convID string) ([]byte, error) {
	if convID == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var report string
	err = d.QueryRow(`SELECT report FROM conv_loss_reports WHERE conv_id = ?`, convID).Scan(&report)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []byte(report), nil
}
