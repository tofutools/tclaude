package agentd

import (
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const regularAgentMessageQueueLimit = 10

// Async, per-target nudge delivery (JOH-310).
//
// The nudge queue is the set of undelivered agent_messages rows. Regular sends
// also have a separate unprocessed backlog used for sender backpressure: an
// offline nudge can be suppressed while the durable unread row still counts.
//
// Delivery is serialized per TARGET, and the target is the stable agent_id for
// normal (head-following) mail — NOT the conv-id. Keying on the actor means a
// message queued before the recipient reincarnated / ran /clear is still
// delivered, to whatever generation is live at drain time (see flushAgent).
// Prev-gen-pinned and non-actor mail is keyed by conv-id instead (flushExactConv).
//
// Two triggers feed the same per-target worker — a SEND (enqueueDeliveryForConv
// from the send handlers) and the recipient's own requests (maybeFlushUndelivered,
// the offline-catchup path) — and both coalesce through one worker per target.
// The hold-on-human-input gate (JOH-308) lives in the flush* drains, so a
// recipient mid-dialog still holds its queue under the async model.

// nudgeTarget identifies one delivery queue: exactly one of agentID / convID is
// set. agentID → head-following drain (flushAgent); convID → exact-conv drain
// (flushExactConv, for pinned / non-actor mail).
type nudgeTarget struct {
	agentID string
	convID  string
}

// nudgeWork is the coalescing single-flight state for one target: while a drain
// is running, a further enqueue sets `again` so exactly one more drain follows.
// The map entry is removed when the worker goes idle, so there is no per-target
// goroutine or map leak.
type nudgeWork struct {
	target  nudgeTarget
	running bool
	again   bool
}

var (
	nudgeMu    sync.Mutex
	nudgeState = map[string]*nudgeWork{}
)

// queueAgentMessage is the single post-startup message entry point: persist
// the durable inbox row first, then hand its recipient to the async delivery
// dispatcher. Callers choose attribution, subject, body, audiences, and other
// message policy on the row; they do not select a tmux transport.
//
// Startup briefings deliberately use db.InsertAgentMessage directly because
// their first delivery is owned by the harness launch seed/welcome. Clone and
// reincarnate handoffs also insert directly, then enqueue after their ordered
// post-spawn rename has settled.
func queueAgentMessage(m *db.AgentMessage) (int64, error) {
	id, err := db.InsertAgentMessage(m)
	if err != nil {
		return 0, err
	}
	enqueueDeliveryForConv(m.ToConv)
	return id, nil
}

// queueRegularAgentMessage is the bounded entry point for human/agent
// one-shot sends. Internal lifecycle, cron, process, and coordination traffic
// keeps using queueAgentMessage so queue pressure cannot break correctness.
func queueRegularAgentMessage(m *db.AgentMessage) (id int64, pending int, err error) {
	id, pending, err = db.InsertAgentMessageBounded(m, regularAgentMessageQueueLimit)
	if err == nil {
		enqueueDeliveryForConv(m.ToConv)
	}
	return id, pending, err
}

func queueRegularAgentMessageWithAttachments(m *db.AgentMessage, attachments []db.AgentMessageAttachment) (id int64, pending int, err error) {
	id, pending, err = db.InsertAgentMessageWithAttachmentsBounded(m, attachments, regularAgentMessageQueueLimit)
	if err == nil {
		enqueueDeliveryForConv(m.ToConv)
	}
	return id, pending, err
}

func agentMessageQueueFull(err error) (*db.AgentMessageQueueFullError, bool) {
	var full *db.AgentMessageQueueFullError
	ok := errors.As(err, &full)
	return full, ok
}

func queueFullHint(pending, limit int) string {
	return fmt.Sprintf("target message backlog is full (%d/%d unprocessed regular messages); no message was queued. Wait for the target to process or read pending messages, then retry", pending, limit)
}

func writeQueueFull(w http.ResponseWriter, target string, full *db.AgentMessageQueueFullError) {
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"error":     queueFullHint(full.Pending, full.Limit),
		"code":      "queue_full",
		"target":    target,
		"pending":   full.Pending,
		"limit":     full.Limit,
		"retryable": true,
	})
}

