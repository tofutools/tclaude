package agentd

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

// liveSpawnNew runs `tclaude session new -d --global --label <label>`
// as a fully-detached subprocess. Same detachment story as
// liveSpawnResume — see its doc comment for the full rationale on
// why this doesn't trip CC's process-ownership checks.
//
// The label is the tclaude-side session ID (used to look up the row
// in SQLite once the conv-id materialises). It must be unique in the
// sessions table.
func liveSpawnNew(a clcommon.SpawnArgs) error {
	timing := config.StartupTiming("spawn_wrapper", "label", a.Label)
	defer timing("dispatch_return")
	label := a.Label
	// effort, model, sandbox, approval, autoReview and trustDir are validated at
	// the spawn boundary (handleGroupSpawn / the `agent spawn` CLI) before they
	// reach here; the forked `tclaude session new` re-validates too, though by
	// then a bad value would only surface as a non-zero exit in the daemon
	// log. sessionNewArgs omits each flag entirely when its value is "" / false.
	wrapper, err := startSpawnWrapper(a, sessionNewArgs, func() {})
	if err != nil {
		return err
	}
	cmd, stderr, cleanup, routeCredentialCleanup := wrapper.cmd, wrapper.stderr, wrapper.cleanup, wrapper.routeCredentialCleanup
	pid := cmd.Process.Pid
	timing("process_started", "child_pid", pid)
	waitStarted := time.Now()
	waitWrapper := func() error {
		err := cmd.Wait()
		// A separate trace: the asynchronous wait can overlap dispatch_return.
		mark := config.StartupTiming("spawn_wrapper_exit", "label", label, "child_pid", pid)
		mark("exited", "wait_ms", time.Since(waitStarted).Milliseconds(), "failed", err != nil)
		return err
	}
	if a.CwdWriteProof != "" || a.DirWriteProof != "" {
		defer cleanup()
		if err := waitWrapper(); err != nil {
			routeCredentialCleanup()
			slog.Error("spawn subprocess exited with error",
				"label", label, "pid", pid, "err", err,
				"stderr", stderr.String(), "stderr_truncated", stderr.Truncated())
			return fmt.Errorf("spawn session wrapper failed: %w: %s", err, stderr.String())
		}
		return nil
	}
	go func() {
		defer cleanup()
		if err := waitWrapper(); err != nil {
			routeCredentialCleanup()
			slog.Error("spawn subprocess exited with error",
				"label", label, "pid", pid, "err", err,
				"stderr", stderr.String(), "stderr_truncated", stderr.Truncated())
			// Report the death to the executeSpawn that forked us (if it is
			// still polling) so the failure surfaces to the caller instead of
			// timing out into a false success. See wrapperFailureSignals.
			signalWrapperFailure(label, fmt.Errorf("spawn session wrapper failed: %w: %s", err, stderr.String()))
		}
	}()
	return nil
}

func appendOpenCodeTransportEnvironment(cmd *exec.Cmd, a clcommon.SpawnArgs) {
	if a.OpenCodeTransport == "" {
		return
	}
	cmd.Env = append(cmd.Env, clcommon.OpenCodeTransportEnv+"="+a.OpenCodeTransport)
	if a.OpenCodeTransport != db.OpenCodeTransportUnixRelay {
		return
	}
	cmd.Env = append(cmd.Env,
		clcommon.OpenCodeControlSocketPathEnv+"="+a.OpenCodeControlSocketPath,
		clcommon.OpenCodeControlSocketDeviceEnv+"="+strconv.FormatInt(
			a.OpenCodeControlSocketDevice, 10),
		clcommon.OpenCodeControlSocketInodeEnv+"="+strconv.FormatInt(
			a.OpenCodeControlSocketInode, 10),
		clcommon.OpenCodeServerPIDEnv+"="+strconv.Itoa(a.OpenCodeServerPID),
	)
}

