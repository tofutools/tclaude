package agentd

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/remoteaccess"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
)

// BuildHandlerForTest exposes the production /v1 mux to flow tests in
// `package agentd_test`. The mux is identical to what serve() installs
// — minus the socket plumbing. The _test.go suffix keeps it out of
// production builds; only test binaries see it.
func BuildHandlerForTest() http.Handler {
	return buildMux()
}

// BuildRemoteDashboardHandlerForTest exposes the REAL remote-listener handler
// (the production remoteAuthMiddleware wrapping the full dashboard route set)
// to flow tests in `package agentd_test`, so they can prove the regular
// dashboard renders over the remote (mTLS + passphrase) path with live data —
// not a stub. The TLS/mTLS layer is the listener's job (proven separately); a
// direct handler call exercises the cookie gate + the shared dashboard routes.
func BuildRemoteDashboardHandlerForTest(m *remoteaccess.Material) http.Handler {
	initDashboardToken()
	return buildRemoteDashboardHandler(m)
}

// RunAuditLogCleanupForTest runs one audit-log retention sweep
// synchronously (the same work startAuditLogCleanup does on its timer),
// so a flow test can drive the prune without starting the goroutine or
// waiting its interval. It reads the retention from the test HOME's
// config.json, defaulting to DefaultAuditRetentionDays.
func RunAuditLogCleanupForTest(now time.Time) { runAuditLogCleanup(now) }

// RunRetiredAgentCleanupForTest runs one long-horizon retired-agent
// cleanup sweep synchronously (the same work startRetiredAgentCleanup
// does on its timer), so a flow test can drive the delete without
// starting the goroutine or waiting its 30-minute interval. It reads the
// opt-in toggle + retention window from the test HOME's config.json.
// Passing an explicit `now` lets a test backdate the cutoff so a
// just-retired agent is treated as long-retired without sleeping.
func RunRetiredAgentCleanupForTest(now time.Time) { runRetiredAgentCleanup(now) }

// RunUnreadReminderTickForTest runs one unread-message reminder sweep
// synchronously (the same work startUnreadReminderSweep does on its timer)
// against a fresh, isolated cadence clock, so a flow test can drive the
// re-nudge without starting the goroutine or waiting its interval. Passing an
// explicit `now` lets a test fast-forward past unreadReminderInterval without
// sleeping; the same `st` threaded across calls models successive ticks.
func RunUnreadReminderTickForTest(now time.Time, st *unreadReminderState) {
	runUnreadReminderTickWith(now, st)
}

// NewUnreadReminderStateForTest mints a fresh cadence clock for a test so
// state never leaks between scenarios in the shared package test binary.
func NewUnreadReminderStateForTest() *unreadReminderState { return newUnreadReminderState() }

// SeedUnreadReminderEpochForTest sets the cadence-clock epoch (the floor a
// never-yet-reminded conv's first reminder is clamped to), so a test can model
// a daemon that started AFTER a message was delivered and prove the restart
// floor defers that message's first reminder to epoch+interval.
func SeedUnreadReminderEpochForTest(st *unreadReminderState, t time.Time) { st.setEpoch(t) }

// SetUnreadReminderIntervalForTest overrides the per-message reminder cadence
// for the duration of a test. Returns a restore closure for t.Cleanup.
func SetUnreadReminderIntervalForTest(d time.Duration) func() {
	prev := unreadReminderInterval
	unreadReminderInterval = d
	return func() { unreadReminderInterval = prev }
}

// SetBranchHistoryPREnrichmentForTest flips the conv_branch_history PR
// enrichment gate, which production leaves off by default. A test that
// wants refreshBranchLink to stamp resolved PRs onto the history table
// calls this with true. Returns a restore closure for t.Cleanup.
func SetBranchHistoryPREnrichmentForTest(enabled bool) func() {
	prev := branchHistoryPREnrichment
	branchHistoryPREnrichment = enabled
	return func() { branchHistoryPREnrichment = prev }
}

// WaitForBackgroundForTest blocks until every goBackground goroutine
// (spawn / clone post-init, etc.) has returned. Flow setup registers
// this in t.Cleanup so the next test's db.ResetForTest doesn't race a
// previous test's post-init goroutine inside db.Open's sync.Once, and
// so t.TempDir's RemoveAll doesn't trip on writes that arrive after
// teardown started (post-init writes to $HOME/.tclaude/db.sqlite via
// db.FindSessionsByConvID).
//
// Drain time is bounded by reincarnateAliveTimeout — flow_setup_test.go
// shrinks both timing knobs via SetWaitTimingsForTest so the bound is
// milliseconds, not minutes.
func WaitForBackgroundForTest() { bgWG.Wait() }

// ResetDeliveryDebounceForTest clears the process-local delivery throttle.
// Flow scenarios reuse stable fixture conv IDs across -count iterations, while
// production naturally retains this state for the daemon lifetime.
func ResetDeliveryDebounceForTest() {
	flushDebounceMu.Lock()
	flushDebounce = map[string]time.Time{}
	flushDebounceMu.Unlock()
}

