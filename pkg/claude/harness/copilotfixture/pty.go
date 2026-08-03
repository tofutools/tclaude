package copilotfixture

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// A pseudo-terminal is not a convenience here; it is the difference between
// measuring Copilot and measuring the absence of a terminal.
//
// Every other scenario in this package runs the CLI with stdin on /dev/null
// and no controlling terminal, which is right for the compatibility fixtures:
// they assert on provider traffic and on the JSONL event stream, neither of
// which a terminal changes.
//
// The permission matrix cannot use that setup, and the first probe written for
// it showed why in the sharpest possible way. Launched WITHOUT
// `--allow-all-tools`, with no TTY, the CLI executed the mock's tool call and
// completed the turn — the same observable outcome as launching WITH the flag.
// Read literally that says the permission flags do nothing. Read correctly it
// says a CLI that cannot draw a prompt does not draw one, so a non-TTY run can
// never observe a permission gate at all.
//
// tclaude spawns Copilot into a tmux pane, which has a real PTY. So the only
// launch shape whose permission behavior is evidence about tclaude is one with
// a PTY attached, and that is what this file provides.
//
// The cost is that there is no machine-readable surface: `-i` renders a TUI,
// so the transcript is screen text with cursor control in it. Scenarios must
// therefore assert on coarse, durable signals — did the process exit, did the
// tool result reach the provider — and treat the rendered text as
// corroboration rather than as the contract. Anything finer would be asserting
// on a screen layout that GitHub is free to restyle.

// PTYQuiescence is how long the transcript must stop growing before a run that
// has not exited is called blocked.
//
// A permission prompt is indistinguishable from slow work by any single
// observation: both are "no exit yet". What separates them is that a prompt is
// terminal — the CLI has drawn its question and will wait forever — while work
// keeps producing output. Two seconds is far longer than the gaps a local
// mock-backed turn produces (the whole turn takes ~2s) and far shorter than
// the deadline, so the classification is not a close call in either direction.
const PTYQuiescence = 2 * time.Second

// readDrainGrace bounds how long the transcript reader may keep going after
// the CLI is gone before the master is closed out from under it. See the
// comment at the close site in RunPTY for why this cannot be left unbounded.
const readDrainGrace = 2 * time.Second

// PTYResult is one pseudo-terminal run.
type PTYResult struct {
	// Transcript is everything the CLI wrote to the terminal, with ANSI escape
	// sequences left in. Use TranscriptText for matching.
	Transcript string

	// Exited reports that the CLI terminated on its own. When false the run hit
	// its deadline with the process still alive, which — combined with
	// Quiesced — is how a blocked launch is identified.
	Exited   bool
	ExitCode int

	// Quiesced reports that output stopped growing for PTYQuiescence before the
	// deadline. On a run that did not exit this is the blocked-state signal.
	Quiesced bool

	// Settled reports that PTYOptions.SettledWhen fired, i.e. the run ended
	// because its evidence had arrived rather than because it ran out of time.
	//
	// A scenario should distinguish the two even though both leave Exited
	// false: "ended early with the follow-up request in hand" and "sat at the
	// deadline having produced nothing" are the allowed and blocked arms.
	Settled bool
}

// TranscriptText strips ANSI control sequences so a scenario can match on
// words the CLI printed.
//
// Matching raw is not an option: the TUI redraws with cursor movement and
// colour, so a phrase the user plainly sees on screen is routinely interleaved
// with escape bytes in the byte stream.
func (r PTYResult) TranscriptText() string {
	return stripANSI(r.Transcript)
}

// Contains reports whether the rendered transcript contains needle, case
// insensitively.
func (r PTYResult) Contains(needle string) bool {
	return strings.Contains(strings.ToLower(r.TranscriptText()), strings.ToLower(needle))
}

