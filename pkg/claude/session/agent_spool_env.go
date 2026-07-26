package session

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// fileSpoolProvisioningEnabled mirrors the daemon's flag resolution: the
// TCLAUDE_EXPERIMENTAL_FILE_TRANSPORT environment variable OR the
// features.file_spool_transport config field (dashboard Config tab →
// Experimental). `session new` runs unsandboxed, so reading private config
// here is fine; sandboxed agent CLIs never consult the flag — they only see
// the TCLAUDE_AGENTD_SPOOL env this provisioning exports.
func fileSpoolProvisioningEnabled() bool {
	if agentipc.FileTransportEnabled() {
		return true
	}
	cfg, err := config.Load()
	return err == nil && cfg.FileSpoolTransportEnabled()
}

// ApplyAgentSpoolEnv provisions the experimental file-spool transport for a
// launching session: a private envelope directory whose possession is the
// session's caller identity toward agentd, bound to the session's conv in
// SQLite before the pane exists. No-op unless the operator opted in (see
// fileSpoolProvisioningEnabled). The socket stays this agent's
// preferred transport; the spool only carries traffic when the sandbox
// leaves the client no dialable socket (or TCLAUDE_AGENTD_TRANSPORT=spool
// forces it). See pkg/claude/common/agentipc spool.go for the protocol.
//
// Provisioning failure fails the launch: the flag is an explicit
// experimental opt-in, and silently spawning without the transport the
// operator asked for would just defer the confusion to the agent's first
// isolated `tclaude agent` call.
//
// Returns the provisioned directory ("" when the flag is off). Callers
// that fail the launch after this point must call CleanupAgentSpool so a
// session that never existed doesn't leave a live identity capability
// behind.
func ApplyAgentSpoolEnv(sessionID string, env map[string]string) (string, error) {
	if !fileSpoolProvisioningEnabled() {
		return "", nil
	}
	root := agentipc.SpoolRoot()
	if root == "" {
		return "", fmt.Errorf("file-spool transport: cannot resolve spool root")
	}
	id, err := agentipc.NewSpoolID()
	if err != nil {
		return "", fmt.Errorf("file-spool transport: %w", err)
	}
	dir := filepath.Join(root, id)
	for _, d := range []string{root, dir, agentipc.SpoolReqDir(dir), agentipc.SpoolRespDir(dir)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", fmt.Errorf("file-spool transport: create %s: %w", d, err)
		}
	}
	if err := db.CreateSpoolBinding(id, sessionID, dir); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("file-spool transport: record binding: %w", err)
	}
	env[agentipc.SpoolEnv] = dir
	return dir, nil
}

// CleanupAgentSpool undoes ApplyAgentSpoolEnv for a launch that failed
// after provisioning: revoke the binding (identity first) and remove the
// directory. Best-effort — the daemon's revoked-binding sweep converges on
// anything left behind.
func CleanupAgentSpool(sessionID, dir string) {
	if dir == "" {
		return
	}
	if _, err := db.RevokeSpoolBindingsForConv(sessionID); err != nil {
		slog.Warn("file-spool transport: revoke binding on failed launch", "session", sessionID, "error", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("file-spool transport: remove spool dir on failed launch", "dir", dir, "error", err)
	}
}