// SetTmuxCacheTTLForTest overrides the shared LiveTmuxSessions cache TTL and
// clears any warm entry, returning a restore closure. newFlow sets it to 0 so
// the cache is transparent (every call re-probes) — preserving each existing
// scenario's tmux-liveness freshness across back-to-back fetches. The TCL-370
// coalescing test re-sets a positive TTL to observe the tick's parallel
// handlers share one probe.
func SetTmuxCacheTTLForTest(ttl time.Duration) func() {
	liveTmuxCache.mu.Lock()
	prev := liveTmuxCache.ttl
	liveTmuxCache.ttl = ttl
	liveTmuxCache.valid = false
	liveTmuxCache.sessions = nil
	liveTmuxCache.err = nil
	liveTmuxCache.expires = time.Time{}
	liveTmuxCache.mu.Unlock()
	return func() {
		liveTmuxCache.mu.Lock()
		liveTmuxCache.ttl = prev
		liveTmuxCache.valid = false
		liveTmuxCache.sessions = nil
		liveTmuxCache.err = nil
		liveTmuxCache.expires = time.Time{}
		liveTmuxCache.mu.Unlock()
	}
}

// SweepWaveChoreographiesForTest runs one pass of the staged-spawn wave runner
// (JOH-244) synchronously — the flow-test entry point that drives a
// choreography forward one gate check without waiting on the production ticker.
// Not a subprocess mock: it just ticks the sweeper the daemon otherwise runs on
// a timer, so a test can assert wave-by-wave advancement deterministically.
func SweepWaveChoreographiesForTest() { sweepWaveChoreographies() }

// SetWaitTimingsForTest overrides reincarnateAliveTimeout +
// reincarnateReadyDelay for the duration of a test. Returns a restore
// closure intended for t.Cleanup. Flow tests call this at setup so the
// post-init drain in WaitForBackgroundForTest can't sit for 60s when a
// scenario doesn't bring the new conv online.
func SetWaitTimingsForTest(aliveTimeout, readyDelay time.Duration) func() {
	prevAlive := reincarnateAliveTimeout
	prevDelay := reincarnateReadyDelay
	reincarnateAliveTimeout = aliveTimeout
	reincarnateReadyDelay = readyDelay
	return func() {
		reincarnateAliveTimeout = prevAlive
		reincarnateReadyDelay = prevDelay
	}
}

func SetReincarnateSpawnTimeoutForTest(timeout time.Duration) func() {
	previous := reincarnateSpawnTimeout
	reincarnateSpawnTimeout = timeout
	return func() { reincarnateSpawnTimeout = previous }
}

// SetInjectSettleDelayForTest shrinks injectTextAndSubmit's per-send-keys
// settle gap for the duration of a test. The simulator processes
// keystrokes synchronously, so the production 500 ms window is pure dead
// wait in flow tests — every soft /exit, /rename, welcome and nudge pays
// ~1 s of it. Flow setup calls this so the whole suite stops sleeping.
// Returns a restore closure for t.Cleanup.
func SetInjectSettleDelayForTest(d time.Duration) func() {
	prev := injectSettleDelay
	injectSettleDelay = d
	return func() { injectSettleDelay = prev }
}

// SetTmuxCommandTimeoutForTest shrinks the nudge-path subprocess deadline so
// a flow can model a hung tmux client without paying the production five
// seconds. Returns a restore closure intended for t.Cleanup.
func SetTmuxCommandTimeoutForTest(d time.Duration) func() {
	prev := tmuxCommandTimeout
	tmuxCommandTimeout = d
	return func() { tmuxCommandTimeout = prev }
}

// ControlNextTmuxCommandTimeoutForTest replaces only the next tmux command's
// real timer with a caller-fired timeout. The timer is armed after the child
// process starts, so receiving from armed is a causal subprocess-start barrier;
// firing it then exercises production kill, reap, and timeout handling without
// sleeping until the deadline. Later commands retain the production timer.
func ControlNextTmuxCommandTimeoutForTest() (armed <-chan time.Duration, fire func(), restore func()) {
	armedCh := make(chan time.Duration, 1)
	firedCh := make(chan time.Time, 1)
	previous := startTmuxCommandTimer
	var claimed atomic.Bool
	startTmuxCommandTimer = func(timeout time.Duration) (<-chan time.Time, func()) {
		if claimed.CompareAndSwap(false, true) {
			armedCh <- timeout
			return firedCh, func() {}
		}
		return previous(timeout)
	}
	fireTimeout := func() {
		select {
		case firedCh <- time.Time{}:
		default:
		}
	}
	return armedCh, fireTimeout, func() {
		fireTimeout()
		startTmuxCommandTimer = previous
	}
}

// SetNudgeRetryTimingForTest overrides durable retry backoff timings for a
// flow that drives failure → reaper retry without real 30-second sleeps.
func SetNudgeRetryTimingForTest(base, max time.Duration) func() {
	prevBase, prevMax := nudgeRetryBase, nudgeRetryMax
	nudgeRetryBase, nudgeRetryMax = base, max
	return func() { nudgeRetryBase, nudgeRetryMax = prevBase, prevMax }
}

// SetStaleNudgeTimingForTest shrinks the queue-health threshold/throttle and
// resets its process-local cadence map for deterministic log assertions.
func SetStaleNudgeTimingForTest(threshold, every time.Duration) func() {
	prevThreshold, prevEvery := staleNudgeThreshold, staleNudgeLogEvery
	staleNudgeThreshold, staleNudgeLogEvery = threshold, every
	staleNudgeLogMu.Lock()
	staleNudgeLoggedAt = map[string]time.Time{}
	staleNudgeLogMu.Unlock()
	return func() {
		staleNudgeThreshold, staleNudgeLogEvery = prevThreshold, prevEvery
		staleNudgeLogMu.Lock()
		staleNudgeLoggedAt = map[string]time.Time{}
		staleNudgeLogMu.Unlock()
	}
}

