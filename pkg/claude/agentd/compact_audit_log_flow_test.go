package agentd_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A "spontaneous /compact" landing in an agent's pane must always be
// traceable to its source. send-keys is the one channel through which
// tclaude can make a pane run a command the agent did not type itself, so
// every lifecycle-slash injection now logs a line to ~/.tclaude/output.log
// naming the conv and what caused it. This drives the dashboard/human
// compact path (no calling conv → reason "human/dashboard") end to end
// through the daemon mux against a CC pane and asserts BOTH surfaces: the
// /compact reaching the pane AND the audit line that records why.
func TestCompact_LogsInjectionWithReason(t *testing.T) {
	// Capture slog for the duration of this test; restore on cleanup.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	f := newFlow(t)
	f.HaveGroup("crew")
	const conv = "019ec004-1111-2222-3333-444444444444"
	f.HaveAliveSession(conv, "cc-1", "tmux-cc-1", "/work")
	f.HaveMember("crew", conv)

	res := f.AsHuman().Compact(conv)
	require.Equal(t, http.StatusOK, res.Code,
		"human compact on a CC agent should succeed; body=%s", res.Raw)

	// Surface 1: /compact actually reached the pane.
	f.AssertSentContains("tmux-cc-1:0.0", "/compact", 2*time.Second)

	// Surface 2: the injection is recorded with the affected conv and its cause.
	logs := buf.String()
	assert.Contains(t, logs, "slash-command injected via send-keys",
		"the compaction injection must be logged for after-the-fact diagnosis")
	assert.Contains(t, logs, `reason="compact (human/dashboard)"`,
		"the audit line must record what caused the compaction")
	assert.Contains(t, logs, conv,
		"the audit line must name the affected conversation")
}
