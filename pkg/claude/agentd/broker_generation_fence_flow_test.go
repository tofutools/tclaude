package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/claude/statusbar"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A session row outlives the launch that created it: a resume or a relaunch
// under the same label reuses it under a new launch generation. A process of
// the launch it replaced can still be alive and still report. These pin that
// agentd drops what such a process sends, on both brokered endpoints, so an
// old launch never moves the state of the one that replaced it.

const (
	fenceConv  = "fe000000-1111-2222-3333-444444444444"
	fenceLabel = "spwn-broker-fence"
)

func fenceSession(t *testing.T) (*testharness.Flow, int, string) {
	t.Helper()
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, fenceConv, fenceLabel, "tmux-broker-fence", brokerPanePID)
	return f, callerPID, "test-layer-generation-" + fenceLabel
}

func TestBrokerFence_HookFromASupersededLaunchIsDropped(t *testing.T) {
	f, callerPID, current := fenceSession(t)
	before, err := db.LoadSession(fenceLabel)
	require.NoError(t, err)
	require.NotEqual(t, session.StatusWorking, before.Status)
	prompt := session.HookCallbackInput{HookEventName: "UserPromptSubmit", Prompt: "go", ConvID: fenceConv, Cwd: f.World.HomeDir}

	code, _ := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		ClaimedSessionID: fenceLabel, ExitGeneration: "generation-of-the-launch-before", Input: prompt,
	})
	assert.Equal(t, http.StatusConflict, code, "an event from the launch this one replaced")
	row, err := db.LoadSession(fenceLabel)
	require.NoError(t, err)
	assert.Equal(t, before.Status, row.Status, "the old launch's turn must not move the new launch's status")
	assert.True(t, before.LastHook.Equal(row.LastHook), "nor count as a sign of life from it")

	code, _ = postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		ClaimedSessionID: fenceLabel, ExitGeneration: current, Input: prompt,
	})
	require.Equal(t, http.StatusOK, code, "the current launch's own event is applied")
	row, err = db.LoadSession(fenceLabel)
	require.NoError(t, err)
	assert.Equal(t, session.StatusWorking, row.Status)

	// A callback that predates the generation carries none and is not fenced.
	code, _ = postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		ClaimedSessionID: fenceLabel,
		Input:            session.HookCallbackInput{HookEventName: "Stop", ConvID: fenceConv, Cwd: f.World.HomeDir},
	})
	assert.Equal(t, http.StatusOK, code)
	row, err = db.LoadSession(fenceLabel)
	require.NoError(t, err)
	assert.NotEqual(t, session.StatusWorking, row.Status)
}

func TestBrokerFence_RenderFromASupersededLaunchIsDropped(t *testing.T) {
	f, callerPID, current := fenceSession(t)
	f.HaveEnrolledAgent(fenceConv)
	render := statusbar.BrokeredRenderRequest{
		ClaimedSessionID: fenceLabel,
		RenderConvID:     fenceConv,
		Payload:          statuslinePayload(fenceConv, "Opus 5", "claude-opus-5", "high", 42, 84000, 3000, 200000, 1.25),
		ApplyWrites:      true,
	}

	render.ExitGeneration = "generation-of-the-launch-before"
	code, _ := postBrokeredRender(t, f, callerPID, render)
	assert.Equal(t, http.StatusConflict, code)
	snapshot, err := db.GetContextSnapshot(fenceLabel)
	require.NoError(t, err)
	assert.Empty(t, snapshot.ModelID, "the old launch's render must not paint the new launch's row")

	render.ExitGeneration = current
	code, _ = postBrokeredRender(t, f, callerPID, render)
	require.Equal(t, http.StatusOK, code)
	snapshot, err = db.GetContextSnapshot(fenceLabel)
	require.NoError(t, err)
	assert.Equal(t, "claude-opus-5", snapshot.ModelID)
}

// Every managed launch now brokers its hooks, not only the sandboxed ones. An
// ordinary launch has no wall and no bubblewrap in its ancestry: the hook
// runs under the harness, which runs under the pane shell the session row was
// keyed by at spawn. The daemon has to place that caller too, from its
// ancestry alone, keep placing it once the row's pid has been corrected to
// the harness, and fence it by generation like any other.
func TestHookBroker_PlacesAnOrdinaryManagedLaunch(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	const conv, label, tmux, generation = "0d000000-1111-2222-3333-444444444444", "spwn-broker-ordinary", "tmux-broker-ordinary", "generation-of-this-launch"
	const hookPID, harnessPID, panePID = 7300, 7301, 7302
	f := newFlow(t)
	f.HaveAliveSession(conv, label, tmux, f.World.HomeDir)
	row, err := db.LoadSession(label)
	require.NoError(t, err)
	row.PID = panePID
	row.ExitLaunchGeneration = generation
	require.NoError(t, db.SaveSession(row))
	require.NotEqual(t, "tclaude-layer", row.SandboxImplementation)
	f.World.Tmux.SetPaneIdentityForTest(tmux, "%1", panePID)
	f.World.Tmux.SetPaneExitGeneration(tmux, generation)
	t.Cleanup(agentd.SetProcTreeForTest(
		map[int]string{hookPID: "tclaude", harnessPID: "node", panePID: "sh"},
		map[int]int{hookPID: harnessPID, harnessPID: panePID},
	))

	send := func(event session.HookCallbackInput, claimedGeneration string) int {
		event.ConvID, event.Cwd = conv, f.World.HomeDir
		code, _ := postBrokeredHook(t, f, hookPID, session.BrokeredHookRequest{
			ClaimedSessionID: label, ExitGeneration: claimedGeneration, Input: event,
		})
		return code
	}

	require.Equal(t, http.StatusOK, send(session.HookCallbackInput{HookEventName: "SessionStart", Source: "startup"}, generation))
	require.Equal(t, http.StatusOK, send(session.HookCallbackInput{HookEventName: "UserPromptSubmit", Prompt: "go"}, generation))
	row, err = db.LoadSession(label)
	require.NoError(t, err)
	assert.Equal(t, session.StatusWorking, row.Status, "the ordinary launch's hooks reach its row through the broker")

	// Whatever pid the row now records, the same caller is still placed.
	require.Equal(t, http.StatusOK, send(session.HookCallbackInput{HookEventName: "Stop"}, generation))
	row, err = db.LoadSession(label)
	require.NoError(t, err)
	assert.NotEqual(t, session.StatusWorking, row.Status)

	assert.Equal(t, http.StatusConflict,
		send(session.HookCallbackInput{HookEventName: "UserPromptSubmit", Prompt: "late"}, "generation-of-the-launch-before"))
	row, err = db.LoadSession(label)
	require.NoError(t, err)
	assert.NotEqual(t, session.StatusWorking, row.Status, "and a superseded launch's event is dropped for it as well")
}
