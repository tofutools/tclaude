package agentd

import (
	"log/slog"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// flushSender delivers a single undelivered nudge. The production
// implementation is realFlushSender; tests inject a stub so the
// claim/iteration logic in `flush` can be exercised without a real
// tmux session.
type flushSender func(m *db.AgentMessage) (delivered bool)

// flushMinInterval is how often a single conv-id is allowed to
// trigger a flush. Five seconds keeps the cost amortised when an
// agent fires a burst of CLI calls back-to-back, while still being
// short enough that the human perceives "queue clears as soon as
// the agent comes back."
const flushMinInterval = 5 * time.Second

var (
	flushDebounceMu sync.Mutex
	flushDebounce   = map[string]time.Time{}
)

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
	return flushQueue("agent:"+agentID,
		func() ([]*db.AgentMessage, error) { return db.ListUndeliveredForAgent(agentID) },
		func() bool { return deliverablePane(head) },
		func(m *db.AgentMessage) bool { return sendNudgeBracket(head, m.ID) })
}

// flush drains the exact-conv queue for convID: undelivered messages that
// must stick to this specific conv — prev-gen-pinned (pin_gen=1) and
// non-actor mail (to_agent=”) — as opposed to head-following agent mail,
// which flushAgent owns. The flushSender seam stays so tests can stub the
// tmux side. Returns the number claimed.
func flush(convID string, send flushSender) int {
	return flushQueue("conv:"+convID,
		func() ([]*db.AgentMessage, error) { return db.ListUndeliveredForExactConv(convID) },
		func() bool { return deliverablePane(convID) },
		send)
}

// flushQueue is the shared drain core: list the queue, hold the WHOLE batch
// unless the delivery pane is reachable RIGHT NOW (alive + not blocked on a
// human), else claim each message atomically (so concurrent drains don't
// double-nudge) and deliver. Returns the number successfully claimed.
//
// The canDeliver gate runs BEFORE ClaimAgentMessageDelivery: a claim stamps
// delivered_at, so claiming a message we then can't deliver — the recipient is
// offline (the async send path routinely hits this) or mid human-input dialog
// (JOH-308) — would consume it without ever nudging. Returning early leaves
// every row undelivered for the next drain (the recipient's next request, or
// the reaper backstop) once it is reachable again.
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
	claimed := 0
	for _, m := range msgs {
		ok, err := db.ClaimAgentMessageDelivery(m.ID)
		if err != nil {
			slog.Warn("flush: claim failed", "error", err, "msg_id", m.ID)
			continue
		}
		if !ok {
			// Another drain got there first. Skip.
			continue
		}
		claimed++
		if !send(m) {
			slog.Debug("flush: nudge failed; recipient must use inbox ls",
				"msg_id", m.ID, "to", m.ToConv)
		}
	}
	if claimed > 0 {
		slog.Info("flush: delivered queued nudges", "target", label, "count", claimed)
	}
	return claimed
}

// realFlushSender is the production sender for the exact-conv drain. Looks
// up an alive tmux session for the message's recorded ToConv and types the
// nudge into its CC pane. Returns false (no error) if no alive session is
// found — the message stays in the inbox; the recipient will see it on next
// `inbox ls`.
func realFlushSender(m *db.AgentMessage) bool {
	return sendNudgeBracket(m.ToConv, m.ID)
}

// sendNudgeBracket finds an alive tmux session for toConv and sends
// the bracketed nudge for msgID. Shares injectTextAndSubmit with
// nudgeIfAlive / injectSlashCommand — see that helper for why the
// text and the submit Enter are split with a sleep.
//
// Caller is responsible for marking delivered_at; this function
// only does the tmux work.
func sendNudgeBracket(toConv string, msgID int64) bool {
	candidates, err := db.FindSessionsByConvID(toConv)
	if err != nil {
		return false
	}
	var sess *db.SessionRow
	for _, c := range candidates {
		if c.TmuxSession == "" {
			continue
		}
		if session.IsTmuxSessionAlive(c.TmuxSession) {
			sess = c
			break
		}
	}
	if sess == nil {
		return false
	}
	nudge := messageNudgeText(msgID)
	if err := injectTextAndSubmit(sess.TmuxSession+":0.0", nudge); err != nil {
		slog.Warn("nudge bracket failed", "error", err, "tmux", sess.TmuxSession)
		return false
	}
	return true
}
