package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/tofutools/tclaude/pkg/common"
)

// hookLockRetryDelay is how often a deadline-bounded acquisition retries.
// Hook events are short; this only has to be small against the caller's
// deadline, not against the lock hold time.
const hookLockRetryDelay = 20 * time.Millisecond

// acquireHookLockContext acquires an exclusive file lock for the given
// session key, honouring the caller's deadline. It returns an unlock
// function. This prevents concurrent hook callbacks for the same session
// from racing on the read-modify-write of session state.
//
// The deadline matters because the same lock is now taken from two very
// different places. In the hook callback it guards one process's own
// session and blocking forever is correct: the only thing that stalls is
// the agent that owns the lock, so that path passes a context with no
// deadline and takes the plain blocking flock, unchanged. Inside agentd
// (TCL-754's broker) the same blocking wait would park a daemon goroutine
// that no client disconnect can free — and the lock file lives under
// ~/.cache/tclaude, which is NOT a protected root, so a wrapped agent can
// hold it deliberately. A sandbox could then pin daemon goroutines and
// file descriptors until agentd stops serving every other agent. A
// bounded context polls and gives up, letting that caller answer "busy"
// instead of holding a goroutine open.
func acquireHookLockContext(ctx context.Context, sessionKey string) (func(), error) {
	lockDir := filepath.Join(common.CacheDir(), "locks")
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		return func() {}, fmt.Errorf("failed to create lock dir: %w", err)
	}

	lockPath := filepath.Join(lockDir, "hook-"+strings.ReplaceAll(sessionKey, "/", "-")+".lock")
	fl := flock.New(lockPath)

	_, hasDeadline := ctx.Deadline()
	if !hasDeadline && ctx.Done() == nil {
		if err := fl.Lock(); err != nil {
			return func() {}, fmt.Errorf("failed to acquire lock: %w", err)
		}
		return func() { _ = fl.Unlock() }, nil
	}

	locked, err := fl.TryLockContext(ctx, hookLockRetryDelay)
	if err != nil {
		return func() {}, fmt.Errorf("failed to acquire lock: %w", err)
	}
	if !locked {
		return func() {}, fmt.Errorf("timed out acquiring hook lock for %s", sessionKey)
	}
	return func() { _ = fl.Unlock() }, nil
}
