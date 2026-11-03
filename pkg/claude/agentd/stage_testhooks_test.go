package agentd

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"
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
	s, err := newStageStatic(worktree, target, p.page.notifyReload, nil)
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

// PageSnapshotJobForTest is what a page snapshot asked its renderer for.
type PageSnapshotJobForTest struct {
	URL, Origin, Token string
	Width, Height      int
	ScrollX, ScrollY   int
	Overlay            string
	Patches            string
}

// SetPageSnapshotRenderForTest stands in for headless Chrome: render gets
// each job and answers the PNG bytes. It also answers the page's snapshot
// token, so a test can check the job carries it.
func SetPageSnapshotRenderForTest(render func(PageSnapshotJobForTest) ([]byte, error)) func() {
	prev := pageSnapshotRender
	pageSnapshotRender = func(_ context.Context, j pageSnapshotJob) ([]byte, error) {
		return render(PageSnapshotJobForTest{URL: j.URL, Origin: j.Origin, Token: j.Token, Width: j.Width, Height: j.Height,
			ScrollX: j.ScrollX, ScrollY: j.ScrollY, Overlay: j.OverlaySV, Patches: j.Patches})
	}
	return func() { pageSnapshotRender = prev }
}

// PageSnapshotTokenForTest is a page's snapshot token, by page id.
func PageSnapshotTokenForTest(id string) string {
	livePages.mu.Lock()
	defer livePages.mu.Unlock()
	if p := livePages.byID[id]; p != nil {
		return p.stage.snapshotToken
	}
	return ""
}

// FakeSandboxRootForTest stands in for an agent's mount namespace: a host
// path P is what the sandbox shows at Dir+P. Bump NS to model a relaunch
// into another sandbox; set Dead to model the sandbox exiting; set Err to
// model a sandbox that cannot be opened.
type FakeSandboxRootForTest struct {
	mu   sync.Mutex
	Dir  string
	ns   int
	dead bool
	err  error
}

// Relaunch models the agent coming back in another sandbox.
func (f *FakeSandboxRootForTest) Relaunch() { f.mu.Lock(); f.ns++; f.dead = false; f.mu.Unlock() }

// Fail makes opening the sandbox fail (nil restores it).
func (f *FakeSandboxRootForTest) Fail(err error) {
	f.mu.Lock()
	f.err = err
	f.dead = err != nil
	f.mu.Unlock()
}

type fakeSandboxRoot struct {
	f  *FakeSandboxRootForTest
	ns int
}

func (r *fakeSandboxRoot) Alive() bool {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	return !r.f.dead && r.f.ns == r.ns
}
func (r *fakeSandboxRoot) NS() string { return "mnt:" + strconv.Itoa(r.ns) }
func (r *fakeSandboxRoot) OpenDir(abs string) (int, error) {
	return unix.Open(filepath.Join(r.f.Dir, abs), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
}
func (r *fakeSandboxRoot) Close() {}

// SetPageSandboxRootForTest makes every agent's sandbox the fake.
func SetPageSandboxRootForTest(f *FakeSandboxRootForTest) (restore func()) {
	prev := pageFSOpenSandboxRoot
	pageFSOpenSandboxRoot = func(string) (pageFSRoot, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.err != nil {
			return nil, f.err
		}
		return &fakeSandboxRoot{f: f, ns: f.ns}, nil
	}
	return func() { pageFSOpenSandboxRoot = prev }
}

// SetPageFSViewRecheckForTest shortens how often a page rechecks its agent's
// generation.
func SetPageFSViewRecheckForTest(d time.Duration) (restore func()) {
	prev := pageFSViewRecheck
	pageFSViewRecheck = d
	return func() { pageFSViewRecheck = prev }
}
