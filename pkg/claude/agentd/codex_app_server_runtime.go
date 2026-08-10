package agentd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/codexappserver"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

const codexAppServerStartupTimeout = 15 * time.Second
const codexAppServerIdentityFile = "server.identity"
const codexAppServerTokenHandoffFile = "tui-capability.handoff"
const codexAppServerEndpointFile = "server.endpoint"
const codexAppServerProofFile = "server.proved"

var codexAppServerRecoveryOwner = func() string {
	var token [16]byte
	if _, err := rand.Read(token[:]); err == nil {
		return "agentd:" + hex.EncodeToString(token[:])
	}
	return fmt.Sprintf("agentd:%d:%d", os.Getpid(), time.Now().UnixNano())
}()

var reconcileCodexNativePermissionRegistry = session.ReconcileCodexNativePermissionRegistry

func reconcileCodexNativePermissionRegistryForGeneration(generation string) error {
	before, err := db.GetCodexNativePermissionProfile(generation)
	if err != nil {
		return fmt.Errorf("load native Codex permission profile before reconciliation: %w", err)
	}
	// No row means this is an outer-layer or legacy app-server generation. Its
	// readiness must not depend on machine-wide native-registry setup used by
	// unrelated builtin-sandbox launches.
	if before == nil {
		return nil
	}
	if err := reconcileCodexNativePermissionRegistry(); err != nil {
		return err
	}
	// Outer-layer and legacy app-server generations deliberately have no native
	// profile. When this generation did register one, however, pruning must not
	// silently make the runtime ready without its exact enforcement profile.
	if before.CleanupPending {
		return errors.New("native Codex permission profile cleanup is pending")
	}
	after, err := db.GetCodexNativePermissionProfile(generation)
	if err != nil {
		return fmt.Errorf("reload native Codex permission profile after reconciliation: %w", err)
	}
	if after == nil || after.CleanupPending {
		return errors.New("native Codex permission profile was removed during reconciliation")
	}
	return nil
}

