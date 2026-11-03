package agentd_test

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// stageRootIs makes the agent's process tree start at pid.
func stageRootIs(t *testing.T, pid int) {
	t.Helper()
	t.Cleanup(agentd.SetStageRootPIDForTest(func(string) (int, error) { return pid, nil }))
}

// strangerPID is a live process outside the test's own tree.
func strangerPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd.Process.Pid
}

func listenerPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	return srv.Listener.Addr().(*net.TCPAddr).Port
}

func useAgentPort(t *testing.T, e *stageEnv, port int) {
	t.Helper()
	up, err := agentd.NewStageAgentPortForTest("agt_owner", port)
	require.NoError(t, err)
	e.page.SetUpstream(up)
}

func TestStageAgentPortProxiesAnOwnedListener(t *testing.T) {
	e := newStageEnv(t)
	seen := make(chan *http.Request, 1)
	dev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Clone(r.Context())
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><head></head><body>dev</body></html>")
	}))
	t.Cleanup(dev.Close)
	port := listenerPort(t, dev)
	// The test process owns the dev server's listener: it is the agent here.
	stageRootIs(t, os.Getpid())
	useAgentPort(t, e, port)

	res := e.do(t, http.MethodGet, e.host(), "/app?x=1", map[string]string{"Origin": "null"})
	body := readBody(t, res)
	require.Equal(t, http.StatusOK, res.StatusCode, body)
	assert.Contains(t, body, stageProbeMarker)
	got := <-seen
	hp := "localhost:" + strconv.Itoa(port)
	assert.Equal(t, hp, got.Host, "the dev server sees itself addressed as localhost")
	assert.Equal(t, "http://"+hp, got.Header.Get("Origin"))
	assert.Equal(t, "/app", got.URL.Path)
	assert.Empty(t, got.Header.Get("Cookie"))
}

func TestStageAgentPortRefusesAListenerTheAgentDoesNotOwn(t *testing.T) {
	e := newStageEnv(t)
	hits := 0
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits++ }))
	t.Cleanup(other.Close)
	stageRootIs(t, strangerPID(t))
	useAgentPort(t, e, listenerPort(t, other))

	res := e.do(t, http.MethodGet, e.host(), "/", nil)
	assert.Equal(t, http.StatusBadGateway, res.StatusCode)
	assert.Contains(t, readBody(t, res), "does not belong to the agent")
	assert.Zero(t, hits, "a listener not owned by the agent is never dialled")
	assertStageCSP(t, res)
}

func TestStageAgentPortNothingListening(t *testing.T) {
	e := newStageEnv(t)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	stageRootIs(t, os.Getpid())
	useAgentPort(t, e, port)

	res := e.do(t, http.MethodGet, e.host(), "/", nil)
	assert.Equal(t, http.StatusBadGateway, res.StatusCode)
	assert.Contains(t, readBody(t, res), "nothing is listening on 127.0.0.1:"+strconv.Itoa(port))
}

func TestStageAgentPortRefusesTheDaemonsPorts(t *testing.T) {
	e := newStageEnv(t)
	stageRootIs(t, os.Getpid()) // the test process owns the "daemon" listener too
	port, err := strconv.Atoi(e.port)
	require.NoError(t, err)
	t.Cleanup(agentd.RegisterStageDaemonPortForTest(port))

	_, err = agentd.NewStageAgentPortForTest("agt_owner", port)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "daemon's own")

	// A port that becomes the daemon's after the page was shown is refused
	// at dial time.
	dev := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(dev.Close)
	devPort := listenerPort(t, dev)
	useAgentPort(t, e, devPort)
	t.Cleanup(agentd.RegisterStageDaemonPortForTest(devPort))
	res := e.do(t, http.MethodGet, e.host(), "/", nil)
	assert.Equal(t, http.StatusBadGateway, res.StatusCode)
	assert.Contains(t, readBody(t, res), "daemon's own")
}

