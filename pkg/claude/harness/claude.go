package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// init registers the Claude Code harness as the default. Claude Code is
// the harness tclaude was built around; every contract below preserves
// its existing behavior exactly — this package is a refactor seam, not a
// behavior change.
func init() {
	Register(&Harness{
		Name:        DefaultName,
		DisplayName: "Claude Code",
		Spawn:       claudeSpawner{},
		Ask:         claudeAsker{},
		Models:      claudeModels{},
		Life:        claudeLifecycle{},
		Convs:       claudeConvStore{},
		// Claude Code's OS sandbox lives in settings.json, not a launch flag;
		// claudeSandbox models a small inherit/on/off tri-state that the
		// spawner translates to a per-session `--settings` override (the
		// default, inherit, adds nothing — see claude_sandbox.go).
		Sandbox: claudeSandbox{},
		// Claude Code's approval posture IS its permission mode; claudeApproval
		// carries the `--permission-mode` enum (+ an inherit default that adds
		// nothing) through the harness-agnostic Approval field, translated by
		// the spawner to `--permission-mode` — see claude_approval.go.
		Approval: claudeApproval{},
		// Claude Code accepts a preset conv-id (--session-id), a launch-time
		// display name (--name, written as a custom-title turn just like
		// /rename), and a positional first-turn prompt — so the daemon can
		// spawn it fully enrolled and skip the post-connect tmux injection.
		LaunchEnrollment: true,
	})
}

// claudeSpawner builds the `claude` invocation that runs inside the tmux
// pane. The logic moved here verbatim from session.buildClaudeCmd — the
// "unset omits the flag" guarantee and the shell-quoting are unchanged.
type claudeSpawner struct{}

func (claudeSpawner) Binary() string { return "claude" }

