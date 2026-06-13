package agentd_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// JOH-161 — the Codex lifecycle verbs, harness-aware, exercised through the
// daemon mux against a CodexSim. Companion to TestCodexAgent_SpawnMessage-
// GracefulStop (JOH-160 spawn/stop): these pin the degraded/native-store
// paths the brief calls out — compact is a no-op (unsupported), rename goes
// to the native title store (threads.title) with NOTHING typed into the
// pane, and reincarnate carries both the codex identity and the title via
// SetTitle/Title. The critical invariant: no Codex pane is ever sent a
// slash command it can't parse.

// Codex exposes no scriptable compaction (Lifecycle.CompactCommand == ""),
// so a /compact request must be REFUSED with a clear "unsupported" — never
// typed into the pane as an unparseable `/compact` line.
func TestCodexAgent_CompactUnsupported(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	const conv = "019ec004-0000-0000-0000-000000000001"
	f.HaveAliveCodexSession(conv, "codex-1", "tmux-codex-1", "/work")
	f.HaveMember("crew", conv)

	res := f.AsHuman().Compact(conv)
	require.Equal(t, http.StatusBadRequest, res.Code,
		"compact on a codex agent must be rejected as unsupported; body=%s", res.Raw)
	assert.Contains(t, string(res.Raw), "does not support compact")

	// The gate fires before any injection: no /compact ever reached the pane.
	for _, sk := range f.World.Tmux.Sent() {
		assert.NotContains(t, sk.Text, "/compact",
			"a codex pane must never be typed /compact")
	}
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

	// Title carry: the predecessor's title (sourced via the native store)
	// was written back as `<prev>-x` through the out-of-band SetTitle. (The
	// successor's own async rename to `<prev>-r-1` logs a benign "no threads
	// row" warning here: a real Codex creates its threads row at startup, but
	// the freshly-spawned CodexSim doesn't model that, so the successor has
	// no row to UPDATE. The successor identity above is the assertable half;
	// its title would persist in production where Codex owns the row.)
	got, err := cx.ThreadTitle()
	require.NoError(t, err)
	assert.Equal(t, "original-codex-x", got,
		"predecessor renamed via the native store on reincarnate")
}
