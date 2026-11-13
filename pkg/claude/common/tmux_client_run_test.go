package common

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

func shortWaitDelay(t *testing.T) {
	t.Helper()
	prev := tmuxClientWaitDelay
	tmuxClientWaitDelay = 100 * time.Millisecond
	t.Cleanup(func() { tmuxClientWaitDelay = prev })
}

func TestRunTmuxClientReportsARejectionWithItsMessage(t *testing.T) {
	_, err := RunTmuxClient(context.Background(),
		exec.Command("/bin/sh", "-c", `echo "can't find session: x" >&2; echo second >&2; exit 1`))
	var rejected *TmuxCommandError
	if !errors.As(err, &rejected) {
		t.Fatalf("want a TmuxCommandError, got %v", err)
	}
	if got := rejected.Message(); got != "can't find session: x\nsecond" {
		t.Errorf("message = %q", got)
	}
	if got := err.Error(); got != "exit status 1: can't find session: x; second" {
		t.Errorf("error text = %q", got)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || !IsTmuxCommandFailure(err) {
		t.Errorf("the client's exit error must stay reachable: %v", err)
	}
}

// A client that exited cleanly has answered. Something it started that keeps
// its output pipes open must neither hold the caller nor turn the answer
// into a failure.
func TestRunTmuxClientSucceedsWhenAChildKeepsThePipesOpen(t *testing.T) {
	shortWaitDelay(t)
	start := time.Now()
	out, err := RunTmuxClient(context.Background(),
		exec.Command("/bin/sh", "-c", "echo answered; sleep 30 & exit 0"))
	if err != nil {
		t.Fatalf("a clean exit must not fail: %v", err)
	}
	if string(out) != "answered\n" {
		t.Errorf("stdout = %q", out)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("waited %s for a pipe held by a background child", elapsed)
	}
}

func TestRunTmuxClientStopsAndReapsWhenTheContextEnds(t *testing.T) {
	shortWaitDelay(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	why := errors.New("caller gave up")
	started := make(chan struct{})
	go func() {
		<-started
		cancel(why)
	}()
	cmd := exec.Command("/bin/sh", "-c", "echo wedged >&2; exec sleep 600")
	done := make(chan error, 1)
	go func() {
		// The command is started inside RunTmuxClient; cancelling once this
		// goroutine runs is enough, whichever side of Start it lands on.
		close(started)
		_, err := RunTmuxClient(ctx, cmd)
		done <- err
	}()
	select {
	case err := <-done:
		var aborted *TmuxAbortedError
		if !errors.As(err, &aborted) || !errors.Is(err, why) {
			t.Fatalf("want a TmuxAbortedError carrying the cause, got %v", err)
		}
		if cmd.ProcessState == nil {
			t.Error("the client was not reaped before returning")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a stopped client was not killed and reaped")
	}
}

// After a control attempt, the forked client gets the caller's whole budget
// again rather than what the attempt left of it.
func TestFreshBudgetRestoresTheWholeAllowance(t *testing.T) {
	started := time.Now()
	parent, cancelParent := context.WithDeadline(context.Background(), started.Add(5*time.Second))
	defer cancelParent()

	now := started.Add(4 * time.Second) // the control attempt used most of it
	ctx, cancel := freshBudget(parent, started, now)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok || !deadline.Equal(now.Add(5*time.Second)) {
		t.Errorf("deadline = %v, want five seconds after the fallback started", deadline)
	}

	// An expired parent does not end the fallback's own allowance.
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	ctx, cancel = freshBudget(expired, time.Now().Add(-6*time.Second), time.Now())
	defer cancel()
	if ctx.Err() != nil {
		t.Errorf("the fallback started already expired: %v", ctx.Err())
	}

	plain, cancel := freshBudget(context.Background(), started, now)
	defer cancel()
	if _, ok := plain.Deadline(); ok {
		t.Error("no deadline in, no deadline out")
	}
}
