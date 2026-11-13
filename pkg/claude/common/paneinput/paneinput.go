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
	"github.com/tofutools/tclaude/pkg/claude/common/tmuxcmd"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

// Input is the channel a WithLock callback types into its pane through. Every
// send re-checks that the pane is not waiting on a human (see Options), so a
// dialog raised in the middle of a sequence stops the rest of it.
type Input struct {
	tmux  tmuxcmd.Client
	pane  tmuxcmd.PaneTarget
	guard func() error
}

// Keys presses keys by tmux key name ("Enter", "C-c"). A press of only C-c
// and Escape is always allowed: cancelling cannot accept a dialog's default,
// and stop/interrupt must stay usable while a human is being asked.
func (in Input) Keys(keys ...string) error {
	if !cancelOnlyKeys(keys) {
		if err := in.guard(); err != nil {
			return err
		}
	}
	return in.tmux.SendKeys(in.pane, keys...)
}

// Literal types text exactly as written.
func (in Input) Literal(text string) error {
	if err := in.guard(); err != nil {
		return err
	}
	return in.tmux.SendLiteral(in.pane, text)
}

// Paste delivers text as one bracketed paste.
func (in Input) Paste(text string) error {
	buffer := fmt.Sprintf("tclaude-inject-%d-%d", os.Getpid(), pasteBufferSequence.Add(1))
	return in.tmux.PasteText(in.pane, buffer, text, in.guard)
}

// Options selects the command boundary and timing policy for one injection.
// Zero values use the production defaults.
type Options struct {
	// AnswerHumanInput is reserved for an explicit operator-authorized dialog
	// answer (agentd auto-permit). Lifecycle/message injection never sets it.
	AnswerHumanInput bool
	// CapturePane optionally checks a known modal UI before an async hook lands.
	CapturePane func(target string) (string, error)
	// Tmux is the client the input is sent through. Agentd supplies its
	// timeout-bounded client; the zero value is the local tmux server with no
	// deadline, which is what other processes use.
	Tmux           tmuxcmd.Client
	SettleDelay    time.Duration
	SettleDelaySet bool
	LockTimeout    time.Duration
	LockRetry      time.Duration
	// ForceBracketedPaste keeps even single-line text in one terminal paste
	// event. This is useful when the foreground application must distinguish
	// programmatic injection from ordinary key presses.
	ForceBracketedPaste bool
	// PrefixKeys are tmux key names sent, in order, before the text — each
	// as its own send-keys, with one settle gap after the last. They exist
	// for a foreground application that must be moved into a state where it
	// accepts the text at all (Copilot's soft exit sends C-c first, because
	// its TUI silently discards a slash command typed mid-turn). Empty for
	// every ordinary injection.
	PrefixKeys []string

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

func (o Options) resolved() Options {
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
// the same pane. fn types through the Input it is handed.
func WithLock(tmuxTarget string, opts Options, fn func(in Input) error) error {
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
	guard := func() error {
		if opts.AnswerHumanInput {
			return nil
		}
		if err := CheckHumanInput(lockTarget); err != nil {
			return err
		}
		if opts.CapturePane != nil {
			screen, err := opts.CapturePane(exactTarget)
			if err != nil {
				return fmt.Errorf("check pane input: %w", err)
			}
			if ScreenAwaitsHumanInput(screen) {
				return ErrAwaitingHumanInput
			}
		}
		return nil
	}
	return fn(Input{tmux: opts.Tmux, pane: tmuxcmd.PaneFromSpec(exactTarget), guard: guard})
}

// A signal-only cancellation cannot accept a dialog's default selection.
// Keep stop/interrupt usable while human input is pending; text and Enter
// always pass the shared guard, including after a soft-exit prefix.
func cancelOnlyKeys(keys []string) bool {
	if len(keys) == 0 {
		return false
	}
	for _, key := range keys {
		if key != "C-c" && key != "Escape" {
			return false
		}
	}
	return true
}

// ExactInputTarget canonicalizes an input target to its exact form: tmux
// pane IDs are already exact targets and pass through (prefixing one with
// the session-name exact marker produces the invalid target "=%N" in real
// tmux), while session-shaped targets get the marker to avoid tmux's
// unique-prefix fallback onto a namesake session. Exported so in-process
// serialization (agentd's pane-inject mutexes) can key on the very same
// canonical identity as this package's cross-process advisory file lock.
func ExactInputTarget(tmuxTarget string) string {
	return tmuxcmd.PaneFromSpec(tmuxTarget).String()
}

// InjectTextAndSubmit delivers one complete prompt turn. Multiline/tabbed
// content (or any content with ForceBracketedPaste) uses tmux bracketed paste
// so its bytes remain one input submission; other single-line content uses the
// smaller send-keys path. The two settled Enter presses preserve the
// long-standing paste-coalescing workaround.
func InjectTextAndSubmit(tmuxTarget, text string, opts Options) error {
	opts = opts.resolved()
	return WithLock(tmuxTarget, opts, func(in Input) error {
		if len(opts.PrefixKeys) > 0 {
			for _, key := range opts.PrefixKeys {
				if err := in.Keys(key); err != nil {
					return fmt.Errorf("send-keys prefix %q: %w", key, err)
				}
			}
			// The same settle the text/Enter split uses: the application has
			// to process the cancel and redraw before the text arrives, or
			// the text lands in the state the prefix was there to leave.
			time.Sleep(opts.SettleDelay)
		}
		if opts.ForceBracketedPaste || strings.ContainsAny(text, "\n\t") {
			if err := in.Paste(text); err != nil {
				return err
			}
		} else if err := in.Literal(text); err != nil {
			return fmt.Errorf("send-keys text: %w", err)
		}
		time.Sleep(opts.SettleDelay)
		if err := in.Keys("Enter"); err != nil {
			return fmt.Errorf("send-keys submit: %w", err)
		}
		time.Sleep(opts.SettleDelay)
		// The first Enter already submitted. A dialog raised by that turn
		// suppresses this workaround Enter, but must not cause full-text retry.
		_ = in.Keys("Enter")
		return nil
	})
}

// SendKeys serializes a single non-text tmux key operation (for example the
// Enter that accepts a plan dialog) with message and lifecycle input streams.
func SendKeys(tmuxTarget string, opts Options, keys ...string) error {
	return WithLock(tmuxTarget, opts, func(in Input) error {
		return in.Keys(keys...)
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
