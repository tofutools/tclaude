package agentd_test

import (
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Flow coverage for the opt-in auto-tiling pass that follows a bulk
// window "focus" op (config focus.tile.enabled). The actual per-platform
// window arrangement is not unit-testable, so these scenarios swap the
// tiling seams (config gate + dispatch) for recorders and assert the
// system-under-test: that a bulk focus dispatches the tiling pass exactly
// when it should — enabled AND more than one window — with the focused
// windows handed over in a deterministic (conv-id) order and the resolved
// layout options passed through, and never on unfocus.

// tileRecorder captures the spec set + options the tiling dispatch was
// handed. Guarded because the dispatch runs from the request goroutine.
type tileRecorder struct {
	mu    sync.Mutex
	calls int
	specs []session.TileSpec
	opts  session.TileOptions
}

func (r *tileRecorder) install(t *testing.T) {
	t.Helper()
	t.Cleanup(agentd.SetTileAgentWindowsForTest(func(specs []session.TileSpec, opts session.TileOptions) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls++
		r.specs = append([]session.TileSpec(nil), specs...)
		r.opts = opts
	}))
}

func (r *tileRecorder) snapshot() (int, []session.TileSpec, session.TileOptions) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, append([]session.TileSpec(nil), r.specs...), r.opts
}

// enableTiling forces the post-focus tiling gate on with the given
// options, for a test that exercises the tiling path.
func enableTiling(t *testing.T, opts session.TileOptions) {
	t.Helper()
	t.Cleanup(agentd.SetTileConfigForFocusForTest(func() (bool, session.TileOptions) {
		return true, opts
	}))
}

// Scenario: a bulk focus with tiling enabled and two focused windows
// dispatches the tiling pass exactly once, handing over both windows in
// conv-id order with the resolved layout options.
func TestAgentWindows_Focus_TilesWhenEnabled(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()
	rec := newWinRecorder()
	rec.installFocus(t)
	tr := &tileRecorder{}
	tr.install(t)
	wantOpts := session.TileOptions{Layout: config.TileLayoutColumns, Resize: true, Gap: 12, Margin: 4}
	enableTiling(t, wantOpts)

	const group = "tclaude-dev"
	const convA = "wtia-1111-2222-3333-4444" // sorts before B
	const convB = "wtib-1111-2222-3333-4444"
	f.HaveGroup(group)
	f.HaveConvWithTitle(convA, "worker-a")
	f.HaveConvWithTitle(convB, "worker-b")
	f.HaveAliveSession(convA, "spwn-wtia", "tmux-wtia", f.TestCwd("wtia"))
	f.HaveAliveSession(convB, "spwn-wtib", "tmux-wtib", f.TestCwd("wtib"))
	f.HaveMember(group, convA)
	f.HaveMember(group, convB)

	code, resp := postAgentWindows(t, mux, map[string]any{
		"direction": "focus", "scope": "group", "group": group,
	})
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, 2, resp.Focused)

	agentd.WaitForBackgroundForTest() // tiling is dispatched in the background
	calls, specs, opts := tr.snapshot()
	require.Equal(t, 1, calls, "tiling dispatched exactly once for a bulk focus")
	require.Len(t, specs, 2)
	// Deterministic conv-id order: A before B; each spec carries the
	// session's tmux name + session id (the two handles tiling keys on).
	assert.Equal(t, session.TileSpec{TmuxSession: "tmux-wtia", SessionID: "spwn-wtia"}, specs[0])
	assert.Equal(t, session.TileSpec{TmuxSession: "tmux-wtib", SessionID: "spwn-wtib"}, specs[1])
	assert.Equal(t, wantOpts, opts, "the resolved layout options are passed through unchanged")
}

// Scenario: tiling enabled but only ONE window focused — the pass is a
// no-op (tiling a single window would just maximise it).
func TestAgentWindows_Focus_SingleWindowNotTiled(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()
	rec := newWinRecorder()
	rec.installFocus(t)
	tr := &tileRecorder{}
	tr.install(t)
	enableTiling(t, session.TileOptions{Layout: config.TileLayoutGrid})

	const group = "tclaude-dev"
	const solo = "wtsa-1111-2222-3333-4444"
	f.HaveGroup(group)
	f.HaveConvWithTitle(solo, "lonely-worker")
	f.HaveAliveSession(solo, "spwn-wtsa", "tmux-wtsa", f.TestCwd("wtsa"))
	f.HaveMember(group, solo)

	code, resp := postAgentWindows(t, mux, map[string]any{
		"direction": "focus", "scope": "group", "group": group,
	})
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, 1, resp.Focused)

	agentd.WaitForBackgroundForTest() // tiling is dispatched in the background
	calls, _, _ := tr.snapshot()
	assert.Equal(t, 0, calls, "a single focused window is never tiled")
}

// Scenario: two windows focused but tiling DISABLED — no tiling dispatch.
func TestAgentWindows_Focus_NoTileWhenDisabled(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()
	rec := newWinRecorder()
	rec.installFocus(t)
	tr := &tileRecorder{}
	tr.install(t)
	// newFlow already neutralizes the gate to "off"; be explicit for clarity.
	t.Cleanup(agentd.SetTileConfigForFocusForTest(func() (bool, session.TileOptions) {
		return false, session.TileOptions{}
	}))

	const group = "tclaude-dev"
	const convA = "wtda-1111-2222-3333-4444"
	const convB = "wtdb-1111-2222-3333-4444"
	f.HaveGroup(group)
	f.HaveConvWithTitle(convA, "worker-a")
	f.HaveConvWithTitle(convB, "worker-b")
	f.HaveAliveSession(convA, "spwn-wtda", "tmux-wtda", f.TestCwd("wtda"))
	f.HaveAliveSession(convB, "spwn-wtdb", "tmux-wtdb", f.TestCwd("wtdb"))
	f.HaveMember(group, convA)
	f.HaveMember(group, convB)

	code, resp := postAgentWindows(t, mux, map[string]any{
		"direction": "focus", "scope": "group", "group": group,
	})
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, 2, resp.Focused)

	agentd.WaitForBackgroundForTest() // tiling is dispatched in the background
	calls, _, _ := tr.snapshot()
	assert.Equal(t, 0, calls, "tiling must not run when focus.tile is disabled")
}

// Scenario: unfocus never tiles, even with tiling enabled — tiling is a
// focus-only follow-up, and the windows are being dismissed anyway.
func TestAgentWindows_Unfocus_NeverTiles(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()
	rec := newWinRecorder()
	rec.installDetach(t)
	tr := &tileRecorder{}
	tr.install(t)
	enableTiling(t, session.TileOptions{Layout: config.TileLayoutGrid})

	const group = "tclaude-dev"
	const convA = "wtua-1111-2222-3333-4444"
	const convB = "wtub-1111-2222-3333-4444"
	f.HaveGroup(group)
	f.HaveConvWithTitle(convA, "worker-a")
	f.HaveConvWithTitle(convB, "worker-b")
	f.HaveAliveSession(convA, "spwn-wtua", "tmux-wtua", f.TestCwd("wtua"))
	f.HaveAliveSession(convB, "spwn-wtub", "tmux-wtub", f.TestCwd("wtub"))
	f.HaveMember(group, convA)
	f.HaveMember(group, convB)

	code, resp := postAgentWindows(t, mux, map[string]any{
		"direction": "unfocus", "scope": "group", "group": group,
	})
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, 2, resp.Detached)

	agentd.WaitForBackgroundForTest() // tiling is dispatched in the background
	calls, _, _ := tr.snapshot()
	assert.Equal(t, 0, calls, "unfocus must never trigger the tiling pass")
}
