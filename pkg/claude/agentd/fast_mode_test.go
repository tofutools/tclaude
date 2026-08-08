package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
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

func TestCodexFastModeForSessionRejectsPreviousGenerationObservation(t *testing.T) {
	created := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	snap := harness.CodexRuntimeSnapshot{
		FastMode:         true,
		HasFastMode:      true,
		FastModeObserved: created.Add(time.Second),
	}
	first := &db.SessionRow{ID: "same-session", ConvID: "same-conv", CreatedAt: created}
	fast, known := codexFastModeForSession(snap, first)
	assert.True(t, known)
	assert.True(t, fast)

	// Session pruning and a later resume can recreate the same keys. Until
	// Codex appends settings for that new process, the follower's old event is
	// not authoritative for the replacement generation.
	resumed := &db.SessionRow{
		ID: "same-session", ConvID: "same-conv", CreatedAt: created.Add(2 * time.Second),
	}
	fast, known = codexFastModeForSession(snap, resumed)
	assert.False(t, known)
	assert.False(t, fast)

	snap.FastMode = false
	snap.FastModeObserved = created.Add(3 * time.Second)
	fast, known = codexFastModeForSession(snap, resumed)
	assert.True(t, known)
	assert.False(t, fast)
}

func valueAfter(args []string, flag string) string {
	for i := range args {
		if args[i] == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
