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

// cancelUnavailableNudgeTargets abandons queued nudges whose recipient can
// never receive them: the target agent is retired or its actor row is gone.
// Without this, such rows sat "undelivered" forever and the stale-queue
// watchdog re-warned about them on every daemon start (the operator's boot
// noise). Running in the reaper tick — rather than only at retire time —
// makes the cleanup self-healing: it also catches a message that raced past
// a retire (observed live: a send landing seconds after its target retired)
// and anything already stuck from before this sweep existed.
//
// Cancellation is durable (CancelAgentMessageNudge) and logged exactly once
// per message; the message itself stays readable in the recipient's inbox.
// Reinstating the agent clears the marker (see db.ReinstateAgentByID), so a
// soft retire remains fully reversible. It runs BEFORE warnStaleNudgeQueues
// so a cancelled queue never also warns as stuck.
func cancelUnavailableNudgeTargets(now time.Time) {
	msgs, err := db.ListAllUndeliveredAgentMessages()
	if err != nil {
		slog.Warn("nudge queue cancel scan failed", "error", err)
		return
	}
	agents := map[string]*db.Agent{}
	for _, m := range msgs {
		targetAgent, reason := nudgeTargetGoneReason(m, agents)
		if reason == "" {
			continue
		}
		// The UPDATE re-validates the unavailability verdict against the live
		// agents row, so a target reinstated after this sweep's (cached) read
		// is left alone rather than re-cancelled with a stamp nothing would
		// ever clear again.
		cancelled, err := db.CancelAgentMessageNudge(m.ID, targetAgent, now, reason)
		if err != nil {
			slog.Warn("nudge cancel failed", "error", err, "msg_id", m.ID)
			continue
		}
		if cancelled {
			slog.Warn("nudge delivery cancelled; target unavailable",
				"msg_id", m.ID,
				"target", nudgeQueueKey(m),
				"reason", reason,
				"queued_for", now.Sub(m.CreatedAt).Round(time.Second))
		}
	}
}

// nudgeTargetGoneReason reports why message m's nudge can never be delivered,
// returning the resolved target actor and the reason ("" when delivery is
// still possible). A head-following or prev-gen-pinned message names its
// actor in to_agent; non-actor conv mail resolves the conv's owning actor at
// scan time (an actor may have been enrolled after the send). A plain conv
// with no actor is left queued — it may simply be offline and come back. Any
// lookup error also leaves the row queued: never cancel on uncertainty.
// agents caches actor rows across one sweep so a deep queue costs one lookup
// per target rather than per message; the verdict is advisory only — the
// cancel UPDATE re-checks it against the live row.
func nudgeTargetGoneReason(m *db.AgentMessage, agents map[string]*db.Agent) (agentID, reason string) {
	agentID = m.ToAgent
	if agentID == "" {
		id, err := db.AgentIDForConv(m.ToConv)
		if err != nil || id == "" {
			return "", ""
		}
		agentID = id
	}
	a, cached := agents[agentID]
	if !cached {
		var err error
		a, err = db.GetAgent(agentID)
		if err != nil {
			return "", ""
		}
		agents[agentID] = a
	}
	if a == nil {
		return agentID, "target agent deleted"
	}
	if !a.Active() {
		return agentID, "target agent retired"
	}
	return agentID, ""
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
