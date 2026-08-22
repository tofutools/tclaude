package agentd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/opencodeapi"
	"github.com/tofutools/tclaude/pkg/claude/session"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
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
	SessionID             string
	ConvID                string
	ServerURL             string
	Password              string
	PID                   int
	Transport             string
	ControlSocketPath     string
	ControlSocketDevice   int64
	ControlSocketInode    int64
	ExecutionBoundaryJSON string
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
	cmd         *exec.Cmd
	pid         int
	tmuxSession string
	done        chan error
	doneOnce    sync.Once
	cancel      context.CancelFunc
	sseDone     chan struct{}
	convID      string
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
	permissionJSON = strings.TrimSpace(permissionJSON)
	if permissionJSON == "" {
		return nil, fmt.Errorf("OpenCode permission policy is required")
	}
	if _, err := decodeOpenCodePermissionRules(permissionJSON); err != nil {
		return nil, err
	}
	var err error
	cwd, err = resolveOpenCodeLaunchCwd(cwd)
	if err != nil {
		return nil, err
	}
	// Freeze the executable bind before serializing the renderer input. The
	// managed server, rather than its attach-only pane, is OpenCode's actual
	// tool-executing boundary.
	if sandboxSpec != nil {
		executable, executableErr := harness.OpenCodeExecutable()
		if executableErr != nil {
			return nil, fmt.Errorf("find OpenCode executable: %w", executableErr)
		}
		if _, executableErr = exposeOpenCodeExecutable(sandboxSpec, executable); executableErr != nil {
			return nil, executableErr
		}
	}
	sandboxSpecJSON := ""
	sandboxImplementation, sandboxSpecJSON, err = openCodeSandboxRecord(
		sandboxImplementation, sandboxSpec)
	if err != nil {
		return nil, err
	}
	executionBoundaryJSON, err := buildOpenCodeExecutionBoundary(
		sandboxImplementation, sandboxSpec, cwd)
	if err != nil {
		slog.Warn("could not record OpenCode server execution boundary",
			"session_id", sessionID, "error", err)
		executionBoundaryJSON = ""
	}
	transport, controlPath, err := openCodeRuntimeTransportForSpec(sandboxSpec)
	if err != nil {
		return nil, err
	}
	existing, err := db.GetOpenCodeRuntime(sessionID)
	if err != nil {
		return nil, fmt.Errorf("look up OpenCode runtime: %w", err)
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
	if existing != nil {
		if _, validationErr := openCodeRuntimeSandboxSpec(*existing); validationErr != nil {
			return nil, fmt.Errorf(
				"refuse invalid persisted OpenCode runtime transport: %w", validationErr)
		}
		// OpenCode's permission paths and API instance are both rooted in cwd.
		// Never reuse a healthy endpoint for a different directory identity:
		// patching a policy compiled for cwd B through cwd A would be ambiguous
		// and could silently target the wrong session instance.
		sameCwd := strings.TrimSpace(existing.Cwd) != "" && existing.Cwd == cwd
		existingImplementation, implementationErr := normalizeOpenCodeRuntimeSandboxImplementation(
			existing.SandboxImplementation)
		sameSandbox := implementationErr == nil &&
			string(existingImplementation) == sandboxImplementation &&
			existing.SandboxLaunchSpecJSON == sandboxSpecJSON &&
			existing.ResourceCgroupDir == resourceCgroupDir &&
			existing.Transport == transport &&
			existing.ControlSocketPath == controlPath
		if sameCwd && sameSandbox && openCodeHealthyAfterRetries(*existing,
			openCodeHealthAttempts, openCodeHealthRetryDelay) {
			if existing.PermissionJSON != permissionJSON {
				existing.PermissionJSON = permissionJSON
				if err := db.UpsertOpenCodeRuntime(*existing); err != nil {
					return nil, fmt.Errorf("persist refreshed OpenCode permission policy: %w", err)
				}
			}
			if err := ensureOpenCodeSessionPermission(*existing); err != nil {
				return nil, fmt.Errorf("verify OpenCode session permission: %w", err)
			}
			ensureOpenCodeSSE(*existing)
			launch := openCodeLaunchFromRuntime(*existing)
			launch.ExecutionBoundaryJSON = executionBoundaryJSON
			return launch, nil
		}
		if !openCodeRuntimeSafeToReplace(*existing) {
			return nil, fmt.Errorf(
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
			return nil, fmt.Errorf("retire prior OpenCode runtime: %w", stopErr)
		}
		reusedResourceCgroup = reusesResourceCgroup
	}
	// A fresh launch is keyed by its temporary tclaude session label because
	// the server has not minted the conversation ID yet. A later resume is
	// keyed by the durable `ses_…` ID. Retire the old label-keyed runtime
	// before starting the resume server so an immediate stop→resume never has
	// two authoritative servers for the same conversation.
	if resumeID != "" {
		if prior, err := db.GetOpenCodeRuntimeByConvID(resumeID); err != nil {
			return nil, fmt.Errorf("look up prior OpenCode runtime: %w", err)
		} else if prior != nil && prior.SessionID != sessionID {
			if err := stopOpenCodeRuntime(prior.SessionID); err != nil {
				return nil, fmt.Errorf("retire prior OpenCode runtime: %w", err)
			}
		}
	}

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
			Cwd:                   cwd,
			PermissionJSON:        permissionJSON,
			SandboxImplementation: sandboxImplementation,
			SandboxLaunchSpecJSON: sandboxSpecJSON,
			Transport:             transport,
			ControlSocketPath:     controlPath,
			ResourceCgroupDir:     resourceCgroupDir,
		}
		process, err := startOpenCodeProcess(&runtime, sandboxSpec)
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
		keepReusedResourceCgroup = true
		launch := openCodeLaunchFromRuntime(runtime)
		launch.ExecutionBoundaryJSON = executionBoundaryJSON
		return launch, nil
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
		return "", "", fmt.Errorf("OpenCode tclaude-layer v4 has incomplete Unix control authority")
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

func buildOpenCodeExecutionBoundary(
	implementation string,
	spec *session.TclaudeLayerLaunchSpec,
	cwd string,
) (string, error) {
	executable, err := harness.OpenCodeExecutable()
	if err != nil {
		return "", err
	}
	launcher := ""
	if spec != nil {
		posture, postureErr := session.TclaudeLayerNetworkPosture(spec.Effective)
		if postureErr != nil {
			return "", postureErr
		}
		root, rootErr := session.TclaudeLayerRootPosture(posture, spec.Effective)
		if rootErr != nil {
			return "", rootErr
		}
		engine, engineErr := session.TclaudeLayerNetworkEngine(spec.Effective)
		if engineErr != nil {
			return "", engineErr
		}
		launcher, _, err = resolveOpenCodeTclaudeLayer(posture, root, engine)
		if err != nil {
			return "", err
		}
	}
	environment := map[string]string{}
	for _, entry := range openCodeServerEnvironment(os.Environ(), spec) {
		name, value, found := strings.Cut(entry, "=")
		if found && name == "PATH" {
			environment[name] = value
		}
	}
	boundary, err := session.BuildExecutionBoundary(session.ExecutionBoundaryInput{
		SandboxImplementation: implementation,
		HarnessName:           harness.OpenCodeName,
		HarnessLookupName:     harness.OpenCodeName,
		HarnessExecutable:     executable,
		LauncherBinary:        launcher,
		Cwd:                   cwd,
		Environment:           environment,
		LayerSpec:             spec,
	})
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(boundary)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func openCodeSandboxRecord(
	rawImplementation string,
	spec *session.TclaudeLayerLaunchSpec,
) (string, string, error) {
	implementation, err := sandboxpolicy.NormalizeImplementation(rawImplementation)
	if err != nil {
		return "", "", err
	}
	if spec == nil {
		switch implementation {
		case sandboxpolicy.ImplementationHarnessBuiltin, sandboxpolicy.ImplementationOff,
			sandboxpolicy.ImplementationResourceOnly:
			return string(implementation), "", nil
		default:
			return "", "", fmt.Errorf(
				"OpenCode sandbox implementation %q requires a launch spec", implementation)
		}
	}
	if implementation != sandboxpolicy.ImplementationTclaudeLayer {
		return "", "", fmt.Errorf(
			"OpenCode sandbox launch spec is incompatible with implementation %q", implementation)
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return "", "", fmt.Errorf("encode OpenCode tclaude-layer launch spec: %w", err)
	}
	return string(sandboxpolicy.ImplementationTclaudeLayer), string(encoded), nil
}

func normalizeOpenCodeRuntimeSandboxImplementation(raw string) (sandboxpolicy.Implementation, error) {
	if strings.TrimSpace(raw) == "" {
		return sandboxpolicy.ImplementationHarnessBuiltin, nil
	}
	return sandboxpolicy.NormalizeImplementation(raw)
}

func openCodeRuntimeSandboxSpec(
	runtime db.OpenCodeRuntime,
) (*session.TclaudeLayerLaunchSpec, error) {
	if err := db.ValidateOpenCodeRuntimeTransport(runtime); err != nil {
		return nil, fmt.Errorf("OpenCode runtime transport authority: %w", err)
	}
	implementation, err := normalizeOpenCodeRuntimeSandboxImplementation(
		runtime.SandboxImplementation)
	if err != nil {
		return nil, fmt.Errorf("OpenCode runtime sandbox implementation: %w", err)
	}
	switch implementation {
	case sandboxpolicy.ImplementationHarnessBuiltin, sandboxpolicy.ImplementationOff,
		sandboxpolicy.ImplementationResourceOnly:
		if strings.TrimSpace(runtime.SandboxLaunchSpecJSON) != "" {
			return nil, fmt.Errorf(
				"OpenCode %s runtime unexpectedly carries a tclaude-layer launch spec",
				implementation)
		}
		return nil, nil
	case sandboxpolicy.ImplementationTclaudeLayer:
	default:
		return nil, fmt.Errorf("unsupported OpenCode runtime sandbox implementation %q", implementation)
	}
	raw := strings.TrimSpace(runtime.SandboxLaunchSpecJSON)
	if raw == "" {
		return nil, fmt.Errorf(
			"OpenCode tclaude-layer runtime has no persisted launch spec; refusing an unwrapped restart")
	}
	if len(raw) > openCodeSandboxSpecMax {
		return nil, fmt.Errorf("OpenCode tclaude-layer launch spec exceeds %d bytes", openCodeSandboxSpecMax)
	}
	var spec session.TclaudeLayerLaunchSpec
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return nil, fmt.Errorf("decode OpenCode tclaude-layer launch spec: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return nil, fmt.Errorf("decode OpenCode tclaude-layer launch spec trailer: %w", err)
	}
	if spec.Version != session.TclaudeLayerLaunchSpecVersion &&
		spec.Version != session.TclaudeLayerLegacyLaunchSpecVersion &&
		spec.Version != session.TclaudeLayerUnixRelaySpecVersion {
		return nil, fmt.Errorf("unsupported OpenCode tclaude-layer launch spec version %d", spec.Version)
	}
	if spec.Contract.HarnessName != harness.OpenCodeName {
		return nil, fmt.Errorf("OpenCode tclaude-layer launch spec names harness %q",
			spec.Contract.HarnessName)
	}
	if !filepath.IsAbs(spec.Contract.StateRoot) {
		return nil, fmt.Errorf("OpenCode tclaude-layer launch spec state root %q is not absolute",
			spec.Contract.StateRoot)
	}
	if len(spec.Contract.StateDirs) == 0 {
		return nil, fmt.Errorf("OpenCode tclaude-layer launch spec has no mutable state directories")
	}
	for _, stateDir := range spec.Contract.StateDirs {
		stateDir = canonicalOpenCodeRuntimePath(stateDir)
		if stateDir == "" {
			return nil, fmt.Errorf("OpenCode tclaude-layer launch spec has a non-absolute state directory")
		}
		inWriteContract := false
		for _, writeDir := range spec.Contract.WriteDirs {
			if canonicalOpenCodeRuntimePath(writeDir) == stateDir {
				inWriteContract = true
				break
			}
		}
		if !inWriteContract {
			return nil, fmt.Errorf(
				"OpenCode tclaude-layer state directory %q is not in the writable launch contract",
				stateDir)
		}
	}
	if len(spec.Contract.ReadOnlyStateDirs) == 0 &&
		len(spec.Contract.ReadOnlyBinds) == 0 {
		return nil, fmt.Errorf(
			"OpenCode tclaude-layer launch spec does not protect its executable state")
	}
	stateRoot := canonicalOpenCodeRuntimePath(spec.Contract.StateRoot)
	for _, stateDir := range spec.Contract.ReadOnlyStateDirs {
		stateDir = canonicalOpenCodeRuntimePath(stateDir)
		if stateDir == "" || stateDir == stateRoot ||
			!sandboxpolicy.PathContainsOrEqual(stateRoot, stateDir) {
			return nil, fmt.Errorf(
				"OpenCode tclaude-layer launch spec has invalid read-only state directory %q",
				stateDir)
		}
		access, covered := sandboxpolicy.EffectiveAccessAt(spec.Effective.Filesystem, stateDir)
		if !covered || access != sandboxpolicy.AccessRead {
			return nil, fmt.Errorf(
				"OpenCode tclaude-layer read-only state directory %q is not protected in the rendered contract",
				stateDir)
		}
	}
	if spec.Version == session.TclaudeLayerLaunchSpecVersion ||
		spec.Version == session.TclaudeLayerUnixRelaySpecVersion {
		if err := validateOpenCodeV3LaunchContract(
			spec.Contract, openCodeFilteredNetworkSpec(&spec)); err != nil {
			return nil, err
		}
	}
	if err := validateOpenCodeFilteredProviderAuthority(&spec); err != nil {
		return nil, fmt.Errorf(
			"revalidate OpenCode filtered provider authority: %w", err)
	}
	cwd := canonicalOpenCodeRuntimePath(runtime.Cwd)
	hasCwd := false
	for _, path := range spec.Contract.WriteDirs {
		if openCodeRuntimePathsEquivalent(path, cwd) {
			hasCwd = true
			break
		}
	}
	if cwd == "" || !hasCwd {
		return nil, fmt.Errorf(
			"OpenCode tclaude-layer launch spec does not preserve runtime cwd %q as a writable contract path",
			runtime.Cwd)
	}
	effective, err := revalidateOpenCodeRuntimeEffective(spec.Effective)
	if err != nil {
		return nil, err
	}
	spec.Effective = effective
	posture, err := session.TclaudeLayerNetworkPosture(spec.Effective)
	if err != nil {
		return nil, err
	}
	// Darwin keeps the authenticated loopback control transport when its
	// filtered model path is carried by the proxy. This is the only filtered
	// v3 replay shape; Linux remains on v4's inherited Unix relay below.
	darwinFilteredLoopback := false
	if goruntime.GOOS == "darwin" &&
		spec.Version == session.TclaudeLayerLaunchSpecVersion &&
		posture == sandboxpolicy.NetworkFiltered &&
		spec.Contract.NetworkEngine == sandboxpolicy.NetworkEngineProxy &&
		runtime.Transport == db.OpenCodeTransportLoopbackTCP {
		engine, engineErr := session.TclaudeLayerNetworkEngine(spec.Effective)
		if engineErr != nil {
			return nil, engineErr
		}
		darwinFilteredLoopback = engine == sandboxpolicy.NetworkEngineProxy
	}
	if spec.Version == session.TclaudeLayerUnixRelaySpecVersion {
		if (posture != sandboxpolicy.NetworkIsolatedWithAgentd &&
			posture != sandboxpolicy.NetworkFiltered) ||
			runtime.Transport != db.OpenCodeTransportUnixRelay ||
			spec.Contract.OpenCodeControl == nil ||
			runtime.ControlSocketPath != spec.Contract.OpenCodeControl.SocketPath {
			return nil, fmt.Errorf(
				"OpenCode tclaude-layer v4 runtime does not match its Unix-relay authority")
		}
		agentID := filepath.Base(filepath.Dir(runtime.ControlSocketPath))
		expectedControlPath, authorityErr := openCodeControlSocketPath(agentID)
		// Split, because the two arms are not the same statement and the
		// combined one could only ever say the second (TCL-909).
		//
		// When authorityErr is set the daemon could not COMPUTE the authority,
		// so "the control path is outside its allocated agent authority" is a
		// verdict about a comparison that never happened. That mattered
		// concretely: a replayed isolated or filtered spec whose allocation was
		// stranded by a changed XDG_DATA_HOME or HOME lands here, and the one
		// sentence naming that cause — and now the way out of it — was
		// discarded on the line below.
		//
		// Wrapping rather than replacing: the outer sentence still says which
		// contract failed, which is what this layer knows.
		if authorityErr != nil {
			return nil, fmt.Errorf(
				"OpenCode tclaude-layer v4 runtime control authority could not be established: %w",
				authorityErr)
		}
		if expectedControlPath != runtime.ControlSocketPath {
			// "does not match", not "is outside". The test is string equality
			// against one exact socket path, not containment in a region, and
			// containment language invites the operator to go looking for a
			// directory boundary that was never consulted.
			return nil, fmt.Errorf(
				"OpenCode tclaude-layer v4 runtime control path %q does not match its allocated agent authority %q",
				runtime.ControlSocketPath, expectedControlPath)
		}
	} else if !darwinFilteredLoopback &&
		(posture != sandboxpolicy.NetworkHostOpen ||
			runtime.Transport == db.OpenCodeTransportUnixRelay) {
		return nil, fmt.Errorf(
			"unsupported_sandbox_profile_network: OpenCode tclaude-layer restart requires the host-open loopback control plane and endpoint-ownership proof")
	}
	if err := session.ValidateTclaudeLayerLaunchSpec(spec); err != nil {
		return nil, fmt.Errorf("validate OpenCode tclaude-layer renderer contract: %w", err)
	}
	return &spec, nil
}

// revalidateOpenCodeRuntimeEffective omits the generated agentd AF_UNIX socket
// floor while re-running directory normalization. A live socket is not a
// directory and therefore cannot pass profile filesystem normalization, but
// the floor is not operator-authored filesystem authority: the launch contract
// binds and revalidates it separately as a Unix socket. Restore the exact
// frozen rows only after every other effective field revalidates unchanged.
func revalidateOpenCodeRuntimeEffective(
	effective sandboxpolicy.EffectiveProfile,
) (sandboxpolicy.EffectiveProfile, error) {
	socketFloor := make(map[string]bool)
	for _, path := range sandboxpolicy.AgentdSocketFloor() {
		path = session.CanonicalTclaudeLayerGeneratedPath(path)
		if path != "" {
			socketFloor[path] = true
		}
	}
	withoutSocketFloor := effective
	withoutSocketFloor.Filesystem = make(
		[]sandboxpolicy.FilesystemGrant, 0, len(effective.Filesystem))
	for _, grant := range effective.Filesystem {
		if socketFloor[filepath.Clean(grant.Path)] {
			if grant.Access != sandboxpolicy.AccessRead {
				return sandboxpolicy.EffectiveProfile{}, fmt.Errorf(
					"revalidate OpenCode tclaude-layer launch spec: generated agentd socket floor %q has unexpected %s access",
					grant.Path, grant.Access)
			}
			continue
		}
		withoutSocketFloor.Filesystem = append(
			withoutSocketFloor.Filesystem, grant)
	}
	snapshot := sandboxpolicy.NewSnapshot(withoutSocketFloor, nil)
	revalidated, err := sandboxpolicy.RevalidateSnapshot(snapshot)
	if err != nil {
		return sandboxpolicy.EffectiveProfile{}, fmt.Errorf(
			"revalidate OpenCode tclaude-layer launch spec: %w", err)
	}
	revalidated.Effective.Filesystem =
		append([]sandboxpolicy.FilesystemGrant(nil), effective.Filesystem...)
	return revalidated.Effective, nil
}

// openCodeReadOnlyConfigBindSource answers the one question both the launch
// contract's validation and the pre-sandbox bootstrap ask of a contract: which
// directory, if any, serves this contract's config app directory read-only?
// The validator only needs to know that one exists; the bootstrap needs to know
// WHICH, because that is the directory the sandbox reads through the bind. They
// ask here rather than each deriving "the config bind" for itself.
//
// The LAST matching bind wins, because that is the one the sandbox serves.
func openCodeReadOnlyConfigBindSource(
	contract session.TclaudeLayerLaunchContract,
) string {
	if len(contract.StateDirs) != 4 {
		return ""
	}
	configTarget := canonicalOpenCodeRuntimePath(contract.StateDirs[2])
	if configTarget == "" {
		return ""
	}
	source := ""
	for _, bind := range contract.ReadOnlyBinds {
		if canonicalOpenCodeRuntimePath(bind.Target) == configTarget {
			source = canonicalOpenCodeRuntimePath(bind.Source)
		}
	}
	return source
}

func canonicalOpenCodeRuntimePath(path string) string {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || !filepath.IsAbs(path) {
		return ""
	}
	return path
}

func startOpenCodeProcess(
	runtime *db.OpenCodeRuntime,
	sandboxSpec *session.TclaudeLayerLaunchSpec,
) (*openCodeProcess, error) {
	executable, err := harness.OpenCodeExecutable()
	if err != nil {
		return nil, fmt.Errorf("find OpenCode executable: %w", err)
	}
	if sandboxSpec != nil {
		executable, err = exposeOpenCodeExecutable(sandboxSpec, executable)
		if err != nil {
			return nil, err
		}
		if err := prepareOpenCodeTclaudeLayerState(sandboxSpec); err != nil {
			return nil, fmt.Errorf("prepare OpenCode tclaude-layer state: %w", err)
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
	command, args, extraFiles, unixHandshake, cleanup, err := openCodeServeProcessExec(
		executable, port, runtime, sandboxSpec)
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
	pidOut, err := clcommon.Default.Command(session.ExternalTmuxNoStartArgs(
		"display-message", "-p", "-t", clcommon.ExactTarget(tmuxSession)+":0.0", "#{pane_pid}")...).Output()
	if err != nil {
		_ = clcommon.Default.Command(session.ExternalTmuxNoStartArgs(
			"kill-session", "-t", clcommon.ExactTarget(tmuxSession))...).Run()
		return nil, fmt.Errorf("read OpenCode tmux process root: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidOut)))
	if err != nil || pid <= 1 {
		_ = clcommon.Default.Command("-N", "kill-session", "-t",
			clcommon.ExactTarget(tmuxSession)).Run()
		return nil, fmt.Errorf("external tmux returned invalid OpenCode process root %q", strings.TrimSpace(string(pidOut)))
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
	if err := clcommon.Default.Command(
		"-N", "has-session", "-t", clcommon.ExactTarget(tmuxSession)).Run(); err != nil {
		return nil
	}
	if err := clcommon.Default.Command(
		"-N", "kill-session", "-t", clcommon.ExactTarget(tmuxSession)).Run(); err != nil {
		return fmt.Errorf("reclaim orphaned OpenCode tmux session: %w", err)
	}
	return nil
}

func captureOpenCodeTmuxPane(tmuxSession string) string {
	out, err := clcommon.Default.Command(
		"-N", "capture-pane", "-p", "-S", "-200", "-t",
		clcommon.ExactTarget(tmuxSession)+":0.0").CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func watchOpenCodeTmuxProcess(process *openCodeProcess, runtime db.OpenCodeRuntime) {
	misses := 0
	for {
		err := clcommon.Default.Command(
			"-N", "has-session", "-t", clcommon.ExactTarget(process.tmuxSession)).Run()
		if err != nil {
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
	noCleanup := func() {}
	if runtime.Transport != db.OpenCodeTransportUnixRelay {
		command, args, err := openCodeServeExec(executable, port, sandboxSpec)
		return command, args, nil, nil, noCleanup, err
	}
	if sandboxSpec == nil || sandboxSpec.Version != session.TclaudeLayerUnixRelaySpecVersion {
		return "", nil, nil, nil, noCleanup, fmt.Errorf(
			"unix-relay OpenCode runtime requires a tclaude-layer v4 spec")
	}
	selfPath, err := openCodeRelayExecutable()
	if err != nil {
		return "", nil, nil, nil, noCleanup,
			fmt.Errorf("resolve tclaude relay executable: %w", err)
	}
	posture, err := session.TclaudeLayerNetworkPosture(sandboxSpec.Effective)
	if err != nil {
		return "", nil, nil, nil, noCleanup, err
	}
	root, err := session.TclaudeLayerRootPosture(posture, sandboxSpec.Effective)
	if err != nil {
		return "", nil, nil, nil, noCleanup, err
	}
	// The DEPLOYED engine, not the authored one on the contract. They diverge
	// for a filtered posture whose policy is non-discriminating: the contract
	// carries `proxy` as authored while the plan deploys none, and probing the
	// proxy engine's floor there would skip the pasta/nft/userns prerequisites
	// the launch is actually about to need. TclaudeLayerNetworkEngine is the
	// same resolution the plan itself performs.
	engine, err := session.TclaudeLayerNetworkEngine(sandboxSpec.Effective)
	if err != nil {
		return "", nil, nil, nil, noCleanup, err
	}
	bwrapBinary, _, err := resolveOpenCodeTclaudeLayer(posture, root, engine)
	if err != nil {
		return "", nil, nil, nil, noCleanup, err
	}
	serveArgs := []string{
		"serve", "--hostname", "127.0.0.1",
		"--port", port, "--log-level", "ERROR",
	}
	listenerFD, relayExecutableFD, err :=
		session.TclaudeLayerUnixRelayServerFDs(*sandboxSpec)
	if err != nil {
		return "", nil, nil, nil, noCleanup, err
	}
	relayArgv := []string{
		"/proc/self/fd/" + strconv.Itoa(relayExecutableFD),
		opencodeapi.InheritedUnixRelayMode,
		strconv.Itoa(listenerFD), "127.0.0.1:" + port, "--", executable,
	}
	relayArgv = append(relayArgv, serveArgs...)
	argv, err := session.TclaudeLayerUnixRelayServerExecArgs(
		bwrapBinary, *sandboxSpec, 2, relayArgv)
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

func openCodeServerEnvironment(
	ambient []string,
	sandboxSpec *session.TclaudeLayerLaunchSpec,
) []string {
	if sandboxSpec == nil {
		return append([]string(nil), ambient...)
	}
	privateState := len(sandboxSpec.Contract.Environment) > 0
	filtered := openCodeFilteredNetworkSpec(sandboxSpec)
	out := make([]string, 0, len(ambient)+len(sandboxSpec.Effective.Environment)+
		len(sandboxSpec.Contract.Environment)+8)
	for _, entry := range ambient {
		name := strings.SplitN(entry, "=", 2)[0]
		if (privateState && openCodePrivateEnvironmentName(name)) ||
			(filtered && openCodeFilteredControlledEnvironmentName(name)) {
			continue
		}
		out = append(out, entry)
	}
	for _, entry := range sandboxSpec.Effective.Environment {
		if privateState && openCodePrivateEnvironmentName(entry.Name) {
			continue
		}
		if filtered && openCodeFilteredControlledEnvironmentName(entry.Name) {
			continue
		}
		out = append(out, entry.Name+"="+entry.Value)
	}
	for _, entry := range sandboxSpec.Contract.Environment {
		out = append(out, entry.Name+"="+entry.Value)
	}
	if filtered {
		// These are OpenCode 1.18.6's provider-source isolation inputs.
		// They make the frozen inline provider content the only dynamic provider
		// source consumed by the authoritative server.
		out = append(out,
			"HOME="+filepath.Join(
				sandboxSpec.Contract.StateRoot, openCodeFilteredHomeBase),
			"OPENCODE_CONFIG=",
			"OPENCODE_CONFIG_DIR=",
			"OPENCODE_DISABLE_PROJECT_CONFIG=1",
			"OPENCODE_PURE=1",
			"OPENCODE_DISABLE_MODELS_FETCH=1",
			"OPENCODE_DISABLE_AUTOUPDATE=1",
			"OPENCODE_AUTH_CONTENT={}",
		)
	}
	return out
}

func openCodeFilteredNetworkSpec(
	spec *session.TclaudeLayerLaunchSpec,
) bool {
	if spec == nil {
		return false
	}
	posture, err := session.TclaudeLayerNetworkPosture(spec.Effective)
	if err != nil || posture != sandboxpolicy.NetworkFiltered {
		return false
	}
	axes, err := sandboxpolicy.PlannedEffectiveAccessAxes(spec.Effective)
	return err == nil &&
		!sandboxpolicy.NetworkRulesArePrivateRoutedOpen(axes.Network)
}

func openCodeFilteredControlledEnvironmentName(name string) bool {
	switch name {
	case "HOME", "OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR",
		"OPENCODE_DISABLE_PROJECT_CONFIG", "OPENCODE_PURE",
		"OPENCODE_DISABLE_MODELS_FETCH", "OPENCODE_DISABLE_AUTOUPDATE",
		"OPENCODE_AUTH_CONTENT":
		return true
	default:
		return false
	}
}

func openCodePrivateEnvironmentName(name string) bool {
	switch name {
	case "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME",
		"OPENCODE_CONFIG_DIR":
		return true
	default:
		return false
	}
}

func validateOpenCodeV3LaunchContract(
	contract session.TclaudeLayerLaunchContract,
	filtered bool,
) error {
	if len(contract.Environment) == 0 {
		// Grandfathered v3 contracts intentionally keep ambient XDG state.
		if openCodeAgentIDRE.MatchString(filepath.Base(
			canonicalOpenCodeRuntimePath(contract.StateRoot))) {
			return fmt.Errorf("private OpenCode v3 launch contract has no enforced XDG environment")
		}
		if len(contract.FinalHideDirs) != 1 {
			return fmt.Errorf("legacy OpenCode v3 launch contract does not hide the private-state parent")
		}
		return nil
	}
	expectedNames := []string{
		"XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME",
	}
	if len(contract.Environment) != len(expectedNames) ||
		len(contract.StateDirs) != len(expectedNames) {
		return fmt.Errorf("private OpenCode v3 launch contract must carry exactly four XDG roots")
	}
	stateRoot := canonicalOpenCodeRuntimePath(contract.StateRoot)
	if stateRoot == "" || !openCodeAgentIDRE.MatchString(filepath.Base(stateRoot)) {
		return fmt.Errorf("private OpenCode v3 state root %q does not end in a stable agent id",
			contract.StateRoot)
	}
	for i, name := range expectedNames {
		entry := contract.Environment[i]
		if entry.Name != name {
			return fmt.Errorf("private OpenCode v3 launch contract environment %d is %q, want %q",
				i, entry.Name, name)
		}
		wantStateDir := filepath.Join(entry.Value, "opencode")
		matchesStateDir := canonicalOpenCodeRuntimePath(contract.StateDirs[i]) ==
			canonicalOpenCodeRuntimePath(wantStateDir)
		if i == 2 && filtered {
			wantConfigBase := filepath.Join(stateRoot, openCodeFilteredConfigBase)
			if canonicalOpenCodeRuntimePath(entry.Value) !=
				canonicalOpenCodeRuntimePath(wantConfigBase) {
				return fmt.Errorf(
					"private filtered OpenCode XDG_CONFIG_HOME is %q, want empty per-agent base %q",
					entry.Value, wantConfigBase)
			}
			// The normal config state dir remains the daemon-final read-only
			// projection of ambient config, but filtered OpenCode does not
			// select it as XDG_CONFIG_HOME.
			matchesStateDir = canonicalOpenCodeRuntimePath(contract.StateDirs[i]) ==
				canonicalOpenCodeRuntimePath(
					filepath.Join(stateRoot, "config", "opencode"))
		}
		if i == 2 && !matchesStateDir {
			// Darwin keeps XDG_CONFIG_HOME at the real host base while the
			// daemon-final read-only root carries the resolved identity of a
			// possible leaf symlink at <base>/opencode.
			matchesStateDir = openCodeRuntimePathsEquivalent(
				contract.StateDirs[i], wantStateDir)
		}
		if !matchesStateDir {
			return fmt.Errorf("private OpenCode v3 %s does not target state directory %q",
				name, contract.StateDirs[i])
		}
	}
	privatePair := false
	for _, pair := range contract.PrivateWriteDirs {
		if canonicalOpenCodeRuntimePath(pair.Current) == stateRoot &&
			canonicalOpenCodeRuntimePath(pair.Parent) == filepath.Dir(stateRoot) {
			privatePair = true
			break
		}
	}
	if !privatePair {
		return fmt.Errorf("private OpenCode v3 launch contract does not hide siblings and reopen its agent root")
	}
	if len(contract.FinalHideDirs) != 3 {
		return fmt.Errorf("private OpenCode v3 launch contract must hide the three ambient mutable XDG roots")
	}
	if openCodeReadOnlyConfigBindSource(contract) == "" {
		return fmt.Errorf("private OpenCode v3 launch contract does not bind global config read-only")
	}
	if filtered {
		expected := []string{
			filepath.Join(stateRoot, openCodeFilteredConfigBase, "opencode"),
			filepath.Join(stateRoot, openCodeFilteredHomeBase, ".opencode"),
		}
		if len(contract.ReadOnlyBinds) < len(expected) {
			return fmt.Errorf(
				"private filtered OpenCode contract does not seal its provider-empty config roots")
		}
		tail := contract.ReadOnlyBinds[len(contract.ReadOnlyBinds)-len(expected):]
		for index, path := range expected {
			if canonicalOpenCodeRuntimePath(tail[index].Source) != path ||
				canonicalOpenCodeRuntimePath(tail[index].Target) != path {
				return fmt.Errorf(
					"private filtered OpenCode contract does not daemon-final seal provider config root %q",
					path)
			}
		}
	}
	return nil
}

func validateOpenCodeFilteredProviderAuthority(
	spec *session.TclaudeLayerLaunchSpec,
) error {
	if !openCodeFilteredNetworkSpec(spec) {
		return nil
	}
	if err := validateOpenCodeFilteredProviderSources(
		spec.Contract.StateRoot); err != nil {
		return err
	}
	return refuseOpenCodeFilteredActiveAccount(spec.Contract.StateRoot)
}

func openCodeRuntimePathsEquivalent(left, right string) bool {
	left = canonicalOpenCodeRuntimePath(left)
	right = canonicalOpenCodeRuntimePath(right)
	if left == "" || right == "" {
		return false
	}
	if left == right {
		return true
	}
	resolvedLeft, leftErr := filepath.EvalSymlinks(left)
	resolvedRight, rightErr := filepath.EvalSymlinks(right)
	return leftErr == nil && rightErr == nil &&
		canonicalOpenCodeRuntimePath(resolvedLeft) ==
			canonicalOpenCodeRuntimePath(resolvedRight)
}

func openCodeServeExec(
	executable, port string,
	sandboxSpec *session.TclaudeLayerLaunchSpec,
) (string, []string, error) {
	serveArgs := []string{
		"serve", "--hostname", "127.0.0.1",
		"--port", port, "--log-level", "ERROR",
	}
	if sandboxSpec == nil {
		return executable, serveArgs, nil
	}
	filteredDarwinProxy := false
	if goruntime.GOOS == "darwin" && openCodeFilteredNetworkSpec(sandboxSpec) {
		engine, engineErr := session.TclaudeLayerNetworkEngine(sandboxSpec.Effective)
		if engineErr != nil {
			return "", nil, engineErr
		}
		filteredDarwinProxy = engine == sandboxpolicy.NetworkEngineProxy
	}
	if !filteredDarwinProxy &&
		sandboxSpec.Effective.NetworkAccess != sandboxpolicy.NetworkAccessInherit &&
		sandboxSpec.Effective.NetworkAccess != sandboxpolicy.NetworkAccessInternet {
		return "", nil, fmt.Errorf(
			"unsupported_sandbox_profile_network: OpenCode tclaude-layer requires the host-open loopback control plane and endpoint-ownership proof",
		)
	}
	resolvePosture := sandboxpolicy.NetworkHostOpen
	if filteredDarwinProxy {
		resolvePosture = sandboxpolicy.NetworkFiltered
	}
	root, err := session.TclaudeLayerRootPosture(
		resolvePosture, sandboxSpec.Effective)
	if err != nil {
		return "", nil, err
	}
	engine, err := session.TclaudeLayerNetworkEngine(sandboxSpec.Effective)
	if err != nil {
		return "", nil, err
	}
	bwrapBinary, _, err := resolveOpenCodeTclaudeLayer(
		resolvePosture, root, engine)
	if err != nil {
		return "", nil, err
	}
	serveCommand := clcommon.ShellQuoteArg(executable)
	for _, arg := range serveArgs {
		serveCommand += " " + clcommon.ShellQuoteArg(arg)
	}
	wrapped := ""
	if filteredDarwinProxy {
		bindPort, parseErr := strconv.Atoi(port)
		if parseErr != nil || bindPort < 1 || bindPort > 65535 {
			return "", nil, fmt.Errorf("parse OpenCode loopback control port %q", port)
		}
		wrapped, err = wrapOpenCodeTclaudeLayerWithLoopbackBind(
			bwrapBinary, *sandboxSpec, bindPort, serveCommand)
	} else {
		wrapped, err = wrapOpenCodeTclaudeLayer(bwrapBinary, *sandboxSpec, serveCommand)
	}
	if err != nil {
		return "", nil, fmt.Errorf("wrap OpenCode server with tclaude-layer: %w", err)
	}
	// exec makes the PID agentd records the top wrapper rather than an
	// intermediate shell. Stop/recovery can therefore target the boundary,
	// while endpoint ownership continues to prove the listener in its subtree.
	return "sh", []string{"-c", "exec " + wrapped}, nil
}

func openCodeTclaudeLayerLaunchSpec(
	implementation, cwd string,
	gitWriteDirs []string,
	snapshot *sandboxpolicy.Snapshot,
	agentID string,
	privateSessionIDs ...string,
) (*session.TclaudeLayerLaunchSpec, error) {
	normalized, err := sandboxpolicy.NormalizeImplementation(implementation)
	if err != nil {
		return nil, err
	}
	if normalized != sandboxpolicy.ImplementationTclaudeLayer {
		return nil, nil
	}
	effective := sandboxpolicy.EffectiveProfile{}
	if snapshot != nil {
		effective = snapshot.Effective
	}
	posture, err := session.TclaudeLayerNetworkPosture(effective)
	if err != nil {
		return nil, err
	}
	if posture != sandboxpolicy.NetworkHostOpen {
		if posture == sandboxpolicy.NetworkFiltered {
			axes, axesErr := sandboxpolicy.PlannedEffectiveAccessAxes(effective)
			if axesErr != nil {
				return nil, axesErr
			}
			providerFiltered := !sandboxpolicy.NetworkRulesArePrivateRoutedOpen(
				axes.Network)
			// The engine comes from the composed policy, through the same
			// resolution the launch itself performs, so the preflight probes
			// the floor the launch will actually build. Never re-derived here:
			// TclaudeLayerNetworkEngine is the one answer, and asking a second
			// way is how a preflight ends up probing a different floor.
			engine, engineErr := session.TclaudeLayerNetworkEngine(effective)
			if engineErr != nil {
				return nil, engineErr
			}
			_, _, filteredErr := resolveOpenCodeTclaudeLayer(
				posture, sandboxpolicy.RootConstructed, engine)
			if filteredErr != nil {
				return nil, filteredErr
			}
			if goruntime.GOOS == "darwin" {
				if !providerFiltered {
					return nil, fmt.Errorf("private routed networking requires Linux")
				}
				return buildOpenCodeTclaudeLayerLaunchSpec(
					cwd, gitWriteDirs, snapshot, agentID, false, true,
					privateSessionIDs...)
			}
			if goruntime.GOOS != "linux" {
				return nil, fmt.Errorf("OpenCode filtered networking requires Linux or macOS")
			}
			return buildOpenCodeTclaudeLayerLaunchSpec(
				cwd, gitWriteDirs, snapshot, agentID, true, providerFiltered,
				privateSessionIDs...)
		}
		openCodeHarness, resolveErr := harness.Resolve(harness.OpenCodeName)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if _, err := session.ValidateTclaudeLayerNetwork(
			openCodeHarness,
			effective,
			harness.ResolvedModelTransport{},
		); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("unsupported OpenCode tclaude-layer network posture %s", posture)
	}
	return buildOpenCodeTclaudeLayerLaunchSpec(
		cwd, gitWriteDirs, snapshot, agentID, false, false,
		privateSessionIDs...)
}

// openCodeUnixRelayLaunchSpec builds the isolated v4 control-plane smoke
// boundary. Public filtered launches use the same builder through
// openCodeTclaudeLayerLaunchSpec after provider and endpoint preflight.
func openCodeUnixRelayLaunchSpec(
	implementation, cwd string,
	gitWriteDirs []string,
	snapshot *sandboxpolicy.Snapshot,
	agentID string,
	privateSessionIDs ...string,
) (*session.TclaudeLayerLaunchSpec, error) {
	if goruntime.GOOS != "linux" {
		return nil, fmt.Errorf("OpenCode Unix relay is Linux-only")
	}
	normalized, err := sandboxpolicy.NormalizeImplementation(implementation)
	if err != nil {
		return nil, err
	}
	if normalized != sandboxpolicy.ImplementationTclaudeLayer {
		return nil, fmt.Errorf("OpenCode Unix relay requires tclaude-layer")
	}
	if snapshot == nil {
		return nil, fmt.Errorf("OpenCode Unix relay requires an isolated effective profile")
	}
	posture, err := session.TclaudeLayerNetworkPosture(snapshot.Effective)
	if err != nil {
		return nil, err
	}
	if posture != sandboxpolicy.NetworkIsolatedWithAgentd {
		return nil, fmt.Errorf("OpenCode Unix relay requires the isolated network posture")
	}
	return buildOpenCodeTclaudeLayerLaunchSpec(
		cwd, gitWriteDirs, snapshot, agentID, true, false,
		privateSessionIDs...)
}

func buildOpenCodeTclaudeLayerLaunchSpec(
	cwd string,
	gitWriteDirs []string,
	snapshot *sandboxpolicy.Snapshot,
	agentID string,
	unixRelay bool,
	filtered bool,
	privateSessionIDs ...string,
) (*session.TclaudeLayerLaunchSpec, error) {
	allocation, err := requireOpenCodeStateAllocation(agentID)
	if err != nil {
		return nil, err
	}
	if filtered {
		if err := refuseOpenCodeFilteredActiveAccount(allocation.StateRoot); err != nil {
			return nil, err
		}
	}
	layout, err := openCodeStateLayoutForAllocation(*allocation)
	if err != nil {
		return nil, err
	}
	if filtered {
		if err := isolateOpenCodeFilteredConfig(layout); err != nil {
			return nil, err
		}
	}
	var privateWriteDirs []session.TclaudeLayerPrivateWriteDir
	if allocation.Mode == db.OpenCodeStatePrivate {
		privateWriteDirs = append(privateWriteDirs, session.TclaudeLayerPrivateWriteDir{
			Parent: layout.parent, Current: allocation.StateRoot,
		})
	}
	if len(privateSessionIDs) > 0 && strings.TrimSpace(privateSessionIDs[0]) != "" {
		privateWriteDirs = append(privateWriteDirs, session.TclaudeLayerPrivateWriteDir{
			Parent:  tclcommon.SpawnAttachmentsPrivateBase(),
			Current: tclcommon.SpawnAttachmentsPrivateDir(privateSessionIDs[0]),
		})
	}
	input := session.TclaudeLayerLaunchInput{
		HarnessName:      harness.OpenCodeName,
		Cwd:              cwd,
		GitWriteDirs:     gitWriteDirs,
		Snapshot:         snapshot,
		PrivateWriteDirs: privateWriteDirs,
		Environment:      layout.environment,
		FinalHideDirs:    layout.finalHideDirs,
		ReadOnlyBinds:    layout.readOnlyBinds,
	}
	if unixRelay {
		controlPath, controlErr := openCodeControlSocketPath(agentID)
		if controlErr != nil {
			return nil, controlErr
		}
		input.OpenCodeControl = &session.TclaudeLayerOpenCodeControl{
			Transport: session.TclaudeLayerUnixRelayTransport, SocketPath: controlPath,
		}
	}
	if allocation.Mode == db.OpenCodeStatePrivate {
		input.StateRoot = allocation.StateRoot
		input.StateDirs = layout.stateDirs
	}
	spec, err := session.BuildTclaudeLayerLaunchSpec(input)
	if err != nil {
		return nil, err
	}
	return &spec, nil
}

// refuseOpenCodeFilteredActiveAccount prevents OpenCode's persistent
// account/org service from loading a remote provider config after the frozen
// inline config. The pinned server keeps this state in its per-agent database,
// independently of auth.json and OPENCODE_AUTH_CONTENT.
func refuseOpenCodeFilteredActiveAccount(stateRoot string) error {
	databasePath := filepath.Join(stateRoot, "data", "opencode", "opencode.db")
	info, err := os.Lstat(databasePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf(
			"OpenCode filtered cannot inspect persistent account authority: %w; fix the private state or use network open",
			err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf(
			"OpenCode filtered cannot inspect non-regular persistent account database %q; fix the private state or use network open",
			databasePath)
	}
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     databasePath,
		RawQuery: "mode=ro&_pragma=busy_timeout(1000)&_pragma=query_only(1)",
	}).String()
	store, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf(
			"OpenCode filtered cannot inspect persistent account authority: %w; sign out of OpenCode or use network open",
			err)
	}
	defer func() { _ = store.Close() }()
	var activeAccount, activeOrg sql.NullString
	err = store.QueryRow(
		`SELECT active_account_id, active_org_id FROM account_state WHERE id = 1`,
	).Scan(&activeAccount, &activeOrg)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf(
			"OpenCode filtered cannot prove persistent account/org provider configuration absent: %w; migrate or repair the OpenCode state under network open, then retry",
			err)
	}
	if activeAccount.Valid && strings.TrimSpace(activeAccount.String) != "" &&
		activeOrg.Valid && strings.TrimSpace(activeOrg.String) != "" {
		return fmt.Errorf(
			"OpenCode filtered does not support an active persistent account/org because its remote config loads after OPENCODE_CONFIG_CONTENT; sign out or clear the active organization, or use network open")
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

func openCodeRequest(method, endpoint string, runtime db.OpenCodeRuntime, body any) (*http.Request, error) {
	return opencodeapi.NewRequest(method, endpoint, runtime, body)
}

func openCodeProcessOwnsEndpoint(rootPID int, endpoint string) bool {
	return opencodeapi.ProcessOwnsEndpoint(rootPID, endpoint)
}

// openCodeRuntimeVerified confirms a recorded runtime's pid is still the managed
// server before that pid is trusted as an identity or a kill target. It is a
// package var so identity/kill tests can stand up a synthetic proc tree without
// also binding a real listening socket (the same seam pattern as procName /
// procParent in identity.go). Production points it at endpoint ownership.
var openCodeRuntimeVerified = openCodeRuntimeOwnsRecordedPID

// openCodeRuntimeOwnsRecordedPID reports whether the pid recorded for runtime is
// still the managed `opencode serve` process — i.e. that pid (or a descendant)
// still owns runtime.ServerURL's listening socket. This is the recovered-PID
// identity gate: a server that died frees its port, so a same-user process that
// later inherits the stale pid cannot pass (it does not own our recorded
// endpoint), while a live managed server always holds its own port. Endpoint
// ownership is a stronger identity signal than start-time/argv because it binds
// the pid to the exact host:port we minted, and it reuses the same proof the
// per-request auth gate already trusts.
//
// NOTE: ownership matches the pid's whole subtree (ProcessOwnsEndpoint walks
// /proc children), and every managed serve is a child of agentd, so passing
// agentd's own pid here would match. Both callers therefore exclude os.Getpid()
// before consulting this gate — the kill path in stopOpenCodeProcess, and the
// identity path in openCodeRuntimeConvByPID (whose parent probe can pass
// agentd's own pid because a managed serve is agentd's direct child).
func openCodeRuntimeOwnsRecordedPID(runtime db.OpenCodeRuntime) bool {
	return runtime.PID > 1 &&
		opencodeapi.RuntimeOwnsEndpoint(runtime)
}

func openCodeHealthy(runtime db.OpenCodeRuntime) bool {
	request, err := openCodeRequest(http.MethodGet,
		runtime.ServerURL+"/global/health", runtime, nil)
	if err != nil {
		return false
	}
	response, err := opencodeapi.Do(openCodeHealthHTTPClient, request, runtime)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false
	}
	var health struct {
		Healthy bool `json:"healthy"`
	}
	return json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&health) == nil &&
		health.Healthy
}

func openCodeHealthyAfterRetries(runtime db.OpenCodeRuntime, attempts int, delay time.Duration) bool {
	for attempt := 0; attempt < attempts; attempt++ {
		if openCodeHealthy(runtime) {
			return true
		}
		if attempt+1 < attempts {
			time.Sleep(delay)
		}
	}
	return false
}

func createOpenCodeSession(runtime db.OpenCodeRuntime, title string) (string, error) {
	rules, err := decodeOpenCodePermissionRules(runtime.PermissionJSON)
	if err != nil {
		return "", err
	}
	body := map[string]any{"permission": rules}
	if strings.TrimSpace(title) != "" {
		body["title"] = title
	}
	endpoint := runtime.ServerURL + "/session?directory=" + url.QueryEscape(runtime.Cwd)
	request, err := openCodeRequest(http.MethodPost, endpoint, runtime, body)
	if err != nil {
		return "", err
	}
	response, err := opencodeapi.Do(openCodeHTTPClient, request, runtime)
	if err != nil {
		return "", fmt.Errorf("create OpenCode session: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		detailText := strings.TrimSpace(string(detail))
		if detailText != "" {
			return "", fmt.Errorf(
				"create OpenCode session: HTTP %d: %s", response.StatusCode, detailText)
		}
		return "", fmt.Errorf("create OpenCode session: HTTP %d", response.StatusCode)
	}
	var created struct {
		ID         string                           `json:"id"`
		Permission []harness.OpenCodePermissionRule `json:"permission"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&created); err != nil {
		return "", fmt.Errorf("decode OpenCode session: %w", err)
	}
	if !strings.HasPrefix(created.ID, "ses_") {
		return "", fmt.Errorf("create OpenCode session returned invalid id %q", created.ID)
	}
	if !openCodePermissionHasSuffix(created.Permission, rules) {
		return "", fmt.Errorf("OpenCode session did not retain the permission policy at creation")
	}
	return created.ID, nil
}

func decodeOpenCodePermissionRules(raw string) ([]harness.OpenCodePermissionRule, error) {
	var rules []harness.OpenCodePermissionRule
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		return nil, fmt.Errorf("decode OpenCode permission policy: %w", err)
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("OpenCode permission policy is empty")
	}
	for i, rule := range rules {
		if strings.TrimSpace(rule.Permission) == "" || strings.TrimSpace(rule.Pattern) == "" {
			return nil, fmt.Errorf("OpenCode permission rule %d has an empty permission or pattern", i)
		}
		switch rule.Action {
		case "allow", "ask", "deny":
		default:
			return nil, fmt.Errorf("OpenCode permission rule %d has invalid action %q", i, rule.Action)
		}
	}
	return rules, nil
}

func ensureOpenCodeSessionPermission(runtime db.OpenCodeRuntime) error {
	if strings.TrimSpace(runtime.PermissionJSON) == "" {
		// A v149 runtime row cannot prove what authority its live session has.
		// Reconciliation must fail closed rather than blessing the historical
		// unscoped posture; the reaper will stop it and a current relaunch will
		// compile and persist an explicit policy.
		return fmt.Errorf("OpenCode runtime has no persisted permission policy; relaunch it under current access control")
	}
	expected, err := decodeOpenCodePermissionRules(runtime.PermissionJSON)
	if err != nil {
		return err
	}
	current, err := getOpenCodeSessionPermission(runtime)
	if err != nil {
		return err
	}
	if openCodePermissionHasSuffix(current, expected) {
		return nil
	}
	endpoint := runtime.ServerURL + "/session/" + url.PathEscape(runtime.ConvID) +
		"?directory=" + url.QueryEscape(runtime.Cwd)
	request, err := openCodeRequest(http.MethodPatch, endpoint, runtime,
		map[string]any{"permission": expected})
	if err != nil {
		return err
	}
	response, err := opencodeapi.Do(openCodeHTTPClient, request, runtime)
	if err != nil {
		return fmt.Errorf("reapply OpenCode session permission: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("reapply OpenCode session permission: HTTP %d", response.StatusCode)
	}
	var updated struct {
		Permission []harness.OpenCodePermissionRule `json:"permission"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&updated); err != nil {
		return fmt.Errorf("decode reapplied OpenCode session permission: %w", err)
	}
	if !openCodePermissionHasSuffix(updated.Permission, expected) {
		return fmt.Errorf("OpenCode session did not retain the reapplied permission policy")
	}
	return nil
}

func getOpenCodeSessionPermission(runtime db.OpenCodeRuntime) ([]harness.OpenCodePermissionRule, error) {
	if strings.TrimSpace(runtime.ConvID) == "" {
		return nil, fmt.Errorf("OpenCode conversation id is empty")
	}
	endpoint := runtime.ServerURL + "/session/" + url.PathEscape(runtime.ConvID) +
		"?directory=" + url.QueryEscape(runtime.Cwd)
	request, err := openCodeRequest(http.MethodGet, endpoint, runtime, nil)
	if err != nil {
		return nil, err
	}
	response, err := opencodeapi.Do(openCodeHTTPClient, request, runtime)
	if err != nil {
		return nil, fmt.Errorf("read OpenCode session permission: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("read OpenCode session permission: HTTP %d", response.StatusCode)
	}
	var session struct {
		Permission []harness.OpenCodePermissionRule `json:"permission"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&session); err != nil {
		return nil, fmt.Errorf("decode OpenCode session permission: %w", err)
	}
	return session.Permission, nil
}

func openCodePermissionHasSuffix(current, expected []harness.OpenCodePermissionRule) bool {
	if len(expected) == 0 || len(current) < len(expected) {
		return false
	}
	offset := len(current) - len(expected)
	for i := range expected {
		if current[offset+i] != expected[i] {
			return false
		}
	}
	return true
}

type openCodePromptAcceptanceUncertainError struct {
	err error
}

func (e *openCodePromptAcceptanceUncertainError) Error() string {
	return e.err.Error()
}

func (e *openCodePromptAcceptanceUncertainError) Unwrap() error {
	return e.err
}

func sendOpenCodePrompt(launch *openCodeLaunch, cwd, prompt, model, effort string) error {
	return sendOpenCodePromptWithMessageID(launch, cwd, prompt, model, effort, "")
}

func sendOpenCodePromptWithMessageID(
	launch *openCodeLaunch,
	cwd, prompt, model, effort, messageID string,
) error {
	if launch == nil || prompt == "" {
		return nil
	}
	body := map[string]any{
		"parts": []map[string]string{{"type": "text", "text": prompt}},
	}
	if provider, modelID, ok := strings.Cut(model, "/"); ok && provider != "" && modelID != "" {
		body["model"] = map[string]string{"providerID": provider, "modelID": modelID}
	}
	if effort != "" {
		body["variant"] = effort
	}
	if messageID != "" {
		body["messageID"] = messageID
	}
	endpoint := launch.ServerURL + "/session/" + url.PathEscape(launch.ConvID) +
		"/prompt_async?directory=" + url.QueryEscape(cwd)
	runtime := db.OpenCodeRuntime{
		PID: launch.PID, ServerURL: launch.ServerURL, Password: launch.Password,
		Transport: launch.Transport, ControlSocketPath: launch.ControlSocketPath,
		ControlSocketDevice: launch.ControlSocketDevice,
		ControlSocketInode:  launch.ControlSocketInode,
	}
	request, err := openCodeRequest(http.MethodPost, endpoint, runtime, body)
	if err != nil {
		return err
	}
	response, err := opencodeapi.Do(openCodeHTTPClient, request, runtime)
	if err != nil {
		return &openCodePromptAcceptanceUncertainError{
			err: fmt.Errorf("submit OpenCode launch prompt: %w", err),
		}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("submit OpenCode launch prompt: HTTP %d", response.StatusCode)
	}
	return nil
}

// sendOpenCodeNudge delivers a queued inbox nudge to the conversation owned by
// the managed OpenCode server. OpenCode's tmux pane is only an attach client;
// typing untrusted message content into that pane can miss the TUI entirely and
// reach its foreground shell. The server prompt endpoint is the authoritative
// input channel and preserves the nudge as one user turn.
//
// A missing or unhealthy runtime is a delivery failure, not permission to fall
// back to send-keys. Returning an error lets the shared nudge queue release its
// durable claim and retry later without losing the inbox row. A runtime that
// stopped is reconciled once before the prompt is attempted.
//
// Delivery is at-least-once: if prompt_async accepts the turn but its response
// or the queue completion stamp is lost, retry may submit it again. The framed
// message ID is the recipient's deduplication cue.
func sendOpenCodeNudge(convID, nudge string) error {
	runtime, err := readyOpenCodeRuntime(convID)
	if err != nil {
		return err
	}
	return sendOpenCodePrompt(&openCodeLaunch{
		SessionID:           runtime.SessionID,
		ConvID:              runtime.ConvID,
		ServerURL:           runtime.ServerURL,
		Password:            runtime.Password,
		PID:                 runtime.PID,
		Transport:           runtime.Transport,
		ControlSocketPath:   runtime.ControlSocketPath,
		ControlSocketDevice: runtime.ControlSocketDevice,
		ControlSocketInode:  runtime.ControlSocketInode,
	}, runtime.Cwd, nudge, "", "")
}

func openCodePromptMessageExists(
	runtime db.OpenCodeRuntime,
	messageID string,
) (bool, error) {
	endpoint := runtime.ServerURL + "/session/" + url.PathEscape(runtime.ConvID) +
		"/message/" + url.PathEscape(messageID) +
		"?directory=" + url.QueryEscape(runtime.Cwd)
	request, err := openCodeRequest(http.MethodGet, endpoint, runtime, nil)
	if err != nil {
		return false, err
	}
	response, err := opencodeapi.Do(openCodeHTTPClient, request, runtime)
	if err != nil {
		return false, fmt.Errorf("reconcile OpenCode prompt message: %w", err)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf(
			"reconcile OpenCode prompt message: HTTP %d", response.StatusCode)
	}
}

// sendOpenCodeStandingOrderNudge wraps prompt submission in a durable origin
// handshake. The SSE projector activates it only when OpenCode publishes an
// assistant message whose parent is the exact submitted user-message ID, then
// suppresses standing-order evaluation until Stop. Unrelated queued prompts
// and late events from an old conversation generation cannot steal the marker.
func sendOpenCodeStandingOrderNudge(
	convID, targetAgent string,
	origin *db.StandingOrderAgentMessageOrigin,
	nudge string,
) error {
	if origin == nil {
		return fmt.Errorf("standing-order OpenCode delivery requires trusted provenance")
	}
	if targetAgent == "" {
		var err error
		targetAgent, err = db.AgentIDForConv(convID)
		if err != nil {
			return fmt.Errorf("resolve standing-order target agent: %w", err)
		}
	}
	if targetAgent == "" {
		return fmt.Errorf("standing-order OpenCode delivery requires a stable target agent")
	}
	runtime, err := readyOpenCodeRuntime(convID)
	if err != nil {
		return err
	}
	exists, err := openCodePromptMessageExists(*runtime, origin.OpenCodeMessageID)
	if err != nil {
		return err
	}
	if exists {
		// A previous prompt_async attempt was accepted even if its response was
		// lost. Never resubmit the same ID: OpenCode may reject the duplicate
		// while the original remains queued. Reconcile its assistant from
		// history so a completed turn does not create a phantom 24-hour pending
		// marker when only the inbox delivery stamp was lost.
		historyState, historyErr := openCodePromptTurnHistory(
			*runtime, origin.OpenCodeMessageID)
		if historyErr != nil {
			return historyErr
		}
		current, getErr := db.GetStandingOrderTurnOrigin(
			targetAgent, convID, time.Now())
		if getErr != nil {
			return getErr
		}
		if current != nil &&
			(current.MessageID != origin.MessageID ||
				current.OpenCodeMessageID != origin.OpenCodeMessageID) {
			return fmt.Errorf("another standing-order turn is pending or active")
		}
		if historyState == openCodePromptTurnCompleted {
			if current != nil && current.State == db.StandingOrderTurnOriginActive {
				return db.CompleteStandingOrderTurnOrigin(targetAgent, convID)
			}
			if current != nil {
				return db.CancelPendingStandingOrderTurnOrigin(
					targetAgent, convID, origin.MessageID,
					origin.OpenCodeMessageID)
			}
			return nil
		}
		if current == nil {
			if armErr := db.ArmStandingOrderTurnOrigin(
				targetAgent, convID, origin.MessageID, origin.OpenCodeMessageID,
				time.Now(), standingOrderOriginActiveTTL); armErr != nil {
				return fmt.Errorf("restore accepted standing-order turn origin: %w", armErr)
			}
			current = &db.StandingOrderTurnOrigin{
				TargetAgent: targetAgent, TargetConv: convID,
				MessageID: origin.MessageID, OpenCodeMessageID: origin.OpenCodeMessageID,
				State: db.StandingOrderTurnOriginPending,
			}
		}
		if historyState == openCodePromptTurnActive &&
			current.State == db.StandingOrderTurnOriginPending {
			_, activateErr := db.ActivateStandingOrderTurnOrigin(
				targetAgent, convID, origin.OpenCodeMessageID,
				time.Now(), standingOrderOriginActiveTTL)
			return activateErr
		}
		if current.State == db.StandingOrderTurnOriginPending {
			if refreshErr := db.RefreshPendingStandingOrderTurnOrigin(
				targetAgent, convID, origin.MessageID, origin.OpenCodeMessageID,
				time.Now(), standingOrderOriginActiveTTL); refreshErr != nil {
				return refreshErr
			}
		}
		return nil
	}
	if err := db.ArmStandingOrderTurnOrigin(
		targetAgent, convID, origin.MessageID, origin.OpenCodeMessageID,
		time.Now(), standingOrderOriginPendingTTL); err != nil {
		return fmt.Errorf("arm standing-order turn origin: %w", err)
	}
	err = sendOpenCodePromptWithMessageID(&openCodeLaunch{
		SessionID:           runtime.SessionID,
		ConvID:              runtime.ConvID,
		ServerURL:           runtime.ServerURL,
		Password:            runtime.Password,
		PID:                 runtime.PID,
		Transport:           runtime.Transport,
		ControlSocketPath:   runtime.ControlSocketPath,
		ControlSocketDevice: runtime.ControlSocketDevice,
		ControlSocketInode:  runtime.ControlSocketInode,
	}, runtime.Cwd, nudge, "", "", origin.OpenCodeMessageID)
	if err != nil {
		var uncertain *openCodePromptAcceptanceUncertainError
		if errors.As(err, &uncertain) {
			// The server may have accepted and persisted the user message before
			// the transport failed. A successful exact lookup turns the attempt
			// into a confirmed delivery; lookup failure keeps the short pending
			// marker and the durable inbox row for a later reconciliation.
			if accepted, lookupErr := openCodePromptMessageExists(
				*runtime, origin.OpenCodeMessageID); lookupErr == nil && accepted {
				_ = db.RefreshPendingStandingOrderTurnOrigin(
					targetAgent, convID, origin.MessageID,
					origin.OpenCodeMessageID, time.Now(),
					standingOrderOriginActiveTTL)
				return nil
			}
		} else {
			if cancelErr := db.CancelPendingStandingOrderTurnOrigin(
				targetAgent, convID, origin.MessageID,
				origin.OpenCodeMessageID); cancelErr != nil {
				slog.Warn("OpenCode standing-order origin: cancel failed prompt handshake",
					"agent", targetAgent, "message", origin.MessageID,
					"error", cancelErr)
			}
		}
		return err
	}
	// A successful response proves acceptance. Give a reminder queued behind a
	// racing human prompt the same generous bound as an active turn. If SSE
	// already promoted (or even completed) the marker, the refresh correctly
	// misses and no delivery retry is needed.
	if refreshErr := db.RefreshPendingStandingOrderTurnOrigin(
		targetAgent, convID, origin.MessageID, origin.OpenCodeMessageID,
		time.Now(), standingOrderOriginActiveTTL); refreshErr != nil {
		slog.Debug("OpenCode standing-order origin: accepted prompt already advanced",
			"agent", targetAgent, "message", origin.MessageID, "error", refreshErr)
	}
	return nil
}

// sendOpenCodeTUICommand publishes a command through the managed server's TUI
// event API. Unlike tmux send-keys, command dispatch is independent of prompt
// mode and user keybinding customizations. expectedSessionID binds lifecycle
// sends to the selected process generation; empty is allowed for non-lifecycle
// callers that already selected by conversation.
func sendOpenCodeTUICommand(
	convID, expectedSessionID string,
	command openCodeTUICommand,
) error {
	runtime, err := readyOpenCodeRuntime(convID)
	if err != nil {
		return err
	}
	if expectedSessionID != "" && runtime.SessionID != expectedSessionID {
		return fmt.Errorf(
			"managed OpenCode runtime session changed for conversation %s", convID,
		)
	}
	// Health reconciliation can take long enough for an idle session to start
	// working or present a permission/question dialog after its caller's first
	// status check. Re-read immediately before the POST so a stale selection
	// cannot dispatch compact/exit into a newly non-idle TUI.
	if beforeOpenCodeTUICommandStatusCheckForTest != nil {
		beforeOpenCodeTUICommandStatusCheckForTest()
	}
	sess := aliveSessionForConv(convID)
	if sess == nil || sess.ID != runtime.SessionID {
		return fmt.Errorf("managed OpenCode session changed for conversation %s", convID)
	}
	if openCodeControlInputBlocked(sess.Status) {
		return fmt.Errorf("managed OpenCode TUI is %s; retry when idle", sess.Status)
	}
	body := map[string]any{
		"type": "tui.command.execute",
		"properties": map[string]string{
			"command": string(command),
		},
	}
	endpoint := runtime.ServerURL + "/tui/publish?directory=" +
		url.QueryEscape(runtime.Cwd)
	request, err := openCodeRequest(http.MethodPost, endpoint, *runtime, body)
	if err != nil {
		return fmt.Errorf("build OpenCode TUI command request: %w", err)
	}
	response, err := opencodeapi.Do(openCodeHTTPClient, request, *runtime)
	if err != nil {
		return fmt.Errorf("publish OpenCode TUI command: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("publish OpenCode TUI command: HTTP %d", response.StatusCode)
	}
	return nil
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

func stopOpenCodeRuntime(sessionID string) error {
	return stopOpenCodeRuntimeWithCgroupDisposition(sessionID, true)
}

// stopOpenCodeRuntimeForReplacement stops the current process and deletes its
// runtime authority while retaining the already-validated resource boundary
// that startOpenCodeRuntime is about to reuse for the replacement process.
func stopOpenCodeRuntimeForReplacement(sessionID string) error {
	return stopOpenCodeRuntimeWithCgroupDisposition(sessionID, false)
}

func stopOpenCodeRuntimeWithCgroupDisposition(sessionID string, removeCgroup bool) error {
	// Serialize teardown with reconcileOpenCodeRuntime. Without the shared lock,
	// reconcile could restart the server after stopOpenCodeProcess returned but
	// before this function checked liveness and deleted the durable claim.
	value, _ := openCodeReconcileLocks.LoadOrStore(sessionID, &sync.Mutex{})
	reconcileMu := value.(*sync.Mutex)
	reconcileMu.Lock()
	defer reconcileMu.Unlock()

	runtime, err := db.GetOpenCodeRuntime(sessionID)
	if err != nil {
		return err
	}
	if runtime == nil {
		return nil
	}
	stopOpenCodeProcess(*runtime, nil)
	if session.IsProcessAlive(runtime.PID) {
		if runtime.Transport == db.OpenCodeTransportUnixRelay {
			return fmt.Errorf(
				"OpenCode recovered process remains alive; retaining Unix replay authority")
		}
		return fmt.Errorf(
			"OpenCode recovered process remains alive; retaining runtime authority")
	}
	if runtime.Transport == db.OpenCodeTransportUnixRelay {
		if err := opencodeapi.RemoveUnixSocket(*runtime); err != nil {
			return fmt.Errorf("finish OpenCode Unix control cleanup: %w", err)
		}
	}
	if removeCgroup {
		if err := removeOpenCodeResourceCgroup(runtime.ResourceCgroupDir); err != nil {
			return fmt.Errorf("remove retired OpenCode resource cgroup: %w", err)
		}
	}
	clearOpenCodeVirtualCostState(sessionID)
	return db.DeleteOpenCodeRuntime(sessionID)
}

func stopOpenCodeProcess(runtime db.OpenCodeRuntime, known *openCodeProcess) {
	if runtime.ResourceCgroupDir != "" {
		// The tmux and process-tree kills below are best-effort: a server
		// descendant that double-forked or outlived the stop wait survives
		// them, but it can never leave the session's resource cgroup. Reap
		// whatever remains there so the durable boundary is empty for the
		// next relaunch and reclaimable at the next wake.
		defer func() {
			if err := session.KillResourceCgroupMembers(runtime.ResourceCgroupDir); err != nil {
				slog.Warn("OpenCode resource cgroup still holds processes after stop",
					"session", runtime.SessionID, "dir", runtime.ResourceCgroupDir, "error", err)
			}
		}()
	}
	removeControlSocket := true
	defer func() {
		if !removeControlSocket {
			return
		}
		if err := opencodeapi.RemoveUnixSocket(runtime); err != nil {
			slog.Warn("OpenCode Unix control socket cleanup refused",
				"session", runtime.SessionID, "error", err)
		}
	}()
	openCodeProcesses.Lock()
	process := known
	if process == nil {
		process = openCodeProcesses.bySession[runtime.SessionID]
	}
	if process == nil {
		process = &openCodeProcess{}
		openCodeProcesses.bySession[runtime.SessionID] = process
	}
	// Keep this tombstone registered until cancellation and projector join
	// complete. A concurrent health/reconcile path must not interpret a
	// temporarily missing entry as permission to launch a replacement SSE
	// consumer during teardown.
	process.stopping = true
	cancel := process.cancel
	sseDone := process.sseDone
	projectorStopped := sseDone == nil
	openCodeProcesses.Unlock()
	defer func() {
		openCodeProcesses.Lock()
		if openCodeProcesses.bySession[runtime.SessionID] == process {
			delete(openCodeProcesses.bySession, runtime.SessionID)
		}
		openCodeProcesses.Unlock()
		if projectorStopped {
			scheduleOpenCodeConversationStateCleanup(runtime)
		}
	}()
	if process != nil {
		if cancel != nil {
			cancel()
		}
		if sseDone != nil {
			// Cancellation interrupts the in-flight HTTP request/scanner and
			// every retry wait. Join the projector before clearing its
			// in-memory state or allowing a resume with the same conv_id;
			// otherwise the stale runtime can repopulate authoritative cost
			// and activity after teardown.
			select {
			case <-sseDone:
				projectorStopped = true
			case <-time.After(openCodeProcessStopWait):
				slog.Warn("OpenCode SSE projector did not stop before timeout",
					"session", runtime.SessionID, "timeout", openCodeProcessStopWait)
			}
		}
		if process.tmuxSession != "" {
			killErr := clcommon.Default.Command(
				"-N", "kill-session", "-t", clcommon.ExactTarget(process.tmuxSession)).Run()
			select {
			case <-process.done:
			case <-time.After(openCodeProcessStopWait):
				if !terminateOpenCodeProcessTree(runtime.PID, openCodeProcessStopWait) {
					removeControlSocket = false
					slog.Warn("OpenCode tmux process tree did not exit",
						"session", runtime.SessionID, "pid", runtime.PID, "tmux_error", killErr)
				}
				process.finish(killErr)
			}
			return
		}
		if process.cmd != nil && process.cmd.Process != nil {
			_ = process.cmd.Process.Signal(os.Interrupt)
			select {
			case <-process.done:
				return
			case <-time.After(openCodeProcessStopWait):
				_ = process.cmd.Process.Kill()
				select {
				case <-process.done:
				case <-time.After(openCodeProcessStopWait):
				}
				return
			}
		}
	}
	// No in-memory handle: this is a recovered PID (e.g. after an agentd
	// restart). Only kill it once it is positively identified as our managed
	// server via endpoint ownership. This closes the PID-reuse window the old
	// `>1` guard left open — a freed pid reassigned to an unrelated same-user
	// process no longer owns runtime.ServerURL, so it is left untouched. The
	// `!= os.Getpid()` guard is retained on top of ownership: subtree ownership
	// would match agentd's own pid (managed serves are its children), so if a
	// stale row's pid coincided with our own after reuse we must never self-kill.
	if runtime.PID > 1 && runtime.PID != os.Getpid() {
		switch {
		case openCodeRuntimeVerified(runtime):
			recordedTree := opencodeapi.RecordedProcessSubtree(runtime.PID)
			tmuxSession := openCodeManagedTmuxSession(runtime.SessionID)
			panePID := openCodeTmuxPanePID(tmuxSession)
			if panePID == runtime.PID {
				_ = clcommon.Default.Command(
					"-N", "kill-session", "-t", clcommon.ExactTarget(tmuxSession)).Run()
			} else {
				killOpenCodePIDs(recordedTree)
			}
			if !waitForOpenCodePIDsExit(recordedTree, openCodeProcessStopWait) {
				removeControlSocket = false
				slog.Warn("OpenCode recovered process tree did not exit; control authority retained",
					"session", runtime.SessionID, "pid", runtime.PID)
			}
		case session.IsProcessAlive(runtime.PID):
			// The pid is still alive but we cannot prove it is our managed server
			// (a wedged serve whose listener died, or the ownership probe being
			// unavailable on this platform). We refuse to kill an unproven pid,
			// but its runtime row is about to be deleted, so surface the possible
			// orphan rather than leaking it silently.
			slog.Warn("OpenCode recovered pid left running: endpoint ownership unproven",
				"session", runtime.SessionID, "pid", runtime.PID,
				"endpoint", runtime.ServerURL)
		}
	}
}

func killOpenCodePIDs(pids []int) {
	for i := len(pids) - 1; i >= 0; i-- {
		if recovered, err := os.FindProcess(pids[i]); err == nil {
			_ = recovered.Kill()
		}
	}
}

func terminateOpenCodeProcessTree(pid int, timeout time.Duration) bool {
	pids := opencodeapi.RecordedProcessSubtree(pid)
	killOpenCodePIDs(pids)
	return waitForOpenCodePIDsExit(pids, timeout)
}

func openCodeTmuxPanePID(tmuxSession string) int {
	out, err := clcommon.Default.Command(
		"-N", "display-message", "-p", "-t", clcommon.ExactTarget(tmuxSession)+":0.0",
		"#{pane_pid}").Output()
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return pid
}

func waitForOpenCodeRuntimeRelease(runtime db.OpenCodeRuntime, timeout time.Duration) bool {
	if runtime.Transport != db.OpenCodeTransportUnixRelay {
		return waitForOpenCodeEndpointRelease(runtime.ServerURL, timeout)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !session.IsProcessAlive(runtime.PID) &&
			openCodeControlPathAbsent(runtime.ControlSocketPath) {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return !session.IsProcessAlive(runtime.PID) &&
		openCodeControlPathAbsent(runtime.ControlSocketPath)
}

func openCodeControlPathAbsent(path string) bool {
	_, err := os.Lstat(path)
	return os.IsNotExist(err)
}

func waitForOpenCodePIDsExit(pids []int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		allExited := true
		for _, pid := range pids {
			if session.IsProcessAlive(pid) {
				allExited = false
				break
			}
		}
		if allExited {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func scheduleOpenCodeConversationStateCleanup(runtime db.OpenCodeRuntime) {
	if runtime.ConvID == "" {
		return
	}
	go func() {
		timer := time.NewTimer(openCodeConversationStateCleanupDelay)
		defer timer.Stop()
		<-timer.C
		cleared := false
		_ = withOpenCodeProjectorApplyLock(context.Background(), runtime, func() {
			openCodeProcesses.Lock()
			active := false
			for _, process := range openCodeProcesses.bySession {
				if process.convID == runtime.ConvID &&
					!process.stopping && !process.exited {
					active = true
					break
				}
			}
			openCodeProcesses.Unlock()
			if active {
				return
			}
			clearOpenCodeConversationStepState(runtime.ConvID)
			cleared = true
		})
		if cleared {
			deleteOpenCodeProjectorApplyLockIfIdle(runtime)
		}
	}()
}

func waitForOpenCodeEndpointRelease(endpoint string, timeout time.Duration) bool {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	deadline := time.Now().Add(timeout)
	for {
		listener, listenErr := net.Listen("tcp", parsed.Host)
		if listenErr == nil {
			_ = listener.Close()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func ensureOpenCodeSSE(runtime db.OpenCodeRuntime) {
	openCodeProcesses.Lock()
	process := openCodeProcesses.bySession[runtime.SessionID]
	if process == nil {
		process = &openCodeProcess{}
		openCodeProcesses.bySession[runtime.SessionID] = process
	}
	process.convID = runtime.ConvID
	if process.cancel != nil {
		openCodeProcesses.Unlock()
		return
	}
	if process.exited || process.stopping {
		// A dead server would spin the reconnect loop until reaping; a stopping
		// server must retain its registry tombstone until the old projector is
		// joined. In either state, starting a consumer is forbidden.
		openCodeProcesses.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	ctx = context.WithValue(ctx, openCodeSSEGenerationKey{}, process)
	sseDone := make(chan struct{})
	process.cancel = cancel
	process.sseDone = sseDone
	openCodeProcesses.Unlock()
	go func() {
		defer close(sseDone)
		consumeOpenCodeSSE(ctx, runtime)
	}()
}

func openCodeProjectorCurrent(ctx context.Context, sessionID string) bool {
	if ctx.Err() != nil {
		return false
	}
	generation, hasGeneration := ctx.Value(openCodeSSEGenerationKey{}).(*openCodeProcess)
	if !hasGeneration {
		// Direct unit/backfill callers are still bounded by their context.
		return true
	}
	openCodeProcesses.Lock()
	current := openCodeProcesses.bySession[sessionID]
	ok := current == generation && !generation.stopping && !generation.exited
	openCodeProcesses.Unlock()
	return ok
}

func openCodeProjectorApplyKey(runtime db.OpenCodeRuntime) string {
	if runtime.ConvID != "" {
		return "conv:" + runtime.ConvID
	}
	return "session:" + runtime.SessionID
}

func withOpenCodeProjectorApplyLock(
	ctx context.Context,
	runtime db.OpenCodeRuntime,
	apply func(),
) bool {
	key := openCodeProjectorApplyKey(runtime)
	openCodeProjectorApplyLocksMu.Lock()
	lock := openCodeProjectorApplyLocks[key]
	if lock == nil {
		lock = newOpenCodeProjectorApplyLock()
		openCodeProjectorApplyLocks[key] = lock
	}
	lock.users++
	openCodeProjectorApplyLocksMu.Unlock()
	defer func() {
		openCodeProjectorApplyLocksMu.Lock()
		lock.users--
		openCodeProjectorApplyLocksMu.Unlock()
	}()
	timer := time.NewTimer(openCodeProcessStopWait)
	select {
	case <-ctx.Done():
		timer.Stop()
		return false
	case <-timer.C:
		slog.Warn("OpenCode projector apply lock timed out",
			"session", runtime.SessionID, "conv_id", runtime.ConvID,
			"timeout", openCodeProcessStopWait)
		return false
	case <-lock.token:
		timer.Stop()
	}
	defer func() { lock.token <- struct{}{} }()
	apply()
	return true
}

func deleteOpenCodeProjectorApplyLockIfIdle(runtime db.OpenCodeRuntime) {
	key := openCodeProjectorApplyKey(runtime)
	openCodeProjectorApplyLocksMu.Lock()
	if lock := openCodeProjectorApplyLocks[key]; lock != nil && lock.users == 0 {
		delete(openCodeProjectorApplyLocks, key)
	}
	openCodeProjectorApplyLocksMu.Unlock()
}

// closeOpenCodeSSEBodyOnCancel actively interrupts Scanner.Scan when request
// context cancellation alone does not wake a custom or wedged transport. The
// Close runs in this watcher goroutine, so a pathological Close implementation
// cannot itself block process teardown.
func closeOpenCodeSSEBodyOnCancel(ctx context.Context, body io.ReadCloser) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if err := body.Close(); err != nil {
				slog.Debug("OpenCode SSE response close failed", "error", err)
			}
		case <-done:
		}
	}()
	return func() { close(done) }
}

func consumeOpenCodeSSE(ctx context.Context, runtime db.OpenCodeRuntime) {
	consumeOpenCodeSSEWithRetry(ctx, runtime, openCodeSSERetryDelay)
}

func consumeOpenCodeSSEWithRetry(ctx context.Context, runtime db.OpenCodeRuntime, retryDelay time.Duration) {
	endpoint := runtime.ServerURL + "/event?directory=" + url.QueryEscape(runtime.Cwd)
	projector := newOpenCodeEventProjector(runtime.ConvID, runtime.Cwd)
	for ctx.Err() == nil {
		request, err := openCodeRequest(http.MethodGet, endpoint, runtime, nil)
		if err == nil {
			request = request.WithContext(ctx)
			var response *http.Response
			response, err = opencodeapi.Do(openCodeSSEHTTPClient, request, runtime)
			if err == nil && response.StatusCode == http.StatusOK {
				stopBodyCancellation := closeOpenCodeSSEBodyOnCancel(ctx, response.Body)
				reconciled := withOpenCodeProjectorApplyLock(ctx, runtime, func() {
					if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
						return
					}
					// The stream is live before snapshots are read, so events that
					// race reconciliation remain buffered on this response and are
					// applied afterward in server order. Serializing snapshots with
					// event application also makes a replacement generation run
					// after any timed-out predecessor finishes its current write.
					if reconcileErr := reconcileOpenCodeSSE(ctx, runtime, projector); reconcileErr != nil {
						slog.Debug("OpenCode SSE reconciliation failed",
							"session", runtime.SessionID, "error", reconcileErr)
					}
					// TCL-673: seed context from message history so a resumed session
					// or a daemon restart is authoritative before its next live turn.
					if openCodeProjectorCurrent(ctx, runtime.SessionID) {
						backfillOpenCodeContextUsage(ctx, runtime)
					}
				})
				if !reconciled {
					stopBodyCancellation()
					_ = response.Body.Close()
					err = errors.New("OpenCode SSE reconciliation apply lock unavailable")
				} else {
					scanner := bufio.NewScanner(response.Body)
					scanner.Buffer(make([]byte, 64<<10), openCodeMaxSSEEventBytes)
					applyLockUnavailable := false
					for scanner.Scan() {
						line := scanner.Text()
						if strings.HasPrefix(line, "data:") {
							if !consumeOpenCodeEvent(ctx, runtime, projector,
								json.RawMessage(strings.TrimSpace(strings.TrimPrefix(line, "data:")))) {
								applyLockUnavailable = true
								break
							}
						}
					}
					stopBodyCancellation()
					_ = response.Body.Close()
					err = scanner.Err()
					if applyLockUnavailable {
						err = errors.New("OpenCode SSE event apply lock unavailable")
					}
				}
			} else if response != nil {
				err = fmt.Errorf("OpenCode SSE returned HTTP %d", response.StatusCode)
				_ = response.Body.Close()
			}
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Debug("OpenCode SSE disconnected; retrying",
				"session", runtime.SessionID, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryDelay):
		}
	}
}

func reconcileOpenCodeSSE(
	ctx context.Context,
	runtime db.OpenCodeRuntime,
	projector *openCodeEventProjector,
) error {
	statuses := make(map[string]openCodeSessionStatus)
	var questions []openCodeQuestionRequest
	var permissions []openCodePermissionRequest
	for _, snapshot := range []struct {
		path   string
		target any
	}{
		{path: "/session/status", target: &statuses},
		{path: "/question", target: &questions},
		{path: "/permission", target: &permissions},
	} {
		endpoint := runtime.ServerURL + snapshot.path +
			"?directory=" + url.QueryEscape(runtime.Cwd)
		request, err := openCodeRequest(http.MethodGet, endpoint, runtime, nil)
		if err != nil {
			return err
		}
		request = request.WithContext(ctx)
		response, err := opencodeapi.Do(openCodeHTTPClient, request, runtime)
		if err != nil {
			return err
		}
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			return fmt.Errorf("OpenCode snapshot %s returned HTTP %d",
				snapshot.path, response.StatusCode)
		}
		err = json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(snapshot.target)
		_ = response.Body.Close()
		if err != nil {
			return fmt.Errorf("decode OpenCode snapshot %s: %w", snapshot.path, err)
		}
	}
	projector.resetToolsForSnapshot()

	// Attention snapshots override "busy". OpenCode reports a session waiting
	// on a question or permission as busy, so applying status first would
	// briefly erase the more useful state and re-notify on every reconnect.
	for _, permission := range permissions {
		if projected := projector.projectPermission(permission); len(projected) > 0 {
			applyOpenCodeHooks(ctx, runtime, projector, projected)
			return nil
		}
	}
	for _, question := range questions {
		if projected := projector.projectQuestion(question); len(projected) > 0 {
			applyOpenCodeHooks(ctx, runtime, projector, projected)
			return nil
		}
	}
	projector.pendingAttention = false
	if status, ok := statuses[runtime.ConvID]; ok {
		// Force the authoritative snapshot through even when its OpenCode
		// status equals the pre-disconnect value. The tclaude state may still
		// be awaiting a permission/question that was answered while offline.
		if projected := projector.projectStatus(status, true); len(projected) > 0 {
			applyOpenCodeHooks(ctx, runtime, projector, projected)
			return nil
		}
	}
	// OpenCode 1.18.4 may omit an idle session from /session/status. Empty
	// attention snapshots plus no usable status therefore mean "not blocked
	// and not known busy": settle to idle. The SSE stream is already open, so
	// genuine concurrent work is buffered and immediately reasserts busy.
	applyOpenCodeHooks(ctx, runtime, projector,
		projector.projectStatus(openCodeSessionStatus{Type: "idle"}, true))
	return nil
}

func consumeOpenCodeEvent(
	ctx context.Context,
	runtime db.OpenCodeRuntime,
	projector *openCodeEventProjector,
	event json.RawMessage,
) bool {
	return withOpenCodeProjectorApplyLock(ctx, runtime, func() {
		consumeOpenCodeEventLocked(ctx, runtime, projector, event)
	})
}

func consumeOpenCodeEventLocked(
	ctx context.Context,
	runtime db.OpenCodeRuntime,
	projector *openCodeEventProjector,
	event json.RawMessage,
) {
	if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return
	}
	removedSessions := observeOpenCodeSessionTree(runtime, event)
	applyOpenCodeSessionDeletion(ctx, runtime, removedSessions)
	eventSessionID := openCodeRawEventSessionID(event)
	costSessionTracked := openCodeSessionTracked(runtime, eventSessionID)
	observeOpenCodeStandingOrderAssistant(runtime, projector, event)
	projected, err := projector.project(event)
	if native, ok := projector.takeNativeHook(); ok {
		applyOpenCodeHooks(ctx, runtime, projector,
			[]session.HookCallbackInput{native})
	}
	if err != nil {
		slog.Debug("OpenCode SSE event could not be projected",
			"session", runtime.SessionID, "error", err)
	} else {
		applyOpenCodeHooks(ctx, runtime, projector, projected)
	}
	// A tool-using assistant message can contain several model calls. OpenCode
	// publishes each call's authoritative token block as a step-finish part;
	// retain it before the following message.updated event supplies model
	// metadata and triggers the aggregate WHAT-IF projection.
	if costSessionTracked {
		if step, ok := parseOpenCodeStepCostUsage(event, eventSessionID); ok {
			applyOpenCodeVirtualCostStep(ctx, runtime, step)
		}
	}
	if costSessionTracked {
		if removal, ok := parseOpenCodeCostRemoval(event, eventSessionID); ok {
			applyOpenCodeVirtualCostRemoval(ctx, runtime, removal)
		}
	}
	if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return
	}
	// Context-window usage rides on the same directory-wide SSE stream as the
	// lifecycle hooks but is a session-row side effect, not a hook event, so it
	// is projected independently of the lifecycle projector, and after it so the
	// current event's status transition is applied before any model-limit fetch.
	// A cold-cache fetch (bounded to one per cache TTL and cancelled with ctx)
	// can briefly delay subsequent buffered events; in practice the local
	// managed server resolves the limit in milliseconds. See TCL-701.
	if costSessionTracked {
		if usage, ok := parseOpenCodeContextUsage(event, eventSessionID); ok {
			if eventSessionID == runtime.ConvID {
				persistOpenCodeContextUsage(ctx, runtime, usage)
				// TCL-673: record the provider/model slug from the root message so
				// child model calls do not replace the parent's dashboard model.
				persistOpenCodeRuntimeMetadata(runtime, usage)
			}
			// TCL-708: the same authoritative per-message usage drives the native
			// catalog what-if projection and provider-aware Usage coverage index.
			applyOpenCodeVirtualCostUsage(ctx, runtime, usage)
		}
	}
	if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return
	}
	// TCL-673: OpenCode's own cumulative session cost rides session.updated.
	// $0/N-A on a subscription; real spend on a pay-per-token key.
	applyOpenCodeCost(runtime, event)
}

func observeOpenCodeStandingOrderAssistant(
	runtime db.OpenCodeRuntime,
	projector *openCodeEventProjector,
	event json.RawMessage,
) {
	if projector == nil || !bytes.Contains(event, []byte(`"message.updated"`)) {
		return
	}
	var envelope openCodeEventEnvelope
	if err := json.Unmarshal(event, &envelope); err != nil ||
		envelope.Type != "message.updated" ||
		envelope.Properties.Info.SessionID != runtime.ConvID ||
		envelope.Properties.Info.Role != "assistant" ||
		envelope.Properties.Info.ParentID == "" {
		return
	}
	agentID, err := db.AgentIDForConv(runtime.ConvID)
	if err != nil || agentID == "" {
		if err != nil {
			slog.Warn("OpenCode standing-order origin: resolve assistant owner failed",
				"conv", runtime.ConvID, "error", err)
		}
		return
	}
	activated, err := db.ActivateStandingOrderTurnOrigin(
		agentID, runtime.ConvID, envelope.Properties.Info.ParentID,
		time.Now(), standingOrderOriginActiveTTL)
	if err != nil {
		slog.Warn("OpenCode standing-order origin: correlate assistant turn failed",
			"agent", agentID, "conv", runtime.ConvID, "error", err)
		return
	}
	if activated {
		projector.standingOrderTurn = true
	}
}

type openCodePromptTurnState int

const (
	openCodePromptTurnMissing openCodePromptTurnState = iota
	openCodePromptTurnPending
	openCodePromptTurnActive
	openCodePromptTurnCompleted
)

func openCodePromptTurnHistory(
	runtime db.OpenCodeRuntime,
	messageID string,
) (openCodePromptTurnState, error) {
	endpoint := runtime.ServerURL + "/session/" + url.PathEscape(runtime.ConvID) +
		"/message?directory=" + url.QueryEscape(runtime.Cwd)
	request, err := openCodeRequest(http.MethodGet, endpoint, runtime, nil)
	if err != nil {
		return openCodePromptTurnMissing, err
	}
	response, err := opencodeapi.Do(openCodeHTTPClient, request, runtime)
	if err != nil {
		return openCodePromptTurnMissing,
			fmt.Errorf("reconcile OpenCode prompt history: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return openCodePromptTurnMissing, fmt.Errorf(
			"reconcile OpenCode prompt history: HTTP %d", response.StatusCode)
	}
	var messages []struct {
		Info struct {
			ID       string `json:"id"`
			Role     string `json:"role"`
			ParentID string `json:"parentID"`
			Time     struct {
				Completed *int64 `json:"completed"`
			} `json:"time"`
		} `json:"info"`
	}
	if err := json.NewDecoder(io.LimitReader(
		response.Body, 64<<20)).Decode(&messages); err != nil {
		return openCodePromptTurnMissing,
			fmt.Errorf("decode OpenCode prompt history: %w", err)
	}
	state := openCodePromptTurnMissing
	for _, message := range messages {
		switch {
		case message.Info.Role == "user" && message.Info.ID == messageID:
			if state == openCodePromptTurnMissing {
				state = openCodePromptTurnPending
			}
		case message.Info.Role == "assistant" &&
			message.Info.ParentID == messageID:
			if message.Info.Time.Completed != nil {
				return openCodePromptTurnCompleted, nil
			}
			state = openCodePromptTurnActive
		}
	}
	return state, nil
}

func reconcileOpenCodeStandingOrderOrigin(
	runtime db.OpenCodeRuntime,
	projector *openCodeEventProjector,
	agentID string,
	origin *db.StandingOrderTurnOrigin,
) (bool, bool) {
	if projector == nil || origin == nil ||
		origin.State != db.StandingOrderTurnOriginPending ||
		runtime.ServerURL == "" {
		return false, true
	}
	state, err := openCodePromptTurnHistory(runtime, origin.OpenCodeMessageID)
	if err != nil {
		slog.Warn("OpenCode standing-order origin: history reconciliation failed",
			"agent", agentID, "conv", runtime.ConvID, "error", err)
		return false, true
	}
	switch state {
	case openCodePromptTurnActive:
		activated, activateErr := db.ActivateStandingOrderTurnOrigin(
			agentID, runtime.ConvID, origin.OpenCodeMessageID,
			time.Now(), standingOrderOriginActiveTTL)
		if activateErr != nil {
			slog.Warn("OpenCode standing-order origin: history activation failed",
				"agent", agentID, "conv", runtime.ConvID, "error", activateErr)
			return false, true
		}
		projector.standingOrderTurn = activated
		return activated, !activated
	case openCodePromptTurnCompleted:
		if cancelErr := db.CancelPendingStandingOrderTurnOrigin(
			agentID, runtime.ConvID, origin.MessageID,
			origin.OpenCodeMessageID); cancelErr != nil {
			slog.Warn("OpenCode standing-order origin: completed history cleanup failed",
				"agent", agentID, "conv", runtime.ConvID, "error", cancelErr)
			return false, true
		}
		return false, false
	default:
		return false, true
	}
}

func applyOpenCodeHooks(
	ctx context.Context,
	runtime db.OpenCodeRuntime,
	projector *openCodeEventProjector,
	projected []session.HookCallbackInput,
) {
	agentID, agentErr := db.AgentIDForConv(runtime.ConvID)
	originUncertain := agentErr != nil
	if agentErr != nil {
		slog.Warn("OpenCode standing-order origin: resolve stable agent failed",
			"conv", runtime.ConvID, "error", agentErr)
	}
	if projector != nil && agentID != "" {
		origin, err := db.GetStandingOrderTurnOrigin(
			agentID, runtime.ConvID, time.Now())
		if err != nil {
			slog.Warn("OpenCode standing-order origin: restore turn failed",
				"agent", agentID, "error", err)
			originUncertain = true
		} else {
			projector.standingOrderTurn =
				origin != nil && origin.State == db.StandingOrderTurnOriginActive
			// Pending means an accepted prompt may be queued behind another
			// turn. Suppress all boundaries fail-closed until its exact
			// assistant parent arrives; unrelated Stop events cannot clear it.
			originUncertain =
				origin != nil && origin.State == db.StandingOrderTurnOriginPending
			if len(projected) > 0 && origin != nil &&
				origin.State == db.StandingOrderTurnOriginPending {
				_, uncertain := reconcileOpenCodeStandingOrderOrigin(
					runtime, projector, agentID, origin)
				originUncertain = uncertain
			}
		}
	}
	for _, input := range projected {
		if projector != nil {
			input.StandingOrderOrigin =
				projector.standingOrderTurn || originUncertain
		} else if originUncertain {
			input.StandingOrderOrigin = true
		}
		if input.StandingOrderOnly {
			deliverOpenCodeStandingOrders(input, runtime.SessionID)
			continue
		}
		deadline := time.Now().Add(openCodeHookRowWait)
		applied := false
		for {
			if ctx.Err() != nil {
				return
			}
			err := session.ApplyHook(input, runtime.SessionID)
			if err == nil {
				applied = true
				deliverOpenCodeStandingOrders(input, runtime.SessionID)
				break
			}
			if !errors.Is(err, sql.ErrNoRows) || time.Now().After(deadline) {
				slog.Debug("OpenCode status event could not be applied",
					"session", runtime.SessionID, "event", input.HookEventName, "error", err)
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(openCodeHookRowRetryDelay):
			}
		}
		if applied && projector != nil && projector.standingOrderTurn &&
			openCodeTurnEnd(input.HookEventName) {
			if err := db.CompleteStandingOrderTurnOrigin(
				agentID, runtime.ConvID); err != nil {
				slog.Warn("OpenCode standing-order origin: clear turn failed",
					"agent", agentID, "event", input.HookEventName, "error", err)
			} else {
				projector.standingOrderTurn = false
			}
		}
	}
}

func openCodeTurnEnd(event string) bool {
	return event == "Stop" || event == "StopFailure"
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