// PTYOptions extends RunOptions with terminal-specific inputs.
type PTYOptions struct {
	RunOptions

	// Input is written to the terminal after the CLI has quiesced, which is the
	// only moment at which typing means anything: sending keystrokes into a
	// TUI that has not finished starting is how a scenario ends up measuring
	// its own race.
	//
	// Each entry is written as a separate line. After the last one the run
	// waits for exit or deadline exactly as it would without input.
	Input []string

	// Deadline bounds the whole run; RunOptions.Timeout is used when zero.
	Deadline time.Duration

	// SettledWhen, when non-nil, ends the run as soon as it returns true.
	//
	// Interactive mode never exits on its own — after a turn completes the CLI
	// sits at its input prompt — so EVERY pty scenario would otherwise pay the
	// full deadline, including the ones whose evidence arrived in two seconds.
	// Across a matrix that is the difference between a suite people run and a
	// suite people disable.
	//
	// It must only report evidence that is already conclusive, typically "the
	// tool-result follow-up request arrived". Returning true on something
	// weaker would cut the run short of the observation it exists to make.
	SettledWhen func() bool
}

// RunPTY launches the CLI on a pseudo-terminal in interactive mode.
//
// The prompt is always delivered with `-i`: a permission posture is a property
// of the interactive mode tclaude spawns, and `-p` refuses to start without
// `--allow-all-tools` in the first place.
//
// A run that never exits is a RESULT, not a failure. That is the whole point:
// the blocked-launch claim is only observable as "still alive, output stopped".
func RunPTY(t *testing.T, opts PTYOptions) PTYResult {
	t.Helper()

	run := opts.RunOptions

	deadline := opts.Deadline
	if deadline == 0 {
		deadline = run.Timeout
	}
	if deadline == 0 {
		deadline = RunTimeout
	}

	args := ptyArgs(t, run)

	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	cmd := exec.CommandContext(ctx, "copilot", args...)
	cmd.Dir = run.WorkDir
	cmd.Env = buildEnv(run)
	// Without this a descendant holding the terminal keeps Wait blocked past
	// the deadline the scenario chose, so a "blocked" verdict would cost the
	// whole test timeout instead of the deadline.
	cmd.WaitDelay = waitDelay

	// A fixed, generous window. The TUI wraps to the terminal width, and an
	// 80-column default would break the CLI's own prompt text across lines and
	// make transcript matching depend on where the wrap fell.
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 50, Cols: 200})
	if err != nil {
		t.Fatalf("copilotfixture: starting copilot on a pty: %v", err)
	}
	defer func() { _ = f.Close() }()

	var (
		mu         sync.Mutex
		transcript strings.Builder
		lastWrite  = time.Now()
	)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				mu.Lock()
				transcript.Write(buf[:n])
				lastWrite = time.Now()
				mu.Unlock()
			}
			if err != nil {
				// EIO is how a pty master reports that the child closed the
				// slave side, i.e. an ordinary exit — not an error worth
				// surfacing.
				return
			}
		}
	}()

	// The exit status is delivered once, on a channel only the main loop reads.
	// exited is a separate broadcast so the input loop can notice a process
	// that died early WITHOUT consuming the status the main loop needs.
	waitDone := make(chan error, 1)
	exitedCh := make(chan struct{})
	go func() {
		err := cmd.Wait()
		close(exitedCh)
		waitDone <- err
	}()

	quiet := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return time.Since(lastWrite) >= PTYQuiescence
	}

	// Input is delivered only once the CLI has stopped producing output, so a
	// scenario types into a settled screen rather than into a startup race.
	for _, line := range opts.Input {
		if !awaitQuiescence(ctx, exitedCh, quiet) {
			break
		}
		mu.Lock()
		lastWrite = time.Now()
		mu.Unlock()
		if _, err := io.WriteString(f, line+"\r"); err != nil {
			break
		}
	}

	quiesced := false
	var waitErr error
	gotExit := false
	settled := false
