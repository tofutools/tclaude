package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/gorilla/websocket"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/internal/backend/providers"
	backend "github.com/tofutools/tclaude/internal/backend/server"
)

func TestBrowserSessionAuthenticatesUnixBackendAndRejectsForeignRequests(t *testing.T) {
	parent, err := os.MkdirTemp("", "browser-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(parent)
	state := filepath.Join(parent, "state")
	if err := backend.Initialize(state); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backDone := make(chan error, 1)
	go func() { backDone <- backend.Serve(ctx, state, providers.NewRegistry()) }()
	waitBrowserBackend(t, state, cancel, backDone)
	view, err := Open(state, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	viewDone := make(chan error, 1)
	go func() { viewDone <- view.Serve(ctx) }()
	defer func() {
		cancel()
		for _, done := range []chan error{viewDone, backDone} {
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(7 * time.Second):
				t.Error("server did not stop")
			}
		}
	}()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: time.Second}
	request := func(method, path, origin string, body any) (int, []byte) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			data, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			reader = bytes.NewReader(data)
		}
		req, err := http.NewRequest(method, view.origin+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, data
	}
	if status, _ := request("GET", "/v2/snapshot", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated proxy: %d", status)
	}
	if status, _ := request("POST", "/session", "http://foreign.invalid", map[string]string{"token": view.bootstrap}); status != http.StatusForbidden {
		t.Fatalf("cross-origin login: %d", status)
	}
	if status, _ := request("POST", "/session", view.origin, map[string]string{"token": view.bootstrap}); status != http.StatusNoContent {
		t.Fatalf("login: %d", status)
	}
	if status, _ := request("POST", "/session", view.origin, map[string]string{"token": view.bootstrap}); status != http.StatusUnauthorized {
		t.Fatalf("bootstrap replay: %d", status)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, data := request("GET", "/v2/snapshot", "", nil)
		if status == http.StatusOK {
			if bytes.Contains(data, []byte(view.bootstrap)) {
				t.Fatal("bootstrap leaked")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("backend unavailable: %d %s", status, data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status, _ := request("POST", "/v2/groups", "", map[string]any{"id": "group", "name": "Group", "members": []string{}}); status != http.StatusForbidden {
		t.Fatalf("mutation without origin: %d", status)
	}
	if status, _ := request("POST", "/v2/groups", "http://foreign.invalid", map[string]any{"id": "group", "name": "Group", "members": []string{}}); status != http.StatusForbidden {
		t.Fatalf("foreign mutation: %d", status)
	}
	if status, data := request("POST", "/v2/groups", view.origin, map[string]any{"id": "group", "name": "Group", "members": []string{}}); status != http.StatusCreated {
		t.Fatalf("group create: %d %s", status, data)
	}
	if status, data := request("GET", "/v2/snapshot", "", nil); status != http.StatusOK || !bytes.Contains(data, []byte("Group")) {
		t.Fatalf("group not persisted: %d %s", status, data)
	}
	req, _ := http.NewRequest("GET", view.origin+"/v2/snapshot", nil)
	req.Host = "foreign.invalid"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatal("foreign Host accepted")
	}
	u, _ := url.Parse(view.origin)
	if len(jar.Cookies(u)) != 1 {
		t.Fatal("expected browser session")
	}
	if status, _ := request("DELETE", "/session", view.origin, nil); status != http.StatusNoContent {
		t.Fatalf("logout: %d", status)
	}
	if status, _ := request("GET", "/v2/snapshot", "", nil); status != http.StatusUnauthorized {
		t.Fatal("logout retained proxy authority")
	}
}

func TestBrowserRefusesPublicListenerAndExpiredBootstrap(t *testing.T) {
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "operator.token"), []byte(strings.Repeat("a", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"0.0.0.0:0", "[::]:0", "localhost:0", "example.invalid:80"} {
		if s, err := Open(state, address); err == nil {
			_ = s.Close()
			t.Fatalf("accepted %s", address)
		}
	}
	s, err := Open(state, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.bootstrapUntil = time.Now().Add(-time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	defer func() { cancel(); <-done }()
	req, _ := http.NewRequest("POST", s.origin+"/session", strings.NewReader(`{"token":"`+s.bootstrap+`"}`))
	req.Header.Set("Origin", s.origin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("expired bootstrap accepted")
	}
}

func TestBrowserAttachmentProxyClosesViewOnShutdown(t *testing.T) {
	parent, err := os.MkdirTemp("", "browser-ws-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(parent)
	state := filepath.Join(parent, "state")
	if err := backend.Initialize(state); err != nil {
		t.Fatal(err)
	}
	credential, err := os.ReadFile(filepath.Join(state, "operator.token"))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(state, "api.sock"))
	if err != nil {
		t.Fatal(err)
	}
	nativeClosed := make(chan struct{})
	upstream := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+string(credential) || r.Header.Get("Cookie") != "" {
			http.Error(w, "bad proxy credentials", http.StatusUnauthorized)
			return
		}
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		defer close(nativeClosed)
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err = conn.WriteMessage(kind, data); err != nil {
				return
			}
		}
	})}
	defer upstream.Close()
	go func() { _ = upstream.Serve(listener) }()
	view, err := Open(state, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- view.Serve(ctx) }()
	payload, _ := json.Marshal(map[string]string{"token": view.bootstrap})
	req, _ := http.NewRequest("POST", view.origin+"/session", bytes.NewReader(payload))
	req.Header.Set("Origin", view.origin)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 204 {
		t.Fatalf("login: %d", response.StatusCode)
	}
	headers := http.Header{"Origin": {view.origin}, "Cookie": {response.Cookies()[0].String()}}
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(view.origin, "http")+"/v2/attach", headers)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err = conn.WriteMessage(websocket.BinaryMessage, []byte("terminal input")); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.ReadMessage()
	if err != nil || string(data) != "terminal input" {
		t.Fatalf("terminal IO: %q %v", data, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("browser did not join proxy")
	}
	select {
	case <-nativeClosed:
	case <-time.After(time.Second):
		t.Fatal("upstream attachment remained open")
	}
}

func TestConcurrentDashboardSessionsRemainIndependent(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	if err := backend.Initialize(state); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	views := make([]*Server, 0, 2)
	for range 2 {
		view, err := Open(state, "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		views = append(views, view)
		done := make(chan error, 1)
		go func() { done <- view.Serve(ctx) }()
		t.Cleanup(func() {
			view.Close()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("dashboard did not stop")
			}
		})
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: time.Second}
	call := func(view *Server, method, path string, body string) int {
		t.Helper()
		req, err := http.NewRequest(method, view.origin+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", view.origin)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	for _, view := range views {
		if status := call(view, "POST", "/session", `{"token":"`+view.bootstrap+`"}`); status != http.StatusNoContent {
			t.Fatalf("login: %d", status)
		}
	}
	// No backend is running: 502 proves each request passed browser authentication.
	for _, view := range views {
		if status := call(view, "GET", "/v2/snapshot", ""); status != http.StatusBadGateway {
			t.Fatalf("independent session lost: %d", status)
		}
	}
	if status := call(views[1], "DELETE", "/session", ""); status != http.StatusNoContent {
		t.Fatalf("logout: %d", status)
	}
	if status := call(views[0], "GET", "/v2/snapshot", ""); status != http.StatusBadGateway {
		t.Fatalf("other logout invalidated session: %d", status)
	}
	if status := call(views[1], "GET", "/v2/snapshot", ""); status != http.StatusUnauthorized {
		t.Fatalf("logged out session remains authorized: %d", status)
	}
}
