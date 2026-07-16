package agentd

import (
	"errors"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// flushSender delivers a single undelivered nudge. The production
// implementation is realFlushSender; tests inject a stub so the
// claim/iteration logic in `flush` can be exercised without a real
// tmux session.
type flushSender func(m *db.AgentMessage, nudge string) (delivered bool)

// flushMinInterval is how often a single conv-id is allowed to
// trigger a flush. Five seconds keeps the cost amortised when an
// agent fires a burst of CLI calls back-to-back, while still being
// short enough that the human perceives "queue clears as soon as
// the agent comes back."
const flushMinInterval = 5 * time.Second

// Failed first-delivery nudges retry exponentially per message, capped so a
// broken pane remains observable/recoverable without being spammed. Attempt
// metadata is durable on agent_messages; these are policy timings only.
var (
	nudgeRetryBase = 30 * time.Second
	nudgeRetryMax  = 5 * time.Minute
)

var (
	flushDebounceMu sync.Mutex
	flushDebounce   = map[string]time.Time{}
	activeNudgeMu   sync.Mutex
	activeNudges    = map[int64]db.AgentMessageNudgeClaim{}
)

func registerActiveNudge(id int64, token db.AgentMessageNudgeClaim) {
	activeNudgeMu.Lock()
	activeNudges[id] = token
	activeNudgeMu.Unlock()
}

func unregisterActiveNudge(id int64, token db.AgentMessageNudgeClaim) {
	activeNudgeMu.Lock()
	if activeNudges[id] == token {
		delete(activeNudges, id)
	}
	activeNudgeMu.Unlock()
}

func isActiveNudge(id int64, token db.AgentMessageNudgeClaim) bool {
	activeNudgeMu.Lock()
	defer activeNudgeMu.Unlock()
	active, ok := activeNudges[id]
	return ok && active == token
}

// maybeFlushUndelivered is the entry point called from the identity
// middleware on every request whose peer resolves to a conv-id. It
// debounces per-conv (a chatty agent doesn't hit SQLite on every
// keypress) and hands the recipient to the async per-target dispatcher
// (JOH-310) so the request it piggy-backed on pays no latency.
func maybeFlushUndelivered(convID string) {
	if convID == "" {
		return
	}
	flushDebounceMu.Lock()
	if last, ok := flushDebounce[convID]; ok && time.Since(last) < flushMinInterval {
		flushDebounceMu.Unlock()
		return
	}
	flushDebounce[convID] = time.Now()
	flushDebounceMu.Unlock()
	// enqueueDeliveryForConv backgrounds + coalesces the actual drain; the
	// debounce above just rate-limits how often a chatty agent re-arms it.
	enqueueDeliveryForConv(convID)
}

// FlushUndeliveredForTest runs the production drains for convID
// synchronously — both the agent-keyed head-following drain and the
// exact-conv (pinned / non-actor) drain that the async dispatcher would
// run — bypassing the debounce and the background goroutine so a flow
// test can assert delivery (and the awaiting-human-input hold) without
// racing a goroutine. Returns the number of messages claimed this call.
//
// It exists only because BuildHandlerForTest serves buildMux() WITHOUT the
// withIdentity middleware that triggers delivery in production, so the
// resume-delivery path has no request-driven entry point under test. Not a
// subprocess mock and not reachable from production — a sanctioned …ForTest
// entry into the real flush path. See CLAUDE.md "In-process session seams".
func FlushUndeliveredForTest(convID string) int {
	n := 0
	if agentID, _ := db.AgentIDForConv(convID); agentID != "" {
		n += flushAgent(agentID)
	}
	n += flush(convID, realFlushSender)
	return n
}

// flushAgent drains an agent's head-following queue (JOH-310): every
// undelivered, non-pinned message addressed to the agent (by stable
// agent_id, across generations) is delivered to the agent's CURRENT head
// conv. Keying on the actor — and resolving the head at drain time — is
// what lets a message queued before a reincarnate/`/clear` reach the live
// generation. Returns the number claimed.
func flushAgent(agentID string) int {
	if agentID == "" {
		return 0
	}
	head, err := db.CurrentConvForAgent(agentID)
	if err != nil {
		slog.Warn("flush: resolve head conv failed", "error", err, "agent", agentID)
		return 0
	}
	if head == "" {
		return 0
	}
	if sess, probe := probeNudgeSession(head); sess == nil {
		if probe == nudgeSessionIndeterminate {
			slog.Warn("flush: holding regular nudges after indeterminate liveness probe", "agent", agentID)
			return 0
		}
		suppressed, suppressErr := db.SuppressOfflineRegularNudgesForAgent(agentID, time.Now())
		if suppressErr != nil {
			slog.Warn("flush: suppress offline regular nudges failed", "error", suppressErr, "agent", agentID)
		} else if suppressed > 0 {
			slog.Info("flush: suppressed offline regular nudges", "agent", agentID, "count", suppressed)
		}
		return 0
	}
	return flushQueue("agent:"+agentID,
		func() ([]*db.AgentMessage, error) { return db.ListUndeliveredForAgent(agentID) },
		func() bool { return deliverablePane(head) },
		func(m *db.AgentMessage, nudge string) bool { return sendNudgeBracket(head, m.ID, nudge) })
}

// flush drains the exact-conv queue for convID: undelivered messages that
// must stick to this specific conv — prev-gen-pinned (pin_gen=1) and
// non-actor mail (to_agent=”) — as opposed to head-following agent mail,
// which flushAgent owns. The flushSender seam stays so tests can stub the
// tmux side. Returns the number claimed.
func flush(convID string, send flushSender) int {
	if sess, probe := probeNudgeSession(convID); sess == nil {
		if probe == nudgeSessionIndeterminate {
			slog.Warn("flush: holding regular nudges after indeterminate liveness probe", "conv", convID)
			return 0
		}
		suppressed, err := db.SuppressOfflineRegularNudgesForExactConv(convID, time.Now())
		if err != nil {
			slog.Warn("flush: suppress offline regular nudges failed", "error", err, "conv", convID)
		} else if suppressed > 0 {
			slog.Info("flush: suppressed offline regular nudges", "conv", convID, "count", suppressed)
		}
		return 0
	}
	return flushQueue("conv:"+convID,
		func() ([]*db.AgentMessage, error) { return db.ListUndeliveredForExactConv(convID) },
		func() bool { return deliverablePane(convID) },
		send)
}

// flushQueue is the shared drain core: list the queue, hold the WHOLE batch
// unless the delivery pane is reachable RIGHT NOW (alive + not blocked on a
// human), else claim each message atomically (so concurrent drains don't
// double-nudge) and deliver. Returns the number successfully completed.
//
// The canDeliver gate runs before the durable nudge claim. Claims no longer
// stamp delivered_at, but taking an attempt while the recipient is mid
// human-input dialog would still create useless retry history and risk the
// dialog swallowing injected text. Offline regular-message nudges are removed
// before this core; internal nudges and online holds remain queued.
func flushQueue(label string, list func() ([]*db.AgentMessage, error), canDeliver func() bool, send flushSender) int {
	msgs, err := list()
	if err != nil {
		slog.Warn("flush: list undelivered failed", "error", err, "target", label)
		return 0
	}
	if len(msgs) == 0 {
		return 0
	}
	if !canDeliver() {
		slog.Debug("flush: holding queued mail; recipient offline or awaiting human input",
			"target", label, "queued", len(msgs))
		return 0
	}
	delivered := 0
	for _, m := range msgs {
		now := time.Now()
		if !nudgeRetryDue(m, now) {
			continue
		}
		// Re-check the gate each iteration: a settle gap (~1s/message) is long
		// enough for the recipient to enter a human-input dialog or for its
		// pane to die mid-batch. Stopping BEFORE the claim leaves the rest of
		// the backlog undelivered for the next drain, rather than claiming a
		// message we can no longer safely deliver.
		if !canDeliver() {
			slog.Debug("flush: pausing batch; recipient no longer deliverable",
				"target", label, "remaining", len(msgs)-delivered)
			break
		}
		token, ok, err := db.ClaimAgentMessageNudge(m.ID, now)
		if err != nil {
			slog.Warn("flush: claim failed", "error", err, "msg_id", m.ID)
			continue
		}
		if !ok {
			// Another drain got there first. Skip.
			continue
		}
		// The periodic orphan reaper must not recycle a claim still owned by
		// this daemon. Session selection is a bounded sequence of probes but
		// may span many historical rows; process-local ownership closes the
		// lease-expiry window without weakening restart recovery.
		registerActiveNudge(m.ID, token)
		nudge, consumed := messageNudgeTextFor(m)
		completed := func() bool {
			defer unregisterActiveNudge(m.ID, token)
			if !send(m, nudge) {
				if released, rerr := db.ReleaseAgentMessageNudge(m.ID, token); rerr != nil || !released {
					slog.Warn("flush: failed to release nudge claim",
						"error", rerr, "released", released, "msg_id", m.ID)
				}
				slog.Warn("flush: nudge failed; queued for retry",
					"msg_id", m.ID, "to", m.ToConv,
					"attempt", m.NudgeAttempts+1,
					"retry_in", nudgeRetryDelay(m.NudgeAttempts+1))
				return false
			}
			stamped, err := db.CompleteAgentMessageNudgeState(m.ID, token, time.Now(), consumed)
			if err != nil || !stamped {
				// The pane may have received the nudge, but without the durable
				// completion stamp we must preserve at-least-once semantics. Release
				// our token if it is still ours; a later retry may duplicate the
				// bracket, which is safe because it names the stable message id.
				_, _ = db.ReleaseAgentMessageNudge(m.ID, token)
				slog.Warn("flush: nudge landed but completion stamp failed; queued for retry",
					"error", err, "completed", stamped, "msg_id", m.ID)
				return false
			}
			return true
		}()
		if !completed {
			continue
		}
		delivered++
	}
	if delivered > 0 {
		slog.Info("flush: delivered queued nudges", "target", label, "count", delivered)
	}
	return delivered
}

func nudgeRetryDue(m *db.AgentMessage, now time.Time) bool {
	if m.NudgeAttempts <= 0 || m.NudgeAttemptedAt.IsZero() {
		return true
	}
	return !now.Before(m.NudgeAttemptedAt.Add(nudgeRetryDelay(m.NudgeAttempts)))
}

func nudgeRetryDelay(attempts int) time.Duration {
	if attempts <= 0 || nudgeRetryBase <= 0 {
		return 0
	}
	d := nudgeRetryBase
	for i := 1; i < attempts && d < nudgeRetryMax; i++ {
		if d > nudgeRetryMax/2 {
			return nudgeRetryMax
		}
		d *= 2
	}
	if d > nudgeRetryMax {
		return nudgeRetryMax
	}
	return d
}

// realFlushSender is the production sender for the exact-conv drain. Looks
// up an alive tmux session for the message's recorded ToConv and types the
// nudge into its CC pane. Returns false (no error) if no alive session is
// found — the message stays in the inbox; the recipient will see it on next
// `inbox ls`.
func realFlushSender(m *db.AgentMessage, nudge string) bool {
	return sendNudgeBracket(m.ToConv, m.ID, nudge)
}

// sendNudgeBracket finds an alive tmux session for toConv and sends
// the bracketed nudge for msgID. Shares injectTextAndSubmit with
// lifecycle/ephemeral injectors — see that helper for why the
// text and the submit Enter are split with a sleep.
//
// Caller is responsible for marking delivered_at; this function
// only does the tmux work.
func sendNudgeBracket(toConv string, msgID int64, nudge string) bool {
	sess := pickNudgeSession(toConv)
	if sess == nil || isAwaitingHumanInput(sess.Status) {
		return false
	}
	// This recheck belongs to the exact row selected for injection: the
	// pre-claim gate may have observed a different live session, or this pane
	// may have entered a human-input dialog meanwhile. A narrow TOCTOU window
	// remains while we wait for the pane lock; closing that would require the
	// injection primitive itself to understand persisted session status.
	if err := injectTextAndSubmit(sess.TmuxSession+":0.0", nudge); err != nil {
		slog.Warn("nudge bracket failed", "error", err, "tmux", sess.TmuxSession)
		return false
	}
	return true
}

// pickNudgeSession returns the most-recent row whose tmux pane answers the
// timeout-bounded delivery liveness probe. Keeping the gate and sender on one
// selector ensures the status checked before a claim belongs to the pane the
// nudge will target afterward.
func pickNudgeSession(convID string) *db.SessionRow {
	sess, _ := probeNudgeSession(convID)
	return sess
}

type nudgeSessionProbe int

const (
	nudgeSessionOffline nudgeSessionProbe = iota
	nudgeSessionAlive
	nudgeSessionIndeterminate
)

// probeNudgeSession distinguishes a positively absent pane from a probe that
// failed before it could establish liveness. Only the former is safe for the
// irreversible offline-notification suppression policy.
func probeNudgeSession(convID string) (*db.SessionRow, nudgeSessionProbe) {
	candidates, err := db.FindSessionsByConvID(convID)
	if err != nil {
		return nil, nudgeSessionIndeterminate
	}
	indeterminate := false
	for _, c := range candidates {
		if c.TmuxSession == "" {
			continue
		}
		switch probeNudgeTmuxSession(c.TmuxSession) {
		case nudgeSessionAlive:
			return c, nudgeSessionAlive
		case nudgeSessionIndeterminate:
			indeterminate = true
		}
	}
	if indeterminate {
		return nil, nudgeSessionIndeterminate
	}
	return nil, nudgeSessionOffline
}

// probeNudgeTmuxSession is the delivery-specific liveness probe. It mirrors
// session.IsTmuxSessionAlive's exact target, but runs under the same deadline
// as send-keys so a stuck has-session cannot hold nudgeState.running forever.
func probeNudgeTmuxSession(sessionName string) nudgeSessionProbe {
	err := runTmuxCommand("has-session", "-t", clcommon.ExactTarget(sessionName))
	if err == nil {
		return nudgeSessionAlive
	}
	if errors.Is(err, errTmuxCommandTimeout) {
		slog.Warn("nudge liveness probe timed out", "tmux", sessionName, "error", err)
		return nudgeSessionIndeterminate
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nudgeSessionOffline
	}
	slog.Warn("nudge liveness probe failed", "tmux", sessionName, "error", err)
	return nudgeSessionIndeterminate
}
