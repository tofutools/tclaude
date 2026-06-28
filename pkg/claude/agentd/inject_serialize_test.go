package agentd

import (
	"os/exec"
	"sync"
	"testing"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// recordingTmux is a clcommon.Tmux fake that records the final argument
// of every send-keys call in order, and sleeps on the non-Enter "text"
// send-keys to widen the window in which two concurrent injectors could
// interleave. The returned command runs `true`, a real no-op, so
// injectTextAndSubmit's Run() error checks pass.
type recordingTmux struct {
	mu        sync.Mutex
	keys      []string
	textDelay time.Duration
}

func (r *recordingTmux) Command(args ...string) *exec.Cmd {
	last := args[len(args)-1]
	r.mu.Lock()
	r.keys = append(r.keys, last)
	delay := r.textDelay
	r.mu.Unlock()
	// The "text" send-keys is the one that, unserialized, lets a second
	// injector slip its own text in before this sequence's Enter. Sleeping
	// here makes that interleave deterministic when the lock is missing.
	if delay > 0 && last != "Enter" {
		time.Sleep(delay)
	}
	return exec.Command("true")
}

func (r *recordingTmux) ListSessions() (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

func (r *recordingTmux) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.keys...)
}

// TestInjectTextAndSubmit_SerializesPerPane is the JOH-310 regression
// guard: two injectors racing on the SAME pane must single-file, not
// interleave their send-keys. Without the per-pane lock the overlapping
// textDelay windows record both texts first (text, text, Enter×4),
// proving the garbled-prompt hazard; with it, each injector's
// [text, Enter, Enter] triple stays contiguous.
func TestInjectTextAndSubmit_SerializesPerPane(t *testing.T) {
	defer SetInjectSettleDelayForTest(0)()
	rt := &recordingTmux{textDelay: 10 * time.Millisecond}
	prev := clcommon.Default
	clcommon.Default = rt
	t.Cleanup(func() { clcommon.Default = prev })

	const target = "tclaude-joh310-same:0.0"
	var wg sync.WaitGroup
	for _, m := range []string{"MSG-A", "MSG-B"} {
		wg.Add(1)
		go func(text string) {
			defer wg.Done()
			if err := injectTextAndSubmit(target, text); err != nil {
				t.Errorf("injectTextAndSubmit(%q): %v", text, err)
			}
		}(m)
	}
	wg.Wait()

	got := rt.snapshot()
	// Each injectTextAndSubmit emits exactly [text, Enter, Enter].
	if len(got) != 6 {
		t.Fatalf("expected 6 send-keys, got %d: %v", len(got), got)
	}
	isText := func(s string) bool { return s == "MSG-A" || s == "MSG-B" }
	if !isText(got[0]) || got[1] != "Enter" || got[2] != "Enter" ||
		!isText(got[3]) || got[4] != "Enter" || got[5] != "Enter" {
		t.Fatalf("send-keys interleaved across concurrent injectors: %v", got)
	}
	if got[0] == got[3] {
		t.Fatalf("same message injected twice, the other was lost: %v", got)
	}
}

// TestPaneInjectLock_PerTargetIdentity pins the keying: one mutex per
// pane target (so two agents are NOT serialized against each other), and
// the same mutex returned for repeat lookups of one target.
func TestPaneInjectLock_PerTargetIdentity(t *testing.T) {
	a1 := paneInjectLock("pane-a:0.0")
	a2 := paneInjectLock("pane-a:0.0")
	b := paneInjectLock("pane-b:0.0")
	if a1 != a2 {
		t.Fatal("same target must return the same mutex")
	}
	if a1 == b {
		t.Fatal("different targets must return different mutexes (per-pane, not a single global lock)")
	}
}

// barrierTmux invokes onText on each non-Enter send-keys (the single
// "text" step of injectTextAndSubmit). Used to prove DIFFERENT panes are
// not serialized against each other.
type barrierTmux struct {
	onText func()
}

func (b *barrierTmux) Command(args ...string) *exec.Cmd {
	if b.onText != nil && args[len(args)-1] != "Enter" {
		b.onText()
	}
	return exec.Command("true")
}

func (b *barrierTmux) ListSessions() (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

// TestInjectTextAndSubmit_DifferentPanesConcurrent guards against the
// over-correction of using one global lock instead of a per-pane lock:
// injectors on different panes must run concurrently. Both must reach
// their (locked) text send-keys before either is released — a global
// lock would wedge the second one on Lock() and the rendezvous would
// time out.
func TestInjectTextAndSubmit_DifferentPanesConcurrent(t *testing.T) {
	defer SetInjectSettleDelayForTest(0)()
	const n = 2
	arrived := make(chan struct{}, n)
	proceed := make(chan struct{})
	bt := &barrierTmux{onText: func() {
		arrived <- struct{}{}
		<-proceed
	}}
	prev := clcommon.Default
	clcommon.Default = bt
	t.Cleanup(func() { clcommon.Default = prev })

	var wg sync.WaitGroup
	for _, target := range []string{"pane-x:0.0", "pane-y:0.0"} {
		wg.Add(1)
		go func(tgt string) {
			defer wg.Done()
			_ = injectTextAndSubmit(tgt, "hi")
		}(target)
	}

	timeout := time.After(2 * time.Second)
	for got := range n {
		select {
		case <-arrived:
		case <-timeout:
			t.Fatalf("only %d/%d injectors reached send-keys; the lock is global, not per-pane", got, n)
		}
	}
	close(proceed)
	wg.Wait()
}
