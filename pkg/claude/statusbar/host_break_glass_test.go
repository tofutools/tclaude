package statusbar

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestBreakGlassWarningsFollowStableIdentityAndRevocation(t *testing.T) {
	t.Setenv("HOME", testutil.CanonicalTempDir(t))
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	const oldConv = "glass-status-old"
	const newConv = "glass-status-new"
	id, err := db.InsertSudoGrant(&db.SudoGrant{ConvID: oldConv, Slug: db.HostBreakGlassSlug, GrantedBy: db.HostBreakGlassGrantOrigin, ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)
	count, until := breakGlassStatus("other-agent")
	require.Equal(t, 1, count)
	require.Empty(t, until)
	count, until = breakGlassStatus(oldConv)
	require.Equal(t, 1, count)
	require.NotEmpty(t, until)
	_, err = db.RotateAgentConv(oldConv, newConv, "reincarnate")
	require.NoError(t, err)
	count, until = breakGlassStatus(newConv)
	require.Equal(t, 1, count)
	require.NotEmpty(t, until)
	require.NoError(t, db.GrantAgentPermission("cloned-agent", db.HostBreakGlassSlug, "system:clone"))
	count, until = breakGlassStatus("cloned-agent")
	require.Equal(t, 1, count)
	require.Empty(t, until)
	_, err = db.RevokeSudoGrant(id)
	require.NoError(t, err)
	count, until = breakGlassStatus(newConv)
	require.Zero(t, count)
	require.Empty(t, until)
}
