//go:build linux

package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/opencodeapi"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"golang.org/x/sys/unix"
)

const (
	openCodeLayerSmokeEnv         = "TCLAUDE_OPENCODE_LAYER_SMOKE"
	openCodeLayerSmokeTclaudeEnv  = "TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY"
	openCodeLayerSmokeSessionID   = "opencode-layer-smoke"
	openCodeLayerSmokeAttachProbe = 5 * time.Second
)

// TestOpenCodeTclaudeLayerExecutorSmoke is the real integration proof for the
// server-authoritative topology. It launches the actual OpenCode server as a
// child of bubblewrap, attaches the actual TUI outside that boundary, verifies
// the persisted permission suffix, drives OpenCode's real bash tool endpoint,
// and requires agentd to resolve the tool subprocess to the exact stable agent
// identity through the recorded wrapper ancestry.
func TestOpenCodeTclaudeLayerExecutorSmoke(t *testing.T) {
	if os.Getenv(openCodeLayerSmokeEnv) != "1" {
		t.Skip("set TCLAUDE_OPENCODE_LAYER_SMOKE=1 on an unsandboxed Linux host with bubblewrap and OpenCode")
	}
	_, _, err := session.ResolveTclaudeLayerServer(sandboxpolicy.NetworkIsolatedWithAgentd)
	require.NoError(t, err)
	openCodeExecutable, err := harness.OpenCodeExecutable()
	require.NoError(t, err)
	tclaudeBinary := strings.TrimSpace(os.Getenv(openCodeLayerSmokeTclaudeEnv))
	require.NotEmpty(t, tclaudeBinary)
	tclaudeBinary, err = filepath.Abs(tclaudeBinary)
	require.NoError(t, err)
	previousRelayExecutable := openCodeRelayExecutable
	openCodeRelayExecutable = func() (string, error) { return tclaudeBinary, nil }
	t.Cleanup(func() { openCodeRelayExecutable = previousRelayExecutable })

	// OpenCode can finish asynchronous dependency-cache writes just after its
	// server process exits. testing.T.TempDir performs one immediate RemoveAll,
	// which races those final writes on CI. Own this directory's cleanup so all
	// registered process cleanups run first, then require the tree to become
	// quiescent and removable within a bounded window.
	home, err := os.MkdirTemp("", "toc-*")
	require.NoError(t, err)
	home, err = filepath.EvalSymlinks(home)
	require.NoError(t, err)
	t.Cleanup(func() {
		var absentSince time.Time
		require.Eventuallyf(t, func() bool {
			if _, err := os.Stat(home); err == nil {
				absentSince = time.Time{}
			}
			if err := os.RemoveAll(home); err != nil {
				absentSince = time.Time{}
				return false
			}
			_, err := os.Stat(home)
			if !errors.Is(err, os.ErrNotExist) {
				absentSince = time.Time{}
				return false
			}
			if absentSince.IsZero() {
				absentSince = time.Now()
				return false
			}
			return time.Since(absentSince) >= 100*time.Millisecond
		}, 5*time.Second, 50*time.Millisecond,
			"OpenCode smoke home remained active after process teardown")
	})
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	root := filepath.Join(home, "fixture")
	cwd := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside")
	ambientData := filepath.Join(home, "data", "opencode")
	ambientCache := filepath.Join(home, "cache", "opencode")
	ambientConfig := filepath.Join(home, "config", "opencode")
	ambientState := filepath.Join(home, "state", "opencode")
	install := filepath.Join(home, ".opencode")
	for _, path := range []string{
		cwd, outside, ambientData, ambientCache, ambientConfig, ambientState, install,
	} {
		require.NoError(t, os.MkdirAll(path, 0o700))
	}
	for _, path := range []string{
		filepath.Join(ambientData, "ambient-data-marker"),
		filepath.Join(ambientCache, "ambient-cache-marker"),
		filepath.Join(ambientState, "ambient-state-marker"),
		filepath.Join(ambientConfig, "shared-config-marker"),
		filepath.Join(install, "shared-install-marker"),
	} {
		require.NoError(t, os.WriteFile(path, []byte("marker"), 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(ambientConfig, ".gitignore"),
		[]byte("node_modules\npackage.json\npackage-lock.json\n.gitignore\n"), 0o600))

	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	agentSocket := filepath.Join(home, ".tclaude", "api", "agentd.sock")
	t.Setenv(agentipc.SocketEnv, agentSocket)
	stopAgentd := startOpenCodeLayerSmokeAgentd(t, tclaudeBinary, agentSocket)
	t.Cleanup(stopAgentd)

	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.Effective.NetworkAccess = sandboxpolicy.NetworkAccessNone
	snapshot.Effective.Filesystem = []sandboxpolicy.FilesystemGrant{
		{Path: outside, Access: sandboxpolicy.AccessDeny},
	}
	snapshot.Effective.Environment = []sandboxpolicy.EnvironmentEntry{{
		Name: "TCLAUDE_OPENCODE_EXECUTOR_SMOKE", Value: "frozen-profile-value",
	}}
	smokeAgentID := db.NewAgentID()
	allocation, err := allocatePrivateOpenCodeState(smokeAgentID)
	require.NoError(t, err)
	siblingAgentID := db.NewAgentID()
	siblingAllocation, err := allocatePrivateOpenCodeState(siblingAgentID)
	require.NoError(t, err)
	siblingControlPath := filepath.Join(siblingAllocation.StateRoot, "control.sock")
	siblingControl, _, _, err := opencodeapi.CreateUnixListener(siblingControlPath)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = siblingControl.Close()
		_ = os.Remove(siblingControlPath)
	})
	siblingMarker := filepath.Join(siblingAllocation.StateRoot, "sibling-marker")
	require.NoError(t, os.WriteFile(siblingMarker, []byte("sibling"), 0o600))
	spec, err := openCodeUnixRelayLaunchSpec(
		string(sandboxpolicy.ImplementationTclaudeLayer), cwd, nil, &snapshot,
		smokeAgentID)
	require.NoError(t, err)
	require.NotNil(t, spec)
	permissionJSON, err := openCodePermissionJSONForLaunch(
		cwd,
		harness.OpenCodeSandboxTclaudeLayer,
		harness.OpenCodeApprovalDeny,
		harness.OpenCodeToolsAllow,
		&snapshot,
	)
	require.NoError(t, err)
	launch, err := startOpenCodeRuntime(
		openCodeLayerSmokeSessionID, cwd, "OpenCode layer smoke", "", permissionJSON, spec)
	if err != nil {
		logOpenCodeLayerSmokeServerLogs(t,
			filepath.Join(allocation.StateRoot, "data", "opencode", "log"))
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = stopOpenCodeRuntime(openCodeLayerSmokeSessionID) })
	require.NotEmpty(t, launch.ConvID)

	now := time.Now()
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:                    openCodeLayerSmokeSessionID,
		ConvID:                launch.ConvID,
		Harness:               harness.OpenCodeName,
		SandboxMode:           harness.OpenCodeSandboxTclaudeLayer,
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
		Cwd:                   cwd,
		Status:                session.StatusWorking,
		CreatedAt:             now,
		UpdatedAt:             now,
	}))
	expectedAgentID, _, err := db.EnsureAgentForConvWithID(
		launch.ConvID, smokeAgentID, "smoke")
	require.NoError(t, err)

	runtime, err := db.GetOpenCodeRuntime(openCodeLayerSmokeSessionID)
	require.NoError(t, err)
	require.NotNil(t, runtime)
	assert.Equal(t, db.OpenCodeTransportUnixRelay, runtime.Transport)
	assert.Equal(t, string(sandboxpolicy.ImplementationTclaudeLayer),
		runtime.SandboxImplementation)
	require.NoError(t, ensureOpenCodeSessionPermission(*runtime),
		"the actual server must retain the compiled permission suffix")
	hostTCP, tcpErr := net.DialTimeout("tcp", strings.TrimPrefix(
		runtime.ServerURL, "http://"), 250*time.Millisecond)
	if hostTCP != nil {
		_ = hostTCP.Close()
	}
	require.Error(t, tcpErr,
		"the internal OpenCode listener must not be reachable through host TCP")

	stopAttach := startOpenCodeLayerSmokeAttach(
		t, tclaudeBinary, openCodeExecutable, *runtime, launch.ConvID, cwd,
		spec.Contract.Environment)
	t.Cleanup(stopAttach)

	command := fmt.Sprintf(
		"set -eu; test \"$TCLAUDE_OPENCODE_EXECUTOR_SMOKE\" = frozen-profile-value; "+
			"test \"$XDG_DATA_HOME\" = %s; test \"$XDG_CACHE_HOME\" = %s; "+
			"test \"$XDG_CONFIG_HOME\" = %s; test \"$XDG_STATE_HOME\" = %s; "+
			"printf executor-ok > %s; printf state-ok > \"$XDG_STATE_HOME/opencode/tool-state\"; "+
			"if printf blocked > %s; then exit 97; fi; "+
			"for hidden in %s %s %s %s; do if test -r \"$hidden\"; then exit 98; fi; done; "+
			"test -r %s; test -r %s; "+
			"if printf planted > %s; then exit 99; fi; "+
			"if printf planted > %s; then exit 100; fi; "+
			"test ! -S %s; test ! -e %s; "+
			"%s agent whoami",
		clcommon.ShellQuoteArg(filepath.Join(allocation.StateRoot, "data")),
		clcommon.ShellQuoteArg(filepath.Join(allocation.StateRoot, "cache")),
		clcommon.ShellQuoteArg(filepath.Join(allocation.StateRoot, "config")),
		clcommon.ShellQuoteArg(filepath.Join(allocation.StateRoot, "state")),
		clcommon.ShellQuoteArg(filepath.Join(cwd, "tool-written")),
		clcommon.ShellQuoteArg(filepath.Join(outside, "blocked")),
		clcommon.ShellQuoteArg(siblingMarker),
		clcommon.ShellQuoteArg(filepath.Join(ambientData, "ambient-data-marker")),
		clcommon.ShellQuoteArg(filepath.Join(ambientCache, "ambient-cache-marker")),
		clcommon.ShellQuoteArg(filepath.Join(ambientState, "ambient-state-marker")),
		clcommon.ShellQuoteArg(filepath.Join(ambientConfig, "shared-config-marker")),
		clcommon.ShellQuoteArg(filepath.Join(install, "shared-install-marker")),
		clcommon.ShellQuoteArg(filepath.Join(ambientConfig, "config-write-blocked")),
		clcommon.ShellQuoteArg(filepath.Join(install, "install-write-blocked")),
		clcommon.ShellQuoteArg(runtime.ControlSocketPath),
		clcommon.ShellQuoteArg(siblingControlPath),
		clcommon.ShellQuoteArg(tclaudeBinary),
	)
	output := runOpenCodeLayerSmokeShell(t, *runtime, command)
	require.FileExists(t, filepath.Join(cwd, "tool-written"))
	_, statErr := os.Stat(filepath.Join(outside, "blocked"))
	require.ErrorIs(t, statErr, os.ErrNotExist,
		"the real OpenCode bash tool must remain inside the server's mount boundary")
	require.FileExists(t, filepath.Join(allocation.StateRoot, "data", "opencode", "opencode.db"))
	require.FileExists(t, filepath.Join(allocation.StateRoot, "state", "opencode", "tool-state"))

	var identityLine string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "agt_") {
			identityLine = strings.TrimSpace(line)
		}
	}
	require.NotEmptyf(t, identityLine, "tool output did not contain managed identity: %q", output)
	assert.Equal(t, expectedAgentID, strings.Fields(identityLine)[0],
		"agentd must resolve the exact managed identity through the wrapped server ancestry")
}

