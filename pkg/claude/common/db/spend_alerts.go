package db

import "strconv"

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

// ImportLegacySpendLimit turns a legacy cost.<period>_limit_usd setting into
// the global usage limit l, carrying over the thresholds already alerted in
// the current span (periodName "daily" or "monthly", periodStart its key) so
// the switch does not repeat them. stepKey maps a legacy threshold to the
// new rule's step key; periodKey is the new firing period key.
func ImportLegacySpendLimit(l *UsageLimit, periodName, periodStart, periodKey string, stepKey func(threshold int) string) error {
	if err := CreateUsageLimit(l); err != nil {
		return err
	}
	d, err := Open()
	if err != nil {
		return err
	}
	rows, err := d.Query(`SELECT threshold FROM spend_alerts WHERE period = ? AND period_start = ?`, periodName, periodStart)
	if err != nil {
		return err
	}
	var thresholds []int
	for rows.Next() {
		var t int
		if err := rows.Scan(&t); err != nil {
			_ = rows.Close()
			return err
		}
		thresholds = append(thresholds, t)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, t := range thresholds {
		if _, err := InsertUsageRuleFiring(UsageRuleFiring{
			RuleKey: "limit:" + strconv.FormatInt(l.ID, 10), Subject: "global", PeriodKey: periodKey,
			StepKey: stepKey(t), Action: "notify", RuleName: l.Rule.Name, Budget: l.Rule.Budget,
		}, nil); err != nil {
			return err
		}
	}
	return nil
}
