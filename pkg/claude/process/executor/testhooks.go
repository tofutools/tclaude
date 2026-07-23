package executor

import (
	"context"

	"github.com/tofutools/tclaude/pkg/claude/process/engine"
)

// SetProgramPerformForTest replaces only the external performer step beneath
// Execute. Authorization, one-shot dispatch consumption, observation apply,
// checkpoint persistence, and evidence persistence continue through the
// production path. It is intended for deterministic tests and benchmarks that
// must exclude real subprocess wall time.
func SetProgramPerformForTest(fn func(context.Context, string, engine.Command) (Result, error)) func() {
	previous := programPerform
	programPerform = fn
	return func() { programPerform = previous }
}
