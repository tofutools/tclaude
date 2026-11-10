package agentd

import (
	"log/slog"
	"os"
	"regexp"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

var openCodeAgentIDRE = regexp.MustCompile(`^agt_[0-9a-f]+$`)

// claimPrivateOpenCodeState allocates an agent's private state and returns what
// removes it again, for a caller whose agent may never come to exist. The
// function is nil when the state was already there: it is then not this
// caller's to remove.
func claimPrivateOpenCodeState(agentID string) (func(), error) {
	existing, err := db.GetOpenCodeAgentStateAllocation(agentID)
	if err != nil {
		return nil, err
	}
	allocation, err := allocatePrivateOpenCodeState(agentID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, nil
	}
	return func() {
		if err := os.RemoveAll(allocation.StateRoot); err != nil {
			slog.Warn("opencode state: remove unused state", "agent", agentID, "error", err)
		}
		if err := db.DeleteOpenCodeAgentStateAllocation(agentID); err != nil {
			slog.Warn("opencode state: forget unused state", "agent", agentID, "error", err)
		}
	}, nil
}