// SetSoftExitRetryDelayForTest shrinks the background soft-exit retry's
// per-attempt wait for the duration of a test. Production waits a few
// seconds between re-injecting /exit into a still-alive pane; under the
// synchronous simulator that is pure dead wait that every stop/retire/
// reincarnate flow would pay (and WaitForBackgroundForTest would block
// on). Returns a restore closure for t.Cleanup.
func SetSoftExitRetryDelayForTest(d time.Duration) func() {
	prev := softExitRetryDelay
	softExitRetryDelay = d
	return func() { softExitRetryDelay = prev }
}

// SetRemoteControlConfirmDelayForTest shrinks the remote-control disable
// timing for the duration of a test — BOTH the pause before CC's confirm menu
// renders and the gap between the menu keystrokes (Up/Up/Enter) — the same
// reason as SetInjectSettleDelayForTest (the sim is synchronous). Returns a
// restore closure for t.Cleanup.
func SetRemoteControlConfirmDelayForTest(d time.Duration) func() {
	prevConfirm := remoteControlConfirmDelay
	prevStep := remoteControlMenuStepDelay
	remoteControlConfirmDelay = d
	remoteControlMenuStepDelay = d
	return func() {
		remoteControlConfirmDelay = prevConfirm
		remoteControlMenuStepDelay = prevStep
	}
}

// ResetCodexContextRefreshForTest clears the process-local Codex context
// refresh throttle. Flow tests reset the DB between scenarios; clearing this
// cache keeps repeated runs of the same session label deterministic.
func ResetCodexContextRefreshForTest() {
	codexContextRefreshMu.Lock()
	defer codexContextRefreshMu.Unlock()
	codexContextRefreshMu.last = nil
}

// AsHumanPeer attaches a synthetic peer context that classify() resolves
// to the human operator (classHuman) — modelling a CLI caller holding a
// valid operator token. All permission gates pass.
func AsHumanPeer(r *http.Request) *http.Request {
	p := &peer{PID: 99999, HumanTokenValid: true}
	return r.WithContext(context.WithValue(r.Context(), peerKey{}, p))
}

// AsUnconfirmedPeer attaches a synthetic peer that classify() resolves
// to classUnconfirmed — a caller with no Claude Code ancestor and no
// operator token. Fail-closed: every human-vs-agent gate refuses it.
func AsUnconfirmedPeer(r *http.Request) *http.Request {
	p := &peer{PID: 99999}
	return r.WithContext(context.WithValue(r.Context(), peerKey{}, p))
}

// AsAgentPeer attaches a synthetic peer context that requirePermission
// resolves to convID. Default-permission lookups (config + DB) still
// run, so grants must be in place for the endpoint to succeed.
func AsAgentPeer(r *http.Request, convID string) *http.Request {
	p := &peer{PID: 99999, HasClaudeAncestor: true, ConvID: convID}
	return r.WithContext(context.WithValue(r.Context(), peerKey{}, p))
}

// SetOperatorTokenForTest installs a known operator token so flow tests
// can exercise the dashboard browser-login path (handleDashboardLogin)
// without scraping the random startup banner. Returns a restore
// function tests schedule via t.Cleanup.
func SetOperatorTokenForTest(tok string) func() {
	operatorTokenMu.Lock()
	prev := operatorToken
	operatorToken = tok
	operatorTokenMu.Unlock()
	return func() {
		operatorTokenMu.Lock()
		operatorToken = prev
		operatorTokenMu.Unlock()
	}
}

// SetPopupBaseURLForTest overrides the popup base URL so flow tests
// can reach the X-Tclaude-Ask-Human escalation branch without binding
// a real loopback HTTP server. Returns a restore function tests can
// schedule via t.Cleanup.
func SetPopupBaseURLForTest(url string) func() {
	prev := popupBaseURL
	popupBaseURL = url
	return func() { popupBaseURL = prev }
}

// RefreshCodexUsageForTest runs one Codex usage scan synchronously — the
// same work startCodexUsagePoller does on its timer — so a flow test can
// populate the in-memory snapshot from rollouts it just wrote under the
// test $HOME, then assert the result on /api/snapshot without standing up
// the poller goroutine.
func RefreshCodexUsageForTest() {
	refreshCodexUsage(true)
}

// StubApprovalForTest swaps the human-approval popup with a stub that
// returns `decision` immediately. Returns a restore function. The
// approvalRequest type stays unexported; the stub closes over `decision`
// and discards the request body since flow tests only care about the
// outcome, not the popup payload.
func StubApprovalForTest(decision bool) func() {
	prev := RequestHumanApprovalImpl
	RequestHumanApprovalImpl = func(*approvalRequest, string) bool {
		return decision
	}
	return func() { RequestHumanApprovalImpl = prev }
}

