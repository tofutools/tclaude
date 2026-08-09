package agentd

import "sync"

// wakingConvs tracks conversations with a manual resume currently in flight.
// A wake is many seconds of real work (managed-server boot, sandbox
// construction, pane fork) during which the agent's row still reads plain
// "offline"; this registry lets every open dashboard render that interval as
// a distinct waking state instead of nothing. In-memory on purpose: an
// in-flight resume cannot outlive the daemon, so neither should the flag.
//
// Refcounted, not boolean: two resumes of the same conversation can overlap
// (a group bulk resume racing a dashboard click — both mark before one of
// them queues on the launch lock), and the first to finish must not unflag a
// wake that is still genuinely in flight.
var wakingConvs = struct {
	sync.Mutex
	inflight map[string]int
}{inflight: map[string]int{}}

// markConvWaking flags a conversation as waking and returns the matching
// clear. Call sites defer the clear so the flag exactly brackets the resume
// attempt, whatever its outcome — the next snapshot then shows either the
// online row or the offline row plus the failure toast the caller raised.
func markConvWaking(convID string) func() {
	wakingConvs.Lock()
	wakingConvs.inflight[convID]++
	wakingConvs.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			wakingConvs.Lock()
			if wakingConvs.inflight[convID]--; wakingConvs.inflight[convID] <= 0 {
				delete(wakingConvs.inflight, convID)
			}
			wakingConvs.Unlock()
		})
	}
}

func isConvWaking(convID string) bool {
	wakingConvs.Lock()
	defer wakingConvs.Unlock()
	return wakingConvs.inflight[convID] > 0
}
