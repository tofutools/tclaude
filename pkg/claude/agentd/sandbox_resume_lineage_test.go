package agentd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func lineageSnapshot(t *testing.T, grants ...sandboxpolicy.FilesystemGrant) sandboxpolicy.Snapshot {
	t.Helper()
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Explicit: &sandboxpolicy.Profile{
		Name: "p", Filesystem: grants,
	}})
	require.NoError(t, err)
	return sandboxpolicy.NewSnapshot(effective, nil)
}

// The security-critical half of the clamp: an operator edits the profile AFTER
// launch to carve a path out from beneath a deny the agent is running under.
// Resume must drop that reopen rather than hand a live agent authority it never
// launched with. (The mirror of RequireContained's reopen-under-deny refusal,
// clamping instead of erroring because resume has no human in the loop.)
func TestClampResumeDenyLineageDropsReopensBeneathALaunchedDeny(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	denied := filepath.Join(root, "home")
	workspace := filepath.Join(denied, "workspace")
	secrets := filepath.Join(denied, "secrets")
	for _, dir := range []string{workspace, secrets} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}

	previous := lineageSnapshot(t,
		sandboxpolicy.FilesystemGrant{Path: denied, Access: sandboxpolicy.AccessDeny},
		sandboxpolicy.FilesystemGrant{Path: workspace, Access: sandboxpolicy.AccessWrite},
	)
	// The edited profile keeps the deny but adds a NEW carve-out beneath it.
	current := lineageSnapshot(t,
		sandboxpolicy.FilesystemGrant{Path: denied, Access: sandboxpolicy.AccessDeny},
		sandboxpolicy.FilesystemGrant{Path: workspace, Access: sandboxpolicy.AccessWrite},
		sandboxpolicy.FilesystemGrant{Path: secrets, Access: sandboxpolicy.AccessRead},
	)

	got := clampResumeDenyLineage(current, previous)
	assert.Equal(t, []sandboxpolicy.FilesystemGrant{
		{Path: denied, Access: sandboxpolicy.AccessDeny},
		{Path: workspace, Access: sandboxpolicy.AccessWrite},
	}, got.Effective.Filesystem, "a reopen the launched snapshot did not permit must be dropped")
	assert.NotContains(t, got.Effective.Provenance.Filesystem, secrets,
		"provenance must not keep naming a profile for a rule that is no longer applied")
}

// A reopen the previous snapshot ALREADY permitted survives: the clamp is an
// intersection, not a blanket refusal of everything beneath a deny.
func TestClampResumeDenyLineageKeepsPreviouslyPermittedReopens(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	denied := filepath.Join(root, "home")
	workspace := filepath.Join(denied, "workspace")
	nested := filepath.Join(workspace, "pkg")
	require.NoError(t, os.MkdirAll(nested, 0o755))

	previous := lineageSnapshot(t,
		sandboxpolicy.FilesystemGrant{Path: denied, Access: sandboxpolicy.AccessDeny},
		sandboxpolicy.FilesystemGrant{Path: workspace, Access: sandboxpolicy.AccessWrite},
	)
	// Narrowing an already-permitted subtree is not widening.
	current := lineageSnapshot(t,
		sandboxpolicy.FilesystemGrant{Path: denied, Access: sandboxpolicy.AccessDeny},
		sandboxpolicy.FilesystemGrant{Path: nested, Access: sandboxpolicy.AccessRead},
	)

	got := clampResumeDenyLineage(current, previous)
	assert.Equal(t, []sandboxpolicy.FilesystemGrant{
		{Path: denied, Access: sandboxpolicy.AccessDeny},
		{Path: nested, Access: sandboxpolicy.AccessRead},
	}, got.Effective.Filesystem)
}

// Both widenings at once: the edit drops the deny AND adds a carve-out. The
// deny is re-imposed and the carve-out is dropped.
func TestClampResumeDenyLineageHandlesDroppedDenyAndNewReopenTogether(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	denied := filepath.Join(root, "home")
	secrets := filepath.Join(denied, "secrets")
	require.NoError(t, os.MkdirAll(secrets, 0o755))

	previous := lineageSnapshot(t, sandboxpolicy.FilesystemGrant{Path: denied, Access: sandboxpolicy.AccessDeny})
	current := lineageSnapshot(t, sandboxpolicy.FilesystemGrant{Path: secrets, Access: sandboxpolicy.AccessRead})

	got := clampResumeDenyLineage(current, previous)
	assert.Equal(t, []sandboxpolicy.FilesystemGrant{
		{Path: denied, Access: sandboxpolicy.AccessDeny},
	}, got.Effective.Filesystem)
}