func logOpenCodeLayerSmokeServerLogs(t *testing.T, dir string) {
	t.Helper()
	dirFile, err := openOpenCodeLayerSmokeLogDir(dir)
	if err != nil {
		t.Logf("read OpenCode smoke log directory %s: %v", dir, err)
		return
	}
	defer dirFile.Close()
	entries, err := dirFile.ReadDir(-1)
	if err != nil {
		t.Logf("enumerate OpenCode smoke log directory %s: %v", dir, err)
		return
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		raw, readErr := readOpenCodeLayerSmokeLogTailAt(
			int(dirFile.Fd()), entry.Name())
		if readErr != nil {
			t.Logf("read OpenCode smoke log %s: %v", path, readErr)
			continue
		}
		t.Logf("OpenCode smoke log %s:\n%s", path, raw)
	}
}

func openOpenCodeLayerSmokeLogDir(path string) (*os.File, error) {
	fd, err := unix.Open(
		path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func readOpenCodeLayerSmokeLogTailAt(dirFD int, name string) ([]byte, error) {
	fd, err := unix.Openat(
		dirFD, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, fmt.Errorf("not a regular file")
	}
	const limit = int64(64 << 10)
	if stat.Size > limit {
		if _, err := file.Seek(stat.Size-limit, io.SeekStart); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(io.LimitReader(file, limit))
}

func TestReadOpenCodeLayerSmokeLogTailRefusesSpecialFilesAndBounds(t *testing.T) {
	dir := t.TempDir()
	large := filepath.Join(dir, "large.log")
	require.NoError(t, os.WriteFile(large, []byte(
		strings.Repeat("a", 64<<10)+strings.Repeat("b", 64<<10)), 0o600))
	dirFile, err := openOpenCodeLayerSmokeLogDir(dir)
	require.NoError(t, err)
	defer dirFile.Close()
	raw, err := readOpenCodeLayerSmokeLogTailAt(int(dirFile.Fd()), "large.log")
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("b", 64<<10), string(raw))

	symlink := filepath.Join(dir, "symlink.log")
	require.NoError(t, os.Symlink(large, symlink))
	_, err = readOpenCodeLayerSmokeLogTailAt(int(dirFile.Fd()), "symlink.log")
	require.Error(t, err)

	fifo := filepath.Join(dir, "fifo.log")
	require.NoError(t, unix.Mkfifo(fifo, 0o600))
	_, err = readOpenCodeLayerSmokeLogTailAt(int(dirFile.Fd()), "fifo.log")
	require.ErrorContains(t, err, "not a regular file")

	dirSymlink := filepath.Join(t.TempDir(), "log")
	require.NoError(t, os.Symlink(dir, dirSymlink))
	_, err = openOpenCodeLayerSmokeLogDir(dirSymlink)
	require.Error(t, err)
}

func startOpenCodeLayerSmokeAgentd(t *testing.T, tclaudeBinary, socket string) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, tclaudeBinary, "agentd", "serve",
		"--socket", socket, "--no-tray", "--no-print-human-token")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	var stopped bool
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		_ = cmd.Wait()
	}
	t.Cleanup(stop)
	require.Eventuallyf(t, func() bool {
		return agentipc.SocketReachable(socket)
	}, 15*time.Second, 25*time.Millisecond, "agentd did not become reachable")
	return stop
}

