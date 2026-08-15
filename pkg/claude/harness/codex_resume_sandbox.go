package harness

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
)

const codexDisabledSandboxPolicy = `{"type":"disabled"}`

// PrepareCodexDangerFullAccessResume repairs Codex's per-thread sandbox state
// before resuming a conversation without a sandbox. Codex persists the last
// effective permission profile in threads.sandbox_policy and may retain that
// stricter profile across `codex resume --sandbox danger-full-access`. Updating
// the stopped thread makes the durable state agree with the explicit launch
// flag before any new model-authored command can run.
//
// The caller supplies the already-validated Codex state root recorded for the
// managed conversation. Only the sandbox column is changed: approval remains
// governed independently by tclaude's recorded --ask-for-approval posture.
func PrepareCodexDangerFullAccessResume(configDir, convID string) error {
	configDir = strings.TrimSpace(configDir)
	convID = strings.TrimSpace(convID)
	if configDir == "" {
		return fmt.Errorf("prepare Codex full-access resume: state root is empty")
	}
	if convID == "" {
		return fmt.Errorf("prepare Codex full-access resume: conversation id is empty")
	}

	path := codexStateDBPathAt(configDir)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("prepare Codex full-access resume: no state DB at %s", path)
		}
		return fmt.Errorf("prepare Codex full-access resume: stat state DB: %w", err)
	}

	d, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return fmt.Errorf("prepare Codex full-access resume: open state DB: %w", err)
	}
	defer func() { _ = d.Close() }()

	res, err := d.Exec(`UPDATE threads SET sandbox_policy = ? WHERE id = ?`,
		codexDisabledSandboxPolicy, convID)
	if err != nil {
		return fmt.Errorf("prepare Codex full-access resume: update threads.sandbox_policy for %s: %w", convID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("prepare Codex full-access resume: rows affected for %s: %w", convID, err)
	}
	if rows == 0 {
		return fmt.Errorf("prepare Codex full-access resume: no threads row for conversation %s", convID)
	}
	return nil
}
