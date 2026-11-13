package agentd

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/paneinput"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// injectSlashCommandOn finds an alive tmux session for convID and dispatches
// the given slash-command line through the selected harness control channel.
// If followUp is non-empty, it is sent as a fresh prompt right after the
// slash submit. Returns true on successful delivery.
//
// reason is a short human-readable label of WHAT triggered this injection
// (e.g. "self-compact", "compact (caller=ab12cd34)", "soft-exit",
// "rename"). It is recorded in the success log line so that every
// slash command tclaude types into a pane — most importantly /compact —
// has an audit trail in ~/.tclaude/output.log. send-keys is the one
// channel through which tclaude can make a pane do something the agent
// did not type itself, so "where did that /compact come from?" must be
// answerable after the fact from the log alone.
//
// Note: when used with /compact, the follow-up bytes queue in the pty
// until CC resumes reading after the slash command settles. We don't
// wait for the slash to complete — there's no clean way to detect it
// without a hook. The follow-up may land in a still-busy textarea on
// unlucky timing; agents that depend on tight ordering should poll
// context-info and submit the follow-up themselves once compact has
// resolved.
//
// The per-pane lock (JOH-310) makes the slash submit and the follow-up
// each atomic, but NOT the pair: another injector can take the pane
// between them. That is consistent with the best-effort follow-up
// ordering described above and is not a regression.
// The explicit delivery channel exists because an override applied only at the
// top is not an override. The
// first version of the spawn's pane fallback set the channel at deliverRenameOn
// and stopped there; the rename it forced to the pane was then refused by
// dispatchSlashCommand, which asks copilotAPIDriven again on its own account
// and has no typed RPC for a rename token. See [deliveryChannel].
func injectSlashCommandOn(
	convID, line, followUp, reason string, channel deliveryChannel,
) bool {
	transport, _ := dispatchSlashCommandOn(convID, line, followUp, reason, channel)
	return transport != slashTransportNone
}

// slashTransport names the channel a lifecycle command was ACTUALLY delivered
// over, as reported by the branch that delivered it.
//
// It exists because the obvious alternative — deciding what to tell the caller
// from the same condition that selected the branch — is the proxy-value mistake
// this series keeps making: TCL-1053 shipped an echo that reported send-keys
// while the pane was being driven over the API, derived from a value that was
// correct about the launch and wrong about the delivery. A transport that comes
// back FROM the delivery cannot disagree with it.
type slashTransport string

const (
	// slashTransportNone means nothing was delivered.
	slashTransportNone        slashTransport = ""
	slashTransportSendKeys    slashTransport = "send-keys"
	slashTransportOpenCodeAPI slashTransport = "opencode-api"
	slashTransportCopilotAPI  slashTransport = "copilot-api"
	slashTransportCodexAPI    slashTransport = "codex-app-server"
	slashTransportPiDrive     slashTransport = "pi-drive"
)

// slashFailure names WHY nothing was delivered, and — like slashTransport — it
// is reported BY the branch that failed rather than re-derived afterwards.
//
// Splitting the failure out was the second half of the same fix. The first
// version reported the transport honestly on success and then, on failure,
// re-asked "is this conversation on the Copilot API drive?" to pick an error
// code — after the dispatch, from a value that could disagree with what the
// dispatch actually did. Both directions are reachable: a conversation with a
// live handle whose PANE probe failed never enters the Copilot branch at all,
// yet would be told its Copilot API dispatch failed; and a Copilot dispatch that
// failed and then lost its connection would be told it has no tmux session. The
// error code is what callers branch on, so this is not cosmetic.
type slashFailure string

const (
	slashFailureNone slashFailure = ""
	// slashFailureNoPane means no live pane was found for the conversation, so
	// no channel was even selected.
	slashFailureNoPane slashFailure = "no_tmux"
	// slashFailureControl means a managed channel was selected and refused.
	slashFailureControl      slashFailure = "control_unavailable"
	slashFailureStarting     slashFailure = "control_starting"
	slashFailureBusy         slashFailure = "busy"
	slashFailureUnsupported  slashFailure = "unsupported"
	slashFailureDisconnected slashFailure = "disconnected"
	slashFailureSettling     slashFailure = "settling"
)

