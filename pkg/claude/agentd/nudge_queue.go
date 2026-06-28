package agentd

import (
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Async, per-target nudge delivery (JOH-310).
//
// The durable queue IS the set of undelivered agent_messages rows
// (delivered_at=''); this dispatcher owns the agentd-side worker that drains
// them, so a SENDER's request never blocks on tmux — it inserts the row,
// enqueues the recipient here, and returns immediately with the queue depth.
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
