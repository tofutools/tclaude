package agentd

import (
	"context"
	"net"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// The refusal this ticket added: the API drive is a loopback TCP channel, and a
// network posture that unshares the namespace makes the port unreachable no
// matter how long anyone waits. Refusing at the spawn boundary is what turns
// that into a named answer instead of a pane that comes up and cannot be
// talked to.
func TestCopilotAPILoopbackFailure(t *testing.T) {
	closedNetwork := &sandboxpolicy.Snapshot{
		Effective: sandboxpolicy.EffectiveProfile{
			Network: &sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeClosed},
		},
	}
	privateNetwork := &sandboxpolicy.Snapshot{
		Effective: sandboxpolicy.EffectiveProfile{
			Network: &sandboxpolicy.NetworkRules{
				Mode:      sandboxpolicy.AccessModeOpen,
				Namespace: sandboxpolicy.NetworkNamespacePrivate,
			},
		},
	}
	openNetwork := &sandboxpolicy.Snapshot{
		Effective: sandboxpolicy.EffectiveProfile{
			Network: &sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeOpen},
		},
	}

	t.Run("a launch that did not ask for the API drive is untouched", func(t *testing.T) {
		assert.Nil(t, copilotAPILoopbackFailure(
			false, privateNetwork, string(sandboxpolicy.ImplementationTclaudeLayer)),
			"this gates the channel, not the sandbox: a send-keys agent may be "+
				"isolated exactly as before")
	})

	t.Run("the API drive under host-open networking is admitted", func(t *testing.T) {
		assert.Nil(t, copilotAPILoopbackFailure(
			true, openNetwork, string(sandboxpolicy.ImplementationTclaudeLayer)))
	})

	t.Run("the API drive under a private namespace is refused", func(t *testing.T) {
		fail := copilotAPILoopbackFailure(
			true, privateNetwork, string(sandboxpolicy.ImplementationTclaudeLayer))
		require.NotNil(t, fail, "an unreachable port must be refused, not launched")
		assert.Equal(t, "copilot_api_unreachable_network_posture", fail.Kind)
		assert.Contains(t, fail.Msg, "filtered",
			"the refusal must name the posture the launch would actually have built")
		assert.Contains(t, fail.Msg, "host loopback",
			"the refusal must say what the drive needs, so it is actionable")
		assert.Contains(t, fail.Msg, "network.namespace host or omitted")
	})

	t.Run("the API drive under closed networking is refused", func(t *testing.T) {
		fail := copilotAPILoopbackFailure(
			true, closedNetwork, string(sandboxpolicy.ImplementationTclaudeLayer))
		require.NotNil(t, fail)
		assert.Contains(t, fail.Msg, "isolated-with-agentd")
	})

	t.Run("no outer layer is admitted whatever the profile says", func(t *testing.T) {
		assert.Nil(t, copilotAPILoopbackFailure(
			true, privateNetwork, string(sandboxpolicy.ImplementationOff)),
			"without a tclaude-built namespace there is only one loopback")
	})

	t.Run("an unreadable profile is refused rather than assumed reachable", func(t *testing.T) {
		broken := &sandboxpolicy.Snapshot{
			Effective: sandboxpolicy.EffectiveProfile{
				Network: &sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessMode("nonsense")},
			},
		}
		assert.NotNil(t, copilotAPILoopbackFailure(
			true, broken, string(sandboxpolicy.ImplementationTclaudeLayer)))
	})
}

// The port must reach the forked `tclaude session new` on both launch paths. A
// resume that dropped it would relaunch an API agent with no embedded server.
func TestSessionArgsCarryTheCopilotAPIPort(t *testing.T) {
	for name, args := range map[string][]string{
		"new": sessionNewArgs(clcommon.SpawnArgs{
			Label: "lbl", Cwd: "/tmp/x", CopilotAPI: true, CopilotAPIPort: 4599}),
		"resume": sessionResumeArgs(clcommon.SpawnArgs{
			ConvID: "conv-1", Cwd: "/tmp/x", CopilotAPI: true, CopilotAPIPort: 4599}),
	} {
		t.Run(name, func(t *testing.T) {
			i := slices.Index(args, "--copilot-api-port")
			require.GreaterOrEqual(t, i, 0, "the allocated port must be forwarded: %v", args)
			require.Less(t, i+1, len(args))
			assert.Equal(t, "4599", args[i+1])
			assert.Contains(t, args, "--copilot-api",
				"the port is meaningless without the drive that uses it")
		})
	}
}

