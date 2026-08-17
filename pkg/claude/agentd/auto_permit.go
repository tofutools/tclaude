package agentd

import (
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/paneinput"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// auto_permit.go is the daemon half of auto-permit: the sweep that notices an
// opted-in agent sitting on a NAMED permission prompt and presses the accept
// key for it, then records what it answered.
//
// The three gates an auto-answer has to pass, in order:
//
//  1. Consent. The agent must carry an opt-in for the condition in
//     agent_auto_permit — off by default, written only through the
//     permission-gated CLI/API (`self.auto-permit`, or the manager pattern).
//  2. Condition. The session must be in awaiting_permission on the harness the
//     condition names, with a status_detail the condition matches.
//  3. Evidence. The pane, captured under the same lock the keystroke is sent
//     under, must show that condition's dialog live on screen.
//
// Gate 3 is what makes this safe rather than merely convenient. Status is a DB
// projection written by a hook; it says an agent was waiting on SOMETHING a
// moment ago. Reading the pane immediately before pressing, inside the pane
// injection lock, is what establishes that the prompt being answered is the one
// consented to and not, say, a Bash prompt that arrived a tick later.
//
// Nothing here can answer a prompt no condition names, and no condition is a
// wildcard. An operator who wants blanket acceptance should ask for it honestly
// with `--dangerously-skip-permissions`.

// autoPermitSweepInterval is how often the sweep looks for an answerable
// prompt. A prompt is a human-blocking stall, so this is deliberately much
// tighter than the housekeeping sweeps; the cost of a tick with no opted-in
// agent is a single small indexed SELECT, and the sweep returns before touching
// tmux at all when nobody has opted in.
const autoPermitSweepInterval = 2 * time.Second

// autoPermitDwell is how long a session must have been in awaiting_permission
// before it is answered. It buys the TUI time to actually paint the dialog
// after the hook stamps the status, so the pane read has something to find
// rather than racing the render and giving up for a tick.
const autoPermitDwell = 750 * time.Millisecond

// autoPermitCooldown is how long the sweep leaves a session alone after
// answering it. The pane-evidence gate already prevents a second press (an
// accepted dialog is gone from the pane), so this is the belt to that braces:
// it bounds how often one session can be pressed even if a pane somehow keeps
// rendering a matching dialog.
const autoPermitCooldown = 15 * time.Second

// autoPermitKeyGap separates the keys of a multi-key accept sequence. Matches
// the menu-walk step delay used elsewhere for the same reason — a TUI needs a
// moment between presses to move a highlight. Single-key conditions (every
// condition today) never wait.
var autoPermitKeyGap = 250 * time.Millisecond

// autoPermitState remembers which sessions were recently answered, so the
// cooldown survives across ticks. In-memory (not a DB column) deliberately: the
// state is a debounce, not a record — the RECORD is the audit row — and the
// worst a daemon restart can do is allow one extra press, which still has to
// pass the pane-evidence gate to happen at all.
type autoPermitState struct {
	mu         sync.Mutex
	answeredAt map[string]time.Time // session id → last auto-answer
}

func newAutoPermitState() *autoPermitState {
	return &autoPermitState{answeredAt: map[string]time.Time{}}
}

// the daemon's single sweep state, shared by the goroutine and the ForTest
// entry point.
var autoPermits = newAutoPermitState()

// startAutoPermitSweep spins up the auto-permit loop in its own goroutine. It
// returns when stop is closed (the daemon-wide quit channel), sharing cronStop
// with the other housekeeping sweeps.
func startAutoPermitSweep(stop <-chan struct{}) {
	go func() {
		t := time.NewTicker(autoPermitSweepInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-t.C:
				runAutoPermitTick(now)
			}
		}
	}()
}

// RunAutoPermitTickForTest drives one sweep from a flow test.
func RunAutoPermitTickForTest(now time.Time) { runAutoPermitTick(now) }

func runAutoPermitTick(now time.Time) {
	runAutoPermitTickWith(now, autoPermits)
}

