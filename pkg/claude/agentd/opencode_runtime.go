package agentd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/opencodeapi"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

const (
	openCodeServerUsername    = opencodeapi.ServerUsername
	openCodeStartupTimeout    = 12 * time.Second
	openCodeHealthAttempts    = 3
	openCodeHealthRetryDelay  = 250 * time.Millisecond
	openCodeProcessStopWait   = 2 * time.Second
	openCodeEndpointCloseWait = 2 * time.Second
	openCodeSSERetryDelay     = time.Second
	openCodeMaxSSEEventBytes  = 4 << 20
	openCodeHookRowWait       = 2 * time.Second
	openCodeHookRowRetryDelay = 25 * time.Millisecond
	openCodeSandboxSpecMax    = 4 << 20
)

type openCodeLaunch struct {
	SessionID           string
	ConvID              string
	ServerURL           string
	Password            string
	PID                 int
	Transport           string
	ControlSocketPath   string
	ControlSocketDevice int64
	ControlSocketInode  int64
}

type openCodeLaunchAuthority struct {
	Executable string
	Launcher   string
}

type openCodeUnixLaunchHandshake struct {
	authority   *os.File
	acknowledge *os.File
}

type openCodeTUICommand string

const (
	openCodeTUICompact openCodeTUICommand = "session.compact"
	openCodeTUIExit    openCodeTUICommand = "app.exit"
)

type openCodeProcess struct {
	cmd           *exec.Cmd
	pid           int
	tmuxSession   string
	done          chan error
	doneOnce      sync.Once
	groupKillOnce sync.Once
	cancel        context.CancelFunc
	sseDone       chan struct{}
	convID        string
	// exited is set (under openCodeProcesses' lock) once cmd.Wait returns, so a
	// consumer that had not yet registered its cancel at death time is never
	// started against an already-dead server. Only processes with a cmd.Wait
	// watcher set it; synthetic reuse entries (ensureOpenCodeSSE's placeholder)
	// leave it false and rely on the reaper, exactly as before.
	exited   bool
	stopping bool
}

var beforeOpenCodeTUICommandStatusCheckForTest func()
var openCodeConversationStateCleanupDelay = 2 * openCodeSSERetryDelay
var openCodeTmuxProbeInterval = time.Second

var openCodeProcesses = struct {
	sync.Mutex
	bySession map[string]*openCodeProcess
}{bySession: map[string]*openCodeProcess{}}

// Delivery and the reaper may discover the same unhealthy managed server at
// once. Serialize stop -> endpoint release -> restart per session so those
// recovery attempts cannot tear down or contend for the same replacement.
var openCodeReconcileLocks sync.Map // map[sessionID]*sync.Mutex

// A projector that outlives the bounded stop join must finish its current
// side effect before a replacement projector applies newer state. Generation
// checks then discard the old continuation, while the replacement runs last.
var (
	openCodeProjectorApplyLocks   = map[string]*openCodeProjectorApplyLock{}
	openCodeProjectorApplyLocksMu sync.Mutex // guards the map, not the per-key tokens
)

var errOpenCodeResourceCgroup = errors.New("OpenCode resource cgroup placement failed")

type openCodeProjectorApplyLock struct {
	token chan struct{}
	users int
}

func newOpenCodeProjectorApplyLock() *openCodeProjectorApplyLock {
	lock := &openCodeProjectorApplyLock{token: make(chan struct{}, 1)}
	lock.token <- struct{}{}
	return lock
}

type openCodeSSEGenerationKey struct{}

var openCodeHTTPClient = &http.Client{Timeout: 5 * time.Second}

// openCodeServeLogLevel is the --log-level every managed `opencode serve`
// runs with. Real-OpenCode smokes raise it so a failure leaves the server's
// own account of where it stalled in its log directory.
var openCodeServeLogLevel = "ERROR"
var openCodeHealthHTTPClient = &http.Client{Timeout: time.Second}

// openCodeSSEHTTPClient is the bounded client for the long-lived /event stream.
// It must NOT carry a whole-request Timeout — that would sever a healthy stream
// after the deadline — so it bounds only the setup phase: connection dial and
// the wait for response headers. Once headers arrive the body is read until the
// server closes it or the request context is cancelled (server death, reconcile,
// or shutdown), which already interrupts the in-flight read.
var openCodeSSEHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		ResponseHeaderTimeout: 10 * time.Second,
	},
}

