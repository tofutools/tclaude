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
// keeps producing output.
//
// This constant used to claim that two seconds is "far longer than the gaps a
// local mock-backed turn produces (the whole turn takes ~2s)". That margin does
// not exist, and the measurement in permission_smoke_test.go's blockedQuiet
// comment is what disproves it: across 1746 pty runs, a working turn's widest
// output gap is 2.2s on Linux and 5.8s on macOS. Both exceed this window, so
// Quiesced=true on a live process is NOT by itself evidence that the CLI has
// stopped for good — it is routinely true of a turn that is still working.
//
// What that means for callers: treat Quiesced as a cheap, noisy hint, never as
// the sole basis for a blocked verdict. ClassifyPermission's ordering already
// reflects this — a tool-result follow-up and a rendered dialog marker each
// decide before the quiescence arm is consulted, and the arm itself only
// applies to a process that is still alive. Sizing this constant to
// separate the two populations is not possible on macOS (see the overlap table
// in that comment), which is why the fix was to add positive evidence rather
// than to retune the window.
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

	// Elapsed is the wall clock the run cost, from launch to the moment the
	// observation loop ended.
	Elapsed time.Duration

	// MaxOutputGap is the longest the transcript stopped growing at any point
	// AFTER it started growing, sampled at the loop's tick resolution.
	//
	// This is the measurement that sizes BlockedAfter. A quiet window only
	// separates "blocked on a prompt" from "still working" if it is longer
	// than any gap working actually produces, and that is an empirical
	// quantity — every pty scenario logs it (see logPTYTiming) so the margin
	// stays visible rather than remembered.
	//
	// Startup silence is excluded on purpose, and the exclusion is the whole
	// point of the field: a launch that took six seconds to draw its first byte
	// has not shown a six-second gap in a working turn, and letting it say so
	// would talk a future tightening into a looser number than the evidence
	// supports. FirstOutput carries that quantity separately.
	MaxOutputGap time.Duration

	// FirstOutput is how long the CLI took to write its first byte. Zero means
	// it never wrote one.
	FirstOutput time.Duration

	// Blocked reports that the run ended early on BlockedAfter: alive, and
	// silent for longer than any working turn goes silent. It is the same
	// state a deadline-reaching run lands in, observed sooner.
	Blocked bool
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

	// Keystrokes are raw byte writes delivered on a wall-clock schedule from
	// the moment the CLI is launched, instead of lines typed at a settled
	// prompt.
	//
	// Input (above) waits for quiescence, which is right for "type a prompt
	// and see what happens" and useless for the opposite question: what a BUSY
	// TUI does with a keystroke. Quiescence is precisely the state that no
	// longer exists mid-turn, so a scenario that needs to type into a running
	// turn has to schedule its bytes rather than wait for a prompt.
	//
	// The bytes are written verbatim — no newline is appended — so a scenario
	// spells out exactly what a terminal would deliver: "\x03" for the cancel
	// key, "\r" for Enter. tmux send-keys delivers the same bytes, which is
	// what makes a measurement here evidence about tclaude's pane injection.
	Keystrokes []Keystroke

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

	// BlockedAfter, when non-zero, ends a run whose transcript has stopped
	// growing for this long without its evidence having arrived.
	//
	// SettledWhen is the allowed arm's early exit; this is the blocked arm's.
	// Without it a blocked run pays its whole Deadline, and blocked arms are
	// most of the permission matrix's wall clock — the deadline has to be
	// generous enough to cover the slowest startup, so every blocked arm was
	// paying for the worst case of a phase it had already finished.
	//
	// The verdict is unchanged: alive, and quiet by a wider margin than
	// Quiesced requires. What changes is when the run stops waiting for a
	// prompt that is already on screen. Size it off MaxOutputGap — the widest
	// gap a WORKING turn produces — not off intuition, and leave Deadline as
	// the outer bound so a run that never gets anywhere still lands in the
	// classifier's error arm rather than being called blocked.
	BlockedAfter time.Duration
}