// dispatchSlashCommand is injectSlashCommand's body, reporting which channel
// carried the command — or, when none did, which step said no.
func dispatchSlashCommand(
	convID, line, followUp, reason string,
) (slashTransport, slashFailure) {
	return dispatchSlashCommandOn(convID, line, followUp, reason, deliveryChannelRouted)
}

// dispatchSlashCommandOn is dispatchSlashCommand with the caller's delivery
// channel. Everything but the spawn's post-init pane fallback passes
// [deliveryChannelRouted] and behaves exactly as before; see [deliveryChannel]
// for what entitles the one exception and why it must never be widened.
func dispatchSlashCommandOn(
	convID, line, followUp, reason string, channel deliveryChannel,
) (slashTransport, slashFailure) {
	sess := aliveSessionForConv(convID)
	if sess == nil {
		return slashTransportNone, slashFailureNoPane
	}
	if sess.Harness == harness.OpenCodeName {
		if injectOpenCodeSlashCommand(sess, line, followUp, reason) {
			return slashTransportOpenCodeAPI, slashFailureNone
		}
		return slashTransportNone, slashFailureControl
	}
	if sess.Harness == harness.PiName {
		piSelected, err := piDriveSelected(convID)
		if err != nil {
			slog.Warn("pi drive posture unreadable; refusing lifecycle pane fallback",
				"error", err, "conv_id", convID, "line", line)
			return slashTransportNone, slashFailureControl
		}
		if piSelected {
			if piDriveSlashCommand(convID, line, followUp, reason) {
				return slashTransportPiDrive, slashFailureNone
			}
			return slashTransportNone, slashFailureControl
		}
	}
	codexSelected := false
	if sess.Harness == harness.CodexName {
		var codexSelectionErr error
		codexSelected, codexSelectionErr = codexAppServerSelected(convID)
		if codexSelectionErr != nil {
			slog.Warn("Codex app-server posture unreadable; refusing lifecycle pane fallback",
				"error", codexSelectionErr, "conv_id", convID, "line", line)
			return slashTransportNone, slashFailureControl
		}
	}
	if channel == deliveryChannelRouted && codexSelected {
		life := harnessForConv(convID).Life
		if life == nil || line != life.CompactCommand() {
			return slashTransportNone, slashFailureUnsupported
		}
		if err := compactCodexAppServerThread(convID, followUp); err != nil {
			slog.Warn("Codex app-server: compaction dispatch failed",
				"error", err, "conv_id", convID, "line", line, "reason", reason)
			return slashTransportNone, codexControlFailure(err)
		}
		slog.Info("Codex app-server: lifecycle command dispatched over RPC",
			"conv_id", convID, "line", line, "reason", reason,
			"tmux_session", sess.TmuxSession, "has_follow_up", followUp != "")
		return slashTransportCodexAPI, slashFailureNone
	}
	// An API-driven Copilot agent takes the typed channel. Never a fallback
	// pair with the keystrokes below: an agent that opted out of the injection
	// sink must not be returned to it one failure at a time, and "the channel
	// is not up" is a refusal rather than permission to type. See
	// copilot_api_drive.go.
	if channel == deliveryChannelRouted && copilotAPIDriven(convID) {
		if dispatchCopilotAPISlashCommand(sess, line, followUp, reason) {
			return slashTransportCopilotAPI, slashFailureNone
		}
		return slashTransportNone, slashFailureControl
	}
	target := sess.TmuxSession + ":0.0"
	if err := injectTextAndSubmit(target, line); err != nil {
		if errors.Is(err, paneinput.ErrAwaitingHumanInput) {
			return slashTransportNone, slashFailureBusy
		}
		slog.Warn("slash-command inject failed", "error", err, "tmux", sess.TmuxSession, "line", line, "reason", reason)
		return slashTransportNone, slashFailureNoPane
	}
	slog.Info("slash-command injected via send-keys",
		"conv_id", convID,
		"line", line,
		"reason", reason,
		"tmux_session", sess.TmuxSession,
		"has_follow_up", followUp != "",
	)
	if followUp != "" {
		if err := injectTextAndSubmit(target, followUp); err != nil {
			if errors.Is(err, paneinput.ErrAwaitingHumanInput) {
				return slashTransportNone, slashFailureBusy
			}
			slog.Warn("slash-command follow-up failed", "error", err, "tmux", sess.TmuxSession)
			return slashTransportNone, slashFailureNoPane
		}
	}
	return slashTransportSendKeys, slashFailureNone
}

