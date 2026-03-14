package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const currentVersion = 4

func migrate(db *sql.DB) error {
	ver := schemaVersion(db)
	if ver == currentVersion {
		return nil
	}

	if ver == 0 {
		if err := createSchema(db); err != nil {
			return err
		}
		if err := importLegacyData(db); err != nil {
			return err
		}
		ver = 1 // createSchema sets version to 1
	}

	if ver < 2 {
		if err := migrateV1toV2(db); err != nil {
			return err
		}
	}

	if ver < 3 {
		if err := migrateV2toV3(db); err != nil {
			return err
		}
	}

	if ver < 4 {
		if err := migrateV3toV4(db); err != nil {
			return err
		}
	}

	return nil
}

func schemaVersion(db *sql.DB) int {
	var ver int
	err := db.QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&ver)
	if err != nil {
		return 0
	}
	return ver
}

func createSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (1);

		CREATE TABLE IF NOT EXISTS sessions (
			id              TEXT PRIMARY KEY,
			tmux_session    TEXT NOT NULL DEFAULT '',
			pid             INTEGER NOT NULL DEFAULT 0,
			cwd             TEXT NOT NULL DEFAULT '',
			conv_id         TEXT NOT NULL DEFAULT '',
			status          TEXT NOT NULL DEFAULT 'idle',
			status_detail   TEXT NOT NULL DEFAULT '',
			auto_registered INTEGER NOT NULL DEFAULT 0,
			created_at      TEXT NOT NULL,
			updated_at      TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_sessions_conv_id ON sessions(conv_id);
		CREATE INDEX IF NOT EXISTS idx_sessions_status_updated ON sessions(status, updated_at);

		CREATE TABLE IF NOT EXISTS notify_state (
			session_id  TEXT PRIMARY KEY,
			notified_at TEXT NOT NULL
		);
	`)
	return err
}

// legacySessionJSON matches the JSON structure of the old file-based session state.
type legacySessionJSON struct {
	ID           string    `json:"id"`
	TmuxSession  string    `json:"tmuxSession"`
	PID          int       `json:"pid"`
	Cwd          string    `json:"cwd"`
	ConvID       string    `json:"convId,omitempty"`
	Status       string    `json:"status"`
	StatusDetail string    `json:"statusDetail,omitempty"`
	Created      time.Time `json:"created"`
	Updated      time.Time `json:"updated"`
}

func migrateV1toV2(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS usage_cache (
			id              INTEGER PRIMARY KEY,
			data            TEXT NOT NULL DEFAULT '{}',
			fetched_at      TEXT NOT NULL DEFAULT '',
			last_attempt_at TEXT NOT NULL DEFAULT ''
		);

		CREATE TABLE IF NOT EXISTS git_cache (
			repo_hash  TEXT PRIMARY KEY,
			data       TEXT NOT NULL DEFAULT '{}',
			fetched_at TEXT NOT NULL DEFAULT ''
		);

		UPDATE schema_version SET version = 2;
	`)
	if err != nil {
		return fmt.Errorf("migrate v1→v2: %w", err)
	}
	return nil
}

