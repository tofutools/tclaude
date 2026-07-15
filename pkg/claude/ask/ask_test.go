package ask

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// scriptedConvStore is a harness.ConvStore whose ListConvs returns a scripted
// sequence of results (one per call) so liveFreshConvResolver's before/after
// diff can be unit-tested without real harness storage.
type scriptedConvStore struct {
	calls    int
	lists    [][]convops.SessionEntry
	errOnGet int // 1-indexed call number that returns an error (0 = never)
}

func (s *scriptedConvStore) ListConvs(string) ([]convops.SessionEntry, error) {
	i := s.calls
	s.calls++
	if s.errOnGet == i+1 {
		return nil, errors.New("scan failed")
	}
	if i < len(s.lists) {
		return s.lists[i], nil
	}
	if len(s.lists) > 0 {
		return s.lists[len(s.lists)-1], nil
	}
	return nil, nil
}
func (*scriptedConvStore) Resolve(string, string, bool) (*harness.ConvRef, error) { return nil, nil }
func (*scriptedConvStore) Title(string) (string, error)                           { return "", nil }
func (*scriptedConvStore) SetTitle(string, string) error                          { return nil }
func (*scriptedConvStore) Exists(string, string) (bool, error)                    { return true, nil }

// TestLiveFreshConvResolver covers the post-run conv-id discovery: the id that
// appears in the "after" listing but not the "before" snapshot is returned;
// among several new ones the newest by mtime wins; nothing new yields "".
func TestLiveFreshConvResolver(t *testing.T) {
	t.Run("picks the newly appeared id", func(t *testing.T) {
		store := &scriptedConvStore{lists: [][]convops.SessionEntry{
			{{SessionID: "a", FileMtime: 1}},
			{{SessionID: "a", FileMtime: 1}, {SessionID: "b", FileMtime: 2}},
		}}
		h := &harness.Harness{Name: "codex", Convs: store}
		assert.Equal(t, "b", liveFreshConvResolver(h, "/repo/x")())
	})
	t.Run("newest of several new ids wins", func(t *testing.T) {
		store := &scriptedConvStore{lists: [][]convops.SessionEntry{
			{{SessionID: "a", FileMtime: 1}},
			{{SessionID: "a", FileMtime: 1}, {SessionID: "b", FileMtime: 5}, {SessionID: "c", FileMtime: 9}},
		}}
		h := &harness.Harness{Name: "codex", Convs: store}
		assert.Equal(t, "c", liveFreshConvResolver(h, "/repo/x")())
	})
	t.Run("nothing new yields empty", func(t *testing.T) {
		store := &scriptedConvStore{lists: [][]convops.SessionEntry{
			{{SessionID: "a", FileMtime: 1}},
			{{SessionID: "a", FileMtime: 1}},
		}}
		h := &harness.Harness{Name: "codex", Convs: store}
		assert.Empty(t, liveFreshConvResolver(h, "/repo/x")())
	})
	t.Run("nil convstore yields empty", func(t *testing.T) {
		h := &harness.Harness{Name: "x"}
		assert.Empty(t, liveFreshConvResolver(h, "/repo/x")())
	})
	t.Run("failed before-snapshot yields empty (never mis-maps a pre-existing conv)", func(t *testing.T) {
		// The first ListConvs (the before snapshot) errors; the after listing
		// succeeds with a conv that existed all along. Without the guard that
		// conv would look "new" and be wrongly recorded — the resolver must
		// instead skip the mapping.
		store := &scriptedConvStore{
			errOnGet: 1,
			lists:    [][]convops.SessionEntry{nil, {{SessionID: "preexisting", FileMtime: 7}}},
		}
		h := &harness.Harness{Name: "codex", Convs: store}
		assert.Empty(t, liveFreshConvResolver(h, "/repo/x")())
	})
	t.Run("failed after-listing yields empty", func(t *testing.T) {
		store := &scriptedConvStore{
			errOnGet: 2,
			lists:    [][]convops.SessionEntry{{{SessionID: "a", FileMtime: 1}}, nil},
		}
		h := &harness.Harness{Name: "codex", Convs: store}
		assert.Empty(t, liveFreshConvResolver(h, "/repo/x")())
	})
}

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
	plans     []runPlan
	answer    string
	errOutput string // written to the plan's Stderr (models a noisy transcript)
	started   bool
	err       error
}

