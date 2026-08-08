package db

import (
	"database/sql"
	"fmt"
	"time"
)

// CopilotAPIRuntime is the per-launch record of the loopback port tclaude
// allocated for one `copilot --ui-server` pane, keyed by conversation.
//
// It records what a launch was TOLD to bind, and nothing stronger. A row here
// does NOT mean a listener is up, and does not mean the listener is the agent's
// — `--ui-server` has no authentication (TCL-1055), so both facts have to be
// established against the live process each time they are relied on. Storing a
// "verified" flag would be storing a claim that ages: true when written, and
// silently a statement about whatever holds the port afterwards.
type CopilotAPIRuntime struct {
	ConvID    string
	Port      int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// UpsertCopilotAPIRuntime records the port a launch was given, replacing any
// port the same conversation was given before — which is what a relaunch onto a
// freshly allocated port needs.
//
// Callers must write this only AFTER the launch it describes has been handed
// off, never before. A row written ahead of the spawn survives a spawn that
// failed, and then names a port for an agent that never started: a record that
// is worse than none, because it reads as authoritative while nothing listens.
func UpsertCopilotAPIRuntime(runtime CopilotAPIRuntime) error {
	if runtime.ConvID == "" {
		return fmt.Errorf("copilot API runtime needs a conversation id")
	}
	if runtime.Port <= 0 || runtime.Port > 65535 {
		return fmt.Errorf("copilot API runtime port %d is out of range", runtime.Port)
	}
	d, err := Open()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if runtime.CreatedAt.IsZero() {
		runtime.CreatedAt = now
	}
	runtime.UpdatedAt = now
	_, err = d.Exec(`
		INSERT INTO copilot_api_runtimes (conv_id, port, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(conv_id) DO UPDATE SET
			port = excluded.port,
			updated_at = excluded.updated_at
	`, runtime.ConvID, runtime.Port,
		dbTime(runtime.CreatedAt), dbTime(runtime.UpdatedAt))
	return err
}

// GetCopilotAPIRuntime returns the recorded port for a conversation, or nil
// when it has none — the ordinary answer for every send-keys Copilot agent and
// every other harness.
func GetCopilotAPIRuntime(convID string) (*CopilotAPIRuntime, error) {
	if convID == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	row := d.QueryRow(`
		SELECT conv_id, port, created_at, updated_at
		FROM copilot_api_runtimes WHERE conv_id = ?
	`, convID)
	runtime, err := scanCopilotAPIRuntime(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return runtime, err
}

// DeleteCopilotAPIRuntime drops the record for a conversation.
//
// This is the release step, and it matters more than a freed port would suggest
// — the OS reclaimed the port the moment the process died, so what is released
// here is the CLAIM. Leaving the row behind lets a later read hand out a number
// belonging to a dead launch, which after port reuse is a number some other
// process now answers on.
func DeleteCopilotAPIRuntime(convID string) error {
	if convID == "" {
		return nil
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM copilot_api_runtimes WHERE conv_id = ?`, convID)
	return err
}

type copilotAPIRuntimeScanner interface {
	Scan(dest ...any) error
}

func scanCopilotAPIRuntime(row copilotAPIRuntimeScanner) (*CopilotAPIRuntime, error) {
	var runtime CopilotAPIRuntime
	var created, updated dbTimestamp
	if err := row.Scan(&runtime.ConvID, &runtime.Port, &created, &updated); err != nil {
		return nil, err
	}
	runtime.CreatedAt = created.Time()
	runtime.UpdatedAt = updated.Time()
	return &runtime, nil
}