// These seams keep flow tests independent of a locally-installed OpenCode
// binary while still exercising executeSpawn's production orchestration.
var (
	startOpenCodeRuntimeForSpawn = startOpenCodeRuntime
	sendOpenCodePromptForSpawn   = sendOpenCodePrompt
	// Engine-aware, and it has to be: the proxy engine's floor is the isolated
	// posture's construction, so probing the filtered posture's prerequisites
	// would refuse an OpenCode proxy launch for want of pasta and nft that it
	// never calls. session.ResolveTclaudeLayerServerForEngine applies the same
	// TclaudeLayerFloorPosture mapping the plain-CLI launch path already uses,
	// rather than a second answer to the same question (TCL-891).
	resolveOpenCodeTclaudeLayer              = session.ResolveTclaudeLayerServerForEngine
	wrapOpenCodeTclaudeLayer                 = session.WrapTclaudeLayerServerSpec
	wrapOpenCodeTclaudeLayerWithLoopbackBind = session.WrapTclaudeLayerServerSpecWithLoopbackBind
	openCodeRelayExecutable                  = os.Executable
	removeOpenCodeResourceCgroup             = session.RemoveResourceCgroup
)

func startOpenCodeRuntime(
	sessionID, cwd, title, resumeID, permissionJSON, sandboxImplementation string,
	sandboxSpec *session.TclaudeLayerLaunchSpec,
	resourceCgroupDir string,
) (*openCodeLaunch, error) {
	in, err := prepareOpenCodeRuntimeLaunch(cwd, permissionJSON, sandboxImplementation, sandboxSpec)
	if err != nil {
		return nil, err
	}
	reusedResourceCgroup := false
	keepReusedResourceCgroup := false
	defer func() {
		if cleanupErr := cleanupAbandonedOpenCodeReplacementCgroup(
			reusedResourceCgroup, keepReusedResourceCgroup, resourceCgroupDir,
		); cleanupErr != nil {
			slog.Warn("remove abandoned OpenCode replacement resource cgroup",
				"session", sessionID, "dir", resourceCgroupDir, "error", cleanupErr)
		}
	}()
	reused, reusesCgroup, err := reuseOrRetireOpenCodeRuntime(sessionID, resourceCgroupDir, in)
	if err != nil {
		return nil, err
	}
	if reused != nil {
		return reused, nil
	}
	reusedResourceCgroup = reusesCgroup
	if err := retirePriorOpenCodeResumeRuntime(sessionID, resumeID); err != nil {
		return nil, err
	}
	if in.authority.Executable == "" {
		in.authority, err = resolveOpenCodeLaunchAuthority(sandboxSpec)
		if err != nil {
			return nil, err
		}
	}
	executionBoundaryJSON, boundaryErr := buildOpenCodeExecutionBoundary(
		in.sandboxImplementation, sandboxSpec, in.cwd, in.authority)
	if boundaryErr != nil {
		slog.Warn("could not record OpenCode server execution boundary",
			"session_id", sessionID, "error", boundaryErr)
		executionBoundaryJSON = ""
	}
	launch, err := launchOpenCodeServer(sessionID, title, resumeID, resourceCgroupDir,
		executionBoundaryJSON, in, sandboxSpec)
	if err != nil {
		return nil, err
	}
	keepReusedResourceCgroup = true
	return launch, nil
}

// openCodeRuntimeLaunchInputs is what every later launch step reads: the
// validated permission policy, the resolved cwd, the frozen launch authority,
// and the sandbox record and control transport to persist.
type openCodeRuntimeLaunchInputs struct {
	permissionJSON, cwd                    string
	authority                              openCodeLaunchAuthority
	sandboxImplementation, sandboxSpecJSON string
	transport, controlPath                 string
}

