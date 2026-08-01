package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestAppendSessionSandboxAccessNotice(t *testing.T) {
	setupTestDB(t)
	snapshot := sandboxpolicy.NewSnapshot(sandboxpolicy.EffectiveProfile{
		ResourceLimits: sandboxpolicy.ResourceLimits{Memory: "1GiB", MemoryBytes: 1 << 30},
	}, nil)
	require.NoError(t, SaveSession(&SessionRow{
		ID: "runtime-limit-notice", ConvID: "conv-runtime-limit-notice",
		Status: "working", CreatedAt: time.Now(), EffectiveSandbox: &snapshot,
	}))
	notice := sandboxpolicy.AccessNotice{
		Class: sandboxpolicy.AccessNoticeClassDegradation, Axis: "resource_limits",
		Reason: sandboxpolicy.AccessNoticeReasonOperatorUnenforcedLaunchOverride,
		Effect: sandboxpolicy.AccessNoticeEffectNotEnforced, Detail: "late attachment failed",
	}
	require.NoError(t, AppendSessionSandboxAccessNotice("runtime-limit-notice", notice))

	got, err := LoadSession("runtime-limit-notice")
	require.NoError(t, err)
	require.NotNil(t, got.EffectiveSandbox)
	require.Len(t, got.EffectiveSandbox.Effective.AccessNotices, 1)
	assert.Equal(t, notice, got.EffectiveSandbox.Effective.AccessNotices[0])
}
