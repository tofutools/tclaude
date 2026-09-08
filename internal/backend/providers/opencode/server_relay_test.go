package opencode

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestServerRelayForwardsAuthenticatedHTTPAndClosesIdleStreamsOnExit(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "oc-relay-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	path := filepath.Join(root, "control")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	require.NoError(t, err)
	listener.SetUnlinkOnClose(false)
	file, err := listener.File()
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })
	require.NoError(t, listener.Close())
	reservation, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	target := reservation.Addr().String()
	require.NoError(t, reservation.Close())
	executable, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("TCLAUDE_RELAY_FIXTURE_TARGET", target)
	t.Setenv("TCLAUDE_RELAY_FIXTURE_READY", filepath.Join(root, "ready"))
	descriptor, err := syscall.Dup(int(file.Fd()))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ExecuteServerRelay(ctx, ServerRelayRequest{ListenerFD: descriptor, Target: target, Executable: executable, Args: []string{"-test.run=^TestServerRelayNativeFixture$"}})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("relay did not settle after cancellation")
		}
	})
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(root, "ready")); return err == nil }, 5*time.Second, 10*time.Millisecond)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	response, err := client.Get("http://control/session")
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, response.StatusCode)
	require.NoError(t, response.Body.Close())
	request, err := http.NewRequest(http.MethodGet, "http://control/session", nil)
	require.NoError(t, err)
	request.SetBasicAuth("opencode", "disposable-secret")
	response, err = client.Do(request)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, "exact session", string(body))
	// An idle stream must not keep the supervisor alive after its server exits.
	idle, err := net.Dial("unix", path)
	require.NoError(t, err)
	defer idle.Close()
	cancel()
	require.NoError(t, idle.SetReadDeadline(time.Now().Add(3*time.Second)))
	_, err = idle.Read(make([]byte, 1))
	require.Error(t, err)
	var timeout net.Error
	require.False(t, errors.As(err, &timeout) && timeout.Timeout(), "idle relay stream must close")
}

func TestServerRelayRejectsNonUnixListenerBeforeStartingNativeCommand(t *testing.T) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	require.NoError(t, err)
	defer listener.Close()
	file, err := listener.File()
	require.NoError(t, err)
	defer file.Close()
	descriptor, err := syscall.Dup(int(file.Fd()))
	require.NoError(t, err)
	marker := filepath.Join(t.TempDir(), "must-not-run")
	err = ExecuteServerRelay(context.Background(), ServerRelayRequest{ListenerFD: descriptor, Target: "127.0.0.1:1234", Executable: "/bin/sh", Args: []string{"-c", "touch \"$1\"", "fixture", marker}})
	require.ErrorContains(t, err, "inherited Unix listener")
	require.NoFileExists(t, marker)
}

func TestServerRelayNativeFixture(t *testing.T) {
	target := os.Getenv("TCLAUDE_RELAY_FIXTURE_TARGET")
	if target == "" {
		return
	}
	listener, err := net.Listen("tcp4", target)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(os.Getenv("TCLAUDE_RELAY_FIXTURE_READY"), []byte("ready"), 0600))
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "opencode" || password != "disposable-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("exact session"))
	})}
	require.NoError(t, server.Serve(listener))
}
