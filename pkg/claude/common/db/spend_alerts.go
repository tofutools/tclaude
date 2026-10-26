package db

import (
	"encoding/json"
	"strconv"
	"time"
)

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
	rule, err := json.Marshal(l.Rule)
	if err != nil {
		return err
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
	now := time.Now()
	res, err := tx.Exec(`INSERT INTO usage_limits (scope, group_id, rule, enabled, created_at, updated_at) VALUES (?, NULL, ?, ?, ?, ?)`,
		UsageScopeGlobal, string(rule), l.Enabled, dbTime(now), dbTime(now))
	if err != nil {
		return err
	}
	if l.ID, err = res.LastInsertId(); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT threshold FROM spend_alerts WHERE period = ? AND period_start = ?`, periodName, periodStart)
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
		if _, err := tx.Exec(`INSERT OR IGNORE INTO usage_rule_firings
			(rule_key, subject, period_key, step_key, action, rule_name, value, budget, dry_run, fired_at)
			VALUES (?, 'global', ?, ?, 'notify', ?, 0, ?, 0, ?)`,
			"limit:"+strconv.FormatInt(l.ID, 10), periodKey, stepKey(t), l.Rule.Name, l.Rule.Budget, dbTime(now)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	l.Scope, l.CreatedAt, l.UpdatedAt = UsageScopeGlobal, now, now
	return nil
}
