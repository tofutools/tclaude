package paneinput

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestInjectTextAndSubmitSerializesIndependentCallers(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondRan := make(chan struct{}, 1)
	var once sync.Once

	firstRun := func(args ...string) error {
		if args[0] == "send-keys" && args[len(args)-1] == "first" {
			once.Do(func() { close(firstStarted) })
			<-releaseFirst
		}
		return nil
	}
	secondRun := func(args ...string) error {
		if args[0] == "send-keys" && args[len(args)-1] == "second" {
			secondRan <- struct{}{}
		}
		return nil
	}
	opts := func(run Runner) Options {
		return Options{
			Run: run, SettleDelay: 0, SettleDelaySet: true,
			LockTimeout: time.Second, LockRetry: time.Millisecond,
		}
	}

	firstDone := make(chan error, 1)
	go func() { firstDone <- InjectTextAndSubmit("pane-serialize:0.0", "first", opts(firstRun)) }()
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first caller did not acquire the pane lock")
	}
	secondDone := make(chan error, 1)
	// The exact-match prefix is presentation, not identity: callers using the
	// raw and already-exact spellings must still contend for one pane lock.
	go func() { secondDone <- InjectTextAndSubmit("=pane-serialize:0.0", "second", opts(secondRun)) }()
	select {
	case <-secondRan:
		t.Fatal("second caller wrote while first caller held the pane lock")
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseFirst)
	require.NoError(t, <-firstDone)
	require.NoError(t, <-secondDone)
	select {
	case <-secondRan:
	default:
		t.Fatal("second caller never wrote after the lock was released")
	}
}
