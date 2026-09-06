package agentd

import (
	"slices"
	"testing"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

func TestSessionResumeArgsCarryManagedOperationCorrelation(t *testing.T) {
	args := sessionResumeArgs(clcommon.SpawnArgs{
		ConvID: "conv-resume", Cwd: "/tmp/project",
		ExecutionID:       "11111111111111111111111111111111",
		ResumeOperationID: "op_11111111111111111111111111111111",
		ResumeClaimFD:     7,
	})
	for _, want := range []string{
		"--execution-id", "11111111111111111111111111111111",
		"--resume-operation-id", "op_11111111111111111111111111111111",
		"--resume-claim-fd", "3",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("resume argv missing %q: %v", want, args)
		}
	}
}
