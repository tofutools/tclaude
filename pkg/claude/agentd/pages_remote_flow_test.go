package agentd_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Live pages for a dashboard on another machine (tcl-balzk0): the operator
// reaches the dashboard listener by a LAN address, a VPN name or through a
// TLS-terminating proxy, where *.ui.localhost means nothing. Such a viewer
// frames each page at /p/<remote token>/ on the host it used. All pages share
// that origin with the dashboard, so isolation rests on the page's CSP naming
// only its own path, redirects held to it, and the dashboard API's own
// Origin and cookie checks. The browser enforces the CSP; these tests pin the
// policy it is given and every check the daemon makes.

const remoteHost = "box.tail1234.ts.net"

// remoteSnapshotPages is the snapshot as a dashboard reached at host sees it.
func (e *pageEnv) remoteSnapshotPages(t *testing.T, host string) []pageOut {
	t.Helper()
	r := testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil)
	r.Host = host
	r.Header.Set("Origin", e.srv.URL)
	rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(), r)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var snap struct {
		LivePages []pageOut `json:"live_pages"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap))
	return snap.LivePages
}

func (e *pageEnv) remoteURL(t *testing.T, id string) string {
	t.Helper()
	for _, p := range e.remoteSnapshotPages(t, remoteHost) {
		if p.ID == id {
			return p.URL
		}
	}
	t.Fatalf("no page %s in the remote snapshot", id)
	return ""
}

// remoteGet requests path on srv as a browser at host would, with headers.
func remoteGet(t *testing.T, srv *httptest.Server, method, host, path string, headers map[string]string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, body)
	require.NoError(t, err)
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func readAll(t *testing.T, res *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return string(b)
}

var iframe = map[string]string{"Sec-Fetch-Dest": "iframe"}

// cspSources maps each directive to its sources.
func cspSources(csp string) map[string][]string {
	out := map[string][]string{}
	for _, d := range strings.Split(csp, ";") {
		f := strings.Fields(d)
		if len(f) > 0 {
			out[f[0]] = f[1:]
		}
	}
	return out
}

// assertPagePolicy checks a remote page's CSP names nothing but its own path
// on host: no 'self' (the dashboard's origin), no other page, no host-wide
// source.
func assertPagePolicy(t *testing.T, csp, host, prefix string) {
	t.Helper()
	own := map[string]bool{
		"http://" + host + prefix + "/": true, "https://" + host + prefix + "/": true,
		"ws://" + host + prefix + "/": true, "wss://" + host + prefix + "/": true,
		"'unsafe-inline'": true, "'unsafe-eval'": true, "data:": true, "blob:": true,
	}
	assert.NotContains(t, csp, "'self'", "'self' is the dashboard's origin here")
	for directive, sources := range cspSources(csp) {
		switch directive {
		case "sandbox", "object-src":
			continue
		case "frame-ancestors":
			assert.ElementsMatch(t, []string{"http://" + host, "https://" + host}, sources, "only this dashboard frames the page")
			continue
		}
		require.NotEmpty(t, sources, directive)
		for _, s := range sources {
			assert.True(t, own[s], "%s allows %q: only the page's own path may be named", directive, s)
		}
	}
	assert.Contains(t, csp, "connect-src http://"+host+prefix+"/ https://"+host+prefix+"/ ws://"+host+prefix+"/ wss://"+host+prefix+"/")
}

func TestPageRemoteViewerFramesItsPageByPathOverPlainHTTP(t *testing.T) {
	e := newPageEnv(t)
	a := e.agent(t, "remote", true)
	site := staticSite(t, a.cwd)
	p, rec := e.show(t, a, site, "site", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "remote_base", "a static page's remote token is never shown to the agent")
	assert.NotContains(t, rec.Body.String(), agentd.PageRemoteTokenForTest(p.ID))

	// The local dashboard keeps the loopback origin; another machine gets a
	// path on the host it used, and no loopback token.
	for _, local := range []string{"127.0.0.1:7000", "localhost:7000", "[::1]:7000"} {
		for _, sp := range e.remoteSnapshotPages(t, local) {
			if sp.ID == p.ID {
				assert.Equal(t, p.Origin, sp.Origin, local)
			}
		}
	}
	for _, host := range []string{remoteHost, "192.168.1.20:7000", "box.lan"} {
		for _, sp := range e.remoteSnapshotPages(t, host) {
			if sp.ID == p.ID {
				assert.Empty(t, sp.Origin, host)
				assert.Regexp(t, `^/p/[0-9a-f]{32}/$`, sp.URL, host)
				assert.NotContains(t, sp.URL, strings.TrimSuffix(strings.TrimPrefix(p.Origin, "http://f-"), ".ui.localhost"))
			}
		}
	}
	path := e.remoteURL(t, p.ID)
	prefix := strings.TrimSuffix(path, "/")

	res := remoteGet(t, e.srv, http.MethodGet, remoteHost, path, iframe, nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	body := readAll(t, res)
	assert.Contains(t, body, "home")
	assert.Contains(t, body, "data-tclaude-probe", "the probe goes in, as on the loopback stage")
	assertPagePolicy(t, res.Header.Get("Content-Security-Policy"), remoteHost, prefix)

	res = remoteGet(t, e.srv, http.MethodGet, remoteHost, prefix+"/public/ok.html", iframe, nil)
	assert.Equal(t, http.StatusOK, res.StatusCode)
	assert.Contains(t, readAll(t, res), "public", "a static source sees the path without the prefix")

	// The stage's other rules hold.
	res = remoteGet(t, e.srv, http.MethodGet, remoteHost, path, map[string]string{"Sec-Fetch-Dest": "document"}, nil)
	assert.Equal(t, http.StatusForbidden, res.StatusCode, "no top-level visit")
	res = remoteGet(t, e.srv, http.MethodGet, remoteHost, "/p/"+strings.Repeat("0", 32)+"/", iframe, nil)
	assert.Equal(t, http.StatusNotFound, res.StatusCode, "an unknown token")
	res = remoteGet(t, e.srv, http.MethodGet, remoteHost, prefix+"/public/../index.html", iframe, nil)
	assert.Equal(t, http.StatusNotFound, res.StatusCode, "a path that leaves itself")

	// Closed: the path stops answering.
	require.Equal(t, http.StatusNoContent, e.call(t, a, http.MethodDelete, "/v1/pages/site", nil).Code)
	res = remoteGet(t, e.srv, http.MethodGet, remoteHost, path, iframe, nil)
	assert.Equal(t, http.StatusNotFound, res.StatusCode)
}

func TestPageRemoteViewerPageCannotReachAnotherPage(t *testing.T) {
	e := newPageEnv(t)
	a := e.agent(t, "remoteab", true)
	pa, rec := e.show(t, a, ":"+strconv.Itoa(devServer(t, "A")), "a", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	pb, rec := e.show(t, a, ":"+strconv.Itoa(devServer(t, "B")), "b", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	prefixA := strings.TrimSuffix(e.remoteURL(t, pa.ID), "/")
	prefixB := strings.TrimSuffix(e.remoteURL(t, pb.ID), "/")
	require.NotEqual(t, prefixA, prefixB)

	res := remoteGet(t, e.srv, http.MethodGet, remoteHost, prefixA+"/", iframe, nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	csp := res.Header.Get("Content-Security-Policy")
	assertPagePolicy(t, csp, remoteHost, prefixA)
	assert.NotContains(t, csp, prefixB, "A's policy names nothing of B's: A can neither load nor fetch it")

	// A request that names the shared origin (the dashboard's, which every
	// page here shares) is not a page's own and is refused; a page's own
	// requests carry Origin: null.
	for _, origin := range []string{"http://" + remoteHost, "https://" + remoteHost, "http://evil.example"} {
		res = remoteGet(t, e.srv, http.MethodGet, remoteHost, prefixB+"/", map[string]string{"Sec-Fetch-Dest": "empty", "Origin": origin}, nil)
		assert.Equal(t, http.StatusForbidden, res.StatusCode, origin)
	}
}

func TestPageRemoteViewerPageCannotUseTheDashboardAPI(t *testing.T) {
	e := newPageEnv(t)
	a := e.agent(t, "remoteapi", true)
	p, rec := e.show(t, a, ":"+strconv.Itoa(devServer(t, "x")), "app", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	strict := httptest.NewServer(agentd.BuildStrictPopupHandlerForTest())
	t.Cleanup(strict.Close)
	session := agentd.DashboardCookieNameForTest() + "=" + agentd.DashboardSessionCookieForTest()
	marks, _ := json.Marshal(map[string]any{"note": "from the page"})

	for _, bind := range []string{"127.0.0.1", "0.0.0.0"} {
		t.Run("bind "+bind, func(t *testing.T) {
			t.Cleanup(agentd.SetDashboardBindHostForTest(bind))
			cases := []struct {
				name    string
				method  string
				path    string
				headers map[string]string
				body    string
			}{
				// A fetch or XHR from the frame: Origin: null.
				{"fetch", http.MethodGet, "/api/snapshot", map[string]string{"Origin": "null", "Sec-Fetch-Dest": "empty"}, ""},
				// Were the cookie sent anyway (it is SameSite=Strict, and
				// the frame is credentialless), Origin: null still fails.
				{"fetch with the cookie", http.MethodGet, "/api/snapshot", map[string]string{"Origin": "null", "Cookie": session}, ""},
				// A no-cors image: no Origin, and no Referer (no-referrer).
				{"image", http.MethodGet, "/api/snapshot", map[string]string{"Sec-Fetch-Dest": "image"}, ""},
				{"image with the cookie", http.MethodGet, "/api/snapshot", map[string]string{"Sec-Fetch-Dest": "image", "Cookie": session}, ""},
				// A form POST from the frame: Origin: null.
				{"form post", http.MethodPost, "/api/pages/" + p.ID + "/marks",
					map[string]string{"Origin": "null", "Content-Type": "application/x-www-form-urlencoded"}, "note=x"},
				{"form post with the cookie", http.MethodPost, "/api/pages/" + p.ID + "/marks",
					map[string]string{"Origin": "null", "Cookie": session, "Content-Type": "application/json"}, string(marks)},
				{"close with the cookie", http.MethodDelete, "/api/pages/" + p.ID, map[string]string{"Origin": "null", "Cookie": session}, ""},
			}
			for _, c := range cases {
				res := remoteGet(t, strict, c.method, remoteHost, c.path, c.headers, strings.NewReader(c.body))
				body := readAll(t, res)
				assert.Equal(t, http.StatusForbidden, res.StatusCode, "%s: %s", c.name, body)
				assert.NotContains(t, body, `"live_pages"`, c.name)
			}
		})
	}
	got := e.marks(t, a, "app", "")
	assert.Empty(t, got.Batches, "no request from a page reached the marks")
	assert.Len(t, e.ls(t, a), 1, "nor closed the page")
}

func TestPageRemoteViewerDevServerUnderItsBase(t *testing.T) {
	e := newPageEnv(t)
	a := e.agent(t, "remotebase", true)
	var (
		mu   sync.Mutex
		seen []*http.Request
	)
	other := "/p/" + strings.Repeat("a", 32) + "/"
	dev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Clone(r.Context()))
		mu.Unlock()
		rest := r.URL.Path[strings.Index(r.URL.Path[3:], "/")+3:]
		switch rest {
		case "/to-api":
			http.Redirect(w, r, "/api/snapshot", http.StatusFound)
		case "/to-other":
			http.Redirect(w, r, other, http.StatusFound)
		case "/to-elsewhere":
			http.Redirect(w, r, "http://evil.example/x", http.StatusFound)
		case "/to-up":
			http.Redirect(w, r, "../../api/snapshot", http.StatusFound)
		case "/to-next":
			http.Redirect(w, r, "next?x=1", http.StatusFound)
		case "/to-own-abs":
			http.Redirect(w, r, "https://"+remoteHost+r.URL.Path[:strings.Index(r.URL.Path[3:], "/")+3]+"/next", http.StatusFound)
		default:
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<html><head></head><body>dev "+r.URL.Path+"</body></html>")
		}
	}))
	t.Cleanup(dev.Close)
	port := strconv.Itoa(listenerPort(t, dev))

	p, rec := e.show(t, a, ":"+port, "app", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var shown struct {
		RemoteBase string `json:"remote_base"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &shown))
	require.Regexp(t, `^/p/[0-9a-f]{32}/$`, shown.RemoteBase, "show tells the agent the --base its dev server needs")
	assert.Equal(t, shown.RemoteBase, e.remoteURL(t, p.ID), "before the re-show the remote frame is at the base")

	// The agent re-shows at its base; local and remote load the same path.
	_, rec = e.show(t, a, ":"+port+shown.RemoteBase, "app", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, shown.RemoteBase, e.remoteURL(t, p.ID), "the base is not doubled")
	prefix := strings.TrimSuffix(shown.RemoteBase, "/")

	res := remoteGet(t, e.srv, http.MethodGet, remoteHost, shown.RemoteBase, map[string]string{
		"Sec-Fetch-Dest": "iframe",
		"Cookie":         agentd.DashboardCookieNameForTest() + "=" + agentd.DashboardSessionCookieForTest() + "; tclaude_remote_session=s",
	}, nil)
	require.Equal(t, http.StatusOK, res.StatusCode)
	assert.Contains(t, readAll(t, res), "dev "+shown.RemoteBase, "a dev server keeps its base")
	mu.Lock()
	last := seen[len(seen)-1]
	mu.Unlock()
	assert.Empty(t, last.Header.Get("Cookie"), "no dashboard or remote session cookie reaches the upstream")

	for _, path := range []string{"/to-api", "/to-other", "/to-elsewhere", "/to-up"} {
		res := remoteGet(t, e.srv, http.MethodGet, remoteHost, prefix+path, iframe, nil)
		assert.Equal(t, http.StatusBadGateway, res.StatusCode, path)
		assert.Empty(t, res.Header.Get("Location"), path)
		assert.Contains(t, readAll(t, res), "outside its own path", path)
	}
	res = remoteGet(t, e.srv, http.MethodGet, remoteHost, prefix+"/to-next", iframe, nil)
	assert.Equal(t, http.StatusFound, res.StatusCode)
	assert.Equal(t, prefix+"/next?x=1", res.Header.Get("Location"), "a redirect within the page's path passes")
	res = remoteGet(t, e.srv, http.MethodGet, remoteHost, prefix+"/to-own-abs", iframe, nil)
	assert.Equal(t, http.StatusFound, res.StatusCode)
	assert.Equal(t, prefix+"/next", res.Header.Get("Location"), "an absolute one on this host is made relative")
}

