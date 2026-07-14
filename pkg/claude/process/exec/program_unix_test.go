//go:build linux || darwin

package processexec

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	osexec "os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/plan"
)

const programTimeoutHelperMarker = "tclaude-program-timeout-helper"

func TestProgramAdapterTimesOutCommand(t *testing.T) {
	dir := t.TempDir()
	readyPath := dir + "/ready"
	pidPath := dir + "/descendant.pid"
	type controlledTimeout struct {
		duration time.Duration
		expire   context.CancelCauseFunc
	}
	timeoutStarted := make(chan controlledTimeout, 1)
	adapter := ProgramAdapter{
		TimeoutContext: func(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancelCause(parent)
			timeoutStarted <- controlledTimeout{duration: timeout, expire: cancel}
			return ctx, func() { cancel(context.Canceled) }
		},
	}

	type result struct {
		observation Observation
		err         error
	}
	resultCh := make(chan result, 1)
	testCtx := t.Context()
	go func() {
		observation, err := adapter.Perform(testCtx, Request{
			Command: plan.Command{ID: "cmd_timeout", IdempotencyKey: "run/timeout", RunID: "run"},
			Performer: model.Performer{
				Kind: model.PerformerProgram,
				Run:  os.Args[0],
				Args: []string{
					"-test.run=^TestProgramAdapterTimeoutHelperProcess$",
					"--", programTimeoutHelperMarker, readyPath, pidPath,
				},
				Timeout: "20ms",
			},
		})
		resultCh <- result{observation: observation, err: err}
	}()

	controlled := <-timeoutStarted
	t.Cleanup(func() { controlled.expire(context.DeadlineExceeded) })
	if controlled.duration != 20*time.Millisecond {
		t.Fatalf("program timeout = %s, want 20ms", controlled.duration)
	}
	descendantPID := waitForProgramTimeoutHelper(t, readyPath, pidPath)
	cleanupPID := descendantPID
	t.Cleanup(func() {
		if cleanupPID != 0 {
			_ = syscall.Kill(cleanupPID, syscall.SIGKILL)
		}
	})
	controlled.expire(context.DeadlineExceeded)

	var got result
	select {
	case got = <-resultCh:
	case <-time.After(10 * time.Second):
		t.Fatal("program adapter did not return after timeout")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.observation.Verdict != "fail" || got.observation.Evidence == nil {
		t.Fatalf("observation = %#v", got.observation)
	}
	var evidence ProgramEvidence
	if err := json.Unmarshal(got.observation.Evidence.Data, &evidence); err != nil {
		t.Fatal(err)
	}
	if !evidence.TimedOut || evidence.ExitCode == 0 {
		t.Fatalf("timeout evidence = %#v", evidence)
	}
	waitForProgramExit(t, descendantPID)
	cleanupPID = 0
}

func TestProgramAdapterTimeoutHelperProcess(t *testing.T) {
	args := flag.Args()
	if len(args) != 3 || args[0] != programTimeoutHelperMarker {
		return
	}
	readyPath, pidPath := args[1], args[2]
	descendant := osexec.Command("sleep", "3600")
	// Preserve the adapter's output pipes in the descendant. If process-group
	// cancellation regresses, Command.Wait remains blocked just as it did in
	// the original shell-based CI flake.
	descendant.Stdout = os.Stdout
	descendant.Stderr = os.Stderr
	if err := descendant.Start(); err != nil {
		t.Fatalf("start timeout descendant: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(descendant.Process.Pid)), 0o600); err != nil {
		t.Fatalf("write descendant pid: %v", err)
	}
	if err := os.WriteFile(readyPath, []byte("ready"), 0o600); err != nil {
		t.Fatalf("write ready marker: %v", err)
	}
	if err := descendant.Wait(); err != nil {
		t.Fatalf("timeout descendant exited before cancellation: %v", err)
	}
	t.Fatal("timeout descendant exited successfully before cancellation")
}

func waitForProgramTimeoutHelper(t *testing.T, readyPath, pidPath string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastState string
	for time.Now().Before(deadline) {
		ready, readyErr := os.ReadFile(readyPath)
		pidBytes, pidErr := os.ReadFile(pidPath)
		lastState = fmt.Sprintf("ready=%q readyErr=%v pid=%q pidErr=%v",
			strings.TrimSpace(string(ready)), readyErr, strings.TrimSpace(string(pidBytes)), pidErr)
		if readyErr == nil && strings.TrimSpace(string(ready)) == "ready" && pidErr == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
			if err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("program timeout helper did not become ready: %s", lastState)
	return 0
}

func waitForProgramExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		lastErr = syscall.Kill(pid, 0)
		if lastErr == syscall.ESRCH {
			return
		}
		if lastErr != nil {
			t.Fatalf("probe descendant process %d: %v", pid, lastErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("descendant process %d still exists after cancellation (last probe: %v)", pid, lastErr)
}