// BuildCommand assembles the `claude` invocation: env exports + the
// binary, an optional --resume, an optional --effort and --model (each
// appended only when an explicit value was chosen — empty leaves claude
// on its own default), then any post-`--` passthrough args. effort and
// model are validated single tokens, but everything is shell-quoted
// anyway; the passthrough args are shell-quoted individually. Kept pure
// so the "unset omits the flag" guarantee is unit-testable without tmux.
func (claudeSpawner) BuildCommand(spec SpawnSpec) string {
	cmd := spec.EnvExports + "claude"
	if spec.ResumeID != "" {
		cmd += " --resume " + spec.ResumeID
	}
	// --session-id pins the conversation id for a FRESH launch only (a
	// resume continues an existing id). It lets the daemon know the conv-id
	// before the pane starts, so the rename + welcome ride in as launch args
	// instead of post-launch tmux injections. Quoted defensively even though
	// it is a validated UUID.
	if spec.SessionID != "" && spec.ResumeID == "" {
		cmd += " --session-id " + clcommon.ShellQuoteArg(spec.SessionID)
	}
	// --name sets the display name at launch; Claude Code records it as a
	// `custom-title` turn the same way /rename does. Quoted because the name
	// is free-ish text handed to `sh -c`.
	if spec.Name != "" {
		cmd += " --name " + clcommon.ShellQuoteArg(spec.Name)
	}
	if spec.Effort != "" {
		// Quote defensively even though effort is a validated single
		// token: this string is handed to `sh -c`, so quoting keeps the
		// safety local here rather than trusting every caller to have
		// validated first. For a clean level it is a no-op.
		cmd += " --effort " + clcommon.ShellQuoteArg(spec.Effort)
	}
	if spec.Model != "" {
		// Quoting is load-bearing here, not just defensive: the `[1m]`
		// aliases contain brackets, which sh would otherwise treat as a
		// glob pattern.
		cmd += " --model " + clcommon.ShellQuoteArg(spec.Model)
	}
	// Claude Code has no `--sandbox` flag; its OS sandbox is a settings.json
	// block. The `on`/`off` launch-containment modes are delivered as a
	// per-session `--settings '<json>'` override that merges over the user's
	// settings (only managed/policy settings outrank it). The default mode
	// (inherit) yields "" here, so an un-chosen spawn emits no flag and the
	// agent stays on the operator's own settings.json config. The JSON is a
	// static, machine-built payload (claudeSandboxSettingsJSON), but shell-
	// quoted as one arg anyway since it's handed to `sh -c`.
	if s := claudeSandboxSettingsJSON(spec.SandboxMode); s != "" {
		cmd += " --settings " + clcommon.ShellQuoteArg(s)
	}
	// Claude Code's approval posture is its permission mode: the harness-
	// agnostic ApprovalPolicy carries a `--permission-mode` value for Claude
	// (validated by claudeApproval; inherit/blank already collapsed to "" so an
	// un-chosen spawn omits the flag and keeps the agent on settings.json + the
	// agentd approval popup). Quoted defensively even though it's a validated
	// enum, since it's handed to `sh -c`.
	if spec.ApprovalPolicy != "" {
		cmd += " --permission-mode " + clcommon.ShellQuoteArg(spec.ApprovalPolicy)
	}
	if len(spec.ExtraArgs) > 0 {
		quoted := make([]string, len(spec.ExtraArgs))
		for i, a := range spec.ExtraArgs {
			quoted[i] = clcommon.ShellQuoteArg(a)
		}
		cmd += " " + strings.Join(quoted, " ")
	}
	// `claude [options] [prompt]` — a trailing positional prompt the
	// interactive session submits itself at launch (verified against the
	// `claude --help` "[prompt] Your prompt" arg). The daemon spawn path
	// uses it to deliver the agent's welcome turn without a tmux send-keys
	// injection. Only on a FRESH launch: a --resume continues an existing
	// conversation and takes no launch prompt. Quoted as a single arg so the
	// whole prompt is one positional, never split into stray flags/words.
	if spec.InitialPrompt != "" && spec.ResumeID == "" {
		cmd += " " + clcommon.ShellQuoteArg(spec.InitialPrompt)
	}
	// --remote-control arms Claude Code's built-in Remote Access at launch
	// (claude.ai/code + the mobile app), for an agent spawned phone-reachable
	// (JOH-258). Emitted LAST — after the positional [prompt] — on purpose: the
	// flag takes an OPTIONAL [name], which commander fills from the NEXT token
	// unless it starts with '-'. Putting it first would make it swallow a bare
	// trailing prompt on the direct-CLI path (`claude --remote-control 'do X'` →
	// name="do X", prompt lost), and only a following --flag (which the daemon's
	// --session-id happens to provide, but the direct CLI may not) prevents that.
	// Emitting it last means nothing follows it, so its [name] is always empty
	// and the prompt is parsed as the positional on EVERY path. Bare boolean
	// flag, no value. Codex has no equivalent; its spawner ignores RemoteControl.
	if spec.RemoteControl {
		cmd += " --remote-control"
	}
	return cmd
}

// claudeAsker builds the `claude` argv for a one-shot `tclaude ask` turn
// (JOH-250). It returns an argv slice rather than a `sh -c` string because an
// ask is exec'd directly with no shell: the question is one element of the
// slice, so it needs no shell-quoting and can never be split into stray
// flags/words.
//
// The shape mirrors the spawner's resume-vs-fresh fork, but for the ask flow:
//   - fresh:  claude [-p] --session-id <uuid> [--effort e] [--model m] "<prompt>"
//   - resume: claude [-p] --resume    <id>   [--effort e] [--model m] "<prompt>"
//
// --session-id pins a caller-minted conv-id for a fresh thread (so the caller
// records the (terminal,cwd)→conv mapping); --resume continues that thread on
// later turns. `-p` is non-interactive capture mode; without it claude runs
// interactively, attached to the caller's TTY, so the agent can ask the human
// back. --effort / --model are appended only when set (an empty token leaves
// claude on its own default), validated by the caller via the ModelCatalog.
// The prompt is always the trailing positional, emitted LAST so no variadic
// flag (e.g. --add-dir) could swallow it.
type claudeAsker struct{}

