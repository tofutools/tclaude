package session

// Downward process-tree enumeration.
//
// The existing /proc helpers in process_unix.go all walk UPWARD (a hook
// callback asking "which harness am I running under"). Tracking background
// shell commands needs the opposite direction: given an agent's recorded
// pid, which processes are running BELOW it right now.
//
// This is the ground truth behind the dashboard's "⚙+N" badge. Claude Code
// exposes no PID for a background task anywhere — not in the hook payload,
// the tool result, or the transcript — and fires no hook when one exits, so
// matching a ledger entry's recorded command against the live descendant
// set is the only way to tell a running background shell from a finished
// one. See db.BgShellSet.
//
// The shape being matched was verified empirically on Linux (TCL-613). A
// background `Bash` launched by a sandboxed Claude Code session runs as:
//
//	claude                                  ← the harness
//	└── bwrap …                             ← sandbox wrapper
//	    └── bwrap …
//	        └── /bin/bash -c '… eval '\''<the command>'\'' …'
//	            └── <the command's own children, e.g. sleep 300>
//
// Unsandboxed, the bwrap hops are absent and the wrapper shell is a direct
// child. Two properties hold either way and are what the matcher relies on:
// the wrapper shell's own argv CONTAINS the command string the hook
// recorded, and it lives for as long as the background task does. Nothing
// here assumes a fixed depth — the walk is fully recursive — because the
// wrapper's distance below the harness varies with the sandbox posture, and
// the recorded pid may be either the harness itself or the pane shell one
// hop above it.

// maxProcTreeNodes bounds a single descendant walk. A pathological or
// corrupted parent map (a cycle the visited-set somehow misses, a fork
// bomb) must not turn a dashboard poll into an unbounded scan. Far above
// any real agent subtree.
const maxProcTreeNodes = 4096

// procTable is one snapshot of the host's process list: every visible
// process's parent, plus a way to read a process's full argv.
type procTable struct {
	// parent maps pid → ppid for every process visible to this user.
	parent map[int]int
	// cmdline returns pid's full argv joined by spaces, or "" when it is
	// unreadable (the process exited between the snapshot and the read, or
	// belongs to another user).
	cmdline func(pid int) string
}

// DescendantCommandLines returns the full argv of every process running
// below rootPID — recursively, excluding rootPID itself.
//
// The bool reports whether the host's process table could be read at all.
// That distinction is load-bearing for the caller: an empty slice with
// ok=true is positive evidence that nothing is running below the agent (so
// ledger entries can be retired), while ok=false means "cannot tell" and
// must leave the ledger alone for its TTL to handle. Conflating the two
// would silently disable the badge on any host where enumeration is
// unavailable — or, worse, retire every entry there.
//
// A rootPID that is not alive reports ok=false for the same reason: an
// agent whose recorded pid is stale or zero yields no information about
// its background shells, and must not be read as "none running".
func DescendantCommandLines(rootPID int) ([]string, bool) {
	if rootPID <= 0 || !IsProcessAlive(rootPID) {
		return nil, false
	}
	table, ok := readProcTable()
	if !ok {
		return nil, false
	}
	children := make(map[int][]int, len(table.parent))
	for pid, ppid := range table.parent {
		if pid != ppid {
			children[ppid] = append(children[ppid], pid)
		}
	}
	var out []string
	visited := map[int]struct{}{rootPID: {}}
	queue := append([]int(nil), children[rootPID]...)
	for len(queue) > 0 && len(visited) < maxProcTreeNodes {
		pid := queue[0]
		queue = queue[1:]
		if _, seen := visited[pid]; seen {
			continue
		}
		visited[pid] = struct{}{}
		if cmd := table.cmdline(pid); cmd != "" {
			out = append(out, cmd)
		}
		queue = append(queue, children[pid]...)
	}
	return out, true
}
