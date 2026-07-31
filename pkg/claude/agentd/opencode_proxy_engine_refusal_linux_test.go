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
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// TestOpenCodeUnixRelayRefusesTheProxyEngine is the structural half of TCL-889,
// and the reason OpenCode's Linux `engine: proxy` capability cells cannot be
// activated by any amount of cooperation evidence: the boundary agentd actually
// launches OpenCode through REFUSES to deploy the proxy engine at all, so there
// is no enforcement to rate.
//
// It is pinned here, on the OpenCode path, because the existing session-package
// test of the same refusal (TestTclaudeLayerUnixRelayRefusesTheProxyEngine)
// builds its spec with HarnessName "claude". That proves the renderer refuses;
// it does not prove that the spec OpenCode's own builder produces reaches the
// refusal, which is the fact the capability rating rests on.
//
// The launch path under test is production's: openCodeServeProcessExec is the
// function that renders a Unix-relay OpenCode server launch, and the refusal
// must surface from it rather than from a renderer called directly. Only the
// bubblewrap host probe is stubbed — the smoke gate for THAT is the executor
// smoke; what this test must not depend on is whether the machine running it
// happens to have bwrap.
//
// Falsifiability: delete either refusal in sandbox_bwrap_linux.go /
// sandbox_bwrap.go and this test reports a rendered argv instead of an error.
func TestOpenCodeUnixRelayRefusesTheProxyEngine(t *testing.T) {
	// Own HOME and XDG_DATA_HOME before touching the database or allocating
	// state: both are read out of the environment, and a test that allocated
	// under the developer's real home would write outside its own tree.
	// A SHORT temp root under /tmp, not t.TempDir and not $TMPDIR: the v4
	// control socket path is built under this tree, and the spec builder
	// validates it against the Linux sockaddr limit BEFORE it reaches the
	// engine question. A long temp root therefore fails this test with
	// "control path exceeds Linux sockaddr capacity" — a fabricated failure
	// that says nothing about the refusal under test.
	home, err := os.MkdirTemp("/tmp", "ocp-*")
	require.NoError(t, err)
	home, err = filepath.EvalSymlinks(home)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	// The isolated boundary binds the canonical agentd socket, and the argv
	// renderer refuses a spec whose socket is absent. Bind a real one so the
	// renderer reaches the engine question it is being asked about.
	socketPath := filepath.Join(home, ".tclaude", "api", "agentd.sock")
	require.NoError(t, os.MkdirAll(filepath.Dir(socketPath), 0o700))
	socket, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = socket.Close() })
	t.Setenv(agentipc.SocketEnv, socketPath)
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)

	cwd := t.TempDir()
	agentID := db.NewAgentID()
	_, err = allocatePrivateOpenCodeState(agentID)
	require.NoError(t, err)

	snapshot := openCodeProxyEngineSnapshot()
	// The filtered builder is the one production reaches for a rules-carrying
	// profile; the engine rides the same snapshot into the spec.
	spec, err := buildOpenCodeTclaudeLayerLaunchSpec(
		cwd, nil, &snapshot, agentID, true, true)
	require.NoError(t, err)
	require.NotNil(t, spec)
	require.Equal(t, sandboxpolicy.NetworkEngineProxy, spec.Contract.NetworkEngine,
		"the spec under test must actually carry the proxy engine")

	previousResolve := resolveOpenCodeTclaudeLayer
	resolveOpenCodeTclaudeLayer = func(
		sandboxpolicy.NetworkPosture, sandboxpolicy.RootPosture,
	) (string, harness.LaunchOSSandbox, error) {
		return "/usr/bin/bwrap", harness.LaunchOSSandbox{}, nil
	}
	t.Cleanup(func() { resolveOpenCodeTclaudeLayer = previousResolve })

	runtime := &db.OpenCodeRuntime{
		SessionID: "opencode-proxy-engine-refusal",
		Transport: db.OpenCodeTransportUnixRelay,
	}
	_, _, _, _, cleanup, err := openCodeServeProcessExec(
		"/usr/bin/opencode", "41999", runtime, spec)
	cleanup()
	require.Error(t, err,
		"a proxy-engine OpenCode launch must fail closed, not render under the packet supervisor's fd contract")
	assert.ErrorContains(t, err,
		"the OpenCode Unix-relay launch does not support the proxy filtering engine")

	// Both renderers the launch calls refuse, not just whichever one runs
	// first. Keeping them in step is what stops a later change from lifting one
	// and leaving a launch that renders argv for a supervisor it never starts.
	_, _, fdErr := session.TclaudeLayerUnixRelayServerFDs(*spec)
	assert.ErrorContains(t, fdErr,
		"the OpenCode Unix-relay launch does not support the proxy filtering engine")
	_, argvErr := session.TclaudeLayerUnixRelayServerExecArgs(
		"/usr/bin/bwrap", *spec, 2, []string{"/usr/bin/opencode", "serve"})
	assert.ErrorContains(t, argvErr,
		"the OpenCode Unix-relay launch does not support the proxy filtering engine")
}

// TestOpenCodeProxyEngineCellsStayUnenforced is the rating that follows from
// the refusal above, asked of the REAL evaluator rather than of the activation
// map directly.
//
// The two tests belong together and are read together: the seam refuses to
// deploy the engine, therefore there is nothing enforced, therefore EnforceNone
// is the honest cell — not a pessimistic placeholder, and not something the
// carriage-cooperation arm (TestOpenCodeProxyCarriageCooperation) can flip,
// because that arm measures the harness's cooperation and not a deployed floor.
//
// Platform is passed explicitly rather than taken from runtime.GOOS so the
// assertion states which platform it is about — GOOS=darwin go vet cannot see
// into a runtime.GOOS branch (TCL-884 handoff §3.4).
func TestOpenCodeProxyEngineCellsStayUnenforced(t *testing.T) {
	snapshot := openCodeProxyEngineSnapshot()
	axes, err := sandboxpolicy.PlannedEffectiveAccessAxes(snapshot.Effective)
	require.NoError(t, err)

	predicted, err := harness.PredictAccessEnforcement(
		harness.MustGet(harness.OpenCodeName),
		sandboxpolicy.ImplementationTclaudeLayer, axes, "", "linux",
	)
	require.NoError(t, err)
	assert.Equal(t, harness.EnforceNone, predicted.NetworkList,
		"OpenCode's proxy-engine allow cells have no deployed engine to rate")
	assert.Empty(t, predicted.NetworkSelectors)
	assert.Equal(t, harness.EnforceNone, predicted.NetworkPorts)
	assert.Empty(t, harness.ProxyEngineActivationSmokes(harness.OpenCodeName),
		"no smoke may be recorded for cells the launch seam cannot deploy")
	assert.Contains(t, predicted.NetworkEngineDetail,
		harness.ProxyEngineNotActivatedNotice,
		"the operator-facing surface must keep saying these cells are not activated")
}

// openCodeProxyEngineSnapshot authors the one profile both tests above are
// about: a discriminating rule set that selects the proxy engine explicitly.
func openCodeProxyEngineSnapshot() sandboxpolicy.Snapshot {
	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.Effective.Network = &sandboxpolicy.NetworkRules{
		Mode:   sandboxpolicy.AccessModeList,
		Engine: sandboxpolicy.NetworkEngineProxy,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{Domain: "example.com", Ports: []int{443}},
		},
	}
	return snapshot
}
