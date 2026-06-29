package session

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/gofrs/flock"
	"github.com/tofutools/tclaude/pkg/common"
)

// acquireConvLaunchLock takes a NON-BLOCKING exclusive file lock that serializes
// concurrent launches of one conversation. Unlike acquireHookLock (which
// blocks), this fails fast: acquired=false means another launch already holds
// the lock. The OS releases the lock when this process exits, so a crashed
// launcher never wedges future resumes — no TTL or stale-claim sweep needed.
// See JOH-332.
func acquireConvLaunchLock(convID string) (release func(), acquired bool, err error) {
	noop := func() {}
	lockDir := filepath.Join(common.CacheDir(), "locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return noop, false, fmt.Errorf("create lock dir: %w", err)
	}
	lockPath := filepath.Join(lockDir, "launch-"+strings.ReplaceAll(convID, "/", "-")+".lock")
	fl := flock.New(lockPath)
	locked, err := fl.TryLock()
	if err != nil {
		return noop, false, fmt.Errorf("acquire launch lock: %w", err)
	}
	if !locked {
		return noop, false, nil
	}
	return func() { _ = fl.Unlock() }, true, nil
}

// ReserveConvForLaunch serializes concurrent launches of convID and verifies,
// UNDER the lock, that no live session exists for it. It closes the race the
// bare LiveSessionForConv read-guard cannot: two resumes both passing the read
// guard, then both running `claude --resume` on the same .jsonl (interleaved
// appends → conv-file corruption). The read guard is the cheap fast-fail; this
// is the authoritative gate taken immediately before `tmux new-session`.
//
// On success it returns a non-nil release func the caller MUST defer (it frees
// the lock once the session row is written, or on any failure path). On
// rejection it returns a nil release and an error to surface — either a live
// session already exists for the conv, or another launch for it is in flight.
//
// A lock-infrastructure failure (e.g. an unwritable cache dir) degrades to
// best-effort: it logs and returns a no-op release with no rejection, falling
// back to read-guard-only behaviour rather than blocking every resume. The
// race window it would have closed is narrow; a broken lock dir must not wedge
// the primary operation. See JOH-332.
func ReserveConvForLaunch(convID string) (release func(), reject error) {
	noop := func() {}
	if convID == "" {
		return noop, nil
	}
	rel, acquired, err := acquireConvLaunchLock(convID)
	if err != nil {
		slog.Warn("conv launch reservation unavailable; proceeding best-effort", "conv", convID, "err", err)
		return noop, nil
	}
	if !acquired {
		return nil, fmt.Errorf("another launch is already starting for this conversation; retry in a moment")
	}
	if owner := LiveSessionForConv(convID); owner != nil {
		rel()
		return nil, fmt.Errorf("session %s already exists for this conversation; attach with: tclaude session attach %s", owner.TmuxSession, owner.TmuxSession)
	}
	return rel, nil
}
