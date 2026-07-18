package paneinput

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestInjectTextAndSubmitSerializesIndependentCallers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var once sync.Once

	firstRun := func(args ...string) error {
		if args[0] == "send-keys" && args[len(args)-1] == "first" {
			once.Do(func() { close(firstStarted) })
			<-releaseFirst
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
	// The exact-match prefix is presentation, not identity: callers using the
	// raw and already-exact spellings must still contend for one pane lock.
	secondCalled := false
	secondOpts := opts(func(args ...string) error {
		secondCalled = true
		return nil
	})
	secondOpts.LockTimeout = 10 * time.Millisecond
	err := InjectTextAndSubmit("=pane-serialize:0.0", "second", secondOpts)
	require.ErrorIs(t, err, ErrLockTimeout)
	require.False(t, secondCalled, "second caller wrote while first caller held the pane lock")

	close(releaseFirst)
	require.NoError(t, <-firstDone)
	require.NoError(t, InjectTextAndSubmit("=pane-serialize:0.0", "second", opts(func(args ...string) error {
		secondCalled = true
		return nil
	})))
	require.True(t, secondCalled, "second caller never wrote after the lock was released")
}

func TestInjectTextAndSubmitUsesLiteralModeForSingleLineText(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var commands [][]string
	err := InjectTextAndSubmit("pane-literal:0.0", "Enter", Options{
		Run: func(args ...string) error {
			commands = append(commands, append([]string(nil), args...))
			return nil
		},
		SettleDelay: 0, SettleDelaySet: true,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"send-keys", "-l", "-t", "=pane-literal:0.0", "Enter"}, commands[0])
}

func TestInjectTextAndSubmitPreservesExactPaneID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var commands [][]string
	err := InjectTextAndSubmit("%42", "/exit", Options{
		Run: func(args ...string) error {
			commands = append(commands, append([]string(nil), args...))
			return nil
		},
		SettleDelay: 0, SettleDelaySet: true,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"send-keys", "-l", "-t", "%42", "/exit"}, commands[0])
}

func TestPaneLockPathUsesPrivateDataDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := paneLockPath("=pane-private:0.0")
	require.NoError(t, err)
	dir := filepath.Dir(path)
	require.Equal(t, filepath.Join(home, ".tclaude", "data", "pane-input-locks"), dir)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	require.NoError(t, os.RemoveAll(dir))
	require.NoError(t, os.Symlink(t.TempDir(), dir))
	_, err = paneLockPath("=pane-private:0.0")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a directory")
}
