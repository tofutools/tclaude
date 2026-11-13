package agentd

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/tmuxcmd"
	"github.com/tofutools/tclaude/pkg/claude/opencodeapi"
	"github.com/tofutools/tclaude/pkg/claude/session"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

func openCodeManagedTmuxSession(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return fmt.Sprintf("__tclaude-opencode-%x", sum[:10])
}

type openCodeTmuxHandshake struct {
	dir        string
	statusPath string
	gatePath   string
	stderrPath string
	status     *os.File
	gate       *os.File
}

var openCodeTmuxHandshakeDataDir = tclcommon.TclaudeDataDir

func prepareOpenCodeTmuxHandshake() (*openCodeTmuxHandshake, error) {
	return prepareOpenCodeTmuxLaunchFiles(true)
}

func prepareOpenCodeTmuxLaunchFiles(needsUnixHandshake bool) (*openCodeTmuxHandshake, error) {
	// The launcher is forked by the external tmux runtime, which may have a
	// different private temporary-directory namespace from agentd. Keep its
	// startup diagnostics and optional cross-process handshake in tclaude's
	// private persistent state instead of assuming that os.TempDir is shared
	// between the two services.
	dataDir := strings.TrimSpace(openCodeTmuxHandshakeDataDir())
	if dataDir == "" {
		return nil, fmt.Errorf("resolve private OpenCode tmux handshake directory")
	}
	root := filepath.Join(dataDir, "opencode-launch-handshakes")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create private OpenCode tmux handshake root: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect private OpenCode tmux handshake root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("private OpenCode tmux handshake root is not a real directory")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("protect private OpenCode tmux handshake root: %w", err)
	}
	dir, err := os.MkdirTemp(root, "launch-")
	if err != nil {
		return nil, fmt.Errorf("create OpenCode tmux handshake directory: %w", err)
	}
	h := &openCodeTmuxHandshake{dir: dir, stderrPath: filepath.Join(dir, "stderr")}
	fail := func(err error) (*openCodeTmuxHandshake, error) {
		h.close()
		return nil, err
	}
	stderr, err := os.OpenFile(h.stderrPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(fmt.Errorf("create OpenCode tmux startup stderr: %w", err))
	}
	if err := stderr.Close(); err != nil {
		return fail(fmt.Errorf("close OpenCode tmux startup stderr: %w", err))
	}
	if !needsUnixHandshake {
		return h, nil
	}
	h.statusPath = filepath.Join(dir, "authority")
	h.gatePath = filepath.Join(dir, "gate")
	if err := syscall.Mkfifo(h.statusPath, 0o600); err != nil {
		return fail(fmt.Errorf("create OpenCode tmux authority fifo: %w", err))
	}
	if err := syscall.Mkfifo(h.gatePath, 0o600); err != nil {
		return fail(fmt.Errorf("create OpenCode tmux launch gate: %w", err))
	}
	h.status, err = os.OpenFile(h.statusPath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fail(fmt.Errorf("open OpenCode tmux authority fifo: %w", err))
	}
	return h, nil
}

func (h *openCodeTmuxHandshake) close() {
	if h == nil {
		return
	}
	if h.status != nil {
		_ = h.status.Close()
	}
	if h.gate != nil {
		_ = h.gate.Close()
	}
	if h.dir != "" {
		_ = os.RemoveAll(h.dir)
	}
}

func (h *openCodeTmuxHandshake) needsUnixHandshake() bool {
	return h != nil && h.statusPath != "" && h.gatePath != ""
}

