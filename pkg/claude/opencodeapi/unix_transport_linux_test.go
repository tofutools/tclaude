//go:build linux

package opencodeapi

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestUnixTransportProvesAuthorityBeforeSendingPassword(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(t *testing.T, runtime *db.OpenCodeRuntime, listener **http.Server)
		wantOK bool
	}{
		{name: "recorded authority", wantOK: true},
		{name: "wrong inode", mutate: func(t *testing.T, runtime *db.OpenCodeRuntime, _ **http.Server) {
			runtime.ControlSocketInode++
		}},
		{name: "foreign peer pid", mutate: func(t *testing.T, runtime *db.OpenCodeRuntime, _ **http.Server) {
			runtime.PID = 99_999_999
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, server, captured := unixHTTPFixture(t)
			if test.mutate != nil {
				test.mutate(t, &runtime, &server)
			}
			request, err := NewRequest(http.MethodGet,
				runtime.ServerURL+"/global/health", runtime, nil)
			require.NoError(t, err)
			response, err := Do(&http.Client{Timeout: time.Second}, request, runtime)
			if test.wantOK {
				require.NoError(t, err)
				_ = response.Body.Close()
				assert.Equal(t, int32(1), captured.Load())
			} else {
				require.Error(t, err)
				assert.Zero(t, captured.Load(),
					"credentials must not reach an unproven peer")
			}
		})
	}
}

func TestUnixTransportRefusesPathReplacementDuringConnect(t *testing.T) {
	runtime, _, captured := unixHTTPFixture(t)
	var replacementCaptured atomic.Int32
	afterUnixConnectForTest = func() {
		afterUnixConnectForTest = nil
		require.NoError(t, os.Remove(runtime.ControlSocketPath))
		replacement, device, inode, err := CreateUnixListener(runtime.ControlSocketPath)
		require.NoError(t, err)
		require.NotEqual(t, runtime.ControlSocketInode, inode)
		require.Equal(t, runtime.ControlSocketDevice, device)
		replacementServer := &http.Server{Handler: http.HandlerFunc(
			func(http.ResponseWriter, *http.Request) { replacementCaptured.Add(1) })}
		t.Cleanup(func() { _ = replacementServer.Close() })
		go func() { _ = replacementServer.Serve(replacement) }()
	}
	t.Cleanup(func() { afterUnixConnectForTest = nil })

	request, err := NewRequest(http.MethodGet,
		runtime.ServerURL+"/global/health", runtime, nil)
	require.NoError(t, err)
	_, err = Do(&http.Client{Timeout: time.Second}, request, runtime)
	require.ErrorContains(t, err, "identity changed during connect")
	assert.Zero(t, captured.Load())
	assert.Zero(t, replacementCaptured.Load())
}

func TestRemoveUnixSocketPreservesReplacement(t *testing.T) {
	runtime, server, _ := unixHTTPFixture(t)
	require.NoError(t, server.Close())
	require.NoError(t, os.Remove(runtime.ControlSocketPath))
	replacement, _, replacementInode, err := CreateUnixListener(runtime.ControlSocketPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = replacement.Close() })
	require.NotEqual(t, runtime.ControlSocketInode, replacementInode)

	require.ErrorContains(t, RemoveUnixSocket(runtime), "replaced")
	info, err := os.Lstat(runtime.ControlSocketPath)
	require.NoError(t, err)
	assert.True(t, info.Mode()&os.ModeSocket != 0)
}

func TestCreateUnixListenerRefusesUnsafeAuthority(t *testing.T) {
	parent := filepath.Join(shortTempDir(t), "agt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(t, os.Mkdir(parent, 0o700))
	path := filepath.Join(parent, "control.sock")
	require.NoError(t, os.WriteFile(path, []byte("attacker"), 0o600))
	_, _, _, err := CreateUnixListener(path)
	require.ErrorContains(t, err, "already exists")
	data, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, "attacker", string(data))
}

func unixHTTPFixture(t *testing.T) (db.OpenCodeRuntime, *http.Server, *atomic.Int32) {
	t.Helper()
	parent := filepath.Join(shortTempDir(t), "agt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(t, os.Mkdir(parent, 0o700))
	path := filepath.Join(parent, "control.sock")
	listener, device, inode, err := CreateUnixListener(path)
	require.NoError(t, err)
	var captured atomic.Int32
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if username, password, ok := r.BasicAuth(); ok &&
			username == ServerUsername && password == "secret" {
			captured.Add(1)
		}
		_, _ = io.WriteString(w, `{"healthy":true}`)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = os.Remove(path)
	})
	return db.OpenCodeRuntime{
		PID: os.Getpid(), ServerURL: "http://127.0.0.1:43210",
		Password: "secret", Transport: db.OpenCodeTransportUnixRelay,
		ControlSocketPath: path, ControlSocketDevice: device,
		ControlSocketInode: inode,
	}, server, &captured
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	path, err := os.MkdirTemp("/tmp", "tcl-780-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(path) })
	return path
}

func TestRunUnixAttachShimPrebindsAndStreams(t *testing.T) {
	runtime, _, captured := unixHTTPFixture(t)
	command := []string{
		os.Args[0], "-test.run=TestUnixAttachShimHelper", "--",
		AttachURLPlaceholder,
	}
	t.Setenv("TCLAUDE_ATTACH_HELPER", "1")
	err := RunUnixAttachShim(context.Background(), runtime, command)
	require.NoError(t, err)
	assert.Equal(t, int32(1), captured.Load())
}

func TestUnixAttachShimHelper(t *testing.T) {
	if os.Getenv("TCLAUDE_ATTACH_HELPER") != "1" {
		return
	}
	url := os.Args[len(os.Args)-1]
	request, err := http.NewRequest(http.MethodGet, url+"/global/health", nil)
	require.NoError(t, err)
	request.SetBasicAuth(ServerUsername, "secret")
	response, err := (&http.Client{Timeout: time.Second}).Do(request)
	require.NoError(t, err)
	_ = response.Body.Close()
	os.Exit(0)
}
