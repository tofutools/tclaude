package agentd

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
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

// SetPopupBaseURLForTest overrides the popup base URL so flow tests
// can reach the X-Tclaude-Ask-Human escalation branch without binding
// a real loopback HTTP server. Returns a restore function tests can
// schedule via t.Cleanup.
func SetPopupBaseURLForTest(url string) func() {
	prev := popupBaseURL
	popupBaseURL = url
	return func() { popupBaseURL = prev }
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

// BuildDashboardHandlerForTest exposes the dashboard mux (the
// loopback-port mux that hosts `/`, `/api/snapshot`,
// `/api/groups/...` mutation endpoints, and the popup `/approve/...`
// route in production). Flow tests use this when asserting the
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
	return &dashTestHandler{inner: mux}
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

// SetOpenTerminalForTest swaps the terminal-spawning seam so flow
// tests can assert the `dir` endpoints' open path without popping a
// real window (and without failing on CI hosts that have no terminal
// emulator). Returns a restore function for t.Cleanup.
func SetOpenTerminalForTest(fn func(string) error) func() {
	prev := openTerminal
	openTerminal = fn
	return func() { openTerminal = prev }
}

// SetWorkflowProjectDirsForTest overrides the project-source dirs used to
// resolve / list workflow templates, so flow tests can point instantiation at a
// fixture template dir instead of the daemon's cwd. Returns a restore function.
func SetWorkflowProjectDirsForTest(dirs ...string) func() {
	prev := workflowProjectDirsFn
	workflowProjectDirsFn = func() []string { return dirs }
	return func() { workflowProjectDirsFn = prev }
}

// SetWorkflowEngineEnabledForTest flips the engine's opt-in gate (off by default
// in production). A flow test that drives the engine must enable it first.
// Returns a restore function for t.Cleanup.
func SetWorkflowEngineEnabledForTest(enabled bool) func() {
	prev := workflowEngineEnabled
	workflowEngineEnabled = enabled
	return func() { workflowEngineEnabled = prev }
}

// RunWorkflowEngineTickForTest drives one synchronous engine sweep, so flow
// tests can advance the autonomous runner deterministically instead of waiting
// on the 5s ticker. Uses a background context. The engine gate must be enabled
// (SetWorkflowEngineEnabledForTest) or the tick is a no-op.
func RunWorkflowEngineTickForTest() {
	runWorkflowEngineTick(context.Background())
}

// WorkflowEngineAssigneeForTest exposes the engine-owner sentinel so a flow test
// can stamp it to simulate an engine-claimed (vs human-driven) running node.
func WorkflowEngineAssigneeForTest() string { return engineAssignee }

// ReapOrphanedEngineNodesForTest runs the startup orphan-node recovery so a flow
// test can assert that a tool/program node left `running` by a "crashed" daemon
// is reset to `ready`. Independent of the engine gate (reaping is always safe).
func ReapOrphanedEngineNodesForTest() {
	reapOrphanedEngineNodes()
}

// SetWorkflowNodeRunTimeoutForTest shrinks the per-node command timeout so a
// hung command fails fast under test. Returns a restore function.
func SetWorkflowNodeRunTimeoutForTest(d time.Duration) func() {
	prev := workflowNodeRunTimeout
	workflowNodeRunTimeout = d
	return func() { workflowNodeRunTimeout = prev }
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

// RegisterPopupRoutesForTest mounts the approval-popup route
// (`/approve/...`) on mux so flow tests can exercise handlePopupApprove
// without binding a real loopback listener.
func RegisterPopupRoutesForTest(mux *http.ServeMux) {
	mux.HandleFunc("/approve/", handlePopupApprove)
}

// SeedPendingApprovalForTest registers a minimal pending approval under
// id so flow tests can drive handlePopupApprove against it. The
// decision channel is buffered, so a POST approve/deny records without
// a blocked reader. Returns a cleanup that removes the entry.
func SeedPendingApprovalForTest(id string) func() {
	req := &approvalRequest{
		id:        id,
		perm:      "self.rename",
		decision:  make(chan bool, 1),
		extend:    make(chan time.Duration, 1),
		createdAt: time.Now(),
		timeout:   60 * time.Second,
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

// MintApproveInitTokenForTest mints a single-use init token scoped to
// the approval popup for id — what tclaude agentd and the tray embed
// in the URL they launch.
func MintApproveInitTokenForTest(id string) string {
	return mintInitToken(initScopeApprove(id))
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