// runAutoPermitTickWith is the testable core: the debounce state is passed in so
// a test can drive the cadence with a fresh clock.
func runAutoPermitTickWith(now time.Time, st *autoPermitState) {
	optIns, err := db.ListAllAutoPermits()
	if err != nil {
		slog.Warn("auto-permit: opt-in read failed", "error", err, "module", "agentd")
		return
	}
	if len(optIns) == 0 {
		// The overwhelmingly common case: nobody has opted in, so the sweep
		// costs one small SELECT and never looks at a session or a pane.
		return
	}
	rows, err := db.ListSessions()
	if err != nil {
		slog.Warn("auto-permit: session listing failed", "error", err, "module", "agentd")
		return
	}
	for _, row := range rows {
		if row == nil || row.Status != session.StatusAwaitingPermission {
			continue
		}
		if now.Sub(row.UpdatedAt) < autoPermitDwell {
			continue // the dialog may not be painted yet; reconsider next tick
		}
		if st.recentlyAnswered(row.ID, now) {
			continue // answered moments ago; leave it to settle
		}
		agentID, err := db.AgentIDForConv(row.ConvID)
		if err != nil || agentID == "" {
			continue // not an enrolled agent — nothing consented for it
		}
		cond := autoPermitConditionFor(optIns[agentID], row)
		if cond == nil {
			continue
		}
		if answerAutoPermit(row, agentID, cond, now) {
			st.markAnswered(row.ID, now)
		}
	}
	st.prune(now)
}

// autoPermitConditionFor picks the condition an agent has consented to that
// matches this session's awaiting state, or nil. Consent is checked FIRST so a
// non-opted-in agent's prompt is never even classified.
func autoPermitConditionFor(consented map[string]bool, row *db.SessionRow) *autoPermitCondition {
	if len(consented) == 0 {
		return nil
	}
	for name := range consented {
		cond := lookupAutoPermitCondition(name)
		if cond == nil {
			// A stored name this build does not know (an older opt-in, a
			// retired condition). Inert, exactly like an unregistered
			// permission slug.
			continue
		}
		if cond.matchesDetail(row.Harness, row.StatusDetail) {
			return cond
		}
	}
	return nil
}

// answerAutoPermit does the pane-evidence check and the keystroke, then records
// the answer. A pane that does not show the condition's dialog is a no-op — the
// common non-error outcome when the status projection is stale or the dialog
// has already been dealt with by the human — and reports false so the session
// stays eligible on the next tick rather than sitting out a cooldown for a
// press that never happened.
func answerAutoPermit(row *db.SessionRow, agentID string, cond *autoPermitCondition, now time.Time) bool {
	if row.TmuxSession == "" {
		return false
	}
	target := row.TmuxSession + ":0.0"
	excerpt, answered, err := injectAutoPermitAccept(target, cond)
	if err != nil {
		slog.Warn("auto-permit: accept keystroke failed", "error", err,
			"condition", cond.Name, "tmux", row.TmuxSession, "conv", row.ConvID,
			"module", "agentd")
		return false
	}
	if !answered {
		slog.Debug("auto-permit: pane does not show the consented dialog; leaving it for the human",
			"condition", cond.Name, "tmux", row.TmuxSession, "conv", row.ConvID,
			"module", "agentd")
		return false
	}
	slog.Info("auto-permit: answered a permission prompt on the operator's behalf",
		"condition", cond.Name, "conv", row.ConvID, "session", row.ID, "module", "agentd")
	recordAutoPermitAnswer(row, agentID, cond, excerpt, now)
	return true
}