func migrateV2toV3(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS conv_index (
			conv_id       TEXT PRIMARY KEY,
			project_dir   TEXT NOT NULL,
			full_path     TEXT NOT NULL,
			file_mtime    INTEGER NOT NULL DEFAULT 0,
			file_size     INTEGER NOT NULL DEFAULT 0,
			first_prompt  TEXT NOT NULL DEFAULT '',
			summary       TEXT NOT NULL DEFAULT '',
			custom_title  TEXT NOT NULL DEFAULT '',
			message_count INTEGER NOT NULL DEFAULT 0,
			created       TEXT NOT NULL DEFAULT '',
			modified      TEXT NOT NULL DEFAULT '',
			git_branch    TEXT NOT NULL DEFAULT '',
			project_path  TEXT NOT NULL DEFAULT '',
			is_sidechain  INTEGER NOT NULL DEFAULT 0,
			indexed_at    TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_conv_index_project_dir ON conv_index(project_dir);

		UPDATE schema_version SET version = 3;
	`)
	if err != nil {
		return fmt.Errorf("migrate v2→v3: %w", err)
	}
	return nil
}

func migrateV3toV4(db *sql.DB) error {
	_, err := db.Exec(`
		ALTER TABLE sessions ADD COLUMN context_pct REAL NOT NULL DEFAULT 0;
		ALTER TABLE sessions ADD COLUMN compact_pending REAL NOT NULL DEFAULT 0;

		UPDATE schema_version SET version = 4;
	`)
	if err != nil {
		return fmt.Errorf("migrate v3→v4: %w", err)
	}
	return nil
}

func importLegacyData(db *sql.DB) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil // no home dir, nothing to import
	}

	importedSessions := importLegacySessions(db, home)
	importedNotify := importLegacyNotifyState(db, home)

	// Move debug.log from old location (~/.tclaude/claude-sessions/debug.log)
	// to new location (~/.tclaude/debug.log) before renaming the directory.
	oldDebugLog := filepath.Join(home, ".tclaude", "claude-sessions", "debug.log")
	newDebugLog := filepath.Join(home, ".tclaude", "debug.log")
	if _, err := os.Stat(oldDebugLog); err == nil {
		if _, err := os.Stat(newDebugLog); os.IsNotExist(err) {
			if err := os.Rename(oldDebugLog, newDebugLog); err != nil {
				slog.Warn("failed to move debug.log", "error", err)
			}
		}
	}

	if importedSessions {
		oldDir := filepath.Join(home, ".tclaude", "claude-sessions")
		newDir := oldDir + ".migrated"
		if err := os.Rename(oldDir, newDir); err != nil {
			slog.Warn("failed to rename legacy sessions dir", "error", err)
		}
	}
	if importedNotify {
		oldDir := filepath.Join(home, ".tclaude", "notify-state")
		newDir := oldDir + ".migrated"
		if err := os.Rename(oldDir, newDir); err != nil {
			slog.Warn("failed to rename legacy notify-state dir", "error", err)
		}
	}

	return nil
}

func importLegacySessions(db *sql.DB, home string) bool {
	dir := filepath.Join(home, ".tclaude", "claude-sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}

	// Collect .auto markers first
	autoMarkers := make(map[string]bool)
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".auto") {
			id := strings.TrimSuffix(entry.Name(), ".auto")
			autoMarkers[id] = true
		}
	}

	tx, err := db.Begin()
	if err != nil {
		return false
	}
	defer tx.Rollback()

	imported := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		var s legacySessionJSON
		if err := json.Unmarshal(data, &s); err != nil {
			continue
		}

		id := strings.TrimSuffix(entry.Name(), ".json")
		autoReg := 0
		if autoMarkers[id] {
			autoReg = 1
		}

		_, err = tx.Exec(`INSERT OR IGNORE INTO sessions
			(id, tmux_session, pid, cwd, conv_id, status, status_detail, auto_registered, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			s.ID, s.TmuxSession, s.PID, s.Cwd, s.ConvID,
			s.Status, s.StatusDetail, autoReg,
			s.Created.Format(time.RFC3339Nano), s.Updated.Format(time.RFC3339Nano))
		if err != nil {
			slog.Warn("failed to import session", "id", s.ID, "error", err)
			continue
		}
		imported++
	}

	if imported == 0 {
		return false
	}

	if err := tx.Commit(); err != nil {
		slog.Warn("failed to commit session import", "error", err)
		return false
	}

	slog.Info(fmt.Sprintf("imported %d legacy sessions into SQLite", imported))
	return true
}

func importLegacyNotifyState(db *sql.DB, home string) bool {
	dir := filepath.Join(home, ".tclaude", "notify-state")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}

	tx, err := db.Begin()
	if err != nil {
		return false
	}
	defer tx.Rollback()

	imported := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		_, err = tx.Exec(`INSERT OR IGNORE INTO notify_state (session_id, notified_at) VALUES (?, ?)`,
			entry.Name(), info.ModTime().Format(time.RFC3339Nano))
		if err != nil {
			continue
		}
		imported++
	}

	if imported == 0 {
		return false
	}

	if err := tx.Commit(); err != nil {
		return false
	}

	slog.Info(fmt.Sprintf("imported %d legacy notify states into SQLite", imported))
	return true
}
