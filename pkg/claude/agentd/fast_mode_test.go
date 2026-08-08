package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestSessionArgsCarryExplicitFastMode(t *testing.T) {
	bare := sessionNewArgs(clcommon.SpawnArgs{Label: "worker", Harness: harness.CodexName})
	assert.NotContains(t, bare, "--fast-mode")

	for _, mode := range []string{harness.FastModeOn, harness.FastModeOff} {
		fresh := sessionNewArgs(clcommon.SpawnArgs{
			Label: "worker", Harness: harness.CodexName, FastMode: mode,
		})
		assert.Equal(t, mode, valueAfter(fresh, "--fast-mode"))
		resume := sessionResumeArgs(clcommon.SpawnArgs{
			ConvID: "conv-1", Harness: harness.CodexName, FastMode: mode,
		})
		assert.Equal(t, mode, valueAfter(resume, "--fast-mode"))
	}
}

func valueAfter(args []string, flag string) string {
	for i := range args {
		if args[i] == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
