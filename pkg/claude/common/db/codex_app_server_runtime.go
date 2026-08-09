package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	CodexAppServerWarming     = "warming"
	CodexAppServerReady       = "ready"
	CodexAppServerUnavailable = "unavailable"
	CodexAppServerDead        = "dead"
)

type CodexAppServerRuntime struct {
	Generation   string
	LaunchID     string
	AgentID      string
	ConvID       string
	ThreadID     string
	SocketPath   string
	ServerPID    int
	CodexVersion string
	State        string
	Detail       string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func UpsertCodexAppServerRuntime(runtime CodexAppServerRuntime) error {
	if strings.TrimSpace(runtime.Generation) == "" || strings.TrimSpace(runtime.LaunchID) == "" ||
		strings.TrimSpace(runtime.AgentID) == "" || strings.TrimSpace(runtime.SocketPath) == "" {
		return errors.New("codex app-server runtime needs generation, launch id, agent id, and socket path")
	}
	switch runtime.State {
	case CodexAppServerWarming, CodexAppServerReady, CodexAppServerUnavailable, CodexAppServerDead:
	default:
		return fmt.Errorf("invalid Codex app-server runtime state %q", runtime.State)
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
		INSERT INTO codex_app_server_runtimes
			(generation, launch_id, agent_id, conv_id, thread_id, socket_path,
			 server_pid, codex_version, state, detail, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(generation) DO UPDATE SET
			launch_id=excluded.launch_id, agent_id=excluded.agent_id,
			conv_id=excluded.conv_id, thread_id=excluded.thread_id,
			socket_path=excluded.socket_path, server_pid=excluded.server_pid,
			codex_version=excluded.codex_version, state=excluded.state,
			detail=excluded.detail, updated_at=excluded.updated_at
	`, runtime.Generation, runtime.LaunchID, runtime.AgentID, runtime.ConvID,
		runtime.ThreadID, runtime.SocketPath, runtime.ServerPID, runtime.CodexVersion,
		runtime.State, runtime.Detail, dbTime(runtime.CreatedAt), dbTime(runtime.UpdatedAt))
	return err
}

func GetCodexAppServerRuntimeByConvID(convID string) (*CodexAppServerRuntime, error) {
	if strings.TrimSpace(convID) == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return scanCodexAppServerRuntime(d.QueryRow(codexAppServerRuntimeSelect+
		` WHERE conv_id = ? ORDER BY updated_at DESC LIMIT 1`, convID))
}

func GetCodexAppServerRuntime(generation string) (*CodexAppServerRuntime, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return scanCodexAppServerRuntime(d.QueryRow(codexAppServerRuntimeSelect+
		` WHERE generation = ?`, generation))
}

func DeleteCodexAppServerRuntime(generation string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM codex_app_server_runtimes WHERE generation = ?`, generation)
	return err
}

const codexAppServerRuntimeSelect = `SELECT generation, launch_id, agent_id,
	conv_id, thread_id, socket_path, server_pid, codex_version, state, detail,
	created_at, updated_at FROM codex_app_server_runtimes`

type codexAppServerRuntimeScanner interface{ Scan(...any) error }

func scanCodexAppServerRuntime(row codexAppServerRuntimeScanner) (*CodexAppServerRuntime, error) {
	var runtime CodexAppServerRuntime
	var created, updated dbTimestamp
	if err := row.Scan(&runtime.Generation, &runtime.LaunchID, &runtime.AgentID,
		&runtime.ConvID, &runtime.ThreadID, &runtime.SocketPath, &runtime.ServerPID,
		&runtime.CodexVersion, &runtime.State, &runtime.Detail, &created, &updated); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	runtime.CreatedAt, runtime.UpdatedAt = created.Time(), updated.Time()
	return &runtime, nil
}
