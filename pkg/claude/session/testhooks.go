package session

import (
	"time"
)

// SetRotateAgentConvForTest swaps the package-private function variable
// that fronts db.RotateAgentConv so flow tests can inject a transient
// failure and assert the post-/clear retry path: a failed rotation must
// leave the session row on the old conv-id and the predicate must stay
// true, then the next hook converges. Returns a restore closure intended
// for t.Cleanup.
//
// Lives outside *_test.go so it is reachable from flow tests in
// other packages (agentd_test, etc.) — _test.go files only export
// to the same package's test binary. The "ForTest" suffix is the
// contract that production code must not call this; the agentd
// package mirrors the same convention (BuildHandlerForTest etc.).
func SetRotateAgentConvForTest(fn func(oldConv, newConv, reason string) (string, bool, error)) func() {
	prev := rotateAgentConv
	rotateAgentConv = fn
	return func() { rotateAgentConv = prev }
}

// SetClearInjectTimingsForTest shrinks the readiness-poll knobs the
// /clear title-restore injection uses (clearInjectAliveTimeout +
// clearInjectReadyDelay) so flow tests don't sit on the 1s
// production ready-delay. Returns a restore closure for t.Cleanup.
// Same cross-package visibility constraint as
// SetRotateAgentConvForTest — lives in a regular .go file so
// agentd_test can call it.
func SetClearInjectTimingsForTest(aliveTimeout, readyDelay time.Duration) func() {
	prevAlive := clearInjectAliveTimeout
	prevDelay := clearInjectReadyDelay
	clearInjectAliveTimeout = aliveTimeout
	clearInjectReadyDelay = readyDelay
	return func() {
		clearInjectAliveTimeout = prevAlive
		clearInjectReadyDelay = prevDelay
	}
}