// prepareOpenCodeRuntimeLaunch validates the permission policy, resolves the
// cwd, freezes a sandboxed launch's executable authority, and serializes the
// sandbox record and transport the runtime row will carry.
func prepareOpenCodeRuntimeLaunch(
	cwd, permissionJSON, sandboxImplementation string,
	sandboxSpec *session.TclaudeLayerLaunchSpec,
) (openCodeRuntimeLaunchInputs, error) {
	permissionJSON = strings.TrimSpace(permissionJSON)
	if permissionJSON == "" {
		return openCodeRuntimeLaunchInputs{}, fmt.Errorf("OpenCode permission policy is required")
	}
	if _, err := decodeOpenCodePermissionRules(permissionJSON); err != nil {
		return openCodeRuntimeLaunchInputs{}, err
	}
	var err error
	cwd, err = resolveOpenCodeLaunchCwd(cwd)
	if err != nil {
		return openCodeRuntimeLaunchInputs{}, err
	}
	// Freeze the executable bind before serializing the renderer input. The
	// managed server, rather than its attach-only pane, is OpenCode's actual
	// tool-executing boundary.
	var authority openCodeLaunchAuthority
	if sandboxSpec != nil {
		authority, err = resolveOpenCodeLaunchAuthority(sandboxSpec)
		if err != nil {
			return openCodeRuntimeLaunchInputs{}, err
		}
	}
	sandboxSpecJSON := ""
	sandboxImplementation, sandboxSpecJSON, err = openCodeSandboxRecord(
		sandboxImplementation, sandboxSpec)
	if err != nil {
		return openCodeRuntimeLaunchInputs{}, err
	}
	transport, controlPath, err := openCodeRuntimeTransportForSpec(sandboxSpec)
	if err != nil {
		return openCodeRuntimeLaunchInputs{}, err
	}
	return openCodeRuntimeLaunchInputs{
		permissionJSON: permissionJSON, cwd: cwd, authority: authority,
		sandboxImplementation: sandboxImplementation, sandboxSpecJSON: sandboxSpecJSON,
		transport: transport, controlPath: controlPath,
	}, nil
}

// reuseOrRetireOpenCodeRuntime handles the session's existing runtime row, if
// any. It refuses an invalid persisted runtime, reuses a healthy server for
// the same cwd and sandbox, refuses to replace a live Unix runtime it cannot
// prove it owns, and otherwise retires the old server. reusedCgroup reports
// that the retired server's resource cgroup is kept for the replacement.
func reuseOrRetireOpenCodeRuntime(
	sessionID, resourceCgroupDir string, in openCodeRuntimeLaunchInputs,
) (reused *openCodeLaunch, reusedCgroup bool, err error) {
	existing, err := db.GetOpenCodeRuntime(sessionID)
	if err != nil {
		return nil, false, fmt.Errorf("look up OpenCode runtime: %w", err)
	}
	if existing != nil {
		if _, validationErr := openCodeRuntimeSandboxSpec(*existing); validationErr != nil {
			return nil, false, fmt.Errorf(
				"refuse invalid persisted OpenCode runtime transport: %w", validationErr)
		}
		// OpenCode's permission paths and API instance are both rooted in cwd.
		// Never reuse a healthy endpoint for a different directory identity:
		// patching a policy compiled for cwd B through cwd A would be ambiguous
		// and could silently target the wrong session instance.
		sameCwd := strings.TrimSpace(existing.Cwd) != "" && existing.Cwd == in.cwd
		existingImplementation, implementationErr := normalizeOpenCodeRuntimeSandboxImplementation(
			existing.SandboxImplementation)
		sameSandbox := implementationErr == nil &&
			string(existingImplementation) == in.sandboxImplementation &&
			existing.SandboxLaunchSpecJSON == in.sandboxSpecJSON &&
			existing.ResourceCgroupDir == resourceCgroupDir &&
			existing.Transport == in.transport &&
			existing.ControlSocketPath == in.controlPath
		if sameCwd && sameSandbox && openCodeHealthyAfterRetries(*existing,
			openCodeHealthAttempts, openCodeHealthRetryDelay) {
			if existing.PermissionJSON != in.permissionJSON {
				existing.PermissionJSON = in.permissionJSON
				if err := db.UpsertOpenCodeRuntime(*existing); err != nil {
					return nil, false, fmt.Errorf("persist refreshed OpenCode permission policy: %w", err)
				}
			}
			if err := ensureOpenCodeSessionPermission(*existing); err != nil {
				return nil, false, fmt.Errorf("verify OpenCode session permission: %w", err)
			}
			ensureOpenCodeSSE(*existing)
			return openCodeLaunchFromRuntime(*existing), false, nil
		}
		if !openCodeRuntimeSafeToReplace(*existing) {
			return nil, false, fmt.Errorf(
				"refusing to replace live OpenCode Unix runtime without socket ownership proof")
		}
		reusesResourceCgroup := openCodeReplacementReusesResourceCgroup(
			existing.ResourceCgroupDir, resourceCgroupDir)
		var stopErr error
		if reusesResourceCgroup {
			stopErr = stopOpenCodeRuntimeForReplacement(sessionID)
		} else {
			stopErr = stopOpenCodeRuntime(sessionID)
		}
		if stopErr != nil {
			return nil, false, fmt.Errorf("retire prior OpenCode runtime: %w", stopErr)
		}
		return nil, reusesResourceCgroup, nil
	}
	return nil, false, nil
}

