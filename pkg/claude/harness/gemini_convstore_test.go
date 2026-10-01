package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// geminiTestStore isolates HOME (and so tclaude's DB) and returns a store
// rooted at a fresh fixture `.gemini` directory.
func geminiTestStore(t *testing.T) (geminiConvStore, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	dir := filepath.Join(t.TempDir(), ".gemini")
	return geminiConvStore{dir: dir}, dir
}

// geminiProject creates tmp/<slug>/.project_root for a project root.
func geminiProject(t *testing.T, dir, slug, projectRoot string) string {
	t.Helper()
	slugDir := filepath.Join(dir, "tmp", slug)
	require.NoError(t, os.MkdirAll(filepath.Join(slugDir, "chats"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(slugDir, ".project_root"), []byte(projectRoot), 0o644))
	return slugDir
}

func geminiWriteSession(t *testing.T, slugDir, fileName string, lines ...string) string {
	t.Helper()
	path := filepath.Join(slugDir, "chats", fileName)
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644))
	return path
}

const (
	geminiIDA = "11111111-aaaa-4bbb-8ccc-000000000001"
	geminiIDB = "22222222-aaaa-4bbb-8ccc-000000000002"
)

func TestGeminiConvStoreListsResumableMainSessions(t *testing.T) {
	store, dir := geminiTestStore(t)
	projA := t.TempDir()
	projB := t.TempDir()
	slugA := geminiProject(t, dir, "proj-a", projA)
	slugB := geminiProject(t, dir, "proj-b", projB)

	geminiWriteSession(t, slugA, "session-2026-10-01T10-00-11111111.jsonl",
		`{"sessionId":"`+geminiIDA+`","projectHash":"h","startTime":"2026-10-01T10:00:00.000Z","lastUpdated":"2026-10-01T10:00:00.000Z","kind":"main"}`,
		`{"id":"m1","timestamp":"t","type":"user","content":[{"text":"Investigate the flaky test"}]}`,
		`{"id":"m2","timestamp":"t","type":"gemini","content":"On it.","model":"gemini-3.1-pro-preview"}`,
		`{"$set":{"lastUpdated":"2026-10-01T10:05:00.000Z","summary":"Flaky test investigation"}}`,
		`{"id":"m3","timestamp":"t","type":"user","content":"and fix it"}`,
	)
	// Startup-only session: no resumable message, so Gemini's own --resume
	// would refuse it and the store must not list it.
	geminiWriteSession(t, slugB, "session-2026-10-01T11-00-33333333.jsonl",
		`{"sessionId":"33333333-aaaa-4bbb-8ccc-000000000003","projectHash":"h","startTime":"2026-10-01T11:00:00.000Z","lastUpdated":"2026-10-01T11:00:00.000Z"}`,
		`{"id":"x1","timestamp":"t","type":"user","content":"/help"}`,
		`{"id":"x2","timestamp":"t","type":"info","content":"help text"}`,
	)
	// A subagent transcript nested under its parent is never a conversation.
	nested := filepath.Join(slugB, "chats", geminiIDA)
	require.NoError(t, os.MkdirAll(nested, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(nested, "sub.jsonl"), []byte(
		`{"sessionId":"sub","projectHash":"h","kind":"subagent"}`+"\n"+
			`{"id":"s1","type":"user","content":"sub task"}`+"\n"), 0o644))
	geminiWriteSession(t, slugB, "session-2026-10-01T12-00-22222222.jsonl",
		`{"sessionId":"`+geminiIDB+`","projectHash":"h","startTime":"2026-10-01T12:00:00.000Z","lastUpdated":"2026-10-01T12:00:00.000Z"}`,
		`{"id":"b1","timestamp":"t","type":"user","content":"Second project"}`,
	)

	all, err := store.ListConvs("")
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.Equal(t, geminiIDB, all[0].SessionID, "newest first")

	a := all[1]
	assert.Equal(t, geminiIDA, a.SessionID)
	assert.Equal(t, filepath.Clean(projA), a.ProjectPath)
	assert.Equal(t, "Investigate the flaky test", a.FirstPrompt)
	assert.Equal(t, "Flaky test investigation", a.Summary)
	assert.Equal(t, 2, a.MessageCount, "user turns")
	assert.Equal(t, "gemini-3.1-pro-preview", a.Model)
	assert.Equal(t, "2026-10-01T10:00:00Z", a.Created)
	assert.Equal(t, "2026-10-01T10:05:00Z", a.Modified)
	assert.Equal(t, GeminiName, a.Harness)

	scoped, err := store.ListConvs(projA)
	require.NoError(t, err)
	require.Len(t, scoped, 1)
	assert.Equal(t, geminiIDA, scoped[0].SessionID)

	ref, err := store.Resolve("1111", projB, false)
	require.NoError(t, err)
	assert.Nil(t, ref, "a cwd-scoped resolve does not see another project")
	ref, err = store.Resolve("1111", projB, true)
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.Equal(t, geminiIDA, ref.ConvID)
	assert.Equal(t, filepath.Clean(projA), ref.ProjectPath)

	ok, err := store.Exists(geminiIDA, "")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = store.Exists("33333333-aaaa-4bbb-8ccc-000000000003", "")
	require.NoError(t, err)
	assert.False(t, ok, "a session Gemini would not resume does not exist for tclaude either")

	title, err := store.Title(geminiIDA)
	require.NoError(t, err)
	assert.Equal(t, "Flaky test investigation", title)
}

