package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gofrs/flock"
)

// acquireHookLock acquires an exclusive file lock for the given session key,
// blocking until the lock is available. Returns an unlock function.
// This prevents concurrent hook callbacks for the same session from racing
// on the read-modify-write of session state.
func acquireHookLock(sessionKey string) (func(), error) {
	lockDir := filepath.Join(os.TempDir(), "tclaude-locks")
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		return func() {}, fmt.Errorf("failed to create lock dir: %w", err)
	}

	lockPath := filepath.Join(lockDir, "hook-"+strings.ReplaceAll(sessionKey, "/", "-")+".lock")
	fl := flock.New(lockPath)
	if err := fl.Lock(); err != nil {
		return func() {}, fmt.Errorf("failed to acquire lock: %w", err)
	}

	return func() {
		_ = fl.Unlock()
	}, nil
}