func startOpenCodeLayerSmokeAttach(
	t *testing.T,
	tclaudeBinary string,
	executable string,
	runtime db.OpenCodeRuntime,
	convID, cwd string,
	environment []sandboxpolicy.EnvironmentEntry,
) func() {
	t.Helper()
	cmd := exec.Command(tclaudeBinary,
		opencodeapi.UnixAttachShimMode,
		strconv.Itoa(runtime.PID),
		runtime.ControlSocketPath,
		strconv.FormatInt(runtime.ControlSocketDevice, 10),
		strconv.FormatInt(runtime.ControlSocketInode, 10),
		runtime.ServerURL,
		"--",
		executable, "attach", opencodeapi.AttachURLPlaceholder,
		"--dir", cwd, "--session", convID)
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"OPENCODE_SERVER_USERNAME="+openCodeServerUsername,
		"OPENCODE_SERVER_PASSWORD="+runtime.Password,
	)
	for _, entry := range environment {
		cmd.Env = append(cmd.Env, entry.Name+"="+entry.Value)
	}
	terminal, err := pty.Start(cmd)
	require.NoError(t, err)
	copied := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, terminal)
		close(copied)
	}()
	var stopped bool
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = terminal.Close()
		select {
		case <-copied:
		case <-time.After(time.Second):
		}
	}
	t.Cleanup(stop)
	require.Eventuallyf(t, func() bool {
		for _, pid := range openCodeLayerSmokeProcessTree(cmd.Process.Pid) {
			raw, readErr := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
			if readErr == nil && strings.Contains(strings.ToLower(string(raw)), "opencode") {
				return true
			}
		}
		return false
	}, openCodeLayerSmokeAttachProbe, 25*time.Millisecond,
		"OpenCode attach did not start behind the Unix shim")
	rawEnvironment, err := os.ReadFile(
		filepath.Join("/proc", strconv.Itoa(cmd.Process.Pid), "environ"))
	require.NoError(t, err)
	for _, entry := range environment {
		assert.Contains(t, string(rawEnvironment), entry.Name+"="+entry.Value+"\x00",
			"attach and server must receive the same private XDG allocation")
	}
	return stop
}

