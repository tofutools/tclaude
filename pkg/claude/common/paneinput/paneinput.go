// Package paneinput owns contention-safe programmatic input into harness panes.
//
// Agentd is not the only process that may need to type into a managed pane:
// hook callbacks and the legacy task runner are separate tclaude subprocesses.
// An in-process mutex therefore cannot prevent their multi-command tmux input
// sequences from interleaving. This package adds a per-pane advisory file lock
// around the shared text/paste + submit sequence. Advisory locks are released
// by the OS if a writer exits, so a crashed helper cannot poison a pane until
// the tmux server restarts.
package paneinput

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

// Runner executes one tmux command (without the leading tmux binary/socket
// arguments). Agentd supplies its timeout-bounded runner; other processes use
// DefaultRunner.
type Runner func(args ...string) error

// Options selects the command boundary and timing policy for one injection.
// Zero values use the production defaults.
type Options struct {
	Run            Runner
	SettleDelay    time.Duration
	SettleDelaySet bool
	LockTimeout    time.Duration
	LockRetry      time.Duration
	// LockID overrides the serialization identity the advisory file lock is
	// keyed on (the send target still routes the tmux commands). A caller
	// that types into an exact pane ID but knows the pane's session must pass
	// the session-shaped target here: pane-ID and session spellings of the
	// same pane otherwise hash to different lock files, so the two input
	// streams would not single-file. Empty = derive from the send target.
	LockID string
}

var ErrLockTimeout = errors.New("pane input lock timeout")

const (
	defaultSettleDelay = 500 * time.Millisecond
	defaultLockTimeout = time.Minute
	defaultLockRetry   = 10 * time.Millisecond
)

var pasteBufferSequence atomic.Uint64

// DefaultRunner executes against tclaude's tmux server.
func DefaultRunner(args ...string) error {
	return clcommon.TmuxCommand(args...).Run()
}

func (o Options) resolved() Options {
	if o.Run == nil {
		o.Run = DefaultRunner
	}
	if !o.SettleDelaySet {
		o.SettleDelay = defaultSettleDelay
	} else if o.SettleDelay < 0 {
		o.SettleDelay = 0
	}
	if o.LockTimeout <= 0 {
		o.LockTimeout = defaultLockTimeout
	}
	if o.LockRetry <= 0 {
		o.LockRetry = defaultLockRetry
	}
	return o
}

// WithLock single-files fn with every cooperating tclaude process targeting
// the same pane. fn receives the exact-match tmux pane target.
func WithLock(tmuxTarget string, opts Options, fn func(run Runner, exactTarget string) error) error {
	opts = opts.resolved()
	exactTarget := ExactInputTarget(tmuxTarget)
	lockTarget := exactTarget
	if opts.LockID != "" {
		lockTarget = ExactInputTarget(opts.LockID)
	}
	lockPath, err := paneLockPath(lockTarget)
	if err != nil {
		return err
	}
	fileLock := flock.New(lockPath)
	ctx, cancel := context.WithTimeout(context.Background(), opts.LockTimeout)
	defer cancel()
	locked, err := fileLock.TryLockContext(ctx, opts.LockRetry)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("%w after %s", ErrLockTimeout, opts.LockTimeout)
		}
		return fmt.Errorf("acquire pane input lock: %w", err)
	}
	if !locked {
		return fmt.Errorf("%w after %s", ErrLockTimeout, opts.LockTimeout)
	}
	defer func() { _ = fileLock.Unlock() }()
	return fn(opts.Run, exactTarget)
}

// ExactInputTarget canonicalizes an input target to its exact form: tmux
// pane IDs are already exact targets and pass through (prefixing one with
// the session-name exact marker produces the invalid target "=%N" in real
// tmux), while session-shaped targets get the marker to avoid tmux's
// unique-prefix fallback onto a namesake session. Exported so in-process
// serialization (agentd's pane-inject mutexes) can key on the very same
// canonical identity as this package's cross-process advisory file lock.
func ExactInputTarget(tmuxTarget string) string {
	target := strings.TrimPrefix(tmuxTarget, "=")
	if strings.HasPrefix(target, "%") {
		return target
	}
	return clcommon.ExactTarget(target)
}

// InjectTextAndSubmit delivers one complete prompt turn. Multiline/tabbed
// content uses tmux bracketed paste so its bytes remain one input submission;
// single-line content uses the smaller send-keys path. The two settled Enter
// presses preserve the long-standing paste-coalescing workaround.
func InjectTextAndSubmit(tmuxTarget, text string, opts Options) error {
	opts = opts.resolved()
	return WithLock(tmuxTarget, opts, func(run Runner, target string) error {
		if strings.ContainsAny(text, "\n\t") {
			buffer := fmt.Sprintf("tclaude-inject-%d-%d", os.Getpid(), pasteBufferSequence.Add(1))
			if err := run("set-buffer", "-b", buffer, text); err != nil {
				return fmt.Errorf("set-buffer text: %w", err)
			}
			if err := run("paste-buffer", "-d", "-p", "-r", "-b", buffer, "-t", target); err != nil {
				return fmt.Errorf("paste-buffer text: %w", err)
			}
		} else if err := run("send-keys", "-l", "-t", target, text); err != nil {
			return fmt.Errorf("send-keys text: %w", err)
		}
		time.Sleep(opts.SettleDelay)
		if err := run("send-keys", "-t", target, "Enter"); err != nil {
			return fmt.Errorf("send-keys submit: %w", err)
		}
		time.Sleep(opts.SettleDelay)
		_ = run("send-keys", "-t", target, "Enter")
		return nil
	})
}

// SendKeys serializes a single non-text tmux key operation (for example the
// Enter that accepts a plan dialog) with message and lifecycle input streams.
func SendKeys(tmuxTarget string, opts Options, keys ...string) error {
	return WithLock(tmuxTarget, opts, func(run Runner, target string) error {
		args := []string{"send-keys", "-t", target}
		args = append(args, keys...)
		return run(args...)
	})
}

func paneLockPath(tmuxTarget string) (string, error) {
	dataDir := tclcommon.TclaudeDataDir()
	if dataDir == "" {
		return "", errors.New("resolve tclaude data directory for pane input lock")
	}
	dir := filepath.Join(dataDir, "pane-input-locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create pane input lock directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("inspect pane input lock directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("pane input lock path is not a directory: %s", dir)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() {
		return "", fmt.Errorf("pane input lock directory is not owned by uid %d: %s", os.Getuid(), dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("secure pane input lock directory: %w", err)
	}
	sum := sha256.Sum256([]byte(tmuxTarget))
	return filepath.Join(dir, fmt.Sprintf("%x.lock", sum[:16])), nil
}