// retirePriorOpenCodeResumeRuntime retires the label-keyed runtime of the
// conversation a resume is about to serve.
//
// A fresh launch is keyed by its temporary tclaude session label because
// the server has not minted the conversation ID yet. A later resume is
// keyed by the durable `ses_…` ID. Retire the old label-keyed runtime
// before starting the resume server so an immediate stop→resume never has
// two authoritative servers for the same conversation.
func retirePriorOpenCodeResumeRuntime(sessionID, resumeID string) error {
	if resumeID != "" {
		if prior, err := db.GetOpenCodeRuntimeByConvID(resumeID); err != nil {
			return fmt.Errorf("look up prior OpenCode runtime: %w", err)
		} else if prior != nil && prior.SessionID != sessionID {
			if err := stopOpenCodeRuntime(prior.SessionID); err != nil {
				return fmt.Errorf("retire prior OpenCode runtime: %w", err)
			}
		}
	}
	return nil
}

// launchOpenCodeServer starts the server on a fresh loopback port, retrying
// up to three ports, then persists the runtime and mints or re-permits its
// conversation. A resource-cgroup placement failure is not retried.
func launchOpenCodeServer(
	sessionID, title, resumeID, resourceCgroupDir, executionBoundaryJSON string,
	in openCodeRuntimeLaunchInputs, sandboxSpec *session.TclaudeLayerLaunchSpec,
) (*openCodeLaunch, error) {
	password, err := randomOpenCodePassword()
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		serverURL, err := allocateOpenCodeServerURL()
		if err != nil {
			return nil, err
		}
		runtime := db.OpenCodeRuntime{
			SessionID:             sessionID,
			ConvID:                resumeID,
			ServerURL:             serverURL,
			Password:              password,
			Cwd:                   in.cwd,
			PermissionJSON:        in.permissionJSON,
			SandboxImplementation: in.sandboxImplementation,
			SandboxLaunchSpecJSON: in.sandboxSpecJSON,
			ExecutionBoundaryJSON: executionBoundaryJSON,
			Transport:             in.transport,
			ControlSocketPath:     in.controlPath,
			ResourceCgroupDir:     resourceCgroupDir,
		}
		process, err := startOpenCodeProcessWithAuthority(&runtime, sandboxSpec, in.authority)
		if err != nil {
			if errors.Is(err, errOpenCodeResourceCgroup) {
				return nil, err
			}
			lastErr = err
			continue
		}
		runtime.PID = process.rootPID()
		if err := db.UpsertOpenCodeRuntime(runtime); err != nil {
			stopOpenCodeProcess(runtime, process)
			return nil, fmt.Errorf("persist OpenCode runtime: %w", err)
		}
		if runtime.ConvID == "" {
			runtime.ConvID, err = createOpenCodeSession(runtime, title)
			if err != nil {
				_ = stopOpenCodeRuntime(sessionID)
				return nil, err
			}
			if err := db.UpsertOpenCodeRuntime(runtime); err != nil {
				_ = stopOpenCodeRuntime(sessionID)
				return nil, fmt.Errorf("persist OpenCode conversation id: %w", err)
			}
		} else if err := ensureOpenCodeSessionPermission(runtime); err != nil {
			_ = stopOpenCodeRuntime(sessionID)
			return nil, fmt.Errorf("reapply OpenCode session permission: %w", err)
		}
		ensureOpenCodeSSE(runtime)
		return openCodeLaunchFromRuntime(runtime), nil
	}
	return nil, fmt.Errorf("start OpenCode server after 3 port attempts: %w", lastErr)
}

