package agentd_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// JOH-161 — the Codex lifecycle verbs, harness-aware, exercised through the
// daemon mux against a CodexSim. Companion to TestCodexAgent_SpawnMessage-
// GracefulStop (JOH-160 spawn/stop): these pin the degraded/native-store
// paths the brief calls out — compact uses Codex's native `/compact`, rename
// goes to the native title store (threads.title) with NOTHING typed into the
// pane, and reincarnate carries both the codex identity and the title via
// SetTitle/Title.

// Codex exposes `/compact`, so the daemon should use the same harness-aware
// slash injection path it uses for Claude Code.
func TestCodexAgent_CompactInjectsNativeCommand(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	const conv = "019ec004-0000-0000-0000-000000000001"
	f.HaveAliveCodexSession(conv, "codex-1", "tmux-codex-1", "/work")
	f.HaveMember("crew", conv)

	res := f.AsHuman().Compact(conv)
	require.Equal(t, http.StatusOK, res.Code,
		"compact on a codex agent must succeed; body=%s", res.Raw)
	f.AssertSentContains("tmux-codex-1:0.0", "/compact", 2*time.Second)
}

// Codex has no in-pane rename command, so a rename writes threads.title
// directly via ConvStore.SetTitle. The daemon's rename endpoint must route
// a Codex conv to that native store, the new title must read back, and
// NOTHING may be injected into the pane (out-of-band, not a send-keys).
func TestCodexAgent_RenameViaNativeStore(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	const conv = "019ec004-0000-0000-0000-000000000002"
	cx := f.HaveAliveCodexSession(conv, "codex-2", "tmux-codex-2", "/work")
	f.HaveMember("crew", conv)
	// Codex creates the threads row at session start; its title begins as
	// the derived first message.
	require.NoError(t, cx.WriteThreadRow(testharness.CodexThreadSeed{
		Title: "hello", FirstUserMessage: "hello", Cwd: "/work",
	}))

	res := f.AsHuman().Rename(conv, "renamed-by-human")
	require.Equal(t, http.StatusOK, res.Code,
		"rename should be delivered via the native title store; body=%s", res.Raw)

	// The native store carries the new title.
	got, err := cx.ThreadTitle()
	require.NoError(t, err)
	assert.Equal(t, "renamed-by-human", got, "threads.title updated by the out-of-band rename")

	// Out-of-band: no rename command was typed into the codex pane.
	for _, sk := range f.World.Tmux.Sent() {
		assert.False(t, strings.Contains(sk.Text, "rename"),
			"a codex rename must not inject into the pane (got %q)", sk.Text)
	}
}

// Reincarnating a Codex agent must (a) bring the successor back under the
// SAME harness — codex, not Claude Code — and (b) carry the predecessor's
// title, read from the native store (threads.title) and written onto the
// retired pane as `<prev>-x` through SetTitle. Pins both halves of
// "identity + title preservation".
func TestCodexAgent_ReincarnateCarriesIdentityAndTitle(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	const conv = "019ec004-0000-0000-0000-000000000003"
	cx := f.HaveAliveCodexSession(conv, "codex-3", "tmux-codex-3", "/work")
	f.HaveMember("crew", conv)
	// Predecessor's title lives in the native store, not the conv_index the
	// CC path reads.
	require.NoError(t, cx.WriteThreadRow(testharness.CodexThreadSeed{
		Title: "original-codex", Cwd: "/work",
	}))

	rein := f.AsHuman().Reincarnate(conv, "fresh start")
	require.NotEmpty(t, rein.NewConv, "reincarnate should yield a successor conv")

	// Identity: the successor is spawned under the same harness.
	sessions, err := db.FindSessionsByConvID(rein.NewConv)
	require.NoError(t, err)
	require.NotEmpty(t, sessions, "successor session row should exist")
	assert.Equal(t, "codex", sessions[0].Harness, "reincarnated codex agent stays codex")

	// Title carry, predecessor half: the retired pane was renamed to
	// `<prev>-x` through the out-of-band SetTitle on its native store.
	got, err := cx.ThreadTitle()
	require.NoError(t, err)
	assert.Equal(t, "original-codex-x", got,
		"predecessor renamed via the native store on reincarnate")

	// Title carry, successor half (end-to-end): the freshly-spawned Codex
	// pane models Codex's session-start threads-row creation, so its async
	// rename to `<prev>-r-1` lands on a real row. Poll the successor sim's
	// threads.title until the post-spawn rename goroutine has delivered it.
	newCx := f.World.Codexes.GetByConvID(rein.NewConv)
	require.NotNil(t, newCx, "successor CodexSim should be registered")
	require.Eventually(t, func() bool {
		title, err := newCx.ThreadTitle()
		return err == nil && title == "original-codex-r-1"
	}, 3*time.Second, 20*time.Millisecond,
		"successor's `<prev>-r-1` rename should persist to its native threads.title")
}
