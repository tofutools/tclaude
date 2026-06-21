package harness

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/convops"
)

// build is a tiny helper that runs the claude harness's command builder
// for the given spec fields, mirroring the old session.buildClaudeCmd
// signature so these acceptance checks read the same as before the seam.
func build(env, resume, effort, model string, extra []string) string {
	return claudeSpawner{}.BuildCommand(SpawnSpec{
		EnvExports: env,
		ResumeID:   resume,
		Effort:     effort,
		Model:      model,
		ExtraArgs:  extra,
	})
}

// TestClaudeSpawner_Model is the acceptance check for the regular
// `tclaude session new` surface: an unset model must NOT add --model to
// the claude invocation (claude keeps its own default), and an explicit
// alias must append `--model <alias>`.
func TestClaudeSpawner_Model(t *testing.T) {
	// Unset → no --model anywhere in the command.
	if got := build("", "", "", "", nil); strings.Contains(got, "--model") {
		t.Fatalf("unset model must omit --model, got %q", got)
	}

	// Set → `--model <alias>` appended.
	if got := build("", "", "", "opus", nil); !strings.Contains(got, "--model opus") {
		t.Fatalf("set model must append --model opus, got %q", got)
	}

	// The [1m] aliases contain sh glob characters; the command is run
	// via `sh -c`, so they must arrive quoted.
	got := build("", "", "", "sonnet[1m]", nil)
	if !strings.Contains(got, `--model 'sonnet[1m]'`) && !strings.Contains(got, `--model "sonnet[1m]"`) {
		t.Fatalf("[1m] model must be shell-quoted, got %q", got)
	}

	// Coexists with --resume, --effort and post-`--` passthrough args.
	got = build("", "conv-123", "max", "fable", []string{"--foo", "bar baz"})
	if !strings.Contains(got, "--resume conv-123") {
		t.Fatalf("expected --resume conv-123, got %q", got)
	}
	if !strings.Contains(got, "--effort max") {
		t.Fatalf("expected --effort max, got %q", got)
	}
	if !strings.Contains(got, "--model fable") {
		t.Fatalf("expected --model fable, got %q", got)
	}
}

// TestClaudeSpawner_Effort is the acceptance check for the regular
// `tclaude session new` surface: an unset effort must NOT add --effort to
// the claude invocation (claude keeps its own default), and an explicit
// level must append `--effort <level>` verbatim.
func TestClaudeSpawner_Effort(t *testing.T) {
	// Unset → no --effort anywhere in the command.
	if got := build("", "", "", "", nil); strings.Contains(got, "--effort") {
		t.Fatalf("unset effort must omit --effort, got %q", got)
	}

	// Set → `--effort <level>` appended.
	if got := build("", "", "high", "", nil); !strings.Contains(got, "--effort high") {
		t.Fatalf("set effort must append --effort high, got %q", got)
	}

	// Coexists with --resume and post-`--` passthrough args.
	got := build("", "conv-123", "max", "", []string{"--foo", "bar baz"})
	if !strings.Contains(got, "--resume conv-123") {
		t.Fatalf("expected --resume conv-123, got %q", got)
	}
	if !strings.Contains(got, "--effort max") {
		t.Fatalf("expected --effort max, got %q", got)
	}
}

