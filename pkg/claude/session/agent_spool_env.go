package session

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// ApplyAgentSpoolEnv provisions the experimental file-spool transport for a
// launching session: a private envelope directory whose possession is the
// session's caller identity toward agentd, bound to the session's conv in
// SQLite before the pane exists. No-op unless the operator opted in via
// TCLAUDE_EXPERIMENTAL_FILE_TRANSPORT. The socket stays this agent's
// preferred transport; the spool only carries traffic when the sandbox
// leaves the client no dialable socket (or TCLAUDE_AGENTD_TRANSPORT=spool
// forces it). See pkg/claude/common/agentipc spool.go for the protocol.
//
// Provisioning failure fails the launch: the flag is an explicit
// experimental opt-in, and silently spawning without the transport the
// operator asked for would just defer the confusion to the agent's first
// isolated `tclaude agent` call.
func ApplyAgentSpoolEnv(sessionID string, env map[string]string) error {
	if !agentipc.FileTransportEnabled() {
		return nil
	}
	root := agentipc.SpoolRoot()
	if root == "" {
		return fmt.Errorf("file-spool transport: cannot resolve spool root")
	}
	id, err := agentipc.NewSpoolID()
	if err != nil {
		return fmt.Errorf("file-spool transport: %w", err)
	}
	dir := filepath.Join(root, id)
	for _, d := range []string{root, dir, agentipc.SpoolReqDir(dir), agentipc.SpoolRespDir(dir)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("file-spool transport: create %s: %w", d, err)
		}
	}
	if err := db.CreateSpoolBinding(id, sessionID, dir); err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("file-spool transport: record binding: %w", err)
	}
	env[agentipc.SpoolEnv] = dir
	return nil
}
