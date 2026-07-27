package agentd

import (
	"bufio"
	"context"
	"crypto/rand"
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
	"strconv"
	"strings"
	"sync"
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
	SessionID string
	ConvID    string
	ServerURL string
	Password  string
	PID       int
}

type openCodeTUICommand string

const (
	openCodeTUICompact openCodeTUICommand = "session.compact"
	openCodeTUIExit    openCodeTUICommand = "app.exit"
)

type openCodeProcess struct {
	cmd     *exec.Cmd
	done    chan error
	cancel  context.CancelFunc
	sseDone chan struct{}
	convID  string
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
	resolveOpenCodeTclaudeLayer  = session.ResolveTclaudeLayerServer
	wrapOpenCodeTclaudeLayer     = session.WrapTclaudeLayerServerSpec
)

func startOpenCodeRuntime(
	sessionID, cwd, title, resumeID, permissionJSON string,
	sandboxSpec *session.TclaudeLayerLaunchSpec,
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
	sandboxImplementation, sandboxSpecJSON, err := openCodeSandboxRecord(sandboxSpec)
	if err != nil {
		return nil, err
	}
	existing, err := db.GetOpenCodeRuntime(sessionID)
	if err != nil {
		return nil, fmt.Errorf("look up OpenCode runtime: %w", err)
	}
	if existing != nil {
		// OpenCode's permission paths and API instance are both rooted in cwd.
		// Never reuse a healthy endpoint for a different directory identity:
		// patching a policy compiled for cwd B through cwd A would be ambiguous
		// and could silently target the wrong session instance.
		sameCwd := strings.TrimSpace(existing.Cwd) != "" && existing.Cwd == cwd
		existingImplementation, implementationErr := normalizeOpenCodeRuntimeSandboxImplementation(
			existing.SandboxImplementation)
		sameSandbox := implementationErr == nil &&
			string(existingImplementation) == sandboxImplementation &&
			existing.SandboxLaunchSpecJSON == sandboxSpecJSON
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
			return &openCodeLaunch{
				SessionID: sessionID, ConvID: existing.ConvID,
				ServerURL: existing.ServerURL, Password: existing.Password,
				PID: existing.PID,
			}, nil
		}
		_ = stopOpenCodeRuntime(sessionID)
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
		}
		process, err := startOpenCodeProcess(runtime, sandboxSpec)
		if err != nil {
			lastErr = err
			continue
		}
		runtime.PID = process.cmd.Process.Pid
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
		return &openCodeLaunch{
			SessionID: sessionID, ConvID: runtime.ConvID,
			ServerURL: runtime.ServerURL, Password: runtime.Password,
			PID: runtime.PID,
		}, nil
	}
	return nil, fmt.Errorf("start OpenCode server after 3 port attempts: %w", lastErr)
}

