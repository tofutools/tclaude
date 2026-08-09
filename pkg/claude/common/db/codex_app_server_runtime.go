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

func GetCodexAppServerRuntimeByLaunchID(launchID string) (*CodexAppServerRuntime, error) {
	if strings.TrimSpace(launchID) == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return scanCodexAppServerRuntime(d.QueryRow(codexAppServerRuntimeSelect+
		` WHERE launch_id = ? ORDER BY updated_at DESC LIMIT 1`, launchID))
}

func GetCodexAppServerRuntime(generation string) (*CodexAppServerRuntime, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return scanCodexAppServerRuntime(d.QueryRow(codexAppServerRuntimeSelect+
		` WHERE generation = ?`, generation))
}

// BindWarmingCodexAppServerRuntimeFromTUI records the thread identity carried
// by a validated hook from the TUI process. Codex 0.147 automatically
// subscribes every connection that is already initialized when a fresh thread
// is created, so agentd must not initialize its control connection until this
// post-creation signal exists. Only the newest warming generation for the
// launch may be bound, and a resume's predeclared conversation must match.
func BindWarmingCodexAppServerRuntimeFromTUI(launchID, threadID string) (bool, error) {
	launchID = strings.TrimSpace(launchID)
	threadID = strings.TrimSpace(threadID)
	if launchID == "" || threadID == "" {
		return false, nil
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	result, err := d.Exec(`UPDATE codex_app_server_runtimes
		SET conv_id = ?, thread_id = ?, updated_at = ?
		WHERE generation = (
			SELECT generation FROM codex_app_server_runtimes
			WHERE launch_id = ? AND state = ?
			ORDER BY created_at DESC LIMIT 1
		) AND (conv_id = '' OR conv_id = ?)`, threadID, threadID, dbTime(now), launchID, CodexAppServerWarming, threadID)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed > 0, err
}

func DeleteCodexAppServerRuntime(generation string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM codex_app_server_runtimes WHERE generation = ?`, generation)
	return err
}

func SetCodexAppServerRuntimeVersion(generation, version string) error {
	if strings.TrimSpace(generation) == "" || strings.TrimSpace(version) == "" {
		return errors.New("codex app-server runtime version needs generation and version")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	result, err := d.Exec(`UPDATE codex_app_server_runtimes
		SET codex_version = ?, updated_at = ? WHERE generation = ? AND state = ?`,
		strings.TrimSpace(version), dbTime(time.Now().UTC()), generation, CodexAppServerWarming)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("codex app-server runtime %q is not warming", generation)
	}
	return nil
}

func MarkCodexAppServerRuntimeUnavailable(generation, detail string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE codex_app_server_runtimes
		SET state = ?, detail = ?, updated_at = ? WHERE generation = ?`,
		CodexAppServerUnavailable, detail, dbTime(time.Now().UTC()), generation)
	return err
}

// MarkCodexAppServerRuntimeTerminalIfUnreplaced records a terminal observation
// only while no newer generation for the same conversation is already ready.
// A late watcher from an obsolete connection must not become the newest
// durable row and obscure its live replacement.
func MarkCodexAppServerRuntimeTerminalIfUnreplaced(generation, state, detail string) (bool, error) {
	if state != CodexAppServerUnavailable && state != CodexAppServerDead {
		return false, fmt.Errorf("codex app-server terminal state must be unavailable or dead, got %q", state)
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	result, err := d.Exec(`UPDATE codex_app_server_runtimes
		SET state = ?, detail = ?, updated_at = ?
		WHERE generation = ? AND NOT EXISTS (
			SELECT 1 FROM codex_app_server_runtimes replacement
			WHERE replacement.conv_id = codex_app_server_runtimes.conv_id
			  AND replacement.generation <> codex_app_server_runtimes.generation
			  AND replacement.state = ?
			  AND replacement.created_at > codex_app_server_runtimes.created_at
		)`, state, detail, dbTime(time.Now().UTC()), generation, CodexAppServerReady)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

// SetSessionStatusForCodexAppServerGeneration projects one app-server status
// observation only while both the session row and the verified runtime
// generation are still current. A late notification from a replaced socket
// therefore cannot overwrite the successor's hook or observer state.
func SetSessionStatusForCodexAppServerGeneration(
	sessionID, convID string,
	sessionCreatedAt time.Time,
	generation, observedStatus string,
	observedUpdatedAt time.Time,
	status, detail string,
	at time.Time,
) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	result, err := d.Exec(`UPDATE sessions
		SET status = ?, status_detail = ?, updated_at = ?
		WHERE id = ? AND conv_id = ? AND created_at = ?
		  AND status = ? AND updated_at = ?
		  AND EXISTS (
			SELECT 1 FROM codex_app_server_runtimes runtime
			WHERE runtime.generation = ? AND runtime.conv_id = ? AND runtime.state = ?
			  AND NOT EXISTS (
				SELECT 1 FROM codex_app_server_runtimes replacement
				WHERE replacement.conv_id = runtime.conv_id
				  AND replacement.generation <> runtime.generation
				  AND replacement.state = ?
				  AND replacement.created_at > runtime.created_at
			  )
		  )`, status, detail, dbTime(at), sessionID, convID, dbTime(sessionCreatedAt),
		observedStatus, dbTime(observedUpdatedAt), generation, convID,
		CodexAppServerReady, CodexAppServerReady)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

// InvalidateCodexAppServerRuntimesAfterRestart makes the durable state match
// the empty in-process handle registry before the daemon starts serving.
func InvalidateCodexAppServerRuntimesAfterRestart() error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE codex_app_server_runtimes
		SET state = ?, detail = ?, updated_at = ? WHERE state IN (?, ?)`,
		CodexAppServerUnavailable, "agentd restarted; verified control handle must be relaunched",
		dbTime(time.Now().UTC()), CodexAppServerWarming, CodexAppServerReady)
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
