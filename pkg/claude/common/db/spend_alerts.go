package db

// InsertSpendAlert atomically records a threshold and its human message. The
// independent ledger survives message deletion and concurrent dashboard polls.
// A failed insert rolls back the claim so a later poll can retry.
func InsertSpendAlert(period, start string, threshold int, message *HumanMessage) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(`INSERT OR IGNORE INTO spend_alerts (period, period_start, threshold) VALUES (?, ?, ?)`, period, start, threshold)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count == 0 {
		return false, err
	}
	if _, err = insertHumanMessage(tx, message); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