func (h *openCodeTmuxHandshake) connectGate(deadline time.Time) error {
	for time.Now().Before(deadline) {
		gate, err := os.OpenFile(h.gatePath, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			h.gate = gate
			// Gate readiness proves the launcher has opened the status writer.
			// Reopen the reader in blocking mode now; FIFO deadlines are not
			// supported on macOS, while a nonblocking reader can return EAGAIN
			// before the launcher writes its authority payload.
			status, openErr := os.OpenFile(h.statusPath, os.O_RDONLY, 0)
			if openErr != nil {
				return fmt.Errorf("reopen OpenCode tmux authority fifo: %w", openErr)
			}
			_ = h.status.Close()
			h.status = status
			return nil
		}
		if !errors.Is(err, syscall.ENXIO) {
			return fmt.Errorf("open OpenCode tmux launch gate: %w", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("OpenCode tmux launch gate did not become ready")
}

func awaitOpenCodeTmuxAuthority(handshake *openCodeTmuxHandshake,
	process *openCodeProcess, timeout time.Duration) (opencodeapi.UnixLaunchAuthority, error) {
	type authorityResult struct {
		authority opencodeapi.UnixLaunchAuthority
		err       error
	}
	authorityReady := make(chan authorityResult, 1)
	go func() {
		authority, err := opencodeapi.ReadUnixLaunchAuthority(handshake.status)
		authorityReady <- authorityResult{authority: authority, err: err}
	}()
	select {
	case result := <-authorityReady:
		return result.authority, result.err
	case processErr := <-process.done:
		if processErr == nil {
			processErr = fmt.Errorf("unix launcher exited before authority handshake")
		}
		return opencodeapi.UnixLaunchAuthority{}, processErr
	case <-time.After(timeout):
		return opencodeapi.UnixLaunchAuthority{},
			fmt.Errorf("OpenCode Unix launcher authority handshake timed out")
	}
}

func shellJoinOpenCodeCommand(command string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, clcommon.ShellQuoteArg(command))
	for _, arg := range args {
		parts = append(parts, clcommon.ShellQuoteArg(arg))
	}
	return strings.Join(parts, " ")
}

func openCodeTmuxLaunchCommand(runtime db.OpenCodeRuntime, command string, args,
	serverEnvironment []string, handshake *openCodeTmuxHandshake) string {
	parts := make([]string, 0, len(serverEnvironment)+1)
	parts = append(parts, "env")
	for _, entry := range serverEnvironment {
		parts = append(parts, clcommon.ShellQuoteArg(entry))
	}
	parts = append(parts, shellJoinOpenCodeCommand(command, args))
	serverCommand := strings.Join(parts, " ")
	if handshake != nil && handshake.statusPath != "" && handshake.gatePath != "" {
		serverCommand += " 3>" + clcommon.ShellQuoteArg(handshake.statusPath) +
			" 4<" + clcommon.ShellQuoteArg(handshake.gatePath)
	}
	if runtime.ResourceCgroupDir != "" {
		serverCommand = session.WrapPreparedResourceCgroupCommand(
			runtime.SessionID, runtime.ResourceCgroupDir, serverCommand, false)
	}
	// Capture outside the optional resource-limit wrapper. Failures in that
	// wrapper, or in the shell while opening the handshake FIFOs, happen before
	// the inner launcher runs and would otherwise disappear with tmux's pane.
	if handshake != nil && handshake.stderrPath != "" {
		serverCommand += " 2>" + clcommon.ShellQuoteArg(handshake.stderrPath)
	}
	return "exec " + serverCommand
}

func openCodeTmuxLaunchArgs(runtime db.OpenCodeRuntime, command string, args,
	serverEnvironment []string, handshake *openCodeTmuxHandshake) ([]string, func(), error) {
	// Keep the generated command out of tmux's size-limited initial argv and
	// away from the user's default shell. The private script self-deletes as
	// its first action; tmux execs Bash directly because it receives multiple
	// command arguments.
	scriptPath, cleanup, err := session.WriteLaunchScript(openCodeTmuxLaunchCommand(
		runtime, command, args, serverEnvironment, handshake))
	if err != nil {
		return nil, func() {}, err
	}
	return []string{"/bin/bash", scriptPath}, cleanup, nil
}

func startOpenCodeProcessThroughTmux(runtime *db.OpenCodeRuntime, command string,
	args, serverEnvironment []string, needsUnixHandshake bool) (*openCodeProcess, error) {
	var handshake *openCodeTmuxHandshake
	var err error
	handshake, err = prepareOpenCodeTmuxLaunchFiles(needsUnixHandshake)
	if err != nil {
		return nil, err
	}
	defer handshake.close()
	tmuxSession := openCodeManagedTmuxSession(runtime.SessionID)
	if err := reclaimOrphanedOpenCodeTmuxSession(tmuxSession); err != nil {
		return nil, err
	}
	tmuxArgs := []string{"new-session", "-d", "-s", tmuxSession, "-c", runtime.Cwd,
		"-x", strconv.Itoa(clcommon.CanonicalAgentPaneWidth),
		"-y", strconv.Itoa(clcommon.CanonicalAgentPaneHeight)}
	launchArgs, cleanupScript, err := openCodeTmuxLaunchArgs(
		*runtime, command, args, serverEnvironment, handshake)
	if err != nil {
		return nil, err
	}
	tmuxArgs = append(tmuxArgs, launchArgs...)
	stderr := newSpawnStderrCapture()
	launch := clcommon.Default.Command(session.ExternalTmuxNoStartArgs(tmuxArgs...)...)
	launch.Stderr = stderr
	if err := launch.Run(); err != nil {
		cleanupScript()
		return nil, fmt.Errorf("launch OpenCode server through external tmux: %w: %s", err, stderr.String())
	}
	pidOut, err := session.ExternalTmux().Display(tmuxcmd.Session(tmuxSession).FirstPane(), "#{pane_pid}")
	if err != nil {
		_ = session.ExternalTmux().KillSession(tmuxcmd.Session(tmuxSession))
		return nil, fmt.Errorf("read OpenCode tmux process root: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(pidOut))
	if err != nil || pid <= 1 {
		_ = tmuxcmd.Local().NoStart().KillSession(tmuxcmd.Session(tmuxSession))
		return nil, fmt.Errorf("external tmux returned invalid OpenCode process root %q", strings.TrimSpace(pidOut))
	}
	process := &openCodeProcess{pid: pid, tmuxSession: tmuxSession,
		done: make(chan error, 1), convID: runtime.ConvID}
	openCodeProcesses.Lock()
	openCodeProcesses.bySession[runtime.SessionID] = process
	openCodeProcesses.Unlock()
	runtime.PID = pid
	go watchOpenCodeTmuxProcess(process, *runtime)
	if handshake.needsUnixHandshake() {
		if err := handshake.connectGate(time.Now().Add(openCodeStartupTimeout)); err != nil {
			output := captureOpenCodeTmuxStartup(handshake, tmuxSession)
			stopOpenCodeProcess(*runtime, process)
			return nil, openCodeTmuxStartupError(err, output)
		}
		authority, err := awaitOpenCodeTmuxAuthority(
			handshake, process, openCodeStartupTimeout)
		if err != nil {
			output := captureOpenCodeTmuxStartup(handshake, tmuxSession)
			stopOpenCodeProcess(*runtime, process)
			return nil, openCodeTmuxStartupError(err, output)
		}
		if err := persistAndAcknowledgeOpenCodeUnixLaunch(runtime, authority, handshake.gate); err != nil {
			stopOpenCodeProcess(*runtime, process)
			return nil, err
		}
	}
	deadline := time.Now().Add(openCodeStartupTimeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-process.done:
			if err == nil {
				err = fmt.Errorf("server exited before health check")
			}
			paneOutput := captureOpenCodeTmuxStartup(handshake, tmuxSession)
			stopOpenCodeProcess(*runtime, process)
			return nil, fmt.Errorf("OpenCode server failed during startup: %w: %s", err, paneOutput)
		default:
		}
		if openCodeHealthy(*runtime) {
			return process, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	paneOutput := captureOpenCodeTmuxStartup(handshake, tmuxSession)
	stopOpenCodeProcess(*runtime, process)
	return nil, fmt.Errorf("OpenCode server at %s did not become healthy within %s: %s",
		runtime.ServerURL, openCodeStartupTimeout, paneOutput)
}

func captureOpenCodeTmuxStartup(handshake *openCodeTmuxHandshake, tmuxSession string) string {
	if handshake != nil && handshake.stderrPath != "" {
		file, err := os.Open(handshake.stderrPath)
		if err == nil {
			defer file.Close()
			raw, readErr := io.ReadAll(io.LimitReader(file, spawnStderrMax+1))
			if readErr == nil {
				if len(raw) > spawnStderrMax {
					raw = raw[:spawnStderrMax]
				}
				if output := strings.TrimSpace(string(raw)); output != "" {
					return output
				}
			}
		}
	}
	return captureOpenCodeTmuxPane(tmuxSession)
}

func openCodeTmuxStartupError(err error, output string) error {
	if strings.TrimSpace(output) == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, output)
}

func reclaimOrphanedOpenCodeTmuxSession(tmuxSession string) error {
	if exists, _ := tmuxcmd.Local().NoStart().HasSession(tmuxcmd.Session(tmuxSession)); !exists {
		return nil
	}
	if err := tmuxcmd.Local().NoStart().KillSession(tmuxcmd.Session(tmuxSession)); err != nil {
		return fmt.Errorf("reclaim orphaned OpenCode tmux session: %w", err)
	}
	return nil
}

func captureOpenCodeTmuxPane(tmuxSession string) string {
	out, err := clcommon.TmuxCombinedOutput(
		"-N", "capture-pane", "-p", "-S", "-200", "-t",
		clcommon.ExactTarget(tmuxSession)+":0.0")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func watchOpenCodeTmuxProcess(process *openCodeProcess, runtime db.OpenCodeRuntime) {
	misses := 0
	for {
		if exists, _ := tmuxcmd.Local().NoStart().HasSession(tmuxcmd.Session(process.tmuxSession)); !exists {
			misses++
			if misses < openCodeHealthAttempts {
				time.Sleep(openCodeTmuxProbeInterval)
				continue
			}
			openCodeProcesses.Lock()
			stopping := process.stopping
			openCodeProcesses.Unlock()
			var waitErr error
			if !stopping {
				waitErr = fmt.Errorf("managed tmux session exited")
			}
			process.finish(waitErr)
			finishOpenCodeProcessExit(process, runtime.SessionID, process.pid, waitErr, nil)
			return
		}
		misses = 0
		time.Sleep(openCodeTmuxProbeInterval)
	}
}

func openCodeTmuxPanePID(tmuxSession string) int {
	pid, _ := tmuxcmd.Local().NoStart().PanePID(tmuxcmd.Session(tmuxSession).FirstPane())
	return pid
}
