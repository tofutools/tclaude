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
	"syscall"
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
	// Keep both sockets live under validator-compatible paths so their
	// identities are guaranteed distinct before the replacement rename (TCL-810).
	replacementParent := filepath.Join(filepath.Dir(filepath.Dir(runtime.ControlSocketPath)),
		"agt_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	require.NoError(t, os.Mkdir(replacementParent, 0o700))
	replacementPath := filepath.Join(replacementParent, "control.sock")
	replacement, _, _, err := CreateUnixListener(replacementPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = replacement.Close() })

	require.NoError(t, os.Remove(runtime.ControlSocketPath))
	require.NoError(t, os.Rename(replacementPath, runtime.ControlSocketPath))

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

// TCL-933. These refusals decide correctly and then described themselves
// inaccurately, which is expensive precisely when an operator is already
// stuck: a confidently wrong message buys a wrong hypothesis. The corrected
// spellings match their siblings in agentd (TCL-923, #1851).
//
// Each assertion carries the RETIRED wording as a negative needle, because
// re-introducing a spelling already removed elsewhere is this family's
// demonstrated failure mode. The needles are full sentences on purpose: a
// short needle like `is not canonical"` also matches the CORRECTED wording
// ("could not be resolved or is not canonical") and would pass while pinning
// nothing.
func TestControlIdentityRefusalsDescribeWhatWasActuallyChecked(t *testing.T) {
	// A raw t.TempDir() can already be non-canonical (on macOS via
	// /var -> /private/var, and on Linux wherever TMPDIR points through a
	// link), so a canonicality assertion built on one can fire for the
	// PLATFORM's symlink rather than the fixture's own construction — passing
	// while testing nothing it names.
	// NOT t.TempDir(): a sockaddr_un path is capped at 108 bytes, and this
	// fixture spends 55 of them on the fixed suffix. On a host whose TMPDIR is
	// deep, the refusal under test is never reached — the length gate fires
	// first, and the subtest goes red for a reason that has nothing to do with
	// canonicality. That happened while writing this, which is exactly why the
	// short base is deliberate rather than incidental.
	shortBase, err := os.MkdirTemp("/tmp", "tcl933-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(shortBase) })
	// Canonicalized FIRST, so the assertion below is about the symlink this
	// test builds and not one the platform put in the path already.
	base, err := filepath.EvalSymlinks(shortBase)
	require.NoError(t, err)
	const agentID = "agt_0123456789abcdef0123456789abcdef"

	t.Run("parent canonicality names resolution failure as a disjunction", func(t *testing.T) {
		real := filepath.Join(base, "real")
		require.NoError(t, os.MkdirAll(filepath.Join(real, agentID), 0o700))
		link := filepath.Join(base, "link")
		require.NoError(t, os.Symlink(real, link))

		// The PARENT itself is a real 0700 directory, so the mode and owner
		// gates pass and execution actually reaches the canonicality check —
		// which fails only because an ANCESTOR is the symlink this test built.
		_, _, err := controlParentIdentity(filepath.Join(link, agentID, "control.sock"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "could not be resolved or is not canonical")
		assert.Contains(t, err.Error(), agentID, "the refusal names the path it judged")
		assert.NotContains(t, err.Error(), "OpenCode control socket parent is not canonical")
	})

	t.Run("authority mode refusal does not name a check it skipped", func(t *testing.T) {
		regular := filepath.Join(base, "not-a-socket")
		require.NoError(t, os.WriteFile(regular, nil, 0o600))

		// requireMode=false is the live path from createUnixListener, taken
		// right after the bind. The permission clause is GATED off here, so
		// naming mode-0600 would describe a check that never ran.
		_, _, err := socketPathIdentity(regular, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is not a real socket")
		assert.NotContains(t, err.Error(), "mode-0600",
			"the mode clause is guarded by requireMode and must not be claimed when it is off")
		assert.NotContains(t, err.Error(), "OpenCode control authority is not a real mode-0600 socket")

		// requireMode=true DOES run it, so naming it is correct there.
		_, _, err = socketPathIdentity(regular, true)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is not a real mode-0600 socket")
	})

	t.Run("owner description never invents a uid it did not read", func(t *testing.T) {
		// The uid-mismatch arm needs a path owned by another user and is not
		// reachable unprivileged, so the renderer is pinned directly. This is
		// the half that matters: printing "uid 0" for an unread owner would
		// name root as the owner of a path whose owner was never looked at.
		assert.Equal(t, "no readable owner", controlStatOwnerDescription(nil, false))
		assert.Equal(t, "no readable owner", controlStatOwnerDescription(nil, true),
			"a nil stat is unreadable regardless of the assertion flag")
		assert.Equal(t, "uid 4242",
			controlStatOwnerDescription(&syscall.Stat_t{Uid: 4242}, true))
	})
}