// injectAutoPermitAccept captures the pane and, only if it shows the condition's
// dialog, sends the accept keys — both inside the pane injection lock, so no
// nudge or peer message can slip between the evidence and the keystroke. Returns
// the pane excerpt kept for the audit row and whether keys were sent.
func injectAutoPermitAccept(tmuxTarget string, cond *autoPermitCondition) (string, bool, error) {
	mu := paneInjectLock(injectLockKey(tmuxTarget))
	if err := acquirePaneInjectLock(mu); err != nil {
		return "", false, err
	}
	defer mu.Unlock()
	var excerpt string
	sent := false
	err := paneinput.WithLock(tmuxTarget, paneinput.Options{
		Run:         runTmuxCommand,
		LockTimeout: paneInjectLockTimeout,
		LockID:      tmuxTarget,
	}, func(run paneinput.Runner, target string) error {
		pane := captureAutoPermitPane(target)
		if !cond.matchesPane(pane) {
			return nil
		}
		excerpt = formatPaneScreenTail(pane)
		for i, key := range cond.AcceptKeys {
			if i > 0 {
				time.Sleep(autoPermitKeyGap)
			}
			if err := run("send-keys", "-t", target, key); err != nil {
				return err
			}
		}
		sent = true
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return excerpt, sent, nil
}

// captureAutoPermitPane reads a pane's visible text. Plain (no -e): the
// condition patterns match the words the dialog renders, and colour escapes
// would only be noise between them. -J joins soft-wrapped lines so a long
// worktree path can't split the question text mid-phrase. A failed capture
// yields "", which fails the evidence gate — the safe direction.
func captureAutoPermitPane(paneTarget string) string {
	out, err := tmuxOutputWithTimeout("capture-pane", "-p", "-J", "-t", paneTarget)
	if err != nil {
		return ""
	}
	return string(out)
}

// recordAutoPermitAnswer writes the answer to the audit log as a system actor.
// This is the operator's after-the-fact view of what was approved on their
// behalf: it lands in the same dashboard Audit tab as every other daemon-side
// action, with the condition and the pane excerpt that satisfied it.
//
// A failed audit write is logged, not fatal — the keystroke has already been
// sent, and losing the trail must not also lose the log line.
func recordAutoPermitAnswer(row *db.SessionRow, agentID string, cond *autoPermitCondition, excerpt string, now time.Time) {
	label := agent.FreshTitle(row.ConvID)
	if label == agent.UnknownTitle {
		label = short8(row.ConvID)
	}
	detail := cond.Name
	if d := strings.TrimSpace(row.StatusDetail); d != "" {
		detail += " — prompt: " + d
	}
	if excerpt != "" {
		detail += " — pane: " + excerpt
	}
	if _, err := db.InsertAuditLog(db.AuditLogEntry{
		At:          now,
		ActorKind:   db.AuditActorSystem,
		ActorLabel:  "tclaude",
		Verb:        auditVerbAutoPermitAnswer,
		TargetConv:  row.ConvID,
		TargetAgent: agentID,
		TargetLabel: label,
		Detail:      auditClip(detail, 400),
		Status:      http.StatusOK,
		Source:      db.AuditSourceAutoPermit,
		SessionID:   row.ID,
		TmuxSession: row.TmuxSession,
	}); err != nil {
		slog.Warn("auto-permit: failed to record the answered prompt",
			"error", err, "condition", cond.Name, "conv", row.ConvID, "module", "agentd")
	}
}

// auditVerbAutoPermitAnswer is the audit verb for one auto-answered prompt.
// Stable: the operator filters on it, and `tclaude agent auto-permit log` reads
// it back.
const auditVerbAutoPermitAnswer = "auto-permit.answer"

// recentlyAnswered reports whether a session is still inside the cooldown that
// follows an actual press.
func (st *autoPermitState) recentlyAnswered(sessionID string, now time.Time) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	last, seen := st.answeredAt[sessionID]
	return seen && now.Sub(last) < autoPermitCooldown
}

// markAnswered starts a session's cooldown. Only a real press marks: an attempt
// that found no matching dialog must leave the session eligible, so a dialog
// that paints a beat later is still answered promptly.
func (st *autoPermitState) markAnswered(sessionID string, now time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.answeredAt[sessionID] = now
}

// prune drops cooldown entries that have expired, so the map stays the size of
// "sessions answered in the last few seconds" rather than growing for the life
// of the daemon.
func (st *autoPermitState) prune(now time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for id, at := range st.answeredAt {
		if now.Sub(at) >= autoPermitCooldown {
			delete(st.answeredAt, id)
		}
	}
}
