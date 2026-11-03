package agentd

import (
	"net/http"
	"net/http/httptest"
)

// BuildPopupHandlerForTest returns the production dashboard-listener handler:
// the Host split between live-page origins (stage) and the dashboard mux, the
// latter wrapped in the cookie/Origin-injecting dashTestHandler so a stage
// test can prove the dashboard credential never crosses over.
func BuildPopupHandlerForTest() http.Handler {
	initDashboardToken()
	mux := http.NewServeMux()
	registerDashboardRoutes(mux)
	return &dashTestHandler{inner: buildPopupHandler(auditRequests(mux))}
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

// SetStageRootPIDForTest swaps how an agent's process-tree root is resolved.
func SetStageRootPIDForTest(fn func(agentID string) (int, error)) func() {
	prev := stageRootPID
	stageRootPID = fn
	return func() { stageRootPID = prev }
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
