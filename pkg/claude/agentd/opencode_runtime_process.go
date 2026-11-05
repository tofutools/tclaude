package agentd

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/opencodeapi"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func startOpenCodeProcess(
	runtime *db.OpenCodeRuntime,
	sandboxSpec *session.TclaudeLayerLaunchSpec,
) (*openCodeProcess, error) {
	authority, err := resolveOpenCodeLaunchAuthority(sandboxSpec)
	if err != nil {
		return nil, err
	}
	boundary, boundaryErr := buildOpenCodeExecutionBoundary(
		runtime.SandboxImplementation, sandboxSpec, runtime.Cwd, authority)
	if boundaryErr != nil {
		slog.Warn("could not record restarted OpenCode server execution boundary",
			"session_id", runtime.SessionID, "error", boundaryErr)
		boundary = ""
	}
	runtime.ExecutionBoundaryJSON = boundary
	return startOpenCodeProcessWithAuthority(runtime, sandboxSpec, authority)
}

func startOpenCodeProcessWithAuthority(
	runtime *db.OpenCodeRuntime,
	sandboxSpec *session.TclaudeLayerLaunchSpec,
	authority openCodeLaunchAuthority,
) (*openCodeProcess, error) {
	executable := authority.Executable
	if sandboxSpec != nil {
		if err := prepareOpenCodeTclaudeLayerState(sandboxSpec); err != nil {
			return nil, fmt.Errorf("prepare OpenCode with tclaude’s sandbox state: %w", err)
		}
		if err := prepareOpenCodeReadOnlyConfigForPlatform(sandboxSpec); err != nil {
			return nil, err
		}
	}
	parsed, err := url.Parse(runtime.ServerURL)
	if err != nil {
		return nil, err
	}
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return nil, fmt.Errorf("parse OpenCode server endpoint: %w", err)
	}
	command, args, extraFiles, unixHandshake, cleanup, err := openCodeServeProcessExecWithAuthority(
		executable, port, runtime, sandboxSpec, authority.Launcher)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	if err := validateOpenCodeFilteredProviderAuthority(sandboxSpec); err != nil {
		return nil, fmt.Errorf(
			"prepare OpenCode filtered provider authority: %w", err)
	}
	serverEnvironment := openCodeServerEnvironment(os.Environ(), sandboxSpec)
	serverEnvironment = append(serverEnvironment,
		"OPENCODE_SERVER_USERNAME="+openCodeServerUsername,
		"OPENCODE_SERVER_PASSWORD="+runtime.Password)
	if session.ExternalResourceDelegationDir() != "" {
		return startOpenCodeProcessThroughTmux(
			runtime, command, args, serverEnvironment, unixHandshake != nil)
	}
	cmd := exec.Command(command, args...)
	configureOpenCodeProcessGroup(cmd)
	cmd.Dir = runtime.Cwd
	cmd.Env = serverEnvironment
	cmd.Stdout = io.Discard
	stderr := newSpawnStderrCapture()
	cmd.Stderr = stderr
	cmd.ExtraFiles = extraFiles
	closeCgroupFD, err := configureOpenCodeResourceCgroup(cmd, runtime.ResourceCgroupDir)
	if err != nil {
		return nil, err
	}
	defer closeCgroupFD()
	// Snapshot before the server can contribute to the counter: this boundary is
	// durable across relaunches, so kills already recorded belong to an earlier
	// server and must not be attributed to this one's exit.
	oomBaseline := session.ReadResourceCgroupOOMKills(runtime.ResourceCgroupDir)
	if err := cmd.Start(); err != nil {
		if runtime.ResourceCgroupDir != "" {
			return nil, fmt.Errorf("%w: start server: %v", errOpenCodeResourceCgroup, err)
		}
		return nil, err
	}
	for _, file := range extraFiles {
		_ = file.Close()
	}
	process := &openCodeProcess{cmd: cmd, pid: cmd.Process.Pid, done: make(chan error, 1), convID: runtime.ConvID}
	openCodeProcesses.Lock()
	openCodeProcesses.bySession[runtime.SessionID] = process
	openCodeProcesses.Unlock()
	go func() {
		err := cmd.Wait()
		process.killWorkloadGroup()
		if runtime.ResourceCgroupDir != "" && session.ResourceCgroupOOMDeath(runtime.ResourceCgroupDir, oomBaseline, err) {
			if recordErr := db.SetSessionExitReason(runtime.SessionID, session.ResourceLimitOOMExitReason); recordErr != nil {
				slog.Warn("OpenCode resource limit: record OOM outcome", "session_id", runtime.SessionID, "error", recordErr)
			}
		}
		process.finish(err)
		finishOpenCodeProcessExit(process, runtime.SessionID, cmd.Process.Pid, err, stderr)
		// The durable boundary dir is deliberately NOT removed here. Removing it
		// raced relaunch reuse: stopOpenCodeProcess returns on process.done, which
		// fires before this goroutine runs, so a restart could reuse the recorded
		// dir just as this deleted it. An empty leftover dir costs nothing and the
		// next PrepareResourceCgroup for the session reclaims it.
	}()
	runtime.PID = cmd.Process.Pid
	if unixHandshake != nil {
		type authorityResult struct {
			authority opencodeapi.UnixLaunchAuthority
			err       error
		}
		authorityReady := make(chan authorityResult, 1)
		go func() {
			authority, readErr := opencodeapi.ReadUnixLaunchAuthority(
				unixHandshake.authority)
			authorityReady <- authorityResult{authority: authority, err: readErr}
		}()
		var result authorityResult
		select {
		case result = <-authorityReady:
		case processErr := <-process.done:
			if processErr == nil {
				processErr = fmt.Errorf("unix launcher exited before authority handshake")
			}
			stopOpenCodeProcess(*runtime, process)
			return nil, fmt.Errorf("OpenCode Unix launcher failed: %w: %s",
				processErr, stderr.String())
		case <-time.After(openCodeStartupTimeout):
			_ = unixHandshake.acknowledge.Close()
			stopOpenCodeProcess(*runtime, process)
			return nil, fmt.Errorf("OpenCode Unix launcher authority handshake timed out")
		}
		if result.err != nil {
			_ = unixHandshake.acknowledge.Close()
			stopOpenCodeProcess(*runtime, process)
			return nil, result.err
		}
		if err := persistAndAcknowledgeOpenCodeUnixLaunch(
			runtime, result.authority, unixHandshake.acknowledge); err != nil {
			_ = unixHandshake.acknowledge.Close()
			stopOpenCodeProcess(*runtime, process)
			return nil, err
		}
		_ = unixHandshake.acknowledge.Close()
	}
	deadline := time.Now().Add(openCodeStartupTimeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-process.done:
			if err == nil {
				err = fmt.Errorf("server exited before health check")
			}
			stopOpenCodeProcess(*runtime, process)
			return nil, fmt.Errorf("OpenCode server failed during startup: %w: %s", err, stderr.String())
		default:
		}
		// Port allocation necessarily has a bind-close-exec gap because
		// OpenCode does not accept a pre-bound listener. Never disclose the
		// password to that endpoint until the launched PID (or a child) is
		// positively observed owning its listening socket.
		if openCodeHealthy(*runtime) {
			return process, nil
		}
		select {
		case err := <-process.done:
			if err == nil {
				err = fmt.Errorf("server exited before health check")
			}
			stopOpenCodeProcess(*runtime, process)
			return nil, fmt.Errorf("OpenCode server failed during startup: %w: %s", err, stderr.String())
		case <-time.After(100 * time.Millisecond):
		}
	}
	stopOpenCodeProcess(*runtime, process)
	return nil, fmt.Errorf("OpenCode server at %s did not become healthy within %s",
		runtime.ServerURL, openCodeStartupTimeout)
}

