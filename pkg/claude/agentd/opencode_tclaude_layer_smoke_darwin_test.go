//go:build darwin

package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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
)

const (
	openCodeDarwinLayerSmokeEnv        = "TCLAUDE_OPENCODE_LAYER_SMOKE"
	openCodeDarwinLayerSmokeTclaudeEnv = "TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY"
	openCodeDarwinLayerSmokeSessionID  = "opencode-darwin-layer-smoke"
)

// TestOpenCodeTclaudeLayerDarwinExecutorSmoke is the hardware-backed proof
// that the agentd-owned tool executor, rather than its attach pane, runs under
// Seatbelt. CI hard-gates its explicit PASS line.
func TestOpenCodeTclaudeLayerDarwinExecutorSmoke(t *testing.T) {
	if os.Getenv(openCodeDarwinLayerSmokeEnv) != "1" {
		t.Skip("set TCLAUDE_OPENCODE_LAYER_SMOKE=1 on macOS with OpenCode installed")
	}
	_, _, err := session.ResolveTclaudeLayerServer(
		sandboxpolicy.NetworkHostOpen, sandboxpolicy.RootHostInherited)
	require.NoError(t, err)
	openCodeExecutable, err := harness.OpenCodeExecutable()
	require.NoError(t, err)
	tclaudeBinary := strings.TrimSpace(os.Getenv(openCodeDarwinLayerSmokeTclaudeEnv))
	require.NotEmpty(t, tclaudeBinary)
	tclaudeBinary, err = filepath.Abs(tclaudeBinary)
	require.NoError(t, err)

	realHome, err := os.UserHomeDir()
	require.NoError(t, err)
	smokeBase := filepath.Join(realHome, ".cache")
	require.NoError(t, os.MkdirAll(smokeBase, 0o700))
	root, err := os.MkdirTemp(smokeBase, "tclaude-opencode-seatbelt-")
	require.NoError(t, err)
	root, err = filepath.EvalSymlinks(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	home := filepath.Join(root, "home")
	cwd := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside")
	ambientData := filepath.Join(home, "data", "opencode")
	ambientCache := filepath.Join(home, "cache", "opencode")
	ambientConfig := filepath.Join(home, "config", "opencode")
	ambientState := filepath.Join(home, "state", "opencode")
	install := filepath.Join(home, ".opencode")
	for _, path := range []string{
		home, cwd, outside, ambientData, ambientCache, ambientConfig, ambientState, install,
	} {
		require.NoError(t, os.MkdirAll(path, 0o700))
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))

	identityProbeBinary := filepath.Join(cwd, "tclaude-agent-probe")
	copyOpenCodeDarwinLayerSmokeExecutable(t, tclaudeBinary, identityProbeBinary)
	for _, path := range []string{
		filepath.Join(ambientData, "ambient-data-marker"),
		filepath.Join(ambientCache, "ambient-cache-marker"),
		filepath.Join(ambientState, "ambient-state-marker"),
		filepath.Join(ambientConfig, "shared-config-marker"),
		filepath.Join(install, "shared-install-marker"),
	} {
		require.NoError(t, os.WriteFile(path, []byte("marker"), 0o600))
	}

	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	agentSocket := filepath.Join(home, ".tclaude", "api", "agentd.sock")
	t.Setenv(agentipc.SocketEnv, agentSocket)
	stopAgentd := startOpenCodeDarwinLayerSmokeAgentd(t, tclaudeBinary, agentSocket)
	t.Cleanup(stopAgentd)

	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.Effective.NetworkAccess = sandboxpolicy.NetworkAccessInherit
	snapshot.Effective.Filesystem = []sandboxpolicy.FilesystemGrant{
		{Path: root, Access: sandboxpolicy.AccessWrite},
		{Path: outside, Access: sandboxpolicy.AccessDeny},
	}
	snapshot.Effective.Environment = []sandboxpolicy.EnvironmentEntry{{
		Name: "TCLAUDE_OPENCODE_EXECUTOR_SMOKE", Value: "darwin-frozen-profile-value",
	}}
	smokeAgentID := db.NewAgentID()
	allocation, err := allocatePrivateOpenCodeState(smokeAgentID)
	require.NoError(t, err)
	siblingAgentID := db.NewAgentID()
	siblingAllocation, err := allocatePrivateOpenCodeState(siblingAgentID)
	require.NoError(t, err)
	siblingMarker := filepath.Join(siblingAllocation.StateRoot, "sibling-marker")
	require.NoError(t, os.WriteFile(siblingMarker, []byte("sibling"), 0o600))

	spec, err := openCodeTclaudeLayerLaunchSpec(
		string(sandboxpolicy.ImplementationTclaudeLayer), cwd, nil, &snapshot,
		smokeAgentID)
	require.NoError(t, err)
	require.NotNil(t, spec)
	require.Equal(t, filepath.Join(home, "config"),
		openCodeDarwinLayerEnvironmentValue(spec.Contract.Environment, "XDG_CONFIG_HOME"))
	for _, bind := range spec.Contract.ReadOnlyBinds {
		require.Equal(t, bind.Source, bind.Target,
			"the Darwin smoke must exercise only Seatbelt-expressible same-path read-only roots")
	}

	permissionJSON, err := openCodePermissionJSONForLaunch(
		cwd,
		harness.OpenCodeSandboxTclaudeLayer,
		harness.OpenCodeApprovalDeny,
		harness.OpenCodeToolsAllow,
		&snapshot,
	)
	require.NoError(t, err)
	launch, err := startOpenCodeRuntime(
		openCodeDarwinLayerSmokeSessionID, cwd, "OpenCode Darwin layer smoke",
		"", permissionJSON, string(sandboxpolicy.ImplementationTclaudeLayer), spec)
	if err != nil {
		logOpenCodeDarwinLayerSmokeServerLogs(t,
			filepath.Join(allocation.StateRoot, "data", "opencode", "log"))
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = stopOpenCodeRuntime(openCodeDarwinLayerSmokeSessionID) })
	require.NotEmpty(t, launch.ConvID)
	configBootstrap, err := os.ReadFile(
		filepath.Join(ambientConfig, openCodeInstallBootstrapFile))
	require.NoError(t, err,
		"the smoke must exercise the production pre-wall config bootstrap")
	assert.Equal(t, openCodeInstallGitignore, string(configBootstrap))

	// sandbox-exec applies the profile and then execs the inner command, so
	// its own process name does not survive at the recorded PID. Pin the
	// managed serve command here; the tool write/hide/read-only probes below
	// are the hardware proof that this process inherited the Seatbelt profile.
	require.Eventually(t, func() bool {
		out, psErr := exec.Command("ps", "-p", fmt.Sprint(launch.PID), "-o", "command=").Output()
		commandLine := string(out)
		return psErr == nil &&
			strings.Contains(commandLine, "opencode") &&
			strings.Contains(commandLine, "serve")
	}, 5*time.Second, 25*time.Millisecond,
		"the recorded OpenCode PID must remain the managed serve command")

	now := time.Now()
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:                    openCodeDarwinLayerSmokeSessionID,
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
		launch.ConvID, smokeAgentID, "darwin-smoke")
	require.NoError(t, err)

	runtimeRow, err := db.GetOpenCodeRuntime(openCodeDarwinLayerSmokeSessionID)
	require.NoError(t, err)
	require.NotNil(t, runtimeRow)
	assert.Equal(t, db.OpenCodeTransportLoopbackTCP, runtimeRow.Transport)
	assert.Equal(t, string(sandboxpolicy.ImplementationTclaudeLayer),
		runtimeRow.SandboxImplementation)
	require.NoError(t, ensureOpenCodeSessionPermission(*runtimeRow),
		"the real Darwin server must retain the compiled permission suffix")

	stopAttach := startOpenCodeDarwinLayerSmokeAttach(
		t, openCodeExecutable, *runtimeRow, launch.ConvID, cwd, spec.Contract.Environment)
	t.Cleanup(stopAttach)

	command := fmt.Sprintf(
		"set -eu; test \"$TCLAUDE_OPENCODE_EXECUTOR_SMOKE\" = darwin-frozen-profile-value; "+
			"test \"$XDG_DATA_HOME\" = %s; test \"$XDG_CACHE_HOME\" = %s; "+
			"test \"$XDG_CONFIG_HOME\" = %s; test \"$XDG_STATE_HOME\" = %s; "+
			"printf executor-ok > %s; printf state-ok > \"$XDG_STATE_HOME/opencode/tool-state\"; "+
			"if printf blocked > %s; then exit 97; fi; "+
			"for hidden in %s %s %s %s; do if test -r \"$hidden\"; then exit 98; fi; done; "+
			"test -r %s; test -r %s; "+
			"if printf planted > %s; then exit 99; fi; "+
			"if printf planted > %s; then exit 100; fi; "+
			"%s agent whoami",
		clcommon.ShellQuoteArg(filepath.Join(allocation.StateRoot, "data")),
		clcommon.ShellQuoteArg(filepath.Join(allocation.StateRoot, "cache")),
		clcommon.ShellQuoteArg(filepath.Join(home, "config")),
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
		clcommon.ShellQuoteArg(identityProbeBinary),
	)
	output := runOpenCodeDarwinLayerSmokeShell(t, *runtimeRow, command)
	require.FileExists(t, filepath.Join(cwd, "tool-written"))
	_, statErr := os.Stat(filepath.Join(outside, "blocked"))
	require.ErrorIs(t, statErr, os.ErrNotExist,
		"the real OpenCode tool must remain inside the Seatbelt boundary")
	require.FileExists(t, filepath.Join(allocation.StateRoot, "data", "opencode", "opencode.db"))
	require.FileExists(t, filepath.Join(allocation.StateRoot, "state", "opencode", "tool-state"))

	var identityLine string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "agt_") {
			identityLine = strings.TrimSpace(line)
		}
	}
	require.NotEmptyf(t, identityLine,
		"tool output did not contain managed identity: %q", output)
	assert.Equal(t, expectedAgentID, strings.Fields(identityLine)[0],
		"agentd must resolve the exact managed identity through sandbox-exec ancestry")
}

