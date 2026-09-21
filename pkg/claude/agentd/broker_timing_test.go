package agentd

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A request under the threshold is silent; one over it logs a per-phase
// breakdown including the proof's own sub-phases and which source answered
// the pane facts — the data that says WHERE a stall came from.
func TestBrokerTiming_LogsSlowRequestsWithPhases(t *testing.T) {
	t.Cleanup(ResetBrokerLimiterForTest())
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	run := func(resolve, proof, apply time.Duration) {
		tm := newBrokerTiming("/v1/whoami/hook", 4242)
		tm.now = clock
		tm.start, tm.last = now, now
		now = now.Add(resolve)
		tm.mark("resolve")
		tm.resolved = "spwn-x"
		now = now.Add(proof)
		tm.proof = &layerProof{dbDur: 5 * time.Millisecond, paneDur: proof - 10*time.Millisecond, walkDur: 5 * time.Millisecond, paneSource: "probe"}
		tm.mark("proof")
		now = now.Add(apply)
		tm.mark("apply")
		tm.finish()
	}

	run(10*time.Millisecond, 20*time.Millisecond, 30*time.Millisecond)
	assert.Empty(t, strings.TrimSpace(logs.String()), "a fast request is silent")

	run(100*time.Millisecond, 2500*time.Millisecond, 50*time.Millisecond)
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	require.Len(t, lines, 1)
	var rec map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &rec))
	assert.Equal(t, "WARN", rec["level"])
	assert.Contains(t, rec["msg"], "slow brokered request")
	assert.EqualValues(t, 4242, rec["caller_pid"])
	assert.Equal(t, "spwn-x", rec["resolved_session"])
	assert.EqualValues(t, 2650, rec["total_ms"])
	assert.EqualValues(t, 100, rec["resolve_ms"])
	assert.EqualValues(t, 2500, rec["proof_ms"])
	assert.EqualValues(t, 50, rec["apply_ms"])
	assert.EqualValues(t, 2490, rec["proof_pane_ms"], "the proof's own sub-phases point at the tmux probe")
	assert.Equal(t, "probe", rec["pane_source"])
	assert.Equal(t, "apply", rec["last_phase"])

	// A second slow request inside the throttle interval is counted, not logged.
	run(0, 3*time.Second, 0)
	assert.Len(t, strings.Split(strings.TrimSpace(logs.String()), "\n"), 1)
}