type codexAppServerHandle struct {
	// runtime is immutable generation identity after registration. Reconnects
	// replace only client while holding mutations, which every control caller
	// also holds; the observer that owned the old client has already returned.
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
	closing   bool
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
	if session.CodexNativeRegistryApplicable(args.CodexAppServer, args.Harness,
		args.Sandbox, args.SandboxImplementation) {
		if err := codexNativeRegistryReadiness(); err != nil {
			return err
		}
		if err := adoptLiveCodexProfilesIntoInstalledRegistry(); err != nil {
			return fmt.Errorf("protect live generated Codex profiles before registry activation: %w", err)
		}
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
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		_ = os.Remove(generationDir)
		return fmt.Errorf("allocate Codex app-server loopback endpoint: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		_ = os.Remove(generationDir)
		return fmt.Errorf("release Codex app-server loopback endpoint: %w", err)
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		_ = os.Remove(generationDir)
		return fmt.Errorf("mint Codex app-server capability: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(secret[:])
	tokenDigest := sha256.Sum256([]byte(token))
	args.CodexAppServerURL = fmt.Sprintf("ws://127.0.0.1:%d", port)
	args.CodexAppServerTokenSHA256 = hex.EncodeToString(tokenDigest[:])
	args.CodexAppServerTokenHandoff = filepath.Join(generationDir, codexAppServerTokenHandoffFile)
	if err := writeExclusiveSecret(filepath.Join(generationDir, codexAppServerEndpointFile), args.CodexAppServerURL); err != nil {
		_ = os.RemoveAll(generationDir)
		return fmt.Errorf("record Codex app-server loopback endpoint: %w", err)
	}
	if err := writeExclusiveSecret(args.CodexAppServerTokenHandoff, token); err != nil {
		_ = os.Remove(generationDir)
		return fmt.Errorf("write one-shot Codex TUI capability handoff: %w", err)
	}
	runtime := db.CodexAppServerRuntime{
		Generation: generation, LaunchID: firstNonEmpty(args.Label, args.ConvID),
		AgentID: owner, ConvID: args.ConvID, SocketPath: args.CodexAppServerSocket,
		State: db.CodexAppServerWarming,
	}
	if err := db.InsertCodexAppServerRuntimeWithCapability(runtime, token); err != nil {
		_ = os.RemoveAll(generationDir)
		return fmt.Errorf("persist warming Codex app-server runtime and capability: %w", err)
	}
	return nil
}

func writeExclusiveSecret(path, token string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(token + "\n"); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

var codexAppServerCapability = db.GetCodexAppServerCapability
var codexAppServerProcessOwnsEndpoint = processOwnsCodexAppServerEndpoint
var codexAppServerServerPID = discoverCodexAppServerPID

var codexAppServerRelayPID = discoverCodexAppServerRelayPID

func readCodexAppServerEndpoint(socketPath string) (string, error) {
	data, err := os.ReadFile(filepath.Join(filepath.Dir(socketPath), codexAppServerEndpointFile))
	if err != nil {
		return "", err
	}
	endpoint := strings.TrimSpace(string(data))
	if !strings.HasPrefix(endpoint, "ws://127.0.0.1:") {
		return "", errors.New("recorded Codex app-server endpoint is not IPv4 loopback")
	}
	return endpoint, nil
}

var codexAppServerEndpoint = readCodexAppServerEndpoint

func waitForCodexAppServerEndpointOwner(ctx context.Context, pid int, socketPath string) error {
	endpoint, err := codexAppServerEndpoint(socketPath)
	if err != nil {
		return err
	}
	for {
		if codexAppServerProcessOwnsEndpoint(pid, endpoint) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("prove Codex app-server loopback listener owner: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func codexAppServerClientOptions(runtime db.CodexAppServerRuntime) (*codexappserver.Options, error) {
	token, err := codexAppServerCapability(runtime.Generation)
	if err != nil {
		return nil, err
	}
	return &codexappserver.Options{CodexVersion: runtime.CodexVersion, BearerToken: token}, nil
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
	if err := session.UnregisterCodexNativePermissionProfile(args.CodexAppServerGeneration); err != nil {
		slog.Warn("roll back failed Codex native permission profile",
			"generation", args.CodexAppServerGeneration, "error", err)
	}
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
		if _, updateErr := db.FailCodexAppServerRuntimeBootstrap(runtime.Generation, cause.Error()); updateErr != nil {
			slog.Warn("record Codex app-server bootstrap failure",
				"generation", runtime.Generation, "error", updateErr)
		}
		slog.Error("Codex app-server control unavailable; refusing send-keys fallback",
			"generation", runtime.Generation, "error", cause)
	}
	version, err := waitForCodexAppServerVersion(ctx, args.CodexAppServerGeneration)
	if err != nil {
		fail(err)
		return
	}
	runtime.CodexVersion = version

	pid, err := codexAppServerServerPID(ctx, runtime.SocketPath, args.CodexAppServerPIDFile)
	if err != nil {
		fail(err)
		return
	}
	runtime.ServerPID = pid
	relayPID, err := codexAppServerRelayPID(ctx, runtime.SocketPath)
	if err != nil {
		fail(err)
		return
	}
	if err := waitForOwnedCodexSocket(ctx, runtime.SocketPath, relayPID); err != nil {
		fail(err)
		return
	}
	if err := waitForCodexAppServerEndpointOwner(ctx, pid, runtime.SocketPath); err != nil {
		fail(err)
		return
	}
	if err := recordCodexAppServerProcessIdentity(runtime.SocketPath, relayPID); err != nil {
		fail(err)
		return
	}
	// Release the TUI only after agentd has proved both process identities and
	// the exact native listener owner. Until then the one-shot capability stays
	// unopened, so an endpoint-allocation race cannot harvest it.
	if err := recordCodexAppServerProof(runtime.SocketPath); err != nil {
		fail(fmt.Errorf("record Codex app-server listener proof: %w", err))
		return
	}
	// Do not dial before the TUI hook has proved that a FRESH thread exists and is
	// bound. In Codex 0.147 a fresh thread auto-subscribes every connection that
	// is already initialized, even if it never calls thread/resume. Waiting
	// before Dial makes approval ownership independent of goroutine birth order.
	threadID := ""
	if args.CodexAppServerExistingThread {
		threadID = strings.TrimSpace(args.ConvID)
		if threadID == "" || runtime.ConvID != threadID {
			fail(errors.New("could not prove the exact existing-thread Codex resume launch/pane/argv"))
			return
		}
		if err := waitForCodexAppServerResumedTUI(ctx, *runtime, threadID); err != nil {
			fail(errors.New("could not prove the exact existing-thread Codex resume launch/pane/argv"))
			return
		}
	} else {
		threadID, err = waitForCodexAppServerTUIBinding(ctx, runtime.Generation)
		if err != nil {
			fail(err)
			return
		}
	}
	clientOptions, err := codexAppServerClientOptions(*runtime)
	if err != nil {
		fail(err)
		return
	}
	client, err := codexappserver.Dial(ctx, runtime.SocketPath, clientOptions)
	if err != nil {
		fail(err)
		return
	}
	thread, err := client.ReadThread(ctx, codexappserver.ThreadReadParams{ThreadID: threadID})
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
	launchAlive := codexAppServerLaunchAlive(*runtime)
	if args.CodexAppServerExistingThread {
		launchAlive = codexAppServerResumedTUIAlive(*runtime, threadID)
	}
	if !launchAlive {
		_ = client.Close()
		fail(errors.New("validated Codex TUI binding does not belong to the recorded live launch/pane"))
		return
	}
	if err := reconcileCodexNativePermissionRegistryForGeneration(runtime.Generation); err != nil {
		_ = client.Close()
		fail(fmt.Errorf("validate native Codex permission registry before ready: %w", err))
		return
	}
	runtime.State = db.CodexAppServerReady
	runtime.Detail = ""
	completed, err := db.CompleteCodexAppServerRuntimeBootstrap(*runtime)
	if err != nil {
		_ = client.Close()
		fail(fmt.Errorf("persist verified thread binding: %w", err))
		return
	}
	if !completed {
		_ = client.Close()
		slog.Debug("Codex app-server bootstrap lost its warming claim before ready",
			"generation", runtime.Generation)
		return
	}
	handle := registerCodexAppServerHandle(*runtime, client)
	projectCodexAppServerRawStatus(handle, thread.Status, time.Now().UTC(), "app-server snapshot")
	if err := reconcileCodexNativePermissionRegistryForGeneration(runtime.Generation); err != nil {
		slog.Warn("prune superseded Codex native permission profiles after ready",
			"generation", runtime.Generation, "error", err)
	}
	go watchCodexAppServerHandle(handle)
}

// startCodexAppServerRecovery re-adopts pane-owned servers after agentd
// restart. The TUI and its thread already exist, so this path performs only
// identity/liveness reads; it never resumes a thread or submits a prompt.
func startCodexAppServerRecovery(stop <-chan struct{}) {
	go func() {
		runCodexAppServerRecoverySweep()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				runCodexAppServerRecoverySweep()
			}
		}
	}()
}

func runCodexAppServerRecoverySweep() {
	runtimes, err := db.RecoverableCodexAppServerRuntimes()
	if err != nil {
		slog.Warn("list Codex app-server recovery candidates", "error", err)
		return
	}
	for i := range runtimes {
		runtime := runtimes[i]
		incomplete := runtime.ThreadID == "" || runtime.ConvID == "" || runtime.CodexVersion == ""
		if incomplete && runtime.State == db.CodexAppServerWarming &&
			time.Since(runtime.CreatedAt) >= codexAppServerStartupTimeout {
			claimed, claimErr := db.ClaimExpiredUnboundCodexAppServerRuntimeRecovery(
				runtime.Generation, codexAppServerRecoveryOwner, time.Now().UTC(), codexAppServerStartupTimeout)
			if claimErr != nil {
				slog.Warn("claim unbound Codex app-server recovery", "generation", runtime.Generation, "error", claimErr)
				continue
			}
			if claimed {
				detail := "daemon restart recovery did not receive a validated Codex TUI binding before the startup deadline"
				changed, failErr := db.FailCodexAppServerRuntimeRecovery(
					runtime.Generation, codexAppServerRecoveryOwner, detail)
				if failErr != nil {
					slog.Warn("expire unbound Codex app-server recovery", "generation", runtime.Generation, "error", failErr)
				} else if changed {
					stopCodexAppServerPaneAfterControlFailure(runtime, detail)
				}
			}
			continue
		}
		if runtime.CodexVersion != "" && (runtime.ThreadID == "" || runtime.ConvID == "") {
			// A restart may land after the pane has started the native server and
			// relay but before the original daemon released the TUI. Re-prove the
			// launch and create the same non-secret barrier; the eventual validated
			// TUI hook then binds the warming row normally.
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			recoveryErr := recoverUnboundCodexAppServerLaunch(ctx, runtime)
			cancel()
			if recoveryErr != nil {
				slog.Debug("Codex app-server pre-bind recovery proof not ready",
					"generation", runtime.Generation, "error", recoveryErr)
			}
			continue
		}
		if incomplete {
			continue
		}
		if codexAppServerReady(runtime.ConvID) {
			continue
		}
		claimed, claimErr := db.ClaimCodexAppServerRuntimeRecovery(
			runtime.Generation, codexAppServerRecoveryOwner, time.Now().UTC(), codexAppServerStartupTimeout)
		if claimErr != nil {
			slog.Warn("claim Codex app-server recovery", "generation", runtime.Generation, "error", claimErr)
			continue
		}
		if claimed {
			go recoverCodexAppServerRuntime(runtime, codexAppServerRecoveryOwner)
		}
	}
}

func recoverUnboundCodexAppServerLaunch(ctx context.Context, runtime db.CodexAppServerRuntime) error {
	if err := codexappserver.CheckVersion(runtime.CodexVersion); err != nil {
		return err
	}
	pid, err := codexAppServerServerPID(ctx, runtime.SocketPath,
		filepath.Join(filepath.Dir(runtime.SocketPath), "server.pid"))
	if err != nil {
		return err
	}
	relayPID, err := codexAppServerRelayPID(ctx, runtime.SocketPath)
	if err != nil {
		return err
	}
	if err := waitForOwnedCodexSocket(ctx, runtime.SocketPath, relayPID); err != nil {
		return err
	}
	if err := waitForCodexAppServerEndpointOwner(ctx, pid, runtime.SocketPath); err != nil {
		return err
	}
	if err := verifyCodexAppServerProcessIdentity(runtime.SocketPath, relayPID); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := recordCodexAppServerProcessIdentity(runtime.SocketPath, relayPID); err != nil {
			return err
		}
	}
	return recordCodexAppServerProof(runtime.SocketPath)
}

func recordCodexAppServerProof(socketPath string) error {
	path := filepath.Join(filepath.Dir(socketPath), codexAppServerProofFile)
	err := writeExclusiveSecret(path, "proved")
	if os.IsExist(err) {
		return nil
	}
	return err
}

func recoverCodexAppServerRuntime(runtime db.CodexAppServerRuntime, owner string) {
	ctx, cancel := context.WithTimeout(context.Background(), codexAppServerStartupTimeout)
	defer cancel()
	fail := func(cause error) {
		changed, err := db.FailCodexAppServerRuntimeRecovery(runtime.Generation, owner, cause.Error())
		if err != nil {
			slog.Warn("record Codex app-server recovery failure", "generation", runtime.Generation, "error", err)
		}
		if changed {
			stopCodexAppServerPaneAfterControlFailure(runtime, cause.Error())
		}
	}
	if err := codexappserver.CheckVersion(runtime.CodexVersion); err != nil {
		fail(fmt.Errorf("recorded Codex version is no longer supported: %w", err))
		return
	}
	pid := runtime.ServerPID
	if pid <= 1 {
		var err error
		pid, err = codexAppServerServerPID(ctx, runtime.SocketPath,
			filepath.Join(filepath.Dir(runtime.SocketPath), "server.pid"))
		if err != nil {
			fail(err)
			return
		}
	}
	relayPID, err := codexAppServerRelayPID(ctx, runtime.SocketPath)
	if err != nil {
		fail(err)
		return
	}
	if err := waitForOwnedCodexSocket(ctx, runtime.SocketPath, relayPID); err != nil {
		fail(err)
		return
	}
	if err := waitForCodexAppServerEndpointOwner(ctx, pid, runtime.SocketPath); err != nil {
		fail(err)
		return
	}
	if err := verifyCodexAppServerProcessIdentity(runtime.SocketPath, relayPID); err != nil {
		fail(fmt.Errorf("re-prove Codex app-server process generation: %w", err))
		return
	}
	if !codexAppServerLaunchAlive(runtime) {
		_ = signalCodexAppServerProcess(pid, syscall.SIGTERM)
		fail(errors.New("recorded Codex TUI launch/pane is no longer alive"))
		return
	}
	clientOptions, err := codexAppServerClientOptions(runtime)
	if err != nil {
		fail(err)
		return
	}
	client, err := codexappserver.Dial(ctx, runtime.SocketPath, clientOptions)
	if err != nil {
		fail(fmt.Errorf("reconnect Codex app-server: %w", err))
		return
	}
	thread, err := client.ReadThread(ctx, codexappserver.ThreadReadParams{ThreadID: runtime.ThreadID})
	if err != nil || thread.ID != runtime.ThreadID {
		_ = client.Close()
		if err == nil {
			err = fmt.Errorf("thread/read returned %q, want %q", thread.ID, runtime.ThreadID)
		}
		fail(fmt.Errorf("re-prove Codex thread identity: %w", err))
		return
	}
	// Revalidate after all asynchronous identity proofs and immediately before
	// the ready CAS. The setup may have been replaced since the sweep claimed
	// this generation; a failure must leave the runtime terminal and unhandled.
	if err := reconcileCodexNativePermissionRegistryForGeneration(runtime.Generation); err != nil {
		_ = client.Close()
		fail(fmt.Errorf("validate native Codex permission registry before recovery ready: %w", err))
		return
	}
	runtime.ServerPID = pid
	runtime.State = db.CodexAppServerReady
	changed, err := db.CompleteCodexAppServerRuntimeRecovery(runtime, owner)
	if err != nil || !changed {
		_ = client.Close()
		if err != nil {
			slog.Warn("complete Codex app-server recovery", "generation", runtime.Generation, "error", err)
		}
		return
	}
	handle := registerCodexAppServerHandle(runtime, client)
	projectCodexAppServerRawStatus(handle, thread.Status, time.Now().UTC(), "app-server daemon reconnect")
	if err := reconcileCodexNativePermissionRegistry(); err != nil {
		slog.Warn("prune superseded Codex native permission profiles after recovery",
			"generation", runtime.Generation, "error", err)
	}
	go watchCodexAppServerHandle(handle)
}

func registerCodexAppServerHandle(runtime db.CodexAppServerRuntime, client *codexappserver.Client) *codexAppServerHandle {
	handle := &codexAppServerHandle{runtime: runtime, client: client}
	codexAppServerHandles.Lock()
	old := codexAppServerHandles.byConv[runtime.ConvID]
	codexAppServerHandles.byConv[runtime.ConvID] = handle
	codexAppServerHandles.byGeneration[runtime.Generation] = handle
	codexAppServerHandles.Unlock()
	if old != nil && old != handle {
		old.mutations.Lock()
		old.closing = true
		_ = old.client.Close()
		old.mutations.Unlock()
	}
	return handle
}

func waitForCodexAppServerTUIBinding(ctx context.Context, generation string) (string, error) {
	for {
		runtime, err := db.GetCodexAppServerRuntime(generation)
		if err != nil {
			return "", err
		}
		if runtime == nil {
			return "", fmt.Errorf("codex app-server runtime %q disappeared", generation)
		}
		if runtime.State == db.CodexAppServerUnavailable || runtime.State == db.CodexAppServerDead {
			return "", fmt.Errorf("codex app-server TUI binding failed: %s", runtime.Detail)
		}
		if strings.TrimSpace(runtime.ThreadID) != "" {
			return runtime.ThreadID, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("wait for TUI-created thread binding: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
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

// codexAppServerProcessIdentity is platform-specific OS evidence containing
// the process start identity and argv. Its implementation also requires argv
// to name this exact generation's Unix socket.
var codexAppServerProcessIdentity = readCodexAppServerProcessIdentity
var signalCodexAppServerProcess = func(pid int, signal syscall.Signal) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(signal)
}

func recordCodexAppServerProcessIdentity(socketPath string, pid int) error {
	identity, err := codexAppServerProcessIdentity(pid, socketPath)
	if err != nil {
		return fmt.Errorf("prove Codex app-server process generation: %w", err)
	}
	path := filepath.Join(filepath.Dir(socketPath), codexAppServerIdentityFile)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return verifyCodexAppServerProcessIdentity(socketPath, pid)
	}
	if err != nil {
		return fmt.Errorf("create Codex app-server process identity: %w", err)
	}
	if _, err = file.WriteString(identity + "\n"); err != nil {
		_ = file.Close()
		return fmt.Errorf("write Codex app-server process identity: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close Codex app-server process identity: %w", err)
	}
	return nil
}

func verifyCodexAppServerProcessIdentity(socketPath string, pid int) error {
	path := filepath.Join(filepath.Dir(socketPath), codexAppServerIdentityFile)
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("read recorded Codex app-server process identity: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Mode().Perm() != 0o600 || int(stat.Uid) != os.Getuid() {
		return errors.New("recorded Codex app-server process identity is not an owned plain 0600 file")
	}
	recorded, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	current, err := codexAppServerProcessIdentity(pid, socketPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(recorded)) != current {
		return errors.New("codex app-server PID/start/argv identity no longer matches its recorded generation")
	}
	return nil
}

func verifyCodexAppServerRuntimeProcesses(runtime db.CodexAppServerRuntime) (int, error) {
	endpoint, err := codexAppServerEndpoint(runtime.SocketPath)
	if err != nil || !codexAppServerProcessOwnsEndpoint(runtime.ServerPID, endpoint) {
		return 0, errors.New("codex app-server PID does not own its recorded loopback listener")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	relayPID, err := codexAppServerRelayPID(ctx, runtime.SocketPath)
	if err != nil {
		return 0, err
	}
	if err := verifyCodexAppServerProcessIdentity(runtime.SocketPath, relayPID); err != nil {
		return 0, err
	}
	return relayPID, nil
}

func liveCodexAppServerLaunch(runtime db.CodexAppServerRuntime) bool {
	row, err := db.LoadSession(runtime.LaunchID)
	if err != nil || row == nil || row.ID != runtime.LaunchID || row.ConvID != runtime.ConvID ||
		row.Harness != harness.CodexName || strings.TrimSpace(row.TmuxSession) == "" ||
		row.CreatedAt.Before(runtime.CreatedAt) {
		return false
	}
	return session.IsTmuxSessionAlive(row.TmuxSession)
}

var codexAppServerLaunchAlive = liveCodexAppServerLaunch

// codexAppServerResumedTUIAlive proves that the live pane belonging to this
// exact runtime is executing `codex resume <expected thread>`. This is the
// hookless resume barrier: the app-server socket/PID/generation were proved
// immediately before it, and ReadThread proves the same immutable id after
// Dial. Fresh launches never use this path because an early Dial would join
// Codex's fresh-thread subscriber set and steal TUI-only requests.
var codexAppServerResumedTUIAlive = liveCodexAppServerResumedTUI

// waitForCodexAppServerResumedTUI bridges the deliberate ordering gap in the
// pane launch: its private app-server is started first, and only after that
// socket becomes ready does the shell exec `codex resume ... --remote ...`.
// Socket/PID readiness therefore cannot imply that the TUI process is already
// visible below the pane. Keep polling the same exact launch/pane/argv proof;
// the shared bootstrap deadline remains the bound and no weaker identity is
// accepted while the TUI is coming up.
func waitForCodexAppServerResumedTUI(
	ctx context.Context, runtime db.CodexAppServerRuntime, expectedThread string,
) error {
	for {
		current, err := db.GetCodexAppServerRuntime(runtime.Generation)
		if err != nil {
			return fmt.Errorf("read Codex app-server bootstrap claim: %w", err)
		}
		if current == nil || current.State != db.CodexAppServerWarming {
			return errors.New("codex app-server bootstrap no longer owns the warming generation")
		}
		if codexAppServerResumedTUIAlive(runtime, expectedThread) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func liveCodexAppServerResumedTUI(runtime db.CodexAppServerRuntime, expectedThread string) bool {
	if !liveCodexAppServerLaunch(runtime) || runtime.ConvID != expectedThread {
		return false
	}
	row, err := db.LoadSession(runtime.LaunchID)
	if err != nil || row == nil {
		return false
	}
	lines, ok := session.ProcessTreeCommandLines(livePanePID(row.TmuxSession))
	if !ok {
		return false
	}
	for _, line := range lines {
		if exactCodexResumeCommandLine(line, expectedThread) {
			return true
		}
	}
	return false
}

func exactCodexResumeCommandLine(line, expectedThread string) bool {
	fields := strings.Fields(line)
	return len(fields) >= 3 && filepath.Base(fields[0]) == "codex" &&
		fields[1] == "resume" && fields[2] == expectedThread
}

func processAlive(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(syscall.Signal(0))
}

func watchCodexAppServerHandle(handle *codexAppServerHandle) {
	state, detail := runCodexAppServerObserver(handle)
	if state == db.CodexAppServerDead && reconnectCodexAppServerHandle(handle) {
		go watchCodexAppServerHandle(handle)
		return
	}
	if changed, err := db.MarkCodexAppServerRuntimeTerminalIfUnreplaced(
		handle.runtime.Generation, state, detail); err != nil {
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
	handle.mutations.Lock()
	closing := handle.closing
	handle.mutations.Unlock()
	// An unexpected interaction request is a deliberate quarantine: the real
	// TUI may still be presenting that approval/input and must remain alive for
	// the human. Transport death or a bounded snapshot hang makes the shared
	// pane unusable and is the deterministic-relaunch case.
	if !closing && (state == db.CodexAppServerDead || strings.Contains(detail, "stopped answering")) {
		stopCodexAppServerPaneAfterControlFailure(handle.runtime, detail)
	}
}

// reconnectCodexAppServerHandle repairs a lost agentd WebSocket while the
// pane-owned server and TUI are still the same verified generation. It never
// starts or resumes a thread, so reconnecting cannot alter approval ownership
// or replay input.
func reconnectCodexAppServerHandle(handle *codexAppServerHandle) bool {
	deadline := time.Now().Add(codexAppServerStartupTimeout)
	for time.Now().Before(deadline) {
		handle.mutations.Lock()
		if handle.closing {
			handle.mutations.Unlock()
			return false
		}
		runtime, err := db.GetCodexAppServerRuntime(handle.runtime.Generation)
		if err != nil || runtime == nil || runtime.State != db.CodexAppServerReady ||
			runtime.ConvID != handle.runtime.ConvID || runtime.ThreadID != handle.runtime.ThreadID ||
			runtime.ServerPID != handle.runtime.ServerPID {
			handle.mutations.Unlock()
			return false
		}
		proofCtx, cancelProof := context.WithTimeout(context.Background(), 500*time.Millisecond)
		relayPID, proofErr := verifyCodexAppServerRuntimeProcesses(*runtime)
		if proofErr == nil {
			proofErr = waitForOwnedCodexSocket(proofCtx, runtime.SocketPath, relayPID)
		}
		cancelProof()
		if proofErr == nil && !codexAppServerLaunchAlive(*runtime) {
			proofErr = errors.New("recorded Codex TUI launch/pane is no longer alive")
		}
		if proofErr != nil {
			handle.mutations.Unlock()
			return false
		}
		clientOptions, optionsErr := codexAppServerClientOptions(*runtime)
		if optionsErr != nil {
			handle.mutations.Unlock()
			return false
		}
		ctx, cancel := context.WithTimeout(context.Background(), codexAppServerCallTimeout)
		client, dialErr := codexappserver.Dial(ctx, runtime.SocketPath, clientOptions)
		if dialErr == nil {
			var thread codexappserver.Thread
			thread, dialErr = client.ReadThread(ctx, codexappserver.ThreadReadParams{ThreadID: runtime.ThreadID})
			if dialErr == nil && thread.ID == runtime.ThreadID {
				handle.client = client
				handle.mutations.Unlock()
				cancel()
				projectCodexAppServerRawStatus(handle, thread.Status, time.Now().UTC(), "app-server reconnect")
				return true
			}
			_ = client.Close()
		}
		handle.mutations.Unlock()
		cancel()
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func stopCodexAppServerPaneAfterControlFailure(runtime db.CodexAppServerRuntime, detail string) {
	if !codexAppServerLaunchAlive(runtime) {
		return
	}
	row, err := db.LoadSession(runtime.LaunchID)
	if err != nil || row == nil || row.ConvID != runtime.ConvID || strings.TrimSpace(row.TmuxSession) == "" {
		return
	}
	slog.Error("Codex app-server control could not be recovered; stopping unusable pane for durable relaunch",
		"conv", runtime.ConvID, "launch", runtime.LaunchID, "tmux", row.TmuxSession, "detail", detail)
	if err := clcommon.TmuxCommand("kill-session", "-t", clcommon.ExactTarget(row.TmuxSession)).Run(); err != nil {
		slog.Warn("stop unrecoverable Codex app-server pane", "conv", runtime.ConvID, "error", err)
	}
}

func stopCodexAppServerRuntimeForConv(convID string) {
	stopCodexAppServerRuntime(convID, "")
}

func stopCodexAppServerRuntime(convID, launchID string) {
	var runtime *db.CodexAppServerRuntime
	var err error
	if launchID != "" {
		runtime, err = db.GetCodexAppServerRuntimeByLaunchID(launchID)
	} else if convID != "" {
		runtime, err = db.GetCodexAppServerRuntimeByConvID(convID)
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
		handle.mutations.Lock()
		handle.closing = true
		_ = handle.client.Close()
		handle.mutations.Unlock()
	}
	liveClaim := runtime.State == db.CodexAppServerWarming || runtime.State == db.CodexAppServerRecovering ||
		runtime.State == db.CodexAppServerReady
	// Terminal rows retain diagnostics, including the old numeric PID. Never
	// signal that value: after process exit it may have been recycled. A live
	// claim is signalled only when its recorded OS start/argv identity still
	// proves this exact generation and socket.
	if liveClaim && runtime.ServerPID > 1 {
		_, proofErr := verifyCodexAppServerRuntimeProcesses(*runtime)
		if proofErr == nil {
			_ = signalCodexAppServerProcess(runtime.ServerPID, syscall.SIGTERM)
		}
	}
	runtime.State = db.CodexAppServerDead
	runtime.Detail = "launch exited"
	_ = db.UpsertCodexAppServerRuntime(*runtime)
	removeCodexAppServerGeneration(runtime.SocketPath)
}

func stopFailedCodexAppServerLaunch(convID, launchID, tmuxSession string) {
	var generation string
	if launchID != "" {
		if runtime, _ := db.GetCodexAppServerRuntimeByLaunchID(launchID); runtime != nil {
			generation = runtime.Generation
		}
	} else if convID != "" {
		if runtime, _ := db.GetCodexAppServerRuntimeByConvID(convID); runtime != nil {
			generation = runtime.Generation
		}
	}
	stopCodexAppServerRuntime(convID, launchID)
	if generation != "" {
		if err := session.UnregisterCodexNativePermissionProfile(generation); err != nil {
			slog.Warn("roll back failed Codex native permission profile",
				"generation", generation, "error", err)
		}
	}
	if strings.TrimSpace(tmuxSession) == "" {
		return
	}
	if err := clcommon.TmuxCommand("kill-session", "-t", clcommon.ExactTarget(tmuxSession)).Run(); err != nil {
		slog.Warn("stop failed Codex app-server launch", "session", tmuxSession, "error", err)
	}
}

func removeCodexAppServerGeneration(socketPath string) {
	dir := filepath.Dir(socketPath)
	generation := filepath.Base(dir)
	if filepath.Base(socketPath) != "app.sock" || filepath.Base(filepath.Dir(filepath.Dir(dir))) != "codex" {
		return
	}
	_ = os.Remove(socketPath)
	_ = os.Remove(filepath.Join(dir, "server.pid"))
	_ = os.Remove(filepath.Join(dir, "server.pid.relay"))
	_ = os.Remove(filepath.Join(dir, codexAppServerEndpointFile))
	_ = os.Remove(filepath.Join(dir, codexAppServerTokenHandoffFile))
	_ = os.Remove(filepath.Join(dir, codexAppServerProofFile))
	_ = os.Remove(filepath.Join(dir, codexAppServerIdentityFile))
	// Keep a non-empty server.log for diagnostics; remove an empty one so the
	// generation can disappear cleanly.
	if info, err := os.Stat(filepath.Join(dir, "server.log")); err == nil && info.Size() == 0 {
		_ = os.Remove(filepath.Join(dir, "server.log"))
	}
	_ = os.Remove(dir)
	_ = db.DeleteCodexAppServerCapability(generation)
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