func openCodeReplacementReusesResourceCgroup(existing, requested string) bool {
	return existing != "" && existing == requested
}

func cleanupAbandonedOpenCodeReplacementCgroup(reused, retained bool, dir string) error {
	if !reused || retained {
		return nil
	}
	return removeOpenCodeResourceCgroup(dir)
}

func openCodeRuntimeTransportForSpec(
	spec *session.TclaudeLayerLaunchSpec,
) (string, string, error) {
	if spec == nil || spec.Version != session.TclaudeLayerUnixRelaySpecVersion {
		return db.OpenCodeTransportLoopbackTCP, "", nil
	}
	if spec.Contract.OpenCodeControl == nil ||
		spec.Contract.OpenCodeControl.Transport != session.TclaudeLayerUnixRelayTransport ||
		strings.TrimSpace(spec.Contract.OpenCodeControl.SocketPath) == "" {
		return "", "", fmt.Errorf("OpenCode with tclaude’s sandbox v4 has incomplete Unix control authority")
	}
	return db.OpenCodeTransportUnixRelay, spec.Contract.OpenCodeControl.SocketPath, nil
}

func openCodeLaunchFromRuntime(runtime db.OpenCodeRuntime) *openCodeLaunch {
	return &openCodeLaunch{
		SessionID: runtime.SessionID, ConvID: runtime.ConvID,
		ServerURL: runtime.ServerURL, Password: runtime.Password, PID: runtime.PID,
		Transport: runtime.Transport, ControlSocketPath: runtime.ControlSocketPath,
		ControlSocketDevice: runtime.ControlSocketDevice,
		ControlSocketInode:  runtime.ControlSocketInode,
	}
}

func resolveOpenCodeLaunchAuthority(
	spec *session.TclaudeLayerLaunchSpec,
) (openCodeLaunchAuthority, error) {
	executable, err := harness.OpenCodeExecutable()
	if err != nil {
		return openCodeLaunchAuthority{}, fmt.Errorf("find OpenCode executable: %w", err)
	}
	if spec != nil {
		executable, err = exposeOpenCodeExecutable(spec, executable)
		if err != nil {
			return openCodeLaunchAuthority{}, err
		}
	}
	launcher, err := resolveOpenCodeLayerLauncher(spec)
	if err != nil {
		return openCodeLaunchAuthority{}, err
	}
	return openCodeLaunchAuthority{Executable: executable, Launcher: launcher}, nil
}

// readyOpenCodeRuntime returns the healthy managed server for convID,
// reconciling it once when necessary. All server-side delivery channels share
// this fail-closed recovery path and never fall back to typing content or
// commands into the attach pane.
func readyOpenCodeRuntime(convID string) (*db.OpenCodeRuntime, error) {
	runtime, err := db.GetOpenCodeRuntimeByConvID(convID)
	if err != nil {
		return nil, fmt.Errorf("look up OpenCode runtime for delivery: %w", err)
	}
	if runtime == nil {
		return nil, fmt.Errorf("no managed OpenCode runtime for conversation %s", convID)
	}
	if !openCodeHealthyAfterRetries(*runtime,
		openCodeHealthAttempts, openCodeHealthRetryDelay) {
		if !reconcileOpenCodeRuntime(runtime.SessionID) {
			return nil, fmt.Errorf("managed OpenCode runtime for conversation %s is unavailable", convID)
		}
		// Reconciliation can restart the server and persist a new PID. Reload
		// before constructing the authenticated request so ownership validation
		// uses the recovered process rather than stale runtime state.
		runtime, err = db.GetOpenCodeRuntimeByConvID(convID)
		if err != nil {
			return nil, fmt.Errorf("reload reconciled OpenCode runtime for delivery: %w", err)
		}
		if runtime == nil {
			return nil, fmt.Errorf("reconciled OpenCode runtime for conversation %s disappeared", convID)
		}
	}
	return runtime, nil
}

