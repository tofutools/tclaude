//go:build linux

package opencodeapi

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
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

func TestInheritedUnixListenerPeerIsRecordedLauncherPID(t *testing.T) {
	parent := filepath.Join(shortTempDir(t),
		"agt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(t, os.Mkdir(parent, 0o700))
	path := filepath.Join(parent, "control.sock")
	statusR, statusW, err := os.Pipe()
	require.NoError(t, err)
	gateR, gateW, err := os.Pipe()
	require.NoError(t, err)
	cmd := exec.Command(os.Args[0],
		"-test.run=^TestUnixLaunchCreatorHelper$", "--", path)
	cmd.Env = append(os.Environ(), "TCLAUDE_UNIX_LAUNCH_HELPER=creator")
	cmd.ExtraFiles = []*os.File{statusW, gateR}
	require.NoError(t, cmd.Start())
	_ = statusW.Close()
	_ = gateR.Close()
	t.Cleanup(func() {
		_ = gateW.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = statusR.Close()
		_ = os.Remove(path)
	})
	authority, err := ReadUnixLaunchAuthority(statusR)
	require.NoError(t, err)
	runtime := db.OpenCodeRuntime{
		PID: cmd.Process.Pid, ServerURL: "http://127.0.0.1:43210",
		Password: "secret", Transport: db.OpenCodeTransportUnixRelay,
		ControlSocketPath: path, ControlSocketDevice: authority.Device,
		ControlSocketInode: authority.Inode,
	}
	_, err = gateW.Write([]byte{1})
	require.NoError(t, err)
	require.NoError(t, gateW.Close())
	require.Eventually(t, func() bool {
		request, requestErr := NewRequest(http.MethodGet,
			runtime.ServerURL+"/global/health", runtime, nil)
		if requestErr != nil {
			return false
		}
		response, requestErr := Do(&http.Client{Timeout: time.Second}, request, runtime)
		if requestErr != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode == http.StatusOK
	}, 3*time.Second, 25*time.Millisecond)
}

func TestUnixLauncherRemovesSocketWhenDurableAckDisappears(t *testing.T) {
	parent := filepath.Join(shortTempDir(t),
		"agt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(t, os.Mkdir(parent, 0o700))
	path := filepath.Join(parent, "control.sock")
	statusR, statusW, err := os.Pipe()
	require.NoError(t, err)
	gateR, gateW, err := os.Pipe()
	require.NoError(t, err)
	cmd := exec.Command(os.Args[0],
		"-test.run=^TestUnixLaunchCreatorHelper$", "--", path)
	cmd.Env = append(os.Environ(), "TCLAUDE_UNIX_LAUNCH_HELPER=creator-no-ack")
	cmd.ExtraFiles = []*os.File{statusW, gateR}
	require.NoError(t, cmd.Start())
	_ = statusW.Close()
	_ = gateR.Close()
	t.Cleanup(func() {
		_ = gateW.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = statusR.Close()
		_ = os.Remove(path)
	})
	_, err = ReadUnixLaunchAuthority(statusR)
	require.NoError(t, err)
	require.NoError(t, gateW.Close(),
		"closing the gate simulates agentd disappearing before durable ack")
	require.NoError(t, cmd.Wait())
	_, err = os.Lstat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestUnixLaunchCreatorHelper(t *testing.T) {
	mode := os.Getenv("TCLAUDE_UNIX_LAUNCH_HELPER")
	if mode != "creator" && mode != "creator-no-ack" {
		return
	}
	path := os.Args[len(os.Args)-1]
	err := ExecUnixRelayLaunch(path, []string{
		os.Args[0], "-test.run=^TestUnixLaunchAcceptedPeerHelper$",
	})
	if mode == "creator-no-ack" {
		require.Error(t, err)
		return
	}
	require.NoError(t, err)
}

func TestUnixLaunchAcceptedPeerHelper(t *testing.T) {
	if os.Getenv("TCLAUDE_UNIX_LAUNCH_HELPER") != "creator" {
		return
	}
	file := os.NewFile(3, "inherited-opencode-control")
	require.NotNil(t, file)
	listener, err := net.FileListener(file)
	require.NoError(t, err)
	_ = file.Close()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != ServerUsername || password != "secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"healthy":true}`)
	})}
	require.NoError(t, server.Serve(listener))
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