func runOpenCodeLayerSmokeShell(
	t *testing.T,
	runtime db.OpenCodeRuntime,
	command string,
) string {
	t.Helper()
	endpoint := runtime.ServerURL + "/session/" + url.PathEscape(runtime.ConvID) +
		"/shell?directory=" + url.QueryEscape(runtime.Cwd)
	request, err := openCodeRequest(http.MethodPost, endpoint, runtime, map[string]any{
		"agent":   "build",
		"command": command,
	})
	require.NoError(t, err)
	response, err := opencodeapi.Do(openCodeHTTPClient, request, runtime)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	require.NoError(t, err)
	require.Equalf(t, http.StatusOK, response.StatusCode, "OpenCode shell response: %s", body)
	var result struct {
		Parts []struct {
			Type  string `json:"type"`
			State struct {
				Status string `json:"status"`
				Output string `json:"output"`
			} `json:"state"`
		} `json:"parts"`
	}
	require.NoError(t, json.Unmarshal(body, &result))
	for _, part := range result.Parts {
		if part.Type == "tool" {
			require.Equal(t, "completed", part.State.Status)
			return part.State.Output
		}
	}
	t.Fatalf("OpenCode shell response contained no tool part: %s", body)
	return ""
}

func openCodeLayerSmokeProcessTree(rootPID int) []int {
	result := []int{rootPID}
	seen := map[int]bool{rootPID: true}
	for cursor := 0; cursor < len(result) && len(result) < 64; cursor++ {
		pid := result[cursor]
		children, err := os.ReadFile(filepath.Join(
			"/proc", strconv.Itoa(pid), "task", strconv.Itoa(pid), "children"))
		if err != nil {
			continue
		}
		for _, raw := range strings.Fields(string(children)) {
			child, err := strconv.Atoi(raw)
			if err == nil && child > 1 && !seen[child] {
				seen[child] = true
				result = append(result, child)
			}
		}
	}
	return result
}
