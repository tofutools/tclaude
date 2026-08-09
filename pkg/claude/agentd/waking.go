package agentd

import "sync"

// wakingConvs tracks conversations with a manual resume currently in flight.
// A wake is many seconds of real work (managed-server boot, sandbox
// construction, pane fork) during which the agent's row still reads plain
// "offline"; this registry lets every open dashboard render that interval as
// a distinct waking state instead of nothing. In-memory on purpose: an
// in-flight resume cannot outlive the daemon, so neither should the flag.
var wakingConvs sync.Map

// markConvWaking flags a conversation as waking and returns the matching
// clear. Call sites defer the clear so the flag exactly brackets the resume
// attempt, whatever its outcome — the next snapshot then shows either the
// online row or the offline row plus the failure toast the caller raised.
func markConvWaking(convID string) func() {
	wakingConvs.Store(convID, struct{}{})
	return func() { wakingConvs.Delete(convID) }
}

func isConvWaking(convID string) bool {
	_, ok := wakingConvs.Load(convID)
	return ok
}