func exposeOpenCodeExecutable(spec *session.TclaudeLayerLaunchSpec, executable string) (string, error) {
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("resolve OpenCode executable for sandbox: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve absolute OpenCode executable for sandbox: %w", err)
	}
	// The executable is launch infrastructure, not profile-authored access.
	// Reopen exactly this file read-only even when a constructed root or a
	// broad deny hides its installation directory. OpenCode's mutable XDG
	// roots remain the separate daemon-owned writable state contract.
	bind := session.TclaudeLayerReadOnlyBind{Source: resolved, Target: resolved}
	for _, existing := range spec.Contract.ReadOnlyBinds {
		if existing == bind {
			return resolved, nil
		}
	}
	spec.Contract.ReadOnlyBinds = append(
		[]session.TclaudeLayerReadOnlyBind{bind}, spec.Contract.ReadOnlyBinds...)
	return resolved, nil
}

func (process *openCodeProcess) rootPID() int {
	if process == nil {
		return 0
	}
	if process.pid > 0 {
		return process.pid
	}
	if process.cmd != nil && process.cmd.Process != nil {
		return process.cmd.Process.Pid
	}
	return 0
}

func (process *openCodeProcess) finish(err error) {
	process.doneOnce.Do(func() {
		process.done <- err
		close(process.done)
	})
}

