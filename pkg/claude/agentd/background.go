package agentd

import "sync"

// bgWG tracks fire-and-forget goroutines launched by post-init paths
// (spawn rename+welcome, clone rename, etc.) that touch shared state
// — sqlite, tmux, the agent_messages table — long after the HTTP
// handler that launched them has returned.
//
// Production never drains it; the daemon runs forever. The WG exists
// so flow tests can `bgWG.Wait()` in cleanup and guarantee no
// orphaned goroutine is still scribbling into the per-test $HOME/
// .tclaude/ when t.TempDir's RemoveAll fires, or racing the next
// test's db.ResetForTest inside db.Open's sync.Once.
var bgWG sync.WaitGroup

// goBackground replaces a bare `go f()` for post-init work that
// outlives its HTTP handler. Wraps the launch with WG bookkeeping so
// WaitForBackgroundForTest can drain.
func goBackground(f func()) {
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		f()
	}()
}
