//go:build windows

package agentd

import "github.com/tofutools/tclaude/pkg/claude/session"

func currentAgentdIdentity() session.ExecutionUnixIdentity {
	return session.ExecutionUnixIdentity{UID: -1, GID: -1}
}