// StubCountingApprovalForTest swaps the human-approval popup with an immediate
// decision and returns a counter accessor plus restore function. It lets flow
// tests distinguish one logical approval from duplicate popup invocations
// without exporting approvalRequest.
func StubCountingApprovalForTest(decision bool) (func() int32, func()) {
	prev := RequestHumanApprovalImpl
	var calls atomic.Int32
	RequestHumanApprovalImpl = func(*approvalRequest, string) bool {
		calls.Add(1)
		return decision
	}
	return calls.Load, func() { RequestHumanApprovalImpl = prev }
}

// StubAlwaysAllowApprovalForTest swaps the popup with a stub that drives
// the "Always allow for this agent" outcome (JOH-367): it routes through
// the REAL applyApprovalOutcome, so it audits AND persists the allow
// override exactly as the production waiter would when the human clicks
// that button. Lets a flow test assert that an always-allow decision both
// lets the pending request through and grants the agent going forward.
// Returns a restore function for t.Cleanup.
func StubAlwaysAllowApprovalForTest() func() {
	prev := RequestHumanApprovalImpl
	RequestHumanApprovalImpl = func(req *approvalRequest, _ string) bool {
		return applyApprovalOutcome(req, outcomeApproveAlways)
	}
	return func() { RequestHumanApprovalImpl = prev }
}

// BuildDashboardHandlerForTest exposes the dashboard mux (the
// dashboard-listener mux that hosts `/`, `/api/snapshot`,
// `/api/groups/...` mutation endpoints, and the access-request decision
// endpoint in production). Flow tests use this when asserting the
// dashboard's snapshot or edit endpoints — `BuildHandlerForTest` only
// covers the /v1 Unix-socket mux.
//
// Initialises the dashboard session token if it isn't already, then
// returns a `dashTestHandler` that injects a valid cookie + Origin on
// every request — the dashboard's auth checks would otherwise refuse
// the synthetic httptest peer.
func BuildDashboardHandlerForTest() http.Handler {
	initDashboardToken()
	mux := http.NewServeMux()
	registerDashboardRoutes(mux)
	// Wrap with auditRequests exactly as the production popup server does,
	// so flow tests exercise the dashboard audit-capture path too.
	return &dashTestHandler{inner: auditRequests(mux)}
}

// RegisterDashboardRoutesForTest exposes registerDashboardRoutes
// directly without the dashTestHandler cookie-injection wrapper. Lets
// flow tests prove that the dashboard's auth check actually refuses
// uncookied requests (rather than the test harness silently passing
// because it always injects a cookie).
func RegisterDashboardRoutesForTest(mux *http.ServeMux) {
	initDashboardToken()
	registerDashboardRoutes(mux)
}

// TermWSHook is the terminal-WebSocket PTY seam (see termWSHook in
// dashboard_term.go): the env-gated real-browser terminal smoke swaps the
// tmux/attach command runPTYOverWS would spawn for a deterministic PTY
// program, and observes PTY starts, applied resizes, and per-connection
// teardowns from outside the browser.
type TermWSHook = termWSHook

// SetTermWSHookForTest installs the terminal-WebSocket PTY hook. Returns a
// restore function for t.Cleanup. Install it BEFORE serving the dashboard
// handler — runPTYOverWS reads the hook once per connection.
func SetTermWSHookForTest(hook *TermWSHook) func() {
	prev := termWSTestHook
	termWSTestHook = hook
	return func() { termWSTestHook = prev }
}

// SetOpenTerminalForTest swaps the terminal-spawning seam so flow
// tests can assert the `dir` endpoints' open path without popping a
// real window (and without failing on CI hosts that have no terminal
// emulator). Returns a restore function for t.Cleanup.
func SetOpenTerminalForTest(fn func(string) error) func() {
	prev := openTerminal
	openTerminal = fn
	return func() { openTerminal = prev }
}

// SetFocusAgentWindowForTest swaps the per-agent focus seam behind
// the bulk /api/agent-windows endpoint so flow tests can assert which
// agents the focus path was dispatched for without popping a real OS
// window. Returns a restore function for t.Cleanup. Mirrors the
// openTerminal seam.
func SetFocusAgentWindowForTest(fn func(*db.SessionRow)) func() {
	prev := focusAgentWindow
	focusAgentWindow = fn
	return func() { focusAgentWindow = prev }
}

// SetHumanMessageNotifierForTest swaps the notify-human OS-notification
// seam so a flow test can assert that handleNotifyHuman dispatched a
// desktop notification — with the right sender/group/subject/body — without
// the production notify.SendHumanMessage (which self-gates on config and
// would no-op under the test's default config anyway). The handler fires
// through goBackground, so drain with WaitForBackgroundForTest before
// asserting. Returns a restore function for t.Cleanup. Mirrors the
// reaperNotify recorder pattern.
func SetHumanMessageNotifierForTest(fn func(senderSessionID, fromTitle, group, subject, body string)) func() {
	prev := humanMsgNotify
	humanMsgNotify = fn
	return func() { humanMsgNotify = prev }
}

// RunHumanMessageAttachmentCleanupForTest runs the filesystem/DB reconciler
// synchronously for flow coverage.
func RunHumanMessageAttachmentCleanupForTest() {
	runHumanMessageAttachmentCleanup()
}