func TestGeminiConvStoreRewindCheckpointAndReplace(t *testing.T) {
	store, dir := geminiTestStore(t)
	proj := t.TempDir()
	slug := geminiProject(t, dir, "p", proj)
	geminiWriteSession(t, slug, "session-2026-10-01T10-00-11111111.jsonl",
		`{"sessionId":"`+geminiIDA+`","projectHash":"h","startTime":"2026-10-01T10:00:00Z","lastUpdated":"2026-10-01T10:00:00Z"}`,
		`{"id":"m1","type":"user","content":"first"}`,
		`{"id":"m2","type":"gemini","content":"answer"}`,
		`{"id":"m3","type":"user","content":"second"}`,
		// Rewind to m2 drops m2 and m3.
		`{"$rewindTo":"m2"}`,
		// A repeated id replaces the earlier record in place.
		`{"id":"m1","type":"user","content":"first, edited"}`,
		`{"id":"m4","type":"user","content":"third"}`,
	)
	all, err := store.ListConvs("")
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, "first, edited", all[0].FirstPrompt)
	assert.Equal(t, 2, all[0].MessageCount)

	geminiWriteSession(t, slug, "session-2026-10-01T10-00-11111111.jsonl",
		`{"sessionId":"`+geminiIDA+`","projectHash":"h"}`,
		`{"id":"m1","type":"user","content":"before checkpoint"}`,
		`{"$set":{"messages":[{"id":"c1","type":"user","content":"after checkpoint"},{"id":"c2","type":"gemini","content":"ok"}]}}`,
	)
	all, err = store.ListConvs("")
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, "after checkpoint", all[0].FirstPrompt)
	assert.Equal(t, 1, all[0].MessageCount)

	geminiWriteSession(t, slug, "session-2026-10-01T10-00-11111111.jsonl",
		`{"sessionId":"`+geminiIDA+`","projectHash":"h"}`,
		`{"id":"m1","type":"user","content":"gone"}`,
		`{"$rewindTo":"unknown-id"}`,
	)
	all, err = store.ListConvs("")
	require.NoError(t, err)
	assert.Empty(t, all, "rewinding to an unknown id drops every message")
}

func TestGeminiConvStoreLegacyJSONAndDuplicates(t *testing.T) {
	store, dir := geminiTestStore(t)
	proj := t.TempDir()
	slug := geminiProject(t, dir, "p", proj)
	// A pretty-printed legacy record, and the .jsonl migration a resume wrote
	// beside it. The later lastUpdated wins.
	geminiWriteSession(t, slug, "session-2026-09-01T10-00-11111111.json", `{
  "sessionId": "`+geminiIDA+`",
  "projectHash": "h",
  "startTime": "2026-09-01T10:00:00Z",
  "lastUpdated": "2026-09-01T10:00:00Z",
  "messages": [
    {"id": "l1", "type": "user", "content": "legacy question"}
  ]
}`)
	geminiWriteSession(t, slug, "session-2026-09-01T10-00-11111111.jsonl",
		`{"sessionId":"`+geminiIDA+`","projectHash":"h","startTime":"2026-09-01T10:00:00Z","lastUpdated":"2026-09-02T10:00:00Z"}`,
		`{"id":"l1","type":"user","content":"legacy question"}`,
		`{"id":"l2","type":"user","content":"after resume"}`,
	)
	all, err := store.ListConvs("")
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, 2, all[0].MessageCount)
	assert.True(t, strings.HasSuffix(all[0].FullPath, ".jsonl"))
}