// dispatchCopilotAPISlashCommand maps a Copilot lifecycle command onto its
// typed RPC.
//
// The mapping is by lifecycle TOKEN, and the tokens are compile-time constants
// from the harness lifecycle — never caller input — which is the property that
// keeps this a fixed switch rather than a command interpreter. An unmapped
// token fails closed exactly as OpenCode's does: no keystroke fallback, because
// the whole point of an API-connected agent is that it does not have one.
//
// Compaction is the only token that reaches here today. That is not a claim
// about which lifecycle commands exist — it is what this sink carries: rename
// branches earlier, in deliverRename, and soft exit does not come through
// dispatchSlashCommand at all (it goes through injectSoftExitTarget, which
// needs the harness's prefix keys). The soft-exit decision and the measurements
// behind it live where soft exit actually is, next to that branch in
// lifecycle_stop.go.
func dispatchCopilotAPISlashCommand(sess *db.SessionRow, line, followUp, reason string) bool {
	if sess == nil {
		return false
	}
	life := harnessForConv(sess.ConvID).Life
	if life == nil {
		return false
	}
	if line != life.CompactCommand() {
		slog.Warn("copilot API: lifecycle command has no typed RPC mapping; refusing to "+
			"fall back to keystrokes",
			"conv_id", sess.ConvID, "line", line, "reason", reason)
		return false
	}
	if err := compactCopilotAPISession(sess.ConvID, followUp); err != nil {
		slog.Warn("copilot API: compaction dispatch failed",
			"error", err, "conv_id", sess.ConvID, "line", line, "reason", reason)
		return false
	}
	slog.Info("copilot API: lifecycle command dispatched over RPC",
		"conv_id", sess.ConvID, "line", line, "reason", reason,
		"tmux_session", sess.TmuxSession, "has_follow_up", followUp != "")
	return true
}

// injectOpenCodeSlashCommand routes the attached TUI's lifecycle commands
// through the managed server event API. OpenCode 1.18.4 exposes
// tui.command.execute, and its attach client dispatches those events through
// the same command registry as a slash command without depending on prompt
// mode or keybindings.
func injectOpenCodeSlashCommand(sess *db.SessionRow, line, followUp, reason string) bool {
	if sess == nil || openCodeControlInputBlocked(sess.Status) {
		if sess != nil {
			slog.Warn("OpenCode slash-command dispatch deferred until TUI is idle",
				"conv_id", sess.ConvID, "line", line, "reason", reason, "status", sess.Status)
		}
		return false
	}
	if followUp != "" {
		// OpenCode command publication does not acknowledge completion, so an
		// immediate prompt_async cannot preserve compact-then-follow-up order.
		// The HTTP surface rejects this pair with a specific error before
		// reaching here; retain a fail-closed guard for future internal callers.
		slog.Warn("OpenCode slash-command follow-up has no ordered API mapping",
			"conv_id", sess.ConvID, "line", line, "reason", reason)
		return false
	}
	var command openCodeTUICommand
	switch line {
	case "/compact":
		command = openCodeTUICompact
	case "/exit":
		command = openCodeTUIExit
	default:
		// OpenCode's current lifecycle descriptor only exposes compact and
		// exit. Fail closed if a future caller reaches this sink without an
		// explicit managed-API mapping; never fall back to keystrokes.
		slog.Warn("OpenCode slash-command has no managed TUI API mapping",
			"conv_id", sess.ConvID, "line", line, "reason", reason)
		return false
	}
	if err := sendOpenCodeTUICommand(sess.ConvID, sess.ID, command); err != nil {
		slog.Warn("OpenCode slash-command API dispatch failed",
			"error", err, "conv_id", sess.ConvID, "line", line,
			"reason", reason, "tmux_session", sess.TmuxSession)
		return false
	}
	slog.Info("OpenCode slash-command dispatched via managed TUI API",
		"conv_id", sess.ConvID, "line", line, "reason", reason,
		"tmux_session", sess.TmuxSession, "has_follow_up", false)
	return true
}