func TestStageAgentPortRootUnknown(t *testing.T) {
	e := newStageEnv(t)
	t.Cleanup(agentd.SetStageRootPIDForTest(func(string) (int, error) {
		return 0, errors.New("internal detail")
	}))
	useAgentPort(t, e, 5173)
	res := e.do(t, http.MethodGet, e.host(), "/", nil)
	assert.Equal(t, http.StatusBadGateway, res.StatusCode)
	assert.NotContains(t, readBody(t, res), "internal detail")
}

func TestStageAgentPortProxiesWebsocketUpgrade(t *testing.T) {
	e := newStageEnv(t)
	var origin, host string
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool {
		origin, host = r.Header.Get("Origin"), r.Host
		return r.Header.Get("Origin") == "http://"+r.Host
	}}
	dev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, http.Header{"Set-Cookie": {"ws=1"}})
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			_ = c.WriteMessage(mt, append([]byte("echo:"), msg...))
		}
	}))
	t.Cleanup(dev.Close)
	port := listenerPort(t, dev)
	stageRootIs(t, os.Getpid())
	useAgentPort(t, e, port)

	dialer := websocket.Dialer{
		NetDial:          func(string, string) (net.Conn, error) { return net.Dial("tcp", e.srv.Listener.Addr().String()) },
		HandshakeTimeout: 5 * time.Second,
	}
	conn, res, err := dialer.Dial("ws://"+e.host()+"/@vite/hmr", http.Header{
		"Origin":         {"null"},
		"Sec-Fetch-Dest": {"websocket"},
		"Cookie":         {"tclaude_dashboard=x"},
	})
	if err != nil && res != nil {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("handshake: %v: %d %s", err, res.StatusCode, b)
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	assert.Equal(t, http.StatusSwitchingProtocols, res.StatusCode)
	assert.Empty(t, res.Header.Values("Set-Cookie"))
	assert.NotEmpty(t, res.Header.Get("Content-Security-Policy"))
	assert.Equal(t, "http://localhost:"+strconv.Itoa(port), origin)
	assert.Equal(t, "localhost:"+strconv.Itoa(port), host)

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("hi")))
	_, msg, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, "echo:hi", string(msg))
}

// --- static ---

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func TestStageStaticServesTheWorktreeWithoutFollowingLinks(t *testing.T) {
	e := newStageEnv(t)
	wt := testutil.CanonicalTempDir(t)
	outside := testutil.CanonicalTempDir(t)
	writeFile(t, filepath.Join(outside, "secret.html"), "SECRET")
	writeFile(t, filepath.Join(wt, "site", "index.html"), "<html><head></head><body>home</body></html>")
	writeFile(t, filepath.Join(wt, "site", "app.js"), "console.log(1)")
	writeFile(t, filepath.Join(wt, "site", "sub", "index.html"), "<p>sub</p>")
	writeFile(t, filepath.Join(wt, "site", ".env.json"), "{}")
	writeFile(t, filepath.Join(wt, "site", "notes.sh"), "rm -rf /")
	writeFile(t, filepath.Join(wt, "elsewhere.html"), "SIBLING")
	require.NoError(t, os.Symlink(filepath.Join(outside, "secret.html"), filepath.Join(wt, "site", "out.html")))
	require.NoError(t, os.Symlink(outside, filepath.Join(wt, "site", "outdir")))
	require.NoError(t, os.Symlink(filepath.Join(wt, "site", "app.js"), filepath.Join(wt, "site", "in.js")))
	require.NoError(t, e.page.UseStaticSource(wt, "site"))

	get := func(p string) (int, string) {
		res := e.do(t, http.MethodGet, e.host(), p, nil)
		return res.StatusCode, readBody(t, res)
	}
	code, body := get("/")
	assert.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "</script></head><body>home")
	res := e.do(t, http.MethodGet, e.host(), "/app.js", map[string]string{"Sec-Fetch-Dest": "script"})
	assert.Equal(t, http.StatusOK, res.StatusCode)
	assert.Equal(t, "text/javascript; charset=utf-8", res.Header.Get("Content-Type"))
	assertStageCSP(t, res)
	code, body = get("/sub/")
	assert.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "<p>sub</p>")
	code, body = get("/sub") // redirected to /sub/
	assert.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "<p>sub</p>")

	for _, p := range []string{
		"/out.html", "/outdir/secret.html", "/in.js", // symlinks, out and in
		"/.env.json", "/notes.sh", // dotfile, disallowed extension
		"/../elsewhere.html", "/%2e%2e/elsewhere.html", "/sub/../../elsewhere.html",
		"/missing.html",
	} {
		code, body := get(p)
		assert.Equal(t, http.StatusNotFound, code, p)
		assert.NotContains(t, body, "SECRET", p)
		assert.NotContains(t, body, "SIBLING", p)
	}
	assert.Equal(t, http.StatusMethodNotAllowed, e.do(t, http.MethodPost, e.host(), "/", nil).StatusCode)
}

