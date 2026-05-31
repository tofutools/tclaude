package workflows

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/tofutools/tclaude/pkg/claude/ccworkflows"
)

// watch.go — `workflows show --watch`: poll a run and redraw its tree until it
// finishes or the user interrupts. The journal is the only live signal, so this
// is a poll (no event/hook); each frame reloads via the same LoadRun path the
// one-shot view uses, so a run that finishes mid-watch flips automatically from
// the journal view to the authoritative completed record.

const (
	defaultWatchInterval = 2 * time.Second
	minWatchInterval     = 500 * time.Millisecond // floor: never hammer the disk
)

// watchDeps are the injectable seams for the watch loop — real time/signals in
// production, deterministic stubs in tests.
type watchDeps struct {
	load     func(runID string) (*ccworkflows.RunState, *ccworkflows.RunRef, error)
	waitNext func() bool // block until the next frame is due; false ⇒ stop (interrupted)
	clear    bool        // emit screen-clear codes (TTY only)
}

// maxWatchLoadErrors is how many consecutive load failures the watch tolerates
// before giving up. A run finishing mid-watch can momentarily yield a
// half-written completed-run JSON (CC's write is not atomic), which would fail
// to parse on the tick that coincides with it — exactly at the finish line. So
// a transient error is non-fatal: note it and keep polling; only a sustained
// run of failures is a real problem.
const maxWatchLoadErrors = 5

// runShowWatch renders frames until the run reaches a terminal status or
// waitNext signals a stop. It returns a process exit code.
func runShowWatch(runID string, stdout, stderr io.Writer, d watchDeps) int {
	consecutiveErrs := 0
	for {
		rs, ref, err := d.load(runID)
		if err != nil {
			consecutiveErrs++
			if consecutiveErrs >= maxWatchLoadErrors {
				fmt.Fprintf(stderr, "Error: %v (gave up after %d consecutive read failures)\n", err, consecutiveErrs)
				return 1
			}
			if d.clear {
				fmt.Fprint(stdout, "\033[H\033[2J")
			}
			fmt.Fprintf(stdout, "⟳ watching %s — temporarily unreadable, retrying (%d/%d)…\n",
				runID, consecutiveErrs, maxWatchLoadErrors)
			if !d.waitNext() {
				fmt.Fprintln(stdout, "\n■ stopped watching.")
				return 0
			}
			continue
		}
		consecutiveErrs = 0

		if d.clear {
			fmt.Fprint(stdout, "\033[H\033[2J") // cursor home + clear screen
		}
		fmt.Fprintf(stdout, "⟳ watching %s — press Ctrl-C to stop\n\n", rs.RunID)
		writeRunTree(stdout, rs, ref)

		if isTerminalStatus(rs.Status) {
			fmt.Fprintf(stdout, "\n✓ run is no longer in flight (%s).\n", rs.Status)
			return 0
		}
		if !d.waitNext() {
			fmt.Fprintln(stdout, "\n■ stopped watching (run still in flight).")
			return 0
		}
	}
}

// isTerminalStatus reports whether a run has reached a final state and watching
// should stop. Unknown is treated as non-terminal (keep polling — it may just be
// a momentarily-unreadable record).
func isTerminalStatus(s ccworkflows.RunStatus) bool {
	return s == ccworkflows.RunCompleted || s == ccworkflows.RunFailed
}

// watchInterval clamps the requested interval (seconds) to the floor, applying
// the default when unset.
func watchInterval(seconds float64) time.Duration {
	if seconds <= 0 {
		return defaultWatchInterval
	}
	d := time.Duration(seconds * float64(time.Second))
	if d < minWatchInterval {
		return minWatchInterval
	}
	return d
}

// realWaitNext blocks for interval, returning false if an interrupt arrives
// first (so Ctrl-C stops the watch promptly instead of after the full sleep).
func realWaitNext(interval time.Duration, sigCh <-chan os.Signal) func() bool {
	return func() bool {
		select {
		case <-time.After(interval):
			return true
		case <-sigCh:
			return false
		}
	}
}

// isTerminalWriter reports whether w is an interactive terminal, so the watch
// loop only emits screen-clear codes when they won't garble a pipe / file.
func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// startWatch wires the production dependencies and runs the watch loop.
func startWatch(params *ShowParams, stdout, stderr io.Writer) int {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	return runShowWatch(params.RunID, stdout, stderr, watchDeps{
		load:     ccworkflows.FindRun,
		waitNext: realWaitNext(watchInterval(params.Interval), sigCh),
		clear:    isTerminalWriter(stdout),
	})
}
