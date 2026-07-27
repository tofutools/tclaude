package db

import (
	"database/sql"
	"fmt"
	"time"
)

const (
	OpenCodeTransportLoopbackTCP = "loopback-tcp"
	OpenCodeTransportUnixRelay   = "unix-relay"
)

// OpenCodeRuntime is the durable recovery record for one agentd-owned
// `opencode serve`. Password is intentionally omitted from logs and public
// session projections; only the private runtime manager reads this row.
type OpenCodeRuntime struct {
	SessionID string
	ConvID    string
	ServerURL string
	Password  string
	PID       int
	Cwd       string
	// SandboxImplementation records whether the managed server itself was
	// launched behind the outer wall. Legacy rows are harness-builtin.
	SandboxImplementation string
	// SandboxLaunchSpecJSON is the exact versioned renderer input required to
	// reproduce a tclaude-layer restart. It is empty for harness-builtin rows.
	SandboxLaunchSpecJSON string
	// Transport records how authenticated HTTP reaches the managed server.
	// Legacy/v2/v3 runtimes use host loopback. A v4 unix-relay runtime carries
	// the exact pathname identity agentd must prove before sending credentials.
	Transport           string
	ControlSocketPath   string
	ControlSocketDevice int64
	ControlSocketInode  int64
	// PermissionJSON is the exact ordered OpenCode session ruleset agentd
	// verifies and re-applies after healthy reuse or a serve restart.
	PermissionJSON string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func UpsertOpenCodeRuntime(runtime OpenCodeRuntime) error {
	if err := ValidateOpenCodeRuntimeTransport(runtime); err != nil {
		return err
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
		INSERT INTO opencode_runtimes
			(session_id, conv_id, server_url, password, pid, cwd, sandbox_implementation,
			 sandbox_launch_spec_json, transport, control_socket_path,
			 control_socket_device, control_socket_inode, permission_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			conv_id = excluded.conv_id,
			server_url = excluded.server_url,
			password = excluded.password,
			pid = excluded.pid,
			cwd = excluded.cwd,
			sandbox_implementation = excluded.sandbox_implementation,
			sandbox_launch_spec_json = excluded.sandbox_launch_spec_json,
			transport = excluded.transport,
			control_socket_path = excluded.control_socket_path,
			control_socket_device = excluded.control_socket_device,
			control_socket_inode = excluded.control_socket_inode,
			permission_json = excluded.permission_json,
			updated_at = excluded.updated_at
	`, runtime.SessionID, runtime.ConvID, runtime.ServerURL, runtime.Password,
		runtime.PID, runtime.Cwd, runtime.SandboxImplementation, runtime.SandboxLaunchSpecJSON,
		normalizeOpenCodeTransport(runtime.Transport), runtime.ControlSocketPath,
		runtime.ControlSocketDevice, runtime.ControlSocketInode, runtime.PermissionJSON,
		runtime.CreatedAt.Format(time.RFC3339Nano),
		runtime.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

func GetOpenCodeRuntime(sessionID string) (*OpenCodeRuntime, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	row := d.QueryRow(`
		SELECT session_id, conv_id, server_url, password, pid, cwd, sandbox_implementation,
			sandbox_launch_spec_json, transport, control_socket_path,
			control_socket_device, control_socket_inode, permission_json, created_at, updated_at
		FROM opencode_runtimes WHERE session_id = ?
	`, sessionID)
	runtime, err := scanOpenCodeRuntime(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return runtime, err
}

func GetOpenCodeRuntimeByConvID(convID string) (*OpenCodeRuntime, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	row := d.QueryRow(`
		SELECT session_id, conv_id, server_url, password, pid, cwd, sandbox_implementation,
			sandbox_launch_spec_json, transport, control_socket_path,
			control_socket_device, control_socket_inode, permission_json, created_at, updated_at
		FROM opencode_runtimes WHERE conv_id = ? ORDER BY created_at DESC LIMIT 1
	`, convID)
	runtime, err := scanOpenCodeRuntime(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return runtime, err
}

// FindOpenCodeRuntimeByPID returns the most recently refreshed runtime row
// whose agentd-owned `opencode serve` process has pid, or nil when no live
// runtime record matches. A reconciled server restart upserts the new PID and
// updated_at, so callers always prefer the freshest row if a stale duplicate
// remains from an older session.
func FindOpenCodeRuntimeByPID(pid int) (*OpenCodeRuntime, error) {
	if pid <= 0 {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	row := d.QueryRow(`
		SELECT session_id, conv_id, server_url, password, pid, cwd, sandbox_implementation,
			sandbox_launch_spec_json, transport, control_socket_path,
			control_socket_device, control_socket_inode, permission_json, created_at, updated_at
		FROM opencode_runtimes WHERE pid = ? ORDER BY updated_at DESC LIMIT 1
	`, pid)
	runtime, err := scanOpenCodeRuntime(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return runtime, err
}

func ListOpenCodeRuntimes() ([]OpenCodeRuntime, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`
		SELECT session_id, conv_id, server_url, password, pid, cwd, sandbox_implementation,
			sandbox_launch_spec_json, transport, control_socket_path,
			control_socket_device, control_socket_inode, permission_json, created_at, updated_at
		FROM opencode_runtimes ORDER BY created_at
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runtimes []OpenCodeRuntime
	for rows.Next() {
		runtime, err := scanOpenCodeRuntime(rows)
		if err != nil {
			return nil, err
		}
		runtimes = append(runtimes, *runtime)
	}
	return runtimes, rows.Err()
}

func DeleteOpenCodeRuntime(sessionID string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM opencode_runtimes WHERE session_id = ?`, sessionID)
	return err
}

type openCodeRuntimeScanner interface {
	Scan(dest ...any) error
}

func scanOpenCodeRuntime(row openCodeRuntimeScanner) (*OpenCodeRuntime, error) {
	var runtime OpenCodeRuntime
	var created, updated string
	if err := row.Scan(&runtime.SessionID, &runtime.ConvID, &runtime.ServerURL,
		&runtime.Password, &runtime.PID, &runtime.Cwd, &runtime.SandboxImplementation,
		&runtime.SandboxLaunchSpecJSON, &runtime.Transport, &runtime.ControlSocketPath,
		&runtime.ControlSocketDevice, &runtime.ControlSocketInode, &runtime.PermissionJSON,
		&created, &updated); err != nil {
		return nil, err
	}
	runtime.Transport = normalizeOpenCodeTransport(runtime.Transport)
	runtime.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	runtime.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return &runtime, nil
}

func normalizeOpenCodeTransport(transport string) string {
	if transport == "" {
		return OpenCodeTransportLoopbackTCP
	}
	return transport
}

func ValidateOpenCodeRuntimeTransport(runtime OpenCodeRuntime) error {
	switch normalizeOpenCodeTransport(runtime.Transport) {
	case OpenCodeTransportLoopbackTCP:
		if runtime.ControlSocketPath != "" ||
			runtime.ControlSocketDevice != 0 ||
			runtime.ControlSocketInode != 0 {
			return fmt.Errorf("loopback OpenCode runtime unexpectedly carries Unix socket authority")
		}
	case OpenCodeTransportUnixRelay:
		if runtime.ControlSocketPath == "" ||
			runtime.ControlSocketDevice <= 0 ||
			runtime.ControlSocketInode <= 0 {
			return fmt.Errorf("Unix-relay OpenCode runtime has incomplete socket authority")
		}
	default:
		return fmt.Errorf("unsupported OpenCode runtime transport %q", runtime.Transport)
	}
	return nil
}