func (claudeAsker) BuildAskArgv(spec AskSpec) []string {
	argv := []string{"claude"}
	if spec.Print {
		argv = append(argv, "-p")
	}
	// Streaming is a print-mode-only refinement: swap the default `text` output
	// (which buffers the whole turn and prints the answer only at the end) for
	// the JSONL event stream, so claudeStreamFilter can render the answer token
	// by token. `--verbose` is mandatory — `claude -p --output-format
	// stream-json` errors without it — and `--include-partial-messages` is what
	// promotes the stream from one-event-per-complete-message up to the
	// token-level `text_delta` chunks the filter reads. Guarded on Print so a
	// stray Stream on an interactive spec can't emit a capture-only flag.
	if spec.Stream && spec.Print {
		argv = append(argv, "--output-format", "stream-json", "--verbose", "--include-partial-messages")
	}
	switch {
	case spec.ResumeID != "":
		argv = append(argv, "--resume", spec.ResumeID)
	case spec.SessionID != "":
		argv = append(argv, "--session-id", spec.SessionID)
	}
	if spec.Effort != "" {
		argv = append(argv, "--effort", spec.Effort)
	}
	if spec.Model != "" {
		argv = append(argv, "--model", spec.Model)
	}
	// The question is the trailing positional. In PRINT mode it goes behind a
	// `--` end-of-options marker: the prompt is fully untrusted (a typed
	// question, or piped data like a `git diff` whose lines start with `-`), so
	// `--` stops claude parsing a leading-dash prompt as a flag (verified:
	// `claude -p -- "--version"` answers as the model instead of printing the
	// version). In INTERACTIVE mode we must NOT emit `--`: it suppresses claude's
	// "submit the positional prompt at launch" behavior (the same launch-arg
	// path the spawn flow relies on, which carries no `--`), leaving the TUI open
	// with no question submitted. Interactive prompts are typed questions that
	// rarely start with `-`, and it's still a single argv element (no shell), so
	// the residual flag-parse risk is acceptable there.
	if spec.Prompt != "" {
		if spec.Print {
			argv = append(argv, "--")
		}
		argv = append(argv, spec.Prompt)
	}
	return argv
}

// PreMintsConvID is true: Claude Code accepts a caller-minted conv-id for a
// fresh ask (`--session-id <uuid>`, emitted above from AskSpec.SessionID), so
// `tclaude ask` records the (terminal,cwd)→conv mapping up front rather than
// discovering it after the turn.
func (claudeAsker) PreMintsConvID() bool { return true }

// NoisyCaptureStderr is false: `claude -p` prints just the answer to stdout and
// keeps stderr quiet, so `tclaude ask` has no transcript to hide.
func (claudeAsker) NoisyCaptureStderr() bool { return false }

// StreamFilter wraps the caller's stdout with a filter that reads Claude Code's
// `--output-format stream-json` output — newline-delimited JSON events — and
// forwards only the assistant's visible text, token by token, as it streams.
// It is the read half of the streaming contract whose write half is the
// stream-json flags BuildAskArgv emits for a Stream spec. Implementing it is
// what makes claudeAsker a StreamAsker (so Harness.SupportsAskStream is true).
//
// When smooth is true the filter SMOOTHS the answer: instead of dumping each
// text_delta the instant it arrives (which lands in visible bursts — a few
// words, a pause, then a whole paragraph), it paces the characters out at an
// estimated steady rate so the answer "types" itself fluidly. When false it
// forwards chunks the instant they arrive (the original behavior). Smoothing is
// purely cosmetic and only ever runs against a real TTY — the ask flow wires
// this filter in only then (a piped or captured stdout keeps the buffered path)
// and resolves the smooth flag from --no-smoothing / TCLAUDE_ASK_SMOOTH.
func (claudeAsker) StreamFilter(w io.Writer, smooth bool, status StreamStatus) io.Writer {
	f := &claudeStreamFilter{
		out:    w,
		smooth: smooth,
		status: status,
		clock:  time.Now,
		sleep:  time.Sleep,
	}
	f.cond = sync.NewCond(&f.mu)
	return f
}