loop:
	for {
		select {
		case waitErr = <-waitDone:
			gotExit = true
			break loop
		case <-ctx.Done():
			// Deadline. Whether the transcript had settled is the discriminator
			// between "blocked on a prompt" and "still working", so it is
			// sampled here rather than inferred later.
			quiesced = quiet()
			break loop
		case <-time.After(200 * time.Millisecond):
			quiesced = quiet()
			// The evidence check is gated on quiescence so a run ends on a
			// finished observation rather than mid-render.
			if quiesced && opts.SettledWhen != nil && opts.SettledWhen() {
				settled = true
				break loop
			}
		}
	}
	if !gotExit {
		// Either the deadline passed or the evidence arrived. In both cases the
		// CLI is still sitting in its interactive session, so it is killed here
		// rather than waited on — waiting would hang until the test timeout.
		cancel()
		<-waitDone
	}

	// Closing the master BEFORE waiting on the reader is what bounds this
	// function, and it is not the same protection runner.go gets.
	//
	// The reader returns when f.Read fails, which on a pty master needs every
	// holder of the slave side to have closed it — and Copilot spawns
	// descendants (shell tools, indexers) that inherit it and outlive a kill of
	// the parent. cmd.WaitDelay, which is exactly how Run bounds this case,
	// does nothing here: with pty.StartWithSize the child's stdio ARE the
	// slave file, so exec.Cmd creates no pipes of its own and WaitDelay has
	// nothing to close. Waiting on the reader first would therefore hang until
	// the package-wide test timeout, turning a scenario's verdict into a job
	// that dies with a stack dump.
	//
	// The grace window exists so an ordinary exit still yields a complete
	// transcript: the reader almost always finishes on its own the moment the
	// process goes away, and only a surviving descendant needs the close.
	_ = f.SetDeadline(time.Now().Add(readDrainGrace))
	select {
	case <-readDone:
	case <-time.After(readDrainGrace):
		_ = f.Close()
		<-readDone
	}

	res := PTYResult{Quiesced: quiesced, Settled: settled}
	mu.Lock()
	res.Transcript = transcript.String()
	mu.Unlock()

	if !gotExit {
		// Alive when the run ended, whether that was the deadline or the
		// evidence arriving early. Either way the CLI did not choose to exit,
		// which is the fact a scenario reasons about.
		return res
	}
	res.Exited = true
	if waitErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			t.Fatalf("copilotfixture: pty run failed: %v", waitErr)
		}
		res.ExitCode = exitErr.ExitCode()
	}
	return res
}

// awaitQuiescence blocks until output settles, the process exits, or the
// deadline passes. It reports whether the caller should still write input.
//
// exited is a broadcast channel rather than the status channel: consuming the
// exit status here would steal it from the main loop, which is the only place
// that may report it.
func awaitQuiescence(ctx context.Context, exited <-chan struct{}, quiet func() bool) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case <-exited:
			return false
		case <-time.After(100 * time.Millisecond):
			if quiet() {
				return true
			}
		}
	}
}

// ptyArgs assembles the interactive argv. It deliberately mirrors Run rather
// than sharing code with it: Run's flag set includes `--output-format json`,
// which does not exist for `-i`, and factoring the two together would invite a
// future edit that silently changes what the permission scenarios launch.
func ptyArgs(t *testing.T, opts RunOptions) []string {
	t.Helper()
	args := []string{"-C", opts.WorkDir}
	if !opts.OmitAllowAllTools {
		args = append(args, "--allow-all-tools")
	}
	args = append(args, "--no-color", "--log-level", "none")
	if opts.ResumeID != "" && opts.SessionID != "" {
		t.Fatal("copilotfixture: SessionID and ResumeID are mutually exclusive")
	}
	switch {
	case opts.ResumeID != "":
		args = append(args, "--resume="+opts.ResumeID)
	case opts.SessionID != "":
		args = append(args, "--session-id", opts.SessionID)
	}
	if opts.Model != "" {
		args = append(args, "--model="+opts.Model)
	}
	if opts.Effort != "" {
		args = append(args, "--effort="+opts.Effort)
	}
	args = append(args, opts.ExtraArgs...)
	return append(args, "-i", opts.Prompt)
}

// stripANSI removes CSI/OSC escape sequences from terminal output.
//
// Hand-rolled rather than regex-based because the TUI emits OSC strings
// terminated by BEL as well as by ST, and a single regex covering both is
// less readable than the state machine it would encode.
func stripANSI(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c != 0x1b {
			out.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(s) {
			break
		}
		switch s[i+1] {
		case '[':
			// CSI: parameters and intermediates, then a final byte in @-~.
			j := i + 2
			for j < len(s) && s[j] >= 0x20 && s[j] <= 0x3f {
				j++
			}
			if j < len(s) {
				j++
			}
			i = j
		case ']':
			// OSC: ends at BEL or ST (ESC \).
			j := i + 2
			for j < len(s) {
				if s[j] == 0x07 {
					j++
					break
				}
				if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
					j += 2
					break
				}
				j++
			}
			i = j
		default:
			i += 2
		}
	}
	return out.String()
}
