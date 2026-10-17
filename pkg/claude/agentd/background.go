package agentd

import "sync"

// bg tracks fire-and-forget goroutines launched by post-init paths
// (spawn rename+welcome, clone rename, etc.) that touch shared state
// — sqlite, tmux, the agent_messages table — long after the HTTP
// handler that launched them has returned.
//
// Production never drains it; the daemon runs forever. It exists so
// flow tests can drain it in cleanup and guarantee no orphaned
// goroutine is still scribbling into the per-test $HOME/.tclaude/ when
// t.TempDir's RemoveAll fires, or racing the next test's
// db.ResetForTest inside db.Open's sync.Once.
//
// Not a sync.WaitGroup: a background goroutine may launch another (or a
// still-running handler may) while a drain is waking up, and a WaitGroup
// panics on an Add that races a returning Wait ("WaitGroup is reused
// before previous Wait has returned"). A counter under a cond simply
// keeps waiting.
var bg = struct {
	mu   sync.Mutex
	cond *sync.Cond
	n    int
}{}

func init() { bg.cond = sync.NewCond(&bg.mu) }

// goBackground replaces a bare `go f()` for post-init work that
// outlives its HTTP handler. Wraps the launch with the bookkeeping
// WaitForBackgroundForTest drains.
func goBackground(f func()) {
	bg.mu.Lock()
	bg.n++
	bg.mu.Unlock()
	go func() {
		defer func() {
			bg.mu.Lock()
			bg.n--
			if bg.n == 0 {
				bg.cond.Broadcast()
			}
			bg.mu.Unlock()
		}()
		f()
	}()
}

// waitBackground blocks until no goBackground goroutine is running.
func waitBackground() {
	bg.mu.Lock()
	for bg.n > 0 {
		bg.cond.Wait()
	}
	bg.mu.Unlock()
}
