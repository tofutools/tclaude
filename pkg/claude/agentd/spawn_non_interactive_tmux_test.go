package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

func TestOneShotExecHelperReturnsResultFromPrivateHandoff(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("tmux one-shot broker runs on Linux")
	}
	t.Setenv("HOME", t.TempDir())
	root := filepath.Join(config.DataDir(), "one-shot")
	dir := filepath.Join(root, "run-test")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	requestPath := filepath.Join(dir, "request.json")
	resultPath := filepath.Join(dir, "result.json")
	start, err := oneShotProcessStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	request := nonInteractiveBrokerRequest{
		Command: nonInteractiveCommand{
			Argv: []string{"/bin/sh", "-c", "printf 'broker-ok\\n'; exit 7"},
			Cwd:  t.TempDir(), Env: os.Environ(), TimeoutSeconds: 10,
		},
		Deadline:  time.Now().Add(10 * time.Second),
		DaemonPID: os.Getpid(), DaemonStartTime: start,
	}
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runOneShotExecHelper(requestPath, resultPath); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	var reply nonInteractiveBrokerReply
	if err := json.Unmarshal(data, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Failure != nil || reply.Result.Stdout != "broker-ok\n" || reply.Result.ExitCode != 7 {
		t.Fatalf("unexpected helper reply: %+v", reply)
	}
}

func TestOneShotTmuxSessionLaunchFailure(t *testing.T) {
	previous := launchNonInteractiveTmuxSession
	launchNonInteractiveTmuxSession = func(string, string, string, ...string) error {
		return fmt.Errorf("tmux unavailable")
	}
	t.Cleanup(func() { launchNonInteractiveTmuxSession = previous })
	command := nonInteractiveCommand{
		Argv: []string{"/bin/sh", "-c", "printf 'direct-ok\\n'"},
		Cwd:  t.TempDir(), Env: os.Environ(), TimeoutSeconds: 10,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, failure := runNonInteractiveThroughTmux(ctx, command)
	if failure == nil || failure.Kind != "run_failed" || result.Stdout != "" {
		t.Fatalf("result=%+v failure=%+v", result, failure)
	}
}

func TestOneShotTmuxBrokerRoundTrip(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("tmux one-shot broker runs on Linux")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TCLAUDE_ONE_SHOT_TEST_HELPER", "1")
	previousLaunch := launchNonInteractiveTmuxSession
	previousAlive := nonInteractiveTmuxSessionAlive
	previousKill := killNonInteractiveTmuxSession
	previousHelper := nonInteractiveHelperShellCommand
	launchNonInteractiveTmuxSession = func(_, _, shell string, _ ...string) error {
		return exec.Command("/bin/sh", "-c", shell).Start()
	}
	nonInteractiveTmuxSessionAlive = func(string) bool { return true }
	killNonInteractiveTmuxSession = func(string) {}
	nonInteractiveHelperShellCommand = func(requestPath, resultPath string) string {
		return clcommon.ShellQuoteArg(os.Args[0]) +
			" -test.run=TestOneShotBrokerHelperSubprocess -- " +
			clcommon.ShellQuoteArg(requestPath) + " " + clcommon.ShellQuoteArg(resultPath)
	}
	t.Cleanup(func() {
		launchNonInteractiveTmuxSession = previousLaunch
		nonInteractiveTmuxSessionAlive = previousAlive
		killNonInteractiveTmuxSession = previousKill
		nonInteractiveHelperShellCommand = previousHelper
	})
	command := nonInteractiveCommand{
		Argv: []string{"/bin/sh", "-c", "printf 'broker-roundtrip\\n'"},
		Cwd:  t.TempDir(), Env: os.Environ(), TimeoutSeconds: 10,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, failure := runNonInteractiveThroughTmux(ctx, command)
	if failure != nil || result.Stdout != "broker-roundtrip\n" || result.ExitCode != 0 {
		t.Fatalf("result=%+v failure=%+v", result, failure)
	}
	entries, err := os.ReadDir(filepath.Join(config.DataDir(), "one-shot"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("handoff not cleaned up: entries=%v error=%v", entries, err)
	}
}

func TestOneShotTmuxCancellationWaitsForPaneCleanup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("tmux one-shot launcher runs on Linux")
	}
	t.Setenv("HOME", t.TempDir())
	previousLaunch := launchNonInteractiveTmuxSession
	previousAlive := nonInteractiveTmuxSessionAlive
	previousKill := killNonInteractiveTmuxSession
	previousHelper := nonInteractiveHelperShellCommand
	t.Cleanup(func() {
		launchNonInteractiveTmuxSession = previousLaunch
		nonInteractiveTmuxSessionAlive = previousAlive
		killNonInteractiveTmuxSession = previousKill
		nonInteractiveHelperShellCommand = previousHelper
	})
	request := make(chan string, 1)
	var alive, cleaned, killedEarly atomic.Bool
	alive.Store(true)
	nonInteractiveHelperShellCommand = func(path, _ string) string {
		request <- path
		return "true"
	}
	launchNonInteractiveTmuxSession = func(_, _, _ string, _ ...string) error { return nil }
	nonInteractiveTmuxSessionAlive = func(string) bool { return alive.Load() }
	killNonInteractiveTmuxSession = func(string) {
		if !cleaned.Load() {
			killedEarly.Store(true)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	cwd := t.TempDir()
	done := make(chan *spawnFailure, 1)
	go func() {
		_, failure := runNonInteractiveThroughTmux(ctx, nonInteractiveCommand{
			Argv: []string{"/bin/sh", "-c", "true"}, Cwd: cwd, Env: os.Environ(), TimeoutSeconds: 5,
		})
		done <- failure
	}()
	var path string
	select {
	case path = <-request:
	case <-time.After(2 * time.Second):
		t.Fatal("one-shot launcher did not create a request")
	}
	cancel()
	go func() {
		for {
			if _, err := os.Stat(path); os.IsNotExist(err) {
				cleaned.Store(true)
				alive.Store(false)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	if failure := <-done; failure == nil || failure.Msg != "one-shot run canceled" {
		t.Fatalf("failure=%+v", failure)
	}
	if killedEarly.Load() || !cleaned.Load() {
		t.Fatalf("pane killed before helper cleanup: killedEarly=%v cleaned=%v", killedEarly.Load(), cleaned.Load())
	}
}

func TestOneShotBrokerHelperSubprocess(t *testing.T) {
	if os.Getenv("TCLAUDE_ONE_SHOT_TEST_HELPER") != "1" {
		return
	}
	args := os.Args
	for i, arg := range args {
		if arg == "--" && i+2 < len(args) {
			if err := runOneShotExecHelper(args[i+1], args[i+2]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	t.Fatal("missing helper request and result paths")
}

func TestCleanupStaleOneShotHandoffs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := filepath.Join(config.DataDir(), "one-shot")
	stale := filepath.Join(root, "run-stale", "request.json")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("private prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStaleOneShotHandoffs(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale handoff remains: %v", err)
	}
}

func TestOneShotExecHelperRefusesHandoffOutsidePrivateData(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	outside := filepath.Join(t.TempDir(), "request.json")
	result := filepath.Join(t.TempDir(), "result.json")
	if err := runOneShotExecHelper(outside, result); err == nil ||
		!strings.Contains(err.Error(), "private data directory") {
		t.Fatalf("expected private handoff refusal, got %v", err)
	}
}
