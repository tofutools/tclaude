package opencode

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func (r *Runtime) StageTerminalFile(ctx context.Context, in ports.StageTerminalFileRequest) (ports.StageTerminalFileResult, error) {
	if r.process == nil || !r.process.Observe().Running {
		return ports.StageTerminalFileResult{Disposition: ports.EffectRefused}, nil
	}
	return host.StageTerminalFile(ctx, r.provider.privateRoot, r.executionID, in)
}