// SetHumanMessageAttachmentQuotasForTest shrinks storage limits for flow tests.
func SetHumanMessageAttachmentQuotasForTest(perSenderBytes, totalBytes int64, perSenderCount, totalCount int) func() {
	oldSender, oldTotal := maxHumanMessageAttachmentSenderBytes, maxHumanMessageAttachmentTotalBytes
	oldSenderCount, oldTotalCount := maxHumanMessageAttachmentSenderCount, maxHumanMessageAttachmentTotalCount
	maxHumanMessageAttachmentSenderBytes, maxHumanMessageAttachmentTotalBytes = perSenderBytes, totalBytes
	maxHumanMessageAttachmentSenderCount, maxHumanMessageAttachmentTotalCount = perSenderCount, totalCount
	return func() {
		maxHumanMessageAttachmentSenderBytes, maxHumanMessageAttachmentTotalBytes = oldSender, oldTotal
		maxHumanMessageAttachmentSenderCount, maxHumanMessageAttachmentTotalCount = oldSenderCount, oldTotalCount
	}
}

// SetHumanMessageAttachmentUploadTimerForTest controls when a stalled upload
// times out without waiting for a real timer. start receives the production
// timeout and callback and returns the callback used to stop the timer.
func SetHumanMessageAttachmentUploadTimerForTest(start func(time.Duration, func()) func()) func() {
	old := humanMessageAttachmentStartUploadTimer
	humanMessageAttachmentStartUploadTimer = start
	return func() { humanMessageAttachmentStartUploadTimer = old }
}

// SetClipboardWriterForTest swaps the platform clipboard-write seam so a
// flow test can assert that handleClipboard reached the copy path with the
// exact text — without execing a real wl-copy/xclip/pbcopy/clip.exe (which
// would fail on a headless CI host anyway). The recorder returns the error
// the fake wants the handler to see, so a test can also drive the
// copy-tool-failure branch. Returns a restore function for t.Cleanup.
func SetClipboardWriterForTest(fn func(text string) error) func() {
	prev := clipboardWrite
	clipboardWrite = fn
	return func() { clipboardWrite = prev }
}

// SetDetachAgentWindowsForTest swaps the per-agent unfocus/detach seam
// behind the bulk /api/agent-windows endpoint. The fake returns the
// detached-client count the real session.DetachSessionClients would
// have, so a flow test can drive the no_window / detached / failed
// outcomes. Returns a restore function for t.Cleanup.
func SetDetachAgentWindowsForTest(fn func(*db.SessionRow) (int, error)) func() {
	prev := detachAgentWindows
	detachAgentWindows = fn
	return func() { detachAgentWindows = prev }
}

// UnfocusAllAgentWindowsForTest drives the in-process unfocus-all path
// the tray's "Unfocus all agents" item triggers, returning the outcome
// counts. The detach seam (SetDetachAgentWindowsForTest) is honoured, so
// a flow test can assert which agents were detached — and that they keep
// running — without real tmux.
func UnfocusAllAgentWindowsForTest() (targeted, detached, noWindow, failed int, err error) {
	resp, err := unfocusAllAgentWindows()
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return resp.Targeted, resp.Detached, resp.NoWindow, resp.Failed, nil
}

// SetTileConfigForFocusForTest swaps the post-focus tiling config gate so
// a flow test can force auto-tiling on (and pick the layout options)
// without writing a config.json. Returns a restore for t.Cleanup.
func SetTileConfigForFocusForTest(fn func() (bool, session.TileOptions)) func() {
	prev := tileConfigForFocus
	tileConfigForFocus = fn
	return func() { tileConfigForFocus = prev }
}

// SetTileAgentWindowsForTest swaps the per-platform tiling dispatch behind
// the bulk focus op so a flow test can assert the ORDERED spec set the
// tiling pass was handed — layout math included — without moving a real
// OS window. Returns a restore for t.Cleanup. Mirrors the focus seam.
func SetTileAgentWindowsForTest(fn func([]session.TileSpec, session.TileOptions)) func() {
	prev := tileAgentWindows
	tileAgentWindows = fn
	return func() { tileAgentWindows = prev }
}

// SetTileSettleWaitForTest no-ops the post-focus settle wait (the poll
// for the focused windows' tmux clients to come up) so a flow test
// doesn't sit at the stability timeout — the TmuxSim answers
// list-clients empty, so no target would ever look attached. Returns a
// restore for t.Cleanup.
func SetTileSettleWaitForTest() func() {
	prev := waitForFocusedWindows
	waitForFocusedWindows = func([]windowTarget) {}
	return func() { waitForFocusedWindows = prev }
}

// SetGitInfoResolverForTest swaps the git/gh resolver behind the
// dashboard's branch-link enrichment with a deterministic fake. The
// fake is handed a (repoDir, branch) pair and returns the repo's
// GitHub base URL, default branch, and PR info (number, URL and
// lower-cased state) — or ok=false to model a non-GitHub repo. Mirrors
// the clcommon.Default / agentd.Spawn / openTerminal seams: the
// subprocess boundary is mocked, the cache + snapshot read path under
// test runs unchanged. Returns a restore closure for t.Cleanup.
func SetGitInfoResolverForTest(fn func(repoDir, branch string) (repoURL, defaultBranch string, prNumber int, prURL, prState string, ok bool)) func() {
	prev := gitInfoResolver
	gitInfoResolver = func(repoDir, branch string) (repoBranchInfo, bool) {
		repoURL, defaultBranch, prNumber, prURL, prState, ok := fn(repoDir, branch)
		if !ok {
			return repoBranchInfo{}, false
		}
		return repoBranchInfo{
			RepoURL:       repoURL,
			DefaultBranch: defaultBranch,
			Branch:        branch,
			PRNumber:      prNumber,
			PRURL:         prURL,
			PRState:       prState,
		}, true
	}
	return func() { gitInfoResolver = prev }
}

