package agentd

import (
	"log/slog"
	"time"
)

// --- slow brokered-request log ---
//
// A brokered callback's client waits a bounded time for the daemon: three
// seconds for a statusline render, twenty for a hook. When the daemon
// overruns that, the client gives up, the request's work is wasted, and
// the only trace used to be a refusal misreported as an ancestry mismatch.
// The stall can come from several places — the process-table reads, the
// database, the tmux pane facts, or applying the event under the session's
// hook lock — and one refusal line cannot say which.
//
// brokerTiming records how long each phase of a request took and writes
// one WARN when the total crosses brokerSlowRequestThreshold, with the
// per-phase breakdown. Throttled per endpoint, since a daemon that is slow
// is slow for many requests at once.

// brokerSlowRequestThreshold is the total after which a request is logged.
// A third of the statusline client's patience: slow enough to be a real
// stall, early enough to be seen before the client starts timing out.
const brokerSlowRequestThreshold = time.Second

const brokerSlowLogKey = "\x00slow"

// brokerTiming is one request's phase clock. Phases are marked in order;
// the proof's internal sub-phases are attached separately because they are
// measured inside proveTclaudeLayerCallerIn.
type brokerTiming struct {
	endpoint  string
	callerPID int
	resolved  string
	start     time.Time
	last      time.Time
	phases    []brokerPhase
	proof     *layerProof
	lastPhase string
	now       func() time.Time
}

type brokerPhase struct {
	name string
	d    time.Duration
}

func newBrokerTiming(endpoint string, callerPID int) *brokerTiming {
	t := &brokerTiming{endpoint: endpoint, callerPID: callerPID, now: time.Now}
	t.start = t.now()
	t.last = t.start
	return t
}

// mark closes the phase that ran since the previous mark under name.
func (t *brokerTiming) mark(name string) {
	now := t.now()
	t.phases = append(t.phases, brokerPhase{name: name, d: now.Sub(t.last)})
	t.last = now
	t.lastPhase = name
}

// finish is deferred by the handler: it logs when the request was slow.
// The row and proof are read at finish time so a handler can set them as
// they become known.
func (t *brokerTiming) finish() {
	total := t.now().Sub(t.start)
	if total < brokerSlowRequestThreshold {
		return
	}
	key := brokerSlowLogKey + ":" + t.endpoint
	defaultBrokerLimiter.observe(key, 0)
	log, suppressed := defaultBrokerLimiter.shouldLogExcess(key)
	if !log {
		return
	}
	attrs := []any{
		"endpoint", t.endpoint,
		"caller_pid", t.callerPID,
		"resolved_session", t.resolved,
		"total_ms", total.Milliseconds(),
		"last_phase", t.lastPhase,
		"suppressed_since_last_log", suppressed - 1,
	}
	for _, p := range t.phases {
		attrs = append(attrs, p.name+"_ms", p.d.Milliseconds())
	}
	if t.proof != nil {
		attrs = append(attrs,
			"proof_db_ms", t.proof.dbDur.Milliseconds(),
			"proof_pane_ms", t.proof.paneDur.Milliseconds(),
			"proof_walk_ms", t.proof.walkDur.Milliseconds(),
			"pane_source", t.proof.paneSource,
		)
	}
	attrs = append(attrs, "module", "hooks")
	slog.Warn("broker: slow brokered request; the caller may have given up waiting", attrs...)
}
