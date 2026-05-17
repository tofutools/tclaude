package common

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// RotatingWriter is the io.Writer slog writes the tclaude log through.
// It wraps the active log file behind a mutex so a rotation — renaming
// the file aside and swapping in a fresh fd — never interleaves with a
// record write.
//
// Why a wrapper at all: agentd is long-lived and holds the log fd for
// its whole life. A plain `mv output.log output.log.1` does NOT redirect
// that fd — on POSIX (and Windows) the fd follows the inode, so the
// daemon would keep writing into the rotated-away file forever. Rotation
// must therefore reopen the log by path and swap the fd in-process;
// rotate() does exactly that under the mutex.
//
// Two writer populations share the log file: the agentd daemon and
// every transient `tclaude` CLI invocation. They all just append
// (O_APPEND, atomic per write) — no cross-process lock. Only agentd
// rotates. A transient CLI process that holds an fd across a rotation
// lands its last few lines in the just-rotated file instead of the
// fresh one; for a log that is fine and not worth a lock to prevent.
// CLI processes get a RotatingWriter too, but never Configure it, so
// MaybeRotate no-ops and it behaves as a plain appending file.
//
// IMPORTANT: no method here may call slog. RotatingWriter IS the sink
// behind slog; logging from inside rotate/MaybeRotate/Write while the
// mutex is held would deadlock. Errors are returned to the caller (the
// agentd rotation ticker), which logs them after the lock is released.
type RotatingWriter struct {
	// path is the active log file path. Immutable after construction,
	// so it is read without the lock.
	path string

	mu      sync.Mutex
	file    *os.File
	maxSize int64 // > 0 enables rotation; 0 means "no rotation" (CLI processes)
	keep    int   // number of rotated files (output.log.1 … .keep) to retain
}

// openLogFile opens (creating, append-mode) the log file at path. It is
// a package var so a test can inject an open failure to exercise the
// reopen-failure rollback in rotate() — the highest-risk branch, which
// no filesystem-permission trick can isolate (a non-writable directory
// fails the rename before the reopen is ever reached).
var openLogFile = func(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
}

// OpenRotatingWriter opens (creating parent dirs and the file as
// needed, append mode) the log file at path and wraps it. Rotation
// stays disabled until Configure sets a positive max size.
func OpenRotatingWriter(path string) (*RotatingWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	f, err := openLogFile(path)
	if err != nil {
		return nil, err
	}
	return &RotatingWriter{path: path, file: f}, nil
}

// Write appends p to the active log file. It implements io.Writer for
// slog's handler. The mutex makes it mutually exclusive with rotate, so
// a record is never split across the file/fd swap.
func (rw *RotatingWriter) Write(p []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return rw.file.Write(p)
}

// Close closes the active log file. Tests use it to avoid leaking fds;
// the agentd daemon never closes (its log lives as long as it does).
func (rw *RotatingWriter) Close() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return rw.file.Close()
}

// Path returns the active log file path.
func (rw *RotatingWriter) Path() string { return rw.path }

// Configure sets the rotation policy: maxSize (bytes; <= 0 disables
// rotation) and keep (how many rotated files to retain). agentd calls
// this once at startup before the rotation ticker runs.
func (rw *RotatingWriter) Configure(maxSize int64, keep int) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.maxSize = maxSize
	if keep < 0 {
		keep = 0
	}
	rw.keep = keep
}

// MaybeRotate rotates the log if it has grown past the configured max
// size. It is the size-based rotation policy; a future time-based mode
// would be a sibling method driving the same rotate(). Cheap enough to
// call from a periodic ticker — one os.Stat when rotation is enabled.
//
// Only agentd's single rotation goroutine calls this, so MaybeRotate
// calls never overlap each other.
func (rw *RotatingWriter) MaybeRotate() error {
	rw.mu.Lock()
	maxSize := rw.maxSize
	rw.mu.Unlock()
	if maxSize <= 0 {
		return nil // rotation disabled
	}

	info, err := os.Stat(rw.path)
	if err != nil {
		if os.IsNotExist(err) {
			// The active log was removed out from under us (e.g. a
			// human deleted it). The daemon's fd still points at the
			// now-unlinked inode, so reopen by path so fresh logs land
			// somewhere visible again.
			return rw.reopen()
		}
		return err
	}
	if info.Size() < maxSize {
		return nil
	}
	return rw.rotate()
}

