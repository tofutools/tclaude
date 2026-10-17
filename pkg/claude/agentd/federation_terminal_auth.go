package agentd

import (
	"sync"
	"sync/atomic"
	"time"
)

// fedAuthMaxAge bounds how stale a terminal stream's cached authorization may
// be when a frame relies on it. Each stream's 1s ticker refreshes the verdict
// well inside this, so the data path normally never runs the check itself.
const fedAuthMaxAge = 1500 * time.Millisecond

// fedAuthGate caches a terminal stream's authorization verdict. The full
// check costs several tmux execs and database reads; running it per output
// chunk and per keystroke capped remote terminals at a handful of frames a
// second and added that latency to every key. Revocation is still seen within
// fedAuthMaxAge (normally by the ticker's refresh, within ~1s), and a refusal
// is sticky: once refused, the gate never passes again.
type fedAuthGate struct {
	// check returns "" when authorized, else the refusal reason.
	check   func() string
	checkMu sync.Mutex
	verdict atomic.Pointer[fedAuthVerdict]
}

type fedAuthVerdict struct {
	reason string
	at     time.Time
}

func newFedAuthGate(check func() string) *fedAuthGate { return &fedAuthGate{check: check} }

// newFedAuthGateBool adapts a boolean check; its refusal reason is "revoked".
func newFedAuthGateBool(check func() bool) *fedAuthGate {
	return newFedAuthGate(func() string {
		if check() {
			return ""
		}
		return "revoked"
	})
}

// allowed returns the cached verdict, re-checking only when it is too old.
func (g *fedAuthGate) allowed() string {
	if v := g.verdict.Load(); v != nil && (v.reason != "" || time.Since(v.at) < fedAuthMaxAge) {
		return v.reason
	}
	return g.refresh()
}

// refresh runs the full check now, unless the gate already refused.
func (g *fedAuthGate) refresh() string {
	g.checkMu.Lock()
	defer g.checkMu.Unlock()
	if v := g.verdict.Load(); v != nil && v.reason != "" {
		return v.reason
	}
	reason := g.check()
	g.verdict.Store(&fedAuthVerdict{reason: reason, at: time.Now()})
	return reason
}