func openCodeDarwinLayerEnvironmentValue(
	entries []sandboxpolicy.EnvironmentEntry,
	name string,
) string {
	for _, entry := range entries {
		if entry.Name == name {
			return entry.Value
		}
	}
	return ""
}

func copyOpenCodeDarwinLayerSmokeExecutable(t *testing.T, source, destination string) {
	t.Helper()
	sourceFile, err := os.Open(source)
	require.NoError(t, err)
	defer sourceFile.Close()
	destinationFile, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	require.NoError(t, err)
	_, err = io.Copy(destinationFile, sourceFile)
	require.NoError(t, err)
	require.NoError(t, destinationFile.Close())
}

func startOpenCodeDarwinLayerSmokeAgentd(
	t *testing.T,
	tclaudeBinary, socket string,
) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
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
	require.Eventuallyf(t, func() bool {
		return agentipc.SocketReachable(socket)
	}, 15*time.Second, 25*time.Millisecond, "agentd did not become reachable")
	return stop
}

func startOpenCodeDarwinLayerSmokeAttach(
	t *testing.T,
	executable string,
	runtimeRow db.OpenCodeRuntime,
	convID, cwd string,
	environment []sandboxpolicy.EnvironmentEntry,
) func() {
	t.Helper()
	cmd := exec.Command(executable, "attach", runtimeRow.ServerURL,
		"--dir", cwd, "--session", convID)
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"OPENCODE_SERVER_USERNAME="+openCodeServerUsername,
		"OPENCODE_SERVER_PASSWORD="+runtimeRow.Password,
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
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case waitErr := <-waited:
		_ = terminal.Close()
		t.Fatalf("OpenCode attach exited before connecting: %v", waitErr)
	case <-time.After(750 * time.Millisecond):
	}
	var stopped bool
	return func() {
		if stopped {
			return
		}
		stopped = true
		_ = cmd.Process.Kill()
		select {
		case <-waited:
		case <-time.After(time.Second):
		}
		_ = terminal.Close()
		select {
		case <-copied:
		case <-time.After(time.Second):
		}
	}
}

func runOpenCodeDarwinLayerSmokeShell(
	t *testing.T,
	runtimeRow db.OpenCodeRuntime,
	command string,
) string {
	t.Helper()
	endpoint := runtimeRow.ServerURL + "/session/" + url.PathEscape(runtimeRow.ConvID) +
		"/shell?directory=" + url.QueryEscape(runtimeRow.Cwd)
	request, err := openCodeRequest(http.MethodPost, endpoint, runtimeRow, map[string]any{
		"agent":   "build",
		"command": command,
	})
	require.NoError(t, err)
	response, err := opencodeapi.Do(openCodeHTTPClient, request, runtimeRow)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	require.NoError(t, err)
	require.Equalf(t, http.StatusOK, response.StatusCode,
		"OpenCode shell response: %s", body)
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

func logOpenCodeDarwinLayerSmokeServerLogs(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Logf("read OpenCode smoke log directory %s: %v", dir, err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Logf("read OpenCode smoke log %s: %v", path, readErr)
			continue
		}
		if len(raw) > 64<<10 {
			raw = raw[len(raw)-(64<<10):]
		}
		t.Logf("OpenCode smoke log %s:\n%s", path, raw)
	}
}
