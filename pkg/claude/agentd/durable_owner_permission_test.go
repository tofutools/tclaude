package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestDurableOwnerAllows(t *testing.T) {
	setupTestDB(t)
	_, _, err := db.EnsureAgentForConv("owner", "test")
	require.NoError(t, err)
	_, _, err = db.EnsureAgentForConv("stranger", "test")
	require.NoError(t, err)
	gID, err := db.CreateAgentGroup("team", "")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupOwner(gID, "owner", "<test>"))
	group := ActionContext{Group: "team", structuralGroup: "team"}

	allowed, err := durableOwnerAllows("trigger", "owner", PermGroupsTriggersManage, group)
	require.NoError(t, err)
	assert.True(t, allowed, "owning the group counts for a group action")

	allowed, err = durableOwnerAllows("trigger", "stranger", PermGroupsTriggersManage, group)
	require.NoError(t, err)
	assert.False(t, allowed, "neither granted nor an owner")

	allowed, err = durableOwnerAllows("trigger", "owner", PermTriggersManage, ActionContext{})
	require.NoError(t, err)
	assert.False(t, allowed, "a global action gets no ownership fallback")

	require.NoError(t, db.SetAgentPermissionOverride("owner", PermGroupsTriggersManage, db.PermEffectDeny, "<test>"))
	allowed, err = durableOwnerAllows("trigger", "owner", PermGroupsTriggersManage, group)
	require.NoError(t, err)
	assert.False(t, allowed, "an explicit deny beats group ownership")
}
