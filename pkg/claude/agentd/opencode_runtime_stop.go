package agentd

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/tmuxcmd"
	"github.com/tofutools/tclaude/pkg/claude/opencodeapi"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

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
	process, cancel, sseDone, projectorStopped := claimOpenCodeProcessForStop(runtime, known)
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
		if joinOpenCodeSSEProjector(runtime, cancel, sseDone) {
			projectorStopped = true
		}
		if process.tmuxSession != "" {
			if stopTmuxHeldOpenCodeProcess(runtime, process) {
				removeControlSocket = false
			}
			return
		}
		if process.cmd != nil && process.cmd.Process != nil {
			if stopDirectOpenCodeServer(runtime, process) {
				removeControlSocket = false
			}
			return
		}
	}
	if stopRecoveredOpenCodeProcess(runtime) {
		removeControlSocket = false
	}
}

// claimOpenCodeProcessForStop marks the session's process (the known one,
// the registered one, or a fresh tombstone) as stopping, and reads its SSE
// cancel and done handles under the registry lock.
func claimOpenCodeProcessForStop(runtime db.OpenCodeRuntime, known *openCodeProcess) (
	process *openCodeProcess, cancel context.CancelFunc, sseDone chan struct{}, projectorStopped bool,
) {
	openCodeProcesses.Lock()
	process = known
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
	cancel = process.cancel
	sseDone = process.sseDone
	projectorStopped = sseDone == nil
	openCodeProcesses.Unlock()
	return process, cancel, sseDone, projectorStopped
}

// joinOpenCodeSSEProjector cancels the SSE consumer and waits, bounded, for
// its projector to stop. stopped reports that it did.
func joinOpenCodeSSEProjector(
	runtime db.OpenCodeRuntime, cancel context.CancelFunc, sseDone chan struct{},
) (stopped bool) {
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
			stopped = true
		case <-time.After(openCodeProcessStopWait):
			slog.Warn("OpenCode SSE projector did not stop before timeout",
				"session", runtime.SessionID, "timeout", openCodeProcessStopWait)
		}
	}
	return stopped
}

// stopTmuxHeldOpenCodeProcess kills the managed tmux session that holds the
// server and, if the server does not exit, its process tree. retained reports
// that the tree survived, so the control authority must be kept.
func stopTmuxHeldOpenCodeProcess(runtime db.OpenCodeRuntime, process *openCodeProcess) (retained bool) {
	killErr := tmuxcmd.Local().NoStart().KillSession(tmuxcmd.Session(process.tmuxSession))
	select {
	case <-process.done:
	case <-time.After(openCodeProcessStopWait):
		if !terminateOpenCodeProcessTree(runtime.PID, openCodeProcessStopWait) {
			retained = true
			slog.Warn("OpenCode tmux process tree did not exit",
				"session", runtime.SessionID, "pid", runtime.PID, "tmux_error", killErr)
		}
		process.finish(killErr)
	}
	return retained
}

// stopDirectOpenCodeServer interrupts a server this daemon started, then
// retires its workload group and kills the process tree recorded before the
// interrupt. retained reports that the tree survived.
func stopDirectOpenCodeServer(runtime db.OpenCodeRuntime, process *openCodeProcess) (retained bool) {
	// Capture descendants before interrupting the bridge: its shell and
	// server can otherwise be reparented when the bridge exits.
	select {
	case <-process.done:
		process.killWorkloadGroup()
		return false
	default:
	}
	recordedTree := opencodeapi.RecordedProcessSubtree(process.cmd.Process.Pid)
	_ = process.cmd.Process.Signal(os.Interrupt)
	select {
	case <-process.done:
	case <-time.After(openCodeProcessStopWait):
	}
	// The group outlives a promptly exiting bridge while its workload
	// remains. Always retire it, including after a graceful leader exit.
	process.killWorkloadGroup()
	killOpenCodePIDs(recordedTree)
	if !waitForOpenCodePIDsExit(recordedTree, openCodeProcessStopWait) {
		retained = true
		slog.Warn("OpenCode process tree did not exit; control authority retained",
			"session", runtime.SessionID, "pid", runtime.PID)
	}
	return retained
}

// stopRecoveredOpenCodeProcess stops a recorded server this daemon has no
// handle for, killing it only once its endpoint ownership is proven.
// retained reports that a proven tree survived.
func stopRecoveredOpenCodeProcess(runtime db.OpenCodeRuntime) (retained bool) {
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
				_ = tmuxcmd.Local().NoStart().KillSession(tmuxcmd.Session(tmuxSession))
			} else {
				killOpenCodePIDs(recordedTree)
			}
			if !waitForOpenCodePIDsExit(recordedTree, openCodeProcessStopWait) {
				retained = true
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
	return retained
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
