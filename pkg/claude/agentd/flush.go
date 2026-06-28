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
// keypress) and runs the actual flush in a goroutine so the request
// it piggy-backed on doesn't pay any latency.
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
	// goBackground, not a bare `go`: this flush touches sqlite + tmux and
	// outlives the request that triggered it, so a flow test must be able to
	// drain it (WaitForBackgroundForTest) before its cleanup restores the
	// clcommon.Default tmux swap and tears down $HOME.
	goBackground(func() {
		_ = flush(convID, realFlushSender)
	})
}

// FlushUndeliveredForTest runs the production flush for convID
// synchronously — the same flush + realFlushSender that maybeFlushUndelivered
// drives — bypassing the per-conv debounce and the background goroutine so a
// flow test can assert delivery (and the awaiting-human-input hold) without
// racing a goroutine. Returns the number of messages claimed this call.
//
// It exists only because BuildHandlerForTest serves buildMux() WITHOUT the
// withIdentity middleware that triggers flush in production, so the
// resume-delivery path has no request-driven entry point under test. Not a
// subprocess mock and not reachable from production — a sanctioned …ForTest
// entry into the real flush path. See CLAUDE.md "In-process session seams".
func FlushUndeliveredForTest(convID string) int {
	return flush(convID, realFlushSender)
}

// flush walks every undelivered message addressed to convID, claims
// each one atomically (so concurrent flushes don't double-nudge),
// and asks `send` to deliver. Returns the number of messages
// successfully claimed — regardless of whether the send itself
// landed (a vanished tmux session is logged but not retried).
//
// A claim failure (ErrLockBusy / IO error) is logged and the
// iteration continues; we want best-effort delivery of as many
// messages as possible.
func flush(convID string, send flushSender) int {
	msgs, err := db.ListUndeliveredAgentMessagesFor(convID)
	if err != nil {
		slog.Warn("flush: list undelivered failed", "error", err, "conv", convID)
		return 0
	}
	if len(msgs) == 0 {
		return 0
	}
	// Hold the whole queue if the recipient's pane is blocked on a human
	// (awaiting_input / awaiting_permission). Crucially this gate runs
	// BEFORE ClaimAgentMessageDelivery below: a claim stamps delivered_at,
	// so claiming-then-failing-to-send would mark a held message delivered
	// and it would never be retried. By returning early we leave every row
	// undelivered for the next flush — the recipient's next request, or the
	// reaper backstop — once they are back to working/idle. See
	// heldForHumanInput.
	if heldForHumanInput(convID) {
		slog.Debug("flush: holding queued mail; recipient awaiting human input",
			"conv", convID, "queued", len(msgs))
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
			// Another goroutine got there first. Skip.
			continue
		}
		claimed++
		if !send(m) {
			slog.Debug("flush: nudge failed; recipient must use inbox ls",
				"msg_id", m.ID, "to", m.ToConv)
		}
	}
	if claimed > 0 {
		slog.Info("flush: delivered queued nudges", "conv", convID, "count", claimed)
	}
	return claimed
}

// realFlushSender is the production sender. Looks up an alive tmux
// session for the recipient and types the nudge into its CC pane.
// Returns false (no error) if no alive session is found — the
// message stays in the inbox; the recipient will see it on next
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
