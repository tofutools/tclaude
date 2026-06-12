package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Per-agent OS-notification preference modes, stored in
// agent_notify_prefs. No row at all means "inherit": the agent follows
// its groups' notify_enabled switches (and the global config toggle).
const (
	// NotifyPrefOn forces notifications for the agent even when a
	// containing group is muted.
	NotifyPrefOn = "on"
	// NotifyPrefOff silences the agent regardless of group settings.
	NotifyPrefOff = "off"
	// NotifyPrefInherit is the absent-row state — not stored, only
	// used at API boundaries to mean "delete the override".
	NotifyPrefInherit = "inherit"
)

// SetConvNotifyPref stores the per-agent notification override for a
// conv-id. mode must be NotifyPrefOn or NotifyPrefOff; NotifyPrefInherit
// (or "") deletes the override so the agent falls back to group/global
// settings.
func SetConvNotifyPref(convID, mode string) error {
	db, err := Open()
	if err != nil {
		return err
	}
	switch mode {
	case "", NotifyPrefInherit:
		_, err = db.Exec(`DELETE FROM agent_notify_prefs WHERE conv_id = ?`, convID)
		return err
	case NotifyPrefOn, NotifyPrefOff:
		_, err = db.Exec(`INSERT OR REPLACE INTO agent_notify_prefs (conv_id, mode, updated_at) VALUES (?, ?, ?)`,
			convID, mode, time.Now().Format(time.RFC3339Nano))
		return err
	default:
		return fmt.Errorf("invalid notify pref %q (want %q, %q or %q)",
			mode, NotifyPrefOn, NotifyPrefOff, NotifyPrefInherit)
	}
}

// GetConvNotifyPref returns the per-agent notification override for a
// conv-id: NotifyPrefOn, NotifyPrefOff, or "" when no override exists
// (inherit).
func GetConvNotifyPref(convID string) (string, error) {
	db, err := Open()
	if err != nil {
		return "", err
	}
	var mode string
	err = db.QueryRow(`SELECT mode FROM agent_notify_prefs WHERE conv_id = ?`, convID).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return mode, nil
}

// ListConvNotifyPrefs returns every per-agent notification override as
// a conv-id → mode map. Convs without an override (inherit) are absent.
func ListConvNotifyPrefs() (map[string]string, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT conv_id, mode FROM agent_notify_prefs`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string]string{}
	for rows.Next() {
		var conv, mode string
		if err := rows.Scan(&conv, &mode); err != nil {
			return nil, err
		}
		out[conv] = mode
	}
	return out, rows.Err()
}