// TestClaudeAsker_BuildAskArgv pins the exact `tclaude ask` argv shape —
// crucially the ORDER (JOH-253). The ask flow tests scan the slice by
// name (position-insensitive), so this is the guard that --effort / --model
// land BEFORE the `--` end-of-options marker and the prompt is always the
// trailing positional: a regression that emitted a flag after `--` would
// make claude swallow it as prompt text, and only an exact-slice check
// catches it. Interactive mode must omit both `-p` and `--`.
func TestClaudeAsker_BuildAskArgv(t *testing.T) {
	eq := func(name string, got, want []string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Fatalf("%s:\n got %q\nwant %q", name, got, want)
		}
	}

	// Fresh print turn: -p, the minted --session-id, --effort, --model,
	// then the `--` guard with the prompt last.
	eq("fresh print",
		claudeAsker{}.BuildAskArgv(AskSpec{
			Print: true, SessionID: "sid-1", Effort: "low", Model: "haiku", Prompt: "q?",
		}),
		[]string{"claude", "-p", "--session-id", "sid-1", "--effort", "low", "--model", "haiku", "--", "q?"})

	// Resume print turn with no model/effort: the flags are simply absent,
	// the prompt still behind `--`.
	eq("resume print, no model/effort",
		claudeAsker{}.BuildAskArgv(AskSpec{
			Print: true, ResumeID: "rid-1", Prompt: "follow up",
		}),
		[]string{"claude", "-p", "--resume", "rid-1", "--", "follow up"})

	// Interactive turn: NO -p, NO `--` (it would suppress claude's
	// submit-at-launch), flags still before the trailing prompt.
	eq("interactive",
		claudeAsker{}.BuildAskArgv(AskSpec{
			SessionID: "sid-2", Effort: "high", Model: "opus", Prompt: "pair on this",
		}),
		[]string{"claude", "--session-id", "sid-2", "--effort", "high", "--model", "opus", "pair on this"})

	// Streaming print turn: the stream-json trio lands right after -p (before
	// resume/session/effort/model), the prompt still behind the `--` guard.
	eq("streaming print",
		claudeAsker{}.BuildAskArgv(AskSpec{
			Print: true, Stream: true, ResumeID: "rid-2", Prompt: "go",
		}),
		[]string{"claude", "-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--resume", "rid-2", "--", "go"})

	// Stream is print-only: a Stream spec that isn't Print (can't happen via the
	// ask flow, but the builder must be safe) emits no capture-only flags.
	eq("stream ignored without print",
		claudeAsker{}.BuildAskArgv(AskSpec{
			Stream: true, SessionID: "sid-3", Prompt: "hi",
		}),
		[]string{"claude", "--session-id", "sid-3", "hi"})

	// Defensive cross-check of the ordering invariant the eq() above
	// already encodes: every flag index precedes the `--` marker.
	argv := claudeAsker{}.BuildAskArgv(AskSpec{
		Print: true, SessionID: "s", Effort: "max", Model: "sonnet", Prompt: "x",
	})
	dashAt := slices.Index(argv, "--")
	if dashAt < 0 {
		t.Fatal("print mode must emit a `--` guard")
	}
	for _, flag := range []string{"-p", "--session-id", "--effort", "--model"} {
		if i := slices.Index(argv, flag); i < 0 || i >= dashAt {
			t.Fatalf("flag %q must appear before the `--` guard (at %d), got index %d", flag, dashAt, i)
		}
	}
	if argv[len(argv)-1] != "x" {
		t.Fatalf("prompt must be the trailing positional, got %q", argv[len(argv)-1])
	}
}

// TestClaudeAsker_SupportsStream confirms claudeAsker is wired as a StreamAsker
// (so Harness.SupportsAskStream is true for the Claude descriptor).
func TestClaudeAsker_SupportsStream(t *testing.T) {
	if _, ok := any(claudeAsker{}).(StreamAsker); !ok {
		t.Fatal("claudeAsker must implement StreamAsker")
	}
	h, err := Resolve(DefaultName)
	if err != nil {
		t.Fatalf("resolve claude: %v", err)
	}
	if !h.SupportsAskStream() {
		t.Fatal("claude harness should report SupportsAskStream")
	}
}