func TestPageRemoteViewerStaticRedirectKeepsThePrefix(t *testing.T) {
	e := newPageEnv(t)
	a := e.agent(t, "remotedir", true)
	site := staticSite(t, a.cwd)
	p, rec := e.show(t, a, site, "site", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	prefix := strings.TrimSuffix(e.remoteURL(t, p.ID), "/")
	res := remoteGet(t, e.srv, http.MethodGet, remoteHost, prefix+"/public", iframe, nil)
	require.True(t, res.StatusCode >= 300 && res.StatusCode < 400, "a directory without its slash redirects: %d", res.StatusCode)
	loc, err := url.Parse(res.Header.Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, prefix+"/public/", loc.Path, "and the redirect keeps the prefix the static source never saw")
}

func TestPageRemoteViewerSkipsAPageThatLoadsOtherHosts(t *testing.T) {
	e := newPageEnv(t)
	a := e.agent(t, "remoteother", true)
	p, rec := e.show(t, a, ":"+strconv.Itoa(devServer(t, "x")), "app", "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	path := e.remoteURL(t, p.ID)
	require.True(t, agentd.SetPageOtherHostsForTest(p.ID, true))
	assert.Empty(t, e.remoteURL(t, p.ID), "its wider sources would reach every page on the shared origin")
	res := remoteGet(t, e.srv, http.MethodGet, remoteHost, path, iframe, nil)
	assert.Equal(t, http.StatusNotFound, res.StatusCode)
	assert.Regexp(t, regexp.MustCompile(`^http://f-[0-9a-f]{32}\.ui\.localhost`), p.Origin, "the local dashboard still shows it")
}