func TestStageStaticRefusesTargetsOutsideTheWorktree(t *testing.T) {
	e := newStageEnv(t)
	wt := testutil.CanonicalTempDir(t)
	outside := testutil.CanonicalTempDir(t)
	writeFile(t, filepath.Join(outside, "x.html"), "x")
	require.NoError(t, os.Symlink(outside, filepath.Join(wt, "linked")))
	writeFile(t, filepath.Join(wt, "run.sh"), "x")

	for _, target := range []string{"..", filepath.Join(outside, "x.html"), "linked", "linked/x.html", "run.sh", "missing"} {
		err := e.page.UseStaticSource(wt, target)
		assert.Error(t, err, target)
	}
}

func TestStageStaticSingleFileAndReloadOnSave(t *testing.T) {
	e := newStageEnv(t)
	wt := testutil.CanonicalTempDir(t)
	file := filepath.Join(wt, "mock.html")
	writeFile(t, file, "<html><head></head>v1</html>")
	writeFile(t, filepath.Join(wt, "other.html"), "OTHER")
	require.NoError(t, e.page.UseStaticSource(wt, file))

	reloads, cancel := e.page.SubscribeReload()
	t.Cleanup(cancel)

	for _, p := range []string{"/", "/mock.html"} {
		res := e.do(t, http.MethodGet, e.host(), p, nil)
		assert.Equal(t, http.StatusOK, res.StatusCode, p)
		assert.Contains(t, readBody(t, res), "v1")
	}
	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, e.host(), "/other.html", nil).StatusCode,
		"a single-file page serves only that file")

	writeFile(t, file, "<html><head></head>v2</html>")
	select {
	case <-reloads:
	case <-time.After(5 * time.Second):
		t.Fatal("a save did not produce a reload event")
	}
	assert.Contains(t, readBody(t, e.do(t, http.MethodGet, e.host(), "/", nil)), "v2")
}

func TestStageStaticDirectoryReloadsOnNestedSave(t *testing.T) {
	e := newStageEnv(t)
	wt := testutil.CanonicalTempDir(t)
	writeFile(t, filepath.Join(wt, "index.html"), "<p>x</p>")
	require.NoError(t, e.page.UseStaticSource(wt, "."))
	reloads, cancel := e.page.SubscribeReload()
	t.Cleanup(cancel)

	require.NoError(t, os.MkdirAll(filepath.Join(wt, "css"), 0o755))
	time.Sleep(200 * time.Millisecond) // let the new directory's watch land
	drain(reloads)
	writeFile(t, filepath.Join(wt, "css", "site.css"), "body{}")
	select {
	case <-reloads:
	case <-time.After(5 * time.Second):
		t.Fatal("a nested save did not produce a reload event")
	}
}

func drain(ch <-chan struct{}) {
	for {
		select {
		case <-ch:
		case <-time.After(300 * time.Millisecond):
			return
		}
	}
}