// TestClaudeStreamFilter exercises the JSONL → clean-text filter: it forwards
// only text_delta chunks (concatenated, even when claude's stdout is split at
// arbitrary byte boundaries), ignores reasoning/system/snapshot noise, ends the
// line on Flush, and falls back to the result event when no deltas streamed
// (so an error or a delta-less turn is never silent).
func TestClaudeStreamFilter(t *testing.T) {
	run := func(chunks ...string) string {
		var out strings.Builder
		w := claudeAsker{}.StreamFilter(&out)
		for _, c := range chunks {
			if _, err := io.WriteString(w, c); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
		if fl, ok := w.(AskStreamFlusher); ok {
			if err := fl.Flush(); err != nil {
				t.Fatalf("flush: %v", err)
			}
		}
		return out.String()
	}

	textDelta := func(s string) string {
		b, _ := json.Marshal(s)
		return `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":` + string(b) + `}}}` + "\n"
	}

	t.Run("concatenates text deltas, drops noise, ends with a newline", func(t *testing.T) {
		stream := `{"type":"system","subtype":"init"}` + "\n" +
			`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"reasoning"}}}` + "\n" +
			textDelta("Hello, ") +
			textDelta("world") +
			`{"type":"assistant","message":{"content":[{"type":"text","text":"Hello, world"}]}}` + "\n" +
			`{"type":"result","subtype":"success","is_error":false,"result":"Hello, world"}` + "\n"
		got := run(stream)
		if got != "Hello, world\n" {
			t.Fatalf("got %q, want %q", got, "Hello, world\n")
		}
		if strings.Contains(got, "reasoning") {
			t.Fatal("thinking text must not be forwarded")
		}
	})

	t.Run("reassembles deltas across split writes", func(t *testing.T) {
		full := textDelta("abc") + textDelta("def")
		// Split the byte stream at an awkward mid-line boundary.
		cut := len(full) / 3
		got := run(full[:cut], full[cut:])
		if got != "abcdef\n" {
			t.Fatalf("got %q, want %q", got, "abcdef\n")
		}
	})

	t.Run("falls back to result text when no deltas streamed", func(t *testing.T) {
		// e.g. a failed turn: claude emits the error message only in the result
		// event. The filter must surface it rather than print nothing.
		stream := `{"type":"system","subtype":"init"}` + "\n" +
			`{"type":"result","subtype":"error","is_error":true,"result":"Error: not logged in"}` + "\n"
		got := run(stream)
		if got != "Error: not logged in\n" {
			t.Fatalf("got %q, want %q", got, "Error: not logged in\n")
		}
	})

	t.Run("does not double-print when both deltas and result are present", func(t *testing.T) {
		stream := textDelta("answer") +
			`{"type":"result","subtype":"success","is_error":false,"result":"answer"}` + "\n"
		got := run(stream)
		if got != "answer\n" {
			t.Fatalf("got %q, want %q (result must not re-print the streamed text)", got, "answer\n")
		}
	})

	t.Run("ignores malformed lines", func(t *testing.T) {
		stream := "not json at all\n" + textDelta("ok") + "{bad\n"
		got := run(stream)
		if got != "ok\n" {
			t.Fatalf("got %q, want %q", got, "ok\n")
		}
	})

	t.Run("empty stream yields empty output", func(t *testing.T) {
		if got := run(""); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("final line without a trailing newline is still consumed", func(t *testing.T) {
		// Drop the terminating newline from the (delta-less) result line; Flush
		// must still surface it rather than leave it buffered and lost.
		stream := `{"type":"result","subtype":"error","is_error":true,"result":"boom"}`
		got := run(stream)
		if got != "boom\n" {
			t.Fatalf("got %q, want %q", got, "boom\n")
		}
	})
}

// TestClaudeConvStore_Exists covers the ask self-heal probe (JOH-252): a
// present per-cwd `.jsonl` is true, an absent one false, an empty id false.
func TestClaudeConvStore_Exists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	cwd := "/home/u/proj"
	dir := convops.GetClaudeProjectPath(cwd)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const id = "11111111-1111-1111-1111-111111111111"
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := claudeConvStore{}
	if ok, err := store.Exists(id, cwd); err != nil || !ok {
		t.Fatalf("present conv: ok=%v err=%v, want true,nil", ok, err)
	}
	if ok, err := store.Exists("22222222-2222-2222-2222-222222222222", cwd); err != nil || ok {
		t.Fatalf("absent conv: ok=%v err=%v, want false,nil", ok, err)
	}
	if ok, err := store.Exists("", cwd); err != nil || ok {
		t.Fatalf("empty id: ok=%v err=%v, want false,nil", ok, err)
	}
}

// TestClaudeSpawner_EnvAndBinary covers the env-export prefix and the
// pass-through binary name — the two remaining moving parts of the spawn
// command.
func TestClaudeSpawner_EnvAndBinary(t *testing.T) {
	got := build("export TCLAUDE_SESSION_ID=abc; ", "", "", "", nil)
	if !strings.HasPrefix(got, "export TCLAUDE_SESSION_ID=abc; claude") {
		t.Fatalf("env exports must precede the claude binary, got %q", got)
	}
	if bin := (claudeSpawner{}).Binary(); bin != "claude" {
		t.Fatalf("claude binary = %q, want claude", bin)
	}
}

// TestClaudeModels_Delegation checks the catalog forwards to the curated
// clcommon validators and that the list getters return non-empty copies.
func TestClaudeModels_Delegation(t *testing.T) {
	c := claudeModels{}

	if _, err := c.ValidateModel("opus"); err != nil {
		t.Fatalf("ValidateModel(opus) unexpected error: %v", err)
	}
	if _, err := c.ValidateModel("definitely-not-a-model"); err == nil {
		t.Fatalf("ValidateModel(bogus) should error")
	}
	if norm, _ := c.ValidateEffort("  HIGH "); norm != "high" {
		t.Fatalf("ValidateEffort normalisation = %q, want high", norm)
	}
	if got, _ := c.ValidateModel(""); got != "" {
		t.Fatalf("empty model must stay empty, got %q", got)
	}

	if len(c.Models()) == 0 {
		t.Fatalf("Models() returned empty list")
	}
	if len(c.EffortLevels()) == 0 {
		t.Fatalf("EffortLevels() returned empty list")
	}
	// The getter must hand back a copy — mutating it must not corrupt the
	// shared source list.
	models := c.Models()
	models[0] = "MUTATED"
	if c.Models()[0] == "MUTATED" {
		t.Fatalf("Models() leaked the shared backing slice")
	}
}

// TestClaudeSpawner_LaunchEnrollment covers the launch-enrollment flags the
// daemon's efficient spawn path relies on: a preset conv-id (--session-id), a
// launch display name (--name), and a positional first-turn prompt. Each is a
// fresh-launch-only flag, and each is shell-quoted because it reaches `sh -c`.
func TestClaudeSpawner_LaunchEnrollment(t *testing.T) {
	spec := SpawnSpec{
		SessionID:     "2567b392-357b-4d6c-9a59-74fd23424cda",
		Name:          "worker bee",
		InitialPrompt: "[system: spawned by the human; read inbox #7]",
	}
	got := claudeSpawner{}.BuildCommand(spec)

	if !strings.Contains(got, "--session-id 2567b392-357b-4d6c-9a59-74fd23424cda") {
		t.Fatalf("expected --session-id, got %q", got)
	}
	// The name has a space, so it must be quoted.
	if !strings.Contains(got, `--name 'worker bee'`) {
		t.Fatalf("expected quoted --name, got %q", got)
	}
	// The welcome carries shell metacharacters ([], #, ;), so the whole
	// positional prompt must arrive as one quoted arg at the end.
	if !strings.Contains(got, `'[system: spawned by the human; read inbox #7]'`) {
		t.Fatalf("expected quoted positional prompt, got %q", got)
	}

	// On a --resume the preset id + positional prompt are omitted (the
	// conversation already has an id and history); --name still applies.
	r := claudeSpawner{}.BuildCommand(SpawnSpec{
		ResumeID:      "conv-9",
		SessionID:     "2567b392-357b-4d6c-9a59-74fd23424cda",
		Name:          "worker",
		InitialPrompt: "hello",
	})
	if strings.Contains(r, "--session-id") {
		t.Fatalf("a resume must not emit --session-id, got %q", r)
	}
	if strings.Contains(r, "hello") {
		t.Fatalf("a resume must not emit a positional prompt, got %q", r)
	}
	if !strings.Contains(r, "--resume conv-9") || !strings.Contains(r, "--name worker") {
		t.Fatalf("resume must keep --resume and --name, got %q", r)
	}

	// The default (claude) harness advertises the capability; an unset spec
	// emits none of the flags.
	if !Default().SupportsLaunchEnrollment() {
		t.Fatalf("claude must support launch enrollment")
	}
	bare := claudeSpawner{}.BuildCommand(SpawnSpec{})
	if strings.Contains(bare, "--session-id") || strings.Contains(bare, "--name") {
		t.Fatalf("an empty spec must omit launch-enrollment flags, got %q", bare)
	}
}

// TestClaudeLifecycle_Tokens pins the CC slash-command tokens so the
// capability flags report supported and the injection call sites keep
// typing the exact commands CC understands.
func TestClaudeLifecycle_Tokens(t *testing.T) {
	h := Default()
	if h.Life.RenameCommand() != "/rename" {
		t.Fatalf("rename token = %q, want /rename", h.Life.RenameCommand())
	}
	if h.Life.CompactCommand() != "/compact" {
		t.Fatalf("compact token = %q, want /compact", h.Life.CompactCommand())
	}
	if h.Life.SoftExitCommand() != "/exit" {
		t.Fatalf("soft-exit token = %q, want /exit", h.Life.SoftExitCommand())
	}
	if !h.SupportsRename() || !h.SupportsCompact() || !h.SupportsSoftExit() {
		t.Fatalf("claude must support rename/compact/soft-exit")
	}
}