// Keystroke is one scheduled raw write onto the terminal.
type Keystroke struct {
	// After is the delay from the PREVIOUS keystroke (from launch, for the
	// first one).
	After time.Duration
	// Bytes is written to the terminal verbatim.
	Bytes string
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

	start := time.Now()
	var (
		mu         sync.Mutex
		transcript strings.Builder
		lastWrite  = start
		maxGap     time.Duration
		firstWrite time.Time
	)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				mu.Lock()
				if transcript.Len() == 0 {
					firstWrite = time.Now()
				}
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

	// idle is how long the transcript has been still, and it records the widest
	// such window the run ever showed. Sampling here rather than in the reader
	// is deliberate: the widest gap is a property of when the observer looked,
	// which is exactly the resolution a decision made from it is taken at.
	//
	// maxGap only starts accumulating once something has been drawn. Node's
	// startup is silent, and counting it would put a startup-dominated number
	// in the timing log — on the very field a future blockedQuiet is meant to
	// be sized from, and in the loose direction, since the widest gap it
	// reports would not be a gap any working TURN produced. Time to first byte
	// is reported separately instead, where it can be read for what it is.
	idle := func() time.Duration {
		mu.Lock()
		defer mu.Unlock()
		gap := time.Since(lastWrite)
		if gap > maxGap && !firstWrite.IsZero() {
			maxGap = gap
		}
		return gap
	}
	sawOutput := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return transcript.Len() > 0
	}

	// Quiescence means the transcript STARTED growing and then stopped. A
	// screen that has never been drawn on is quiet by exactly the same test as
	// one showing a finished prompt, and treating the two alike is a bug that
	// showed up three separate ways: input typed into a TUI that did not exist,
	// a startup mistaken for a prompt, and a run reporting its evidence had
	// settled before the CLI had emitted a byte.
	//
	// Requiring first output here rather than at each of the three call sites
	// is what keeps them from drifting apart. It is also the conservative
	// direction for the one caller that cannot be fixed by waiting: a run that
	// reaches its deadline having drawn nothing now reports Quiesced=false, so
	// ClassifyPermission lands in its error arm — "most likely still working"
	// — rather than recording a launch that never started as blocked.
	quiet := func() bool { return sawOutput() && idle() >= PTYQuiescence }

	// Scheduled keystrokes run on their own clock, since the states they are
	// written to observe (a turn in flight, a dialog on screen) are exactly
	// the ones quiescence never arrives in.
	if len(opts.Keystrokes) > 0 {
		go func() {
			for _, k := range opts.Keystrokes {
				select {
				case <-ctx.Done():
					return
				case <-exitedCh:
					return
				case <-time.After(k.After):
				}
				if _, err := io.WriteString(f, k.Bytes); err != nil {
					return
				}
			}
		}()
	}

	// Input is delivered only once the CLI has stopped producing output, so a
	// scenario types into a settled screen rather than into a startup race.
	// quiet() will not report a screen that has drawn nothing, which is what
	// makes "settled" here mean a prompt rather than a slow Node startup — see
	// its definition. Getting this wrong cost a CI run: the bytes went nowhere,
	// the scenario waited out its deadline, and it read as the CLI ignoring a
	// command instead of a race, with a 109-second silence in the timing log.
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
	blocked := false
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
			gap := idle()
			drawn := sawOutput()
			quiesced = drawn && gap >= PTYQuiescence
			// The evidence check is gated on quiescence so a run ends on a
			// finished observation rather than mid-render.
			if quiesced && opts.SettledWhen != nil && opts.SettledWhen() {
				settled = true
				break loop
			}
			// No evidence, and silent for longer than a working turn ever goes
			// silent: the same state the deadline would have found, reached
			// without waiting for it. Checked AFTER the evidence, so a run that
			// satisfies both on the same tick is settled rather than blocked —
			// the one ordering that could turn an allowed arm into a finding.
			//
			// Nothing on screen yet is NOT that state. A launch that has not
			// written its first byte is still in Node's startup, which is
			// silent by nature and stretches under load — calling that
			// "parked on a prompt" would turn a busy runner into a finding.
			if opts.BlockedAfter > 0 && drawn && gap >= opts.BlockedAfter {
				blocked = true
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

	res := PTYResult{
		Quiesced: quiesced,
		Settled:  settled,
		Blocked:  blocked,
		Elapsed:  time.Since(start),
	}
	mu.Lock()
	res.Transcript = transcript.String()
	res.MaxOutputGap = maxGap
	if !firstWrite.IsZero() {
		res.FirstOutput = firstWrite.Sub(start)
	}
	mu.Unlock()

	if gotExit {
		res.Exited = true
		if waitErr != nil {
			var exitErr *exec.ExitError
			if !errors.As(waitErr, &exitErr) {
				t.Fatalf("copilotfixture: pty run failed: %v", waitErr)
			}
			res.ExitCode = exitErr.ExitCode()
		}
	}
	// Alive when the run ended — whether that was the deadline, the evidence
	// arriving, or the quiet window closing — is the fact a scenario reasons
	// about, so it is reported either way.
	logPTYTiming(t, res)
	return res
}

// logPTYTiming records what a run cost and why it ended.
//
// It is not debug output. The suite's early exits — SettledWhen for an allowed
// arm, BlockedAfter for a blocked one — are only as sound as the margin
// between the quiet window they trust and the widest gap a working turn
// actually produces, and that margin drifts as the CLI changes. Printing it on
// every run means a future tightening is argued from the log of a real job
// rather than from the last person's recollection.
func logPTYTiming(t *testing.T, res PTYResult) {
	t.Helper()
	end := "deadline"
	switch {
	case res.Exited:
		end = "exited"
	case res.Settled:
		end = "settled"
	case res.Blocked:
		end = "blocked-early"
	}
	t.Logf("PTY TIMING elapsed=%s ended=%s first-output=%s max-output-gap=%s",
		res.Elapsed.Round(100*time.Millisecond), end,
		res.FirstOutput.Round(100*time.Millisecond),
		res.MaxOutputGap.Round(100*time.Millisecond))
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
