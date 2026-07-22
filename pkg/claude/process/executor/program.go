package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/process/engine"
)

var programPerform = performProgram

const (
	DefaultProgramTimeout     = 10 * time.Minute
	MaxProgramTimeout         = time.Hour
	MaxOutputTailBytes        = 16 * 1024
	MaxProgramArgs            = 256
	MaxProgramExecutableBytes = 4 * 1024
	MaxProgramArgBytes        = 16 * 1024
	MaxProgramArgvBytes       = 32 * 1024
	MaxProgramErrorBytes      = 4 * 1024
	MaxEnvironmentValue       = 16 * 1024
	MaxEnvironmentBytes       = 64 * 1024
	programWaitDelay          = time.Second
)

var ErrUnauthorized = errors.New("program execution is not authorized for this run")

type Result struct {
	Observation     engine.ProgramObservation `json:"observation"`
	StartedAt       time.Time                 `json:"startedAt"`
	FinishedAt      time.Time                 `json:"finishedAt"`
	Dispatched      bool                      `json:"dispatched"`
	TimedOut        bool                      `json:"timedOut,omitempty"`
	Canceled        bool                      `json:"canceled,omitempty"`
	Stdout          string                    `json:"stdout,omitempty"`
	Stderr          string                    `json:"stderr,omitempty"`
	StdoutTruncated bool                      `json:"stdoutTruncated,omitempty"`
	StderrTruncated bool                      `json:"stderrTruncated,omitempty"`
	CleanupError    string                    `json:"cleanupError,omitempty"`
}

// Execute validates explicit per-run authorization, dispatches argv without a
// shell, waits for process cleanup, and then commits the observation. A commit
// failure leaves the durable outstanding command for explicit reconciliation.
func Execute(ctx context.Context, run *Run, dispatch *Dispatch, authorization Authorization) (Result, error) {
	result, err := runProgram(ctx, run, dispatch, authorization)
	if err != nil {
		return Result{}, err
	}
	if err := recordResult(run, dispatch, result); err != nil {
		// The program may have executed, so the spent in-memory permission must
		// never become dispatchable again. Dropping it exposes the same explicit
		// reconciliation actions as a cold load of the durable outstanding row.
		run.dispatch = nil
		return result, fmt.Errorf("program observation is not durable; reconciliation required: %w", err)
	}
	return result, nil
}

func runProgram(ctx context.Context, run *Run, dispatch *Dispatch, authorization Authorization) (Result, error) {
	if run == nil || dispatch == nil || dispatch.owner != run || dispatch.stateVersion != run.stateVersion || run.dispatch != dispatch {
		return Result{}, ErrStaleDispatch
	}
	command := dispatch.command
	if authorization.RunID != run.id || authorization.Profile != command.Program.Profile {
		return Result{}, ErrUnauthorized
	}
	dispatch.mu.Lock()
	if dispatch.used {
		dispatch.mu.Unlock()
		return Result{}, ErrStaleDispatch
	}
	dispatch.used = true
	dispatch.mu.Unlock()

	return programPerform(ctx, run.id, command)
}

