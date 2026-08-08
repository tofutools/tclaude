//go:build !linux && !darwin

package portowner

// ProcessOwnsLoopbackPort has no implementation on this platform and therefore
// reports "not owned" for everything. That is the safe answer, not a stub to be
// filled in opportunistically: a caller uses this to decide whether talking to
// an unauthenticated local endpoint is safe, and a platform where tclaude
// cannot prove the answer must not be told the proof succeeded.
func ProcessOwnsLoopbackPort(_, _ int) bool { return false }

// HasLoopbackListener has no implementation on this platform. It reports "no
// listener", which is only a diagnostic hint and never a trust decision.
func HasLoopbackListener(_ int) bool { return false }

// ProcessInSubtree fails closed for the same reason.
func ProcessInSubtree(_, _ int) bool { return false }

// ProcessSubtree reports only the root itself. Callers use it for bookkeeping
// bounds rather than for a trust decision.
func ProcessSubtree(rootPID int) []int {
	if rootPID <= 1 {
		return nil
	}
	return []int{rootPID}
}