func TestGeminiConvStoreProjectRootFallsBackToRegistry(t *testing.T) {
	store, dir := geminiTestStore(t)
	proj := t.TempDir()
	slugDir := filepath.Join(dir, "tmp", "proj")
	require.NoError(t, os.MkdirAll(filepath.Join(slugDir, "chats"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "projects.json"),
		[]byte(`{"projects":{"`+proj+`":"proj"}}`), 0o644))
	geminiWriteSession(t, slugDir, "session-2026-10-01T10-00-11111111.jsonl",
		`{"sessionId":"`+geminiIDA+`","projectHash":"h"}`,
		`{"id":"m1","type":"user","content":"hi"}`,
	)
	// And a slug with neither marker nor registry entry is unlisted.
	orphan := filepath.Join(dir, "tmp", "deadbeef")
	require.NoError(t, os.MkdirAll(filepath.Join(orphan, "chats"), 0o755))
	geminiWriteSession(t, orphan, "session-2026-10-01T10-00-22222222.jsonl",
		`{"sessionId":"`+geminiIDB+`","projectHash":"h"}`,
		`{"id":"m1","type":"user","content":"hi"}`,
	)
	all, err := store.ListConvs("")
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, filepath.Clean(proj), all[0].ProjectPath)
}

func TestGeminiConvStoreTitleOverlay(t *testing.T) {
	store, dir := geminiTestStore(t)
	proj := t.TempDir()
	slug := geminiProject(t, dir, "p", proj)

	// A spawn names its agent before Gemini has written anything: the title
	// must be accepted for a not-yet-listable id and surface once it is.
	require.NoError(t, store.SetTitle(geminiIDA, "worker-1"))
	geminiWriteSession(t, slug, "session-2026-10-01T10-00-11111111.jsonl",
		`{"sessionId":"`+geminiIDA+`","projectHash":"h"}`,
		`{"id":"m1","type":"user","content":"hello"}`,
		`{"$set":{"summary":"Gemini's own summary"}}`,
	)
	all, err := store.ListConvs("")
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, "worker-1", all[0].CustomTitle)
	assert.Equal(t, "Gemini's own summary", all[0].Summary)
	assert.Equal(t, "worker-1", all[0].DisplayTitle())

	// The listing sync must not wipe the tclaude-owned title.
	_, err = store.ListConvs("")
	require.NoError(t, err)
	title, err := store.Title(geminiIDA)
	require.NoError(t, err)
	assert.Equal(t, "worker-1", title)

	require.NoError(t, store.SetTitle(geminiIDA, "renamed"))
	title, err = store.Title(geminiIDA)
	require.NoError(t, err)
	assert.Equal(t, "renamed", title)

	assert.Error(t, store.SetTitle(geminiIDA, "  "))
	assert.Error(t, store.SetTitle("", "x"))
}

func TestGeminiConvStoreMissingHomeIsEmpty(t *testing.T) {
	store, _ := geminiTestStore(t)
	all, err := store.ListConvs("")
	require.NoError(t, err)
	assert.Empty(t, all)
	ok, err := store.Exists(geminiIDA, "")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestGeminiContentText(t *testing.T) {
	assert.Equal(t, "plain", geminiContentText([]byte(`"plain"`)))
	assert.Equal(t, "ab", geminiContentText([]byte(`[{"text":"a"},{"inlineData":{}},"b"]`)))
	assert.Equal(t, "single", geminiContentText([]byte(`{"text":"single"}`)))
	assert.Equal(t, "", geminiContentText(nil))
}

func TestGeminiAskerArgv(t *testing.T) {
	a := geminiAsker{}
	assert.True(t, a.PreMintsConvID())
	assert.True(t, a.NoisyCaptureStderr())

	assert.Equal(t,
		[]string{"gemini", "--session-id", geminiIDA, "--model", "flash", "--prompt=--why is this?"},
		a.BuildAskArgv(AskSpec{SessionID: geminiIDA, Model: "flash", Print: true, Prompt: "--why is this?"}))
	assert.Equal(t,
		[]string{"gemini", "--resume", geminiIDA, "--prompt-interactive=follow up"},
		a.BuildAskArgv(AskSpec{ResumeID: geminiIDA, SessionID: "ignored", Prompt: "follow up"}))
	assert.Equal(t, []string{"gemini"}, a.BuildAskArgv(AskSpec{}))
}