// claudeStreamEvent is the slice of Claude Code's stream-json event schema this
// filter cares about. Each stdout line is one such JSON object. The visible
// answer arrives as `text_delta` chunks:
//
//	{"type":"stream_event","event":{"type":"content_block_delta",
//	 "delta":{"type":"text_delta","text":"…"}}}
//
// and the terminal `{"type":"result","result":"…"}` event carries the full
// final answer (or, on failure, the error message) — used only as a fallback
// when no deltas streamed.
//
// We also watch `content_block_start` to learn where one text SECTION ends and
// the next begins:
//
//	{"type":"stream_event","event":{"type":"content_block_start",
//	 "content_block":{"type":"text"}}}
//
// A turn can be several sections — the model emits text, calls a tool, then
// emits more text in a fresh block/message — and the deltas of different
// sections carry no separator, so without this they would run together. Every
// other event type (system/init, message_start, message_stop, thinking_delta,
// tool_use blocks, the per-message `assistant` snapshots, …) is still ignored,
// so reasoning and tool-use chatter never reach the user's stdout.
type claudeStreamEvent struct {
	Type   string `json:"type"`
	Result string `json:"result"`
	Event  struct {
		Type         string `json:"type"`
		ContentBlock struct {
			Type string `json:"type"`
		} `json:"content_block"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`
}

// claudeStreamFilter implements io.Writer (+ Flush, the AskStreamFlusher half):
// claude's stdout is written into it, and it writes the extracted visible text
// to out. It buffers a partial trailing line across Write calls (the pipe can
// split a write mid-line) and only ever acts on complete, newline-terminated
// events. A failure writing to out is stashed and surfaced from Flush so a
// short write never propagates back to claude's pipe (which would abort it).
//
// When smooth is set, the extracted text is not written straight through:
// Write/consume only ENQUEUE it (the JSON parsing stays on claude's stdout
// goroutine) and a background pacer goroutine drains
// the queue character-by-character at an estimated steady rate (see pace /
// pacingCPS), so the answer types itself fluidly instead of arriving in chunky
// bursts. Flush signals end-of-input and waits for the queue to drain before
// returning. With smooth unset the text is written straight through, exactly as
// before — no goroutine, no pacing.
//
// Concurrency: mu guards ALL shared state — buf, pending, the arrival stats,
// finalResult, wroteText/endedNL, started/inputDone, writeErr — so the filter is
// self-synchronizing and never leans on an external happens-before. Write parses
// and enqueues entirely under mu; the pacer drains under mu; Flush consumes the
// trailing buf and reads started/finalResult under mu, releasing it only while it
// blocks on the pacer's doneCh. cond wakes the pacer when text is enqueued or
// input completes. (In the real ask flow claude's stdout goroutine finishes
// before Flush is even called, so contention is near-zero — but the locking means
// a future caller that streamed in from several goroutines, or flushed
// concurrently with a write, still would not race. The previous version relied on
// that external ordering; this no longer does.)
//
// Memory is bounded by the largest single event line plus the un-emitted text
// backlog — at steady state a fraction of a second of output, since the pacer
// keeps pace with the model. Each complete JSON line is dropped from buf the
// moment its newline arrives; claude's stream-json is one JSON object per line.
type claudeStreamFilter struct {
	out    io.Writer
	smooth bool
	status StreamStatus // optional "working…" indicator; nil = none

	buf []byte // incomplete trailing line carried between Write calls (guarded by mu)

	mu          sync.Mutex
	cond        *sync.Cond
	pending     []rune // parsed visible text awaiting paced emit (smooth mode)
	inputDone   bool   // Flush has been called: no more text will arrive
	started     bool   // the pacer goroutine has been launched
	wroteText   bool   // at least one text_delta rune has been forwarded to out
	endedNL     bool   // the last bytes forwarded ended in a newline
	emittedText bool   // logical: some visible text has been emitted (across all sections)
	emitEndsNL  bool   // logical: the last emitted text ended in a newline (drives section separators)
	finalResult string // result-event text, emitted by Flush only if nothing streamed
	writeErr    error  // first error writing to out; returned by Flush

	// Arrival stats feeding the pacing rate estimate, updated as deltas arrive.
	haveFirst bool      // the first delta has been seen (first is valid)
	first     time.Time // arrival time of the first delta
	arrived   int       // total runes received across all deltas
	chunks    int       // number of text_delta events received

	clock  func() time.Time    // time.Now; swappable in tests
	sleep  func(time.Duration) // time.Sleep; swappable in tests
	doneCh chan struct{}       // closed when the pacer goroutine exits
}

func (f *claudeStreamFilter) Write(p []byte) (int, error) {
	f.mu.Lock()
	f.buf = append(f.buf, p...)
	for {
		i := bytes.IndexByte(f.buf, '\n')
		if i < 0 {
			break
		}
		line := f.buf[:i]
		f.buf = f.buf[i+1:]
		f.consumeLocked(line)
	}
	f.mu.Unlock()
	// Always report the full length consumed: a short write would make claude's
	// stdout pipe think the reader failed and tear the turn down. Real write
	// errors are stashed in writeErr and reported by Flush instead.
	return len(p), nil
}

// consumeLocked parses one complete event line and forwards its visible text. A
// line that isn't valid JSON, or is an event type we don't render, is silently
// dropped — the stream legitimately carries many such lines. Caller holds mu.
func (f *claudeStreamFilter) consumeLocked(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	var ev claudeStreamEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return
	}
	switch ev.Type {
	case "stream_event":
		switch ev.Event.Type {
		case "content_block_start":
			if ev.Event.ContentBlock.Type == "text" {
				f.startTextSectionLocked()
			}
		case "content_block_delta":
			if ev.Event.Delta.Type == "text_delta" {
				f.emitLocked(ev.Event.Delta.Text)
			}
		}
	case "result":
		// The final answer (or, on failure, the error message). Kept only as a
		// fallback: on a normal turn the same text already streamed as deltas,
		// so Flush ignores it; it's surfaced only when nothing streamed, so an
		// error or a delta-less stream is never silent.
		f.finalResult = ev.Result
	}
}

