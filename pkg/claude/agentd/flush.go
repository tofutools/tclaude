package agentd

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
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
	go func() {
		_ = flush(convID, realFlushSender)
	}()
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
// the bracketed nudge for msgID. Shared between the flush path and
// (a future) refactor of nudgeIfAlive.
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
	nudge := fmt.Sprintf(
		"[system: new agent message #%d for you. fetch with: tclaude agent inbox read %d]",
		msgID, msgID,
	)
	target := sess.TmuxSession + ":0.0"
	if err := clcommon.TmuxCommand("send-keys", "-t", target, nudge, "Enter").Run(); err != nil {
		slog.Warn("nudge bracket failed (text)", "error", err, "tmux", sess.TmuxSession)
		return false
	}
	if err := clcommon.TmuxCommand("send-keys", "-t", target, "Enter").Run(); err != nil {
		slog.Warn("nudge bracket failed (submit)", "error", err, "tmux", sess.TmuxSession)
		return false
	}
	return true
}