// openCodeControlInputBlocked uses the session status already projected from
// OpenCode's managed SSE stream and reconciliation snapshots. A TUI control
// must wait for idle/error: while working, compaction is not ready; while
// awaiting_* the active permission/question layer overrides commands such as
// app.exit. Returning false surfaces a retryable failure to compact/stop
// callers and never reports the command as delivered.
func openCodeControlInputBlocked(status string) bool {
	return status == session.StatusWorking ||
		status == session.StatusRunning ||
		isAwaitingHumanInput(status)
}

// paneInjectMu guards paneInjectLocks. paneInjectLocks holds one mutex
// per tmux pane target, serializing send-keys injection into that pane
// (JOH-310).
//
// Every channel through which the daemon types into a CC pane — queued inbox
// nudges (sendNudgeBracket),
// slash commands (/rename, /compact, soft-exit), the welcome prompt,
// ephemeral reminders, and the remote-access toggle — runs a MULTI-STEP
// send-keys sequence (text → settle → Enter → settle → Enter, or a full
// menu walk). Those steps are NOT atomic at the tmux layer, so two
// sequences racing on the same pane interleave: the text from one and
// the Enter from another land in the same input box, submitting a
// garbled or merged prompt (and a nudge can be lost).
//
// This still matters under the async per-agent delivery queue (JOH-310):
// inbox nudges no longer run on the sender's request, but the per-agent and
// per-conv drains can both target one pane (a head-following and a pinned
// message), and the unread-reminder sweep, slash/welcome/export injectors and
// cron all type into the same panes. The lock keeps any two such sequences
// single-file.
//
// SCOPE — this in-process mutex cheaply serializes agentd workers. The shared
// paneinput primitive then takes a crash-releasing advisory file lock, which
// extends the same contention boundary to hook callbacks and CLI/task
// subprocesses targeting the pane.
//
// COST — the lock is held across injectTextAndSubmit's two ~500 ms settle
// sleeps (and injectMenuToggle's whole menu walk). Under the async model the
// waiters are BACKGROUND drain/reminder goroutines, not the sender's request
// (the send path enqueues and returns), so this latency is no longer on any
// caller's critical path — it only single-files the daemon's own injectors.
//
// The map gains one entry per distinct pane target the daemon ever
// injects into (bounded by total agents launched this daemon life) and
// is never pruned — a handful of bytes per ever-seen pane, negligible at
// personal scale and not worth the lock churn of cleanup.
var (
	paneInjectMu    sync.Mutex
	paneInjectLocks = map[string]*sync.Mutex{}
	// A nudge claim lives for two minutes. Keep lock acquisition well inside
	// that lease so a live worker cannot wait past claim expiry and later
	// duplicate an injection performed by the worker that reclaimed it.
	paneInjectLockTimeout = time.Minute
	// Test seam invoked after immediate acquisition fails, before the timed
	// polling loop. Production leaves it nil.
	paneInjectLockContendedHook func()
)

