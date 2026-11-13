package paneinput

import (
	"context"

	"github.com/tofutools/tclaude/pkg/claude/common/tmuxcmd"
)

// viaRunner is a tmux client whose every command is handed to run instead of
// reaching a tmux server, so a test sees the exact commands an injection
// sends and can fail or stall any one of them.
func viaRunner(run func(args ...string) error) tmuxcmd.Client {
	return tmuxcmd.NewClient(tmuxcmd.TransportFunc(func(_ context.Context, args []string) ([]byte, error) {
		return nil, run(args...)
	}))
}