// An unallocated port omits the flag entirely, so a send-keys launch produces
// exactly the argv it produced before this field existed.
func TestSessionArgsOmitTheCopilotAPIPortWhenUnset(t *testing.T) {
	for name, args := range map[string][]string{
		"new":    sessionNewArgs(clcommon.SpawnArgs{Label: "lbl", Cwd: "/tmp/x"}),
		"resume": sessionResumeArgs(clcommon.SpawnArgs{ConvID: "conv-1", Cwd: "/tmp/x"}),
	} {
		t.Run(name, func(t *testing.T) {
			assert.NotContains(t, args, "--copilot-api-port")
			assert.NotContains(t, args, "--copilot-api")
		})
	}
}

// The allocator must hand back a port that is actually free. It binds and
// releases — the bind-close-exec gap this design accepts knowingly, because
// copilot cannot be given a pre-bound listener.
func TestAllocateCopilotAPIPortReturnsAFreePort(t *testing.T) {
	port, err := allocateCopilotAPIPort()
	require.NoError(t, err)
	assert.Positive(t, port)

	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	require.NoError(t, err, "the allocated port must be bindable afterwards")
	require.NoError(t, listener.Close())
}

// A send-keys launch allocates nothing at all — no port, no record, no change.
func TestPrepareCopilotAPIPortIsQuietWithoutTheDrive(t *testing.T) {
	args := clcommon.SpawnArgs{SessionID: "conv-1"}
	require.NoError(t, prepareCopilotAPIPort(&args))
	assert.Zero(t, args.CopilotAPIPort)
}

// Allocation must NOT depend on knowing the conversation yet. The port has to
// be in the argv, so it is chosen before the fork; the conv id does not, and an
// unenrolled clone or reincarnation mints it afterwards. Coupling the two would
// refuse those launches outright rather than record their port a moment later.
func TestPrepareCopilotAPIPortAllocatesWithoutAConversation(t *testing.T) {
	args := clcommon.SpawnArgs{CopilotAPI: true}
	require.NoError(t, prepareCopilotAPIPort(&args))
	assert.Positive(t, args.CopilotAPIPort,
		"a launch that mints its conv-id later still needs a port in its argv")
}

// The happy path: a port is chosen and written into the args the argv is built
// from, so the number agentd holds is the number the pane is told to bind.
func TestPrepareCopilotAPIPortAllocatesForTheDrive(t *testing.T) {
	args := clcommon.SpawnArgs{CopilotAPI: true, SessionID: "conv-1"}
	require.NoError(t, prepareCopilotAPIPort(&args))
	assert.Positive(t, args.CopilotAPIPort)
}

// The record's whole lifecycle, including the part that matters most: a
// relaunch REPLACES the port rather than accumulating, and an exit removes the
// claim entirely.
func TestCopilotAPIRuntimeRecordLifecycle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()

	require.NoError(t, db.UpsertCopilotAPIRuntime(
		db.CopilotAPIRuntime{ConvID: "conv-1", Port: 4599}))
	got, err := db.GetCopilotAPIRuntime("conv-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 4599, got.Port)

	// A relaunch binds a newly allocated port. The old number must not survive
	// it — a stale port is the convincing lie this design is trying to avoid.
	require.NoError(t, db.UpsertCopilotAPIRuntime(
		db.CopilotAPIRuntime{ConvID: "conv-1", Port: 4600}))
	got, err = db.GetCopilotAPIRuntime("conv-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 4600, got.Port, "a relaunch must replace the recorded port")

	require.NoError(t, db.DeleteCopilotAPIRuntime("conv-1"))
	got, err = db.GetCopilotAPIRuntime("conv-1")
	require.NoError(t, err)
	assert.Nil(t, got, "an exited agent must leave no port claim behind")
}

// A conversation nobody launched on the API drive has no port, and asking for
// one says so rather than returning a zero that a caller might dial.
func TestVerifiedCopilotAPIPortWithoutARecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()

	_, _, err := verifiedCopilotAPIPort(context.Background(), "conv-unknown")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no recorded Copilot API port")
}

