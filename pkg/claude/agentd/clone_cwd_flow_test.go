package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: the human clones an agent but redirects the clone into a
// different directory — typically a git worktree, so the sibling picks
// up work on a parallel branch instead of fighting over the source's
// checkout.
//
// The dashboard's clone modal resolves its worktree picker to a path
// and passes it as `cwd`; here we pass a plain existing directory to
// keep the test off real git. The clone's spawned session must land in
// that directory, not the source's cwd.
func TestClone_CwdOverrideSpawnsThere(t *testing.T) {
	f := newFlow(t)

	const oldConv = "old-clonecwd-aaaa-bbbb-cccc"
	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveAliveSession(oldConv, "spwn-clonecwd-1", "tclaude-spwn-clonecwd-1", "/tmp/work")
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)

	// World.HomeDir is a real temp dir, so it passes the daemon's
	// exists-and-is-a-directory check.
	c := f.AsHuman().CloneWith(oldConv, map[string]any{
		"no_copy_conv": true,
		"cwd":          f.World.HomeDir,
	})
	require.Equalf(t, http.StatusOK, c.Code, "clone with cwd override should succeed; body=%s", c.Raw)

	sess, err := db.FindSessionByConvID(c.NewConv)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, f.World.HomeDir, sess.Cwd,
		"clone should spawn in the override cwd, not the source's cwd")
}

// Scenario: the human clones an agent with a cwd override that doesn't
// exist. Same fail-fast contract as spawn — an immediate 400 rather
// than a clone that silently never materialises.
func TestClone_InvalidCwdOverrideReportsError(t *testing.T) {
	f := newFlow(t)

	const oldConv = "old-badclonecwd-aaaa-bbbb"
	f.HaveConvWithTitle(oldConv, "worker")
	f.HaveAliveSession(oldConv, "spwn-badclonecwd-1", "tclaude-spwn-badclonecwd-1", "/tmp/work")
	f.HaveGroup("alpha")
	f.HaveMember("alpha", oldConv)

	c := f.AsHuman().CloneWith(oldConv, map[string]any{
		"no_copy_conv": true,
		"cwd":          "/no/such/clone/directory",
	})

	assert.Equalf(t, http.StatusBadRequest, c.Code,
		"clone with a bad cwd override should 400; body=%s", c.Raw)
	assert.Contains(t, string(c.Raw), "does not exist")
}
