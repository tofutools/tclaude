package ask

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// forceConvExists overrides the on-disk conversation existence check (the flow
// tests use a fake runner that writes no real .jsonl), restoring it on cleanup.
func forceConvExists(t *testing.T, exists bool) {
	t.Helper()
	prev := convExists
	convExists = func(*harness.Harness, string, string) bool { return exists }
	t.Cleanup(func() { convExists = prev })
}

// setupAskTestDB points db.Open() at a throwaway SQLite under a temp HOME and
// resets the singleton so each test gets a fresh, migrated database (the same
// recipe db's own setupTestDB uses, exported here via db.ResetForTest).
func setupAskTestDB(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()
}

// fakeRun is a stand-in for the harness subprocess: it records every runPlan
// it is handed and writes a canned answer to the plan's stdout, so a flow test
// can assert on the argv/cwd/streams without launching a real `claude`.
type fakeRun struct {
	plans   []runPlan
	answer  string
	started bool
	err     error
}

func (f *fakeRun) install(t *testing.T) {
	t.Helper()
	prev := runner
	runner = func(p runPlan) (bool, error) {
		f.plans = append(f.plans, p)
		if f.answer != "" && p.Stdout != nil {
			_, _ = io.WriteString(p.Stdout, f.answer)
		}
		return f.started, f.err
	}
	t.Cleanup(func() { runner = prev })
}

func (f *fakeRun) last() runPlan {
	return f.plans[len(f.plans)-1]
}

func argvValue(argv []string, flag string) (string, bool) {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1], true
		}
	}
	return "", false
}

func argvHas(argv []string, tok string) bool {
	return slices.Contains(argv, tok)
}

func ttyInput(termKey, cwd, q string) askInput {
	return askInput{
		TermKey:          termKey,
		Cwd:              cwd,
		Question:         q,
		StdinIsTerminal:  true,
		StdoutIsTerminal: true,
	}
}

func io2buf() (askIO, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return askIO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb}, &out, &errb
}

// TestAsk_FreshThenResumeContinuity is the headline flow: a first question in
// a terminal+dir mints a fresh conversation (--session-id), the answer is
// printed, and the mapping is persisted; a second question from the same
// terminal+dir RESUMES that exact conversation (--resume <same id>, no new
// --session-id); a question from a different cwd starts its own thread.
func TestAsk_FreshThenResumeContinuity(t *testing.T) {
	setupAskTestDB(t)
	forceConvExists(t, true) // pretend the resumed conv is on disk
	f := &fakeRun{answer: "the answer\n", started: true}
	f.install(t)

	// 1) fresh
	aio, out, _ := io2buf()
	require.NoError(t, runAsk(ttyInput("term-A", "/repo/x", "what is up?"), aio))

	first := f.last()
	assert.Equal(t, "/repo/x", first.Cwd, "runs in the caller's cwd")
	assert.Equal(t, "claude", first.Argv[0], "execs the claude binary")
	assert.True(t, argvHas(first.Argv, "-p"), "default is print mode")
	conv, ok := argvValue(first.Argv, "--session-id")
	require.True(t, ok, "fresh turn pins a conv-id with --session-id")
	assert.NotEmpty(t, conv)
	assert.Equal(t, "what is up?", first.Argv[len(first.Argv)-1], "question is the trailing positional")
	assert.Equal(t, "--", first.Argv[len(first.Argv)-2], "print-mode prompt is guarded by an end-of-options --")
	assert.Nil(t, first.Stdin, "print mode wires no interactive stdin")
	assert.Contains(t, out.String(), "the answer", "answer is printed to stdout")

	thread, err := db.GetAskThread("term-A", "/repo/x")
	require.NoError(t, err)
	require.NotNil(t, thread, "mapping persisted")
	assert.Equal(t, conv, thread.ConvID, "mapping points at the minted conv-id")

	// 2) resume — same terminal + cwd continues the same conversation
	aio2, _, _ := io2buf()
	require.NoError(t, runAsk(ttyInput("term-A", "/repo/x", "follow up?"), aio2))

	second := f.last()
	assert.False(t, argvHas(second.Argv, "--session-id"), "resume does not re-pin a fresh id")
	resumeID, ok := argvValue(second.Argv, "--resume")
	require.True(t, ok, "second turn resumes")
	assert.Equal(t, conv, resumeID, "resumes the SAME conversation as turn 1")

	// 3) different cwd in the same terminal → its own thread
	aio3, _, _ := io2buf()
	require.NoError(t, runAsk(ttyInput("term-A", "/repo/y", "in another dir"), aio3))
	third := f.last()
	otherConv, ok := argvValue(third.Argv, "--session-id")
	require.True(t, ok, "a new cwd starts fresh")
	assert.NotEqual(t, conv, otherConv, "different cwd → different conversation")
}