func performProgram(ctx context.Context, runID string, command engine.Command) (Result, error) {
	started := time.Now().UTC()
	result := Result{
		StartedAt: started,
		Observation: engine.ProgramObservation{
			CommandID: command.ID, NodeID: command.NodeID,
			Outcome: engine.ProgramFailed, ExitCode: -1,
		},
	}
	timeout, err := validateProgram(command.Program)
	if err != nil {
		result.FinishedAt = time.Now().UTC()
		result.Observation.Error = boundedError(err.Error())
		return result, nil
	}
	environment, err := programEnvironment(runID, command.ID)
	if err != nil {
		result.FinishedAt = time.Now().UTC()
		result.Observation.Error = boundedError(err.Error())
		return result, nil
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stdout := newTailBuffer(MaxOutputTailBytes)
	stderr := newTailBuffer(MaxOutputTailBytes)
	cmd := osexec.CommandContext(runCtx, command.Program.Run, command.Program.Args...)
	configureProgramCommand(cmd)
	cmd.Env = environment
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	runErr := cmd.Run()
	result.Dispatched = cmd.Process != nil
	if cleanupErr := cleanupProgramCommand(cmd); cleanupErr != nil {
		result.CleanupError = boundedError(cleanupErr.Error())
		if runErr == nil {
			runErr = fmt.Errorf("process-group cleanup: %w", cleanupErr)
		} else {
			runErr = fmt.Errorf("%v; process-group cleanup: %w", runErr, cleanupErr)
		}
	}
	result.FinishedAt = time.Now().UTC()
	result.Stdout, result.Stderr = stdout.String(), stderr.String()
	result.StdoutTruncated, result.StderrTruncated = stdout.Truncated(), stderr.Truncated()
	if cmd.ProcessState != nil {
		result.Observation.ExitCode = cmd.ProcessState.ExitCode()
	}
	if runErr == nil && result.Observation.ExitCode == 0 {
		result.Observation.Outcome = engine.ProgramSucceeded
		return result, nil
	}
	cause := context.Cause(runCtx)
	result.TimedOut = errors.Is(cause, context.DeadlineExceeded)
	result.Canceled = !result.TimedOut && cause != nil
	switch {
	case result.TimedOut:
		result.Observation.Error = boundedError(fmt.Sprintf("program timed out after %s", timeout))
	case result.Canceled:
		result.Observation.Error = boundedError("program canceled: " + cause.Error())
	case runErr != nil:
		result.Observation.Error = boundedError(runErr.Error())
	default:
		result.Observation.Error = boundedError(fmt.Sprintf("program exited with code %d", result.Observation.ExitCode))
	}
	return result, nil
}

func recordResult(run *Run, dispatch *Dispatch, result Result) error {
	if run == nil || dispatch == nil || dispatch.owner != run || dispatch.stateVersion != run.stateVersion || run.dispatch != dispatch || !dispatch.wasUsed() {
		return ErrStaleDispatch
	}
	command := dispatch.command
	if result.Observation.CommandID != command.ID || result.Observation.NodeID != command.NodeID {
		return ErrStaleDispatch
	}
	next, err := engine.Apply(run.checkpoint, run.definition, engine.Transition{
		Kind: engine.TransitionProgramObserved, Observation: &result.Observation,
	})
	if err != nil {
		return err
	}
	return persist(run, next, event("program_observed", &command, executorActor, result))
}

func validateProgram(program engine.ProgramCommand) (time.Duration, error) {
	if strings.TrimSpace(program.Run) == "" || len(program.Run) > MaxProgramExecutableBytes || !utf8.ValidString(program.Run) || strings.IndexByte(program.Run, 0) >= 0 {
		return 0, fmt.Errorf("program executable must be valid UTF-8 without NUL and at most %d bytes", MaxProgramExecutableBytes)
	}
	if len(program.Args) > MaxProgramArgs {
		return 0, fmt.Errorf("program argv exceeds %d arguments", MaxProgramArgs)
	}
	total := len(program.Run)
	for index, arg := range program.Args {
		if len(arg) > MaxProgramArgBytes || !utf8.ValidString(arg) || strings.IndexByte(arg, 0) >= 0 {
			return 0, fmt.Errorf("program args[%d] must be valid UTF-8 without NUL and at most %d bytes", index, MaxProgramArgBytes)
		}
		total += len(arg)
		if total > MaxProgramArgvBytes {
			return 0, fmt.Errorf("program argv exceeds %d bytes", MaxProgramArgvBytes)
		}
	}
	if strings.TrimSpace(program.Timeout) == "" {
		return DefaultProgramTimeout, nil
	}
	timeout, err := time.ParseDuration(strings.TrimSpace(program.Timeout))
	if err != nil || timeout <= 0 || timeout > MaxProgramTimeout {
		return 0, fmt.Errorf("program timeout must be greater than zero and at most %s", MaxProgramTimeout)
	}
	return timeout, nil
}

func programEnvironment(runID, commandID string) ([]string, error) {
	names := [...]string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL"}
	environment := make([]string, 0, len(names)+2)
	total := 0
	appendEntry := func(name, value string) error {
		if len(value) > MaxEnvironmentValue || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
			return fmt.Errorf("program environment variable %s exceeds its safe bound", name)
		}
		entry := name + "=" + value
		total += len(entry)
		if total > MaxEnvironmentBytes {
			return fmt.Errorf("program environment exceeds %d bytes", MaxEnvironmentBytes)
		}
		environment = append(environment, entry)
		return nil
	}
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok {
			if err := appendEntry(name, value); err != nil {
				return nil, err
			}
		}
	}
	if err := appendEntry("TCLAUDE_PROCESS_RUN_ID", runID); err != nil {
		return nil, err
	}
	if err := appendEntry("TCLAUDE_PROCESS_COMMAND_ID", commandID); err != nil {
		return nil, err
	}
	return environment, nil
}

func boundedError(value string) string {
	value = strings.ToValidUTF8(value, "�")
	if len(value) <= MaxProgramErrorBytes {
		return value
	}
	value = value[:MaxProgramErrorBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

type tailBuffer struct {
	maximum   int
	data      []byte
	truncated bool
}

func newTailBuffer(maximum int) *tailBuffer { return &tailBuffer{maximum: maximum} }

func (b *tailBuffer) Write(p []byte) (int, error) {
	written := len(p)
	if b.maximum <= 0 {
		b.truncated = b.truncated || len(p) > 0
		return written, nil
	}
	if len(p) >= b.maximum {
		b.truncated = b.truncated || len(p) > b.maximum || len(b.data) > 0
		b.data = append(b.data[:0], p[len(p)-b.maximum:]...)
		return written, nil
	}
	if overflow := len(b.data) + len(p) - b.maximum; overflow > 0 {
		b.truncated = true
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
	}
	b.data = append(b.data, p...)
	return written, nil
}

func (b *tailBuffer) String() string  { return strings.ToValidUTF8(string(b.data), "?") }
func (b *tailBuffer) Truncated() bool { return b.truncated }