// rotate performs one rotation: cascade the existing rotated files up a
// slot (dropping any past keep), sweep any files orphaned by a lowered
// keep, move the active log into slot 1, then reopen a fresh active log
// and swap the write fd. All renames are within the log's directory —
// same filesystem, so each os.Rename is atomic on POSIX and
// replace-existing on Windows.
//
// Best-effort: a failed cascade rename is collected and returned but
// does not abort the rotation. If the final reopen fails the old fd is
// left in place (the daemon keeps logging, into the rotated-away file)
// and the active file is rolled back to its path so a later tick can
// retry. rotate never leaves rw.file pointing at a closed/invalid fd.
//
// Known edge: if the reopen fails persistently (a permanently
// unwritable log directory), every retry tick re-runs the cascade
// before failing again — so the rotated files shift up a slot and the
// oldest is dropped each tick, slowly losing history while no fresh
// active file rotates in. Accepted: a persistently unwritable log dir
// is a broken host, and the alternative (deferring the cascade until
// the reopen is known to succeed) complicates the common path for a
// pathological case.
func (rw *RotatingWriter) rotate() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	keep := rw.keep

	var errs []error

	// The file in the last slot has nowhere to cascade to — drop it.
	if err := removeIfExists(rotatedPath(rw.path, keep)); err != nil {
		errs = append(errs, fmt.Errorf("drop oldest rotated log: %w", err))
	}
	// Shift slots keep-1 … 1 up by one: .(i) -> .(i+1). Oldest-first so
	// nothing is clobbered.
	for i := keep - 1; i >= 1; i-- {
		src, dst := rotatedPath(rw.path, i), rotatedPath(rw.path, i+1)
		if err := renameIfExists(src, dst); err != nil {
			errs = append(errs, fmt.Errorf("cascade %s -> %s: %w", src, dst, err))
		}
	}
	// Sweep rotated files orphaned by a lowered keep: rotate() only ever
	// touches slots 1..keep, so files left by an earlier run with a
	// larger keep would leak forever and defeat the bounded-history
	// goal. Slots are contiguous, so stop at the first absent one.
	for n := keep + 1; ; n++ {
		p := rotatedPath(rw.path, n)
		if _, err := os.Stat(p); err != nil {
			break
		}
		if err := removeIfExists(p); err != nil {
			errs = append(errs, fmt.Errorf("sweep orphaned %s: %w", p, err))
		}
	}

	// Move the active log into slot 1 (or, when keep is 0, discard it).
	if keep >= 1 {
		if err := os.Rename(rw.path, rotatedPath(rw.path, 1)); err != nil {
			errs = append(errs, fmt.Errorf("rotate active log: %w", err))
			return errors.Join(errs...)
		}
	} else if err := removeIfExists(rw.path); err != nil {
		errs = append(errs, fmt.Errorf("discard active log: %w", err))
		return errors.Join(errs...)
	}

	// Reopen a fresh active log and swap the fd.
	if err := rw.reopenLocked(); err != nil {
		// Reopen failed: the old fd still works (it now writes into the
		// rotated-away inode), so logging is not lost. Roll the active
		// file back to its path so the next tick can retry cleanly.
		if keep >= 1 {
			_ = os.Rename(rotatedPath(rw.path, 1), rw.path)
		}
		errs = append(errs, fmt.Errorf("reopen active log after rotation: %w", err))
	}
	return errors.Join(errs...)
}

// reopen takes the lock and reopens the active log file. Used when the
// file vanished outside a rotation.
func (rw *RotatingWriter) reopen() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return rw.reopenLocked()
}

// reopenLocked opens a fresh file at rw.path and swaps it in as the
// write target, closing the previous fd. Caller must hold rw.mu.
func (rw *RotatingWriter) reopenLocked() error {
	f, err := openLogFile(rw.path)
	if err != nil {
		return err
	}
	old := rw.file
	rw.file = f
	_ = old.Close()
	return nil
}

// rotatedPath returns the path of the n-th rotated log file, e.g.
// /…/output.log -> /…/output.log.1. Rotated files are siblings of the
// active file (same directory) so every rename stays on one filesystem.
func rotatedPath(active string, n int) string {
	return active + "." + strconv.Itoa(n)
}

// removeIfExists removes path, treating "not found" as success.
func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// renameIfExists renames src to dst, treating a missing src as success
// (a rotated slot we have not filled yet).
func renameIfExists(src, dst string) error {
	if err := os.Rename(src, dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