// TestAsk_InteractiveMode covers the -i opt-in: the full TUI, attached to the
// caller's terminal. No -p, no `--` (which would suppress claude's submit-at-
// launch), and the caller's real stdin is wired so the agent can prompt back.
func TestAsk_InteractiveMode(t *testing.T) {
	setupAskTestDB(t)
	f := &fakeRun{answer: "", started: true}
	f.install(t)

	in := ttyInput("term-I", "/repo/x", "let's pair on this")
	in.ForceInteractive = true
	aio, _, _ := io2buf()
	require.NoError(t, runAsk(in, aio))

	p := f.last()
	assert.False(t, argvHas(p.Argv, "-p"), "interactive is not print mode")
	assert.False(t, argvHas(p.Argv, "--"), "interactive omits -- so the prompt submits at launch")
	assert.Equal(t, "let's pair on this", p.Argv[len(p.Argv)-1], "question is the trailing positional")
	assert.NotNil(t, p.Stdin, "interactive wires the real terminal stdin")
}

// TestAsk_SelfHealsGhostConversation covers the robustness fix: if a recorded
// thread points at a conversation that no longer exists on disk (a fresh turn
// that died before it was written, or a conv the user deleted), the next
// question starts fresh instead of trying to --resume the ghost forever.
func TestAsk_SelfHealsGhostConversation(t *testing.T) {
	setupAskTestDB(t)
	f := &fakeRun{answer: "ok\n", started: true}
	f.install(t)

	// seed a thread (fresh turn records a conv-id)
	aio, _, _ := io2buf()
	require.NoError(t, runAsk(ttyInput("term-G", "/repo/x", "first"), aio))
	seeded, ok := argvValue(f.last().Argv, "--session-id")
	require.True(t, ok)

	// the recorded conversation is now gone on disk
	forceConvExists(t, false)
	aio2, _, _ := io2buf()
	require.NoError(t, runAsk(ttyInput("term-G", "/repo/x", "second"), aio2))

	healed, ok := argvValue(f.last().Argv, "--session-id")
	require.True(t, ok, "a ghost conversation self-heals to a fresh --session-id")
	assert.NotEqual(t, seeded, healed, "minted a new conversation, not the ghost")
	assert.False(t, argvHas(f.last().Argv, "--resume"), "does not resume the ghost")
}

// TestAsk_CapturedModeFoldsStdin covers the `git diff | ai "safe?"` shape:
// piped stdin forces -p capture mode, the payload is folded into the prompt
// under a labelled fence, and no interactive stdin is wired to the child.
func TestAsk_CapturedModeFoldsStdin(t *testing.T) {
	setupAskTestDB(t)
	f := &fakeRun{answer: "looks fine\n", started: true}
	f.install(t)

	in := askInput{
		TermKey:          "term-pipe",
		Cwd:              "/repo/x",
		Question:         "is this safe to push?",
		StdinPayload:     "diff --git a/x b/x\n+danger()\n",
		StdinIsTerminal:  false, // piped
		StdoutIsTerminal: true,
	}
	var out bytes.Buffer
	require.NoError(t, runAsk(in, askIO{Stdout: &out, Stderr: io.Discard}))

	p := f.last()
	assert.True(t, argvHas(p.Argv, "-p"), "piped stdin forces capture mode")
	assert.Nil(t, p.Stdin, "capture mode wires no interactive stdin")
	prompt := p.Argv[len(p.Argv)-1]
	assert.Contains(t, prompt, "is this safe to push?", "question is included")
	assert.Contains(t, prompt, "piped input (stdin)", "payload is fenced under a label")
	assert.Contains(t, prompt, "+danger()", "payload content is included")
	assert.Contains(t, out.String(), "looks fine")
}