var errPaneInjectLockTimeout = errors.New("pane injection lock timeout")

// paneInjectLock returns the mutex serializing send-keys injection into
// tmuxTarget, creating it on first use. Hold it for the WHOLE injection
// sequence (text + both Enters, or a full menu walk) — never just a
// single send-keys call — so the sequence is atomic against other
// injectors hitting the same pane.
func paneInjectLock(tmuxTarget string) *sync.Mutex {
	paneInjectMu.Lock()
	defer paneInjectMu.Unlock()
	mu := paneInjectLocks[tmuxTarget]
	if mu == nil {
		mu = &sync.Mutex{}
		paneInjectLocks[tmuxTarget] = mu
	}
	return mu
}

func acquirePaneInjectLock(mu *sync.Mutex) error {
	if mu.TryLock() {
		return nil
	}
	if paneInjectLockContendedHook != nil {
		paneInjectLockContendedHook()
	}
	timer := time.NewTimer(paneInjectLockTimeout)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if mu.TryLock() {
				return nil
			}
		case <-timer.C:
			return fmt.Errorf("%w after %s", errPaneInjectLockTimeout, paneInjectLockTimeout)
		}
	}
}

// injectTextAndSubmit types `text` into a harness pane and submits it as a
// fresh prompt. Multiline/tabbed text is staged in a uniquely named tmux
// buffer and pasted with bracketed-paste mode, which preserves those bytes in
// one input turn instead of treating each newline as Enter. Single-line text
// keeps the smaller send-keys path.
//
// The text/paste and submit Enter are split with a 500 ms gap so the harness's
// bracketed-paste mode can't coalesce the trailing Enter into a paste-newline —
// when that happens, the text gets pasted into the input box but never
// submitted.
// (We learned this the hard way during reincarnate's handoff nudge:
// rename worked, the [system: new agent message ...] text appeared
// in the prompt, and neither Enter actually submitted because both
// arrived back-to-back during the same paste-mode window. 200 ms was
// enough in casual testing; 500 ms is the safety margin for slower
// terminals / heavier load.)
//
// The trailing Enter is sent twice (belt-and-suspenders); the second
// is a no-op if the first already submitted. Caller must have verified
// the tmux pane is alive.
//
// The whole sequence runs under the pane's injection lock so two
// injectors targeting the same pane single-file instead of interleaving
// their send-keys (JOH-310 — see paneInjectLock).
func injectTextAndSubmit(tmuxTarget, text string) error {
	return injectTextAndSubmitWithOptions(tmuxTarget, tmuxTarget, text, false)
}

// injectBracketedTextAndSubmit keeps one-line text atomic for a terminal
// application that recognises bracketed paste as an event. Agent-message
// nudges use this path so an inbox watcher can release itself without trying
// to reconstruct a server-authored envelope from individual key presses.
func injectBracketedTextAndSubmit(tmuxTarget, text string) error {
	return injectTextAndSubmitWithOptions(tmuxTarget, tmuxTarget, text, true)
}

// injectSoftExitTextSerializedBy sends the harness's soft-exit command to
// tmuxTarget while serializing under lockTarget's identity, preceded by that
// harness's prefix keys (harness.Lifecycle.SoftExitPrefixKeys).
//
// The two targets differ because lifecycle types into the exact pane ID (%N)
// while the same pane's message/nudge streams lock its session-shaped target —
// the two spellings otherwise key different in-process mutexes AND different
// cross-process advisory lock files, so the input sequences would not
// single-file. Callers that know the pane's session pass it as lockTarget.
//
// The prefix keys are part of the same locked sequence as the text and its
// Enters: a cancel that another injector could slip a keystroke into would
// defeat the state it exists to establish.
func injectSoftExitTextSerializedBy(lockTarget, tmuxTarget, text string, prefixKeys []string) error {
	return injectTextAndSubmitWithOptions(lockTarget, tmuxTarget, text, false, prefixKeys...)
}

