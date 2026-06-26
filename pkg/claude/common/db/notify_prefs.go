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
	// The pref is keyed on the stable agent_id (JOH-26) so a "mute this agent"
	// choice follows the actor across conv rotations.
	switch mode {
	case "", NotifyPrefInherit:
		agentID, aerr := AgentIDForConv(convID)
		if aerr != nil {
			return aerr
		}
		if agentID == "" {
			return nil // no actor ⇒ nothing to clear
		}
		_, err = db.Exec(`DELETE FROM agent_notify_prefs WHERE agent_id = ?`, agentID)
		return err
	case NotifyPrefOn, NotifyPrefOff:
		agentID, _, aerr := EnsureAgentForConv(convID, "notify-pref")
		if aerr != nil {
			return aerr
		}
		_, err = db.Exec(`INSERT OR REPLACE INTO agent_notify_prefs (agent_id, mode, updated_at) VALUES (?, ?, ?)`,
			agentID, mode, time.Now().Format(time.RFC3339Nano))
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
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return "", err
	}
	if agentID == "" {
		return "", nil
	}
	var mode string
	err = db.QueryRow(`SELECT mode FROM agent_notify_prefs WHERE agent_id = ?`, agentID).Scan(&mode)
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
	// Keyed by each actor's current conv for the display-facing map.
	rows, err := db.Query(`SELECT ag.current_conv_id, n.mode
		FROM agent_notify_prefs n JOIN agents ag ON ag.agent_id = n.agent_id`)
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