// reconcileOpenCodeRuntime is the server-side half of OpenCode liveness.
// A healthy pane is insufficient: the conversation lives in `serve`. Restart
// the same authenticated endpoint when possible so the attached client can
// reconnect; return false when recovery failed and the reaper should fail the
// pane visibly.
var restartOpenCodeProcess = startOpenCodeProcess

func reconcileOpenCodeRuntime(sessionID string) bool {
	value, _ := openCodeReconcileLocks.LoadOrStore(sessionID, &sync.Mutex{})
	reconcileMu := value.(*sync.Mutex)
	reconcileMu.Lock()
	defer reconcileMu.Unlock()

	runtime, err := db.GetOpenCodeRuntime(sessionID)
	if err != nil || runtime == nil {
		return false
	}
	if openCodeHealthyAfterRetries(*runtime,
		openCodeHealthAttempts, openCodeHealthRetryDelay) {
		if err := ensureOpenCodeSessionPermission(*runtime); err != nil {
			slog.Error("OpenCode session permission verification failed",
				"session", sessionID, "error", err)
			return false
		}
		ensureOpenCodeSSE(*runtime)
		return true
	}
	sandboxSpec, err := openCodeRuntimeSandboxSpec(*runtime)
	if err != nil {
		slog.Error("OpenCode server restart refused: sandbox cannot be reproduced",
			"session", sessionID, "error", err)
		return false
	}
	if !openCodeRuntimeSafeToReplace(*runtime) {
		slog.Error("OpenCode server restart refused: live Unix runtime ownership unproven",
			"session", sessionID)
		return false
	}
	stopOpenCodeProcess(*runtime, nil)
	if !waitForOpenCodeRuntimeRelease(*runtime, openCodeEndpointCloseWait) {
		slog.Error("OpenCode server endpoint remained occupied after stop",
			"session", sessionID, "endpoint", runtime.ServerURL)
		return false
	}
	process, err := restartOpenCodeProcess(runtime, sandboxSpec)
	if err != nil {
		slog.Error("OpenCode server restart failed", "session", sessionID, "error", err)
		return false
	}
	runtime.PID = process.rootPID()
	if err := db.UpsertOpenCodeRuntime(*runtime); err != nil {
		slog.Error("OpenCode server restart state could not be persisted",
			"session", sessionID, "error", err)
		stopOpenCodeProcess(*runtime, process)
		return false
	}
	if err := ensureOpenCodeSessionPermission(*runtime); err != nil {
		slog.Error("OpenCode session permission reapply failed",
			"session", sessionID, "error", err)
		stopOpenCodeProcess(*runtime, process)
		return false
	}
	ensureOpenCodeSSE(*runtime)
	return true
}

func openCodeRuntimeSafeToReplace(runtime db.OpenCodeRuntime) bool {
	if runtime.Transport != db.OpenCodeTransportUnixRelay ||
		!session.IsProcessAlive(runtime.PID) {
		return true
	}
	openCodeProcesses.Lock()
	known := openCodeProcesses.bySession[runtime.SessionID]
	openCodeProcesses.Unlock()
	if known != nil && known.rootPID() == runtime.PID {
		return true
	}
	return opencodeapi.RuntimeOwnsEndpoint(runtime)
}

func reapOrphanedOpenCodeRuntimes(states []*session.SessionState) {
	known := make(map[string]bool, len(states))
	for _, state := range states {
		if state.Harness == harness.OpenCodeName && state.Status != session.StatusExited {
			known[state.ID] = true
		}
	}
	runtimes, err := db.ListOpenCodeRuntimes()
	if err != nil {
		slog.Warn("OpenCode runtime orphan scan failed", "error", err)
		return
	}
	for _, runtime := range runtimes {
		if !known[runtime.SessionID] {
			if !runtime.CreatedAt.IsZero() &&
				time.Since(runtime.CreatedAt) < sessionReaperGracePeriod {
				continue
			}
			if err := stopOpenCodeRuntime(runtime.SessionID); err != nil {
				slog.Warn("OpenCode orphan runtime cleanup failed",
					"session", runtime.SessionID, "error", err)
			}
		}
	}
}