// SetPresentedPRInfoResolverForTest swaps the GitHub PR-by-URL resolver used
// for explicitly presented PRs. The dashboard snapshot still performs the real
// preload/cache/singleflight path; only the `gh pr view <url>` boundary is
// replaced.
func SetPresentedPRInfoResolverForTest(fn func(rawURL string) (number int, resolvedURL, state string, ok bool)) func() {
	prev := presentedPRInfoResolver
	presentedPRInfoResolver = func(rawURL string) (presentedPRInfo, bool) {
		number, resolvedURL, state, ok := fn(rawURL)
		if !ok {
			return presentedPRInfo{}, false
		}
		return presentedPRInfo{Number: number, URL: resolvedURL, State: state}, true
	}
	return func() { presentedPRInfoResolver = prev }
}

// SetAsyncSpawnInlineGraceForTest shrinks the non-blocking spawn's
// conv-id inline grace so a flow test can drive the JOH-205 inc2 pending
// path — a spawn whose conv-id never materialises returns PENDING and
// records a pending_spawns row — without waiting the production multi-second
// grace. Returns a restore closure for t.Cleanup.
func SetAsyncSpawnInlineGraceForTest(d time.Duration) func() {
	prev := asyncSpawnInlineGrace
	asyncSpawnInlineGrace = d
	return func() { asyncSpawnInlineGrace = prev }
}

// SetCodexAsyncSpawnResponseGraceForTest shrinks the seed-needing harness
// response grace separately from the background inline-discovery budget, so a
// flow test can assert that the HTTP response returns Pending quickly while the
// short back-fill still promotes the spawn when its conv-id appears soon after.
func SetCodexAsyncSpawnResponseGraceForTest(d time.Duration) func() {
	prev := codexAsyncSpawnResponseGrace
	codexAsyncSpawnResponseGrace = d
	return func() { codexAsyncSpawnResponseGrace = prev }
}

// SetBeforeExecuteSpawnForTest installs a one-shot mutation seam after the
// HTTP handler snapshots/resolves profiles but before executeSpawn re-reads
// them. It exists solely for the TCL-308 launched-value echo regression.
func SetBeforeExecuteSpawnForTest(fn func()) func() {
	prev := beforeExecuteSpawnForTest
	beforeExecuteSpawnForTest = fn
	return func() { beforeExecuteSpawnForTest = prev }
}

// SetBeforeSoftExitTargetRevalidateForTest installs a one-shot lifecycle
// pause used to prove predecessor/successor pane-swap safety.
func SetBeforeSoftExitTargetRevalidateForTest(fn func()) func() {
	prev := beforeSoftExitTargetRevalidateForTest
	beforeSoftExitTargetRevalidateForTest = fn
	return func() { beforeSoftExitTargetRevalidateForTest = prev }
}

func StopOneConvWithIntentForTest(convID, action string) string {
	return stopOneConvWithIntent(convID, false, action, "").Action
}

// SetAfterSoftExitTargetSendForTest installs a probe seam after exact-pane
// delivery and before the post-send liveness probe.
func SetAfterSoftExitTargetSendForTest(fn func()) func() {
	prev := afterSoftExitTargetSendForTest
	afterSoftExitTargetSendForTest = fn
	return func() { afterSoftExitTargetSendForTest = prev }
}

func SetBeforeSoftExitTargetRetryProbeForTest(fn func(int)) func() {
	prev := beforeSoftExitTargetRetryProbeForTest
	beforeSoftExitTargetRetryProbeForTest = fn
	return func() { beforeSoftExitTargetRetryProbeForTest = prev }
}

// RunPendingSpawnSweepForTest runs one pending-spawn sweep synchronously,
// so a flow test can deterministically trigger the back-fill that enrolls a
// pending spawn once its conv-id has materialised — without starting the
// sweeper's goroutine or waiting its interval.
func RunPendingSpawnSweepForTest() { sweepPendingSpawns() }

// SessionReaperHandle wraps a sessionReaper so flow tests can drive
// ticks deterministically without starting its goroutine.
type SessionReaperHandle struct{ r *sessionReaper }

// NewSessionReaperForTest builds a reaper with the grace window set to
// `grace` (pass 0 to disable the fresh-row exemption) and the offline
// notification routed to `onNotify` instead of the OS notifier — so a
// flow test can assert exactly which sessions produced an alive→dead
// transition. onNotify receives the conv-id and the pre-exit status.
func NewSessionReaperForTest(grace time.Duration, onNotify func(convID, prevStatus string)) *SessionReaperHandle {
	r := newSessionReaper()
	r.grace = grace
	r.notify = func(st *session.SessionState, prevStatus string) {
		onNotify(st.ConvID, prevStatus)
	}
	return &SessionReaperHandle{r: r}
}

// Tick runs one reaper sweep and returns the number of sessions reaped.
func (h *SessionReaperHandle) Tick() int { return h.r.tick(time.Now()) }

// SeedPendingApprovalForTest registers a minimal pending approval under
// id so flow tests can drive the dashboard access-request decision endpoint
// against it. The decision channel is buffered, so a POST approve/deny records
// without a blocked reader. Returns a cleanup that removes the entry.
func SeedPendingApprovalForTest(id string) func() {
	return SeedApprovalForTest(id, "self.rename", false)
}

