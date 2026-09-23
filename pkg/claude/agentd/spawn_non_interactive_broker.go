package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

const nonInteractiveBrokerGrace = 5 * time.Second

type nonInteractiveBrokerRequest struct {
	Command  nonInteractiveCommand `json:"command"`
	Deadline time.Time             `json:"deadline"`
}

type nonInteractiveBrokerReply struct {
	Result  nonInteractiveSpawnResult `json:"result"`
	Failure *spawnFailure             `json:"failure,omitempty"`
}

var nonInteractiveTmuxCommand = func(shellCommand string) *exec.Cmd {
	return clcommon.Default.Command("run-shell", shellCommand)
}

// The daemon can be confined more tightly than the tmux server. Its normal
// sandbox probe runs under tmux, so launch the matching one-shot boundary there
// too. A tmux run-shell job creates no session or group member.
func runNonInteractiveThroughTmux(ctx context.Context, command nonInteractiveCommand) (nonInteractiveSpawnResult, *spawnFailure) {
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
	request := nonInteractiveBrokerRequest{Command: command, Deadline: deadline}
	data, err := json.Marshal(request)
	if err != nil {
		return fail(fmt.Sprintf("encode one-shot handoff: %v", err))
	}
	if err := os.WriteFile(requestPath, data, 0o600); err != nil {
		return fail(fmt.Sprintf("write one-shot handoff: %v", err))
	}
	executable, err := os.Executable()
	if err != nil {
		return fail(fmt.Sprintf("resolve one-shot helper binary: %v", err))
	}
	verb := " agentd one-shot-exec"
	if filepath.Base(executable) == "tclaude-agentd" {
		verb = " one-shot-exec"
	}
	shellCommand := clcommon.ShellQuoteArg(executable) + verb +
		" --request " + clcommon.ShellQuoteArg(requestPath) +
		" --result " + clcommon.ShellQuoteArg(resultPath)
	// tmux performs format expansion before the shell interprets quoting.
	shellCommand = strings.ReplaceAll(shellCommand, "#", "##")
	tmux := nonInteractiveTmuxCommand(shellCommand)
	tmux.Stdout = io.Discard
	var tmuxStderr strings.Builder
	tmux.Stderr = &tmuxStderr
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
		return fail(fmt.Sprintf("one-shot tmux job produced no result: %v (tmux: %v; stderr: %s)", err, jobErr, tmuxStderr.String()))
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
	if len(request.Command.Argv) == 0 || request.Deadline.IsZero() {
		return errors.New("incomplete one-shot request")
	}
	ctx, cancel := context.WithDeadline(context.Background(), request.Deadline)
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if _, err := os.Stat(requestPath); errors.Is(err, os.ErrNotExist) {
					cancel()
					return
				}
			}
		}
	}()
	result, failure := executeNonInteractiveCommand(ctx, request.Command)
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