// The three ways verification can fail have three different remedies, so they
// must not collapse into one message. Each branch is built from what the wait
// actually observed.
func TestCopilotAPIUnverifiedErrorNamesTheFailure(t *testing.T) {
	noPane := copilotAPIUnverifiedError("conv-1", 4599, false, false)
	assert.Contains(t, noPane.Error(), "no live pane process")

	noListener := copilotAPIUnverifiedError("conv-1", 4599, true, false)
	assert.Contains(t, noListener.Error(), "nothing ever listened")
	assert.Contains(t, noListener.Error(), "folder trust",
		"the likeliest cause is a startup prompt that blocks the TUI, so name it")

	// The bind race, lost. This is the security-relevant one: something is
	// listening and it is provably not ours, so the only safe move is to refuse.
	foreign := copilotAPIUnverifiedError("conv-1", 4599, true, true)
	assert.Contains(t, foreign.Error(), "outside the agent's pane subtree")
	assert.Contains(t, foreign.Error(), "no authentication",
		"the refusal must say why an unverified listener cannot simply be used")
}

// Regression: a relaunched agent's port record must survive its own
// predecessor.
//
// Session rows are per-launch and exited ones are retained, but the port record
// is per conversation and a relaunch reuses the conv id. So the predecessor's
// exited row and the successor's live row share an id, and releasing off the
// exited row alone deleted the successor's record — on the next tick, and every
// tick after, because nothing ever cleans the predecessor up.
func TestReleaseCopilotAPIPortKeepsARelaunchedConversation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()

	require.NoError(t, db.UpsertCopilotAPIRuntime(
		db.CopilotAPIRuntime{ConvID: "conv-1", Port: 4599}))

	// The predecessor's exited row is what drives the release, but the
	// conversation still has a live launch.
	releaseCopilotAPIPortForExit(
		harness.CopilotName, "conv-1", map[string]bool{"conv-1": true}, 0)

	got, err := db.GetCopilotAPIRuntime("conv-1")
	require.NoError(t, err)
	require.NotNil(t, got, "a conversation with a live launch must keep its port")
	assert.Equal(t, 4599, got.Port)
}

// Regression: a record younger than the spawn grace must not be released.
//
// A launch records its port immediately after the fork; its session row appears
// a moment later. A reaper tick landing in between sees a conversation with no
// live launch and a record that is nonetheless perfectly good.
func TestReleaseCopilotAPIPortSparesAFreshRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()

	require.NoError(t, db.UpsertCopilotAPIRuntime(
		db.CopilotAPIRuntime{ConvID: "conv-1", Port: 4599}))

	releaseCopilotAPIPortForExit(
		harness.CopilotName, "conv-1", map[string]bool{}, time.Hour)

	got, err := db.GetCopilotAPIRuntime("conv-1")
	require.NoError(t, err)
	assert.NotNil(t, got, "a just-recorded port belongs to a launch still coming up")
}

// The case release exists for: the conversation has no live launch and its
// record is old enough that no launch can still be starting.
func TestReleaseCopilotAPIPortDropsADeadConversation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()

	require.NoError(t, db.UpsertCopilotAPIRuntime(
		db.CopilotAPIRuntime{ConvID: "conv-1", Port: 4599}))

	releaseCopilotAPIPortForExit(harness.CopilotName, "conv-1", map[string]bool{}, 0)

	got, err := db.GetCopilotAPIRuntime("conv-1")
	require.NoError(t, err)
	assert.Nil(t, got, "an exited agent must leave no port claim behind")
}

// A non-Copilot exit must not reach the database at all — the reaper walks
// every session row on every tick.
func TestReleaseCopilotAPIPortIgnoresOtherHarnesses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()

	require.NoError(t, db.UpsertCopilotAPIRuntime(
		db.CopilotAPIRuntime{ConvID: "conv-1", Port: 4599}))

	releaseCopilotAPIPortForExit(harness.CodexName, "conv-1", map[string]bool{}, 0)

	got, err := db.GetCopilotAPIRuntime("conv-1")
	require.NoError(t, err)
	assert.NotNil(t, got, "another harness's exit says nothing about a Copilot port")
}
