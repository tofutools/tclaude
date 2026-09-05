package agentd

import (
	"context"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

type worktreeTimingKey struct{}

// The browser already supplies this bounded progress ID before the spawn has
// a label. Carry it through nested worktree traces without logging Git URLs,
// command output, credentials, or repository paths.
func worktreeStartupTiming(ctx context.Context, component string) func(string, ...any) {
	id, _ := ctx.Value(worktreeTimingKey{}).(string)
	return config.StartupTiming(component, "worktree_progress_id", id)
}