func (f *fakeRun) install(t *testing.T) {
	t.Helper()
	prev := runner
	runner = func(p runPlan) (bool, error) {
		f.plans = append(f.plans, p)
		if f.answer != "" && p.Stdout != nil {
			// When the ask flow asked for the streaming output format, the real
			// claude writes JSONL events, not the bare answer — and p.Stdout is
			// the harness's StreamFilter, which only extracts text from those
			// events. Emit a realistic event stream so the filter is exercised
			// end to end and the assertions still see the answer text.
			if argvHas(p.Argv, "stream-json") {
				_, _ = io.WriteString(p.Stdout, fakeClaudeStreamJSON(f.answer))
			} else {
				_, _ = io.WriteString(p.Stdout, f.answer)
			}
		}
		if f.errOutput != "" && p.Stderr != nil {
			_, _ = io.WriteString(p.Stderr, f.errOutput)
		}
		return f.started, f.err
	}
	t.Cleanup(func() { runner = prev })
}

// fakeClaudeStreamJSON renders answer as the JSONL event stream Claude Code's
// `--output-format stream-json --include-partial-messages` emits: an init
// event, the answer split across two text_delta chunks (so the test proves the
// filter concatenates deltas, not just forwards one), some ignored noise
// (thinking + a per-message assistant snapshot, which must NOT be echoed), then
// the terminal result event. The trailing answer newline is dropped from the
// deltas — real text deltas carry none; the filter re-adds one on Flush.
func fakeClaudeStreamJSON(answer string) string {
	text := strings.TrimRight(answer, "\n")
	mid := len(text) / 2
	delta := func(s string) string {
		b, _ := json.Marshal(s)
		return `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":` + string(b) + `}}}`
	}
	resB, _ := json.Marshal(text)
	lines := []string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"hmm"}}}`,
		delta(text[:mid]),
		delta(text[mid:]),
		`{"type":"assistant","message":{"content":[{"type":"text","text":` + string(resB) + `}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":` + string(resB) + `}`,
	}
	return strings.Join(lines, "\n") + "\n"
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

// TestResolveAskTarget covers the no-profile model/effort precedence: a
// per-call flag wins, else the config.ask block, else the built-in default
// constants — resolved independently per field (JOH-253). With no ask
// profile selected the harness is always the default (Claude).
func TestResolveAskTarget(t *testing.T) {
	pinned := &config.Config{Ask: &config.AskConfig{Model: "opus", Effort: "high"}}
	onlyModel := &config.Config{Ask: &config.AskConfig{Model: "haiku"}}

	cases := []struct {
		name                  string
		flagModel, flagEffort string
		cfg                   *config.Config
		wantModel, wantEffort string
	}{
		{"empty config → built-in defaults", "", "", &config.Config{},
			config.DefaultAskModel, config.DefaultAskEffort},
		{"nil config → built-in defaults", "", "", nil,
			config.DefaultAskModel, config.DefaultAskEffort},
		{"config block used when no flag", "", "", pinned, "opus", "high"},
		{"flag overrides config", "fable", "low", pinned, "fable", "low"},
		{"flag overrides built-in default", "fable", "max", &config.Config{}, "fable", "max"},
		{"partial config: model only → default effort", "", "", onlyModel,
			"haiku", config.DefaultAskEffort},
		{"flag model only, effort falls through config", "fable", "", pinned,
			"fable", "high"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, m, e, err := resolveAskTarget(tc.flagModel, tc.flagEffort, tc.cfg)
			require.NoError(t, err)
			assert.Equal(t, harness.DefaultName, h, "harness (no profile → default)")
			assert.Equal(t, tc.wantModel, m, "model")
			assert.Equal(t, tc.wantEffort, e, "effort")
		})
	}
}

// TestResolveAskTarget_Profile covers the JOH-252 fold-in: a selected ask
// profile supplies the harness (+ model/effort), a per-call flag still wins,
// a non-Claude harness leaves a blank model/effort blank (no Claude fast
// default leaks into the Codex catalog), and a missing profile self-heals to
// the no-profile path.
func TestResolveAskTarget_Profile(t *testing.T) {
	setupAskTestDB(t) // resolveAskTarget reads db.GetSpawnProfile

	_, err := db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "codex-fast", Harness: "codex", Model: "gpt-5", Effort: "low",
	})
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "codex-bare", Harness: "codex", // no model/effort
	})
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "claude-blank", Harness: "claude", // no model/effort → Claude fast defaults
	})
	require.NoError(t, err)

	cfg := func(profile string) *config.Config {
		return &config.Config{Ask: &config.AskConfig{Profile: profile}}
	}

	t.Run("codex profile supplies harness+model+effort", func(t *testing.T) {
		h, m, e, err := resolveAskTarget("", "", cfg("codex-fast"))
		require.NoError(t, err)
		assert.Equal(t, "codex", h)
		assert.Equal(t, "gpt-5", m)
		assert.Equal(t, "low", e)
	})
	t.Run("flag overrides the codex profile model", func(t *testing.T) {
		h, m, e, err := resolveAskTarget("gpt-5-codex", "high", cfg("codex-fast"))
		require.NoError(t, err)
		assert.Equal(t, "codex", h)
		assert.Equal(t, "gpt-5-codex", m)
		assert.Equal(t, "high", e)
	})
	t.Run("blank codex profile leaves model/effort empty (no Claude default)", func(t *testing.T) {
		h, m, e, err := resolveAskTarget("", "", cfg("codex-bare"))
		require.NoError(t, err)
		assert.Equal(t, "codex", h)
		assert.Empty(t, m, "no haiku leaks into the Codex catalog")
		assert.Empty(t, e)
	})
	t.Run("blank claude profile still gets the Claude fast defaults", func(t *testing.T) {
		h, m, e, err := resolveAskTarget("", "", cfg("claude-blank"))
		require.NoError(t, err)
		assert.Equal(t, harness.DefaultName, h)
		assert.Equal(t, config.DefaultAskModel, m)
		assert.Equal(t, config.DefaultAskEffort, e)
	})
	t.Run("missing profile self-heals to no-profile defaults", func(t *testing.T) {
		h, m, e, err := resolveAskTarget("", "", cfg("does-not-exist"))
		require.NoError(t, err)
		assert.Equal(t, harness.DefaultName, h)
		assert.Equal(t, config.DefaultAskModel, m)
		assert.Equal(t, config.DefaultAskEffort, e)
	})
	t.Run("disabled profile fails with its stored reason", func(t *testing.T) {
		_, err := db.CreateSpawnProfile(&db.SpawnProfile{
			Name: "paused", Disabled: true, DisabledReason: "provider outage",
		})
		require.NoError(t, err)
		_, _, _, err = resolveAskTarget("", "", cfg("paused"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `spawn profile "paused" is disabled: provider outage`)
	})
}

