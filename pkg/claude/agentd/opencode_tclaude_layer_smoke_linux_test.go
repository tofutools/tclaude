//go:build linux

package agentd

import (
	"context"
	"encoding/json"
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
	"github.com/tofutools/tclaude/pkg/claude/session"
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
	_, _, err := session.ResolveTclaudeLayer(sandboxpolicy.NetworkHostOpen)
	require.NoError(t, err)
	openCodeExecutable, err := harness.OpenCodeExecutable()
	require.NoError(t, err)
	tclaudeBinary := strings.TrimSpace(os.Getenv(openCodeLayerSmokeTclaudeEnv))
	require.NotEmpty(t, tclaudeBinary)
	tclaudeBinary, err = filepath.Abs(tclaudeBinary)
	require.NoError(t, err)

	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	root := filepath.Join(home, "fixture")
	cwd := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside")
	require.NoError(t, os.MkdirAll(cwd, 0o700))
	require.NoError(t, os.MkdirAll(outside, 0o700))

	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	agentSocket := filepath.Join(home, ".tclaude", "api", "agentd.sock")
	t.Setenv(agentipc.SocketEnv, agentSocket)
	stopAgentd := startOpenCodeLayerSmokeAgentd(t, tclaudeBinary, agentSocket)
	t.Cleanup(stopAgentd)

	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.Effective.Filesystem = []sandboxpolicy.FilesystemGrant{
		{Path: outside, Access: sandboxpolicy.AccessDeny},
	}
	spec, err := openCodeTclaudeLayerLaunchSpec(
		string(sandboxpolicy.ImplementationTclaudeLayer), cwd, nil, &snapshot)
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
	expectedAgentID, _, err := db.EnsureAgentForConv(launch.ConvID, "smoke")
	require.NoError(t, err)

	runtime, err := db.GetOpenCodeRuntime(openCodeLayerSmokeSessionID)
	require.NoError(t, err)
	require.NotNil(t, runtime)
	assert.Equal(t, string(sandboxpolicy.ImplementationTclaudeLayer),
		runtime.SandboxImplementation)
	require.NoError(t, ensureOpenCodeSessionPermission(*runtime),
		"the actual server must retain the compiled permission suffix")

	stopAttach := startOpenCodeLayerSmokeAttach(
		t, openCodeExecutable, *runtime, launch.ConvID, cwd)
	t.Cleanup(stopAttach)

	command := fmt.Sprintf(
		"set -eu; printf executor-ok > %s; if printf blocked > %s; then exit 97; fi; %s agent whoami",
		clcommon.ShellQuoteArg(filepath.Join(cwd, "tool-written")),
		clcommon.ShellQuoteArg(filepath.Join(outside, "blocked")),
		clcommon.ShellQuoteArg(tclaudeBinary),
	)
	output := runOpenCodeLayerSmokeShell(t, *runtime, command)
	require.FileExists(t, filepath.Join(cwd, "tool-written"))
	_, statErr := os.Stat(filepath.Join(outside, "blocked"))
	require.ErrorIs(t, statErr, os.ErrNotExist,
		"the real OpenCode bash tool must remain inside the server's mount boundary")

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
	executable string,
	runtime db.OpenCodeRuntime,
	convID, cwd string,
) func() {
	t.Helper()
	cmd := exec.Command(executable,
		"attach", runtime.ServerURL, "--dir", cwd, "--session", convID)
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"OPENCODE_SERVER_USERNAME="+openCodeServerUsername,
		"OPENCODE_SERVER_PASSWORD="+runtime.Password,
	)
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
		return processHasOpenCodeServerConnection(cmd.Process.Pid, runtime.ServerURL)
	}, openCodeLayerSmokeAttachProbe, 25*time.Millisecond,
		"OpenCode attach did not connect to the managed server")
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
	response, err := openCodeHTTPClient.Do(request)
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

func processHasOpenCodeServerConnection(rootPID int, endpoint string) bool {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	_, rawPort, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return false
	}
	port, err := strconv.ParseUint(rawPort, 10, 16)
	if err != nil {
		return false
	}
	portHex := fmt.Sprintf("%04X", port)
	data, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return false
	}
	inodes := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 9 && fields[3] == "01" &&
			strings.HasSuffix(fields[2], ":"+portHex) {
			inodes[fields[9]] = true
		}
	}
	for _, pid := range openCodeLayerSmokeProcessTree(rootPID) {
		entries, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(pid), "fd"))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			target, err := os.Readlink(filepath.Join(
				"/proc", strconv.Itoa(pid), "fd", entry.Name()))
			if err == nil && strings.HasPrefix(target, "socket:[") &&
				inodes[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] {
				return true
			}
		}
	}
	return false
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
