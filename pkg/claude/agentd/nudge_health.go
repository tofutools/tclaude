package agentd

import (
	"log/slog"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

var (
	staleNudgeThreshold = 3 * time.Minute
	staleNudgeLogEvery  = 5 * time.Minute
	staleNudgeLogMu     sync.Mutex
	staleNudgeLoggedAt  = map[string]time.Time{}
	nudgeClaimLease     = 2 * time.Minute
)

type staleNudgeQueue struct {
	key    string
	oldest *db.AgentMessage
	count  int
}

// warnStaleNudgeQueues is the WARN-level watchdog for durable delivery. It
// runs before the reaper performs any tmux I/O, so even a wedged liveness probe
// cannot suppress the incident line. One aggregate per target names the queue
// key, oldest message id, elapsed time, attempt count, and claim age — enough
// to locate both a held queue and an in-flight drain stuck after its claim.
func warnStaleNudgeQueues(now time.Time) {
	msgs, err := db.ListAllUndeliveredAgentMessages()
	if err != nil {
		slog.Warn("nudge queue health scan failed", "error", err)
		return
	}
	queues := map[string]*staleNudgeQueue{}
	for _, m := range msgs {
		if m.CreatedAt.IsZero() || now.Sub(m.CreatedAt) < staleNudgeThreshold {
			continue
		}
		key := nudgeQueueKey(m)
		q := queues[key]
		if q == nil {
			q = &staleNudgeQueue{key: key, oldest: m}
			queues[key] = q
		}
		q.count++
		if m.CreatedAt.Before(q.oldest.CreatedAt) {
			q.oldest = m
		}
	}

	staleNudgeLogMu.Lock()
	defer staleNudgeLogMu.Unlock()
	for key := range staleNudgeLoggedAt {
		if _, still := queues[key]; !still {
			delete(staleNudgeLoggedAt, key)
		}
	}
	for key, q := range queues {
		if last := staleNudgeLoggedAt[key]; !last.IsZero() && now.Sub(last) < staleNudgeLogEvery {
			continue
		}
		m := q.oldest
		attrs := []any{
			"target", key,
			"msg_id", m.ID,
			"elapsed", now.Sub(m.CreatedAt).Round(time.Second),
			"queued", q.count,
			"attempts", m.NudgeAttempts,
		}
		if !m.NudgeClaimedAt.IsZero() {
			attrs = append(attrs, "claim_elapsed", now.Sub(m.NudgeClaimedAt).Round(time.Second))
		}
		slog.Warn("nudge delivery queue stuck", attrs...)
		staleNudgeLoggedAt[key] = now
	}
}

func releaseExpiredNudgeClaims(now time.Time) {
	msgs, err := db.ListAllUndeliveredAgentMessages()
	if err != nil {
		slog.Warn("nudge claim lease recovery failed", "error", err)
		return
	}
	cutoff := now.Add(-nudgeClaimLease)
	released := 0
	for _, m := range msgs {
		if m.NudgeClaimedAt.IsZero() || !m.NudgeClaimedAt.Before(cutoff) {
			continue
		}
		token := db.AgentMessageNudgeClaim{
			ClaimedAt: m.NudgeClaimedAt.Format(time.RFC3339Nano),
			Attempt:   m.NudgeAttempts,
		}
		// An old claim can still be making forward progress through this
		// daemon's bounded tmux probes. Recycling it would permit a second
		// worker to inject the same side effect. Orphans have no registry
		// entry and remain recoverable here; a daemon restart clears them all
		// before starting workers.
		if isActiveNudge(m.ID, token) {
			continue
		}
		ok, releaseErr := db.ReleaseAgentMessageNudge(m.ID, token)
		if releaseErr != nil {
			slog.Warn("nudge claim lease recovery failed", "error", releaseErr, "msg_id", m.ID)
			continue
		}
		if ok {
			released++
		}
	}
	if released > 0 {
		slog.Warn("released expired nudge claims", "count", released, "lease", nudgeClaimLease)
	}
}

func nudgeQueueKey(m *db.AgentMessage) string {
	if m.ToAgent != "" && !m.PinGen {
		return "agent:" + m.ToAgent
	}
	return "conv:" + m.ToConv
}
