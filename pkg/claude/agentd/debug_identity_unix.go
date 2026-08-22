//go:build unix

package agentd

import (
	"os"

	"github.com/tofutools/tclaude/pkg/claude/session"
)

func currentAgentdIdentity() session.ExecutionUnixIdentity {
	groups, _ := os.Getgroups()
	return session.ExecutionUnixIdentity{UID: os.Getuid(), GID: os.Getgid(), Groups: groups}
}
