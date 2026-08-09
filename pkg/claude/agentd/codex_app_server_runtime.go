package agentd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/codexappserver"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

const codexAppServerStartupTimeout = 15 * time.Second

type codexAppServerHandle struct {
	runtime     db.CodexAppServerRuntime
	client      *codexappserver.Client
	observation codexAppServerObservation
	// mutations serializes every tclaude-originated write to this thread. The
	// app-server connection itself supports concurrent calls, but the control
	// policy needs the thread/read snapshot and the following mutation to be
	// one ordered decision.
	mutations sync.Mutex
	compact   *codexCompactionStage
	nextOpID  uint64
}

var codexAppServerHandles = struct {
	sync.Mutex
	byConv       map[string]*codexAppServerHandle
	byGeneration map[string]*codexAppServerHandle
}{byConv: map[string]*codexAppServerHandle{}, byGeneration: map[string]*codexAppServerHandle{}}

// prepareCodexAppServerRuntime performs every check that can be proved before
// the pane exists, allocates an exclusive private generation, and records its
// warming state. A selected drive never falls back to the old pane channel.
func prepareCodexAppServerRuntime(args *clcommon.SpawnArgs) error {
	if args == nil || !args.CodexAppServer {
		return nil
	}
	owner := strings.TrimSpace(args.AgentID)
	if owner == "" {
		owner = strings.TrimSpace(args.ConvID)
	}
	if owner == "" {
		owner = strings.TrimSpace(args.Label)
	}
	if owner == "" {
		return errors.New("codex app-server launch needs an agent, conversation, or launch identity")
	}
	digest := sha256.Sum256([]byte(owner))
	ownerDir := hex.EncodeToString(digest[:8])
	var generationBytes [8]byte
	if _, err := rand.Read(generationBytes[:]); err != nil {
		return fmt.Errorf("mint Codex app-server generation: %w", err)
	}
	generation := hex.EncodeToString(generationBytes[:])
	root := filepath.Join(tclcommon.TclaudeAPIDir(), "codex", ownerDir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create Codex app-server owner directory: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return fmt.Errorf("secure Codex app-server owner directory: %w", err)
	}
	generationDir := filepath.Join(root, generation)
	if err := os.Mkdir(generationDir, 0o700); err != nil {
		return fmt.Errorf("create Codex app-server generation directory: %w", err)
	}
	args.CodexAppServerGeneration = generation
	args.CodexAppServerSocket = filepath.Join(generationDir, "app.sock")
	args.CodexAppServerPIDFile = filepath.Join(generationDir, "server.pid")
	args.CodexAppServerLogFile = filepath.Join(generationDir, "server.log")
	runtime := db.CodexAppServerRuntime{
		Generation: generation, LaunchID: firstNonEmpty(args.Label, args.ConvID),
		AgentID: owner, ConvID: args.ConvID, SocketPath: args.CodexAppServerSocket,
		State: db.CodexAppServerWarming,
	}
	if err := db.UpsertCodexAppServerRuntime(runtime); err != nil {
		_ = os.Remove(generationDir)
		return fmt.Errorf("persist warming Codex app-server runtime: %w", err)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return "unknown"
}

func failPreparedCodexAppServerRuntime(args clcommon.SpawnArgs, cause error) {
	if !args.CodexAppServer || args.CodexAppServerGeneration == "" {
		return
	}
	runtime, _ := db.GetCodexAppServerRuntime(args.CodexAppServerGeneration)
	if runtime != nil {
		runtime.State = db.CodexAppServerUnavailable
		runtime.Detail = cause.Error()
		_ = db.UpsertCodexAppServerRuntime(*runtime)
	}
	removeCodexAppServerGeneration(args.CodexAppServerSocket)
}

func startCodexAppServerBootstrap(args clcommon.SpawnArgs) {
	if !args.CodexAppServer || args.CodexAppServerGeneration == "" {
		return
	}
	go runCodexAppServerBootstrap(args)
}

func runCodexAppServerBootstrap(args clcommon.SpawnArgs) {
	ctx, cancel := context.WithTimeout(context.Background(), codexAppServerStartupTimeout)
	defer cancel()
	runtime, err := db.GetCodexAppServerRuntime(args.CodexAppServerGeneration)
	if err != nil || runtime == nil {
		return
	}
	fail := func(cause error) {
		runtime.State = db.CodexAppServerUnavailable
		runtime.Detail = cause.Error()
		_ = db.UpsertCodexAppServerRuntime(*runtime)
		slog.Error("Codex app-server control unavailable; refusing send-keys fallback",
			"generation", runtime.Generation, "error", cause)
	}
	version, err := waitForCodexAppServerVersion(ctx, args.CodexAppServerGeneration)
	if err != nil {
		fail(err)
		return
	}
	runtime.CodexVersion = version

	pid, err := waitForCodexAppServerPID(ctx, args.CodexAppServerPIDFile)
	if err != nil {
		fail(err)
		return
	}
	runtime.ServerPID = pid
	if err := waitForOwnedCodexSocket(ctx, runtime.SocketPath, pid); err != nil {
		fail(err)
		return
	}
	client, err := codexappserver.Dial(ctx, runtime.SocketPath,
		&codexappserver.Options{CodexVersion: runtime.CodexVersion})
	if err != nil {
		fail(err)
		return
	}

	expected := strings.TrimSpace(args.ConvID)
	var threadID string
	for threadID == "" {
		loaded, listErr := client.ListLoadedThreads(ctx, codexappserver.ThreadLoadedListParams{})
		if listErr != nil {
			_ = client.Close()
			fail(listErr)
			return
		}
		if expected != "" {
			for _, candidate := range loaded.Data {
				if candidate == expected {
					threadID = candidate
					break
				}
			}
			if len(loaded.Data) > 1 {
				_ = client.Close()
				fail(fmt.Errorf("ambiguous resume binding: loaded threads %v", loaded.Data))
				return
			}
		} else if len(loaded.Data) == 1 {
			threadID = loaded.Data[0]
		} else if len(loaded.Data) > 1 {
			_ = client.Close()
			fail(fmt.Errorf("ambiguous birth binding: loaded threads %v", loaded.Data))
			return
		}
		if threadID == "" {
			select {
			case <-ctx.Done():
				_ = client.Close()
				fail(fmt.Errorf("bind TUI-created thread: %w", ctx.Err()))
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	thread, err := client.ReadThread(ctx, codexappserver.ThreadReadParams{ThreadID: threadID, IncludeTurns: true})
	if err != nil || thread.ID != threadID {
		_ = client.Close()
		if err == nil {
			err = fmt.Errorf("thread/read returned %q, want %q", thread.ID, threadID)
		}
		fail(err)
		return
	}
	runtime.ConvID = threadID
	runtime.ThreadID = threadID
	runtime.State = db.CodexAppServerReady
	runtime.Detail = ""
	if err := db.UpsertCodexAppServerRuntime(*runtime); err != nil {
		_ = client.Close()
		fail(fmt.Errorf("persist verified thread binding: %w", err))
		return
	}
	handle := &codexAppServerHandle{runtime: *runtime, client: client}
	codexAppServerHandles.Lock()
	codexAppServerHandles.byConv[threadID] = handle
	codexAppServerHandles.byGeneration[runtime.Generation] = handle
	codexAppServerHandles.Unlock()
	projectCodexAppServerRawStatus(handle, thread.Status, time.Now().UTC(), "app-server snapshot")
	go watchCodexAppServerHandle(handle)
}

func waitForCodexAppServerVersion(ctx context.Context, generation string) (string, error) {
	for {
		runtime, err := db.GetCodexAppServerRuntime(generation)
		if err != nil {
			return "", err
		}
		if runtime == nil {
			return "", fmt.Errorf("codex app-server runtime %q disappeared", generation)
		}
		if runtime.State == db.CodexAppServerUnavailable || runtime.State == db.CodexAppServerDead {
			return "", fmt.Errorf("codex app-server version proof failed: %s", runtime.Detail)
		}
		if strings.TrimSpace(runtime.CodexVersion) != "" {
			return runtime.CodexVersion, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("wait for exact Codex version proof: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func waitForCodexAppServerPID(ctx context.Context, path string) (int, error) {
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 1 {
				return pid, nil
			}
			return 0, fmt.Errorf("invalid Codex app-server pid file %s", path)
		}
		if !os.IsNotExist(err) {
			return 0, err
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("wait for Codex app-server pid: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func waitForOwnedCodexSocket(ctx context.Context, path string, pid int) error {
	for {
		info, err := os.Lstat(path)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
				return fmt.Errorf("codex app-server endpoint is not a plain Unix socket: %s", path)
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || int(stat.Uid) != os.Getuid() {
				return fmt.Errorf("codex app-server socket has wrong owner")
			}
			if info.Mode().Perm() != 0o600 {
				return fmt.Errorf("codex app-server socket mode is %04o, want 0600", info.Mode().Perm())
			}
			if err := processAlive(pid); err != nil {
				return fmt.Errorf("codex app-server process is not alive: %w", err)
			}
			return nil
		}
		if !os.IsNotExist(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for Codex app-server socket: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func processAlive(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(syscall.Signal(0))
}

func watchCodexAppServerHandle(handle *codexAppServerHandle) {
	handle.runtime.State, handle.runtime.Detail = runCodexAppServerObserver(handle)
	if changed, err := db.MarkCodexAppServerRuntimeTerminalIfUnreplaced(
		handle.runtime.Generation, handle.runtime.State, handle.runtime.Detail); err != nil {
		slog.Warn("record Codex app-server terminal state", "generation", handle.runtime.Generation, "error", err)
	} else if !changed {
		slog.Debug("ignored obsolete Codex app-server watcher after replacement became ready",
			"generation", handle.runtime.Generation, "conv", handle.runtime.ConvID)
	}
	codexAppServerHandles.Lock()
	if codexAppServerHandles.byConv[handle.runtime.ConvID] == handle {
		delete(codexAppServerHandles.byConv, handle.runtime.ConvID)
	}
	if codexAppServerHandles.byGeneration[handle.runtime.Generation] == handle {
		delete(codexAppServerHandles.byGeneration, handle.runtime.Generation)
	}
	codexAppServerHandles.Unlock()
}

func stopCodexAppServerRuntimeForConv(convID string) {
	stopCodexAppServerRuntime(convID, "")
}

func stopCodexAppServerRuntime(convID, launchID string) {
	runtime, err := db.GetCodexAppServerRuntimeByConvID(convID)
	if (err != nil || runtime == nil) && launchID != "" {
		runtime, err = db.GetCodexAppServerRuntimeByLaunchID(launchID)
	}
	if err != nil || runtime == nil {
		return
	}
	codexAppServerHandles.Lock()
	handle := codexAppServerHandles.byGeneration[runtime.Generation]
	delete(codexAppServerHandles.byGeneration, runtime.Generation)
	if codexAppServerHandles.byConv[runtime.ConvID] == handle {
		delete(codexAppServerHandles.byConv, runtime.ConvID)
	}
	codexAppServerHandles.Unlock()
	if handle != nil {
		_ = handle.client.Close()
	}
	if runtime.ServerPID > 1 {
		if process, findErr := os.FindProcess(runtime.ServerPID); findErr == nil {
			_ = process.Signal(syscall.SIGTERM)
		}
	}
	runtime.State = db.CodexAppServerDead
	runtime.Detail = "launch exited"
	_ = db.UpsertCodexAppServerRuntime(*runtime)
	removeCodexAppServerGeneration(runtime.SocketPath)
}

func stopFailedCodexAppServerLaunch(convID, launchID, tmuxSession string) {
	stopCodexAppServerRuntime(convID, launchID)
	if strings.TrimSpace(tmuxSession) == "" {
		return
	}
	if err := clcommon.TmuxCommand("kill-session", "-t", clcommon.ExactTarget(tmuxSession)).Run(); err != nil {
		slog.Warn("stop failed Codex app-server launch", "session", tmuxSession, "error", err)
	}
}

func removeCodexAppServerGeneration(socketPath string) {
	dir := filepath.Dir(socketPath)
	if filepath.Base(socketPath) != "app.sock" || filepath.Base(filepath.Dir(filepath.Dir(dir))) != "codex" {
		return
	}
	_ = os.Remove(socketPath)
	_ = os.Remove(filepath.Join(dir, "server.pid"))
	// Keep a non-empty server.log for diagnostics; remove an empty one so the
	// generation can disappear cleanly.
	if info, err := os.Stat(filepath.Join(dir, "server.log")); err == nil && info.Size() == 0 {
		_ = os.Remove(filepath.Join(dir, "server.log"))
	}
	_ = os.Remove(dir)
}

func codexAppServerReady(convID string) bool {
	codexAppServerHandles.Lock()
	defer codexAppServerHandles.Unlock()
	return codexAppServerHandles.byConv[convID] != nil
}

func codexAppServerHandleForConv(convID string) *codexAppServerHandle {
	codexAppServerHandles.Lock()
	defer codexAppServerHandles.Unlock()
	return codexAppServerHandles.byConv[convID]
}

// codexAppServerSelected is deliberately broader than readiness. Once a
// launch selected the app-server drive, warming, disconnected, and failed
// control states remain on that drive and must never reopen the pane-input
// fallback.
func codexAppServerSelected(convID string) (bool, error) {
	profile, err := db.RecordedLaunchPostureForConv(convID)
	if err != nil {
		return false, err
	}
	if profile != nil && profile.CodexAppServer != nil {
		return *profile.CodexAppServer, nil
	}
	// Runtime rows describe generations; they do not select the current drive.
	// In particular, a historical row must never override a later explicit
	// --codex-app-server=false posture.
	return false, nil
}

func awaitCodexAppServer(convID string) bool {
	return awaitCodexAppServerLaunch(convID, "")
}

// Lifecycle seams keep failure-path tests fast without weakening the
// production readiness check or its timeout.
var (
	awaitCodexAppServerReady       = awaitCodexAppServer
	awaitCodexAppServerLaunchReady = awaitCodexAppServerLaunch
)

func awaitCodexAppServerLaunch(convID, launchID string) bool {
	deadline := time.Now().Add(codexAppServerStartupTimeout)
	for time.Now().Before(deadline) {
		if codexAppServerReady(convID) {
			return true
		}
		runtime, _ := db.GetCodexAppServerRuntimeByConvID(convID)
		if runtime == nil && launchID != "" {
			runtime, _ = db.GetCodexAppServerRuntimeByLaunchID(launchID)
		}
		if runtime != nil && (runtime.State == db.CodexAppServerUnavailable || runtime.State == db.CodexAppServerDead) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}
