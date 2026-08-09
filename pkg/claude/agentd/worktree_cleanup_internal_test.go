package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestAgentWorktreeClaimSnapshotIncompleteFailsClosed(t *testing.T) {
	const conv = "claimant-discovery-failed"
	snap := agentWorktreeClaimSnapshot{
		views: map[string]agentWorktreeView{
			conv: {Path: "/tmp/worktree", Branch: "feat", Kind: "linked"},
		},
		dirClaims: map[string]map[string]bool{},
		complete:  false,
	}

	got := snap.resolve(conv, map[string]bool{conv: true})
	assert.True(t, got.Shared, "incomplete claimant discovery must make deletion unsafe")
	assert.False(t, got.Removable(), "an unknown claimant set must fail closed")
}

func TestRetireExcludesOnlyTargetsManagedOpenCodeRuntime(t *testing.T) {
	const conv = "ses_target"
	const other = "ses_other"
	path := "/tmp/opencode-worktree"
	snap := agentWorktreeClaimSnapshot{
		views: map[string]agentWorktreeView{
			conv: {Path: path, Branch: "feature", Kind: "linked"},
		},
		dirClaims: map[string]map[string]bool{
			path: {openCodeRuntimeClaimant(conv): true},
		},
		complete: true,
	}

	generic := snap.resolve(conv, map[string]bool{conv: true})
	assert.True(t, generic.Shared, "ordinary deletion must protect a live managed server")
	assert.Equal(t, openCodeRuntimeSharedNote, generic.SharedNote)

	retire := snap.resolve(conv, map[string]bool{
		conv: true, openCodeRuntimeClaimant(conv): true,
	})
	assert.True(t, retire.Removable(),
		"retirement can offer cleanup because it stops its own runtime before removal")

	snap.dirClaims[path][openCodeRuntimeClaimant(other)] = true
	retire = snap.resolve(conv, map[string]bool{
		conv: true, openCodeRuntimeClaimant(conv): true,
	})
	assert.False(t, retire.Removable(), "another agent's OpenCode runtime remains a blocker")
}

func TestDirContains(t *testing.T) {
	assert.True(t, dirContains("/tmp/agent-root", "/tmp/agent-root"))
	assert.True(t, dirContains("/tmp/agent-root", "/tmp/agent-root/pkg"))
	assert.False(t, dirContains("/tmp/agent-root", "/tmp/agent-root-2"))
	assert.False(t, dirContains("/tmp/agent-root", "/tmp"))
}

func TestCompareSessionLaunchRecencyIgnoresExitedStatus(t *testing.T) {
	base := time.Now()
	old := &db.SessionRow{
		CreatedAt: base,
		UpdatedAt: base.Add(4 * time.Minute),
		Status:    "running",
	}
	newExited := &db.SessionRow{
		CreatedAt: base.Add(2 * time.Minute),
		UpdatedAt: base.Add(3 * time.Minute),
		Status:    "exited",
	}

	assert.Equal(t, 1, compareSessionLaunchRecency(newExited, old),
		"the newer launch wins even when the older row received a later update")
	assert.Equal(t, -1, compareSessionLaunchRecency(old, newExited))
}