// SeedApprovalForTest is SeedPendingApprovalForTest with control over the
// perm slug and the autoGrantable flag, so a flow test can drive the
// popup's "always" decision path (JOH-367) and assert its server-side
// eligibility gate for both an eligible and an ineligible slug. Returns a
// cleanup that removes the entry.
func SeedApprovalForTest(id, perm string, autoGrantable bool) func() {
	req := &approvalRequest{
		id:            id,
		perm:          perm,
		autoGrantable: autoGrantable,
		decision:      make(chan approvalOutcome, 1),
		extend:        make(chan time.Duration, 1),
		createdAt:     time.Now(),
		timeout:       60 * time.Second,
	}
	approvals.mu.Lock()
	approvals.pending[id] = req
	approvals.mu.Unlock()
	return func() {
		approvals.mu.Lock()
		delete(approvals.pending, id)
		approvals.mu.Unlock()
	}
}

// ResetApprovalsForTest clears the in-memory approval registry so a flow test
// that leaves a pending approval doesn't leak it into another test's snapshot
// (the registry is a package global). Handled history lives in each test's temp
// DB and is isolated there. Called from newFlow.
func ResetApprovalsForTest() {
	approvals.mu.Lock()
	approvals.pending = map[string]*approvalRequest{}
	approvals.mu.Unlock()
}

// SeedApprovalWithWaiterForTest registers a pending approval (perm,
// convID, autoGrantable) AND starts a goroutine that consumes the first
// decision exactly as the production waiter does — through
// applyApprovalOutcome — so the audit + persist side-effects run for real.
// Lets a flow test POST a decision to the dashboard access-request endpoint
// and observe the end-to-end effect (e.g. the persisted override), rather
// than testing the handler and the persist in disconnected halves.
// The returned channel yields the waiter's approved() result once a
// decision lands (or the internal timeout fires); cleanup removes the
// pending entry.
func SeedApprovalWithWaiterForTest(id, perm, convID string, autoGrantable bool) (<-chan bool, func()) {
	return SeedApprovalCallerWithWaiterForTest(id, perm, convID, peerAgentID(convID), autoGrantable)
}

// SeedApprovalCallerWithWaiterForTest is the stable-caller form of
// SeedApprovalWithWaiterForTest. Keeping agentID independent from convID lets
// display-flow tests prove that refreshes never borrow another actor's title.
func SeedApprovalCallerWithWaiterForTest(id, perm, convID, agentID string, autoGrantable bool) (<-chan bool, func()) {
	req := &approvalRequest{
		id:            id,
		perm:          perm,
		convID:        convID,
		agentID:       agentID,
		autoGrantable: autoGrantable,
		decision:      make(chan approvalOutcome, 1),
		extend:        make(chan time.Duration, 1),
		createdAt:     time.Now(),
		timeout:       10 * time.Second,
	}
	approvals.mu.Lock()
	approvals.pending[id] = req
	approvals.mu.Unlock()

	// Mirror realRequestHumanApproval's request-raise side-effect: the
	// agent-attributed approval.request audit row (JOH-392) is written at
	// registration, before any decision, so a flow test observes the same
	// request→decision pair production writes.
	recordApprovalRequest(req)

	done := make(chan bool, 1)
	go func() {
		timer := time.NewTimer(req.timeout)
		defer timer.Stop()
		// Mirror realRequestHumanApproval: run the outcome side-effects, record
		// the resolution into the recent-history ring, and drop the pending
		// entry — so a flow test sees the same pending→handled transition the
		// dashboard snapshot renders in production.
		remove := func() {
			approvals.mu.Lock()
			delete(approvals.pending, id)
			approvals.mu.Unlock()
		}
		select {
		case d := <-req.decision:
			approved := applyApprovalOutcome(req, d)
			approvals.recordResolved(req, outcomeLabel(d))
			remove()
			done <- approved
		case <-timer.C:
			approvals.recordResolved(req, "timed out")
			remove()
			done <- false
		}
	}()
	return done, func() {
		approvals.mu.Lock()
		delete(approvals.pending, id)
		approvals.mu.Unlock()
	}
}

// SetWorktreeFnsForTest swaps the git-worktree seam — directory
// classification + linked-worktree removal — so cleanup flow tests
// can exercise worktree deletion without real git repos on disk.
// Returns a restore func for t.Cleanup.
func SetWorktreeFnsForTest(
	inspect func(dir string) worktree.WorktreeStatus,
	remove func(root string, force bool) (bool, error),
) func() {
	prevInspect := inspectWorktreeFn
	prevRemove := removeWorktreeFn
	inspectWorktreeFn = inspect
	removeWorktreeFn = remove
	return func() {
		inspectWorktreeFn = prevInspect
		removeWorktreeFn = prevRemove
	}
}

// SetRetireWorktreeFnForTest swaps the retire-time worktree+branch
// removal seam (removeWorktreeBranchFn), so retire flow tests can
// exercise worktree+branch cleanup without real git repos. Returns a
// restore func for t.Cleanup.
func SetRetireWorktreeFnForTest(
	remove func(root, branch string, force bool) (bool, bool, error),
) func() {
	prev := removeWorktreeBranchFn
	removeWorktreeBranchFn = remove
	return func() { removeWorktreeBranchFn = prev }
}