// injectSignalExitSerializedBy is a harness's keystroke-free soft exit: the
// ordered key names from harness.Lifecycle.SignalExitKeys sent into the pane
// one settle apart, as one lock-held sequence. Keystroke-free on purpose — a
// typed slash command is silently dropped both mid-turn and whenever a TUI's
// keypress reader wedges outright (the Copilot 2026-08-09 incident: three
// /exit injections ignored for the full 10 s escalation deadline while ctrl-c
// handling demonstrably kept working), while the ctrl-c quit rides the
// surviving signal path. The exact keys and press count are per harness (see
// each Lifecycle.SignalExitKeys): Copilot and Codex send four C-c; Claude Code
// prefixes an Escape to clear a permission dialog or half-typed line first.
//
// The gap is signalExitKeyGap (330 ms), deliberately tighter than the typed
// path's injectSettleDelay: the presses must land inside the harness's
// re-press window, and Claude Code's — bracketed at ~0.8 s on an idle pane —
// leaves 500 ms too little margin when scheduling delay or pane load pushes a
// press late (observed post-TCL-1137-deploy as first attempts that armed but
// never quit). 330 ms sits inside every measured re-press window (Copilot
// 1.2–1.5 s, Claude Code ~0.8 s, Codex unbounded) with room to spare. One
// honest caveat: Copilot's fast side was only verified down to 0.5 s between
// presses, so 330 ms is below its tested lower bracket — mitigated because
// the sequence's third press lands at +660 ms, inside the verified band, even
// if a too-quick second press were swallowed. Key names carry no
// paste-coalescing concern, which is what injectSettleDelay's 500 ms exists
// for. The flow-test override keeps simulated stops fast.
// Only the FIRST key's failure is an error — a pane commonly dies on a later
// press, making a subsequent "can't find pane" the success case.
//
// keys must be non-empty; callers select this path precisely when
// SignalExitKeys is non-empty (see sendSoftExitToTarget), so the empty-list
// error below is a belt-and-braces guard, not a reachable production path.
func injectSignalExitSerializedBy(lockTarget, tmuxTarget string, keys []string) error {
	if len(keys) == 0 {
		return fmt.Errorf("signal exit: no keys for %s", tmuxTarget)
	}
	mu := paneInjectLock(injectLockKey(lockTarget))
	if err := acquirePaneInjectLock(mu); err != nil {
		return err
	}
	defer mu.Unlock()
	return paneinput.WithLock(tmuxTarget, paneinput.Options{
		Tmux:        boundedTmux(),
		CapturePane: captureInputPane,
		LockTimeout: paneInjectLockTimeout,
		LockID:      lockTarget,
	}, func(in paneinput.Input) error {
		for i, key := range keys {
			if i > 0 {
				time.Sleep(signalExitKeyGap)
			}
			if err := in.Keys(key); err != nil {
				if i == 0 {
					return fmt.Errorf("send-keys %s: %w", key, err)
				}
				return nil
			}
		}
		return nil
	})
}

func injectTextAndSubmitWithOptions(lockTarget, tmuxTarget, text string, forceBracketedPaste bool, prefixKeys ...string) error {
	mu := paneInjectLock(injectLockKey(lockTarget))
	if err := acquirePaneInjectLock(mu); err != nil {
		return err
	}
	defer mu.Unlock()
	return paneinput.InjectTextAndSubmit(tmuxTarget, text, paneinput.Options{
		Tmux:                boundedTmux(),
		CapturePane:         captureInputPane,
		SettleDelay:         injectSettleDelay,
		SettleDelaySet:      true,
		LockTimeout:         paneInjectLockTimeout,
		LockID:              lockTarget,
		ForceBracketedPaste: forceBracketedPaste,
		PrefixKeys:          prefixKeys,
	})
}