// emitLocked forwards one delta's visible text. In smooth mode it ENQUEUES the
// text for the pacer to type out; otherwise it writes straight through (the
// original behavior). An empty string and a prior write error are no-ops either
// way. Caller holds mu.
func (f *claudeStreamFilter) emitLocked(s string) {
	if s == "" {
		return
	}
	// Track the logical end-of-text state (in stream order, before any pacing) so
	// startTextSectionLocked knows whether the previous section already ended in a
	// newline.
	f.emittedText = true
	f.emitEndsNL = strings.HasSuffix(s, "\n")
	if !f.smooth {
		f.putLocked(s)
		return
	}
	f.enqueueLocked(s)
}

// startTextSectionLocked is called when a new text content block begins. The
// model splits a turn into sections — text, a tool call, then more text in a
// fresh block/message — whose deltas carry no separator, so consecutive sections
// would otherwise run together ("…the files.The largest is…"). When a new
// section starts after text that didn't already end in a newline, insert one so
// the sections read as separate lines. Guarded on emitEndsNL so a section the
// model already terminated isn't double-spaced, and on emittedText so the very
// first section gets no leading blank line.
func (f *claudeStreamFilter) startTextSectionLocked() {
	if f.emittedText && !f.emitEndsNL {
		f.emitLocked("\n")
	}
}

// enqueueLocked appends a delta's text to the pending queue, updates the arrival
// stats the pacer uses to estimate speed, and (on the first delta) launches the
// pacer goroutine. Caller holds mu.
func (f *claudeStreamFilter) enqueueLocked(s string) {
	rs := []rune(s)
	if !f.haveFirst {
		f.first = f.clock()
		f.haveFirst = true
	}
	f.arrived += len(rs)
	f.chunks++
	f.pending = append(f.pending, rs...)
	if !f.started {
		f.started = true
		f.doneCh = make(chan struct{})
		go f.pace()
	}
	f.cond.Signal()
}

const (
	// streamPaceTick is how often the pacer emits a small batch. ~60 Hz reads as
	// continuous typing to the eye while keeping the emit count (and syscalls)
	// modest even for a fast model.
	streamPaceTick = 16 * time.Millisecond
	// streamDumpThreshold caps the backlog: past this many un-emitted runes the
	// model is producing far faster than anything could plausibly be "typed", so
	// the pacer stops pacing and flushes the rest at once rather than trail the
	// model by seconds.
	streamDumpThreshold = 4096
)

