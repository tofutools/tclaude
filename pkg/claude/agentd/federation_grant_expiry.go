package agentd

import (
	"log/slog"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Grant checks read the expiry on every request, so an expired grant stops
// working on time by itself. Peers learn what they hold from our catalog,
// though, which is only sent on a change: the timer below re-sends it when
// the soonest timed grant runs out, so a peer's view drops it too.
var grantExpiry struct {
	mu    sync.Mutex
	timer *time.Timer
	at    time.Time
}

// grantExpirySlack lets the expiry pass before the catalog is rebuilt.
const grantExpirySlack = time.Second

// grantExpiryRecheck bounds one wait: a timer's monotonic clock stops while
// the host sleeps, so a long wait would wake late after a suspend.
const grantExpiryRecheck = 5 * time.Minute

// scheduleGrantExpiryBroadcast arms the timer for the soonest grant expiry,
// replacing an earlier one. Call it whenever grants change.
func scheduleGrantExpiryBroadcast() {
	next, err := db.NextFederationPeerGrantExpiry()
	if err != nil {
		slog.Warn("federation: next grant expiry", "error", err)
		return
	}
	grantExpiry.mu.Lock()
	defer grantExpiry.mu.Unlock()
	if grantExpiry.timer != nil {
		grantExpiry.timer.Stop()
		grantExpiry.timer = nil
	}
	grantExpiry.at = next
	if next.IsZero() {
		return
	}
	wait := min(time.Until(next)+grantExpirySlack, grantExpiryRecheck)
	var t *time.Timer
	t = time.AfterFunc(wait, func() {
		grantExpiry.mu.Lock()
		// A reschedule that raced this firing owns the state now.
		if grantExpiry.timer != t {
			grantExpiry.mu.Unlock()
			return
		}
		due := !time.Now().Before(grantExpiry.at)
		grantExpiry.timer = nil
		grantExpiry.mu.Unlock()
		if due {
			broadcastFederationCatalogs()
		}
		scheduleGrantExpiryBroadcast()
	})
	grantExpiry.timer = t
}