// enqueueDeliveryForConv routes a recipient conv-id to the right drain(s): the
// agent-keyed drain when the conv is an enrolled agent (head-following mail),
// AND the conv-keyed drain (pinned-to-this-conv / non-actor mail). It is the
// single entry used by every trigger — the send handlers, maybeFlushUndelivered,
// and the post-reincarnate/clone/cron nudges — so all of them go async and
// coalesce. Non-blocking: it returns as soon as the workers are signalled.
func enqueueDeliveryForConv(convID string) {
	if convID == "" {
		return
	}
	if agentID, _ := db.AgentIDForConv(convID); agentID != "" {
		enqueueAgentNudge(agentID)
	}
	enqueueConvNudge(convID)
}

// EnqueueDeliveryForTest arms the production dispatcher for a row inserted by
// a flow test. It lets delivery-infrastructure scenarios seed unbounded
// internal messages without pretending they came through a regular-send API.
func EnqueueDeliveryForTest(convID string) {
	enqueueDeliveryForConv(convID)
}

// enqueueAgentNudge signals the head-following drain for an agent.
func enqueueAgentNudge(agentID string) {
	if agentID == "" {
		return
	}
	enqueueNudge("agent:"+agentID, nudgeTarget{agentID: agentID})
}

// enqueueConvNudge signals the exact-conv drain (pinned / non-actor mail).
func enqueueConvNudge(convID string) {
	if convID == "" {
		return
	}
	enqueueNudge("conv:"+convID, nudgeTarget{convID: convID})
}

// enqueueNudge starts (or re-arms) the single-flight worker for key.
func enqueueNudge(key string, t nudgeTarget) {
	nudgeMu.Lock()
	w := nudgeState[key]
	if w == nil {
		w = &nudgeWork{target: t}
		nudgeState[key] = w
	}
	if w.running {
		// A drain is already in flight; tell it to loop once more so it
		// picks up whatever was just inserted, then return without blocking.
		w.again = true
		nudgeMu.Unlock()
		return
	}
	w.running = true
	nudgeMu.Unlock()
	// goBackground (not a bare `go`): the drain touches sqlite + tmux and
	// outlives the request that triggered it, so flow tests can drain it via
	// WaitForBackgroundForTest before teardown.
	goBackground(func() { drainNudgeLoop(key) })
}

// drainNudgeLoop runs the target's drain, repeating while `again` was set during
// the previous pass, then removes the idle entry. Exactly one of these runs per
// target at a time (the running flag), so a target's nudges are delivered
// single-file even under concurrent senders.
func drainNudgeLoop(key string) {
	for {
		nudgeMu.Lock()
		w := nudgeState[key]
		if w == nil {
			nudgeMu.Unlock()
			return
		}
		t := w.target
		// Clear before draining: an enqueue arriving during the drain re-sets
		// `again`, guaranteeing another pass — no wakeup is lost.
		w.again = false
		nudgeMu.Unlock()

		drainNudgeTarget(t)

		nudgeMu.Lock()
		if w = nudgeState[key]; w != nil && w.again {
			nudgeMu.Unlock()
			continue
		}
		delete(nudgeState, key)
		nudgeMu.Unlock()
		return
	}
}

// drainNudgeTarget delivers one target's queued nudges, one at a time.
func drainNudgeTarget(t nudgeTarget) {
	if t.agentID != "" {
		flushAgent(t.agentID)
		return
	}
	if t.convID != "" {
		flush(t.convID, realFlushSender)
	}
}

// queueDepthFor returns how many undelivered nudges sit in the recipient's queue
// (including the one the caller just inserted) — the "pending" depth the send
// response reports so the sender sees the queue without blocking on delivery. It
// mirrors the drain partition: a pinned / non-actor message counts against the
// exact-conv queue, everything else against the agent queue.
func queueDepthFor(finalConv string, pinGen bool) int {
	if !pinGen {
		if agentID, _ := db.AgentIDForConv(finalConv); agentID != "" {
			n, _ := db.CountUndeliveredForAgent(agentID)
			return n
		}
	}
	n, _ := db.CountUndeliveredForExactConv(finalConv)
	return n
}
