package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/tofutools/tclaude/pkg/common"
)

// standingOrderLockTimeout bounds how long one path will wait for another to
// finish delivering. Deliberately short: this sits inside the agent's turn,
// behind a hook client that gives up at 20s, and the cost of giving up is one
// possibly-duplicated reminder — far cheaper than holding a turn open.
const standingOrderLockTimeout = 3 * time.Second

const standingOrderLockRetryDelay = 20 * time.Millisecond

// lockStandingOrderDelivery serializes the evaluate → deliver → record
// sequence for one conversation.
//
// The cadence check is a read-modify-write: StandingOrderDeliveredInEpoch asks
// "has this been delivered", and the ledger row that answers it is written
// only after delivery succeeds. Two SessionStart events for the same
// conversation — a compaction racing a resume, or a replay — can otherwise
// both read "not yet" and both deliver a once-per-generation order.
//
// It is a FILE lock rather than a mutex because the two paths that deliver run
// in different processes: the direct hook callback runs inside the agent's own
// pane, while the OpenCode observation path runs inside agentd. A process-local
// mutex would serialize neither against the other.
//
// It is also a DISTINCT lock from the per-session hook lock, not a widening of
// it. That lock is taken and released by applyHook before any of this runs, so
// the two never nest and cannot deadlock; reusing it would instead mean holding
// the hook path's lock across a delivery, which is exactly the kind of
// long-held critical section the 20s hook deadline exists to avoid.
//
// Failure to acquire is reported as acquired=false rather than as an error,
// and the caller must SKIP the boundary for anything cadence-gated instead of
// proceeding unserialized. Proceeding would knowingly deliver a second copy of
// a once-per-generation order — the exact race the lock exists to close.
// Skipping leaves the cadence open, so the order is still pending and the next
// boundary delivers it; and in the common case the holder we lost the race to
// is delivering that very order right now, so nothing is lost at all.
func lockStandingOrderDelivery(ctx context.Context, convID string) (func(), bool) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		// No conversation, nothing to serialize against. Not a failure: the
		// callers that reach here have already resolved a real conv, and a
		// caller that has not cannot deliver anything either.
		return func() {}, true
	}
	lockDir := filepath.Join(common.CacheDir(), "locks")
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		slog.Warn("standing orders: no lock dir, skipping cadence-gated delivery",
			"error", err, "module", "hooks")
		return func() {}, false
	}
	// The conversation id is hashed rather than sanitized. It reaches this
	// function from the resolved session row, but that row's id is ultimately
	// harness-supplied data, and a lock filename is a path: "..", separators
	// on either platform convention, and pathological lengths all have to be
	// impossible, not merely unlikely. A fixed-width hex digest is, and it
	// still maps one conversation onto exactly one lock.
	sum := sha256.Sum256([]byte(convID))
	lockPath := filepath.Join(lockDir, "standing-order-"+hex.EncodeToString(sum[:])+".lock")
	fl := flock.New(lockPath)

	lockCtx, cancel := context.WithTimeout(ctx, standingOrderLockTimeout)
	defer cancel()
	locked, err := fl.TryLockContext(lockCtx, standingOrderLockRetryDelay)
	if err != nil || !locked {
		slog.Info("standing orders: another path holds this conversation's delivery lock, skipping cadence-gated orders",
			"conv_id", convID, "error", err, "module", "hooks")
		return func() {}, false
	}
	return func() {
		if err := fl.Unlock(); err != nil {
			slog.Debug("standing orders: failed to release delivery lock",
				"conv_id", convID, "error", err, "module", "hooks")
		}
	}, true
}
