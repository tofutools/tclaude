package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

const nonInteractiveBrokerGrace = 5 * time.Second

type nonInteractiveBrokerRequest struct {
	Command         nonInteractiveCommand `json:"command"`
	Deadline        time.Time             `json:"deadline"`
	DaemonPID       int                   `json:"daemon_pid"`
	DaemonStartTime string                `json:"daemon_start_time"`
}

type nonInteractiveBrokerReply struct {
	Result  nonInteractiveSpawnResult `json:"result"`
	Failure *spawnFailure             `json:"failure,omitempty"`
}

var nonInteractiveTmuxCommand = func(shellCommand string) *exec.Cmd {
	return clcommon.Default.Command("run-shell", shellCommand)
}

var nonInteractiveHelperShellCommand = func(requestPath, resultPath string) string {
	// The standalone tclaude-agentd binary transitions back into the daemon's
	// AppArmor profile when exec'd by tmux. Invoke the sibling tclaude CLI,
	// just as the tmux-based sandbox capability probe does.
	return clcommon.DetectAbsoluteCmd("agentd", "one-shot-exec") +
		" --request " + clcommon.ShellQuoteArg(requestPath) +
		" --result " + clcommon.ShellQuoteArg(resultPath)
}

var nonInteractiveTmuxServerAvailable = func() bool {
	out, err := clcommon.Default.Command("display-message", "-p", "#{pid}").Output()
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	return err == nil && pid > 0
}

// The daemon can be confined more tightly than the tmux server. Its normal
// sandbox probe runs under tmux, so launch the matching one-shot boundary there
// too. A tmux run-shell job creates no session or group member.
func runNonInteractiveThroughTmux(ctx context.Context, command nonInteractiveCommand) (nonInteractiveSpawnResult, *spawnFailure) {
	if !nonInteractiveTmuxServerAvailable() {
		// With no server, tmux's capability probe uses this process too. The
		// direct launch is therefore the matching context and avoids making a
		// persistent server solely for a one-shot run.
		return executeNonInteractiveCommand(ctx, command)
	}
	fail := func(message string) (nonInteractiveSpawnResult, *spawnFailure) {
		return nonInteractiveSpawnResult{}, &spawnFailure{Status: 502, Kind: "run_failed", Msg: message}
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return fail("one-shot tmux launch has no deadline")
	}
	root := filepath.Join(config.DataDir(), "one-shot")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fail(fmt.Sprintf("create private one-shot directory: %v", err))
	}
	dir, err := os.MkdirTemp(root, "run-")
	if err != nil {
		return fail(fmt.Sprintf("create one-shot handoff: %v", err))
	}
	defer func() { _ = os.RemoveAll(dir) }()
	requestPath := filepath.Join(dir, "request.json")
	resultPath := filepath.Join(dir, "result.json")
	startTime, err := oneShotProcessStartTime(os.Getpid())
	if err != nil {
		return fail(fmt.Sprintf("identify one-shot daemon: %v", err))
	}
	request := nonInteractiveBrokerRequest{
		Command: command, Deadline: deadline,
		DaemonPID: os.Getpid(), DaemonStartTime: startTime,
	}
	data, err := json.Marshal(request)
	if err != nil {
		return fail(fmt.Sprintf("encode one-shot handoff: %v", err))
	}
	if err := os.WriteFile(requestPath, data, 0o600); err != nil {
		return fail(fmt.Sprintf("write one-shot handoff: %v", err))
	}
	shellCommand := nonInteractiveHelperShellCommand(requestPath, resultPath)
	// tmux performs format expansion before the shell interprets quoting.
	shellCommand = strings.ReplaceAll(shellCommand, "#", "##")
	tmux := nonInteractiveTmuxCommand(shellCommand)
	tmuxStdout := &boundedHeadBuffer{max: 16 << 10}
	tmux.Stdout = tmuxStdout
	tmuxStderr := &boundedHeadBuffer{max: 16 << 10}
	tmux.Stderr = tmuxStderr
	if err := tmux.Start(); err != nil {
		return fail(fmt.Sprintf("start one-shot tmux job: %v", err))
	}
	done := make(chan error, 1)
	go func() { done <- tmux.Wait() }()
	grace := time.NewTimer(time.Until(deadline.Add(nonInteractiveBrokerGrace)))
	defer grace.Stop()
	var jobErr error
	select {
	case jobErr = <-done:
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.Canceled) {
			_ = os.Remove(requestPath) // the helper watches this as its cancellation signal
			_ = tmux.Process.Kill()
			<-done
			return fail("one-shot run canceled")
		}
		select {
		case jobErr = <-done:
		case <-grace.C:
			_ = os.Remove(requestPath)
			_ = tmux.Process.Kill()
			<-done
			return nonInteractiveSpawnResult{Stderr: "run timed out\n", ExitCode: 124}, nil
		}
	case <-grace.C:
		_ = os.Remove(requestPath)
		_ = tmux.Process.Kill()
		<-done
		return nonInteractiveSpawnResult{Stderr: "run timed out\n", ExitCode: 124}, nil
	}
	data, err = os.ReadFile(resultPath)
	if err != nil {
		return fail(fmt.Sprintf("one-shot tmux job produced no result: %v (tmux: %v; output: %s%s)", err, jobErr, tmuxStdout.String(), tmuxStderr.String()))
	}
	// JSON may expand a control byte to a six-byte Unicode escape.
	if len(data) > 12*maxNonInteractiveOutputBytes+8192 {
		return fail("one-shot tmux result exceeded the output limit")
	}
	var reply nonInteractiveBrokerReply
	if err := json.Unmarshal(data, &reply); err != nil {
		return fail(fmt.Sprintf("decode one-shot tmux result: %v", err))
	}
	return reply.Result, reply.Failure
}

