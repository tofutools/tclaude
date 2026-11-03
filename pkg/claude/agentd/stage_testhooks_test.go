package agentd

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"time"
)

// BuildPopupHandlerForTest returns the production dashboard-listener handler:
// the Host split between live-page origins (stage) and the dashboard mux. Only
// the dashboard branch gets the cookie/Origin-injecting dashTestHandler; stage
// requests arrive exactly as the test sent them, as a frame's would.
func BuildPopupHandlerForTest() http.Handler {
	initDashboardToken()
	mux := http.NewServeMux()
	registerDashboardRoutes(mux)
	return buildPopupHandler(&dashTestHandler{inner: auditRequests(mux)})
}

// DashboardSessionCookieForTest is the live dashboard session cookie value.
func DashboardSessionCookieForTest() string { return dashboardSessionToken }

// StagePageForTest is a page opened on the stage for a test.
type StagePageForTest struct {
	Token         string
	SnapshotToken string
	Host          string // host name without port
	page          *stagePage
}

// SetUpstream swaps the page's upstream in place.
func (p *StagePageForTest) SetUpstream(up http.RoundTripper) { p.page.setUpstream(up) }

// SetAllowOtherHosts flips the operator's "allow other hosts" toggle.
func (p *StagePageForTest) SetAllowOtherHosts(on bool) {
	p.page.mu.Lock()
	p.page.allowOtherHosts = on
	p.page.mu.Unlock()
}

// OpenStagePageForTest registers a page with the given upstream.
func OpenStagePageForTest(owner, name string, up http.RoundTripper) *StagePageForTest {
	p := stagePages.open(owner, name, up)
	return &StagePageForTest{Token: p.token, SnapshotToken: p.snapshotToken, Host: stageHost(p.token), page: p}
}

// CloseStagePageForTest closes a page, forgetting its token.
func CloseStagePageForTest(token string) { stagePages.close(token) }

// HandlerTransport adapts an http.Handler into a RoundTripper, the fake
// upstream for stage tests. It never dials.
type HandlerTransport struct{ Handler http.Handler }

func (t HandlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.Handler.ServeHTTP(rec, r)
	res := rec.Result()
	res.Request = r
	return res, nil
}

// SetStageAgentForTest swaps how an agent's process tree is resolved: its
// root pid and whether its launch has a private network namespace.
func SetStageAgentForTest(fn func(agentID string) (root int, privateNet bool, err error)) func() {
	prev := stageResolveAgent
	stageResolveAgent = func(agentID string) (stageAgentProc, error) {
		root, private, err := fn(agentID)
		return stageAgentProc{root: root, privateNet: private}, err
	}
	return func() { stageResolveAgent = prev }
}

// SetStageNetnsForTest swaps the sandbox-entry lookup and the in-namespace
// dial.
func SetStageNetnsForTest(entry func(root int) int, dial func(ctx context.Context, pid, port int) (net.Conn, error)) func() {
	prevEntry, prevDial := stageForeignNetns, stageNetnsDial
	stageForeignNetns, stageNetnsDial = entry, dial
	return func() { stageForeignNetns, stageNetnsDial = prevEntry, prevDial }
}

// RegisterStageDaemonPortForTest marks a port as the daemon's own.
func RegisterStageDaemonPortForTest(port int) func() {
	registerStageDaemonPort(port)
	return func() { stageDaemonPorts.Delete(port) }
}

// NewStageAgentPortForTest builds the agent-port source.
func NewStageAgentPortForTest(agentID string, port int) (http.RoundTripper, error) {
	return newStageAgentPort(agentID, port)
}

// UseStaticSource points the page at a static directory or file and wires
// its watcher to the page's reload signal.
func (p *StagePageForTest) UseStaticSource(worktree, target string) error {
	s, err := newStageStatic(worktree, target, p.page.notifyReload)
	if err != nil {
		return err
	}
	p.page.setUpstream(s)
	return nil
}

// SubscribeReload exposes the page's reload signal.
func (p *StagePageForTest) SubscribeReload() (<-chan struct{}, func()) {
	return p.page.subscribeReload()
}

// CloseAllLivePagesForTest closes every live page, so tests do not see each
// other's pages in the process-wide registry.
func CloseAllLivePagesForTest() {
	for _, p := range livePages.list("") {
		livePages.closeByID(p.id)
	}
}

// OpenLivePageForTest registers a live page directly, as a show would, with
// the given upstream: the dashboard's snapshot lists it and its origin serves
// it. It answers the page id. For fixtures (docs screenshots) that need a page
// without an agent process or a dev server.
func OpenLivePageForTest(owner, name, title, target, kind, worktree, start string, up http.RoundTripper) string {
	now := time.Now().UTC()
	reg := livePages
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.rev++
	p := &livePage{
		id: newPageID(), owner: owner, name: name, title: title, target: target, kind: kind, worktree: worktree,
		path: start, state: pageStateOpen, rev: reg.rev, shownAt: now, updated: now,
		stage: stagePages.open(owner, name, up),
	}
	if reg.byOwner[owner] == nil {
		reg.byOwner[owner] = map[string]*livePage{}
	}
	reg.byOwner[owner][name] = p
	reg.byID[p.id] = p
	return p.id
}
