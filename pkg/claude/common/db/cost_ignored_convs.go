package db

import (
	"errors"
	"time"
)

// SetCostConvIgnored excludes convID's spend from every cost total, or
// restores it. Its recorded history is never modified, so a restore brings the
// spend back exactly. Ignoring an already ignored conversation keeps the
// original time.
func SetCostConvIgnored(convID string, ignored bool) error {
	if convID == "" {
		return errors.New("cost ignore: empty conv id")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	if !ignored {
		_, err = d.Exec(`DELETE FROM cost_ignored_convs WHERE conv_id = ?`, convID)
		return err
	}
	_, err = d.Exec(`INSERT INTO cost_ignored_convs (conv_id, ignored_at) VALUES (?, ?)
		ON CONFLICT(conv_id) DO NOTHING`, convID, dbTime(time.Now()))
	return err
}

// CostIgnoredConvs returns every ignored conversation with the time it was
// ignored.
func CostIgnoredConvs() (map[string]time.Time, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT conv_id, ignored_at FROM cost_ignored_convs`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]time.Time{}
	for rows.Next() {
		var convID string
		var at dbTimestamp
		if err := rows.Scan(&convID, &at); err != nil {
			return nil, err
		}
		out[convID] = at.time
	}
	return out, rows.Err()
}

// CostIgnoreBilledTo prefixes the reason of a conversation whose model calls
// went through a peer's model gateway: the rest is the gateway reference
// (<gateway>@<peer instance>). That peer's cost totals carry the spend.
const CostIgnoreBilledTo = "billed_to:"

// MarkCostConvsBilledElsewhere ignores, with a billed-to reason, every
// conversation with recorded spend from a session launched through a peer's
// model gateway (usage sharing, requester-paid workers). Their history stays
// recorded; the totals leave it out. An operator's own Ignore of such a
// conversation gains the reason. The mark is per conversation: one resumed
// in another session keeps it either way. Spend recorded before a session
// knows its conversation has no conversation to mark and stays counted.
//
// fromDay limits the rows looked at. The default reads pass the start of what
// they read: a conversation's first row marks it within a poll, and each
// opening refresh sweeps everything with "".
func MarkCostConvsBilledElsewhere(fromDay string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	// The totals read this often: write only when a conversation is new. CROSS
	// JOIN pins SQLite to walking the launches, usually none, instead of every
	// cost row.
	const unmarked = `FROM model_proxy_launches l CROSS JOIN session_cost_daily c ON c.session_id = l.session
		WHERE c.day >= ? AND c.conv_id <> '' AND NOT EXISTS (SELECT 1 FROM cost_ignored_convs i WHERE i.conv_id = c.conv_id AND i.reason <> '')`
	var pending bool
	if err := d.QueryRow(`SELECT EXISTS (SELECT 1 `+unmarked+`)`, fromDay).Scan(&pending); err != nil || !pending {
		return err
	}
	_, err = d.Exec(`INSERT INTO cost_ignored_convs (conv_id, ignored_at, reason)
		SELECT c.conv_id, ?, ? || MIN(l.reference) `+unmarked+` GROUP BY c.conv_id
		ON CONFLICT(conv_id) DO UPDATE SET reason = excluded.reason WHERE cost_ignored_convs.reason = ''`,
		dbTime(time.Now()), CostIgnoreBilledTo, fromDay)
	return err
}

// CostIgnoredReasons returns each ignored conversation's reason ("" for the
// operator's own Ignore).
func CostIgnoredReasons() (map[string]string, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT conv_id, reason FROM cost_ignored_convs WHERE reason <> ''`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var convID, reason string
		if err := rows.Scan(&convID, &reason); err != nil {
			return nil, err
		}
		out[convID] = reason
	}
	return out, rows.Err()
}