func configureOpenCodeResourceCgroup(cmd *exec.Cmd, dir string) (func(), error) {
	if dir == "" {
		return func() {}, nil
	}
	cleanup, err := session.ConfigureProcessResourceCgroup(cmd, dir)
	if err != nil {
		return func() {}, fmt.Errorf("%w: %v", errOpenCodeResourceCgroup, err)
	}
	return cleanup, nil
}

func openCodeServeProcessExec(
	executable, port string,
	runtime *db.OpenCodeRuntime,
	sandboxSpec *session.TclaudeLayerLaunchSpec,
) (string, []string, []*os.File, *openCodeUnixLaunchHandshake, func(), error) {
	launcher, err := resolveOpenCodeLayerLauncher(sandboxSpec)
	if err != nil {
		return "", nil, nil, nil, func() {}, err
	}
	return openCodeServeProcessExecWithAuthority(
		executable, port, runtime, sandboxSpec, launcher)
}

func openCodeServeProcessExecWithAuthority(
	executable, port string,
	runtime *db.OpenCodeRuntime,
	sandboxSpec *session.TclaudeLayerLaunchSpec,
	launcher string,
) (string, []string, []*os.File, *openCodeUnixLaunchHandshake, func(), error) {
	noCleanup := func() {}
	if runtime.Transport != db.OpenCodeTransportUnixRelay {
		command, args, err := openCodeServeExecWithAuthority(executable, port, sandboxSpec, launcher, runtime.SessionID)
		return command, args, nil, nil, noCleanup, err
	}
	if sandboxSpec == nil || sandboxSpec.Version != session.TclaudeLayerUnixRelaySpecVersion {
		return "", nil, nil, nil, noCleanup, fmt.Errorf(
			"unix-relay OpenCode runtime requires a tclaude sandbox v4 spec")
	}
	selfPath, err := openCodeRelayExecutable()
	if err != nil {
		return "", nil, nil, nil, noCleanup,
			fmt.Errorf("resolve tclaude relay executable: %w", err)
	}
	serveArgs := []string{
		"serve", "--hostname", "127.0.0.1",
		"--port", port, "--log-level", openCodeServeLogLevel,
	}
	listenerFD, relayExecutableFD, err :=
		session.TclaudeLayerUnixRelayServerFDs(*sandboxSpec)
	if err != nil {
		return "", nil, nil, nil, noCleanup, err
	}
	relayWorkload := []string{executable}
	relayWorkload = append(relayWorkload, serveArgs...)
	gatewayCLIPath, err := session.HTTPProxyCLIForLayerSpec(sandboxSpec)
	if err != nil {
		return "", nil, nil, nil, noCleanup, err
	}
	wrappedWorkload := session.WrapHTTPProxyRuntimeCommand(runtime.SessionID, shellJoinOpenCodeCommand(relayWorkload[0], relayWorkload[1:]), gatewayCLIPath)
	if cfg, err := config.Load(); err == nil && cfg.HTTPProxyConfigured() {
		relayWorkload = []string{clcommon.BootstrapShellPath(), "-c", wrappedWorkload}
	}
	relayArgv := []string{
		"/proc/self/fd/" + strconv.Itoa(relayExecutableFD),
		opencodeapi.InheritedUnixRelayMode,
		strconv.Itoa(listenerFD), "127.0.0.1:" + port, "--",
	}
	relayArgv = append(relayArgv, relayWorkload...)
	argv, err := session.TclaudeLayerUnixRelayServerExecArgs(
		launcher, *sandboxSpec, 2, relayArgv)
	if err != nil {
		return "", nil, nil, nil, noCleanup, err
	}
	authorityR, authorityW, err := os.Pipe()
	if err != nil {
		return "", nil, nil, nil, noCleanup,
			fmt.Errorf("create OpenCode Unix authority pipe: %w", err)
	}
	gateR, gateW, err := os.Pipe()
	if err != nil {
		_ = authorityR.Close()
		_ = authorityW.Close()
		return "", nil, nil, nil, noCleanup,
			fmt.Errorf("create OpenCode Unix launch gate: %w", err)
	}
	cleanup := func() {
		_ = authorityR.Close()
		_ = authorityW.Close()
		_ = gateR.Close()
		_ = gateW.Close()
	}
	launcherArgs := []string{
		opencodeapi.UnixLaunchMode, runtime.ControlSocketPath, "--",
	}
	launcherArgs = append(launcherArgs, argv...)
	return selfPath, launcherArgs, []*os.File{authorityW, gateR},
		&openCodeUnixLaunchHandshake{
			authority: authorityR, acknowledge: gateW,
		}, cleanup, nil
}