// injectLockKey canonicalizes a lock identity by delegating to the exact
// helper paneinput keys its advisory file with, so the in-process mutex
// and the cross-process file lock can never drift onto different
// identities for one pane, and "sess:0.0" / "=sess:0.0" spellings share
// one in-process mutex.
func injectLockKey(target string) string {
	return paneinput.ExactInputTarget(target)
}

// injectMenuToggle types a slash command that opens a confirm MENU, submits
// it EXACTLY ONCE, waits confirmDelay for the menu to render, then walks the
// menu with menuKeys — one key per send-keys, a stepDelay between each.
//
// It deliberately does NOT route through injectTextAndSubmit. That helper
// sends a belt-and-suspenders SECOND Enter after the submit (paste-coalescing
// insurance), which is harmless for a command that just submits a prompt — but
// fatal for one that opens a menu: the stray Enter lands ON the confirm menu
// and accepts its DEFAULT entry, tearing the menu down before menuKeys can
// move the highlight. That is exactly what left Remote Access stuck ON when
// the operator hit "disable" (the default entry is "keep connected", not
// "disconnect"). So here the toggle is submitted with a single Enter, after a
// settle gap so bracketed-paste mode can't coalesce that one Enter into the
// paste. That toggle→Enter gap deliberately reuses the package-global
// injectSettleDelay (it's the same paste-coalescing concern as
// injectTextAndSubmit, so the two stay in lockstep under
// SetInjectSettleDelayForTest); only the menu-render and per-key settles are
// parameters, since those are remote-control-specific. Caller must have
// verified the pane is alive and that the command opens a menu.
//
// Like injectTextAndSubmit, the whole toggle+menu-walk runs under the
// pane's injection lock so a concurrent nudge can't slip a send-keys
// into the middle of the menu navigation (JOH-310 — see paneInjectLock).
// It does NOT call injectTextAndSubmit, so there is no re-entrant lock.
func injectMenuToggle(tmuxTarget, toggle string, menuKeys []string, confirmDelay, stepDelay time.Duration) error {
	mu := paneInjectLock(injectLockKey(tmuxTarget))
	if err := acquirePaneInjectLock(mu); err != nil {
		return err
	}
	defer mu.Unlock()
	return paneinput.WithLock(tmuxTarget, paneinput.Options{
		Tmux:        boundedTmux(),
		CapturePane: captureInputPane,
		LockTimeout: paneInjectLockTimeout,
	}, func(in paneinput.Input) error {
		if err := in.Keys(toggle); err != nil {
			return fmt.Errorf("send-keys toggle: %w", err)
		}
		time.Sleep(injectSettleDelay)
		if err := in.Keys("Enter"); err != nil {
			return fmt.Errorf("send-keys submit: %w", err)
		}
		// Let the confirm menu render before moving its highlight.
		time.Sleep(confirmDelay)
		for i, key := range menuKeys {
			if i > 0 {
				time.Sleep(stepDelay)
			}
			if err := in.Keys(key); err != nil {
				return fmt.Errorf("send-keys menu key %q: %w", key, err)
			}
		}
		return nil
	})
}

// injectSettleDelay is the gap injectTextAndSubmit leaves between its
// send-keys calls (see that function for why 500 ms in production). It
// is a package var, not a constant, so flow tests can shrink it to
// ~nothing: the simulator processes keystrokes synchronously and needs
// no settle window, yet a hardcoded 500 ms made every injection-driven
// flow test (soft /exit, /rename, welcome, nudge) sit on ~1 s of real
// sleeps. Overridden via SetInjectSettleDelayForTest in the flow
// harness setup; production keeps the 500 ms safety margin.
var injectSettleDelay = 500 * time.Millisecond

// signalExitKeyGap is the gap injectSignalExitSerializedBy leaves between
// signal-exit key presses — see that function for why it is tighter than
// injectSettleDelay. A package var for the same flow-test reason; shrunk
// alongside injectSettleDelay by SetInjectSettleDelayForTest.
var signalExitKeyGap = 330 * time.Millisecond
