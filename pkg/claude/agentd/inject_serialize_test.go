package agentd

import (
	"errors"
	"os/exec"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// recordingTmux is a clcommon.Tmux fake that records the final argument
// of every send-keys call in order. It can hold the first text command at
// a barrier so a test can start a competing injector while the first still
// owns the pane lock. The returned command runs `true`, a real no-op, so
// injectTextAndSubmit's Run() error checks pass.
type recordingTmux struct {
	mu               sync.Mutex
	keys             []string
	firstTextSeen    bool
	firstTextStarted chan struct{}
	releaseFirstText chan struct{}
}

type commandRecordingTmux struct {
	mu       sync.Mutex
	commands [][]string
}

func (r *commandRecordingTmux) Command(args ...string) *exec.Cmd {
	r.mu.Lock()
	r.commands = append(r.commands, append([]string(nil), args...))
	r.mu.Unlock()
	return exec.Command("true")
}

func (r *commandRecordingTmux) ListSessions() (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

func (r *commandRecordingTmux) snapshot() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.commands))
	for i := range r.commands {
		out[i] = append([]string(nil), r.commands[i]...)
	}
	return out
}

func TestInjectTextAndSubmit_MultilineUsesBracketedPaste(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Cleanup(SetInjectSettleDelayForTest(0))
	rt := &commandRecordingTmux{}
	prev := clcommon.Default
	clcommon.Default = rt
	t.Cleanup(func() { clcommon.Default = prev })

	const text = "first line\n\tsecond line"
	require.NoError(t, injectTextAndSubmit("pane-multiline:0.0", text))
	commands := rt.snapshot()
	require.Len(t, commands, 4)
	assert.Equal(t, "set-buffer", commands[0][0])
	assert.Equal(t, text, commands[0][len(commands[0])-1])
	assert.Equal(t, "paste-buffer", commands[1][0])
	assert.True(t, slices.Contains(commands[1], "-p"), "paste must enable bracketed-paste mode: %v", commands[1])
	assert.True(t, slices.Contains(commands[1], "-r"), "paste must preserve LF bytes: %v", commands[1])
	assert.Equal(t, []string{"send-keys", "-t", "=pane-multiline:0.0", "Enter"}, commands[2])
	assert.Equal(t, commands[2], commands[3])
}

func (r *recordingTmux) Command(args ...string) *exec.Cmd {
	last := args[len(args)-1]
	r.mu.Lock()
	r.keys = append(r.keys, last)
	blockFirstText := last != "Enter" && !r.firstTextSeen
	if blockFirstText {
		r.firstTextSeen = true
	}
	r.mu.Unlock()
	if blockFirstText && r.firstTextStarted != nil {
		close(r.firstTextStarted)
		<-r.releaseFirstText
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
// interleave their send-keys. The first injector is held after recording its
// text command; only after the second is known to be waiting on the pane lock
// is the first released. Each injector's [text, Enter, Enter] triple must stay
// contiguous.
func TestInjectTextAndSubmit_SerializesPerPane(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Cleanup(SetInjectSettleDelayForTest(0))
	firstTextStarted := make(chan struct{})
	releaseFirstText := make(chan struct{})
	var releaseOnce sync.Once
	releaseFirst := func() { releaseOnce.Do(func() { close(releaseFirstText) }) }
	rt := &recordingTmux{
		firstTextStarted: firstTextStarted,
		releaseFirstText: releaseFirstText,
	}
	prev := clcommon.Default
	clcommon.Default = rt
	contended := make(chan struct{}, 1)
	previousHook := paneInjectLockContendedHook
	paneInjectLockContendedHook = func() { contended <- struct{}{} }

	const target = "tclaude-joh310-same:0.0"
	var wg sync.WaitGroup
	t.Cleanup(func() {
		// Release and join the injectors before restoring package globals so a
		// failure cannot send the remaining fake commands through real tmux.
		releaseFirst()
		wg.Wait()
		paneInjectLockContendedHook = previousHook
		clcommon.Default = prev
	})
	start := func(message string) {
		wg.Add(1)
		go func(text string) {
			defer wg.Done()
			if err := injectTextAndSubmit(target, text); err != nil {
				t.Errorf("injectTextAndSubmit(%q): %v", text, err)
			}
		}(message)
	}
	start("MSG-A")
	select {
	case <-firstTextStarted:
	case <-time.After(time.Second):
		t.Fatal("first injector did not reach the text-command barrier")
	}
	start("MSG-B")
	select {
	case <-contended:
	case <-time.After(time.Second):
		t.Fatal("second injector did not contend on the pane lock")
	}
	if got := rt.snapshot(); len(got) != 1 || got[0] != "MSG-A" {
		t.Fatalf("second injector emitted a command while the first held the pane lock: %v", got)
	}
	releaseFirst()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("serialized injectors did not finish after releasing the barrier")
	}

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

func TestInjectTextAndSubmit_TimesOutWaitingForPaneLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const target = "pane-held:0.0"
		mu := paneInjectLock(target)
		mu.Lock()
		t.Cleanup(mu.Unlock)

		previous := paneInjectLockTimeout
		paneInjectLockTimeout = 10 * time.Millisecond
		t.Cleanup(func() { paneInjectLockTimeout = previous })

		started := time.Now()
		err := injectTextAndSubmit(target, "must-not-send")
		if !errors.Is(err, errPaneInjectLockTimeout) {
			t.Fatalf("expected pane lock timeout, got %v", err)
		}
		if elapsed := time.Since(started); elapsed != paneInjectLockTimeout {
			t.Fatalf("pane lock wait = %s, want %s", elapsed, paneInjectLockTimeout)
		}
	})
}

func TestAcquirePaneInjectLock_WaitsForRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		mu.Lock()

		previous := paneInjectLockTimeout
		paneInjectLockTimeout = time.Second
		t.Cleanup(func() { paneInjectLockTimeout = previous })

		done := make(chan error, 1)
		go func() {
			done <- acquirePaneInjectLock(&mu)
		}()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("lock acquisition returned before release: %v", err)
		default:
		}

		mu.Unlock()
		if err := <-done; err != nil {
			t.Fatalf("acquire released pane lock: %v", err)
		}
		mu.Unlock()
	})
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
	t.Setenv("HOME", t.TempDir())
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