// pace is the background typewriter loop (smooth mode). It drains pending at the
// rate pacingCPS estimates, emitting in small per-tick batches so a fast model
// still keeps up while a slow trickle stays smooth. It exits once input is done
// and the queue is empty, or as soon as a write to out has failed.
func (f *claudeStreamFilter) pace() {
	defer close(f.doneCh)
	for {
		f.mu.Lock()
		for len(f.pending) == 0 && !f.inputDone && f.writeErr == nil {
			f.cond.Wait()
		}
		if f.writeErr != nil {
			f.pending = nil // a write failed; drop the rest, Flush surfaces the error
			f.mu.Unlock()
			return
		}
		if len(f.pending) == 0 { // implies inputDone: nothing left to type
			f.mu.Unlock()
			return
		}
		n, delay := f.planEmitLocked()
		f.putLocked(string(f.pending[:n]))
		f.pending = f.pending[n:]
		drained := f.inputDone && len(f.pending) == 0
		f.mu.Unlock()

		// Skip the inter-tick wait when we just finished, or when a dump cleared
		// the backlog in one shot — loop straight back instead of pausing.
		if drained || delay <= 0 {
			continue
		}
		f.sleep(delay)
	}
}

// planEmitLocked decides how many runes to emit this tick and how long to wait
// afterwards. Caller holds mu.
func (f *claudeStreamFilter) planEmitLocked() (n int, delay time.Duration) {
	backlog := len(f.pending)
	if backlog >= streamDumpThreshold {
		return backlog, 0 // hopelessly behind: dump the rest now
	}
	var elapsed time.Duration
	if f.haveFirst {
		elapsed = f.clock().Sub(f.first)
	}
	cps := pacingCPS(backlog, f.arrived, f.chunks, elapsed)
	n = min(max(int(math.Round(cps*streamPaceTick.Seconds())), 1), backlog)
	return n, streamPaceTick
}

// pacingCPS estimates the characters-per-second to type the streamed answer at.
// It is deliberately a pure function of the observed stream so the policy can be
// unit-tested without goroutines or a clock.
//
// The core is self-stabilizing: aim to drain the CURRENT backlog over a short
// fixed horizon, so the rate rises when we fall behind and eases off as we catch
// up. At steady state, with the model producing at rate M, the backlog settles
// at ~M*horizon and we emit at ~M — i.e. we track the model's own speed with a
// constant sub-second lag, no per-stream tuning needed.
//
// Once at least two chunks have arrived the model's average rate can also be
// measured directly (arrived/elapsed) and is used as a FLOOR, so a steady fast
// stream is never throttled below the speed it is actually arriving at (this is
// the "estimate from ≥2 chunks" idea — but we start typing on chunk one rather
// than stalling for it, then lock onto the measured rate once it's available).
// Everything is clamped to a sane [min,max] so one tiny first chunk doesn't
// crawl and a big burst doesn't machine-gun.
func pacingCPS(backlog, arrived, chunks int, elapsed time.Duration) float64 {
	const (
		minCPS  = 25.0  // floor: never crawl slower than a brisk typist
		maxCPS  = 480.0 // ceiling: faster than this is indistinguishable from instant
		horizon = 0.55  // seconds to drain the current backlog at steady state
	)
	cps := float64(backlog) / horizon
	if chunks >= 2 && elapsed > 0 {
		cps = max(cps, float64(arrived)/elapsed.Seconds())
	}
	return min(max(cps, minCPS), maxCPS)
}

