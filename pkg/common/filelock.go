package common

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/alexflint/go-filemutex"
)

// AcquireHookLock acquires a cross-process file lock used to serialize
// hook callbacks and status bar invocations. This prevents concurrent
// processes from racing on session state and usage API fetches.
// Returns a function to release the lock (safe to call even if locking failed).
func AcquireHookLock() func() {
	lockPath := filepath.Join(CacheDir(), "hook-callback.lock")
	_ = os.MkdirAll(filepath.Dir(lockPath), 0755)

	fmtx, err := filemutex.New(lockPath)
	if err != nil {
		slog.Warn("failed to create hook file lock, proceeding unlocked", "error", err)
		return func() {}
	}

	if err := fmtx.Lock(); err != nil {
		slog.Warn("failed to acquire hook file lock, proceeding unlocked", "error", err)
		return func() {}
	}

	return func() {
		_ = fmtx.Unlock()
	}
}