func openCodeSandboxRecord(
	spec *session.TclaudeLayerLaunchSpec,
) (string, string, error) {
	if spec == nil {
		return string(sandboxpolicy.ImplementationHarnessBuiltin), "", nil
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
	implementation, err := normalizeOpenCodeRuntimeSandboxImplementation(
		runtime.SandboxImplementation)
	if err != nil {
		return nil, fmt.Errorf("OpenCode runtime sandbox implementation: %w", err)
	}
	switch implementation {
	case sandboxpolicy.ImplementationHarnessBuiltin:
		if strings.TrimSpace(runtime.SandboxLaunchSpecJSON) != "" {
			return nil, fmt.Errorf(
				"OpenCode harness-builtin runtime unexpectedly carries a tclaude-layer launch spec")
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
		spec.Version != session.TclaudeLayerLegacyLaunchSpecVersion {
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
	if spec.Version == session.TclaudeLayerLaunchSpecVersion {
		if err := validateOpenCodeV3LaunchContract(spec.Contract); err != nil {
			return nil, err
		}
	}
	cwd := canonicalOpenCodeRuntimePath(runtime.Cwd)
	hasCwd := false
	for _, path := range spec.Contract.WriteDirs {
		if canonicalOpenCodeRuntimePath(path) == cwd {
			hasCwd = true
			break
		}
	}
	if cwd == "" || !hasCwd {
		return nil, fmt.Errorf(
			"OpenCode tclaude-layer launch spec does not preserve runtime cwd %q as a writable contract path",
			runtime.Cwd)
	}
	snapshot := sandboxpolicy.NewSnapshot(spec.Effective, nil)
	revalidated, err := sandboxpolicy.RevalidateSnapshot(snapshot)
	if err != nil {
		return nil, fmt.Errorf("revalidate OpenCode tclaude-layer launch spec: %w", err)
	}
	spec.Effective = revalidated.Effective
	posture, err := sandboxpolicy.NetworkPostureForAccess(spec.Effective.NetworkAccess)
	if err != nil {
		return nil, err
	}
	if posture != sandboxpolicy.NetworkHostOpen {
		return nil, fmt.Errorf(
			"unsupported_sandbox_profile_network: OpenCode tclaude-layer restart requires the host-open loopback control plane and endpoint-ownership proof")
	}
	if err := session.ValidateTclaudeLayerLaunchSpec(spec); err != nil {
		return nil, fmt.Errorf("validate OpenCode tclaude-layer renderer contract: %w", err)
	}
	return &spec, nil
}

func canonicalOpenCodeRuntimePath(path string) string {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || !filepath.IsAbs(path) {
		return ""
	}
	return path
}

func startOpenCodeProcess(
	runtime db.OpenCodeRuntime,
	sandboxSpec *session.TclaudeLayerLaunchSpec,
) (*openCodeProcess, error) {
	if sandboxSpec != nil {
		if err := session.PrepareTclaudeLayerHarnessState(*sandboxSpec); err != nil {
			return nil, fmt.Errorf("prepare OpenCode tclaude-layer state: %w", err)
		}
	}
	executable, err := harness.OpenCodeExecutable()
	if err != nil {
		return nil, fmt.Errorf("find OpenCode executable: %w", err)
	}
	parsed, err := url.Parse(runtime.ServerURL)
	if err != nil {
		return nil, err
	}
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return nil, fmt.Errorf("parse OpenCode server endpoint: %w", err)
	}
	command, args, err := openCodeServeExec(executable, port, sandboxSpec)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(command, args...)
	cmd.Dir = runtime.Cwd
	cmd.Env = openCodeServerEnvironment(os.Environ(), sandboxSpec)
	cmd.Env = append(cmd.Env,
		"OPENCODE_SERVER_USERNAME="+openCodeServerUsername,
		"OPENCODE_SERVER_PASSWORD="+runtime.Password)
	cmd.Stdout = io.Discard
	stderr := newSpawnStderrCapture()
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	process := &openCodeProcess{cmd: cmd, done: make(chan error, 1), convID: runtime.ConvID}
	openCodeProcesses.Lock()
	openCodeProcesses.bySession[runtime.SessionID] = process
	openCodeProcesses.Unlock()
	go func() {
		err := cmd.Wait()
		process.done <- err
		close(process.done)
		finishOpenCodeProcessExit(process, runtime.SessionID, cmd.Process.Pid, err, stderr)
	}()
	runtime.PID = cmd.Process.Pid
	deadline := time.Now().Add(openCodeStartupTimeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-process.done:
			if err == nil {
				err = fmt.Errorf("server exited before health check")
			}
			return nil, fmt.Errorf("OpenCode server failed during startup: %w: %s", err, stderr.String())
		default:
		}
		// Port allocation necessarily has a bind-close-exec gap because
		// OpenCode does not accept a pre-bound listener. Never disclose the
		// password to that endpoint until the launched PID (or a child) is
		// positively observed owning its listening socket.
		if openCodeHealthy(runtime) {
			return process, nil
		}
		select {
		case err := <-process.done:
			if err == nil {
				err = fmt.Errorf("server exited before health check")
			}
			return nil, fmt.Errorf("OpenCode server failed during startup: %w: %s", err, stderr.String())
		case <-time.After(100 * time.Millisecond):
		}
	}
	stopOpenCodeProcess(runtime, process)
	return nil, fmt.Errorf("OpenCode server at %s did not become healthy within %s",
		runtime.ServerURL, openCodeStartupTimeout)
}

func openCodeServerEnvironment(
	ambient []string,
	sandboxSpec *session.TclaudeLayerLaunchSpec,
) []string {
	if sandboxSpec == nil {
		return append([]string(nil), ambient...)
	}
	privateState := len(sandboxSpec.Contract.Environment) > 0
	out := make([]string, 0, len(ambient)+len(sandboxSpec.Effective.Environment)+
		len(sandboxSpec.Contract.Environment))
	for _, entry := range ambient {
		if privateState && openCodePrivateEnvironmentName(strings.SplitN(entry, "=", 2)[0]) {
			continue
		}
		out = append(out, entry)
	}
	for _, entry := range sandboxSpec.Effective.Environment {
		if privateState && openCodePrivateEnvironmentName(entry.Name) {
			continue
		}
		out = append(out, entry.Name+"="+entry.Value)
	}
	for _, entry := range sandboxSpec.Contract.Environment {
		out = append(out, entry.Name+"="+entry.Value)
	}
	return out
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
	for i, name := range expectedNames {
		entry := contract.Environment[i]
		if entry.Name != name {
			return fmt.Errorf("private OpenCode v3 launch contract environment %d is %q, want %q",
				i, entry.Name, name)
		}
		wantStateDir := filepath.Join(entry.Value, "opencode")
		if canonicalOpenCodeRuntimePath(contract.StateDirs[i]) !=
			canonicalOpenCodeRuntimePath(wantStateDir) {
			return fmt.Errorf("private OpenCode v3 %s does not target state directory %q",
				name, contract.StateDirs[i])
		}
	}
	stateRoot := canonicalOpenCodeRuntimePath(contract.StateRoot)
	if stateRoot == "" || !openCodeAgentIDRE.MatchString(filepath.Base(stateRoot)) {
		return fmt.Errorf("private OpenCode v3 state root %q does not end in a stable agent id",
			contract.StateRoot)
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
	configTarget := canonicalOpenCodeRuntimePath(contract.StateDirs[2])
	configReadOnly := false
	for _, bind := range contract.ReadOnlyBinds {
		if canonicalOpenCodeRuntimePath(bind.Target) == configTarget {
			configReadOnly = true
		}
	}
	if !configReadOnly {
		return fmt.Errorf("private OpenCode v3 launch contract does not bind global config read-only")
	}
	return nil
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
	if sandboxSpec.Effective.NetworkAccess != sandboxpolicy.NetworkAccessInherit &&
		sandboxSpec.Effective.NetworkAccess != sandboxpolicy.NetworkAccessInternet {
		return "", nil, fmt.Errorf(
			"unsupported_sandbox_profile_network: OpenCode tclaude-layer requires the host-open loopback control plane and endpoint-ownership proof",
		)
	}
	bwrapBinary, _, err := resolveOpenCodeTclaudeLayer(sandboxpolicy.NetworkHostOpen)
	if err != nil {
		return "", nil, err
	}
	serveCommand := clcommon.ShellQuoteArg(executable)
	for _, arg := range serveArgs {
		serveCommand += " " + clcommon.ShellQuoteArg(arg)
	}
	wrapped, err := wrapOpenCodeTclaudeLayer(bwrapBinary, *sandboxSpec, serveCommand)
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
	network := sandboxpolicy.NetworkAccessInherit
	if snapshot != nil {
		network = snapshot.Effective.NetworkAccess
	}
	posture, err := sandboxpolicy.NetworkPostureForAccess(network)
	if err != nil {
		return nil, err
	}
	if posture != sandboxpolicy.NetworkHostOpen {
		return nil, fmt.Errorf(
			"unsupported_sandbox_profile_network: OpenCode tclaude-layer requires host-open networking because agentd and the attach pane use its authenticated loopback control plane and endpoint-ownership proof",
		)
	}
	allocation, err := requireOpenCodeStateAllocation(agentID)
	if err != nil {
		return nil, err
	}
	layout, err := openCodeStateLayoutForAllocation(*allocation)
	if err != nil {
		return nil, err
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
		openCodeProcessOwnsEndpoint(runtime.PID, runtime.ServerURL)
}

func openCodeHealthy(runtime db.OpenCodeRuntime) bool {
	request, err := openCodeRequest(http.MethodGet,
		runtime.ServerURL+"/global/health", runtime, nil)
	if err != nil {
		return false
	}
	response, err := openCodeHealthHTTPClient.Do(request)
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
	response, err := openCodeHTTPClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("create OpenCode session: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
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
	response, err := openCodeHTTPClient.Do(request)
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
	response, err := openCodeHTTPClient.Do(request)
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

func sendOpenCodePrompt(launch *openCodeLaunch, cwd, prompt, model, effort string) error {
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
	endpoint := launch.ServerURL + "/session/" + url.PathEscape(launch.ConvID) +
		"/prompt_async?directory=" + url.QueryEscape(cwd)
	request, err := openCodeRequest(http.MethodPost, endpoint, db.OpenCodeRuntime{
		PID: launch.PID, ServerURL: launch.ServerURL, Password: launch.Password,
	}, body)
	if err != nil {
		return err
	}
	response, err := openCodeHTTPClient.Do(request)
	if err != nil {
		return fmt.Errorf("submit OpenCode launch prompt: %w", err)
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
		SessionID: runtime.SessionID,
		ConvID:    runtime.ConvID,
		ServerURL: runtime.ServerURL,
		Password:  runtime.Password,
		PID:       runtime.PID,
	}, runtime.Cwd, nudge, "", "")
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
	response, err := openCodeHTTPClient.Do(request)
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
	stopOpenCodeProcess(*runtime, nil)
	if !waitForOpenCodeEndpointRelease(runtime.ServerURL, openCodeEndpointCloseWait) {
		slog.Error("OpenCode server endpoint remained occupied after stop",
			"session", sessionID, "endpoint", runtime.ServerURL)
		return false
	}
	process, err := startOpenCodeProcess(*runtime, sandboxSpec)
	if err != nil {
		slog.Error("OpenCode server restart failed", "session", sessionID, "error", err)
		return false
	}
	runtime.PID = process.cmd.Process.Pid
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

func stopOpenCodeRuntime(sessionID string) error {
	runtime, err := db.GetOpenCodeRuntime(sessionID)
	if err != nil {
		return err
	}
	if runtime == nil {
		return nil
	}
	stopOpenCodeProcess(*runtime, nil)
	clearOpenCodeVirtualCostState(sessionID)
	return db.DeleteOpenCodeRuntime(sessionID)
}

func stopOpenCodeProcess(runtime db.OpenCodeRuntime, known *openCodeProcess) {
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
			if recovered, err := os.FindProcess(runtime.PID); err == nil {
				_ = recovered.Kill()
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
			response, err = openCodeSSEHTTPClient.Do(request)
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
		response, err := openCodeHTTPClient.Do(request)
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
			applyOpenCodeHooks(ctx, runtime, projected)
			return nil
		}
	}
	for _, question := range questions {
		if projected := projector.projectQuestion(question); len(projected) > 0 {
			applyOpenCodeHooks(ctx, runtime, projected)
			return nil
		}
	}
	projector.pendingAttention = false
	if status, ok := statuses[runtime.ConvID]; ok {
		// Force the authoritative snapshot through even when its OpenCode
		// status equals the pre-disconnect value. The tclaude state may still
		// be awaiting a permission/question that was answered while offline.
		if projected := projector.projectStatus(status, true); len(projected) > 0 {
			applyOpenCodeHooks(ctx, runtime, projected)
			return nil
		}
	}
	// OpenCode 1.18.4 may omit an idle session from /session/status. Empty
	// attention snapshots plus no usable status therefore mean "not blocked
	// and not known busy": settle to idle. The SSE stream is already open, so
	// genuine concurrent work is buffered and immediately reasserts busy.
	applyOpenCodeHooks(ctx, runtime,
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
	projected, err := projector.project(event)
	if err != nil {
		slog.Debug("OpenCode SSE event could not be projected",
			"session", runtime.SessionID, "error", err)
	} else {
		applyOpenCodeHooks(ctx, runtime, projected)
	}
	// A tool-using assistant message can contain several model calls. OpenCode
	// publishes each call's authoritative token block as a step-finish part;
	// retain it before the following message.updated event supplies model
	// metadata and triggers the aggregate WHAT-IF projection.
	if step, ok := parseOpenCodeStepCostUsage(event, runtime.ConvID); ok {
		applyOpenCodeVirtualCostStep(ctx, runtime, step)
	}
	if removal, ok := parseOpenCodeCostRemoval(event, runtime.ConvID); ok {
		applyOpenCodeVirtualCostRemoval(ctx, runtime, removal)
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
	if usage, ok := parseOpenCodeContextUsage(event, runtime.ConvID); ok {
		persistOpenCodeContextUsage(ctx, runtime, usage)
		// TCL-673: record the provider/model slug from the same message so the
		// dashboard model column and cost-history denormalisation are populated.
		persistOpenCodeModelSlug(runtime, usage)
		// TCL-708: the same authoritative per-message usage drives the native
		// catalog what-if projection and provider-aware Usage coverage index.
		applyOpenCodeVirtualCostUsage(ctx, runtime, usage)
	}
	if !openCodeProjectorCurrent(ctx, runtime.SessionID) {
		return
	}
	// TCL-673: OpenCode's own cumulative session cost rides session.updated.
	// $0/N-A on a subscription; real spend on a pay-per-token key.
	applyOpenCodeCost(runtime, event)
}

func applyOpenCodeHooks(
	ctx context.Context,
	runtime db.OpenCodeRuntime,
	projected []session.HookCallbackInput,
) {
	for _, input := range projected {
		deadline := time.Now().Add(openCodeHookRowWait)
		for {
			if ctx.Err() != nil {
				return
			}
			err := session.ApplyHook(input, runtime.SessionID)
			if err == nil {
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
	}
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
