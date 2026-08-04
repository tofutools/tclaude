package agentd

import "sync"

// seanceClaimant is the synthetic claimant id a live séance contributes to a
// claim snapshot's dirClaims. It is deliberately not a conv-id: the directory a
// séance runs in belongs to a PREDECESSOR generation, so registering the claim
// under that conversation's own id would let a retirement of that very
// conversation exclude it (resolve's `excluding`, retireWorktreeDrift's
// `claimant != convID`) and delete the directory anyway — the exact case this
// claim exists to stop. A non-conv id matches no exclusion and therefore always
// blocks, which is the correct answer: a live process is in there regardless of
// whose retirement is asking.
const seanceClaimant = "seance:live"

// seanceSharedNote phrases the operator-facing "kept" reason for a séance hold.
// The default note says "shared with another agent", which would send the
// operator looking for a peer that does not exist — the holder is a daemon-owned
// subprocess with no pane and no conversation of its own.
const seanceSharedNote = "a séance is running in it"

// seanceWorktreeClaims records the directories in-flight séance subprocesses are
// running in, so worktree cleanup can see them.
//
// In-memory, not durable, and that is a correctness choice rather than a
// convenience one. A séance is a synchronous child of this daemon
// (handleWhoamiSeanceRun holds a seanceSlots slot for the request's duration),
// so a durable row would outlive a crashed daemon with no process behind it and
// pin the worktree forever — a claim with no process. The residual of the
// in-memory form is the opposite polarity, and is accepted rather than
// impossible: see holdSeanceWorktree.
//
// Refcounted because seanceConcurrency admits more than one séance at a time and
// two of them can legitimately target the same directory.
var seanceWorktreeClaims = struct {
	mu   sync.Mutex
	dirs map[string]int
}{dirs: map[string]int{}}

// holdSeanceWorktree claims dir for a séance about to run in it and returns the
// release. Call it BEFORE the subprocess starts — a claim registered after the
// exec would reintroduce the very window this closes — and release it with a
// defer so panics and every error path unwind through it.
//
// Accepted residual: the claim does not survive this process dying. The séance
// child is NOT killed by the kernel when the daemon dies — executil gives it its
// own process group (Setpgid, which means even a group kill aimed at the
// daemon's pgid misses it), there is no Pdeathsig on the path, and the group
// kill is enforced only by (*executil.Cmd).watch, a goroutine that dies with the
// daemon. So SIGKILL, an OOM kill, or os.Exit orphans a live harness in dir and
// a restarted daemon will not know about it — a process with no claim, which is
// the polarity that deletes a live agent's working directory rather than the one
// that merely pins a worktree. A handler panic is NOT in that set: net/http
// recovers it, the request context cancels, and both the group kill and this
// release run normally.
//
// Do NOT assume maxSeanceTimeout still bounds an orphan. That deadline is
// enforced by (*executil.Cmd).watch, the same goroutine the daemon takes with
// it, so the orphan is precisely the case where the timeout stops applying.
// What actually limits it is stdio: cmd.Stdout is a plain io.Writer, so os/exec
// wires it through a pipe whose read end closes with the daemon, and the child
// dies of EPIPE/SIGPIPE on its next write. That is prompt for a harness that is
// producing output and NOT a bound at all for one blocked before its first
// write — a stalled API call has nothing to stop it.
//
// So the exposure needs the daemon killed, then restarted, then an operator
// retiring the predecessor WITH worktree deletion, while that orphan is still
// alive — usually seconds, unbounded in the stalled case. The orphaned child
// itself is a pre-existing leak that TCL-1026 does not own.
func holdSeanceWorktree(dir string) func() {
	dir = cleanClaimDir(dir)
	if dir == "" {
		return func() {}
	}
	seanceWorktreeClaims.mu.Lock()
	seanceWorktreeClaims.dirs[dir]++
	seanceWorktreeClaims.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			seanceWorktreeClaims.mu.Lock()
			defer seanceWorktreeClaims.mu.Unlock()
			seanceWorktreeClaims.dirs[dir]--
			if seanceWorktreeClaims.dirs[dir] <= 0 {
				delete(seanceWorktreeClaims.dirs, dir)
			}
		})
	}
}

// heldSeanceWorktrees returns the directories live séances are running in. The
// result is a copy: callers iterate it while other séances start and finish.
func heldSeanceWorktrees() []string {
	seanceWorktreeClaims.mu.Lock()
	defer seanceWorktreeClaims.mu.Unlock()
	dirs := make([]string, 0, len(seanceWorktreeClaims.dirs))
	for dir := range seanceWorktreeClaims.dirs {
		dirs = append(dirs, dir)
	}
	return dirs
}