// TestAsk_EffortValidatedAndPassed checks a valid effort reaches the argv
// as --effort, and an invalid one is rejected before running anything —
// the effort twin of TestAsk_ModelValidatedAndPassed.
func TestAsk_EffortValidatedAndPassed(t *testing.T) {
	setupAskTestDB(t)
	f := &fakeRun{answer: "ok\n", started: true}
	f.install(t)

	in := ttyInput("term-E", "/repo/x", "quick one")
	in.Effort = "low"
	aio, _, _ := io2buf()
	require.NoError(t, runAsk(in, aio))

	e, ok := argvValue(f.last().Argv, "--effort")
	require.True(t, ok, "--effort is forwarded")
	assert.Equal(t, "low", e)

	// an invalid effort is rejected before running anything
	bad := ttyInput("term-E", "/repo/x", "quick one")
	bad.Effort = "ludicrous"
	aio2, _, _ := io2buf()
	err := runAsk(bad, aio2)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--effort")
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

// stubFreshConvResolver replaces the post-run conv-id discovery (the flow
// tests have no real ~/.codex storage) so a fresh non-pre-minting ask resolves
// to a canned id, restoring it on cleanup. An empty id models "codex created
// no conv".
func stubFreshConvResolver(t *testing.T, id string) {
	t.Helper()
	prev := newFreshConvResolver
	newFreshConvResolver = func(*harness.Harness, string) func() string {
		return func() string { return id }
	}
	t.Cleanup(func() { newFreshConvResolver = prev })
}

// TestAsk_CodexFreshDiscoversConvID is the Codex headline (JOH-252): a fresh
// ask on a Codex thread routes to `codex exec` (NOT pre-minting a
// --session-id), runs read-only, and the conv-id Codex created is discovered
// post-run and recorded as a codex-harness mapping.
func TestAsk_CodexFreshDiscoversConvID(t *testing.T) {
	setupAskTestDB(t)
	f := &fakeRun{answer: "ok\n", started: true}
	f.install(t)
	stubFreshConvResolver(t, "codex-conv-1")

	in := ttyInput("term-CX", "/repo/x", "what is up?")
	in.Harness = "codex"
	aio, _, _ := io2buf()
	require.NoError(t, runAsk(in, aio))

	p := f.last()
	assert.Equal(t, "codex", p.Argv[0], "execs the codex binary")
	assert.Equal(t, "exec", p.Argv[1], "fresh capture uses `codex exec`")
	assert.False(t, argvHas(p.Argv, "--session-id"), "codex does not pre-mint a conv-id")
	assert.False(t, argvHas(p.Argv, "resume"), "a fresh ask does not resume")
	sb, ok := argvValue(p.Argv, "--sandbox")
	require.True(t, ok, "capture pins a sandbox")
	assert.Equal(t, "read-only", sb, "captured codex ask is read-only")
	assert.True(t, argvHas(p.Argv, "--skip-git-repo-check"), "ask works from any directory")

	thread, err := db.GetAskThread("term-CX", "/repo/x")
	require.NoError(t, err)
	require.NotNil(t, thread, "mapping persisted from the discovered id")
	assert.Equal(t, "codex-conv-1", thread.ConvID, "records the discovered conv-id")
	assert.Equal(t, "codex", thread.Harness, "mapping is tagged codex")
}

// TestAsk_CodexResumeRouting: a second question on a Codex-recorded thread
// resumes via `codex exec resume <id>` (the subcommand form), keeps the conv's
// recorded harness, and never pre-mints.
func TestAsk_CodexResumeRouting(t *testing.T) {
	setupAskTestDB(t)
	forceConvExists(t, true)
	f := &fakeRun{answer: "ok\n", started: true}
	f.install(t)
	require.NoError(t, db.SetAskThread("term-CR", "/repo/x", "codex-conv-9", "codex"))

	aio, _, _ := io2buf()
	require.NoError(t, runAsk(ttyInput("term-CR", "/repo/x", "follow up"), aio))

	p := f.last()
	require.GreaterOrEqual(t, len(p.Argv), 4)
	assert.Equal(t, []string{"codex", "exec", "resume", "codex-conv-9"}, p.Argv[:4],
		"resume uses the `codex exec resume <id>` subcommand")
	assert.False(t, argvHas(p.Argv, "--session-id"))
	assert.Equal(t, "follow up", p.Argv[len(p.Argv)-1], "question is the trailing positional")
}

// TestAsk_CodexFreshNoConv_NoMapping: if a fresh Codex ask creates no
// conversation (e.g. the run errored before writing one), discovery returns
// "" and no dangling mapping is recorded — the next ask starts fresh.
func TestAsk_CodexFreshNoConv_NoMapping(t *testing.T) {
	setupAskTestDB(t)
	f := &fakeRun{answer: "", started: true}
	f.install(t)
	stubFreshConvResolver(t, "") // codex wrote no rollout

	in := ttyInput("term-CN", "/repo/x", "q")
	in.Harness = "codex"
	aio, _, _ := io2buf()
	require.NoError(t, runAsk(in, aio))

	got, err := db.GetAskThread("term-CN", "/repo/x")
	require.NoError(t, err)
	assert.Nil(t, got, "no mapping recorded when codex created no conv")
}

// TestAsk_CodexCaptureHidesStderrOnSuccess: a successful captured Codex ask
// prints only the clean stdout answer; its noisy stderr transcript (banner,
// hook lines, token count) is suppressed.
func TestAsk_CodexCaptureHidesStderrOnSuccess(t *testing.T) {
	setupAskTestDB(t)
	forceConvExists(t, true)
	require.NoError(t, db.SetAskThread("term-CS", "/repo/x", "codex-conv-1", "codex"))
	f := &fakeRun{answer: "Hi.\n", errOutput: "hook: SessionStart\ntokens used\n42\n", started: true}
	f.install(t)

	aio, out, errb := io2buf()
	require.NoError(t, runAsk(ttyInput("term-CS", "/repo/x", "say hi"), aio))

	assert.Contains(t, out.String(), "Hi.", "the clean answer is printed")
	assert.NotContains(t, errb.String(), "hook: SessionStart", "the noisy transcript is hidden")
	assert.NotContains(t, errb.String(), "tokens used")
}

// TestAsk_CodexCaptureShowsStderrOnFailure: when a captured Codex ask FAILS,
// the suppressed transcript is flushed so the error isn't silent.
func TestAsk_CodexCaptureShowsStderrOnFailure(t *testing.T) {
	setupAskTestDB(t)
	forceConvExists(t, true)
	require.NoError(t, db.SetAskThread("term-CF", "/repo/x", "codex-conv-1", "codex"))
	f := &fakeRun{errOutput: "error: not logged in\n", started: true, err: errors.New("exit status 1")}
	f.install(t)

	aio, _, errb := io2buf()
	err := runAsk(ttyInput("term-CF", "/repo/x", "say hi"), aio)
	require.Error(t, err)
	assert.Contains(t, errb.String(), "not logged in", "a failed run surfaces the suppressed transcript")
}

// TestAsk_CodexCaptureVerboseShowsStderr: --verbose keeps the transcript live
// even on success.
func TestAsk_CodexCaptureVerboseShowsStderr(t *testing.T) {
	setupAskTestDB(t)
	forceConvExists(t, true)
	require.NoError(t, db.SetAskThread("term-CV", "/repo/x", "codex-conv-1", "codex"))
	f := &fakeRun{answer: "Hi.\n", errOutput: "hook: SessionStart\n", started: true}
	f.install(t)

	in := ttyInput("term-CV", "/repo/x", "say hi")
	in.Verbose = true
	aio, _, errb := io2buf()
	require.NoError(t, runAsk(in, aio))
	assert.Contains(t, errb.String(), "hook: SessionStart", "--verbose keeps the transcript visible")
}

// TestAsk_ClaudeCaptureStderrNotHidden: a harness that isn't noisy on stderr
// (Claude) has its stderr passed straight through — the suppression is
// codex-specific (Asker.NoisyCaptureStderr).
func TestAsk_ClaudeCaptureStderrNotHidden(t *testing.T) {
	setupAskTestDB(t)
	forceConvExists(t, true)
	f := &fakeRun{answer: "ok\n", errOutput: "claude: a warning\n", started: true}
	f.install(t)

	aio, _, errb := io2buf()
	require.NoError(t, runAsk(ttyInput("term-CL", "/repo/x", "q"), aio)) // default harness = claude
	assert.Contains(t, errb.String(), "claude: a warning", "claude's stderr is not suppressed")
}

// TestAsk_ClaudeTTYStreamsAnswer: a print-mode Claude ask to a real terminal
// asks for the streaming output format and renders the answer through the
// harness's StreamFilter (which reassembles the text_delta chunks), so a human
// sees it build up live. The clean answer still reaches stdout.
func TestAsk_ClaudeTTYStreamsAnswer(t *testing.T) {
	setupAskTestDB(t)
	forceConvExists(t, true)
	f := &fakeRun{answer: "the answer\n", started: true}
	f.install(t)

	aio, out, _ := io2buf()
	require.NoError(t, runAsk(ttyInput("term-S", "/repo/x", "what is up?"), aio))

	argv := f.last().Argv
	assert.True(t, argvHas(argv, "-p"), "still print mode")
	assert.True(t, argvHas(argv, "stream-json"), "TTY print mode requests the streaming output format")
	assert.True(t, argvHas(argv, "--include-partial-messages"), "token-level deltas are requested")
	if v, ok := argvValue(argv, "--output-format"); assert.True(t, ok) {
		assert.Equal(t, "stream-json", v)
	}
	// The two text_delta chunks the fake stream split the answer into are
	// reassembled into the whole answer; the thinking/assistant noise is not.
	assert.Equal(t, "the answer\n", out.String(), "answer is streamed clean (with a single trailing newline)")
	assert.NotContains(t, out.String(), "hmm", "reasoning/thinking is never echoed to stdout")
}

// TestAsk_ClaudePipedStaysBuffered: when stdout is NOT a terminal (piped /
// captured, e.g. `x=$(tclaude ask …)`), the ask keeps the plain buffered path —
// no stream-json flags — because a pipe reads the answer whole regardless, and
// this keeps capture output exact and unchanged.
func TestAsk_ClaudePipedStaysBuffered(t *testing.T) {
	setupAskTestDB(t)
	forceConvExists(t, true)
	f := &fakeRun{answer: "captured answer\n", started: true}
	f.install(t)

	in := askInput{
		TermKey: "term-P", Cwd: "/repo/x", Question: "q",
		StdinIsTerminal: true, StdoutIsTerminal: false, // stdout redirected
	}
	aio, out, _ := io2buf()
	require.NoError(t, runAsk(in, aio))

	assert.False(t, argvHas(f.last().Argv, "stream-json"), "a piped/captured stdout keeps the buffered path")
	assert.Equal(t, "captured answer\n", out.String(), "captured answer is byte-for-byte the harness output")
}

// TestAsk_StreamFlushErrorWarns: when streaming output to the terminal fails
// (a downstream write error the filter stashes), runAsk surfaces it as a
// warning on stderr after the run — without masking a successful run's outcome.
func TestAsk_StreamFlushErrorWarns(t *testing.T) {
	setupAskTestDB(t)
	forceConvExists(t, true)
	f := &fakeRun{answer: "the answer\n", started: true}
	f.install(t)

	var errb bytes.Buffer
	aio := askIO{Stdin: strings.NewReader(""), Stdout: failingWriter{}, Stderr: &errb}
	require.NoError(t, runAsk(ttyInput("term-FE", "/repo/x", "q"), aio))
	assert.Contains(t, errb.String(), "could not finish streaming output",
		"a streaming write failure is surfaced as a warning")
}

// failingWriter is a stdout stand-in that always errors, to drive the stream
// filter's stashed-write-error path through runAsk.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("tty gone") }

// TestAsk_InteractiveDoesNotStreamJSON: interactive mode (-i, the full TUI)
// streams natively and must never get the print-only stream-json flags.
func TestAsk_InteractiveDoesNotStreamJSON(t *testing.T) {
	setupAskTestDB(t)
	forceConvExists(t, true)
	f := &fakeRun{answer: "", started: true}
	f.install(t)

	in := askInput{
		TermKey: "term-I", Cwd: "/repo/x", Question: "refactor this",
		ForceInteractive: true, StdinIsTerminal: true, StdoutIsTerminal: true,
	}
	require.NoError(t, runAsk(in, askIO{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard}))

	argv := f.last().Argv
	assert.False(t, argvHas(argv, "-p"), "interactive is not print mode")
	assert.False(t, argvHas(argv, "stream-json"), "interactive never gets the print-only streaming flags")
}

// TestAsk_ClaudeTTYNoSmoothingStillStreamsCleanly: --no-smoothing keeps the live
// stream (still stream-json) but turns the pacing off; the clean answer is
// unchanged. Smoothed vs unsmoothed differ only in timing, so we assert the
// wiring (flag flows through to the filter) doesn't disturb the output.
func TestAsk_ClaudeTTYNoSmoothingStillStreamsCleanly(t *testing.T) {
	setupAskTestDB(t)
	forceConvExists(t, true)
	f := &fakeRun{answer: "the answer\n", started: true}
	f.install(t)

	in := ttyInput("term-NS", "/repo/x", "what is up?")
	in.NoSmoothing = true
	aio, out, _ := io2buf()
	require.NoError(t, runAsk(in, aio))

	assert.True(t, argvHas(f.last().Argv, "stream-json"), "--no-smoothing still streams, just unpaced")
	assert.Equal(t, "the answer\n", out.String(), "the unsmoothed stream still yields the clean answer")
}

// TestAsk_ClaudeTTYShowsThenClearsSpinner: when stderr is a terminal, the live
// "working…" indicator is drawn on stderr only — stdout carries the clean answer,
// the answer never leaks onto the indicator line, and every indicator write ends
// in a clear-to-EOL so nothing is left dangling. (How fast the answer arrives is
// timing-dependent, so the exact frames aren't asserted here — the spinner's own
// render test pins those deterministically.)
func TestAsk_ClaudeTTYShowsThenClearsSpinner(t *testing.T) {
	setupAskTestDB(t)
	forceConvExists(t, true)
	f := &fakeRun{answer: "the answer\n", started: true}
	f.install(t)

	in := ttyInput("term-SP", "/repo/x", "what is up?")
	in.StderrIsTerminal = true
	aio, out, errb := io2buf()
	require.NoError(t, runAsk(in, aio))

	assert.Equal(t, "the answer\n", out.String(), "the answer is clean on stdout")
	assert.NotContains(t, errb.String(), "the answer", "the answer never leaks onto the stderr indicator line")
	if e := errb.String(); e != "" {
		assert.True(t, strings.HasSuffix(e, "\033[K"), "any indicator output ends cleared; stderr=%q", e)
	}
}

// TestResolveSmooth pins the flag/env precedence: --no-smoothing forces off;
// otherwise TCLAUDE_ASK_SMOOTH=falsey turns it off; otherwise it stays on.
func TestResolveSmooth(t *testing.T) {
	t.Run("default on", func(t *testing.T) {
		t.Setenv("TCLAUDE_ASK_SMOOTH", "")
		assert.True(t, resolveSmooth(false))
	})
	t.Run("--no-smoothing forces off", func(t *testing.T) {
		t.Setenv("TCLAUDE_ASK_SMOOTH", "")
		assert.False(t, resolveSmooth(true))
	})
	t.Run("env falsey turns off", func(t *testing.T) {
		for _, v := range []string{"0", "false", "off", "no", "OFF", " no "} {
			t.Setenv("TCLAUDE_ASK_SMOOTH", v)
			assert.Falsef(t, resolveSmooth(false), "env=%q", v)
		}
	})
	t.Run("env truthy or unrecognized stays on", func(t *testing.T) {
		for _, v := range []string{"1", "true", "yes", "on", "whatever"} {
			t.Setenv("TCLAUDE_ASK_SMOOTH", v)
			assert.Truef(t, resolveSmooth(false), "env=%q", v)
		}
	})
	t.Run("flag beats a truthy env", func(t *testing.T) {
		t.Setenv("TCLAUDE_ASK_SMOOTH", "1")
		assert.False(t, resolveSmooth(true))
	})
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