func oneShotExecCmd() *cobra.Command {
	var requestPath, resultPath string
	cmd := &cobra.Command{
		Use: "one-shot-exec", Hidden: true,
		Short:             "Run a daemon-authorized one-shot under the tmux server (internal)",
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(*cobra.Command, []string) error {
			return runOneShotExecHelper(requestPath, resultPath)
		},
	}
	cmd.Flags().StringVar(&requestPath, "request", "", "private one-shot request path (internal)")
	cmd.Flags().StringVar(&resultPath, "result", "", "private one-shot result path (internal)")
	return cmd
}

func runOneShotExecHelper(requestPath, resultPath string) error {
	root := filepath.Join(config.DataDir(), "one-shot")
	if err := validateOneShotHandoffPath(root, requestPath, "request.json"); err != nil {
		return err
	}
	if err := validateOneShotHandoffPath(root, resultPath, "result.json"); err != nil {
		return err
	}
	if filepath.Dir(requestPath) != filepath.Dir(resultPath) {
		return errors.New("one-shot request and result must share a private directory")
	}
	info, err := os.Lstat(requestPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return errors.New("one-shot request must be a private regular file")
	}
	data, err := os.ReadFile(requestPath)
	if err != nil {
		return err
	}
	var request nonInteractiveBrokerRequest
	if err := json.Unmarshal(data, &request); err != nil {
		return err
	}
	if len(request.Command.Argv) == 0 || request.Deadline.IsZero() ||
		request.DaemonPID <= 0 || request.DaemonStartTime == "" {
		return errors.New("incomplete one-shot request")
	}
	start, err := oneShotProcessStartTime(request.DaemonPID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check one-shot daemon identity: %w", err)
	}
	if err != nil || start != request.DaemonStartTime {
		_ = os.RemoveAll(filepath.Dir(requestPath))
		return nil // the daemon that authorized this handoff is already gone
	}
	ctx, cancel := context.WithDeadline(context.Background(), request.Deadline)
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	var orphaned atomic.Bool
	var watcherError atomic.Value
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if _, err := os.Stat(requestPath); err != nil {
					if !errors.Is(err, os.ErrNotExist) {
						watcherError.Store(err)
						cancel()
						return
					}
					orphaned.Store(true)
					cancel()
					return
				}
				start, err := oneShotProcessStartTime(request.DaemonPID)
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					watcherError.Store(err)
					cancel()
					return
				}
				if err != nil || start != request.DaemonStartTime {
					orphaned.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	result, failure := executeNonInteractiveCommand(ctx, request.Command)
	if value := watcherError.Load(); value != nil {
		return fmt.Errorf("watch one-shot daemon identity: %w", value.(error))
	}
	if orphaned.Load() {
		_ = os.RemoveAll(filepath.Dir(requestPath))
		return nil
	}
	reply, err := json.Marshal(nonInteractiveBrokerReply{Result: result, Failure: failure})
	if err != nil {
		return err
	}
	partial := resultPath + ".partial"
	if err := os.WriteFile(partial, reply, 0o600); err != nil {
		return err
	}
	if err := os.Rename(partial, resultPath); err != nil {
		_ = os.Remove(partial)
		return err
	}
	return nil
}

// A PID can be reused after a daemon crash. Linux's starttime (field 22 of
// /proc/<pid>/stat) distinguishes the daemon that authorized this handoff.
func oneShotProcessStartTime(pid int) (string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", errors.New("malformed process status")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) <= 19 {
		return "", errors.New("process status has no start time")
	}
	return fields[19], nil
}

// A daemon restart owns no jobs from the prior process. Remove handoffs left
// behind if it died before the helper could consume or clean them.
func cleanupStaleOneShotHandoffs() error {
	root := filepath.Join(config.DataDir(), "one-shot")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "run-") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func validateOneShotHandoffPath(root, path, basename string) error {
	if path == "" || filepath.Base(path) != basename || !filepath.IsAbs(path) {
		return errors.New("invalid one-shot handoff path")
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.Dir(rel) == "." {
		return errors.New("one-shot handoff must be under the daemon's private data directory")
	}
	return nil
}