// putLocked writes s to out and updates the forwarded-text bookkeeping. Caller
// holds mu. A first write error is stashed and silences all further writes; it
// is surfaced by Flush. (Returning it from Write would make os/exec tear the
// claude pipe down mid-turn.)
func (f *claudeStreamFilter) putLocked(s string) {
	if s == "" || f.writeErr != nil {
		return
	}
	// A visible character is about to hit stdout — let the indicator erase itself
	// (and note the write) first, so it never overlaps the answer. Called before
	// EVERY stdout write (this is the single funnel for all of them: paced emit,
	// unsmoothed passthrough, the Flush fallback, AND the closing newline), which
	// is also what lets the indicator re-appear during a mid-stream stall and
	// clear again on resume.
	if f.status != nil {
		f.status.BeforeOutput()
	}
	if _, err := io.WriteString(f.out, s); err != nil {
		f.writeErr = err
		return
	}
	f.wroteText = true
	f.endedNL = strings.HasSuffix(s, "\n")
}

// Flush is called once after the claude process exits. In smooth mode it tells
// the pacer no more text is coming and waits for the queue to drain, so the
// whole answer is on screen before we return. It then surfaces a buffered final
// answer/error when nothing streamed, and leaves the cursor on a fresh line —
// text_delta chunks carry no trailing newline, so without this the shell prompt
// would resume mid-answer.
func (f *claudeStreamFilter) Flush() error {
	f.mu.Lock()
	// Process any final line the stream didn't newline-terminate. claude's
	// stream-json terminates every event (including the last) with a newline, so
	// this is belt-and-suspenders — but it keeps a result event without a
	// trailing newline from being silently dropped. Must run BEFORE we signal
	// input-done, since it can enqueue a last delta.
	if len(bytes.TrimSpace(f.buf)) > 0 {
		f.consumeLocked(f.buf)
		f.buf = nil
	}
	smoothDrain := f.smooth && f.started
	if smoothDrain {
		f.inputDone = true
		f.cond.Signal()
	}
	f.mu.Unlock()

	if smoothDrain {
		<-f.doneCh // wait for the pacer to type out the remaining backlog
	}

	// The pacer has exited (or never ran), so this bookkeeping has no concurrent
	// writer; mu still guards it for any future caller and keeps putLocked's
	// invariant.
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.wroteText && f.finalResult != "" {
		f.putLocked(f.finalResult)
	}
	if f.wroteText && !f.endedNL {
		// Route the closing newline through putLocked as well, so the indicator is
		// stopped before it (the single-funnel invariant putLocked documents) and a
		// stashed writeErr is honored. Without this, a still-shown inline indicator
		// could be restored-and-cleared by the later spinner teardown ONTO the line
		// above this newline — a stray cursor/clear artifact.
		f.putLocked("\n")
	}
	return f.writeErr
}

// claudeModels delegates to the curated clcommon validators so the model
// and effort knowledge stays in one place; the catalog is the
// harness-agnostic dispatch point that a future codex catalog parallels.
type claudeModels struct{}

func (claudeModels) ValidateModel(s string) (string, error) {
	return clcommon.ValidateModel(s)
}

func (claudeModels) ValidateEffort(s string) (string, error) {
	return clcommon.ValidateEffort(s)
}

// Models returns a copy so callers can't mutate the shared source list.
func (claudeModels) Models() []string {
	return slices.Clone(clcommon.ValidModels)
}

// EffortLevels returns a copy so callers can't mutate the shared source list.
func (claudeModels) EffortLevels() []string {
	return slices.Clone(clcommon.ValidEffortLevels)
}

// claudeLifecycle names Claude Code's in-pane control slash commands. All
// are supported, so CC behavior is unchanged when call sites gate
// injections on these tokens. RemoteControlCommand toggles CC's built-in
// Remote Access (claude.ai/code + the Claude mobile app); see JOH-254.
type claudeLifecycle struct{}

func (claudeLifecycle) RenameCommand() string        { return "/rename" }
func (claudeLifecycle) CompactCommand() string       { return "/compact" }
func (claudeLifecycle) SoftExitCommand() string      { return "/exit" }
func (claudeLifecycle) RemoteControlCommand() string { return "/remote-control" }

// claudeConvStore assembles conversations from Claude Code's storage
// model — one cwd-indexed `.jsonl` per conv under ~/.claude/projects — by
// delegating to the existing convops read paths. It's the reference impl
// the Codex ConvStore (JOH-152) plugs in beside; the production callers
// (conv ls / search / dashboard) are rewired to enumerate every
// registered harness in the multi-harness enumeration slice (JOH-153).
type claudeConvStore struct{}