// SetSweepWorktreeFnsForTest swaps the repo-wide worktree-janitor seams
// — repo listing, repo-root resolution, dirty detection, main-repo
// resolution and prune — so the worktree-sweep discovery/cleanup flow
// tests run without real git repos. Returns a restore func for
// t.Cleanup.
func SetSweepWorktreeFnsForTest(
	list func(dir string) ([]worktree.WorktreeInfo, error),
	repoRoot func(path string) (string, error),
	dirty func(dir string) bool,
	mainRepo func(dir string) string,
	prune func(dir string) error,
) func() {
	prevList, prevRoot, prevDirty := listWorktreesInFn, repoRootForPathFn, worktreeDirtyFn
	prevMain, prevPrune := mainRepoForPathFn, pruneWorktreesFn
	listWorktreesInFn = list
	repoRootForPathFn = repoRoot
	worktreeDirtyFn = dirty
	mainRepoForPathFn = mainRepo
	pruneWorktreesFn = prune
	return func() {
		listWorktreesInFn, repoRootForPathFn, worktreeDirtyFn = prevList, prevRoot, prevDirty
		mainRepoForPathFn, pruneWorktreesFn = prevMain, prevPrune
	}
}

// SetRetireWorktreeGraceForTest shrinks the deferred retire cleanup's
// exit-grace window so a flow test can exercise the grace-timeout branch
// (agent never exits → worktree kept → human notice) without waiting the
// production 60s. Returns a restore func for t.Cleanup.
func SetRetireWorktreeGraceForTest(grace time.Duration) func() {
	prev := retireWorktreeExitGrace
	retireWorktreeExitGrace = grace
	return func() { retireWorktreeExitGrace = prev }
}

// StartConvMonitorForTest starts the live conv_index fsnotify monitor
// against the current test HOME's ~/.claude/projects, with the debounce
// shrunk to `debounce` so a test does not have to wait the production
// convMonitorDebounce for a Write to settle. Registers a t.Cleanup that
// stops the monitor synchronously — closes the stop channel and blocks
// until the event-loop goroutine has fully exited — so no in-flight
// ScanAndUpsertFile can race the next test's db.ResetForTest.
//
// Returns the *convMonitor, which is nil when the watcher could not be
// created (e.g. a sandbox with no inotify); a caller can t.Skip on nil.
func StartConvMonitorForTest(t *testing.T, debounce time.Duration) *convMonitor {
	t.Helper()
	prevDebounce := convMonitorDebounce
	convMonitorDebounce = debounce
	stop := make(chan struct{})
	m := startConvMonitor(stop)
	t.Cleanup(func() {
		close(stop)
		m.wait()
		convMonitorDebounce = prevDebounce
	})
	return m
}

// WaitForConvMonitorStartupForTest waits until the monitor's startup scan has
// returned. The scan and live event handling share one goroutine, so this is a
// causal barrier: once it closes, every startup reindex/skip decision is done.
func WaitForConvMonitorStartupForTest(t *testing.T, m *convMonitor) {
	t.Helper()
	if m == nil {
		return
	}
	select {
	case <-m.startupDone:
	case <-m.done:
		t.Fatal("conv monitor stopped before completing its startup scan")
	case <-time.After(10 * time.Second):
		t.Fatal("conv monitor did not complete its startup scan")
	}
}

// StartCodexApprovalMonitorForTest starts the managed-profile approval
// monitor against the current test CODEX_HOME and stops it synchronously.
func StartCodexApprovalMonitorForTest(t *testing.T, debounce time.Duration) *codexApprovalMonitor {
	t.Helper()
	prevDebounce := codexApprovalMonitorDebounce
	codexApprovalMonitorDebounce = debounce
	stop := make(chan struct{})
	m := startCodexApprovalMonitorWithProcessing(stop, make(chan string, 32))
	t.Cleanup(func() {
		close(stop)
		m.wait()
		codexApprovalMonitorDebounce = prevDebounce
	})
	return m
}

// WaitForCodexApprovalProcessingForTest waits until the real fsnotify event for
// path has passed through debounce and reconcile. Tests can then assert a
// negative result without using a short wall-clock observation window.
func WaitForCodexApprovalProcessingForTest(t *testing.T, m *codexApprovalMonitor, path string) {
	t.Helper()
	if m == nil {
		return
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case processed := <-m.processed:
			if processed == path {
				return
			}
		case <-m.done:
			t.Fatalf("Codex approval monitor stopped before processing %s", path)
		case <-timer.C:
			t.Fatalf("Codex approval monitor did not process %s", path)
		}
	}
}

// ResetPerfForTest clears the in-memory poll-timing rings (perf.go) so a
// flow test asserting on /api/perf starts from an empty recorder rather
// than samples recorded by earlier tests in the same process.
func ResetPerfForTest() { perfReset() }

type dashTestHandler struct{ inner http.Handler }

func (h *dashTestHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Cookie") == "" {
		r.AddCookie(&http.Cookie{
			Name:  dashboardCookieName,
			Value: dashboardSessionToken,
		})
	}
	if popupBaseURL != "" && r.Header.Get("Origin") == "" {
		r.Header.Set("Origin", popupBaseURL)
	}
	h.inner.ServeHTTP(w, r)
}
