package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestRenderMountPlanInterimOrdersAncestorsFirst(t *testing.T) {
	plan, err := renderMountPlanInterim(sandboxpolicy.EffectiveProfile{
		Filesystem: []sandboxpolicy.FilesystemGrant{
			{Path: "/home/user/project/cache", Access: sandboxpolicy.AccessDeny},
			{Path: "/home/user", Access: sandboxpolicy.AccessDeny},
			{Path: "/home/user/project", Access: sandboxpolicy.AccessWrite},
			{Path: "/", Access: sandboxpolicy.AccessRead},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, []sandboxpolicy.MountEntry{
		{Path: "/", Mode: sandboxpolicy.MountRO},
		{Path: "/home/user", Mode: sandboxpolicy.MountHide},
		{Path: "/home/user/project", Mode: sandboxpolicy.MountRW},
		{Path: "/home/user/project/cache", Mode: sandboxpolicy.MountHide},
	}, plan.Entries)
}
