package agentd

import (
	"log/slog"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/common"
)

// logRotationInterval is how often agentd checks the active log's size.
// Rotation is coarse — a log rarely grows its whole budget within 30s,
// and an os.Stat is cheap — so this matches the daemon's other
// housekeeping tickers (the cron scheduler, the session reaper). It is
// a var, not a const, so tests can shrink it.
var logRotationInterval = 30 * time.Second

// startLogRotation wires agentd's size-based rotation of ~/.tclaude/output.log.
//
// Writer model (verified for this feature): every tclaude process —
// the agentd daemon and every transient CLI invocation — appends to
// that file with O_APPEND, so concurrent appends are atomic and need
// no cross-process lock. Only agentd rotates.
//
// agentd holds the log fd for its whole life, so a plain rename would
// leave it writing into the rotated-away inode forever. Rotation
// therefore renames the file AND reopens a fresh one, swapping the
// writer's fd under an in-process mutex — common.RotatingWriter does
// this. Transient CLI processes need nothing: each opens the log fresh
// by path and exits quickly, so post-rotation it just opens the new
// file.
//
// rw is common.ActiveLogRotator() — the writer the shared logging
// setup installed. It is nil only when logging setup failed (e.g. no
// home directory), in which case rotation is simply skipped. The
// goroutine stops when the daemon-wide stop channel closes.
func startLogRotation(stop <-chan struct{}, rw *common.RotatingWriter, cfg *config.Config) {
	maxSize, keep := cfg.ResolvedLogRotation()
	if rw == nil {
		slog.Warn("log rotation: no active log writer; rotation disabled")
		return
	}
	if maxSize <= 0 {
		slog.Info("log rotation: disabled (config max_size is 0)")
		return
	}
	rw.Configure(maxSize, keep)
	slog.Info("log rotation enabled", "path", rw.Path(), "max_size_bytes", maxSize, "keep", keep)

	go func() {
		// First check fires immediately so an already-oversized log —
		// the common case the first time this feature runs on a
		// long-lived install — rotates at startup, not a full interval
		// later.
		rotateLogOnce(rw)
		t := time.NewTicker(logRotationInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				rotateLogOnce(rw)
			}
		}
	}()
}

// rotateLogOnce runs one size check + rotation, logging any failure.
// Failures are non-fatal: a failed rotation leaves the old fd valid
// (the daemon keeps logging) and the next tick retries. Logging here is
// safe — MaybeRotate has returned, so the RotatingWriter mutex is
// released and slog will not deadlock on it.
func rotateLogOnce(rw *common.RotatingWriter) {
	if err := rw.MaybeRotate(); err != nil {
		slog.Warn("log rotation check failed", "err", err)
	}
}
