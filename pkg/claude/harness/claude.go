package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

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
func (claudeAsker) StreamFilter(w io.Writer) io.Writer {
	return &claudeStreamFilter{out: w}
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
// when no deltas streamed. Every other event type (system/init, message_start,
// thinking_delta, the per-message `assistant` snapshots, …) is ignored, so
// reasoning and tool-use chatter never reach the user's stdout.
type claudeStreamEvent struct {
	Type   string `json:"type"`
	Result string `json:"result"`
	Event  struct {
		Type  string `json:"type"`
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
// Memory is bounded by the largest single event line, not the whole turn: each
// complete line is dropped from buf the moment its newline arrives. claude's
// stream-json is one JSON object per line, so the cap is one event — the
// terminal `result` (which carries the full answer) being the biggest — never
// the cumulative stream.
type claudeStreamFilter struct {
	out         io.Writer
	buf         []byte // incomplete trailing line carried between Write calls
	wroteText   bool   // at least one text_delta has been forwarded
	endedNL     bool   // the last bytes forwarded ended in a newline
	finalResult string // result-event text, emitted by Flush only if nothing streamed
	writeErr    error  // first error writing to out; returned by Flush
}

func (f *claudeStreamFilter) Write(p []byte) (int, error) {
	f.buf = append(f.buf, p...)
	for {
		i := bytes.IndexByte(f.buf, '\n')
		if i < 0 {
			break
		}
		line := f.buf[:i]
		f.buf = f.buf[i+1:]
		f.consume(line)
	}
	// Always report the full length consumed: a short write would make claude's
	// stdout pipe think the reader failed and tear the turn down. Real write
	// errors are stashed in writeErr and reported by Flush instead.
	return len(p), nil
}

// consume parses one complete event line and forwards its visible text. A line
// that isn't valid JSON, or is an event type we don't render, is silently
// dropped — the stream legitimately carries many such lines.
func (f *claudeStreamFilter) consume(line []byte) {
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
		if ev.Event.Type == "content_block_delta" && ev.Event.Delta.Type == "text_delta" {
			f.emit(ev.Event.Delta.Text)
		}
	case "result":
		// The final answer (or, on failure, the error message). Kept only as a
		// fallback: on a normal turn the same text already streamed as deltas,
		// so Flush ignores it; it's surfaced only when nothing streamed, so an
		// error or a delta-less stream is never silent.
		f.finalResult = ev.Result
	}
}

func (f *claudeStreamFilter) emit(s string) {
	if s == "" || f.writeErr != nil {
		return
	}
	if _, err := io.WriteString(f.out, s); err != nil {
		f.writeErr = err
		return
	}
	f.wroteText = true
	f.endedNL = strings.HasSuffix(s, "\n")
}

// Flush is called once after the claude process exits. It surfaces a buffered
// final answer/error when nothing streamed, and leaves the cursor on a fresh
// line — text_delta chunks carry no trailing newline, so without this the
// shell prompt would resume mid-answer.
func (f *claudeStreamFilter) Flush() error {
	// Process any final line the stream didn't newline-terminate. claude's
	// stream-json terminates every event (including the last) with a newline, so
	// this is belt-and-suspenders — but it keeps a result event without a
	// trailing newline from being silently dropped.
	if len(bytes.TrimSpace(f.buf)) > 0 {
		f.consume(f.buf)
		f.buf = nil
	}
	if !f.wroteText && f.finalResult != "" {
		f.emit(f.finalResult)
	}
	if f.wroteText && !f.endedNL && f.writeErr == nil {
		if _, err := io.WriteString(f.out, "\n"); err != nil {
			f.writeErr = err
		}
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
// three are supported, so CC behavior is unchanged when call sites gate
// injections on these tokens.
type claudeLifecycle struct{}

func (claudeLifecycle) RenameCommand() string   { return "/rename" }
func (claudeLifecycle) CompactCommand() string  { return "/compact" }
func (claudeLifecycle) SoftExitCommand() string { return "/exit" }

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