func persistAndAcknowledgeOpenCodeUnixLaunch(
	runtime *db.OpenCodeRuntime,
	authority opencodeapi.UnixLaunchAuthority,
	acknowledge io.Writer,
) error {
	runtime.ControlSocketDevice = authority.Device
	runtime.ControlSocketInode = authority.Inode
	if err := db.UpsertOpenCodeRuntime(*runtime); err != nil {
		return fmt.Errorf(
			"persist provisional OpenCode Unix launch authority: %w", err)
	}
	written, err := acknowledge.Write([]byte{1})
	if err != nil {
		return fmt.Errorf("acknowledge OpenCode Unix launch authority: %w", err)
	}
	if written != 1 {
		return fmt.Errorf("acknowledge OpenCode Unix launch authority: %w", io.ErrShortWrite)
	}
	return nil
}

// finishOpenCodeProcessExit records a managed server's exit. It flags the
// process and cancels its SSE consumer the moment the server dies — otherwise
// the reconnect loop keeps spinning at its 1s cadence (a /proc scan + log line
// each time) until the reaper's ≤30s reconcile calls stopOpenCodeProcess. The
// cancel is read under the lock because ensureOpenCodeSSE may install it after
// this watcher starts; setting exited under the same lock closes that race so a
// later ensureOpenCodeSSE cannot launch a doomed loop against a dead server.
func finishOpenCodeProcessExit(process *openCodeProcess, sessionID string, pid int, waitErr error, stderr *spawnStderrCapture) {
	openCodeProcesses.Lock()
	process.exited = true
	cancel := process.cancel
	openCodeProcesses.Unlock()
	if cancel != nil {
		cancel()
	}
	if waitErr != nil {
		attrs := []any{"session", sessionID, "pid", pid, "error", waitErr}
		if stderr != nil {
			attrs = append(attrs, "stderr", stderr.String(),
				"stderr_truncated", stderr.Truncated())
		}
		slog.Warn("OpenCode server exited", attrs...)
	}
}

func allocateOpenCodeServerURL() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	address := listener.Addr().(*net.TCPAddr)
	if err := listener.Close(); err != nil {
		return "", err
	}
	return "http://127.0.0.1:" + strconv.Itoa(address.Port), nil
}

func randomOpenCodePassword() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("mint OpenCode server password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// killWorkloadGroup retires the launch-owned group once. The waiter calls it
// immediately at leader exit, before publishing done; later teardown must not
// signal a cached group ID after a long-dead leader could have been reused.
func (process *openCodeProcess) killWorkloadGroup() {
	process.groupKillOnce.Do(func() {
		if process.cmd != nil {
			killOpenCodeProcessGroup(process.cmd)
		}
	})
}
