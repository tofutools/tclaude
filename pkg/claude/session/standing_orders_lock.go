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

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/common"
)

// standingOrderLockTimeout bounds how long one path will wait for another to
// finish delivering. Deliberately short: this sits inside the agent's turn,
// behind a hook client that gives up at 20s, and the cost of giving up is one
// possibly-duplicated reminder — far cheaper than holding a turn open.
const standingOrderLockTimeout = 3 * time.Second

const standingOrderLockRetryDelay = 20 * time.Millisecond

// lockStandingOrderDelivery serializes the evaluate → deliver → record
// sequence for one recipient scope.
//
// Cadence and cooldown checks are read-modify-write operations: the ledger row
// that closes either window is written only after delivery succeeds. Two
// SessionStart events in the same protected scope can otherwise both read
// "not yet" and both deliver one rate-controlled order.
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
// a rate-controlled order — the exact race the lock exists to close. Skipping
// leaves the rate window open, so the order is still pending and the next
// boundary can deliver it; and in the common case the holder we lost the race
// to is delivering that very order right now, so nothing is lost at all.
type standingOrderRateLocks struct {
	release          func()
	cadenceAcquired  bool
	cooldownAcquired bool
}

// lockStandingOrderRateControls acquires only the scopes this order set needs.
//
// Cadence without cooldown is conversation-generation scoped. Keeping that
// lock on convID preserves a zero-cooldown order's ability to deliver to a new
// generation while the old one is still awaiting its hook ACK. Cooldown is
// deliberately stable-agent scoped so /clear or reincarnation cannot reset
// the minimum interval. A cooldown order that also uses once-per-generation
// needs only the stronger agent lock: it serializes every generation and
// therefore also serializes one generation.
func lockStandingOrderRateControls(
	ctx context.Context,
	orders []*db.StandingOrder,
	convID, agentID string,
) standingOrderRateLocks {
	needCadence, needCooldown := false, false
	for _, o := range orders {
		if o == nil {
			continue
		}
		if o.CooldownSeconds > 0 {
			needCooldown = true
		} else if o.Cadence == db.StandingCadenceOncePerGeneration {
			needCadence = true
		}
	}
	locks := standingOrderRateLocks{
		release:          func() {},
		cadenceAcquired:  !needCadence,
		cooldownAcquired: !needCooldown,
	}
	var releases []func()
	if needCooldown {
		release, acquired := lockStandingOrderDelivery(ctx, "agent", agentID)
		locks.cooldownAcquired = acquired
		if acquired {
			releases = append(releases, release)
		}
	}
	if needCadence {
		release, acquired := lockStandingOrderDelivery(ctx, "conv", convID)
		locks.cadenceAcquired = acquired
		if acquired {
			releases = append(releases, release)
		}
	}
	locks.release = func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	return locks
}

func lockStandingOrderDelivery(ctx context.Context, scope, key string) (func(), bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		// An empty key must never become one shared bucket for every
		// actorless conversation. Refuse rate-gated delivery instead.
		return func() {}, false
	}
	lockDir := filepath.Join(common.CacheDir(), "locks")
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		slog.Warn("standing orders: no lock dir, skipping cadence-gated delivery",
			"error", err, "module", "hooks")
		return func() {}, false
	}
	// Hashing keeps even a malformed persisted id from becoming a path.
	sum := sha256.Sum256([]byte(scope + ":" + key))
	lockPath := filepath.Join(lockDir, "standing-order-"+hex.EncodeToString(sum[:])+".lock")
	fl := flock.New(lockPath)

	lockCtx, cancel := context.WithTimeout(ctx, standingOrderLockTimeout)
	defer cancel()
	locked, err := fl.TryLockContext(lockCtx, standingOrderLockRetryDelay)
	if err != nil || !locked {
		slog.Info("standing orders: another path holds this agent's delivery lock, skipping rate-gated orders",
			"scope", scope, "key", key, "error", err, "module", "hooks")
		return func() {}, false
	}
	return func() {
		if err := fl.Unlock(); err != nil {
			slog.Debug("standing orders: failed to release delivery lock",
				"scope", scope, "key", key, "error", err, "module", "hooks")
		}
	}, true
}
