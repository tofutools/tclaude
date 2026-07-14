package processexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
)

const (
	DefaultProgramTimeout  = 10 * time.Minute
	DefaultOutputTailBytes = 32 * 1024
)

type ProgramAdapter struct {
	DefaultTimeout time.Duration
	// TimeoutContext defaults to context.WithTimeout. Tests may replace it to
	// drive timeout behavior after an external process has completed a startup
	// handshake, without relying on a scheduler-sensitive wall-clock deadline.
	TimeoutContext  func(context.Context, time.Duration) (context.Context, context.CancelFunc)
	OutputTailBytes int
	Now             func() time.Time
}

type ProgramEvidence struct {
	SchemaVersion  int       `json:"schemaVersion"`
	CommandID      string    `json:"commandId"`
	IdempotencyKey string    `json:"idempotencyKey"`
	Command        string    `json:"command"`
	Args           []string  `json:"args,omitempty"`
	StartedAt      time.Time `json:"startedAt"`
	FinishedAt     time.Time `json:"finishedAt"`
	ExitCode       int       `json:"exitCode"`
	TimedOut       bool      `json:"timedOut,omitempty"`
	StdoutTail     string    `json:"stdoutTail,omitempty"`
	StderrTail     string    `json:"stderrTail,omitempty"`
	Error          string    `json:"error,omitempty"`
}

func (a ProgramAdapter) Validate(request Request) error {
	if request.Performer.Kind != model.PerformerProgram {
		return fmt.Errorf("program adapter cannot perform kind %q", request.Performer.Kind)
	}
	commandName := strings.TrimSpace(request.Performer.Run)
	if commandName == "" {
		return fmt.Errorf("program performer run is required")
	}
	if strings.IndexByte(commandName, 0) >= 0 {
		return fmt.Errorf("program performer run contains a NUL byte")
	}
	if !state.ValidateActorRef(state.ActorRef("program:" + commandName + "@exit0")) {
		return fmt.Errorf("program performer run %q cannot form a valid actor ref", commandName)
	}
	for i, arg := range request.Performer.Args {
		if strings.IndexByte(arg, 0) >= 0 {
			return fmt.Errorf("program performer args[%d] contains a NUL byte", i)
		}
	}
	_, err := a.timeout(request.Performer.Timeout)
	return err
}

func (a ProgramAdapter) Perform(ctx context.Context, request Request) (Observation, error) {
	if err := a.Validate(request); err != nil {
		return Observation{}, err
	}
	timeout, _ := a.timeout(request.Performer.Timeout)
	timeoutContext := a.TimeoutContext
	if timeoutContext == nil {
		timeoutContext = context.WithTimeout
	}
	runCtx, cancel := timeoutContext(ctx, timeout)
	defer cancel()

	now := a.Now
	if now == nil {
		now = time.Now
	}
	startedAt := now().UTC()
	stdout := newTailBuffer(a.tailBytes())
	stderr := newTailBuffer(a.tailBytes())
	commandName := strings.TrimSpace(request.Performer.Run)
	command := osexec.CommandContext(runCtx, commandName, request.Performer.Args...)
	configureProgramCommand(command)
	command.Stdout = stdout
	command.Stderr = stderr
	command.Env = append(minimalProgramEnvironment(),
		"TCLAUDE_PROCESS_COMMAND_ID="+request.Command.ID,
		"TCLAUDE_PROCESS_IDEMPOTENCY_KEY="+request.Command.IdempotencyKey,
	)
	runErr := command.Run()
	finishedAt := now().UTC()
	if ctx.Err() != nil {
		return Observation{}, ctx.Err()
	}

	exitCode := -1
	if command.ProcessState != nil {
		exitCode = command.ProcessState.ExitCode()
	}
	timedOut := errors.Is(context.Cause(runCtx), context.DeadlineExceeded)
	evidence := ProgramEvidence{
		SchemaVersion:  1,
		CommandID:      request.Command.ID,
		IdempotencyKey: request.Command.IdempotencyKey,
		Command:        commandName,
		Args:           append([]string(nil), request.Performer.Args...),
		StartedAt:      startedAt,
		FinishedAt:     finishedAt,
		ExitCode:       exitCode,
		TimedOut:       timedOut,
		StdoutTail:     stdout.String(),
		StderrTail:     stderr.String(),
	}
	if runErr != nil {
		evidence.Error = runErr.Error()
	}
	body, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return Observation{}, fmt.Errorf("encode program evidence for command %q: %w", request.Command.ID, err)
	}
	body = append(body, '\n')
	verdict := "fail"
	if exitCode == 0 {
		verdict = "pass"
	}
	return Observation{
		Actor:    state.ActorRef(fmt.Sprintf("program:%s@exit%d", commandName, exitCode)),
		Verdict:  verdict,
		Feedback: evidence.StderrTail,
		Evidence: &Artifact{Name: "program-" + request.Command.ID + ".json", Data: body},
	}, nil
}

func minimalProgramEnvironment() []string {
	environment := make([]string, 0, 8)
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch name {
		case "PATH", "HOME", "TMPDIR", "LANG":
			environment = append(environment, entry)
		default:
			if strings.HasPrefix(name, "LC_") {
				environment = append(environment, entry)
			}
		}
	}
	return environment
}

func (a ProgramAdapter) timeout(value string) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		if a.DefaultTimeout > 0 {
			return a.DefaultTimeout, nil
		}
		return DefaultProgramTimeout, nil
	}
	timeout, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("invalid program timeout %q: %w", value, err)
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("program timeout must be greater than zero")
	}
	return timeout, nil
}

func (a ProgramAdapter) tailBytes() int {
	if a.OutputTailBytes > 0 {
		return a.OutputTailBytes
	}
	return DefaultOutputTailBytes
}

type tailBuffer struct {
	max  int
	data []byte
}

func newTailBuffer(maxBytes int) *tailBuffer {
	return &tailBuffer{max: maxBytes}
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	written := len(p)
	if b.max <= 0 {
		return written, nil
	}
	if len(p) >= b.max {
		b.data = append(b.data[:0], p[len(p)-b.max:]...)
		return written, nil
	}
	overflow := len(b.data) + len(p) - b.max
	if overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
	}
	b.data = append(b.data, p...)
	return written, nil
}

func (b *tailBuffer) String() string {
	return string(b.data)
}
