package exec

import runtime "github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"

var (
	verify      = runtime.VerifyExecutionInput
	unsupported = runtime.ErrExclusiveUnsupported
	input       *runtime.VerifiedExclusiveInput
	observation runtime.ExclusiveObservation
)

func parallelEnabled() bool { return input.ParallelEnabled() }