func openCodeAttachProcessEnvironment(environment []string) []string {
	out := make([]string, 0, len(environment))
	for _, entry := range environment {
		if openCodePrivateEnvironmentName(strings.SplitN(entry, "=", 2)[0]) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// liveSpawnResume runs `tclaude session new -r <conv> -d --global`
// as a fully-detached subprocess.
//
// Detachment story:
//   - `tclaude session new -d` only means "don't attach this terminal
//     to the new tmux session." The wrapper still runs in foreground
//     and inherits whatever stdio its parent gave it.
//   - We explicitly null stdio so nothing leaks back into the
//     daemon's logs.
//   - detachSpawn (unix-only) sets Setsid so the wrapper has its own
//     session and process group — no controlling tty inherited from
//     the daemon, and on daemon exit the wrapper gets reparented to
//     init/PID 1 cleanly.
//   - The actual CC process is parented to the long-running tmux
//     server (because `tclaude session new -d` shells out to
//     `tmux new-session -d ...` which forks the command as a child of
//     the tmux server, not of the caller). So the CC process never
//     has *us* in its ancestry chain — important so it doesn't trip
//     CC's own process-ownership / sandbox checks via parent walks.
//
// The wrapper is reaped synchronously. It exits immediately after tmux accepts
// or rejects the detached launch, so callers learn whether this resume won the
// launch reservation and can roll back refreshed actor state on failure.
func liveSpawnResume(a clcommon.SpawnArgs) error {
	if err := prepareRouteHelperResumeArgs(&a); err != nil {
		return err
	}
	privateAttachmentRootCreated := false
	resumeLaunched := false
	defer func() {
		if privateAttachmentRootCreated && !resumeLaunched {
			_ = os.Remove(tclcommon.SpawnAttachmentsPrivateDir(a.ConvID))
		}
	}()
	var openCodeLaunch *openCodeLaunch
	if a.Harness == harness.OpenCodeName {
		implementation, implementationErr := sandboxpolicy.NormalizeImplementation(
			a.SandboxImplementation,
		)
		if implementationErr != nil {
			return implementationErr
		}
		if implementation == sandboxpolicy.ImplementationTclaudeLayer {
			_, created, prepareErr :=
				tclcommon.PrepareSpawnAttachmentsPrivateDir(a.ConvID)
			if prepareErr != nil {
				return prepareErr
			}
			privateAttachmentRootCreated = created
		}
		agentID, identityErr := db.AgentIDForConv(a.ConvID)
		if identityErr != nil {
			return identityErr
		}
		// A spawn that resumes imported history has already started this
		// launch's managed server (its args carry it) and briefs the agent
		// through it after the fork. Starting a second one here would retire
		// that server (retirePriorOpenCodeResumeRuntime stops the runtime
		// serving this conversation) and lose the brief (tcl-gjmtvb). The
		// caller owns that server: it is not started, sent to or stopped here.
		if a.OpenCodeServerURL == "" {
			var err error
			openCodeLaunch, err = startManagedOpenCodeServer(&a, a.ConvID, agentID, "", a.ConvID, true)
			if err != nil {
				return err
			}
		}
	}
	stopOpenCode := func() {
		if openCodeLaunch != nil {
			_ = stopOpenCodeRuntime(openCodeLaunch.SessionID)
		}
	}
	convID := a.ConvID
	wrapper, err := startSpawnWrapper(a, sessionResumeArgs, stopOpenCode)
	if err != nil {
		return err
	}
	cmd, stderr, cleanup, routeCredentialCleanup := wrapper.cmd, wrapper.stderr, wrapper.cleanup, wrapper.routeCredentialCleanup
	pid := cmd.Process.Pid
	defer cleanup()
	if err := cmd.Wait(); err != nil {
		routeCredentialCleanup()
		stopOpenCode()
		slog.Error("resume subprocess exited with error",
			"conv", convID, "pid", pid, "err", err,
			"stderr", stderr.String(), "stderr_truncated", stderr.Truncated())
		return fmt.Errorf("resume session wrapper failed: %w: %s", err, stderr.String())
	}
	resumeLaunched = true
	if openCodeLaunch != nil {
		// OpenCode ignores the --initial-prompt the wrapper was handed: its
		// first turn of this launch (the managed standing guidance) goes over
		// the managed server, as spawn's does.
		deliverLaunchTextOverDrive(convID, a.InitialPrompt, openCodeLaunch, a.Cwd, a.Model, a.Effort)
	}
	return nil
}

// spawnWrapper is a started detached `tclaude session new` wrapper: the process,
// its bounded stderr capture, and the two handoff cleanups the caller still owns
// once it is running (the sandbox snapshot file and the route-helper credential
// rendezvous).
type spawnWrapper struct {
	cmd                    *exec.Cmd
	stderr                 *spawnStderrCapture
	cleanup                func()
	routeCredentialCleanup func()
}

// startSpawnWrapper is the setup liveSpawnNew and liveSpawnResume share: hand
// the sandbox snapshot and any route-helper credential off, build the wrapper
// command from argv(a) with the launch environment, detach it and start it.
// Every failure releases what was handed off so far and then runs onFail, which
// lets a resume stop the OpenCode runtime it started for this launch.
func startSpawnWrapper(a clcommon.SpawnArgs, argv func(clcommon.SpawnArgs) []string, onFail func()) (*spawnWrapper, error) {
	var cleanup func()
	var err error
	a, cleanup, err = spawnArgsWithSandboxHandoff(a)
	if err != nil {
		onFail()
		return nil, err
	}
	routeCredentialCleanup := func() {}
	if a.RouteHelperCredential != "" {
		a.RouteHelperCredentialHandoffSocketPath, routeCredentialCleanup, err = prepareRouteHelperCredentialHandoff(a.RouteHelperCredential)
		if err != nil {
			cleanup()
			onFail()
			return nil, err
		}
	}
	cmd := exec.Command("tclaude", argv(a)...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	// Capture stderr so a silent subprocess failure (PATH issue, bad
	// cwd, broken tmux server, etc.) shows up in the daemon log
	// instead of disappearing into /dev/null. Bounded so a runaway
	// child can't grow the buffer unboundedly.
	stderr := newSpawnStderrCapture()
	cmd.Stderr = stderr
	// Spawned agents must not inherit the human's operator token.
	cmd.Env = environmentWithCodexStateRoot(spawnEnvWithoutOperatorToken(), a.CodexStateRoot)
	if len(a.OpenCodeEnvironment) > 0 {
		cmd.Env = openCodeAttachProcessEnvironment(cmd.Env)
	}
	if a.OpenCodeServerURL != "" {
		cmd.Env = append(cmd.Env, "TCLAUDE_OPENCODE_SERVER_URL="+a.OpenCodeServerURL)
	}
	if a.OpenCodeServerPassword != "" {
		cmd.Env = append(cmd.Env, "OPENCODE_SERVER_PASSWORD="+a.OpenCodeServerPassword)
	}
	appendOpenCodeTransportEnvironment(cmd, a)
	for _, entry := range a.OpenCodeEnvironment {
		cmd.Env = append(cmd.Env, entry.Name+"="+entry.Value)
	}
	if a.OpenCodeStateIsolation != "" {
		cmd.Env = append(cmd.Env,
			clcommon.OpenCodeStateIsolationEnv+"="+a.OpenCodeStateIsolation)
	}
	detachSpawn(cmd)
	if err := cmd.Start(); err != nil {
		routeCredentialCleanup()
		cleanup()
		onFail()
		return nil, err
	}
	return &spawnWrapper{cmd: cmd, stderr: stderr, cleanup: cleanup, routeCredentialCleanup: routeCredentialCleanup}, nil
}

// prepareRouteHelperResumeArgs restores the namespace-local helper for every
// known resume surface (group resume, recovery, reincarnation, and clone).
// The resumed conversation already has a stable identity, so this path can
// support both Claude and Codex; OpenCode's server boundary has no pane-local
// helper and is refused when its group has opted into routes.
func prepareRouteHelperResumeArgs(a *clcommon.SpawnArgs) error {
	if (runtime.GOOS != "linux" && runtime.GOOS != "darwin") || a == nil || strings.TrimSpace(a.ConvID) == "" {
		return nil
	}
	// A clone may already have staged the exact destination-group contract.
	// Recomputing before its post-spawn memberships land would discard that plan.
	if a.RouteHelperCredential != "" {
		return nil
	}
	hostRoutesEnabled := false
	if hostPublishAllowed(a.ConvID) {
		reason := hostRouteHelperIneligibleReason(a.Harness, a.SandboxImplementation, a.EffectiveSandbox)
		hostRoutesEnabled = reason == ""
		if !hostRoutesEnabled {
			warnHostRouteHelperSkipped(a.ConvID, "resume", reason)
		}
	}
	if runtime.GOOS == "darwin" && hostRoutesEnabled {
		a.DarwinRouteCapable = true
	}
	if runtime.GOOS == "darwin" && !a.DarwinRouteCapable {
		return nil
	}
	groups, err := db.ListGroupsForConv(a.ConvID)
	if err != nil {
		return fmt.Errorf("resolve route-enabled groups for resume: %w", err)
	}
	var routeGroups []int64
	for _, group := range groups {
		enabled, enabledErr := db.IsAgentGroupRouteEnabled(group.ID, PermRoutesPublish, PermRoutesConsume)
		if enabledErr != nil {
			return fmt.Errorf("resolve route capability for group %d: %w", group.ID, enabledErr)
		}
		if enabled {
			routeGroups = append(routeGroups, group.ID)
		}
	}
	if hostRoutesEnabled {
		routeGroups = append(routeGroups, 0)
	}
	if len(routeGroups) == 0 {
		return nil
	}
	if a.SandboxImplementation != string(sandboxpolicy.ImplementationTclaudeLayer) || a.Harness == harness.OpenCodeName {
		return errors.New("linux group routes require a pane-authoritative resume with tclaude’s sandbox")
	}
	agentID, err := db.AgentIDForConv(a.ConvID)
	if err != nil || strings.TrimSpace(agentID) == "" {
		if err == nil {
			err = errors.New("stable agent identity is empty")
		}
		return fmt.Errorf("resolve route helper identity for resume: %w", err)
	}
	a.RouteHelperAgentID = agentID
	if a.DarwinRouteCapable {
		a.DarwinRouteAgentID = agentID
	}
	a.RouteHelperConvID = a.ConvID
	credential, generation, credentialErr := mintRouteHelperCredential(agentID, a.ConvID)
	if credentialErr != nil {
		return fmt.Errorf("mint route helper credential for resume: %w", credentialErr)
	}
	a.RouteHelperLaunchGeneration = generation
	a.RouteHelperCredential = credential
	a.RouteHelperGroupIDs = routeGroups
	a.RouteHelperProxyOnly = runtime.GOOS == "darwin"
	return nil
}

func spawnArgsWithSandboxHandoff(a clcommon.SpawnArgs) (clcommon.SpawnArgs, func(), error) {
	cleanup := func() {}
	if a.EffectiveSandbox == nil {
		return a, cleanup, nil
	}
	path, digest, err := sandboxpolicy.WriteSnapshotFile(config.DataDir(), *a.EffectiveSandbox)
	if err != nil {
		return clcommon.SpawnArgs{}, cleanup, err
	}
	a.SandboxSnapshotPath = path
	a.SandboxSnapshotDigest = digest
	cleanup = func() { _ = os.Remove(path) }
	return a, cleanup, nil
}

// spawnStderrCapture is a bounded io.Writer used for capturing
// subprocess stderr of detached `tclaude session new` invocations.
// Caps at spawnStderrMax bytes; further writes are silently dropped
// and Truncated() reports whether truncation happened. Concurrent
// writes are not expected (exec.Cmd has a single writer goroutine)
// but the mutex makes accidental concurrent String() calls safe.
const spawnStderrMax = 8 << 10

type spawnStderrCapture struct {
	buf       []byte
	truncated bool
}

func newSpawnStderrCapture() *spawnStderrCapture {
	return &spawnStderrCapture{buf: make([]byte, 0, 512)}
}

func (c *spawnStderrCapture) Write(p []byte) (int, error) {
	if c == nil {
		return len(p), nil
	}
	remaining := spawnStderrMax - len(c.buf)
	if remaining <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		c.buf = append(c.buf, p[:remaining]...)
		c.truncated = true
		return len(p), nil
	}
	c.buf = append(c.buf, p...)
	return len(p), nil
}

func (c *spawnStderrCapture) String() string {
	if c == nil {
		return ""
	}
	return strings.TrimRight(string(c.buf), "\r\n ")
}

func (c *spawnStderrCapture) Truncated() bool {
	if c == nil {
		return false
	}
	return c.truncated
}