// ListConvs returns the conversations under cwd's Claude project dir, or —
// when cwd is the empty sentinel — every indexed conversation across all
// projects (served from the conv_index cache, the same fast path watch
// mode uses).
func (claudeConvStore) ListConvs(cwd string) ([]convops.SessionEntry, error) {
	if cwd == "" {
		return convops.LoadEntriesFromDB("")
	}
	idx, err := convops.LoadSessionsIndex(convops.GetClaudeProjectPath(cwd))
	if err != nil {
		return nil, err
	}
	return idx.Entries, nil
}

// Resolve maps an id prefix to a conv via the conv_index cache. It
// distinguishes the three outcomes the contract requires: no match
// (nil, nil), an unreadable store (nil, err), and an ambiguous prefix
// (nil, err) — never collapsing the latter two into "not found". An exact
// id match always wins over prefix matches.
func (claudeConvStore) Resolve(idPrefix, cwd string, global bool) (*ConvRef, error) {
	if idPrefix == "" {
		return nil, nil
	}

	var rows []*db.ConvIndexRow
	var err error
	if global {
		rows, err = db.ListAllConvIndex()
	} else {
		rows, err = db.ListConvIndex(convops.GetClaudeProjectPath(cwd))
	}
	if err != nil {
		return nil, fmt.Errorf("resolve conversation %q: %w", idPrefix, err)
	}

	// An exact id match is unambiguous regardless of how many other ids
	// share it as a prefix.
	for _, r := range rows {
		if r.ConvID == idPrefix {
			return claudeConvRef(r), nil
		}
	}

	var matches []*db.ConvIndexRow
	for _, r := range rows {
		if strings.HasPrefix(r.ConvID, idPrefix) {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return claudeConvRef(matches[0]), nil
	default:
		return nil, fmt.Errorf("ambiguous conversation id %q: matches %d conversations", idPrefix, len(matches))
	}
}

// Title returns the conv's display title, refreshing from the `.jsonl` if
// it changed on disk. Priority mirrors SessionEntry.DisplayTitle:
// customTitle > summary > first prompt. An unknown conv is ("", nil).
func (claudeConvStore) Title(convID string) (string, error) {
	row := convops.RefreshConvIndexEntry(convID)
	if row == nil {
		return "", nil
	}
	switch {
	case row.CustomTitle != "":
		return row.CustomTitle, nil
	case row.Summary != "":
		return row.Summary, nil
	default:
		return row.FirstPrompt, nil
	}
}

// SetTitle is a guard for Claude Code: its title store is the `.jsonl`
// customTitle turn, which only a live CC pane writes (via the `/rename`
// slash command). There is no direct-write path, so agentd renames a CC
// conv by injecting `/rename` (gated on Lifecycle.RenameCommand) and never
// calls this. Returning an error rather than silently writing
// conv_index.custom_title is deliberate: a direct conv_index write would
// be clobbered by the next .jsonl rescan (which finds no customTitle turn).
func (claudeConvStore) SetTitle(convID, title string) error {
	return fmt.Errorf("claude renames via the %q slash injection, not a direct title write", "/rename")
}

// Exists reports whether convID's `.jsonl` is still on disk under cwd's
// Claude project dir — the cwd-scoped store Claude Code resumes from. A
// present file is (true, nil); a confirmed-absent one is (false, nil); any
// other stat error (e.g. permission) is (false, err) so the ask caller can
// keep the thread rather than self-heal on a transient failure. This is the
// harness-agnostic move of `tclaude ask`'s old hardcoded Claude probe.
func (claudeConvStore) Exists(convID, cwd string) (bool, error) {
	if convID == "" {
		return false, nil
	}
	p := filepath.Join(convops.GetClaudeProjectPath(cwd), convID+".jsonl")
	switch _, err := os.Stat(p); {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, err
	}
}

func claudeConvRef(r *db.ConvIndexRow) *ConvRef {
	return &ConvRef{
		ConvID:      r.ConvID,
		ProjectPath: r.ProjectPath,
		Harness:     r.Harness,
	}
}
