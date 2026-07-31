//go:build linux

package agentd

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// TCL-896 asked whether the legacy-shared state shape has TCL-892's EROFS gap:
// that branch names no state directories in its LAYOUT and binds the ambient
// config app directory read-only, so a StateDirs-shaped bootstrap predicate
// would skip it and leave a shared-state launch to fail its first session
// creation with an opaque HTTP 500.
//
// It does not, and this test is why rather than an assurance that it does not.
// The layout is not what the bootstrap reads — the CONTRACT is, and
// BuildTclaudeLayerLaunchSpec fills StateDirs for OpenCode when the input
// leaves them empty, from the ambient XDG roots. So a legacy-shared contract
// carries four state directories whose config entry IS the ambient config app
// directory, the same directory its read-only bind names, and TCL-892's
// predicate matches it.
//
// The shape is therefore covered by construction, which is exactly the kind of
// coverage that disappears silently: nothing in TCL-892's tests goes through
// the spec builder, so a future change to how OpenCode contracts are built
// could take the legacy-shared launch back to the opaque 500 with every
// existing test still green. This one goes through the production builder and
// asserts the file lands.
func TestLegacySharedOpenCodeLaunchGetsItsConfigBootstrap(t *testing.T) {
	// Short temp root under /tmp: the OpenCode control socket path is built
	// beneath it and a long one overruns the Linux sockaddr limit.
	home, err := os.MkdirTemp("/tmp", "ocls-*")
	require.NoError(t, err)
	home, err = filepath.EvalSymlinks(home)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))

	// The ambient config app directory EXISTS but has no bootstrap file. That
	// is the host state the ticket is about: the layout binds this directory
	// read-only, and OpenCode writes .gitignore into it while creating a
	// session.
	ambientConfig := filepath.Join(home, "config", "opencode")
	require.NoError(t, os.MkdirAll(ambientConfig, 0o700))
	bootstrap := filepath.Join(ambientConfig, openCodeInstallBootstrapFile)
	require.NoFileExists(t, bootstrap)

	socketPath := filepath.Join(home, ".tclaude", "api", "agentd.sock")
	require.NoError(t, os.MkdirAll(filepath.Dir(socketPath), 0o700))
	socket, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = socket.Close() })
	t.Setenv(agentipc.SocketEnv, socketPath)
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)

	agentID := db.NewAgentID()
	inserted, err := db.InsertOpenCodeAgentStateAllocation(
		db.OpenCodeAgentStateAllocation{
			AgentID: agentID,
			Mode:    db.OpenCodeStateLegacyShared,
		})
	require.NoError(t, err)
	require.True(t, inserted)

	snapshot := sandboxpolicy.EmptySnapshot()
	spec, err := buildOpenCodeTclaudeLayerLaunchSpec(
		t.TempDir(), nil, &snapshot, agentID, false, false)
	require.NoError(t, err)
	require.NotNil(t, spec)

	// The contract facts the bootstrap depends on, asserted rather than
	// assumed — if a future change stops filling StateDirs for this shape, the
	// failure lands here with its reason rather than as an opaque 500 in
	// production.
	require.Len(t, spec.Contract.StateDirs, 4,
		"the builder fills the ambient XDG roots for a legacy-shared contract")
	assert.Equal(t, ambientConfig, spec.Contract.StateDirs[2])
	require.Equal(t, ambientConfig,
		openCodeReadOnlyConfigBindSource(spec.Contract),
		"the config app directory must be served read-only by a bind naming it")

	require.NoError(t, prepareOpenCodeReadOnlyConfigForPlatform(spec))
	raw, err := os.ReadFile(bootstrap)
	require.NoError(t, err,
		"without this file OpenCode's first session creation fails with EROFS behind an HTTP 500")
	// Content, not existence: a payload OpenCode would rewrite leaves a dirty
	// diff in the operator's own dotfiles.
	assert.Equal(t, openCodeInstallGitignore, string(raw))
}