// TestAsk_NewResets covers --new: with a recorded thread, --new + a question
// starts a brand-new conversation; --new with no question just resets and
// reports, running nothing.
func TestAsk_NewResets(t *testing.T) {
	setupAskTestDB(t)
	f := &fakeRun{answer: "ok\n", started: true}
	f.install(t)

	// seed a thread
	aio, _, _ := io2buf()
	require.NoError(t, runAsk(ttyInput("term-N", "/repo/x", "first"), aio))
	orig, _ := argvValue(f.last().Argv, "--session-id")

	// --new + question → fresh conversation
	in := ttyInput("term-N", "/repo/x", "start over")
	in.New = true
	aio2, _, _ := io2buf()
	require.NoError(t, runAsk(in, aio2))
	fresh, ok := argvValue(f.last().Argv, "--session-id")
	require.True(t, ok, "--new starts fresh, not resume")
	assert.NotEqual(t, orig, fresh, "--new mints a different conversation")

	// --new alone → reset only, no run
	before := len(f.plans)
	reset := askInput{TermKey: "term-N", Cwd: "/repo/x", New: true, StdinIsTerminal: true, StdoutIsTerminal: true}
	aio3, _, errb := io2buf()
	require.NoError(t, runAsk(reset, aio3))
	assert.Equal(t, before, len(f.plans), "--new with no question runs nothing")
	assert.Contains(t, errb.String(), "fresh conversation")
	got, err := db.GetAskThread("term-N", "/repo/x")
	require.NoError(t, err)
	assert.Nil(t, got, "thread was reset")
}

func TestAsk_Validation(t *testing.T) {
	setupAskTestDB(t)
	(&fakeRun{started: true}).install(t)

	// no question, no payload
	aio, _, _ := io2buf()
	err := runAsk(ttyInput("term-V", "/repo/x", ""), aio)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no question")

	// --interactive with piped stdin is contradictory (no terminal for the TUI)
	in := askInput{
		TermKey: "term-V", Cwd: "/repo/x", Question: "hi",
		ForceInteractive: true, StdinIsTerminal: false, StdoutIsTerminal: true,
	}
	aio2, _, _ := io2buf()
	err = runAsk(in, aio2)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "real terminal")
}

// TestAsk_ModelValidatedAndPassed checks a valid --model reaches the argv.
func TestAsk_ModelValidatedAndPassed(t *testing.T) {
	setupAskTestDB(t)
	f := &fakeRun{answer: "ok\n", started: true}
	f.install(t)

	in := ttyInput("term-M", "/repo/x", "quick one")
	in.Model = "haiku"
	aio, _, _ := io2buf()
	require.NoError(t, runAsk(in, aio))

	m, ok := argvValue(f.last().Argv, "--model")
	require.True(t, ok, "--model is forwarded")
	assert.NotEmpty(t, m)

	// an invalid model is rejected before running anything
	bad := ttyInput("term-M", "/repo/x", "quick one")
	bad.Model = "definitely-not-a-real-model-xyz"
	aio2, _, _ := io2buf()
	err := runAsk(bad, aio2)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--model")
}

// TestAsk_NotStarted_NoMapping: if the harness never started (e.g. binary
// missing), no dangling conversation mapping is recorded, and the start error
// propagates.
func TestAsk_NotStarted_NoMapping(t *testing.T) {
	setupAskTestDB(t)
	f := &fakeRun{started: false, err: errors.New("claude not found on PATH")}
	f.install(t)

	aio, _, _ := io2buf()
	err := runAsk(ttyInput("term-Z", "/repo/x", "hello"), aio)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")

	got, err := db.GetAskThread("term-Z", "/repo/x")
	require.NoError(t, err)
	assert.Nil(t, got, "no mapping recorded when the harness never started")
}

// TestAssemblePrompt covers the three prompt shapes directly.
func TestAssemblePrompt(t *testing.T) {
	assert.Equal(t, "q", assemblePrompt("q", ""))
	assert.Equal(t, "payload", assemblePrompt("", "payload\n\n"))
	got := assemblePrompt("q", "data\n")
	assert.Contains(t, got, "q\n\n")
	assert.Contains(t, got, "piped input (stdin)")
	assert.Contains(t, got, "data")
	assert.Equal(t, "", assemblePrompt("", ""))
}
